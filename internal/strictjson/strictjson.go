// Package strictjson decodes JSON another program wrote for Berth to act on: exactly one value,
// no field Berth does not know and nothing after it, so no two readers can see different things.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Decode reads b into into, refusing unknown fields and anything but white space after the value.
// Where Go's decoder is more lenient than other readers it refuses too: a key repeated in one
// object (Go keeps the last, other readers the first), a key that names a field only when case
// is ignored (Go matches it, other readers do not), and invalid UTF-8 or a lone surrogate escape
// (Go reads U+FFFD, other readers refuse or keep the bytes).
func Decode(b []byte, into any) error {
	if !utf8.Valid(b) {
		return errors.New("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	if loneSurrogate(b) {
		return errors.New("a lone surrogate escape")
	}
	return checkValue(json.NewDecoder(bytes.NewReader(b)), reflect.TypeOf(into))
}

// loneSurrogate reports a \u escape of half a surrogate pair without its other half. b is valid
// JSON, so every backslash starts an escape inside a string.
func loneSurrogate(b []byte) bool {
	escape := func(i int) rune {
		if i+6 > len(b) || b[i] != '\\' || b[i+1] != 'u' {
			return -1
		}
		n, err := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
		if err != nil {
			return -1
		}
		return rune(n)
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		r := escape(i)
		switch {
		case r < 0:
			i++
		case utf16.IsSurrogate(r):
			if utf16.DecodeRune(r, escape(i+6)) == utf8.RuneError {
				return true
			}
			i += 11
		default:
			i += 5
		}
	}
	return false
}

// checkValue walks the next value alongside t, the Go type it was decoded into; nil means a
// value whose keys only need to be unique.
func checkValue(dec *json.Decoder, t reflect.Type) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		t = nil
	}
	switch tok {
	case json.Delim('{'):
		return checkObject(dec, t)
	case json.Delim('['):
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for dec.More() {
			if err := checkValue(dec, elem); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return nil
}

func checkObject(dec *json.Decoder, t reflect.Type) error {
	var fields map[string]reflect.Type
	var elem reflect.Type
	switch {
	case t == nil:
	case t.Kind() == reflect.Struct:
		fields = fieldsOf(t, map[string]reflect.Type{})
	case t.Kind() == reflect.Map:
		elem = t.Elem()
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("key %q repeats", key)
		}
		seen[key] = true
		child := elem
		if fields != nil {
			ft, exact := fields[key]
			if !exact {
				for name := range fields {
					if strings.EqualFold(name, key) {
						return fmt.Errorf("key %q names field %q only when case is ignored", key, name)
					}
				}
			}
			child = ft
		}
		if err := checkValue(dec, child); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// fieldsOf adds the JSON names encoding/json reads into struct t, with their types, embedded
// structs' fields included, to into.
func fieldsOf(t reflect.Type, into map[string]reflect.Type) map[string]reflect.Type {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			fieldsOf(ft, into)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		into[name] = f.Type
	}
	return into
}
