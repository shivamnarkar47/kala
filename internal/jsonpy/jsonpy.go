// Package jsonpy marshals Go values the way Python's json.dumps does with
// ensure_ascii=False and default separators: `{"a": 1, "b": [1, 2]}` — ", "
// and ": " separators, raw UTF-8 output, no HTML escaping.
//
// The kaal wire layer must emit request bodies byte-identical to the Python
// build (P7 parity gate: "wire bodies byte-identical on turn 2+"), so this
// serializer is load-bearing — do not swap it for encoding/json. Map keys are
// sorted (Python preserves insertion order; we only marshal maps where order
// is not parity-critical — tool schemas — while every parity-critical
// structure is a struct whose field order mirrors Python's dict insertion
// order).
package jsonpy

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marshal returns the Python-style JSON encoding of v.
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeValue(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeValue(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case int8:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int16:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float32:
		writeFloat(b, float64(x))
	case float64:
		writeFloat(b, x)
	case []any:
		return writeList(b, x)
	case map[string]any:
		return writeMap(b, x)
	default:
		return writeReflect(b, reflect.ValueOf(v))
	}
	return nil
}

func writeList(b *strings.Builder, items []any) error {
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		if err := writeValue(b, it); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

func writeMap(b *strings.Builder, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		writeString(b, k)
		b.WriteString(": ")
		if err := writeValue(b, m[k]); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// writeReflect handles structs (field order = Python dict insertion order),
// slices/arrays, and pointers.
func writeReflect(b *strings.Builder, rv reflect.Value) error {
	rt := rv.Type()
	switch rt.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		return writeReflect(b, rv.Elem())
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeValue(b, rv.Index(i).Interface()); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case reflect.Struct:
		b.WriteByte('{')
		first := true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			name, omitEmpty := parseTag(f)
			if name == "-" {
				continue
			}
			fv := rv.Field(i)
			if omitEmpty && isEmptyValue(fv) {
				continue
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			writeString(b, name)
			b.WriteString(": ")
			if err := writeValue(b, fv.Interface()); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	}
	return fmt.Errorf("jsonpy: unsupported type %s", rt)
}

func parseTag(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	parts := strings.Split(tag, ",")
	omitEmpty := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitEmpty = true
		}
	}
	if parts[0] == "" {
		return f.Name, omitEmpty
	}
	return parts[0], omitEmpty
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// writeString matches Python's string escaping: only ", \, and control
// characters (< 0x20) are escaped; everything else — including U+2028/29 —
// stays raw UTF-8.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// writeFloat mirrors Python's float repr: shortest round-trip form, with the
// ".0" kept on integral floats and the non-standard NaN/Infinity spellings.
func writeFloat(b *strings.Builder, f float64) {
	switch {
	case math.IsNaN(f):
		b.WriteString("NaN")
	case math.IsInf(f, 1):
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		b.WriteString("-Infinity")
	default:
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		b.WriteString(s)
	}
}

// RuneCount is a tiny helper so callers can mirror Python's rune-based len()
// for token accounting (wire_token_cost: len(json.dumps(...)) // 3).
func RuneCount(b []byte) int {
	return utf8.RuneCount(b)
}
