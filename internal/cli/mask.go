package cli

import (
	"bytes"
	"io"
	"sync"
)

// masker replaces every secret value in a stream with ****. It holds back any tail that could
// still grow into a value, so a value split across writes is masked as well.
type masker struct {
	mu      sync.Mutex
	w       io.Writer
	values  [][]byte
	pending []byte
}

// newMasker copies the values; Close zeroes the copies.
func newMasker(w io.Writer, values [][]byte) *masker {
	m := &masker{w: w}
	for _, v := range values {
		if len(v) > 0 {
			m.values = append(m.values, bytes.Clone(v))
		}
	}
	return m
}

func (m *masker) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, p...)
	if err := m.drain(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes what is held back and zeroes every copy of the values.
func (m *masker) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.drain(true)
	for _, v := range m.values {
		clear(v)
	}
	clear(m.pending[:cap(m.pending)])
	m.values, m.pending = nil, nil
	return err
}

func (m *masker) drain(eof bool) error {
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
			out = append(out, maskReplacement...)
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
