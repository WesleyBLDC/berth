//go:build darwin

package spawn

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"berth/internal/adapters/execid"
)

func TestSuspendedChildRunsOnlyAfterResume(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	pid, err := StartSuspended("/bin/sh", []string{"sh", "-c", `touch "$1"; printf %s "$K"`, "sh", marker},
		[][]byte{[]byte("K=value"), []byte("PATH=/usr/bin:/bin")}, [3]*os.File{devnull, w, w})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the suspended child ran: %v", err)
	}
	live, err := Live(pid)
	if err != nil {
		t.Fatal(err)
	}
	want, err := execid.New(execid.DefaultCodesign).CDHash(t.Context(), "/bin/sh")
	if err != nil || live.CDHash != want || !live.Valid() {
		t.Fatalf("live %x status %#x, want cdhash %x (%v)", live.CDHash, live.Status, want, err)
	}
	if err := Resume(pid); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	state, err := (&os.Process{Pid: pid}).Wait()
	if err != nil || !state.Success() || string(out) != "value" {
		t.Fatalf("child %v %v output %q", state, err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the resumed child did not run: %v", err)
	}
}

func TestKilledSuspendedChildNeverRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	pid, err := StartSuspended("/usr/bin/touch", []string{"touch", marker}, nil, [3]*os.File{devnull, devnull, devnull})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.Wait()
	if err != nil || !strings.Contains(state.String(), "killed") {
		t.Fatalf("state %v %v", state, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the killed child ran: %v", err)
	}
}

func TestStartFailsForAMissingFile(t *testing.T) {
	if _, err := StartSuspended("/nonexistent/tool", []string{"tool"}, nil, [3]*os.File{os.Stdin, os.Stdout, os.Stderr}); err == nil {
		t.Fatal("started a missing file")
	}
}
