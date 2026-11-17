package procscan

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

func procargs(argc uint32, body string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, argc)
	return append(b, body...)
}

// padded is the exec path as the kernel lays it out: NUL terminated, then NUL padded to the
// pointer aligned boundary.
func padded(path string) string {
	n := (len(path) + stringAlign) / stringAlign * stringAlign
	return path + strings.Repeat("\x00", n-len(path))
}

func TestParseProcargs(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []string
		bad  bool
	}{
		{"padded exec path", procargs(2, padded("/bin/chrome")+"/bin/chrome\x00--headless\x00SECRET=canary\x00"), []string{"/bin/chrome", "--headless"}, false},
		{"exec path filling the boundary", procargs(1, padded("/bin/abcdefg")+"x\x00"), []string{"x"}, false},
		{"empty argv[0] keeps the environment out", procargs(2, padded("/bin/sleep")+"\x00"+"5\x00SECRET=canary\x00"), []string{"", "5"}, false},
		{"empty argument kept", procargs(3, padded("/x")+"/x\x00\x00--y\x00"), []string{"/x", "", "--y"}, false},
		{"no arguments", procargs(0, padded("/x")), []string{}, false},
		{"truncated argv", procargs(3, padded("/x")+"/x\x00--y"), nil, true},
		{"text inside the padding", procargs(1, "/x\x00\x00ab\x00\x00\x00\x00y\x00"), nil, true},
		{"buffer ends before the boundary", procargs(1, "/x\x00\x00"), nil, true},
		{"no exec path end", procargs(1, "/x"), nil, true},
		{"too short", []byte{1, 0}, nil, true},
		{"argc past the limit", procargs(maxArgs+1, padded("/x")+"/x\x00"), nil, true},
	}
	for _, c := range cases {
		got, err := parseProcargs(c.in)
		if (err != nil) != c.bad || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, %v", c.name, got, err)
		}
	}
}
