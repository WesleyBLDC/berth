package procscan

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// maxArgs bounds what a hostile process can make berthd allocate by passing a huge argc.
const maxArgs = 4096

var errBadProcargs = errors.New("malformed kern.procargs2 buffer")

// stringAlign is how the kernel pads the exec path: argv starts at the next multiple of the
// pointer size, counted from the end of argc (checked on macOS 26.5 for paths of every length
// modulo 8).
const stringAlign = 8

// parseProcargs reads the arguments out of a kern.procargs2 buffer: a 32-bit argc, the exec path,
// NUL padding up to the pointer aligned boundary, then argc NUL terminated strings and the
// environment. argv starts exactly at that boundary rather than at the first byte that is not
// NUL, so an empty argv[0] cannot shift the environment into argv; any layout that does not match
// is refused rather than guessed. It never reads past argv, so the environment, which can hold
// secrets, is never turned into strings.
func parseProcargs(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errBadProcargs
	}
	argc := int(binary.NativeEndian.Uint32(b))
	if argc > maxArgs {
		return nil, errBadProcargs
	}
	area := b[4:]
	end := bytes.IndexByte(area, 0)
	if end < 0 {
		return nil, errBadProcargs
	}
	start := (end + stringAlign) / stringAlign * stringAlign
	if start > len(area) || bytes.ContainsFunc(area[end:start], func(r rune) bool { return r != 0 }) {
		return nil, errBadProcargs
	}
	rest := area[start:]
	args := make([]string, 0, argc)
	for range argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, errBadProcargs
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}
	return args, nil
}
