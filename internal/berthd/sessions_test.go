package berthd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/discovery"
	"berth/internal/core/ports"
	"berth/internal/core/session"
	"berth/internal/testutil"
)

const (
	testUID   = 501
	shellPath = "/Users/u/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell"
	chromeApp = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
)

var bootTime = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

func row(pid, ppid int, uid uint32, exe string, args ...string) testutil.FakeProcess {
	return testutil.FakeProcess{
		ProcessInfo: ports.ProcessInfo{ProcessRef: ports.ProcessRef{PID: pid, Start: bootTime.Add(time.Duration(pid) * time.Second)}, PPID: ppid, UID: uid, Executable: exe},
		Args:        args,
	}
}

type browsersFixture struct {
	b     *browsers
	procs *testutil.Processes
	store *testutil.Store
	rows  []testutil.FakeProcess
}

// fixtureProcesses is the process table every browsers test starts from: a Playwright browser
// under a terminal, the person's Chrome, and another user's browser.
func fixtureProcesses() []testutil.FakeProcess {
	launchd := row(1, 0, 0, "/sbin/launchd")
	launchd.Opaque = true
	theirs := row(300, 1, 502, shellPath)
	theirs.Opaque = true
	return []testutil.FakeProcess{
		launchd,
		row(30, 1, testUID, "/Applications/Ghostty.app/Contents/MacOS/ghostty"),
		row(80, 30, testUID, "/opt/homebrew/bin/node", "node", "playwright", "test"),
		row(100, 80, testUID, shellPath, shellPath, "--headless", "--remote-debugging-port=0"),
		row(101, 100, testUID, shellPath, shellPath, "--type=renderer"),
		row(200, 1, testUID, chromeApp, chromeApp),
		theirs,
	}
}

func newBrowsersFixture(t *testing.T) *browsersFixture {
	t.Helper()
	f := &browsersFixture{procs: &testutil.Processes{}, store: testutil.NewStore(), rows: fixtureProcesses()}
	f.procs.Set(f.rows...)
	f.b = newBrowsers(t.Context(), browsersDeps{
		Log: slog.New(slog.DiscardHandler), Source: f.procs, Store: f.store, List: f.store, Events: f.store, Clock: testutil.NewClock(), UID: testUID,
		Changed: func() {},
	})
	if err := f.b.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *browsersFixture) id(pid int) string {
	for _, r := range f.rows {
		if r.PID == pid {
			return discovery.ID(r.ProcessRef)
		}
	}
	return ""
}

func TestListAndInspectSessions(t *testing.T) {
	f := newBrowsersFixture(t)
	resp, err := f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{})
	if err != nil || len(resp.GetSessions()) != 3 {
		t.Fatalf("list: %v %v", resp, err)
	}
	pw := resp.GetSessions()[0]
	if pw.GetId() != f.id(100) || pw.GetKind() != berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL || pw.GetDriver() != berthv1.Driver_DRIVER_PLAYWRIGHT ||
		pw.GetCdp() != berthv1.CdpMode_CDP_MODE_PORT || pw.GetOwner().GetKind() != berthv1.OwnerKind_OWNER_KIND_TERMINAL || pw.GetProcessCount() != 2 {
		t.Errorf("Playwright session %v", pw)
	}
	theirs := resp.GetSessions()[2]
	if !theirs.GetOtherUser() || theirs.GetDriver() != berthv1.Driver_DRIVER_UNSPECIFIED || theirs.GetUserDataDir() != "" {
		t.Errorf("other user's session %v", theirs)
	}
	got, err := f.b.InspectSession(t.Context(), &berthv1.InspectSessionRequest{Id: f.id(100)})
	if err != nil || len(got.GetProcesses()) != 2 || len(got.GetOwnerChain()) != 3 || got.GetOwnerChain()[0].GetPid() != 80 {
		t.Fatalf("inspect: %v %v", got, err)
	}
	if _, err := f.b.InspectSession(t.Context(), &berthv1.InspectSessionRequest{Id: "9-9"}); !errors.Is(err, api.ErrSessionNotFound) {
		t.Fatalf("inspect unknown: %v", err)
	}
	leftovers, err := f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{States: []berthv1.SessionState{berthv1.SessionState_SESSION_STATE_LEFTOVER}})
	if err != nil || len(leftovers.GetSessions()) != 1 || leftovers.GetSessions()[0].GetId() != f.id(300) {
		t.Fatalf("leftovers: %v %v", leftovers, err)
	}
}

func TestStopRefusesThePersonsAndOtherUsersBrowsers(t *testing.T) {
	f := newBrowsersFixture(t)
	if _, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: f.id(200)}); !errors.Is(err, api.ErrSessionNotStoppable) {
		t.Fatalf("stop the person's Chrome without force: %v", err)
	}
	if _, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: f.id(300), Force: true}); !errors.Is(err, api.ErrSessionNotStoppable) {
		t.Fatalf("stop another user's browser: %v", err)
	}
	if _, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: "9-9"}); !errors.Is(err, api.ErrSessionNotFound) {
		t.Fatalf("stop unknown: %v", err)
	}
	if len(f.procs.Signals()) != 0 {
		t.Fatalf("signals sent: %v", f.procs.Signals())
	}
}

func TestStopSignalsAndEndsWhenTheBrowserGoes(t *testing.T) {
	f := newBrowsersFixture(t)
	done := make(chan *berthv1.StopSessionResponse, 1)
	go func() {
		resp, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: f.id(100)})
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	testutil.Eventually(func() bool { return len(f.procs.Signals()) > 0 })
	if got := f.procs.Signals(); !slices.Equal(got, []testutil.SentSignal{{PID: 100, Signal: ports.SignalTerm}}) {
		t.Fatalf("signals %v, want SIGTERM to 100", got)
	}
	f.procs.Set(slices.DeleteFunc(slices.Clone(f.rows), func(r testutil.FakeProcess) bool { return r.PID == 100 || r.PID == 101 })...)
	if err := f.b.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp := <-done; resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
		t.Fatalf("stop answered %v", resp)
	}
	st, _, _ := f.store.LoadState(t.Context(), session.Entity(f.id(100)))
	if st.State != string(session.Stopped) {
		t.Fatalf("stored state %v", st)
	}
	all, _ := f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{IncludeStopped: true})
	if len(all.GetSessions()) != 3 {
		t.Fatalf("include_stopped lists %d sessions, want 3", len(all.GetSessions()))
	}
}

func TestBootEndsSessionsThatAreGone(t *testing.T) {
	f := newBrowsersFixture(t)
	gone := session.Entity("7-7")
	if err := f.store.SaveTransition(t.Context(), ports.TransitionRecord{Entity: gone, Kind: session.Kind, Version: 1, Event: "Busy", From: "Discovered", To: "Active"}); err != nil {
		t.Fatal(err)
	}
	if err := f.b.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := f.store.LoadState(t.Context(), gone); st.State != string(session.Stopped) {
		t.Fatalf("a session gone at boot is %s", st.State)
	}
	if f.hasActor(gone) {
		t.Fatal("a session ended at boot kept its actor")
	}
}

func (f *browsersFixture) hasActor(entity string) bool {
	f.b.actors.mu.Lock()
	defer f.b.actors.mu.Unlock()
	_, ok := f.b.actors.live[entity]
	return ok
}

// An event that reaches a session after it is Stopped, as a stop or the grace timer racing the
// exit does, must not start an actor that nothing will ever stop.
func TestLateEventsToAStoppedSessionStartNoActor(t *testing.T) {
	f := newBrowsersFixture(t)
	id := f.id(100)
	f.set(t, slices.DeleteFunc(slices.Clone(f.rows), func(r testutil.FakeProcess) bool { return r.PID == 100 || r.PID == 101 })...)
	if st, _ := f.b.state(t.Context(), session.Entity(id)); st != session.Stopped {
		t.Fatalf("the exited session is %s", st)
	}
	f.b.send(t.Context(), id, session.Event{Kind: session.StopRequested})
	f.b.send(t.Context(), id, session.Event{Kind: session.StopTimedOut})
	if f.hasActor(session.Entity(id)) {
		t.Fatal("a late event to a Stopped session started an actor")
	}
}

func (f *browsersFixture) set(t *testing.T, rows ...testutil.FakeProcess) {
	t.Helper()
	f.rows = rows
	f.procs.Set(rows...)
	if err := f.b.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func waitSignals(t *testing.T, procs *testutil.Processes, n int) []testutil.SentSignal {
	t.Helper()
	testutil.Eventually(func() bool { return len(procs.Signals()) >= n })
	return procs.Signals()
}

// A spawner of short lived fake browsers must not grow berthd: stopped sessions give up their
// actor, and only the newest endedMax stay in memory, none older than endedAge.
func TestChurnStaysBounded(t *testing.T) {
	f := newBrowsersFixture(t)
	base := slices.Clone(f.rows)
	for i := range 3 * endedMax {
		fake := row(1000+i, 80, testUID, shellPath, shellPath, "--headless")
		f.set(t, append(slices.Clone(base), fake)...)
		f.set(t, base...)
	}
	f.b.mu.Lock()
	live, ended, order := len(f.b.live), len(f.b.ended), len(f.b.endedOrder)
	f.b.mu.Unlock()
	if actors := f.b.actors.count(); actors > live {
		t.Errorf("%d actors running for %d live sessions", actors, live)
	}
	if live != 3 || ended != endedMax || order != endedMax {
		t.Errorf("live %d, ended %d (order %d); want 3 and %d", live, ended, order, endedMax)
	}
	f.b.Clock.(*testutil.Clock).Advance(endedAge + time.Minute)
	all, err := f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{IncludeStopped: true})
	if err != nil || len(all.GetSessions()) != 3 {
		t.Fatalf("after an hour include_stopped lists %d sessions, want the 3 live ones (%v)", len(all.GetSessions()), err)
	}
	// A trimmed session is still Stopped in the database.
	resp, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: discovery.ID(row(1000, 80, testUID, shellPath).ProcessRef)})
	if err != nil || resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
		t.Fatalf("stop of a trimmed session: %v %v", resp, err)
	}
}

func TestStopRefusesAnAppsBrowserWithoutForce(t *testing.T) {
	f := newBrowsersFixture(t)
	f.set(t, append(slices.Clone(f.rows),
		row(60, 1, testUID, "/Applications/Cursor.app/Contents/MacOS/Cursor"),
		row(61, 60, testUID, "/opt/homebrew/bin/node", "node", "server.js"),
		row(110, 61, testUID, shellPath, shellPath, "--headless"),
	)...)
	id := f.id(110)
	if _, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: id}); !errors.Is(err, api.ErrSessionNotStoppable) {
		t.Fatalf("stop an app's browser without force: %v", err)
	}
	if len(f.procs.Signals()) != 0 {
		t.Fatalf("signals sent: %v", f.procs.Signals())
	}
	go func() { _, _ = f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: id, Force: true}) }()
	if got := waitSignals(t, f.procs, 1); !slices.Equal(got, []testutil.SentSignal{{PID: 110, Signal: ports.SignalTerm}}) {
		t.Fatalf("forced stop sent %v", got)
	}
}

// A browser that ignores SIGTERM gets SIGKILL after the grace, on every process of the session
// berthd's user owns and on no other.
func TestStopTimesOutIntoSIGKILLOfOwnProcessesOnly(t *testing.T) {
	f := newBrowsersFixture(t)
	f.b.grace = 20 * time.Millisecond
	f.set(t, append(slices.Clone(f.rows), row(102, 100, 502, shellPath))...)
	id := f.id(100)
	done := make(chan *berthv1.StopSessionResponse, 1)
	go func() {
		resp, _ := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: id})
		done <- resp
	}()
	got := waitSignals(t, f.procs, 3)
	want := []testutil.SentSignal{{PID: 100, Signal: ports.SignalTerm}, {PID: 100, Signal: ports.SignalKill}, {PID: 101, Signal: ports.SignalKill}}
	if !slices.Equal(got, want) {
		t.Fatalf("signals %v, want %v", got, want)
	}
	if st, _ := f.b.state(t.Context(), session.Entity(id)); st != session.Stopping {
		t.Fatalf("after SIGKILL the session is %s, want Stopping until it is gone", st)
	}
	f.set(t, slices.DeleteFunc(slices.Clone(f.rows), func(r testutil.FakeProcess) bool { return r.PID >= 100 && r.PID <= 102 })...)
	if resp := <-done; resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
		t.Fatalf("stop answered %v", resp)
	}
}

// stream runs WatchSessions as a client would, collecting what berthd sends.
type stream struct {
	msgs   chan *berthv1.WatchSessionsResponse
	done   chan error
	cancel context.CancelFunc
}

func (f *browsersFixture) watch(t *testing.T, after int64) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &stream{msgs: make(chan *berthv1.WatchSessionsResponse, 1024), done: make(chan error, 1), cancel: cancel}
	go func() {
		s.done <- f.b.WatchSessions(ctx, after, func(m *berthv1.WatchSessionsResponse) error {
			s.msgs <- m
			return nil
		})
	}()
	t.Cleanup(cancel)
	return s
}

func (s *stream) next(t *testing.T) *berthv1.WatchSessionsResponse {
	t.Helper()
	select {
	case m := <-s.msgs:
		return m
	case err := <-s.done:
		t.Fatalf("stream ended: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no stream message within 3 seconds")
	}
	return nil
}

// change reads the next message, which must be a change of session id to state want.
func (s *stream) change(t *testing.T, id, event string, want berthv1.SessionState) *berthv1.SessionChange {
	t.Helper()
	c := s.next(t).GetChange()
	if c.GetSession().GetId() != id || c.GetEvent() != event || c.GetSession().GetState() != want {
		t.Fatalf("change %v, want %s %s to %s", c, id, event, want)
	}
	return c
}

func (s *stream) quiet(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.msgs:
		t.Fatalf("unexpected message %v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func extraBrowser(pid int) testutil.FakeProcess {
	return row(pid, 80, testUID, shellPath, shellPath, "--headless")
}

func TestWatchStartsWithASnapshotThenStreamsChanges(t *testing.T) {
	f := newBrowsersFixture(t)
	s := f.watch(t, 0)
	snap := s.next(t).GetSnapshot()
	if snap.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_START || len(snap.GetSessions()) != 3 || snap.GetEventId() == 0 {
		t.Fatalf("snapshot %v", snap)
	}
	if got := snap.GetSessions()[2]; got.GetId() != f.id(300) || got.GetState() != berthv1.SessionState_SESSION_STATE_LEFTOVER {
		t.Fatalf("snapshot shows the other user's browser as %v", got)
	}
	base := slices.Clone(f.rows)
	f.set(t, append(slices.Clone(base), extraBrowser(500))...)
	id := f.id(500)
	c := s.change(t, id, "Seen", berthv1.SessionState_SESSION_STATE_DISCOVERED)
	if c.GetEventId() <= snap.GetEventId() || c.GetSession().GetKind() != berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL ||
		c.GetSession().GetOwner().GetKind() != berthv1.OwnerKind_OWNER_KIND_TERMINAL {
		t.Fatalf("Seen change %v after snapshot %d", c, snap.GetEventId())
	}
	f.set(t, base...)
	s.change(t, id, "Exited", berthv1.SessionState_SESSION_STATE_STOPPED)
	s.quiet(t)
}

// A client that drops its stream and reconnects with the last event id it applied gets exactly
// what it missed, in order, and no snapshot.
func TestWatchResumesAfterDisconnect(t *testing.T) {
	f := newBrowsersFixture(t)
	first := f.watch(t, 0)
	cursor := first.next(t).GetSnapshot().GetEventId()
	first.cancel()
	if err := <-first.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled stream returned %v", err)
	}
	base := slices.Clone(f.rows)
	f.set(t, append(slices.Clone(base), extraBrowser(500), extraBrowser(501))...)
	f.set(t, append(slices.Clone(base), extraBrowser(501))...)
	resumed := f.watch(t, cursor)
	resumed.change(t, discovery.ID(extraBrowser(500).ProcessRef), "Seen", berthv1.SessionState_SESSION_STATE_DISCOVERED)
	last := resumed.change(t, f.id(501), "Seen", berthv1.SessionState_SESSION_STATE_DISCOVERED)
	gone := resumed.change(t, discovery.ID(extraBrowser(500).ProcessRef), "Exited", berthv1.SessionState_SESSION_STATE_STOPPED)
	if gone.GetEventId() <= last.GetEventId() {
		t.Fatalf("event ids out of order: %d then %d", last.GetEventId(), gone.GetEventId())
	}
	resumed.quiet(t)
	// Resuming from the newest id sends nothing until something changes.
	f.watch(t, gone.GetEventId()).quiet(t)
}

func TestWatchSendsASnapshotWhenBehindOrLost(t *testing.T) {
	f := newBrowsersFixture(t)
	f.b.backlog = 2
	last, _ := f.store.LastEventID(t.Context())
	base := slices.Clone(f.rows)
	f.set(t, append(slices.Clone(base), extraBrowser(500), extraBrowser(501), extraBrowser(502))...)
	behind := f.watch(t, last).next(t).GetSnapshot()
	if behind.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND || len(behind.GetSessions()) != 6 {
		t.Fatalf("a client 3 events behind with a backlog of 2 got %v", behind)
	}
	newest, _ := f.store.LastEventID(t.Context())
	if behind.GetEventId() != newest {
		t.Fatalf("snapshot cursor %d, newest event %d", behind.GetEventId(), newest)
	}
	lost := f.watch(t, newest+100).next(t).GetSnapshot()
	if lost.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_UNKNOWN_CURSOR || lost.GetEventId() != newest {
		t.Fatalf("a cursor beyond the newest event got %v", lost)
	}
}

// A client that stops reading must not hold back the actors: they keep saving while its send is
// blocked, and once it reads again it gets one snapshot instead of the backlog.
func TestSlowWatcherDoesNotBlockTheActors(t *testing.T) {
	f := newBrowsersFixture(t)
	f.b.backlog = 10
	release := make(chan struct{})
	got := make(chan *berthv1.WatchSessionsResponse, 64)
	go func() {
		_ = f.b.WatchSessions(t.Context(), 0, func(m *berthv1.WatchSessionsResponse) error {
			got <- m
			if m.GetChange() != nil {
				<-release
			}
			return nil
		})
	}()
	if (<-got).GetSnapshot() == nil {
		t.Fatal("first message is not a snapshot")
	}
	before := len(f.store.Events())
	base := slices.Clone(f.rows)
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for i := range 50 {
			f.set(t, append(slices.Clone(base), extraBrowser(1000+i))...)
			f.set(t, base...)
		}
	}()
	select {
	case <-churned:
	case <-time.After(10 * time.Second):
		t.Fatal("the actors stalled behind a client that stopped reading")
	}
	if saved := len(f.store.Events()) - before; saved != 100 {
		t.Fatalf("%d transitions saved while the client was stuck, want 100 (Seen and Exited for 50 browsers)", saved)
	}
	close(release)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-got:
			if snap := m.GetSnapshot(); snap != nil {
				if snap.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND || len(snap.GetSessions()) != 3 {
					t.Fatalf("snapshot after the backlog %v", snap)
				}
				return
			}
		case <-deadline:
			t.Fatal("no BEHIND snapshot after the client read again")
		}
	}
}

func TestWatchEndsWhenServingStops(t *testing.T) {
	f := newBrowsersFixture(t)
	s := f.watch(t, 0)
	s.next(t)
	f.b.closeWatches()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean end", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after berthd stopped serving")
	}
}

func TestWatchStreamsAreBounded(t *testing.T) {
	f := newBrowsersFixture(t)
	for range maxWatchers {
		f.watch(t, 0).next(t)
	}
	err := f.b.WatchSessions(t.Context(), 0, func(*berthv1.WatchSessionsResponse) error { return nil })
	if !errors.Is(err, api.ErrTooManyWatchers) {
		t.Fatalf("stream %d: %v, want ErrTooManyWatchers", maxWatchers+1, err)
	}
}

type refusingLister struct{}

func (refusingLister) EntitiesOfKind(context.Context, string) ([]ports.EntityState, error) {
	return nil, errors.New("a snapshot read every session ever seen")
}

// A snapshot reads the rows of live sessions only, so its cost does not grow with history.
func TestSnapshotReadsOnlyLiveSessions(t *testing.T) {
	f := newBrowsersFixture(t)
	f.b.List = refusingLister{}
	if snap := f.watch(t, 0).next(t).GetSnapshot(); len(snap.GetSessions()) != 3 {
		t.Fatalf("snapshot %v", snap)
	}
}

// A browser on a Berth profile that no managed browser holds was left by a previous berthd; boot
// stops it, and only it, and a profile a scanned browser uses counts as in use.
func TestBrowsersOnBerthProfilesAreFoundAndStopped(t *testing.T) {
	f := newBrowsersFixture(t)
	const root = "/Users/u/Library/Application Support/Berth/profiles"
	f.set(t, append(slices.Clone(f.rows),
		row(400, 1, testUID, shellPath, shellPath, "--remote-debugging-pipe", "--user-data-dir="+root+"/work"),
		row(500, 1, testUID, shellPath, shellPath, "--remote-debugging-pipe", "--user-data-dir="+root+"-not/work"),
	)...)
	if !f.b.ProfileInUse(root+"/work") || f.b.ProfileInUse(root+"/other") {
		t.Fatal("ProfileInUse must name exactly the profile a scanned browser uses")
	}
	// The same folder written another way is the same profile.
	for _, same := range []string{root + "/work/", root + "/./work", root + "/x/../work"} {
		if !f.b.ProfileInUse(same) {
			t.Fatalf("ProfileInUse(%q) must match %s/work", same, root)
		}
	}
	if n := f.b.StopUnder(t.Context(), root); n != 1 {
		t.Fatalf("StopUnder asked %d browsers to stop, want 1", n)
	}
	if got := f.procs.Signals(); !slices.Equal(got, []testutil.SentSignal{{PID: 400, Signal: ports.SignalTerm}}) {
		t.Fatalf("signals %v, want SIGTERM to 400 only", got)
	}
}

// A profile reached through a symlink (/var is /private/var on macOS) is the same profile.
func TestProfileInUseResolvesSymlinks(t *testing.T) {
	f := newBrowsersFixture(t)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || real == dir {
		t.Skipf("the temporary folder %s has no symlink in its path", tmp)
	}
	f.set(t, append(slices.Clone(f.rows), row(400, 1, testUID, shellPath, shellPath, "--remote-debugging-pipe", "--user-data-dir="+dir))...)
	if !f.b.ProfileInUse(real) {
		t.Fatalf("ProfileInUse(%s) must match a browser using %s", real, dir)
	}
}

// A browser with an invalid UTF-8 profile path must not break the listing for everyone.
func TestInvalidUTF8DoesNotBreakListing(t *testing.T) {
	f := newBrowsersFixture(t)
	f.set(t, append(slices.Clone(f.rows), row(120, 80, testUID, shellPath, shellPath, "--headless", "--user-data-dir=/tmp/\xff"))...)
	resp, err := f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.Marshal(resp); err != nil {
		t.Fatalf("ListSessions cannot be sent: %v", err)
	}
	s := f.watch(t, 0)
	if _, err := proto.Marshal(s.next(t)); err != nil {
		t.Fatalf("the snapshot cannot be sent: %v", err)
	}
}

// A live browser whose file was replaced stays live, with its last path marked changed.
func TestReplacedBrowserStaysLive(t *testing.T) {
	f := newBrowsersFixture(t)
	rows := slices.Clone(f.rows)
	rows[3].Executable = ""
	f.set(t, rows...)
	if st, _ := f.b.state(t.Context(), session.Entity(f.id(100))); st == session.Stopped {
		t.Fatal("a browser whose file was replaced was recorded Stopped")
	}
	got, err := f.b.InspectSession(t.Context(), &berthv1.InspectSessionRequest{Id: f.id(100)})
	if err != nil || got.GetSession().GetBrowser().GetExecutable() != shellPath || !got.GetSession().GetBrowser().GetExecutableChanged() {
		t.Fatalf("inspect: %v %v", got.GetSession().GetBrowser(), err)
	}
}

// A session is published with its facts, so a stop that arrives before the scan applies its
// first observation still refuses another user's browser.
func TestANewSessionIsPublishedWithItsFacts(t *testing.T) {
	f := newBrowsersFixture(t)
	theirs := row(310, 1, 502, shellPath)
	theirs.Opaque = true
	f.procs.Set(append(slices.Clone(f.rows), theirs)...)
	seen, err := discovery.NewScanner(f.procs, testUID).Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(seen, func(o discovery.Observed) bool { return o.Root.PID == 310 })
	if _, err := f.b.track(t.Context(), seen[i]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.StopSession(t.Context(), &berthv1.StopSessionRequest{Id: seen[i].ID, Force: true}); !errors.Is(err, api.ErrSessionNotStoppable) {
		t.Fatalf("stop of another user's browser before its first observation: %v", err)
	}
}

// Two steps can return in either order; the kept state is the later one's.
func TestAnOlderStepDoesNotOverwriteALaterState(t *testing.T) {
	f := newBrowsersFixture(t)
	id := f.id(100)
	f.b.recorded(id, ports.TransitionRecord{Version: 5, To: string(session.Stopping), At: bootTime})
	f.b.recorded(id, ports.TransitionRecord{Version: 4, To: string(session.Active), At: bootTime})
	f.b.mu.Lock()
	st := f.b.live[id].disc.State
	f.b.mu.Unlock()
	if st != session.Stopping {
		t.Fatalf("kept state %s, want Stopping from the later step", st)
	}
}

// A session saved as Stopping when berthd went down gets its SIGTERM and grace timer again at
// boot, so it ends in SIGKILL rather than staying Stopping forever.
func TestBootRearmsAStoppingSession(t *testing.T) {
	f := newBrowsersFixture(t)
	entity := session.Entity(f.id(100))
	st, _, err := f.store.LoadState(t.Context(), entity)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveTransition(t.Context(), ports.TransitionRecord{
		Entity: entity, Kind: session.Kind, Version: st.Version + 1, Event: "StopRequested", From: st.State, To: string(session.Stopping),
	}); err != nil {
		t.Fatal(err)
	}
	b := newBrowsers(t.Context(), f.b.browsersDeps)
	b.grace = 20 * time.Millisecond
	if err := b.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := waitSignals(t, f.procs, 3)
	want := []testutil.SentSignal{{PID: 100, Signal: ports.SignalTerm}, {PID: 100, Signal: ports.SignalKill}, {PID: 101, Signal: ports.SignalKill}}
	if !slices.Equal(got, want) {
		t.Fatalf("signals %v, want %v", got, want)
	}
}

// Sessions whose browsers started at the same moment come back in one order every time.
func TestListOrderIsStableForEqualStartTimes(t *testing.T) {
	var sessions []*berthv1.Session
	for _, id := range []string{"9-1", "3-1", "7-1", "1-1", "5-1", "8-1", "2-1", "6-1", "4-1"} {
		sessions = append(sessions, &berthv1.Session{Id: id})
	}
	sortSessions(sessions)
	for i := 1; i < len(sessions); i++ {
		if sessions[i-1].GetId() > sessions[i].GetId() {
			t.Fatalf("order %v", sessions)
		}
	}
}

// Resolving a profile path a process chose can block on a hung mount; it must not hold the lock
// every listing needs.
func TestProfileResolutionDoesNotHoldTheLock(t *testing.T) {
	f := newBrowsersFixture(t)
	f.set(t, append(slices.Clone(f.rows), row(130, 80, testUID, shellPath, shellPath, "--headless", "--user-data-dir=/Volumes/hung/profile"))...)
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	resolve = func(p string) (string, error) {
		if strings.HasPrefix(p, "/Volumes/hung") {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return p, nil
	}
	t.Cleanup(func() { resolve = filepath.EvalSymlinks })
	done := make(chan bool, 1)
	go func() { done <- f.b.ProfileInUse("/tmp/other") }()
	<-entered
	listed := make(chan struct{})
	go func() {
		_, _ = f.b.ListSessions(t.Context(), &berthv1.ListSessionsRequest{})
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Error("ListSessions waited on a profile path being resolved")
	}
	close(release)
	<-done
	<-listed
}
