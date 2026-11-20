package cli

import (
	"strconv"
	"testing"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
)

// nastyText is what the fuzzers start from beyond shownCorpus: invalid UTF-8, encoded
// surrogates, text that already looks quoted, and escapes that end mid sequence.
var nastyText = []string{
	"\xff", "a\xc3", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xc0\xaf", `"`, `""`, `"x\n"`, `"claude"`, `\x1b`, "\"\u202e\"",
	"\"\x1b", "\u0301", "\u0301\u0301", "\U0001f600\u200d\U0001f600", "\u061c", "\u2066rtl\u2069", "\x7f\u0080\u009f",
	"\ufffd", "\U0010ffff", "\U000e007f", "\u00ad", "\u180e", "a\u034f", "\u3000", "\u00a0",
	"\"\u0301", "\"\ufe0f", "\n\u0301", "\\\u0301", "\x1b\u0301\u0301", "\xff\u20dd",
}

func allGraphic(s string) bool {
	for _, r := range s {
		if !unicode.IsGraphic(r) {
			return false
		}
	}
	return utf8.ValidString(s)
}

// FuzzShown checks that shown's output is valid UTF-8 of graphic runes only, and unambiguous:
// text printed as it is never begins with a quote, and quoted text unquotes to the input, so no
// two inputs print the same, and prints no mark joined to its quote or an escape.
func FuzzShown(f *testing.F) {
	for _, s := range append(shownCorpus, nastyText...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := shown(s)
		if !allGraphic(out) {
			t.Fatalf("shown(%q) = %q, which holds a rune that is not graphic", s, out)
		}
		if out == s {
			if s != "" && s[0] == '"' {
				t.Fatalf("shown(%q) prints it as it is, the same as the quoted form of %q", s, unquoted(s))
			}
			return
		}
		if back, err := strconv.Unquote(out); err != nil || back != s {
			t.Fatalf("shown(%q) = %q, which unquotes to %q (%v)", s, out, back, err)
		}
		if markJoinsEscape(out) {
			t.Fatalf("shown(%q) = %q has a mark joined to its quote or an escape", s, out)
		}
	})
}

// markJoinsEscape reads a Go quoted string unit by unit and reports a mark printed as it is
// right after the opening quote or an escape, where it would draw on top of them.
func markJoinsEscape(q string) bool {
	rest := q[1 : len(q)-1]
	afterEscape := true
	for rest != "" {
		r, _, tail, err := strconv.UnquoteChar(rest, '"')
		if err != nil {
			return true
		}
		escape := rest[0] == '\\'
		if !escape && afterEscape && unicode.Is(unicode.M, r) {
			return true
		}
		afterEscape, rest = escape, tail
	}
	return false
}

func unquoted(s string) string {
	u, err := strconv.Unquote(s)
	if err != nil {
		return "nothing"
	}
	return u
}

// FuzzQuoted checks that quoted's output is graphic, unquotes to the input, and prints no mark
// where it would join the opening quote or an escape.
func FuzzQuoted(f *testing.F) {
	for _, s := range append(shownCorpus, nastyText...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := quoted(s)
		if !allGraphic(q) {
			t.Fatalf("quoted(%q) = %q, which holds a rune that is not graphic", s, q)
		}
		if back, err := strconv.Unquote(q); err != nil || back != s {
			t.Fatalf("quoted(%q) = %q, which unquotes to %q (%v)", s, q, back, err)
		}
		if markJoinsEscape(q) {
			t.Fatalf("quoted(%q) = %q has a mark joined to its quote or an escape", s, q)
		}
	})
}

// FuzzGraphicJSON checks that --json output holds only graphic runes and newlines and still
// reads back as the same message.
func FuzzGraphicJSON(f *testing.F) {
	for _, s := range append(shownCorpus, nastyText...) {
		f.Add(s, "/tmp/a\u0085b")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		m := &berthv1.Session{Id: a, UserDataDir: b, Owner: &berthv1.SessionOwner{Name: a + b}}
		raw, err := protojson.MarshalOptions{Multiline: true}.Marshal(m)
		if err != nil {
			return // protojson refuses invalid UTF-8, so --json never sees it
		}
		out := graphicJSON(raw)
		for _, r := range out {
			if r != '\n' && !unicode.IsGraphic(r) {
				t.Fatalf("graphicJSON of %q left %U", raw, r)
			}
		}
		var back berthv1.Session
		if err := protojson.Unmarshal([]byte(out), &back); err != nil || !proto.Equal(&back, m) {
			t.Fatalf("graphicJSON(%q) = %q, which reads back as %v (%v)", raw, out, &back, err)
		}
	})
}
