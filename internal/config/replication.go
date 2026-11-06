package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
)

// Stream holds every per-stream key. The same keys at the top of a
// replication file are the defaults.
type Stream struct {
	Mode         string `toml:"mode"`
	Object       string `toml:"object"`
	PrimaryKey   Names  `toml:"primary_key"`
	UpdateKey    string `toml:"update_key"`
	SQL          string `toml:"sql"`
	Steps        string `toml:"steps"`
	Select       Names  `toml:"select"`
	Where        string `toml:"where"`
	Limit        int    `toml:"limit"`
	Range        string `toml:"range"`
	Disabled     bool   `toml:"disabled"`
	QueryTimeout string `toml:"query_timeout"`

	// api
	Path         string            `toml:"path"`
	Method       string            `toml:"method"`
	Query        map[string]string `toml:"query"`
	Body         string            `toml:"body"`
	Records      string            `toml:"records"`
	Paginate     string            `toml:"paginate"`
	PageSize     int               `toml:"page_size"`
	PageParam    string            `toml:"page_param"`
	SizeParam    string            `toml:"size_param"`
	CursorField  string            `toml:"cursor_field"`
	CursorParam  string            `toml:"cursor_param"`
	ForEach      string            `toml:"for_each"`
	AddColumns   map[string]string `toml:"add_columns"`
	SkipExisting string            `toml:"skip_existing"`  // target column; parents with a value already loaded are skipped
	ForEachSince string            `toml:"for_each_since"` // "field, value": parents below it are skipped; value a date, number or Nd
	SinceParam   string            `toml:"since_param"`
	SinceFormat  string            `toml:"since_format"` // rfc3339 (default), date, unix, unix_ms, raw
	Sorted       string            `toml:"sorted"`       // desc: newest first, paging stops at the watermark

	SkipOn     []int  `toml:"skip_on"`     // statuses that end a fetch with no error
	StopWhen   string `toml:"stop_when"`   // JMESPath on the response; truthy stops paging
	TotalField string `toml:"total_field"` // JMESPath to the total record count; paging stops at it
	ParamsIn   string `toml:"params_in"`   // query (default) or body: where page, size and cursor go
}

type Replication struct {
	Stream

	Source   string `toml:"source"`
	Target   string `toml:"target"`
	Schedule string `toml:"schedule"`
	Enabled  *bool  `toml:"enabled"`
	Exclude  Names  `toml:"exclude"`
	OnError  string `toml:"on_error"` // shell command run by tummy serve when a run fails

	Streams map[string]toml.Primitive `toml:"streams"`

	Path     string           `toml:"-"`
	Name     string           `toml:"-"`
	Resolved []ResolvedStream `toml:"-"`

	draft *ReplicationDraft // values as written, for editing
}

// ResolvedStream is a stream with the defaults applied.
type ResolvedStream struct {
	Name string
	Stream
	Set  []string // keys in the stream's own table; the others come from the defaults
	From string   // the table pattern this stream was expanded from, by Expand
}

// Names is a list of names. In a file, a single string is a list of one.
type Names []string

func (n *Names) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*n = Names{x}
	case []any:
		out := make(Names, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("want a string or a list of strings, got %v", v)
			}
			out = append(out, s)
		}
		*n = out
	default:
		return fmt.Errorf("want a string or a list of strings, got %v", v)
	}
	return nil
}

const defaultMode = "full-refresh"

// LoadReplication reads a replication file with ${VAR} references resolved and
// sql = "file:..." read in. DraftOf on the result still gives the values as written.
func LoadReplication(path string) (*Replication, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseReplication(string(raw), path, true)
}

// LoadReplicationRaw reads a replication file as written, for editing and checks that
// must not depend on the environment.
func LoadReplicationRaw(path string) (*Replication, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseReplication(string(raw), path, false)
}

// LoadReplicationText parses replication text as written, as if it were at path.
func LoadReplicationText(text, path string) (*Replication, error) {
	return parseReplication(text, path, false)
}

func (r *Replication) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

func parseReplication(text, path string, expand bool) (*Replication, error) {
	fail := func(err error) (*Replication, error) { return nil, fmt.Errorf("%s: %w", path, err) }
	var r Replication
	md, err := toml.Decode(text, &r)
	if err != nil {
		return fail(err)
	}
	r.Path = path
	r.Name = NameOf(path)

	// Stream names in file order; keys set at the top and in each stream.
	topKeys := map[string]bool{}
	var names []string
	streamKeys := map[string][]string{}
	for _, k := range md.Keys() {
		if len(k) == 1 {
			topKeys[k[0]] = true
		}
		if len(k) < 2 || k[0] != "streams" {
			continue
		}
		if _, ok := streamKeys[k[1]]; !ok {
			names = append(names, k[1])
			streamKeys[k[1]] = []string{}
		}
		if len(k) >= 3 && !contains(streamKeys[k[1]], k[2]) {
			streamKeys[k[1]] = append(streamKeys[k[1]], k[2])
		}
	}
	for _, name := range names {
		prim, ok := r.Streams[name]
		if !ok {
			continue
		}
		s := r.Stream.clone()
		// a map in a stream replaces the default map, it does not merge into it
		sv := reflect.ValueOf(&s).Elem()
		for i := 0; i < sv.NumField(); i++ {
			f := sv.Type().Field(i)
			if f.Type.Kind() == reflect.Map && md.IsDefined("streams", name, tomlKey(f)) {
				sv.Field(i).SetZero()
			}
		}
		if err := md.PrimitiveDecode(prim, &s); err != nil {
			return fail(fmt.Errorf("stream %s: %w", name, err))
		}
		r.Resolved = append(r.Resolved, ResolvedStream{Name: name, Stream: s, Set: streamKeys[name]})
	}
	if err := undecodedErr(md); err != nil {
		return fail(err)
	}
	if r.Source == "" || r.Target == "" {
		return fail(errors.New("source and target are required"))
	}
	if len(r.Resolved) == 0 {
		return fail(errors.New(`no streams: add a [streams.NAME] table`))
	}
	if r.Mode == "" {
		r.Mode = defaultMode
	}
	for i := range r.Resolved {
		if r.Resolved[i].Mode == "" {
			r.Resolved[i].Mode = defaultMode
		}
	}
	r.draft = draftFrom(&r, topKeys)

	if expand {
		if err := expandFields(&r, "on_error"); err != nil {
			return fail(err)
		}
		for i := range r.Resolved {
			s := &r.Resolved[i]
			if err := expandFields(&s.Stream); err != nil {
				return fail(fmt.Errorf("stream %s: %w", s.Name, err))
			}
			if err := s.readSQLFile(filepath.Dir(path)); err != nil {
				return fail(fmt.Errorf("stream %s: %w", s.Name, err))
			}
		}
	}
	if err := r.validate(!expand); err != nil {
		return fail(err)
	}
	return &r, nil
}

// readSQLFile replaces sql = "file:x.sql" with the file's text, relative to dir.
func (s *ResolvedStream) readSQLFile(dir string) error {
	p, ok := strings.CutPrefix(s.SQL, "file://")
	if !ok {
		if p, ok = strings.CutPrefix(s.SQL, "file:"); !ok {
			return nil
		}
	}
	p = ResolvePath(strings.TrimSpace(p), dir)
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("sql: %w", err)
	}
	text, err := expandEnv(string(b))
	if err != nil {
		return fmt.Errorf("sql %s: %w", p, err)
	}
	s.SQL = strings.TrimSpace(text)
	if s.SQL == "" {
		return fmt.Errorf("sql: %s is empty", p)
	}
	return nil
}

// Expand turns wildcard streams into one stream per matching table, applies exclude and
// lets an explicit stream win over a wildcard match of the same name. tables lists the
// source's tables in a schema ("" for the connection's default) as schema.name or name.
func (r *Replication) Expand(tables func(schema string) ([]string, error)) ([]ResolvedStream, error) {
	explicit := map[string]bool{}
	for _, s := range r.Resolved {
		if !s.IsWildcard() {
			explicit[s.Name] = true
		}
	}
	var out []ResolvedStream
	for _, s := range r.Resolved {
		if !s.IsWildcard() {
			if !r.Excluded(s.Name) {
				out = append(out, s)
			}
			continue
		}
		schema, pattern := "", s.Name
		if i := strings.LastIndexByte(s.Name, '.'); i >= 0 {
			schema, pattern = s.Name[:i], s.Name[i+1:]
		}
		list, err := tables(schema)
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", s.Name, err)
		}
		for _, t := range list {
			name := t[strings.LastIndexByte(t, '.')+1:]
			if ok, _ := path.Match(pattern, name); !ok || r.Excluded(t) || explicit[t] {
				continue
			}
			m := s
			m.Name, m.From = t, s.Name
			out = append(out, m)
		}
	}
	return out, nil
}

// Excluded reports whether a table name matches an exclude pattern.
func (r *Replication) Excluded(name string) bool {
	for _, pat := range r.Exclude {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// IsWildcard reports whether the stream name is a table pattern.
func (s ResolvedStream) IsWildcard() bool { return strings.ContainsAny(s.Name, "*?[") }

// Columns applies select to the source's columns: the listed ones in that order, or with
// select = ["-a", "-b"] every column but those. An empty select is every column.
func (s Stream) Columns(all []string) []string {
	if len(s.Select) == 0 {
		return all
	}
	if !strings.HasPrefix(s.Select[0], "-") {
		return s.Select
	}
	drop := map[string]bool{}
	for _, c := range s.Select {
		drop[strings.ToLower(strings.TrimPrefix(c, "-"))] = true
	}
	var out []string
	for _, c := range all {
		if !drop[strings.ToLower(c)] {
			out = append(out, c)
		}
	}
	return out
}

// UnknownStreams returns the names that match no stream, table pattern included.
func (r *Replication) UnknownStreams(names []string) []string {
	var out []string
	for _, n := range names {
		found := false
		for _, s := range r.Resolved {
			if s.Name == n {
				found = true
				break
			}
			if s.IsWildcard() {
				if ok, _ := path.Match(s.Name, n); ok && !r.Excluded(n) {
					found = true
					break
				}
			}
		}
		if !found {
			out = append(out, n)
		}
	}
	return out
}
