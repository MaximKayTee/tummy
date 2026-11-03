package engine

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"tummy/internal/api"
)

// Watermark handling for API streams: how the last value is sent, how records
// compare against it, and when paging can stop.

var timeLayouts = []string{
	time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02",
}

// asTime parses the values tummy meets: time.Time, ISO strings, epoch seconds or millis.
func asTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case string:
		s := strings.TrimSpace(x)
		for _, l := range timeLayouts {
			if t, err := time.Parse(l, s); err == nil {
				return t, true
			}
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return epoch(n)
		}
	case int64:
		return epoch(float64(x))
	case float64:
		return epoch(x)
	case json.Number:
		if n, err := x.Float64(); err == nil {
			return epoch(n)
		}
	}
	return time.Time{}, false
}

// epoch reads seconds, or millis above 1e11.
func epoch(n float64) (time.Time, bool) {
	switch {
	case n > 1e11:
		return time.UnixMilli(int64(n)), true
	case n > 1e8:
		return time.Unix(int64(n), 0), true
	}
	return time.Time{}, false
}

func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		n, err := x.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n, err == nil
	case []byte:
		n, err := strconv.ParseFloat(strings.TrimSpace(string(x)), 64)
		return n, err == nil
	}
	return 0, false
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// compare orders two update-key values: numbers, then times, then text. Two numbers
// compare as numbers, not as epoch times. Returns -1, 0, 1; ok=false for nil.
func compare(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	_, ta := a.(time.Time)
	_, tb := b.(time.Time)
	if !ta && !tb {
		if ia, ok := asInt(a); ok {
			if ib, ok := asInt(b); ok {
				return cmp.Compare(ia, ib), true
			}
		}
		if na, ok := asNumber(a); ok {
			if nb, ok := asNumber(b); ok {
				return cmp.Compare(na, nb), true
			}
		}
	}
	if ta, ok := asTime(a); ok {
		if tb, ok := asTime(b); ok {
			return ta.Compare(tb), true
		}
	}
	sa, sb := fmt.Sprint(a), fmt.Sprint(b)
	if _, isT := a.(time.Time); isT {
		sa = formatWatermark(a, "rfc3339")
	}
	return strings.Compare(sa, sb), true
}

// isNumber reports ints, floats and numeric text.
func isNumber(v any) bool {
	if _, ok := v.(time.Time); ok {
		return false
	}
	_, ok := asNumber(v)
	return ok
}

// formatWatermark renders the watermark for a since parameter or {watermark}.
// By default numbers go as they are and times as RFC 3339.
func formatWatermark(v any, format string) string {
	if format == "raw" || format == "" && isNumber(v) {
		return formatValue(v)
	}
	t, ok := asTime(v)
	if !ok {
		return formatValue(v)
	}
	switch format {
	case "date":
		return t.UTC().Format("2006-01-02")
	case "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case "unix_ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return t.UTC().Format(time.RFC3339)
}

// field reads a record key, exact first, then case-insensitively.
func field(r api.Record, key string) any {
	if v, ok := r[key]; ok {
		return v
	}
	for k, v := range r {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

// newerThan returns the records whose key is above the watermark, or at it when inclusive.
// Records without a key are left out, as in SQL.
func newerThan(recs []api.Record, key string, watermark any, inclusive bool) []api.Record {
	if watermark == nil {
		return recs
	}
	var out []api.Record
	for _, r := range recs {
		if c, ok := compare(field(r, key), watermark); ok && (c > 0 || inclusive && c == 0) {
			out = append(out, r)
		}
	}
	return out
}

// reachedWatermark reports whether a newest-first page has passed the watermark:
// once a record is below it (or at it, when not inclusive), every later page is older.
func reachedWatermark(page []api.Record, key string, watermark any, inclusive bool) bool {
	if watermark == nil {
		return false
	}
	for _, r := range page {
		if c, ok := compare(field(r, key), watermark); ok && (c < 0 || !inclusive && c == 0) {
			return true
		}
	}
	return false
}

// oldestFirst sorts a copy of recs by key, ascending.
func oldestFirst(recs []api.Record, key string) []api.Record {
	out := slices.Clone(recs)
	slices.SortStableFunc(out, func(a, b api.Record) int {
		c, _ := compare(field(a, key), field(b, key))
		return c
	})
	return out
}
