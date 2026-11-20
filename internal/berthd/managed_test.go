package berthd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/browser"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

type fakeProxy struct {
	mu      sync.Mutex
	served  []string
	stopped []string
	fail    bool
}

func (p *fakeProxy) Serve(id string, _ ports.BrowserConn, _ [32]byte) (string, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return "", nil, errors.New("no port")
	}
	p.served = append(p.served, id)
	return "http://127.0.0.1:9", func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.stopped = append(p.stopped, id)
	}, nil
}

func (p *fakeProxy) stops() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.stopped)
}

type fakeSeen struct {
	mu      sync.Mutex
	inUse   string
	stopped []string
}

func (s *fakeSeen) ProfileInUse(dir string) bool { return dir == s.inUse }
func (s *fakeSeen) StopUnder(_ context.Context, root string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, root)
	return 0
}

type managedFixture struct {
	m      *managed
	driver *testutil.Browsers
	proxy  *fakeProxy
	seen   *fakeSeen
	store  *testutil.Store
	home   string
	exe    string
}

func newManagedFixture(t *testing.T) *managedFixture {
	t.Helper()
	home := t.TempDir()
	exe := filepath.Join(t.TempDir(), "chrome-headless-shell")
	if err := os.WriteFile(exe, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &managedFixture{driver: &testutil.Browsers{}, proxy: &fakeProxy{}, seen: &fakeSeen{}, store: testutil.NewStore(), home: home, exe: exe}
	f.m = newManaged(t.Context(), managedDeps{
		Log: slog.New(slog.DiscardHandler), Store: f.store, List: f.store, Clock: testutil.NewClock(),
		Driver: f.driver, Proxy: f.proxy, Seen: f.seen, Home: home, Browser: exe,
		AgentLive: func(context.Context, string) bool { return true },
	})
	// A browser a test leaves running would hold its exit watch, and the bubble, open forever.
	t.Cleanup(func() {
		for _, b := range f.driver.Launched() {
			b.Exit()
		}
	})
	return f
}

func (f *managedFixture) launch(t *testing.T, profile string) (*berthv1.LaunchBrowserResponse, error) {
	t.Helper()
	return f.m.LaunchBrowser(t.Context(), &berthv1.LaunchBrowserRequest{RequestId: "launch-" + profile, Profile: profile, Headless: true})
}

func (f *managedFixture) events(t *testing.T, id string) []string {
	t.Helper()
	hist, err := f.store.History(t.Context(), browser.Entity(id), 20)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := len(hist) - 1; i >= 0; i-- {
		names = append(names, hist[i].Event)
	}
	return names
}

func waitBrowserClosed(t *testing.T, m *managed, id string) {
	t.Helper()
	synctest.Wait()
	if m.state(id) != browser.Closed {
		t.Fatalf("browser %s is %s, want Closed", id, m.state(id))
	}
}

func TestLaunchReachesReadyWithAProxyAndAToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		resp, err := f.launch(t, "work")
		if err != nil {
			t.Fatal(err)
		}
		b := resp.GetBrowser()
		if b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY || b.GetCdpEndpoint() != "http://127.0.0.1:9" || b.GetProduct() != "FakeChrome/1.0" {
			t.Fatalf("browser = %v", b)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(resp.GetCdpToken()) || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(b.GetId()) {
			t.Fatalf("token or id malformed: %q %q", resp.GetCdpToken(), b.GetId())
		}
		if b.GetSessionId() != "5001-1800000000000000" {
			t.Fatalf("session id = %q, want the observed session's <pid>-<start µs>", b.GetSessionId())
		}
		launched := f.driver.Launched()
		dir := filepath.Join(f.home, "profiles", "work")
		if len(launched) != 1 || launched[0].Launch != (ports.BrowserLaunch{Executable: f.exe, ProfileDir: dir, Headless: true}) {
			t.Fatalf("launches = %+v", launched)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("profile folder: %v %v", st, err)
		}
		if got := f.events(t, b.GetId()); !slices.Equal(got, []string{"Launch", "Started"}) {
			t.Fatalf("events = %v", got)
		}
		list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{})
		if len(list.GetBrowsers()) != 1 || list.GetBrowsers()[0].GetId() != b.GetId() {
			t.Fatalf("list = %v", list)
		}
	})
}

func TestOneLiveBrowserPerProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		first, err := f.launch(t, "work")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.launch(t, "work"); !errors.Is(err, api.ErrProfileBusy) {
			t.Fatalf("second launch on a held profile = %v, want ErrProfileBusy", err)
		}
		if _, err := f.launch(t, "other"); err != nil {
			t.Fatalf("another profile must launch: %v", err)
		}
		if _, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: first.GetBrowser().GetId()}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.launch(t, "work"); err != nil {
			t.Fatalf("a closed browser's profile must be free again: %v", err)
		}
	})
}

func TestAProfileABrowserOutsideBerthUsesIsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		f.seen.inUse = filepath.Join(f.home, "profiles", "work")
		if _, err := f.launch(t, "work"); !errors.Is(err, api.ErrProfileBusy) {
			t.Fatalf("launch = %v, want ErrProfileBusy", err)
		}
		if len(f.driver.Launched()) != 0 {
			t.Fatal("nothing may start on a busy profile")
		}
	})
}

func TestOnlyChromiumFamilyExecutablesLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		for _, exe := range []string{"/bin/sh", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", filepath.Join(t.TempDir(), "chrome-headless-shell")} {
			_, err := f.m.LaunchBrowser(t.Context(), &berthv1.LaunchBrowserRequest{RequestId: "launch-x", Profile: "work", Executable: exe})
			if !errors.Is(err, api.ErrBrowserLaunchFailed) {
				t.Fatalf("launch of %s = %v, want ErrBrowserLaunchFailed", exe, err)
			}
		}
		if len(f.driver.Launched()) != 0 {
			t.Fatal("a refused executable must never start")
		}
	})
}

func TestAFailedLaunchLeavesNothingBehind(t *testing.T) {
	for name, set := range map[string]func(*testutil.Browsers){
		"does not start":   func(d *testutil.Browsers) { d.LaunchErr = errors.New("exec failed") },
		"does not answer":  func(d *testutil.Browsers) { d.Silent = true },
		"proxy cannot run": nil,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newManagedFixture(t)
				if set != nil {
					set(f.driver)
				} else {
					f.proxy.fail = true
				}
				if _, err := f.launch(t, "work"); !errors.Is(err, api.ErrBrowserLaunchFailed) {
					t.Fatalf("launch = %v, want ErrBrowserLaunchFailed", err)
				}
				for _, b := range f.driver.Launched() {
					if !b.Killed() {
						t.Fatal("a browser that failed to launch must be killed")
					}
				}
				list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{IncludeClosed: true})
				if len(list.GetBrowsers()) != 1 || list.GetBrowsers()[0].GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED {
					t.Fatalf("list = %v", list)
				}
				f.driver.LaunchErr, f.driver.Silent, f.proxy.fail = nil, false, false
				if _, err := f.launch(t, "work"); err != nil {
					t.Fatalf("the profile must be free after a failed launch: %v", err)
				}
			})
		})
	}
}

func TestCloseAndItsKillAfterTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		resp, _ := f.launch(t, "work")
		id := resp.GetBrowser().GetId()
		closed, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: id})
		if err != nil || closed.GetBrowser().GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED || closed.GetBrowser().GetCloseReason() != "closed" {
			t.Fatalf("close = %v %v", closed, err)
		}
		if got := f.events(t, id); !slices.Equal(got, []string{"Launch", "Started", "CloseRequested", "Exited"}) {
			t.Fatalf("events = %v", got)
		}
		if !slices.Equal(f.proxy.stops(), []string{id}) {
			t.Fatalf("proxy stops = %v; a closed browser's proxy must stop", f.proxy.stops())
		}
		again, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: id})
		if err != nil || again.GetBrowser().GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED {
			t.Fatalf("closing a closed browser = %v %v", again, err)
		}

		f.driver.Stubborn = true
		resp, _ = f.launch(t, "stubborn")
		id = resp.GetBrowser().GetId()
		if _, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: id}); err != nil {
			t.Fatal(err)
		}
		if got := f.events(t, id); !slices.Equal(got, []string{"Launch", "Started", "CloseRequested", "CloseTimedOut", "Exited"}) {
			t.Fatalf("a browser ignoring Browser.close must be killed after the grace; events = %v", got)
		}
		if !f.driver.Launched()[1].Killed() {
			t.Fatal("KillGroup did not kill")
		}
		if _, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: "0123456789abcdef"}); !errors.Is(err, api.ErrBrowserNotFound) {
			t.Fatalf("close of an unknown id = %v", err)
		}
	})
}

// A browser that stopped reading its pipe holds the Browser.close write; the grace still runs
// out and its group is killed, rather than the close waiting on the write.
func TestABrowserWedgedOnItsPipeIsKilledAfterTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		f.driver.Wedged = true
		resp, _ := f.launch(t, "wedged")
		id := resp.GetBrowser().GetId()
		if _, err := f.m.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{Id: id}); err != nil {
			t.Fatal(err)
		}
		waitBrowserClosed(t, f.m, id)
		if got := f.events(t, id); !slices.Equal(got, []string{"Launch", "Started", "CloseRequested", "CloseTimedOut", "Exited"}) {
			t.Fatalf("events = %v; a wedged browser must be killed after the grace", got)
		}
		if !f.driver.Launched()[0].Killed() {
			t.Fatal("KillGroup did not kill")
		}
	})
}

func TestABrowserThatExitsOnItsOwnIsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		resp, _ := f.launch(t, "work")
		id := resp.GetBrowser().GetId()
		f.driver.Launched()[0].Exit()
		waitBrowserClosed(t, f.m, id)
		list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{IncludeClosed: true})
		if b := list.GetBrowsers()[0]; b.GetCloseReason() != "exited" || b.GetCdpEndpoint() != "" {
			t.Fatalf("browser = %v", b)
		}
		if _, err := f.launch(t, "work"); err != nil {
			t.Fatalf("an exited browser's profile must be free: %v", err)
		}
	})
}

func TestBootEndsBrowsersAPreviousRunLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		entity := browser.Entity("00000000000000aa")
		for _, rec := range []ports.TransitionRecord{
			{Entity: entity, Kind: browser.Kind, Version: 1, Event: "Launch", From: "Requested", To: "Launching"},
			{Entity: entity, Kind: browser.Kind, Version: 2, Event: "Started", From: "Launching", To: "Ready"},
		} {
			if err := f.store.SaveTransition(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.m.recover(t.Context()); err != nil {
			t.Fatal(err)
		}
		st, _, _ := f.store.LoadState(t.Context(), entity)
		hist, _ := f.store.History(t.Context(), entity, 1)
		if st.State != string(browser.Closed) || hist[0].Detail != "gone at boot" {
			t.Fatalf("state %s, last %+v", st.State, hist[0])
		}
		if !slices.Equal(f.seen.stopped, []string{filepath.Join(f.home, "profiles")}) {
			t.Fatalf("boot must stop browsers left on Berth profiles; StopUnder got %v", f.seen.stopped)
		}
	})
}

func TestShutdownClosesEveryBrowser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		a, _ := f.launch(t, "a")
		f.driver.Stubborn = true
		b, _ := f.launch(t, "b")
		f.m.stop(t.Context(), 300*time.Millisecond)
		for _, id := range []string{a.GetBrowser().GetId(), b.GetBrowser().GetId()} {
			waitBrowserClosed(t, f.m, id)
		}
		if !f.driver.Launched()[1].Killed() {
			t.Fatal("a browser ignoring Browser.close must be killed at shutdown")
		}
		if _, err := f.launch(t, "c"); !errors.Is(err, api.ErrBrowserLaunchFailed) {
			t.Fatalf("a launch after stop = %v, want ErrBrowserLaunchFailed", err)
		}
	})
}

// An owner who gives up while the browser starts leaves no browser the owner tools cannot see:
// the launch settles on berthd's context and the browser is closed, its profile lock freed.
func TestACancelledLaunchClosesItsBrowser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newManagedFixture(t)
		f.driver.Hold = make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := f.m.LaunchBrowser(ctx, &berthv1.LaunchBrowserRequest{RequestId: "launch-work", Profile: "work", Headless: true})
			done <- err
		}()
		id := waitLiveBrowser(t, f)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("launch = %v, want context.Canceled", err)
		}
		close(f.driver.Hold)
		waitBrowserClosed(t, f.m, id)
		list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{IncludeClosed: true})
		if len(list.GetBrowsers()) != 1 || list.GetBrowsers()[0].GetId() != id {
			t.Fatalf("the abandoned browser must stay visible to the owner: %v", list)
		}
		select {
		case <-f.driver.Launched()[0].Done():
		default:
			t.Fatal("the abandoned browser's process must be closed")
		}
		f.driver.Hold = nil
		if _, err := f.launch(t, "work"); err != nil {
			t.Fatalf("the profile must be free again: %v", err)
		}
	})
}
