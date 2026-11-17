//go:build darwin

package procscan

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"berth/internal/core/discovery"
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

// The kernel's layout for an empty argv[0]: a parser that skipped every NUL after the exec path
// would read the environment as arguments.
func TestEmptyArgvZeroFromTheKernel(t *testing.T) {
	s := source(t)
	pid, err := syscall.ForkExec("/bin/sleep", []string{"", "30"}, &syscall.ProcAttr{Env: []string{"BERTH_CANARY=env-canary"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
	})
	ref := find(t, s, pid).ProcessRef
	// The child may not have exec'd yet; its arguments settle within moments.
	deadline := time.Now().Add(2 * time.Second)
	for {
		args, err := s.Args(t.Context(), ref)
		if err == nil && slices.Equal(args, []string{"", "30"}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("args = %q, %v; want [\"\" \"30\"]", args, err)
		}
		time.Sleep(20 * time.Millisecond)
		ref = find(t, s, pid).ProcessRef
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

// openFiles counts this process's open descriptors; /dev/fd shows only some of them on macOS.
func openFiles(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := range 4096 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}

// A refused watch must close its kqueue at once: a stream of stale refs would otherwise hold one
// descriptor each until the watch noticed.
func TestRefusedWatchesCloseTheirKqueue(t *testing.T) {
	s := source(t)
	self := find(t, s, os.Getpid())
	stale := ports.ProcessRef{PID: self.PID, Start: self.Start.Add(-time.Second)}
	before := openFiles(t)
	for range 200 {
		if _, err := s.WatchExit(t.Context(), stale); !errors.Is(err, ports.ErrProcessGone) {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for openFiles(t) > before+5 {
		if time.Now().After(deadline) {
			t.Fatalf("%d descriptors open after 200 refused watches, %d before", openFiles(t), before)
		}
		time.Sleep(5 * time.Millisecond)
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

// BenchmarkScan measures one discovery scan of this Mac's real table: fresh reads every
// process's arguments as if seen for the first time; cached reuses what the last scan read.
func BenchmarkScan(b *testing.B) {
	s, err := New()
	if err != nil {
		b.Fatal(err)
	}
	uid := uint32(os.Getuid()) //nolint:gosec // a uid fits
	b.Run("fresh", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := discovery.NewScanner(s, uid).Scan(b.Context()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		sc := discovery.NewScanner(s, uid)
		for b.Loop() {
			if _, err := sc.Scan(b.Context()); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestLineageOfAChildReachesLaunchd(t *testing.T) {
	s := source(t)
	cmd := exec.CommandContext(t.Context(), "/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	got, err := s.Lineage(t.Context(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 || got[0].PID != cmd.Process.Pid || got[1].PID != os.Getpid() || got[len(got)-1].PID != 1 {
		t.Fatalf("lineage %+v: want the child, this test, ..., launchd", got)
	}
	if got[0].PPID != os.Getpid() || got[0].UID != uint32(os.Getuid()) || got[1].Start.IsZero() { //nolint:gosec // user IDs fit in 32 bits
		t.Fatalf("lineage facts %+v", got[:2])
	}
	if !got[1].Same(find(t, s, os.Getpid()).ProcessRef) {
		t.Fatal("lineage and List disagree about this process's start time")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := s.Lineage(t.Context(), cmd.Process.Pid); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("lineage of an exited child: %v", err)
	}
}

// TestLineageStopsAtAReusedParentPID: the child was read naming parent 20, then 20 exited and
// a later process took its pid. That process and its ancestors (here the root 10) must not join
// the child's lineage.
func TestLineageStopsAtAReusedParentPID(t *testing.T) {
	at := func(s int64) time.Time { return time.Unix(s, 0) }
	table := map[int]ports.ProcessInfo{
		1:  {ProcessRef: ports.ProcessRef{PID: 1, Start: at(0)}},
		10: {ProcessRef: ports.ProcessRef{PID: 10, Start: at(1)}, PPID: 1},
		20: {ProcessRef: ports.ProcessRef{PID: 20, Start: at(9)}, PPID: 10}, // the reuser
		30: {ProcessRef: ports.ProcessRef{PID: 30, Start: at(5)}, PPID: 20}, // still names the old 20
	}
	lookup := func(pid int) (ports.ProcessInfo, bool) { p, ok := table[pid]; return p, ok }
	got, err := walkLineage(t.Context(), 30, lookup)
	if err != nil || len(got) != 1 || got[0].PID != 30 {
		t.Fatalf("lineage through a reused parent pid: %+v %v", got, err)
	}
	// With the real parent in place the walk reaches launchd.
	table[20] = ports.ProcessInfo{ProcessRef: ports.ProcessRef{PID: 20, Start: at(2)}, PPID: 10}
	if got, err := walkLineage(t.Context(), 30, lookup); err != nil || len(got) != 4 || got[3].PID != 1 {
		t.Fatalf("lineage: %+v %v", got, err)
	}
	if _, err := walkLineage(t.Context(), 99, lookup); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("missing pid: %v", err)
	}
}

// Only a missing process is gone: a sysctl that fails for another reason must not end what the
// process stands for, such as an agent session whose root watch checks the process first.
func TestCurrentReportsOnlyAMissingProcessAsGone(t *testing.T) {
	t.Cleanup(func() { kinfoProc = unix.SysctlKinfoProc })
	self := ports.ProcessRef{PID: os.Getpid()}
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) { return nil, unix.ENOMEM }
	if err := current(self); err == nil || errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("ENOMEM gave %v, want an error that is not ErrProcessGone", err)
	}
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) { return nil, unix.ESRCH }
	if err := current(self); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("ESRCH gave %v, want ErrProcessGone", err)
	}
}
