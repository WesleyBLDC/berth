//go:build darwin

package procscan

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"berth/internal/core/ports"
)

func source(t *testing.T) *Source {
	t.Helper()
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func find(t *testing.T, s *Source, pid int) ports.ProcessInfo {
	t.Helper()
	all, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.PID == pid {
			return p
		}
	}
	t.Fatalf("pid %d not listed among %d processes", pid, len(all))
	return ports.ProcessInfo{}
}

func TestListAndArgsOfOwnProcess(t *testing.T) {
	s := source(t)
	self := find(t, s, os.Getpid())
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	if self.PPID != os.Getppid() || self.UID != uint32(os.Getuid()) || self.Executable != exe { //nolint:gosec // uid fits
		t.Fatalf("self = %+v, want ppid %d uid %d exe %s", self, os.Getppid(), os.Getuid(), exe)
	}
	if self.Start.IsZero() || time.Since(self.Start) > time.Hour {
		t.Fatalf("start time %v is not this test's", self.Start)
	}
	args, err := s.Args(t.Context(), self.ProcessRef)
	if err != nil || !slices.Equal(args, os.Args) {
		t.Fatalf("args = %q, %v; want %q", args, err, os.Args)
	}
}

func TestAnotherUsersArgumentsAreOpaque(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every process's arguments")
	}
	s := source(t)
	launchd := find(t, s, 1)
	if launchd.Executable != "/sbin/launchd" {
		t.Fatalf("launchd = %+v", launchd)
	}
	if _, err := s.Args(t.Context(), launchd.ProcessRef); !errors.Is(err, ports.ErrOpaqueProcess) {
		t.Fatalf("launchd args error = %v, want ErrOpaqueProcess", err)
	}
}

func TestStaleRefIsGone(t *testing.T) {
	s := source(t)
	self := find(t, s, os.Getpid())
	stale := ports.ProcessRef{PID: self.PID, Start: self.Start.Add(-time.Second)}
	if _, err := s.Args(t.Context(), stale); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("Args with a stale start = %v", err)
	}
	if err := s.Signal(t.Context(), stale, ports.SignalKill); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("Signal with a stale start = %v", err)
	}
	if _, err := s.WatchExit(t.Context(), stale); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("WatchExit with a stale start = %v", err)
	}
}

func TestCPUTimeGrows(t *testing.T) {
	s := source(t)
	self := find(t, s, os.Getpid()).ProcessRef
	before, err := s.CPUTime(t.Context(), self)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); { //nolint:revive // spin to burn CPU
	}
	after, err := s.CPUTime(t.Context(), self)
	if err != nil {
		t.Fatal(err)
	}
	if grew := after - before; grew < 100*time.Millisecond || grew > 2*time.Second {
		t.Fatalf("CPU time grew %v over a 200 ms spin", grew)
	}
}

func TestSignalAndExitOfAChild(t *testing.T) {
	s := source(t)
	child := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	ref := find(t, s, child.Process.Pid).ProcessRef
	exited, err := s.WatchExit(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Signal(t.Context(), ref, ports.SignalTerm); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("no NOTE_EXIT within 5 seconds of SIGTERM")
	}
	<-done
	if err := s.Signal(t.Context(), ref, ports.SignalKill); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("Signal after exit = %v, want ErrProcessGone", err)
	}
}

// BenchmarkList measures one full table read, which berthd's scan does every 2 seconds.
func BenchmarkList(b *testing.B) {
	s, err := New()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := s.List(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}
