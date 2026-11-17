package cli

import (
	"bytes"
	"strings"
	"testing"
)

func maskAll(t *testing.T, values []string, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	vs := make([][]byte, len(values))
	for i, v := range values {
		vs[i] = []byte(v)
	}
	m := newMasker(&out, vs)
	for _, c := range chunks {
		if n, err := m.Write([]byte(c)); err != nil || n != len(c) {
			t.Fatalf("write %q: %d, %v", c, n, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestMaskerSplitAtEveryBoundary(t *testing.T) {
	values := []string{"s3cr3t-value", "abc", "abcdef"}
	stream := "x s3cr3t-value y abcdef z abc abcd s3cr3t-valu s3cr3t-value"
	want := "x **** y **** z **** ****d s3cr3t-valu ****"

	if got := maskAll(t, values, stream); got != want {
		t.Fatalf("one write:\n got %q\nwant %q", got, want)
	}
	bytewise := strings.Split(stream, "")
	if got := maskAll(t, values, bytewise...); got != want {
		t.Fatalf("byte by byte:\n got %q\nwant %q", got, want)
	}
	for i := range len(stream) + 1 {
		for j := i; j <= len(stream); j++ {
			if got := maskAll(t, values, stream[:i], stream[i:j], stream[j:]); got != want {
				t.Fatalf("split at %d,%d:\n got %q\nwant %q", i, j, got, want)
			}
		}
	}
}

func TestMaskerLeavesOtherOutputAlone(t *testing.T) {
	values := []string{"s3cr3t-value"}
	for _, stream := range []string{"", "plain output\n", "almost s3cr3t-valu", "ends with a prefix s3cr3t"} {
		if got := maskAll(t, values, strings.Split(stream, "")...); got != stream {
			t.Fatalf("got %q, want %q", got, stream)
		}
	}
}

func TestMaskerZeroesItsCopies(t *testing.T) {
	m := newMasker(&bytes.Buffer{}, [][]byte{[]byte("s3cr3t")})
	held := m.values[0]
	_ = m.Close()
	if !bytes.Equal(held, make([]byte, len(held))) {
		t.Fatalf("value copy not zeroed: %q", held)
	}
}
