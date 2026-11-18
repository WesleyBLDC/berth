// Package e2e runs the real berthd and berth binaries.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"berth/internal/client"
	"berth/internal/paths"

	berthv1 "berth/gen/berth/v1"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "berth-bin-")
	if err != nil {
		panic(err)
	}
	// berth_e2e compiles in the test trust rules, which release builds lack: berthd pins the berth
	// and fake berth-native built here, and berth pins berthd, by cdhash without Apple's chain,
	// so all three are signed ad hoc, with the hardened runtime the live checks still demand.
	for _, args := range [][]string{
		{"go", "build", "-tags", "berth_e2e", "-o", dir + "/", "berth/cmd/berthd", "berth/cmd/berth"},
		{"go", "build", "-o", filepath.Join(dir, "berth-native"), "berth/internal/testutil/fakehelper"},
		{"go", "build", "-o", filepath.Join(dir, "berth-mcp"), "berth/cmd/berth-mcp"},
		{"go", "build", "-o", filepath.Join(dir, "consumer"), "berth/test/e2e/consumer"},
		{"go", "build", "-o", filepath.Join(dir, "marker"), "berth/test/e2e/marker"},
		{"codesign", "--force", "-s", "-", "-o", "runtime", "-i", "com.wesleybldc.berth.cli", filepath.Join(dir, "berth")},
		{"codesign", "--force", "-s", "-", "-o", "runtime", "-i", "com.wesleybldc.berth.daemon", filepath.Join(dir, "berthd")},
		{"codesign", "--force", "-s", "-", "-o", "runtime", "-i", "com.wesleybldc.berth.native", filepath.Join(dir, "berth-native")},
	} {
		build := exec.CommandContext(context.Background(), args[0], args[1:]...)
		if out, err := build.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("%v: %v\n%s", args, err, out))
		}
	}
	if err := buildProxy(dir); err != nil {
		panic(err)
	}
	binDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type daemonProc struct {
	cmd  *exec.Cmd
	home string
}

func home(t *testing.T) string {
	t.Helper()
	// Socket paths are limited to 104 bytes, so avoid the long t.TempDir path.
	h, err := os.MkdirTemp("", "berth-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(h) })
	return h
}

func start(t *testing.T, h string, args ...string) *daemonProc {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(binDir, "berthd"), args...)
	cmd.Env = append(os.Environ(), "BERTH_HOME="+h)
	logFile, _ := os.Create(filepath.Join(h, "berthd.log"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &daemonProc{cmd: cmd, home: h}
}

func waitServing(t *testing.T, h string) *client.Client {
	const want = "Serving"
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, err := client.Connect(ctx, paths.Socket(h), "e2e")
		if err == nil {
			s, err := c.Daemon.Status(ctx, &berthv1.StatusRequest{})
			cancel()
			if err == nil && s.GetState() == want {
				return c
			}
		} else {
			cancel()
		}
		time.Sleep(50 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(h, "berthd.log"))
	t.Fatalf("daemon never reached %s; log:\n%s", want, log)
	return nil
}

func berth(t *testing.T, h string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(binDir, "berth"), args...)
	cmd.Env = append(os.Environ(), "BERTH_HOME="+h)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("berth %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestKilledDaemonRecoversAndRecordsTheCrash(t *testing.T) {
	h := home(t)
	first := start(t, h)
	waitServing(t, h)

	_ = first.cmd.Process.Signal(syscall.SIGKILL)
	_ = first.cmd.Wait()

	start(t, h)
	c := waitServing(t, h)
	st, err := c.Debug.GetState(context.Background(), &berthv1.GetStateRequest{Entity: "daemon", HistoryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	hist := st.GetHistory()
	// Newest first: Recovered, Boot (from Serving, the crash), then the first run.
	if len(hist) != 4 {
		t.Fatalf("want 4 transitions, got %d: %v", len(hist), hist)
	}
	if hist[1].GetEvent() != "Boot" || hist[1].GetFromState() != "Serving" {
		t.Fatalf("restart boot not recorded from Serving: %v", hist[1])
	}
	if hist[0].GetToState() != "Serving" || !strings.Contains(hist[0].GetDetail(), "unclean shutdown") {
		t.Fatalf("recovery did not record the crash: %v", hist[0])
	}
	if st.GetVersion() != hist[0].GetVersion() {
		t.Fatalf("state version %d does not match newest event %d", st.GetVersion(), hist[0].GetVersion())
	}
}

func TestSIGTERMStopsCleanly(t *testing.T) {
	h := home(t)
	d := start(t, h)
	waitServing(t, h)
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
	if _, err := os.Stat(paths.Socket(h)); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	// A clean stop means the next boot is not flagged as unclean.
	start(t, h)
	c := waitServing(t, h)
	st, _ := c.Debug.GetState(context.Background(), &berthv1.GetStateRequest{Entity: "daemon", HistoryLimit: 1})
	if strings.Contains(st.GetHistory()[0].GetDetail(), "unclean") {
		t.Fatalf("clean stop was flagged unclean: %v", st.GetHistory()[0])
	}
}

func TestShutdownCallStopsTheDaemon(t *testing.T) {
	h := home(t)
	d := start(t, h)
	c := waitServing(t, h)
	// The app's settings say which berthd answers from this path.
	st, err := c.Daemon.Status(t.Context(), &berthv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(binDir, "berthd")); st.GetExecutable() != want {
		t.Fatalf("Status names %q as berthd, want %q", st.GetExecutable(), want)
	}
	if _, err := c.Daemon.Shutdown(context.Background(), &berthv1.ShutdownRequest{RequestId: "e2e-shutdown-1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
}

func TestCLIStatusAndDebugState(t *testing.T) {
	h := home(t)
	start(t, h)
	waitServing(t, h)
	if out := berth(t, h, "status"); !strings.Contains(out, "state    Serving (v2)") {
		t.Fatalf("status output:\n%s", out)
	}
	out := berth(t, h, "debug", "state", "daemon")
	for _, want := range []string{"daemon (daemon): Serving, version 2", "Boot", "Recovered", "previous run ended in Starting"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug state output missing %q:\n%s", want, out)
		}
	}
}
