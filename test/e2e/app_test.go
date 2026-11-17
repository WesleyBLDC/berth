package e2e

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"berth/internal/codetrust"
	"berth/internal/paths"
)

var (
	probeOnce sync.Once
	probePath string
	probeErr  string
)

// appProbe builds berth-app-probe, Berth.app's own client in a command line tool, once per run.
func appProbe(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("swift"); err != nil {
		t.Skip("no swift toolchain")
	}
	probeOnce.Do(func() {
		pkg, _ := filepath.Abs("../../macos")
		build := exec.CommandContext(context.Background(), "swift", "build", "--package-path", pkg, "--product", "berth-app-probe")
		if out, err := build.CombinedOutput(); err != nil {
			probeErr = err.Error() + "\n" + string(out)
			return
		}
		bin, err := exec.CommandContext(context.Background(), "swift", "build", "--package-path", pkg, "--show-bin-path").Output()
		if err != nil {
			probeErr = err.Error()
			return
		}
		probePath = filepath.Join(strings.TrimSpace(string(bin)), "berth-app-probe")
	})
	if probeErr != "" {
		t.Fatalf("build berth-app-probe: %s", probeErr)
	}
	return probePath
}

type probeRun struct {
	cmd   *exec.Cmd
	lines chan string
	done  chan struct{}
}

// e2eDaemon is the requirement the ad hoc signed test berthd meets: its identifier, no team.
var e2eDaemon = `identifier "` + codetrust.DaemonSigningID + `"`

func startProbe(t *testing.T, h, requirement string, after int64) *probeRun {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), appProbe(t), paths.Socket(h), requirement, strconv.FormatInt(after, 10))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &probeRun{cmd: cmd, lines: make(chan string, 256), done: make(chan struct{})}
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			p.lines <- s.Text()
		}
		close(p.lines)
		_ = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
	})
	return p
}

// until returns the lines read up to and including the first one ok accepts.
func (p *probeRun) until(t *testing.T, what string, ok func(string) bool) []string {
	t.Helper()
	var seen []string
	deadline := time.After(20 * time.Second)
	for {
		select {
		case line, open := <-p.lines:
			if !open {
				t.Fatalf("berth-app-probe ended before it %s; it printed:\n%s", what, strings.Join(seen, "\n"))
			}
			seen = append(seen, line)
			if ok(line) {
				return seen
			}
		case <-deadline:
			t.Fatalf("berth-app-probe never %s; it printed:\n%s", what, strings.Join(seen, "\n"))
		}
	}
}

func changeOf(line, prefix, state string) bool {
	f := strings.Fields(line)
	return len(f) == 5 && f[0] == "change" && strings.HasPrefix(f[3], prefix) && f[4] == state
}

// Berth.app's client, through the real berthd, sees a real headless browser appear and stop,
// and a second stream after the snapshot's cursor replays exactly those changes, no snapshot.
func TestAppClientWatchesSessionsAndResumes(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	p := startProbe(t, h, e2eDaemon, 0)
	first := p.until(t, "sent a snapshot", func(l string) bool { return strings.HasPrefix(l, "snapshot ") })
	f := strings.Fields(first[len(first)-1])
	if len(f) != 4 || f[2] != "SNAPSHOT_REASON_START" {
		t.Fatalf("first line %q", first[len(first)-1])
	}
	cursor, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	_, args := chromeArgs(t)
	browser := exec.CommandContext(t.Context(), chrome, args...)
	if err := browser.Start(); err != nil {
		t.Fatal(err)
	}
	pid := browser.Process.Pid
	killOnCleanup(t, pid)
	prefix := strconv.Itoa(pid) + "-"
	p.until(t, "saw the browser", func(l string) bool { return changeOf(l, prefix, "discovered") })
	_ = browser.Process.Kill()
	_ = browser.Wait()
	live := p.until(t, "saw the browser stop", func(l string) bool { return changeOf(l, prefix, "stopped") })

	again := startProbe(t, h, e2eDaemon, cursor)
	replayed := again.until(t, "replayed the stop", func(l string) bool { return changeOf(l, prefix, "stopped") })
	for _, l := range replayed {
		if !strings.HasPrefix(l, "change ") {
			t.Fatalf("a resumed stream sent %q", l)
		}
	}
	var mine []string
	for _, l := range live {
		if strings.HasPrefix(l, "change ") {
			mine = append(mine, l)
		}
	}
	if strings.Join(mine, "\n") != strings.Join(replayed[len(replayed)-len(mine):], "\n") {
		t.Fatalf("live changes:\n%s\nreplayed:\n%s", strings.Join(mine, "\n"), strings.Join(replayed, "\n"))
	}
}

// The app trusts only the release berthd: against the ad hoc signed test berthd, the release
// requirement refuses the server and the probe exits 1 having watched nothing.
func TestAppClientRefusesAnUnsignedBerthd(t *testing.T) {
	h := home(t)
	start(t, h)
	waitServing(t, h)
	p := startProbe(t, h, codetrust.Requirement(codetrust.DaemonSigningID), 0)
	lines := p.until(t, "refused the server", func(l string) bool { return strings.HasPrefix(l, "error ") })
	if got := lines[len(lines)-1]; len(lines) != 1 || !strings.Contains(got, "not the signed berthd") {
		t.Fatalf("probe printed %q", lines)
	}
	<-p.done
	if code := p.cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("probe exited %d, want 1", code)
	}
}
