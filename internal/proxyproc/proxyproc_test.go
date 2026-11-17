package proxyproc_test

import (
	"io"
	"testing"
	"time"

	"berth/internal/proxyproc"
)

// TestStopReportsTheRealExitCode runs a real child, since the exit code berth maps comes from the
// kernel through exec: an exit is its code, a kill is -1.
func TestStopReportsTheRealExitCode(t *testing.T) {
	p, err := proxyproc.Start(t.Context(), "/bin/sh", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin, "exit 4\n"); err != nil {
		t.Fatal(err)
	}
	if killed, code := proxyproc.Stop(p, 5*time.Second, false); killed || code != 4 {
		t.Fatalf("killed %v, code %d, want 4", killed, code)
	}

	p, err = proxyproc.Start(t.Context(), "/bin/sh", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin, "exec /bin/sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	if killed, code := proxyproc.Stop(p, 200*time.Millisecond, false); !killed || code != -1 {
		t.Fatalf("killed %v, code %d, want a kill", killed, code)
	}
}
