package proxy

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Value holds one secret in an anonymous mapping outside the Go heap, so the garbage collector
// never copies it and, when mlock succeeds, it is never written to swap.
type Value struct {
	mem    []byte
	n      int
	locked bool
}

// errBadValue never says what the value holds.
var errBadValue = errors.New("the secret is empty or holds CR, LF or NUL, which cannot go in a header")

// lockValue moves src into locked memory and zeroes src, whatever happens.
func lockValue(src []byte) (*Value, error) {
	defer clear(src)
	if len(src) == 0 || bytes.ContainsAny(src, "\r\n\x00") {
		return nil, errBadValue
	}
	page := os.Getpagesize()
	size := (len(src) + page - 1) / page * page
	mem, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	v := &Value{mem: mem, n: copy(mem, src)}
	v.locked = unix.Mlock(mem) == nil
	return v, nil
}

// Bytes is the value itself; callers write it straight to a connection and never keep or
// convert it.
func (v *Value) Bytes() []byte { return v.mem[:v.n] }

// Locked reports whether mlock succeeded; the proxy still runs without it.
func (v *Value) Locked() bool { return v.locked }

// Zero overwrites the value. The mapping stays so a late reader sees zeroes instead of faulting;
// the process exits right after.
func (v *Value) Zero() {
	clear(v.mem)
	if v.locked {
		_ = unix.Munlock(v.mem)
		v.locked = false
	}
}

func zeroAll(grants []*Grant) {
	for _, g := range grants {
		if g.value != nil {
			g.value.Zero()
		}
	}
}
