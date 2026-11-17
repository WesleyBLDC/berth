//go:build darwin

package peer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFactsForOwnProcess(t *testing.T) {
	got, err := New().Facts(t.Context(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	if got.Executable != self {
		t.Fatalf("executable %q, want %q", got.Executable, self)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkingDir != wd {
		t.Fatalf("working dir %q, want %q", got.WorkingDir, wd)
	}
	if len(got.Tree) == 0 || got.Tree[0].PID != os.Getppid() {
		t.Fatalf("tree %+v does not start at the parent %d", got.Tree, os.Getppid())
	}
	last := got.Tree[len(got.Tree)-1]
	for _, p := range got.Tree {
		if p.Executable == "" || p.PID <= 0 {
			t.Fatalf("empty link in tree %+v", got.Tree)
		}
	}
	// Our own user can always see launchd, so the chain must reach it.
	if last.PID != 1 || last.Executable != "/sbin/launchd" {
		t.Fatalf("tree ends at %+v, want launchd", last)
	}
}

// A child started in another folder: the kernel, not the child, says where it runs and whose
// child it is.
func TestFactsForChild(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	got, err := New().Facts(t.Context(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Executable != "/bin/sleep" || got.WorkingDir != dir {
		t.Fatalf("got %+v", got)
	}
	if len(got.Tree) == 0 || got.Tree[0].PID != os.Getpid() {
		t.Fatalf("tree %+v does not start at the test process", got.Tree)
	}
}

func TestFactsForMissingProcess(t *testing.T) {
	// pid_max on macOS is 99998, so this pid never exists.
	_, err := New().Facts(t.Context(), 999999)
	if !errors.Is(err, unix.ESRCH) {
		t.Fatalf("err = %v, want ESRCH", err)
	}
	if _, err := New().Facts(t.Context(), 0); err == nil {
		t.Fatal("pid 0 was accepted")
	}
}
