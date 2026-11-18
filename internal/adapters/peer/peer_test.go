//go:build darwin

package peer

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"berth/internal/adapters/execid"
	"berth/internal/adapters/socket"
	"berth/internal/core/ports"
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

// TestLiveCodeMatchesTheFile connects to itself over a socket, takes the audit token the
// kernel reports, and checks the live cdhash equals what codesign reads from the test binary.
func TestLiveCodeMatchesTheFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "berth-code-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	var d net.Dialer
	client, err := d.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server := <-accepted
	defer func() { _ = server.Close() }()
	token, err := socket.KernelPeerToken(server.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	live, err := NewCode().Live(t.Context(), ports.Peer{PID: os.Getpid(), AuditToken: token})
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	want, err := execid.New(execid.DefaultCodesign).CDHash(t.Context(), self)
	if err != nil {
		t.Fatal(err)
	}
	if live.CDHash != want || !live.Valid() {
		t.Fatalf("live %x status %#x, file cdhash %x", live.CDHash, live.Status, want)
	}
	// The Go linker signs ad hoc without the hardened runtime.
	if live.Hardened() {
		t.Fatalf("test binary reported as hardened: %#x", live.Status)
	}
	if _, err := NewCode().Live(t.Context(), ports.Peer{PID: os.Getpid() + 1, AuditToken: token}); err == nil {
		t.Fatal("a token for another pid was accepted")
	}
	if _, err := NewCode().Live(t.Context(), ports.Peer{PID: os.Getpid()}); err == nil {
		t.Fatal("a missing token was accepted")
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
func TestProcessReportsOnlyAMissingProcessAsGone(t *testing.T) {
	t.Cleanup(func() { kinfoProc = unix.SysctlKinfoProc })
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) { return nil, unix.ENOMEM }
	if _, err := Process(os.Getpid()); err == nil || errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("ENOMEM gave %v, want an error that is not ErrProcessGone", err)
	}
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) { return nil, unix.ESRCH }
	if _, err := Process(os.Getpid()); !errors.Is(err, ports.ErrProcessGone) {
		t.Fatalf("ESRCH gave %v, want ErrProcessGone", err)
	}
}
