package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"tummy/internal/steps"
)

// Modes are the load modes a stream may use.
var Modes = []string{"full-refresh", "truncate", "incremental", "backfill", "snapshot"}

var (
	paginateList = []string{"page", "offset", "cursor", "link"}
	methodList   = []string{"GET", "POST", "PUT", "PATCH"}
	sinceFormats = []string{"rfc3339", "date", "unix", "unix_ms", "raw"}
	topOnlyKeys  = []string{"source", "target", "schedule", "enabled", "exclude", "on_error"}
)

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// orList renders "a, b or c".
func orList(list []string) string {
	if len(list) < 2 {
		return strings.Join(list, "")
	}
	return strings.Join(list[:len(list)-1], ", ") + " or " + list[len(list)-1]
}

// keysOf lists the TOML keys of a struct, embedded structs included.
func keysOf(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, keysOf(reflect.Zero(f.Type).Interface())...)
			continue
		}
		if k := tomlKey(f); k != "" && f.IsExported() {
			out = append(out, k)
		}
	}
	return out
}

// unknownKeyMsg describes an unknown key, naming the closest known one.
func unknownKeyMsg(k string, known []string) string {
	switch k {
	case "source_options", "target_options", "fetch_size", "concurrency":
		return k + ": not supported"
	}
	best, bestD := "", 3
	for _, c := range known {
		if d := editDistance(k, c); d < bestD && d < len(k) {
			best, bestD = c, d
		}
	}
	if best != "" {
		return fmt.Sprintf("unknown key %q (%s?)", k, best)
	}
	return fmt.Sprintf("unknown key %q", k)
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			// transposition counts as one edit
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], prev[j-2+1]-1+1)
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// undecodedErr explains keys the replication decoder did not use.
func undecodedErr(md toml.MetaData) error {
	var msgs []string
	done := map[string]bool{}
	add := func(id, msg string) {
		if !done[id] {
			done[id] = true
			msgs = append(msgs, msg)
		}
	}
	for _, k := range md.Undecoded() {
		switch {
		case k[0] == "defaults":
			add("defaults", "[defaults]: defaults are top-level keys, above the first [streams.NAME] header")
		case k[0] == "streams" && len(k) >= 3:
			id := strings.Join(k[:3], "\x00")
			switch {
			case md.Type(k[:3]...) == "Hash":
				add(id, fmt.Sprintf(`[streams.%s.%s]: quote names that contain dots, as in [streams."%s.%s"]`, k[1], k[2], k[1], k[2]))
			case contains(topOnlyKeys, k[2]):
				add(id, fmt.Sprintf("stream %s: %s is a top-level key: move it above the first [streams.NAME] header", k[1], k[2]))
			default:
				add(id, fmt.Sprintf("stream %s: %s", k[1], unknownKeyMsg(k[2], keysOf(Stream{}))))
			}
		case k[0] == "streams":
			add("streams", "streams: want [streams.NAME] tables")
		default:
			add(k[0], unknownKeyMsg(k[0], keysOf(Replication{})))
		}
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

// validate checks the schedule and every stream. raw skips format checks on values that
// still hold ${VAR} references.
func (r *Replication) validate(raw bool) error {
	if r.Schedule != "" && !(raw && hasEnvRef(r.Schedule)) {
		if _, err := ParseSchedule(r.Schedule); err != nil {
			return err
		}
	}
	for i := range r.Resolved {
		if err := validateStream(r.Resolved[i].Name, &r.Resolved[i].Stream, raw); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStream checks one stream as LoadReplication does.
func ValidateStream(name string, s *Stream) error {
	if s.Mode == "" {
		s.Mode = defaultMode
	}
	return validateStream(name, s, false)
}

// controlIn is the first control character of s. Newlines and tabs are allowed in
// multi-line values. A NUL from a terminal hides in a name and only fails on the database.
func controlIn(s string, multiline bool) (rune, bool) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			if multiline && (r == '\n' || r == '\t' || r == '\r') {
				continue
			}
			return r, true
		}
	}
	return 0, false
}

// checkControls rejects control characters in the string fields of v, a struct.
func checkControls(v reflect.Value, multiline ...string) error {
	for _, f := range fieldsOf(v) {
		if f.v.Kind() != reflect.String {
			continue
		}
		if r, ok := controlIn(f.v.String(), slices.Contains(multiline, f.key)); ok {
			return fmt.Errorf("%s: control character %U in the value", f.key, r)
		}
	}
	return nil
}

// ParseForEachSince splits "field, value" into the parent field and the lower bound.
// A value of the form Nd is N days before now.
func ParseForEachSince(v string) (field string, since any, err error) {
	f, val, ok := strings.Cut(v, ",")
	f, val = strings.TrimSpace(f), strings.TrimSpace(val)
	if !ok || f == "" || val == "" {
		return "", nil, fmt.Errorf(`for_each_since %q: want "field, value"`, v)
	}
	if n, found := strings.CutSuffix(val, "d"); found {
		if days, err := strconv.Atoi(n); err == nil && days >= 0 {
			return f, time.Now().UTC().AddDate(0, 0, -days), nil
		}
	}
	return f, val, nil
}

func validateStream(name string, s *Stream, raw bool) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("stream %s: "+format, append([]any{name}, args...)...)
	}
	check := func(v string) bool { return v != "" && !(raw && hasEnvRef(v)) }
	if r, ok := controlIn(name, false); ok {
		return fail("control character %U in the name", r)
	}
	if err := checkControls(reflect.ValueOf(*s), "sql", "steps", "body"); err != nil {
		return fail("%v", err)
	}
	if !contains(Modes, s.Mode) {
		return fail("unknown mode %q (%s)", s.Mode, orList(Modes))
	}
	if s.SQL != "" && s.Object == "" {
		return fail("sql needs object")
	}
	if s.Path != "" && s.SQL != "" {
		return fail("path and sql cannot both be set")
	}
	if strings.ContainsAny(name, "*?[") {
		switch {
		case s.SQL != "":
			return fail("a table pattern cannot have sql")
		case s.Path != "":
			return fail("a table pattern cannot have path")
		case s.Object != "" && !strings.Contains(s.Object, "{stream_table}") && !strings.Contains(s.Object, "{stream_name}"):
			return fail("object %q needs {stream_table} or {stream_name}", s.Object)
		}
	}
	if s.Mode == "backfill" && (s.UpdateKey == "" || s.Range == "") {
		return fail(`mode = "backfill" needs update_key and range`)
	}
	if s.Range != "" && s.Mode != "backfill" {
		return fail(`range needs mode = "backfill"`)
	}
	if check(s.Range) && !strings.Contains(s.Range, ",") {
		return fail(`range %q: want "lo,hi"`, s.Range)
	}
	if s.Mode == "incremental" && s.UpdateKey == "" && len(s.PrimaryKey) == 0 {
		return fail("incremental requires primary_key or update_key")
	}
	if s.Steps != "" {
		if s.Path != "" {
			return fail("steps need a database source")
		}
		if check(s.Steps) {
			if _, err := steps.Parse(s.Steps); err != nil {
				return fail("steps: %w", err)
			}
		}
	}
	if s.ForEach != "" && s.Path == "" {
		return fail("for_each needs path")
	}
	if s.ForEach == name {
		return fail("for_each names the stream itself")
	}
	if s.SkipExisting != "" && s.ForEach == "" {
		return fail("skip_existing needs for_each")
	}
	if s.ForEachSince != "" {
		if s.ForEach == "" {
			return fail("for_each_since needs for_each")
		}
		if _, _, err := ParseForEachSince(s.ForEachSince); err != nil {
			return fail("%v", err)
		}
	}
	if s.SinceFormat != "" && !contains(sinceFormats, s.SinceFormat) {
		return fail("since_format %q: want %s", s.SinceFormat, orList(sinceFormats))
	}
	if s.Sorted != "" && s.Sorted != "desc" && s.Sorted != "asc" {
		return fail("sorted %q: want asc or desc", s.Sorted)
	}
	if s.Paginate != "" && !contains(paginateList, s.Paginate) {
		return fail("paginate %q: want %s", s.Paginate, orList(paginateList))
	}
	if s.Paginate == "cursor" && s.CursorField == "" {
		return fail(`paginate = "cursor" needs cursor_field`)
	}
	if check(s.Method) && !contains(methodList, strings.ToUpper(s.Method)) {
		return fail("method %q: want %s", s.Method, orList(methodList))
	}
	if s.ParamsIn != "" && s.ParamsIn != "query" && s.ParamsIn != "body" {
		return fail("params_in %q: want query or body", s.ParamsIn)
	}
	if check(s.QueryTimeout) {
		if d, err := time.ParseDuration(s.QueryTimeout); err != nil || d <= 0 {
			return fail("query_timeout %q: want a duration such as 30s or 5m", s.QueryTimeout)
		}
	}
	if s.Limit < 0 || s.PageSize < 0 {
		return fail("limit and page_size cannot be negative")
	}
	drop := 0
	for _, c := range s.Select {
		if strings.TrimPrefix(c, "-") == "" {
			return fail("select: empty column name")
		}
		if strings.HasPrefix(c, "-") {
			drop++
		}
	}
	if drop > 0 && drop < len(s.Select) {
		return fail(`select: either columns to keep or "-column"s to drop, not both`)
	}
	for _, c := range s.PrimaryKey {
		if c == "" {
			return fail("primary_key: empty column name")
		}
	}
	return nil
}
