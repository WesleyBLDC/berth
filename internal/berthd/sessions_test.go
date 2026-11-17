package berthd

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

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

func newBrowsersFixture(t *testing.T) *browsersFixture {
	t.Helper()
	launchd := row(1, 0, 0, "/sbin/launchd")
	launchd.Opaque = true
	theirs := row(300, 1, 502, shellPath)
	theirs.Opaque = true
	f := &browsersFixture{procs: &testutil.Processes{}, store: testutil.NewStore(), rows: []testutil.FakeProcess{
		launchd,
		row(30, 1, testUID, "/Applications/Ghostty.app/Contents/MacOS/ghostty"),
		row(80, 30, testUID, "/opt/homebrew/bin/node", "node", "playwright", "test"),
		row(100, 80, testUID, shellPath, shellPath, "--headless", "--remote-debugging-port=0"),
		row(101, 100, testUID, shellPath, shellPath, "--type=renderer"),
		row(200, 1, testUID, chromeApp, chromeApp),
		theirs,
	}}
	f.procs.Set(f.rows...)
	f.b = newBrowsers(t.Context(), browsersDeps{
		Log: slog.New(slog.DiscardHandler), Source: f.procs, Store: f.store, List: f.store, Clock: testutil.NewClock(), UID: testUID,
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
	deadline := time.Now().Add(2 * time.Second)
	for len(f.procs.Signals()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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
}
