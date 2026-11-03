package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// Record is one flattened JSON object.
type Record map[string]any

// JSONText is an array or object kept as JSON.
type JSONText string

// MarshalJSON writes the text unchanged.
func (j JSONText) MarshalJSON() ([]byte, error) { return []byte(j), nil }

// toRecords turns the selected value into records: every element of an
// array, else one record. keys are the record keys in first-seen order; objects
// without a known order contribute theirs sorted.
func toRecords(v any, order keyOrder) ([]Record, []string, error) {
	f := flattener{order: order, seen: map[string]bool{}}
	switch x := v.(type) {
	case nil:
		return nil, nil, nil
	case []any:
		out := make([]Record, 0, len(x))
		for _, el := range x {
			r, err := f.record(el)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, r)
		}
		return out, f.keys, nil
	default:
		r, err := f.record(x)
		if err != nil {
			return nil, nil, err
		}
		return []Record{r}, f.keys, nil
	}
}

type flattener struct {
	order keyOrder
	keys  []string
	seen  map[string]bool
}

func (f *flattener) record(v any) (Record, error) {
	m, ok := v.(map[string]any)
	if !ok {
		f.add("value")
		return Record{"value": value(v)}, nil
	}
	r := Record{}
	return r, f.flatten("", m, r)
}

func (f *flattener) add(k string) {
	if !f.seen[k] {
		f.seen[k] = true
		f.keys = append(f.keys, k)
	}
}

// flatten turns nested objects into parent_child keys. Two fields with the
// same flattened name are an error.
func (f *flattener) flatten(prefix string, m map[string]any, out Record) error {
	keys := f.order.of(m)
	if len(keys) != len(m) {
		keys = slices.Sorted(maps.Keys(m))
	}
	for _, k := range keys {
		name := k
		if prefix != "" {
			name = prefix + "_" + k
		}
		if x, ok := m[k].(map[string]any); ok && len(x) > 0 {
			if err := f.flatten(name, x, out); err != nil {
				return err
			}
			continue
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("two fields flatten to %q", name)
		}
		out[name] = value(m[k])
		f.add(name)
	}
	return nil
}

// value: arrays and objects become JSONText, numbers int64, float64 or json.Number.
func value(v any) any {
	switch x := v.(type) {
	case json.Number:
		return number(x)
	case []any:
		return jsonText(x)
	case map[string]any:
		if len(x) == 0 {
			return nil
		}
		return jsonText(x)
	}
	return v
}

// number keeps json.Number when int64 or float64 would lose digits.
func number(n json.Number) any {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !exact(s, f) {
		return n
	}
	return f
}

// exact reports whether f's shortest form has the same value as s.
func exact(s string, f float64) bool {
	if len(s) > 40 {
		return false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		if e, err := strconv.Atoi(s[i+1:]); err != nil || e > 400 || e < -400 {
			return false
		}
	}
	a, ok := new(big.Rat).SetString(s)
	if !ok {
		return false
	}
	b, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	return ok && a.Cmp(b) == 0
}

func jsonText(v any) JSONText {
	b, err := marshal(v)
	if err != nil {
		return JSONText("null")
	}
	return JSONText(b)
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
