package berthd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/browser"
	"berth/internal/testutil"
)

const (
	sessionA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sessionB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// liveSessions is an AgentLive whose sessions a test ends.
type liveSessions struct {
	mu    sync.Mutex
	ended map[string]bool
	// endOnCall ends the session at this call to live, counting from 1, to race a launch.
	endOnCall int
	calls     int
}

func (l *liveSessions) live(_ context.Context, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.endOnCall > 0 && l.calls >= l.endOnCall {
		return false
	}
	return !l.ended[id]
}

func newAgentBrowsersFixture(t *testing.T) (*managedFixture, *liveSessions) {
	t.Helper()
	f := newManagedFixture(t)
	l := &liveSessions{ended: map[string]bool{}}
	f.m.AgentLive = l.live
	return f, l
}

func openAs(t *testing.T, f *managedFixture, agent string) (*berthv1.OpenAgentBrowserResponse, error) {
	t.Helper()
	return f.m.OpenAgentBrowser(api.WithAgentSession(t.Context(), agent), &berthv1.OpenAgentBrowserRequest{RequestId: "open-request"})
}

func closeAs(t *testing.T, f *managedFixture, agent, id string) (*berthv1.CloseAgentBrowserResponse, error) {
	t.Helper()
	return f.m.CloseAgentBrowser(api.WithAgentSession(t.Context(), agent), &berthv1.CloseAgentBrowserRequest{RequestId: "close-request", Id: id})
}

func (f *managedFixture) profileExists(name string) bool {
	_, err := os.Stat(filepath.Join(f.home, "profiles", name))
	return err == nil
}

func TestAgentBrowserIsHeadlessOnItsOwnProfileAndListedWithItsSession(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	resp, err := openAs(t, f, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	b := resp.GetBrowser()
	if b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY || b.GetCdpEndpoint() == "" || len(resp.GetCdpToken()) != 64 {
		t.Fatalf("open = %v", resp)
	}
	launched := f.driver.Launched()
	profile := "agent-" + sessionA + "-1"
	if len(launched) != 1 || !launched[0].Launch.Headless || launched[0].Launch.Executable != f.exe || launched[0].Launch.ProfileDir != filepath.Join(f.home, "profiles", profile) {
		t.Fatalf("launches = %+v", launched)
	}
	list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{})
	if len(list.GetBrowsers()) != 1 || list.GetBrowsers()[0].GetAgentSessionId() != sessionA || list.GetBrowsers()[0].GetProfile() != profile {
		t.Fatalf("the owner must see the agent browser with its session: %v", list)
	}
	second, err := openAs(t, f, sessionA)
	if err != nil || f.driver.Launched()[1].Launch.ProfileDir != filepath.Join(f.home, "profiles", "agent-"+sessionA+"-2") {
		t.Fatalf("a second browser gets the next profile: %v %v", second, err)
	}
}

func TestAgentBrowserCloseDeletesItsProfile(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	resp, err := openAs(t, f, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	profile := "agent-" + sessionA + "-1"
	if !f.profileExists(profile) {
		t.Fatal("the profile folder must exist while the browser runs")
	}
	closed, err := closeAs(t, f, sessionA, resp.GetBrowser().GetId())
	if err != nil || closed.GetBrowser().GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED {
		t.Fatalf("close = %v %v", closed, err)
	}
	if f.profileExists(profile) {
		t.Fatal("a closed agent browser's profile must be deleted")
	}
	again, err := closeAs(t, f, sessionA, resp.GetBrowser().GetId())
	if err != nil || again.GetBrowser().GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED {
		t.Fatalf("closing a closed agent browser = %v %v", again, err)
	}
}

func TestAgentBrowserReachesOnlyItsOwnSession(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	mine, err := openAs(t, f, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := f.launch(t, "work")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ agent, id string }{
		"another session's browser": {sessionB, mine.GetBrowser().GetId()},
		"the owner's browser":       {sessionA, owners.GetBrowser().GetId()},
		"an unknown id":             {sessionA, "0123456789abcdef"},
		"no agent session":          {"", mine.GetBrowser().GetId()},
	} {
		if _, err := closeAs(t, f, tc.agent, tc.id); !errors.Is(err, api.ErrBrowserNotFound) {
			t.Fatalf("%s: close = %v, want ErrBrowserNotFound", name, err)
		}
	}
	for agent, want := range map[string]int{sessionA: 1, sessionB: 0, "": 0} {
		list, _ := f.m.ListAgentBrowsers(api.WithAgentSession(t.Context(), agent))
		if len(list.GetBrowsers()) != want || want == 1 && list.GetBrowsers()[0].GetId() != mine.GetBrowser().GetId() {
			t.Fatalf("session %q lists %v, want only its own %d", agent, list, want)
		}
	}
	for _, id := range []string{mine.GetBrowser().GetId(), owners.GetBrowser().GetId()} {
		if st := f.m.state(id); st != browser.Ready {
			t.Fatalf("a refused close changed browser %s to %s", id, st)
		}
	}
	if _, err := f.m.OpenAgentBrowser(t.Context(), &berthv1.OpenAgentBrowserRequest{RequestId: "open-request"}); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("open with no agent session = %v", err)
	}
}

func TestAgentBrowserLimits(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	clock, _ := f.m.Clock.(*testutil.Clock)
	first, _ := openAs(t, f, sessionA)
	if _, err := openAs(t, f, sessionA); err != nil {
		t.Fatal(err)
	}
	if _, err := openAs(t, f, sessionA); !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("a third open browser = %v, want ErrRateLimited", err)
	}
	if _, err := openAs(t, f, sessionB); err != nil {
		t.Fatalf("another session has its own limit: %v", err)
	}
	if _, err := closeAs(t, f, sessionA, first.GetBrowser().GetId()); err != nil {
		t.Fatal(err)
	}
	// Two opened, one closed: seven more opens and closes reach ten in the hour.
	for range 8 {
		resp, err := openAs(t, f, sessionA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := closeAs(t, f, sessionA, resp.GetBrowser().GetId()); err != nil {
			t.Fatal(err)
		}
	}
	launches := len(f.driver.Launched())
	if _, err := openAs(t, f, sessionA); !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("an eleventh open in an hour = %v, want ErrRateLimited", err)
	}
	if len(f.driver.Launched()) != launches {
		t.Fatal("a refused open must start nothing")
	}
	clock.Advance(time.Hour)
	if _, err := openAs(t, f, sessionA); err != nil {
		t.Fatalf("an hour later the session may open again: %v", err)
	}
}

func TestAgentSessionEndClosesItsBrowsersAndDeletesTheirProfiles(t *testing.T) {
	f, l := newAgentBrowsersFixture(t)
	a1, _ := openAs(t, f, sessionA)
	a2, _ := openAs(t, f, sessionA)
	b1, _ := openAs(t, f, sessionB)
	l.mu.Lock()
	l.ended[sessionA] = true
	l.mu.Unlock()
	f.m.closeAgent(t.Context(), sessionA)
	for _, id := range []string{a1.GetBrowser().GetId(), a2.GetBrowser().GetId()} {
		waitBrowserClosed(t, f.m, id)
	}
	for n := range 2 {
		if name := "agent-" + sessionA + "-" + string(rune('1'+n)); f.profileExists(name) {
			t.Fatalf("profile %s survived its session's end", name)
		}
	}
	if f.m.state(b1.GetBrowser().GetId()) != browser.Ready || !f.profileExists("agent-"+sessionB+"-1") {
		t.Fatal("another session's browser must be left alone")
	}
	if _, err := openAs(t, f, sessionA); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("an ended session's open = %v", err)
	}
}

// A session that ends while its browser starts finds no browser to close; the open closes it.
func TestAgentSessionEndingDuringOpenClosesTheBrowser(t *testing.T) {
	f, l := newAgentBrowsersFixture(t)
	l.endOnCall = 2
	// The end hook ran first; the open still read the session live and recreated its counts.
	f.m.closeAgent(t.Context(), sessionA)
	if _, err := openAs(t, f, sessionA); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("open = %v, want ErrAgentTokenInvalid", err)
	}
	f.m.mu.Lock()
	_, opened := f.m.opened[sessionA]
	_, seq := f.m.seq[sessionA]
	f.m.mu.Unlock()
	if opened || seq {
		t.Fatal("the ended session's counts were left behind by the racing open")
	}
	list, _ := f.m.ListBrowsers(t.Context(), &berthv1.ListBrowsersRequest{IncludeClosed: true})
	if len(list.GetBrowsers()) != 1 {
		t.Fatalf("list = %v", list)
	}
	waitBrowserClosed(t, f.m, list.GetBrowsers()[0].GetId())
	if f.profileExists("agent-" + sessionA + "-1") {
		t.Fatal("the browser of a session that ended during its open must close and lose its profile")
	}
}

func TestOwnerCannotLaunchOnAnAgentProfile(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	if _, err := f.launch(t, "agent-"+sessionA+"-1"); !errors.Is(err, api.ErrBrowserLaunchFailed) {
		t.Fatalf("launch = %v, want ErrBrowserLaunchFailed", err)
	}
	if len(f.driver.Launched()) != 0 {
		t.Fatal("nothing may start on an agent profile name")
	}
}

func TestBootDeletesOnlyAgentProfilesLeftBehind(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	for _, name := range []string{"agent-" + sessionA + "-1", "agent-" + sessionB + "-3", "work", "agent-notes"} {
		if err := os.MkdirAll(filepath.Join(f.home, "profiles", name, "Default"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.seen.inUse = filepath.Join(f.home, "profiles", "agent-"+sessionB+"-3")
	if err := f.m.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"agent-" + sessionA + "-1": false, "agent-" + sessionB + "-3": true, "work": true, "agent-notes": true} {
		if f.profileExists(name) != want {
			t.Fatalf("after boot, profile %s exists = %v, want %v", name, !want, want)
		}
	}
}

// The agent session's end reaches its browsers through the saved hook, on berthd's context.
func TestAgentSessionRevokeClosesItsBrowsers(t *testing.T) {
	af := newAgentsFixture(t)
	f := newManagedFixture(t)
	a := newAgents(t.Context(), agentsDeps{
		Log: slog.New(slog.DiscardHandler), Store: af.store, Records: af.store, Clock: af.clock,
		Lineage: af.procs, Code: anyLive{af.procs}, AgentUID: agentUID, After: af.timers.after,
		StopProxies: func(context.Context, string) {}, WatchExit: af.exits.watch,
		CloseBrowsers: func(ctx context.Context, id string) {
			if ctx.Err() == nil {
				f.m.closeAgent(ctx, id)
			}
		},
	})
	f.m.AgentLive = a.live
	created, err := a.CreateAgentSession(t.Context(), &berthv1.CreateAgentSessionRequest{RequestId: "create-request"})
	if err != nil {
		t.Fatal(err)
	}
	id := created.GetSession().GetId()
	resp, err := openAs(t, f, id)
	if err != nil {
		t.Fatal(err)
	}
	// A caller that gives up right after the revoke still closes the browser.
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := a.RevokeAgentSession(ctx, &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-request", Id: id}); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitBrowserClosed(t, f.m, resp.GetBrowser().GetId())
	if f.profileExists("agent-" + id + "-1") {
		t.Fatal("the revoked session's profile must be deleted")
	}
	if events := f.events(t, resp.GetBrowser().GetId()); !strings.Contains(strings.Join(events, " "), "CloseRequested") {
		t.Fatalf("events = %v", events)
	}
}

// A caller that gives up while its browser starts leaves nothing running: the launch finishes on
// berthd's context and the browser is closed, its profile deleted.
func TestACancelledOpenClosesItsBrowser(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	f.m.launch = 5 * time.Second
	f.driver.Hold = make(chan struct{})
	ctx, cancel := context.WithCancel(api.WithAgentSession(t.Context(), sessionA))
	done := make(chan error, 1)
	go func() {
		_, err := f.m.OpenAgentBrowser(ctx, &berthv1.OpenAgentBrowserRequest{RequestId: "open-request"})
		done <- err
	}()
	id := waitLiveBrowser(t, f)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("open = %v, want context.Canceled", err)
	}
	close(f.driver.Hold)
	waitBrowserClosed(t, f.m, id)
	if f.profileExists("agent-" + sessionA + "-1") {
		t.Fatal("the abandoned browser's profile must be deleted")
	}
	if launched := f.driver.Launched(); len(launched) != 1 {
		t.Fatalf("launches = %d", len(launched))
	}
}

// A session that ends while an open waits on the launch closes the browser once it starts, with
// the session's end hook having found it still without a row.
func TestSessionEndDuringAnInFlightOpenClosesIt(t *testing.T) {
	f, l := newAgentBrowsersFixture(t)
	f.m.launch = 5 * time.Second
	f.driver.Hold = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := openAs(t, f, sessionA); done <- err }()
	id := waitLiveBrowser(t, f)
	l.mu.Lock()
	l.ended[sessionA] = true
	l.mu.Unlock()
	f.m.closeAgent(t.Context(), sessionA)
	close(f.driver.Hold)
	if err := <-done; !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("open = %v, want ErrAgentTokenInvalid", err)
	}
	waitBrowserClosed(t, f.m, id)
	if f.profileExists("agent-" + sessionA + "-1") {
		t.Fatal("the browser's profile must be deleted")
	}
}

// waitLiveBrowser waits for one browser to be live and returns its id.
func waitLiveBrowser(t *testing.T, f *managedFixture) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f.m.mu.Lock()
		for id := range f.m.live {
			f.m.mu.Unlock()
			return id
		}
		f.m.mu.Unlock()
	}
	t.Fatal("no browser went live")
	return ""
}

// After a restart the profile numbers start again; one left on disk is skipped, not reused.
func TestAgentProfileSkipsAFolderLeftBehind(t *testing.T) {
	f, _ := newAgentBrowsersFixture(t)
	left := filepath.Join(f.home, "profiles", "agent-"+sessionA+"-1")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openAs(t, f, sessionA); err != nil {
		t.Fatal(err)
	}
	if got := f.driver.Launched()[0].Launch.ProfileDir; got != filepath.Join(f.home, "profiles", "agent-"+sessionA+"-2") {
		t.Fatalf("profile = %s, want the next free number", got)
	}
}
