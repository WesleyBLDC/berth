package procscan

import (
	"encoding/binary"
	"slices"
	"testing"
)

func procargs(argc uint32, body string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, argc)
	return append(b, body...)
}

func TestParseProcargs(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []string
		bad  bool
	}{
		{"padded exec path", procargs(2, "/bin/chrome\x00\x00\x00\x00/bin/chrome\x00--headless\x00SECRET=canary\x00"), []string{"/bin/chrome", "--headless"}, false},
		{"empty argument kept", procargs(3, "/x\x00/x\x00\x00--y\x00"), []string{"/x", "", "--y"}, false},
		{"no arguments", procargs(0, "/x\x00\x00\x00"), []string{}, false},
		{"truncated argv", procargs(3, "/x\x00/x\x00--y"), nil, true},
		{"no exec path end", procargs(1, "/x"), nil, true},
		{"too short", []byte{1, 0}, nil, true},
		{"huge argc", procargs(1<<30, "/x\x00/x\x00"), nil, true},
	}
	for _, c := range cases {
		got, err := parseProcargs(c.in)
		if (err != nil) != c.bad || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, %v", c.name, got, err)
		}
	}
}
