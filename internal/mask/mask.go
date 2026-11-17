// Package mask hides secret values in a byte stream: berth run masks its child's output, and
// berth-proxy redacts the value from response bodies.
package mask

import (
	"bytes"
	"io"
	"sync"
)

// Replacement is what NewWriter puts in place of every value.
const Replacement = "****"

// Writer replaces every secret value in a stream. It holds back any tail that could still grow
// into a value, so a value split across writes is masked as well.
type Writer struct {
	mu         sync.Mutex
	w          io.Writer
	values     [][]byte
	owned      bool
	sameLength bool
	pending    []byte
}

// NewWriter copies the values and replaces each with Replacement; Close zeroes the copies.
func NewWriter(w io.Writer, values [][]byte) *Writer {
	m := &Writer{w: w, owned: true}
	for _, v := range values {
		if len(v) > 0 {
			m.values = append(m.values, bytes.Clone(v))
		}
	}
	return m
}

// NewSameLength replaces each value with as many asterisks as it has bytes, so a stream framed
// by its length stays valid. It borrows the values instead of copying them: the caller keeps
// them unchanged until Close and zeroes them itself.
func NewSameLength(w io.Writer, values [][]byte) *Writer {
	m := &Writer{w: w, sameLength: true}
	for _, v := range values {
		if len(v) > 0 {
			m.values = append(m.values, v)
		}
	}
	return m
}

func (m *Writer) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, p...)
	if err := m.drain(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes what is held back, then zeroes the held back bytes and every copy of the values.
func (m *Writer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.drain(true)
	if m.owned {
		for _, v := range m.values {
			clear(v)
		}
	}
	clear(m.pending[:cap(m.pending)])
	m.values, m.pending = nil, nil
	return err
}

func (m *Writer) drain(eof bool) error {
	var out []byte
	i := 0
scan:
	for i < len(m.pending) {
		rest := m.pending[i:]
		longest := 0
		for _, v := range m.values {
			switch {
			case !eof && len(rest) < len(v) && bytes.HasPrefix(v, rest):
				// rest may become a value with the next write, and a longer value starting here
				// wins over a shorter one, so nothing from here on is safe to emit yet.
				break scan
			case bytes.HasPrefix(rest, v) && len(v) > longest:
				longest = len(v)
			}
		}
		if longest > 0 {
			if m.sameLength {
				out = append(out, bytes.Repeat([]byte{'*'}, longest)...)
			} else {
				out = append(out, Replacement...)
			}
			i += longest
			continue
		}
		out = append(out, m.pending[i])
		i++
	}
	held := copy(m.pending, m.pending[i:])
	clear(m.pending[held:])
	m.pending = m.pending[:held]
	if len(out) == 0 {
		return nil
	}
	_, err := m.w.Write(out)
	return err
}
