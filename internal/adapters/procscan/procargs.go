package procscan

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// maxArgs bounds what a hostile process can make berthd allocate by passing a huge argc.
const maxArgs = 4096

var errBadProcargs = errors.New("malformed kern.procargs2 buffer")

// parseProcargs reads the arguments out of a kern.procargs2 buffer: a 32-bit argc, the exec path,
// NUL padding, then argc NUL terminated strings and the environment. It never reads past argv,
// so the environment, which can hold secrets, is never turned into strings.
func parseProcargs(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errBadProcargs
	}
	argc := int(binary.NativeEndian.Uint32(b))
	if argc > maxArgs {
		return nil, errBadProcargs
	}
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, errBadProcargs
	}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
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
