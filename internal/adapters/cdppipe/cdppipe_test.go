package cdppipe_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"berth/internal/adapters/cdppipe"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// TestMain doubles as a fake Chrome: launched with the pipe flags, it answers on fds 3 and 4.
func TestMain(m *testing.M) {
	if slices.Contains(os.Args, "--remote-debugging-pipe") {
		fakeChrome()
		return
	}
	os.Exit(m.Run())
}

// fakeChrome replies to every message with its method, owns session own-1 after an attach, and
// emits the event a message asks for with "emit".
func fakeChrome() {
	// A profile named helper-* starts a helper in the browser's process group, records its pid,
	// and exits at once, as a crashing browser leaves its helpers behind.
	if wd, _ := os.Getwd(); strings.HasPrefix(filepath.Base(wd), "helper-") {
		helper := exec.CommandContext(context.Background(), "/bin/sleep", "60")
		if helper.Start() == nil {
			_ = os.WriteFile("helper.pid", []byte(strconv.Itoa(helper.Process.Pid)), 0o600)
		}
		os.Exit(0)
	}
	in, out := os.NewFile(3, "in"), os.NewFile(4, "out")
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, 1<<20)
	sc.Split(func(data []byte, _ bool) (int, []byte, error) {
		for i, c := range data {
			if c == 0 {
				return i + 1, data[:i], nil
			}
		}
		return 0, nil, nil
	})
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, 0))
	}
	for sc.Scan() {
		var m struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Method {
		case "Target.attachToTarget":
			write(map[string]any{"id": m.ID, "result": map[string]any{"sessionId": "own-1"}})
		case "Fail.me":
			write(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
		case "Emit":
			var p struct {
				SessionID string `json:"sessionId"`
				Method    string `json:"method"`
			}
			_ = json.Unmarshal(m.Params, &p)
			write(map[string]any{"method": p.Method, "sessionId": p.SessionID, "params": map[string]any{"sessionId": "own-1"}})
			write(map[string]any{"id": m.ID, "result": map[string]any{}})
		case "Hang":
		case "Stall":
			// It stops reading, as a wedged browser does, until it is killed.
			time.Sleep(time.Hour)
		default:
			write(map[string]any{"id": m.ID, "result": map[string]any{"method": m.Method}})
		}
	}
}

type sink struct {
	mu  sync.Mutex
	got []string
}

func (s *sink) add(raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, string(raw))
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func launchFake(t *testing.T) ports.BrowserConn {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := cdppipe.Driver{}.Launch(t.Context(), ports.BrowserLaunch{Executable: exe, ProfileDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Kill(); <-c.Done() })
	return c
}

func TestCallsRepliesAndTheSink(t *testing.T) {
	c := launchFake(t)
	ctx := t.Context()
	got, err := c.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil || !strings.Contains(string(got), "Browser.getVersion") {
		t.Fatalf("Call = %s, %v", got, err)
	}
	if _, err := c.Call(ctx, "", "Fail.me", nil); err == nil || !strings.Contains(err.Error(), "no such method") {
		t.Fatalf("a CDP error must come back as an error, got %v", err)
	}
	s := &sink{}
	c.Sink(s.add)
	if err := c.Send([]byte(`{"id":7,"method":"Page.enable"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Send([]byte("{\"id\":8,\"method\":\"a\x00b\"}")); err == nil {
		t.Fatal("a message holding a NUL byte must be refused, or it would split on the pipe")
	}
	// Berth's own call is answered to Berth, never to the sink.
	if _, err := c.Call(ctx, "", "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(func() bool { return len(s.all()) >= 1 })
	if all := s.all(); len(all) != 1 || !strings.Contains(all[0], `"id":7`) {
		t.Fatalf("sink got %v, want only the reply to id 7", all)
	}
}

func TestBerthsOwnSessionsNeverReachTheSink(t *testing.T) {
	c := launchFake(t)
	ctx := t.Context()
	s := &sink{}
	c.Sink(s.add)
	if c.OwnSession("own-1") {
		t.Fatal("no session is Berth's before it attaches")
	}
	if _, err := c.Call(ctx, "", "Target.attachToTarget", []byte(`{"targetId":"T","flatten":true}`)); err != nil {
		t.Fatal(err)
	}
	if !c.OwnSession("own-1") || c.OwnSession("agent-1") || c.OwnSession("") {
		t.Fatal("the session from Berth's own attach must be Berth's, and only it")
	}
	for _, p := range []string{
		`{"sessionId":"own-1","method":"Page.frameNavigated"}`,
		`{"sessionId":"agent-1","method":"Page.loadEventFired"}`,
	} {
		if _, err := c.Call(ctx, "", "Emit", []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	all := s.all()
	if len(all) != 1 || !strings.Contains(all[0], "agent-1") {
		t.Fatalf("sink got %v, want only the agent session's event", all)
	}
	// Berth's session ends with its detach event, which the agent never sees either.
	if _, err := c.Call(ctx, "", "Emit", []byte(`{"method":"Target.detachedFromTarget"}`)); err != nil {
		t.Fatal(err)
	}
	if c.OwnSession("own-1") || len(s.all()) != 1 {
		t.Fatalf("a detached Berth session must be forgotten silently; sink %v", s.all())
	}
}

func TestKillEndsPendingCalls(t *testing.T) {
	c := launchFake(t)
	errs := make(chan error, 1)
	go func() {
		_, err := c.Call(context.WithoutCancel(t.Context()), "", "Hang", nil)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, ports.ErrBrowserGone) {
			t.Fatalf("pending call = %v, want ErrBrowserGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pending call outlived the browser")
	}
	<-c.Done()
	if _, err := c.Call(t.Context(), "", "Browser.getVersion", nil); !errors.Is(err, ports.ErrBrowserGone) {
		t.Fatalf("call after exit = %v", err)
	}
}

// A browser that stops reading its pipe fills it; the write then times out instead of holding
// every writer, and the browser is ended, since part of the message may be on the pipe.
func TestAWriteToAStalledBrowserTimesOut(t *testing.T) {
	defer cdppipe.ShortenWrites(200 * time.Millisecond)()
	c := launchFake(t)
	if err := c.Send([]byte(`{"id":1,"method":"Stall"}`)); err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"id":2,"method":"Page.enable","params":{"pad":"` + strings.Repeat("x", 4<<20) + `"}}`)
	errs := make(chan error, 1)
	go func() { errs <- c.Send(big) }()
	select {
	case err := <-errs:
		if !errors.Is(err, ports.ErrBrowserGone) {
			t.Fatalf("a write to a stalled browser = %v, want ErrBrowserGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write to a browser that stopped reading never returned")
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a browser whose pipe is out of step must be ended")
	}
}

func TestArgsHaveNoPortAndOnlyThePipe(t *testing.T) {
	args := cdppipe.Args(ports.BrowserLaunch{Executable: "/x", ProfileDir: "/p", Headless: true})
	want := []string{"--remote-debugging-pipe", "--user-data-dir=/p", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--headless=new", "about:blank"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

// realChrome finds a headless Chromium build already on this Mac, as the end to end tests do.
func realChrome(t *testing.T) string {
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
	} {
		matches, _ := filepath.Glob(filepath.Join(home, glob))
		if len(matches) > 0 {
			slices.Sort(matches)
			return matches[len(matches)-1]
		}
	}
	t.Skip("no headless Chromium found; set BERTH_E2E_CHROME to one")
	return ""
}

func TestRealChromiumOverThePipe(t *testing.T) {
	exe := realChrome(t)
	profile, err := os.MkdirTemp("", "berth-pipe-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c, err := cdppipe.Driver{}.Launch(ctx, ports.BrowserLaunch{Executable: exe, ProfileDir: profile, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	pgid := c.Process().PID
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if c.Process().Start.IsZero() {
		t.Fatal("the launched browser must carry its start time")
	}
	call := func(session, method string, params any) json.RawMessage {
		t.Helper()
		p, _ := json.Marshal(params)
		got, err := c.Call(ctx, session, method, p)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return got
	}
	var version struct{ Product string }
	_ = json.Unmarshal(call("", "Browser.getVersion", map[string]any{}), &version)
	if !strings.Contains(version.Product, "Chrome") {
		t.Fatalf("product = %q", version.Product)
	}
	var target struct{ TargetID string }
	_ = json.Unmarshal(call("", "Target.createTarget", map[string]any{"url": "about:blank"}), &target)
	var attached struct{ SessionID string }
	_ = json.Unmarshal(call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}), &attached)
	if !c.OwnSession(attached.SessionID) {
		t.Fatal("a session Berth attached to must be Berth's own")
	}
	call(attached.SessionID, "Page.navigate", map[string]any{"url": "data:text/html,<title>berth pipe</title>"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		var res struct {
			Result struct{ Value string }
		}
		_ = json.Unmarshal(call(attached.SessionID, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}), &res)
		if res.Result.Value == "berth pipe" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title = %q", res.Result.Value)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the browser outlived Kill")
	}
	if !testutil.Eventually(func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }) {
		t.Fatal("a process of the browser's group outlived Kill")
	}
}

// Downloads go to the profile's own folder with no prompt, and Chrome's other preferences stay.
func TestPrepareProfilePointsDownloadsAtTheProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(`{"browser":{"x":1},"download":{"default_directory":"/Users/you/Downloads","prompt_for_download":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cdppipe.PrepareProfile(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	var prefs struct {
		Browser  map[string]any
		Download struct {
			DefaultDirectory  string `json:"default_directory"`
			PromptForDownload bool   `json:"prompt_for_download"`
		}
		Savefile struct {
			DefaultDirectory string `json:"default_directory"`
		}
	}
	if err := json.Unmarshal(b, &prefs); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "Downloads")
	if prefs.Download.DefaultDirectory != want || prefs.Download.PromptForDownload || prefs.Savefile.DefaultDirectory != want || prefs.Browser["x"] != float64(1) {
		t.Fatalf("preferences = %s", b)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("downloads folder: %v %v", st, err)
	}
}

// Kill after the browser was reaped must signal nothing: the pid may already name another
// process group.
func TestKillAfterTheReapSignalsNothing(t *testing.T) {
	c := launchFake(t)
	kills, restore := cdppipe.WatchKills()
	defer restore()
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	<-c.Done()
	before := len(kills())
	if err := c.Kill(); err != nil {
		t.Fatalf("a second Kill after the reap = %v", err)
	}
	if got := kills(); len(got) != before || slices.ContainsFunc(got, func(g int) bool { return g != c.Process().PID }) {
		t.Fatalf("groups signalled = %v; after the reap Kill must signal nothing", got)
	}
}

// A helper that outlives its browser's leader is killed with the group before the leader is
// reaped.
func TestHelpersOutlivingTheLeaderAreKilled(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "helper-profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := cdppipe.Driver{}.Launch(t.Context(), ports.BrowserLaunch{Executable: exe, ProfileDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the fake browser did not exit")
	}
	b, err := os.ReadFile(filepath.Join(dir, "helper.pid"))
	pid, _ := strconv.Atoi(string(b))
	if err != nil || pid <= 0 {
		t.Fatalf("no helper pid: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !testutil.Eventually(func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }) {
		t.Fatal("a helper outlived its browser's leader")
	}
}

// An exit watch that fails for another reason than a zombie leader must not kill a live browser:
// berthd falls back to a plain wait.
func TestAFailedExitWatchLeavesTheBrowserRunning(t *testing.T) {
	restore := cdppipe.FailExitWatch(syscall.EMFILE)
	defer restore()
	c := launchFake(t)
	time.Sleep(200 * time.Millisecond)
	if _, err := c.Call(t.Context(), "", "Browser.getVersion", nil); err != nil {
		t.Fatalf("the browser died when its exit watch failed: %v", err)
	}
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Kill did not end the browser")
	}
}
