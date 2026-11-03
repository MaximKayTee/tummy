package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// WriteConnections writes every connection, patching the existing file so comments,
// blank lines and ordering are kept. A new file is created 0600.
func WriteConnections(path string, c *Connections) error {
	var b strings.Builder
	for i, n := range c.Names() {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[conn.%s]\n", key(n))
		for _, f := range fieldsOf(reflect.ValueOf(*c.Conn[n])) {
			if !isEmpty(f.v) {
				fmt.Fprintf(&b, "%s = %s\n", f.key, value(f.v))
			}
		}
	}
	text := b.String()
	var check Connections
	if _, err := toml.Decode(text, &check); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		renames := map[string]string{}
		for to, from := range c.renamed {
			renames["conn\x00"+from] = "conn\x00" + to
		}
		patched, err := patchRenamed(string(existing), text, renames)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		text = patched
	}
	if err := writeFile(path, text); err != nil {
		return err
	}
	c.renamed = nil
	return nil
}

// ReplicationDraft is a replication as the editor sees it: defaults plus explicit streams,
// with values as written in the file (${VAR} references unexpanded).
type ReplicationDraft struct {
	Name string
	// From is the name of the file the draft was read from. For a rename, while NAME.toml
	// does not exist yet, From's file is the text WriteReplication patches.
	From     string
	Source   string
	Target   string
	Schedule string
	Enabled  bool
	Exclude  []string
	OnError  string
	Defaults Stream
	Streams  []StreamDraft

	keys map[string]bool // top-level keys in the file
}

// StreamDraft is one stream with the defaults merged in.
type StreamDraft struct {
	Name   string
	Stream Stream
	// Keys lists the keys in the stream's own table in the file. They are written even
	// when equal to the defaults; the other keys follow the defaults unless changed.
	Keys []string

	inherited *Stream // defaults Stream was merged from; nil for a new stream
}

func draftFrom(r *Replication, topKeys map[string]bool) *ReplicationDraft {
	d := &ReplicationDraft{
		Name: r.Name, From: r.Name, Source: r.Source, Target: r.Target, Schedule: r.Schedule,
		Enabled: r.IsEnabled(), Exclude: append([]string(nil), r.Exclude...), OnError: r.OnError,
		Defaults: r.Stream.clone(), keys: topKeys,
	}
	for _, s := range r.Resolved {
		inh := r.Stream.clone()
		d.Streams = append(d.Streams, StreamDraft{Name: s.Name, Stream: s.Stream.clone(), Keys: append([]string{}, s.Set...), inherited: &inh})
	}
	return d
}

func (d ReplicationDraft) clone() ReplicationDraft {
	out := d
	out.Exclude = append([]string(nil), d.Exclude...)
	out.Defaults = d.Defaults.clone()
	out.keys = maps.Clone(d.keys)
	out.Streams = make([]StreamDraft, len(d.Streams))
	for i, s := range d.Streams {
		out.Streams[i] = StreamDraft{Name: s.Name, Stream: s.Stream.clone()}
		if s.Keys != nil {
			out.Streams[i].Keys = append([]string{}, s.Keys...)
		}
		if s.inherited != nil {
			inh := s.inherited.clone()
			out.Streams[i].inherited = &inh
		}
	}
	return out
}

// DraftOf turns a loaded replication into a draft of the values as written in the file,
// whether it was loaded with LoadReplication or LoadReplicationRaw.
func DraftOf(r *Replication) ReplicationDraft {
	if r.draft != nil {
		return r.draft.clone()
	}
	d := ReplicationDraft{
		Name: r.Name, From: r.Name, Source: r.Source, Target: r.Target, Schedule: r.Schedule,
		Enabled: r.IsEnabled(), Exclude: append([]string(nil), r.Exclude...), OnError: r.OnError,
		Defaults: r.Stream.clone(),
	}
	for _, s := range r.Resolved {
		d.Streams = append(d.Streams, StreamDraft{Name: s.Name, Stream: s.Stream.clone(), Keys: s.Set})
	}
	return d
}

// WriteReplication writes HOME/replications/NAME.toml from a draft, patching an existing
// file so comments, blank lines and ordering are kept.
func WriteReplication(home string, d ReplicationDraft) (string, error) {
	path, text, _, err := planWrite(home, d)
	if err != nil {
		return "", err
	}
	return path, writeFile(path, text)
}

// PreviewWrite returns the text WriteReplication would write for d, without writing it,
// and its lines against the file it patches: "  " kept, "+ " added, "- " removed; all
// added for a new file. On an error text is what could be rendered, possibly nothing.
func PreviewWrite(home string, d ReplicationDraft) (text string, diff []string, err error) {
	_, text, base, err := planWrite(home, d)
	if text == "" {
		return "", nil, err
	}
	return text, diffLines(base, text), err
}

// planWrite renders d and patches it onto the file it replaces. base is that file's text,
// "" for a new file. A rendered text that does not load comes back with the error.
func planWrite(home string, d ReplicationDraft) (path, text, base string, err error) {
	if d.Name == "" {
		return "", "", "", errors.New("name is required")
	}
	if path, err = ReplicationPath(home, d.Name); err != nil {
		return "", "", "", err
	}
	existing, readErr := os.ReadFile(path)
	if errors.Is(readErr, fs.ErrNotExist) && d.From != "" && d.From != d.Name {
		if from, err := ReplicationPath(home, d.From); err == nil {
			existing, readErr = os.ReadFile(from)
		}
	}
	if readErr == nil {
		if old, err := LoadReplicationText(string(existing), path); err == nil {
			d = d.withKeysFrom(old.draft)
		}
	}
	if text, err = RenderReplication(d); err != nil {
		return path, "", "", err
	}
	if _, err := LoadReplicationText(text, path); err != nil {
		return path, text, "", err
	}
	if readErr != nil {
		return path, text, "", nil
	}
	base = string(existing)
	patched, err := patchTOML(base, text)
	if err != nil {
		return path, text, base, fmt.Errorf("%s: %w", path, err)
	}
	return path, patched, base, nil
}

// diffLines marks the lines of b against a: "  " kept, "+ " added, "- " removed.
func diffLines(a, b string) []string {
	split := func(s string) []string {
		s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	x, y := split(a), split(b)
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			out = append(out, "  "+y[j])
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, "- "+x[i])
			i++
		default:
			out = append(out, "+ "+y[j])
			j++
		}
	}
	return out
}

// withKeysFrom takes the explicit keys of streams built without them from the file's
// version of the same stream.
func (d ReplicationDraft) withKeysFrom(old *ReplicationDraft) ReplicationDraft {
	d = d.clone()
	if d.keys == nil {
		d.keys = maps.Clone(old.keys)
	}
	byName := map[string]StreamDraft{}
	for _, s := range old.Streams {
		byName[s.Name] = s
	}
	for i := range d.Streams {
		s := &d.Streams[i]
		if o, ok := byName[s.Name]; ok && s.Keys == nil && s.inherited == nil {
			s.Keys = append([]string{}, o.Keys...)
		}
	}
	return d
}

// RenderReplication produces the TOML text for a draft. A stream gets its explicit keys
// and the values that differ from the defaults.
func RenderReplication(d ReplicationDraft) (string, error) {
	var b strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&b, "%s = %s\n", k, v) }
	kv("source", quote(d.Source))
	kv("target", quote(d.Target))
	if d.Schedule != "" {
		kv("schedule", quote(d.Schedule))
	}
	if !d.Enabled {
		kv("enabled", "false")
	}
	if len(d.Exclude) > 0 {
		kv("exclude", list(d.Exclude))
	}
	if d.OnError != "" {
		kv("on_error", quote(d.OnError))
	}
	cur := fieldsOf(reflect.ValueOf(d.Defaults))
	for _, f := range cur {
		if isEmpty(f.v) || f.key == "mode" && f.v.String() == defaultMode && !d.keys["mode"] {
			continue
		}
		kv(f.key, value(f.v))
	}
	seen := map[string]bool{}
	for _, s := range d.Streams {
		if s.Name == "" {
			return "", errors.New("a stream has no name")
		}
		if seen[s.Name] {
			return "", fmt.Errorf("stream %s: listed twice", s.Name)
		}
		seen[s.Name] = true
		fmt.Fprintf(&b, "\n[streams.%s]\n", key(s.Name))
		base := cur
		if s.inherited != nil {
			base = fieldsOf(reflect.ValueOf(*s.inherited))
		}
		explicit := map[string]bool{}
		for _, k := range s.Keys {
			explicit[k] = true
		}
		for i, f := range fieldsOf(reflect.ValueOf(s.Stream)) {
			switch {
			case f.key == "mode" && f.v.String() == "":
				continue
			case explicit[f.key]:
				if isEmpty(f.v) && isEmpty(cur[i].v) {
					continue
				}
			case equalValue(f.v, base[i].v) || equalValue(f.v, cur[i].v):
				continue
			}
			kv(f.key, value(f.v))
		}
	}
	return b.String(), nil
}

type field struct {
	key string
	v   reflect.Value
}

// fieldsOf lists the TOML-tagged fields of a struct in declaration order.
func fieldsOf(v reflect.Value) []field {
	var out []field
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if k := tomlKey(f); k != "" && f.IsExported() {
			out = append(out, field{k, v.Field(i)})
		}
	}
	return out
}

func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	}
	return v.IsZero()
}

func equalValue(a, b reflect.Value) bool {
	if isEmpty(a) && isEmpty(b) {
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// value renders a TOML value; empty lists and maps render as [] and {}.
func value(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return quote(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Slice:
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = value(v.Index(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		if v.Len() == 0 {
			return "{}"
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = key(k.String()) + " = " + value(v.MapIndex(k))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return quote(fmt.Sprint(v.Interface()))
}

func list(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// quote renders a TOML string. Multi-line text uses a basic multi-line string.
func quote(s string) string {
	if strings.Contains(s, "\n") && !strings.Contains(s, `"""`) && !strings.HasSuffix(s, `"`) && !hasControl(s) {
		return "\"\"\"\n" + strings.ReplaceAll(s, `\`, `\\`) + "\"\"\""
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func hasControl(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return true
		}
	}
	return false
}

// key renders a table key, quoting it when it is not a bare key.
func key(s string) string {
	if s == "" {
		return `""`
	}
	for i := 0; i < len(s); i++ {
		if !isBare(s[i]) {
			return quote(s)
		}
	}
	return s
}

// KV is one key and its value as TOML text.
type KV struct{ Key, Value string }

// Values lists the stream's non-empty keys in declaration order, values as TOML text.
func (s Stream) Values() []KV {
	var out []KV
	for _, f := range fieldsOf(reflect.ValueOf(s)) {
		if !isEmpty(f.v) {
			out = append(out, KV{f.key, value(f.v)})
		}
	}
	return out
}
