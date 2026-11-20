package cdppolicy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"berth/internal/core/cdppolicy"
)

// knownAt are the keys the policy reads, by level, as the fuzzer's own copy: a fuzzer that
// shared the package's table would agree with any mistake in it.
var knownAt = map[string][]string{
	"": {"id", "method", "sessionId", "params"},
	"params": {
		"url", "flatten", "sessionId", "targetId", "commands", "data", "proxyServer", "proxyBypassList",
		"key", "code", "modifiers", "windowsVirtualKeyCode", "entryId",
	},
	"params.data": {"files"},
}

// parseSeeds are today's unit test inputs and the nastiest messages they lead to.
var parseSeeds = []string{
	`{"id":7,"method":"Page.navigate","sessionId":"S","params":{"url":"data:,"}}`,
	`[]`, `{"method":"Page.enable"}`, `{"id":1}`, `{"id":1.5,"method":"Page.enable"}`, `{"id":"1","method":"Page.enable"}`,
	`{"id":1,"method":7}`, `not json`, `{"id":1,"method":"Page.enable"} {}`, `{"id":1,"method":"Page.enable","params":[1]}`,
	`{"id":1,"method":"Browser.setDownloadBehavior","params":{"behavior":"allow","Behavior":"deny"}}`,
	`{"id":1,"method":"Page.enable","method":"Browser.close"}`,
	`{"id":1,"id":2,"method":"Page.enable"}`,
	`{"id":1,"method":"Page.navigate","params":{"url":"https://a.example","url":"file:///etc/passwd"}}`,
	`{"id":1,"method":"Runtime.evaluate","params":{"expression":"1","x":{"a":1,"a":2}}}`,
	`{"id":1,"method":"Page.enable","SessionId":"AGENT"}`,
	`{"id":1,"method":"Page.enable","sessionId":"AGENT","SESSIONID":"BERTH-SESSION"}`,
	`{"id":1,"Method":"Page.enable","method":"Browser.close"}`,
	`{"id":1,"method":"Page.navigate","params":{"URL":"file:///etc/passwd"}}`,
	`{"id":1,"method":"Target.attachToTarget","params":{"targetId":"T","Flatten":true}}`,
	`{"id":1,"method":"Input.dispatchDragEvent","params":{"type":"drop","data":{"Files":["/etc/passwd"]}}}`,
	`{"id":1,"method":"Page.navigateToHistoryEntry","params":{"entryId":1,"EntryId":2}}`,
	`{"id":1,"method":"Input.dispatchKeyEvent","params":{"type":"rawKeyDown","key":"Insert","modifiers":8,"Modifiers":0}}`,
	"{\"id\":1,\"method\":\"Page.enable\",\"ſessionId\":\"AGENT\"}",
	"{\"id\":1,\"method\":\"Page.enable\",\"\\u017fessionId\":\"AGENT\"}",
	`{"id":1,"method":"Page.enable","\u0073essionId":"A","sessionId":"B"}`,
	"{\"id\":1,\"method\":\"Input.dispatchKeyEvent\",\"params\":{\"\u212aey\":\"Insert\",\"modifiers\":8}}",
	`{"id":1,"method":"Target.attachToTarget","params":{"targetId":"T","flatten":true},"sessionId":"BERTH-SESSION"}`,
	`{"id":999999999,"method":"Page.enable"}`, `{"id":1000000000,"method":"Page.enable"}`, `{"id":-1,"method":"Page.enable"}`,
	`{"id":9223372036854775808,"method":"Page.enable"}`, `{"id":1e3,"method":"Page.enable"}`, `{"id":-0,"method":"Page.enable"}`,
	`{"id":1,"method":"Page.navigate","params":null}`, `{"id":1,"method":"Page.navigate","params":{"url":" FILE:///x"}}`,
	"{\"id\":1,\"method\":\"Page.navigate\",\"params\":{\"url\":\"\\tfi\\nle:///x\"}}",
	`{"id":1,"method":"Input.dispatchKeyEvent","params":{"commands":["Paste"]}}`,
	`{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":null}}`,
	`{"id":1,"method":"Page.enable","params":{"x":[[[[[[[[[[[]]]]]]]]]]]}}`,
	// Invalid UTF-8 and lone surrogates that once passed are in testdata/fuzz/FuzzParse.
	`{"id":1,"method":"Page.enable","x":"\ud83d\ude00","y":"\\ud800"}`,
	`{"id":1,"method":"Page.enable",}`, `{"id":1,"method":"Page.enable"}` + "\n", ` {"id":1,"method":"Page.enable"}`,
}

// FuzzParse checks that Parse never panics, gives the same answer twice, accepts only messages
// with no key repeated anywhere and no case folded known key, stays within its limits and
// allocates in proportion to the message, and that Check reads an accepted message the same
// way after it is encoded again.
func FuzzParse(f *testing.F) {
	for _, s := range parseSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := cdppolicy.Parse(raw)
		m2, err2 := cdppolicy.Parse(raw)
		if (err == nil) != (err2 == nil) || !sameMessage(m, m2) {
			t.Fatalf("%q parsed as %+v %v, then %+v %v", raw, m, err, m2, err2)
		}
		if err != nil && !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Fatalf("%q refused with %v, which is not ErrMalformed", raw, err)
		}
		// The bound is generous; a quadratic walk passes it within a few hundred bytes.
		if allocs := testing.AllocsPerRun(1, func() { _, _ = cdppolicy.Parse(raw) }); allocs > float64(64+2*len(raw)) {
			t.Fatalf("%d bytes took %.0f allocations", len(raw), allocs)
		}
		if err != nil {
			return
		}
		if !utf8.Valid(raw) || !pairedOnly(raw) {
			t.Fatalf("accepted %q, which holds invalid UTF-8 or a lone surrogate escape", raw)
		}
		walkAccepted(t, raw)
		for _, own := range []func(string) bool{ownOnly, func(string) bool { return false }} {
			why, again := cdppolicy.Check(m, own), cdppolicy.Check(m, own)
			if why != again {
				t.Fatalf("%q checked as %q, then %q", raw, why, again)
			}
			re, err := cdppolicy.Parse(reencode(t, m))
			if err != nil || !sameMessage(re, m) || cdppolicy.Check(re, own) != why {
				t.Fatalf("%q encoded again parses as %+v %v, checked %q, want %+v checked %q", raw, re, err, cdppolicy.Check(re, own), m, why)
			}
		}
	})
}

func sameMessage(a, b cdppolicy.Message) bool {
	return a.ID == b.ID && a.Method == b.Method && a.SessionID == b.SessionID && compact(a.Params) == compact(b.Params)
}

// compact reads raw JSON back by meaning, so two spellings of one value compare equal.
func compact(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func reencode(t *testing.T, m cdppolicy.Message) []byte {
	t.Helper()
	out := map[string]any{"id": m.ID, "method": m.Method}
	if m.SessionID != "" {
		out["sessionId"] = m.SessionID
	}
	if len(m.Params) > 0 {
		out["params"] = m.Params
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// walkAccepted reads an accepted message with the fuzzer's own walk: no object repeats a key, no
// two keys at a level the policy reads are equal but for case, none of them is a known key in
// another case, and nothing nests past MaxDepth or holds more than MaxKeys keys.
func walkAccepted(t *testing.T, raw []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		if depth > cdppolicy.MaxDepth {
			t.Fatalf("accepted %q nested past %d", raw, cdppolicy.MaxDepth)
		}
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("accepted %q, which does not tokenize: %v", raw, err)
		}
		switch tok {
		case json.Delim('{'):
			var keys []string
			for dec.More() {
				k, _ := dec.Token()
				key, _ := k.(string)
				if slices.Contains(keys, key) {
					t.Fatalf("accepted %q with %q twice", raw, key)
				}
				if known, read := knownAt[path]; read {
					for _, other := range append(slices.Clone(known), keys...) {
						if other != key && strings.EqualFold(other, key) {
							t.Fatalf("accepted %q with %q and %q at %q", raw, key, other, path)
						}
					}
				}
				keys = append(keys, key)
				if len(keys) > cdppolicy.MaxKeys {
					t.Fatalf("accepted %q with more than %d keys", raw, cdppolicy.MaxKeys)
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				walk(child, depth+1)
			}
			_, _ = dec.Token()
		case json.Delim('['):
			for dec.More() {
				walk(path+"[]", depth+1)
			}
			_, _ = dec.Token()
		}
	}
	walk("", 0)
}

var escapes = regexp.MustCompile(`\\(u[0-9a-fA-F]{4}|.)`)

// pairedOnly reports whether every surrogate escape in raw is a high one right before a low one,
// read from the escapes left to right, so an escaped backslash is never taken for an escape.
func pairedOnly(raw []byte) bool {
	ms := escapes.FindAllIndex(raw, -1)
	surrogate := func(k int) uint64 {
		if k >= len(ms) || ms[k][1]-ms[k][0] != 6 {
			return 0
		}
		n, _ := strconv.ParseUint(string(raw[ms[k][0]+2:ms[k][1]]), 16, 32)
		return n
	}
	for k := 0; k < len(ms); k++ {
		switch r := surrogate(k); {
		case r >= 0xd800 && r < 0xdc00:
			lo := surrogate(k + 1)
			if lo < 0xdc00 || lo > 0xdfff || ms[k+1][0] != ms[k][1] {
				return false
			}
			k++
		case r >= 0xdc00 && r <= 0xdfff:
			return false
		}
	}
	return true
}

// FuzzParseRefusesKeyClashes builds a message with a key repeated at any level, or a known key
// in another case at a level the policy reads, and checks that Parse refuses it however the key
// is spelled or escaped.
func FuzzParseRefusesKeyClashes(f *testing.F) {
	f.Add("x", uint8(0), uint64(0), false)
	f.Add("sessionId", uint8(0), uint64(1), true)
	f.Add("url", uint8(1), uint64(0b101), true)
	f.Add("files", uint8(2), uint64(1), true)
	f.Add("ſ", uint8(3), uint64(0), false)
	f.Add("\u212a", uint8(1), uint64(7), false)
	f.Add("a\x00b", uint8(4), uint64(0), false)
	f.Fuzz(func(t *testing.T, key string, level uint8, flips uint64, variant bool) {
		paths := []string{"", "params", "params.data", "params.deep", "params.list[]"}
		path := paths[int(level)%len(paths)]
		first, second := key, key
		if variant {
			known := knownAt[path]
			if known == nil {
				return
			}
			first = known[int(flips>>32)%len(known)]
			second = flipCase(first, flips)
			if second == first {
				return
			}
		}
		keyJSON := func(k string) string {
			b, _ := json.Marshal(k)
			return string(b)
		}
		pair := keyJSON(first) + `:1,` + keyJSON(second) + `:2`
		var raw string
		switch path {
		case "":
			raw = `{"id":1,"method":"Runtime.evaluate",` + pair + `}`
		case "params":
			raw = `{"id":1,"method":"Runtime.evaluate","params":{` + pair + `}}`
		case "params.data":
			raw = `{"id":1,"method":"Input.dispatchDragEvent","params":{"data":{` + pair + `}}}`
		case "params.deep":
			raw = `{"id":1,"method":"Runtime.evaluate","params":{"a":{"b":{` + pair + `}}}}`
		default:
			raw = `{"id":1,"method":"Runtime.evaluate","params":{"list":[{},{` + pair + `}]}}`
		}
		if _, err := cdppolicy.Parse([]byte(raw)); !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Fatalf("%s parsed (%v); a repeated or case folded key must be refused", raw, err)
		}
	})
}

// flipCase replaces each rune whose bit in flips is set by the next rune in its case folding
// orbit, so the result folds the same as s.
func flipCase(s string, flips uint64) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		if flips>>(i%32)&1 == 1 {
			r = unicode.SimpleFold(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FuzzCheck checks that Check never panics, gives the same answer twice, never passes an id
// outside the agent range, Berth's own session or a method no list names, and loads only web
// URLs.
func FuzzCheck(f *testing.F) {
	for _, s := range parseSeeds {
		if m, err := cdppolicy.Parse([]byte(s)); err == nil {
			f.Add(m.ID, m.Method, m.SessionID, []byte(m.Params))
		}
	}
	f.Add(int64(1), "Page.navigate", "", []byte(`{"url":"javascript:alert(1)"}`))
	f.Add(int64(1), "Target.createTarget", "", []byte(`{"url":"chrome://settings"}`))
	f.Add(int64(1), "Fetch.continueRequest", "", []byte(`{"url":"file:///etc/passwd"}`))
	f.Add(int64(1), "Target.detachFromTarget", "", []byte(`{"sessionId":"BERTH-SESSION"}`))
	f.Add(int64(1), "Input.dispatchKeyEvent", "", []byte(`{"key":"Delete","modifiers":8}`))
	f.Add(int64(1), "Input.dispatchKeyEvent", "", []byte(`{"commands":"paste"}`))
	f.Add(int64(1), "SystemInfo.getInfo", "", []byte(nil))
	f.Add(int64(1), ".", "", []byte(nil))
	f.Add(int64(1), "Page.navigate.extra", "", []byte(`{"url":"https://a"}`))
	f.Fuzz(func(t *testing.T, id int64, method, session string, params []byte) {
		m := cdppolicy.Message{ID: id, Method: method, SessionID: session, Params: params}
		why := cdppolicy.Check(m, ownOnly)
		if again := cdppolicy.Check(m, ownOnly); why != again {
			t.Fatalf("%+v checked as %q, then %q", m, why, again)
		}
		if why != "" {
			return
		}
		domain, name, _ := strings.Cut(method, ".")
		switch {
		case id < 0 || id >= cdppolicy.FirstBerthID:
			t.Fatalf("passed id %d", id)
		case session == "BERTH-SESSION":
			t.Fatalf("passed Berth's own session")
		case cdppolicy.Denied[method] != "" || cdppolicy.Denied[domain+".*"] != "":
			t.Fatalf("passed denied %q", method)
		case cdppolicy.Methods[domain] != nil && !slices.Contains(cdppolicy.Methods[domain], name),
			cdppolicy.Methods[domain] == nil && !slices.Contains(cdppolicy.Allowed, domain):
			t.Fatalf("passed %q, which no list names", method)
		}
		var p struct {
			URL *string `json:"url"`
		}
		_ = json.Unmarshal(params, &p)
		if p.URL != nil && *p.URL != "" && slices.Contains([]string{"Page.navigate", "Target.createTarget", "Fetch.continueRequest"}, method) &&
			!cdppolicy.Loadable(*p.URL) {
			t.Fatalf("%s passed with url %q", method, *p.URL)
		}
	})
}

// FuzzLoadable checks that a URL Loadable passes is one net/url, after Chrome's stripping,
// reads with a web scheme too.
func FuzzLoadable(f *testing.F) {
	for _, s := range []string{
		"https://a.example", "about:blank", "ABOUT:BLANK", "data:,", "blob:https://a/1", "file:///etc/passwd",
		" \tfile:///x", "fi\nle:///x", "javascript:alert(1)", "chrome://settings", "http:", ":", "", "1http://a",
		"h\x00ttp://a", "https\u200b://a", "HTTPS://A", "\x1fhttp://a\x1f", "about:blank#x", "view-source:https://a",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, u string) {
		if !cdppolicy.Loadable(u) {
			return
		}
		stripped := strings.TrimFunc(u, func(r rune) bool { return r <= ' ' })
		stripped = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(stripped)
		if strings.EqualFold(stripped, "about:blank") {
			return
		}
		scheme, _, _ := strings.Cut(stripped, ":")
		if !slices.Contains([]string{"http", "https", "data", "blob"}, strings.ToLower(scheme)) {
			t.Fatalf("Loadable(%q) passed scheme %q", u, scheme)
		}
		if parsed, err := url.Parse(stripped); err == nil && !strings.EqualFold(parsed.Scheme, scheme) {
			t.Fatalf("Loadable(%q): net/url reads scheme %q, the policy %q", u, parsed.Scheme, scheme)
		}
	})
}
