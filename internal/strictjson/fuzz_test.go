package strictjson_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"unicode/utf8"

	"berth/internal/strictjson"
)

type inner struct {
	Name string `json:"name"`
}

// fuzzLine has the shapes Berth's callers decode: plain fields, a nested struct, a list, a map
// of free keys and raw JSON kept for later.
type fuzzLine struct {
	Port   int               `json:"port"`
	Mode   string            `json:"mode"`
	Names  []string          `json:"names"`
	Inner  *inner            `json:"inner"`
	List   []inner           `json:"list"`
	Free   map[string]string `json:"free"`
	Raw    json.RawMessage   `json:"raw"`
	Ignore string            `json:"-"`
}

var structKeys = map[string]bool{"port": true, "mode": true, "names": true, "inner": true, "list": true, "free": true, "raw": true}

// keysOK is the fuzzer's own reading of an accepted document: valid UTF-8 with no lone
// surrogate, no object repeating a key, and only the exact field names at the struct levels.
func keysOK(t *testing.T, b []byte) {
	t.Helper()
	if !utf8.Valid(b) {
		t.Fatalf("accepted invalid UTF-8 %q", b)
	}
	if !surrogatePairsOnly(b) {
		t.Fatalf("accepted a lone surrogate escape in %q", b)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	// level is "top" or "inner" for a struct, "list" for the array of inner, "free" otherwise.
	var walk func(level string)
	walk = func(level string) {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("accepted %q but the token walk failed: %v", b, err)
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				k, _ := dec.Token()
				key, _ := k.(string)
				if seen[key] {
					t.Fatalf("accepted %q with key %q twice", b, key)
				}
				seen[key] = true
				child := "free"
				switch {
				case level == "top" && !structKeys[key], level == "inner" && key != "name":
					t.Fatalf("accepted %q with key %q in the %s struct", b, key, level)
				case level == "top" && key == "inner":
					child = "inner"
				case level == "top" && key == "list":
					child = "list"
				}
				walk(child)
			}
			_, _ = dec.Token()
		case json.Delim('['):
			child := "free"
			if level == "list" {
				child = "inner"
			}
			for dec.More() {
				walk(child)
			}
			_, _ = dec.Token()
		}
	}
	walk("top")
}

// surrogatePairsOnly reports whether every \u escape of a surrogate is a high one followed by a
// low one, read straight from the bytes.
func surrogatePairsOnly(b []byte) bool {
	hexOf := func(s []byte) rune {
		var r rune
		for _, c := range s {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return -1
			}
		}
		return r
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		if b[i+1] != 'u' || i+6 > len(b) {
			i++
			continue
		}
		r := hexOf(b[i+2 : i+6])
		switch {
		case r >= 0xd800 && r < 0xdc00:
			if i+12 > len(b) || b[i+6] != '\\' || b[i+7] != 'u' {
				return false
			}
			lo := hexOf(b[i+8 : i+12])
			if lo < 0xdc00 || lo > 0xdfff {
				return false
			}
			i += 11
		case r >= 0xdc00 && r <= 0xdfff:
			return false
		default:
			i += 5
		}
	}
	return true
}

// FuzzDecode checks that Decode never panics, takes only one well formed value with exactly the
// known keys, each once, and that what it accepts reads back the same after encoding.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, "{\"a\":1}\n", `{"a":1,"b":2}`, `{"a":1}{"a":2}`, `{"a":1}}`, `{"a":1} junk`, ``, `{"a":"one"}`,
		` {"a":1} ` + "\n",
		`{"port":1,"mode":"owner","names":["x"],"inner":{"name":"n"},"list":[{"name":"m"}],"free":{"k":"v"},"raw":{"z":[1]}}`,
		// The repeated, case folded and lone surrogate keys that once passed are in testdata/fuzz.
		`{"mode":"\` + `ud83d\` + `ude00"}`, `{"mode":"\\ud800"}`, `{"mode":"\ud800\` + `u0041"}`, `{"Ignore":"x"}`,
		`{"-":"x"}`, `{"port":1e2}`, `{"port":null}`, `[]`, `null`, `"x"`, `{"inner":null,"list":null}`,
	} {
		f.Add([]byte(s))
	}
	for _, c := range surrogateCases(f) {
		f.Add([]byte(`{"mode":"` + c.Escape + `"}`))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var got fuzzLine
		err := strictjson.Decode(b, &got)
		var again fuzzLine
		if err2 := strictjson.Decode(b, &again); (err == nil) != (err2 == nil) {
			t.Fatalf("%q decoded once with %v, then with %v", b, err, err2)
		}
		if err != nil {
			return
		}
		if !json.Valid(bytes.TrimSpace(b)) {
			t.Fatalf("accepted %q, which is not one JSON value", b)
		}
		keysOK(t, b)
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("accepted %q but cannot encode it: %v", b, err)
		}
		var back fuzzLine
		if err := strictjson.Decode(out, &back); err != nil || !reflect.DeepEqual(normal(back), normal(got)) {
			t.Fatalf("%q read back from %q as %+v, want %+v (%v)", b, out, back, got, err)
		}
	})
}

// normal compacts raw JSON so a reading back compares by meaning.
func normal(l fuzzLine) fuzzLine {
	if string(l.Raw) == "null" {
		l.Raw = nil
	}
	if l.Raw != nil {
		var buf bytes.Buffer
		if json.Compact(&buf, l.Raw) == nil {
			l.Raw = buf.Bytes()
		}
	}
	return l
}

// surrogateCase is one entry of internal/testdata/surrogates.json, the lone surrogate seeds
// strictjson and cdppolicy share.
type surrogateCase struct {
	Escape string `json:"escape"`
	Lone   bool   `json:"lone"`
}

func surrogateCases(tb testing.TB) []surrogateCase {
	tb.Helper()
	raw, err := os.ReadFile("../testdata/surrogates.json")
	if err != nil {
		tb.Fatal(err)
	}
	var file struct {
		Cases []surrogateCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		tb.Fatal(err)
	}
	return file.Cases
}

func TestDecodeRefusesExactlyTheLoneSurrogates(t *testing.T) {
	for _, c := range surrogateCases(t) {
		var got fuzzLine
		if err := strictjson.Decode([]byte(`{"mode":"`+c.Escape+`"}`), &got); (err != nil) != c.Lone {
			t.Errorf("%s: %v, want refused %v", c.Escape, err, c.Lone)
		}
	}
}
