package cli

import (
	"strconv"
	"testing"
	"unicode"
	"unicode/utf8"
)

// nastyText is what the fuzzers start from beyond shownCorpus: invalid UTF-8, encoded
// surrogates, text that already looks quoted, and escapes that end mid sequence.
var nastyText = []string{
	"\xff", "a\xc3", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xc0\xaf", `"`, `""`, `"x\n"`, `"claude"`, `\x1b`, "\"\u202e\"",
	"\"\x1b", "\u0301", "\u0301\u0301", "\U0001f600\u200d\U0001f600", "\u061c", "\u2066rtl\u2069", "\x7f\u0080\u009f",
	"\ufffd", "\U0010ffff", "\U000e007f", "\u00ad", "\u180e", "a\u034f", "\u3000", "\u00a0",
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
// two inputs print the same.
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
	})
}

func unquoted(s string) string {
	u, err := strconv.Unquote(s)
	if err != nil {
		return "nothing"
	}
	return u
}

// FuzzQuoted checks that quoted's output is graphic, unquotes to the input, and opens with no
// mark that would join its quote.
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
		if r, _ := utf8.DecodeRuneInString(q[1:]); unicode.Is(unicode.M, r) {
			t.Fatalf("quoted(%q) = %q opens with a mark", s, q)
		}
	})
}
