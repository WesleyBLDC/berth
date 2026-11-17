package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	berthv1 "berth/gen/berth/v1"
)

// findChrome returns a Chromium build already on this Mac, lightest first; the tests install
// nothing. BERTH_E2E_CHROME overrides the search, and a path there that is not a file skips.
func findChrome(t *testing.T) string {
	t.Helper()
	if p, ok := os.LookupEnv("BERTH_E2E_CHROME"); ok {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			t.Skipf("BERTH_E2E_CHROME=%s is not a browser executable", p)
		}
		return p
	}
	home, _ := os.UserHomeDir()
	for _, glob := range []string{
		"Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-mac-*/chrome-headless-shell",
		".cache/puppeteer/chrome-headless-shell/*/chrome-headless-shell-mac-*/chrome-headless-shell",
		".cache/puppeteer/chrome/*/chrome-mac-*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
	} {
		matches, _ := filepath.Glob(filepath.Join(home, glob))
		if len(matches) > 0 {
			slices.Sort(matches)
			return matches[len(matches)-1]
		}
	}
	if p := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; fileExists(p) {
		return p
	}
	t.Skip("no Chromium build found (Playwright or Puppeteer cache, or Google Chrome); set BERTH_E2E_CHROME to one")
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// chromeArgs start a headless browser on a fresh profile named the way Playwright names its own.
func chromeArgs(t *testing.T) (profile string, args []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-e2e-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	profile, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	profile = filepath.Join(profile, "playwright_chromiumdev_profile-berth")
	return profile, []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + profile, "--no-first-run", "about:blank"}
}

// killOnCleanup kills only the browser this test started, by its pid, if it is still running.
func killOnCleanup(t *testing.T, pid int) {
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

func listJSON(t *testing.T, h string, args ...string) []*berthv1.Session {
	t.Helper()
	out := berth(t, h, append([]string{"ps", "--json"}, args...)...)
	var resp berthv1.ListSessionsResponse
	if err := protojson.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("ps --json: %v\n%s", err, out)
	}
	return resp.GetSessions()
}

// waitSession polls berth ps until the session of browser pid matches ok, and returns it.
func waitSession(t *testing.T, h string, pid int, what string, ok func(*berthv1.Session) bool) *berthv1.Session {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last *berthv1.Session
	for time.Now().Before(deadline) {
		for _, s := range listJSON(t, h, "--all") {
			if int(s.GetBrowser().GetPid()) == pid {
				last = s
				if ok(s) {
					return s
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("browser %d never %s; last seen %v", pid, what, last)
	return nil
}

func expectedDriver(chrome string) berthv1.Driver {
	if strings.Contains(chrome, "/.cache/puppeteer/") {
		return berthv1.Driver_DRIVER_PUPPETEER
	}
	return berthv1.Driver_DRIVER_PLAYWRIGHT
}

func TestPsSeesAHeadlessBrowserAndItsExit(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	profile, args := chromeArgs(t)
	// Arguments can carry credentials (--proxy-server with a password), so none may leave berthd.
	const canary = "berth-argv-canary-7f3e19"
	args = append(args[:len(args)-1], "--berth-e2e-canary="+canary, args[len(args)-1])
	browser := exec.CommandContext(t.Context(), chrome, args...)
	if err := browser.Start(); err != nil {
		t.Fatal(err)
	}
	pid := browser.Process.Pid
	killOnCleanup(t, pid)
	exited := make(chan struct{})
	go func() { _ = browser.Wait(); close(exited) }()

	s := waitSession(t, h, pid, "showed its helpers", func(s *berthv1.Session) bool { return s.GetProcessCount() >= 2 })
	if !s.GetHeadless() || s.GetDriver() != expectedDriver(chrome) || s.GetCdp() != berthv1.CdpMode_CDP_MODE_PORT ||
		s.GetCdpPort() != 0 || s.GetUserDataDir() != profile || s.GetOtherUser() || s.GetBrowser().GetExecutable() != chrome {
		t.Fatalf("session %v", s)
	}
	out := berth(t, h, "inspect", s.GetId())
	for _, want := range []string{"session " + s.GetId(), "headless    yes", "cdp         port auto", "profile     " + profile} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, out)
		}
	}
	// The test started the browser, so it heads the owner chain.
	if !slices.ContainsFunc(strings.Split(out, "\n"), func(line string) bool {
		f := strings.Fields(line)
		return len(f) >= 2 && f[0] == strconv.Itoa(os.Getpid()) && f[1] == "ancestor"
	}) {
		t.Fatalf("inspect does not list this test (pid %d) as an ancestor:\n%s", os.Getpid(), out)
	}
	ps := berth(t, h, "ps")
	if !strings.Contains(ps, s.GetId()) {
		t.Fatal("berth ps does not list the session")
	}
	psJSON := berth(t, h, "ps", "--json")
	for name, text := range map[string]string{"ps": ps, "ps --json": psJSON, "inspect": out} {
		if strings.Contains(text, canary) {
			t.Fatalf("an argument reached berth %s:\n%s", name, text)
		}
	}

	// Killed outside Berth: the exit watch or the next scan records it.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	<-exited
	waitSession(t, h, pid, "stopped", func(s *berthv1.Session) bool { return s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED })
	out = berth(t, h, "debug", "state", "session/"+s.GetId())
	if !strings.Contains(out, "Exited") {
		t.Fatalf("no Exited transition:\n%s", out)
	}
	for _, f := range []string{"berthd.log", "berth.db", "berth.db-wal"} {
		if b, _ := os.ReadFile(filepath.Join(h, f)); strings.Contains(string(b), canary) {
			t.Fatalf("an argument reached %s", f)
		}
	}
}

func TestABrowserWhoseOwnerExitedIsALeftoverAndStops(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	_, args := chromeArgs(t)
	// A shell that starts the browser in the background and exits at once, as a crashed test
	// runner would, so the browser is reparented to launchd.
	quoted := make([]string, 0, len(args)+1)
	for _, a := range append([]string{chrome}, args...) {
		quoted = append(quoted, "'"+a+"'")
	}
	owner := exec.CommandContext(t.Context(), "/bin/sh", "-c", strings.Join(quoted, " ")+" >/dev/null 2>&1 & echo $!")
	out, err := owner.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("owner printed %q", out)
	}
	killOnCleanup(t, pid)

	s := waitSession(t, h, pid, "became a leftover", func(s *berthv1.Session) bool { return s.GetState() == berthv1.SessionState_SESSION_STATE_LEFTOVER })
	if s.GetOwner().GetKind() != berthv1.OwnerKind_OWNER_KIND_NONE {
		t.Fatalf("leftover owner %v", s.GetOwner())
	}
	if out := berth(t, h, "leftovers"); !strings.Contains(out, s.GetId()) {
		t.Fatalf("berth leftovers does not list %s:\n%s", s.GetId(), out)
	}
	// Only this test's browser is stopped; berth leftovers --stop would end every leftover on
	// the machine.
	if out := berth(t, h, "stop", s.GetId()); out != "stopped "+s.GetId()+"\n" {
		t.Fatalf("stop: %q", out)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("browser %d still runs after berth stop", pid)
	}
	out2 := berth(t, h, "debug", "state", "session/"+s.GetId())
	for _, want := range []string{"OwnerExited", "StopRequested", "Exited", "Stopped"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("debug state missing %q:\n%s", want, out2)
		}
	}
}
