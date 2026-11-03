package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var (
	envRef  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// expandEnv replaces ${VAR} with the environment value. Unset variables are an error.
func expandEnv(s string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := os.LookupEnv(name)
		if !ok && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func hasEnvRef(s string) bool { return envRef.MatchString(s) }

// tomlKey is the key a struct field decodes from, "" for fields without one.
func tomlKey(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	if tag == "-" {
		return ""
	}
	return tag
}

// expandFields expands ${VAR} in every string, string list and string map field of the
// struct ptr points to, embedded structs included. Keys in skip stay as written.
// Slices and maps are replaced, never written through. Errors name the key.
func expandFields(ptr any, skip ...string) error {
	rv := reflect.ValueOf(ptr).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f, fv := rt.Field(i), rv.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && fv.Kind() == reflect.Struct {
			if err := expandFields(fv.Addr().Interface(), skip...); err != nil {
				return err
			}
			continue
		}
		key := tomlKey(f)
		if key == "" || slices.Contains(skip, key) {
			continue
		}
		switch fv.Kind() {
		case reflect.String:
			s, err := expandEnv(fv.String())
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fv.SetString(s)
		case reflect.Slice:
			if fv.Type().Elem().Kind() != reflect.String || fv.IsNil() {
				continue
			}
			out := reflect.MakeSlice(fv.Type(), fv.Len(), fv.Len())
			for j := 0; j < fv.Len(); j++ {
				s, err := expandEnv(fv.Index(j).String())
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				out.Index(j).SetString(s)
			}
			fv.Set(out)
		case reflect.Map:
			if fv.Type().Elem().Kind() != reflect.String || fv.Type().Key().Kind() != reflect.String || fv.IsNil() {
				continue
			}
			out := reflect.MakeMapWithSize(fv.Type(), fv.Len())
			keys := fv.MapKeys()
			sort.Slice(keys, func(a, b int) bool { return keys[a].String() < keys[b].String() })
			for _, k := range keys {
				s, err := expandEnv(fv.MapIndex(k).String())
				if err != nil {
					return fmt.Errorf("%s.%s: %w", key, k.String(), err)
				}
				out.SetMapIndex(k, reflect.ValueOf(s).Convert(fv.Type().Elem()))
			}
			fv.Set(out)
		}
	}
	return nil
}

// deepCopy copies every slice and map field of the struct ptr points to, so the copy
// shares no storage with the original.
func deepCopy(ptr any) {
	rv := reflect.ValueOf(ptr).Elem()
	for i := 0; i < rv.NumField(); i++ {
		fv := rv.Field(i)
		if !rv.Type().Field(i).IsExported() {
			continue
		}
		switch fv.Kind() {
		case reflect.Struct:
			deepCopy(fv.Addr().Interface())
		case reflect.Slice:
			if !fv.IsNil() {
				out := reflect.MakeSlice(fv.Type(), fv.Len(), fv.Len())
				reflect.Copy(out, fv)
				fv.Set(out)
			}
		case reflect.Map:
			if !fv.IsNil() {
				out := reflect.MakeMapWithSize(fv.Type(), fv.Len())
				iter := fv.MapRange()
				for iter.Next() {
					out.SetMapIndex(iter.Key(), iter.Value())
				}
				fv.Set(out)
			}
		}
	}
}

// clone returns a copy that shares no slices or maps with s.
func (s Stream) clone() Stream {
	deepCopy(&s)
	return s
}
