package berthd

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/discovery"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// holdingApprover keeps every store dialog open until release closes.
type holdingApprover struct {
	*testutil.Approver
	release chan struct{}
}

func (a holdingApprover) StoreSecret(ctx context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	select {
	case <-a.release:
	case <-ctx.Done():
		return ports.StoreFailed, ctx.Err()
	}
	return a.Approver.StoreSecret(ctx, p)
}

type needsFixture struct {
	*fixture
	b     *browsers
	procs *testutil.Processes
	n     *needs
}

// newNeedsFixture wires secrets, browsers and needs on one store, as berthd does.
func newNeedsFixture(t *testing.T, approver ports.Approver) *needsFixture {
	t.Helper()
	f := &needsFixture{fixture: newFixture(t, approver), procs: &testutil.Processes{}}
	streams := newWatchSet()
	f.s.Changed = streams.wakeAll
	f.procs.Set(fixtureProcesses()...)
	f.b = newBrowsers(t.Context(), browsersDeps{
		Log: slog.New(slog.DiscardHandler), Source: f.procs, Store: f.store, List: f.store, Events: f.store,
		Clock: testutil.NewClock(), UID: testUID, Changed: streams.wakeAll,
	})
	if err := f.b.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.n = newNeeds(needsDeps{
		Events: f.store, List: f.store, Grant: f.store.Grant, Leftovers: f.b.leftovers, Leftover: f.b.leftoverNeed, Streams: streams,
	})
	return f
}

type needStream struct {
	msgs   chan *berthv1.WatchNeedsResponse
	done   chan error
	cancel context.CancelFunc
}

func (f *needsFixture) watch(t *testing.T, after int64) *needStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &needStream{msgs: make(chan *berthv1.WatchNeedsResponse, 1024), done: make(chan error, 1), cancel: cancel}
	go func() {
		s.done <- f.n.WatchNeeds(ctx, after, func(m *berthv1.WatchNeedsResponse) error {
			s.msgs <- m
			return nil
		})
	}()
	t.Cleanup(cancel)
	return s
}

func (s *needStream) next(t *testing.T) *berthv1.WatchNeedsResponse {
	t.Helper()
	select {
	case m := <-s.msgs:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	return nil
}

// change waits for the next change and checks it.
func (s *needStream) change(t *testing.T, event berthv1.NeedEvent, want *berthv1.Need) *berthv1.NeedChange {
	t.Helper()
	c := s.next(t).GetChange()
	if c.GetEvent() != event || c.GetNeed().GetId() != want.GetId() || c.GetNeed().GetKind() != want.GetKind() ||
		c.GetNeed().GetSecretName() != want.GetSecretName() || c.GetNeed().GetOwnerName() != want.GetOwnerName() {
		t.Fatalf("change %v, want %v %v", c, event, want)
	}
	return c
}

func (s *needStream) quiet(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.msgs:
		t.Fatalf("unexpected message %v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func leftoverOf(pid int, owner string) *berthv1.Need {
	for _, r := range fixtureProcesses() {
		if r.PID == pid {
			return &berthv1.Need{Id: "session/" + discovery.ID(r.ProcessRef), Kind: berthv1.NeedKind_NEED_KIND_LEFTOVER, OwnerName: owner}
		}
	}
	return nil
}

var (
	storeNeed = &berthv1.Need{Id: "secret/gh-token", Kind: berthv1.NeedKind_NEED_KIND_STORE, SecretName: "gh-token"}
	grantNeed = &berthv1.Need{Id: "grant/1", Kind: berthv1.NeedKind_NEED_KIND_GRANT, SecretName: "gh-token"}
)

// A store dialog, a grant waiting for Touch ID and a leftover browser each open a need and
// resolve it, and every other transition sends nothing.
func TestNeedsOpenAndResolve(t *testing.T) {
	f := newNeedsFixture(t, &testutil.Approver{})
	s := f.watch(t, 0)
	snap := s.next(t).GetSnapshot()
	theirs := leftoverOf(300, "")
	if snap.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_START || len(snap.GetNeeds()) != 1 || snap.GetNeeds()[0].GetId() != theirs.GetId() {
		t.Fatalf("snapshot %v, want only the other user's leftover browser", snap)
	}

	f.request(t)
	opened := s.change(t, berthv1.NeedEvent_NEED_EVENT_OPENED, storeNeed)
	if opened.GetEventId() <= snap.GetEventId() {
		t.Fatalf("change %d not after the snapshot's %d", opened.GetEventId(), snap.GetEventId())
	}
	s.change(t, berthv1.NeedEvent_NEED_EVENT_RESOLVED, storeNeed)
	f.grant(t, "")
	s.change(t, berthv1.NeedEvent_NEED_EVENT_OPENED, grantNeed)
	s.change(t, berthv1.NeedEvent_NEED_EVENT_RESOLVED, grantNeed)
	// A use is a transition of the grant that waits on no one.
	f.use(t, "gh", "pr", "list")
	s.quiet(t)

	// The terminal and node exit, so the Playwright browser becomes a leftover; the other
	// user's browser goes away.
	var rows []testutil.FakeProcess
	for _, r := range fixtureProcesses() {
		if !slices.Contains([]int{30, 80, 300}, r.PID) {
			rows = append(rows, r)
		}
	}
	f.procs.Set(rows...)
	if err := f.b.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := []*berthv1.NeedChange{s.next(t).GetChange(), s.next(t).GetChange()}
	slices.SortFunc(got, func(a, b *berthv1.NeedChange) int { return int(a.GetEvent()) - int(b.GetEvent()) })
	ours := leftoverOf(100, "")
	if got[0].GetEvent() != berthv1.NeedEvent_NEED_EVENT_OPENED || got[0].GetNeed().GetId() != ours.GetId() ||
		got[0].GetNeed().GetKind() != berthv1.NeedKind_NEED_KIND_LEFTOVER || got[0].GetNeed().GetOwnerName() != ours.GetOwnerName() ||
		got[0].GetNeed().GetBrowser() != berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL {
		t.Fatalf("new leftover %v, want %v", got[0], ours)
	}
	if got[1].GetEvent() != berthv1.NeedEvent_NEED_EVENT_RESOLVED || got[1].GetNeed().GetId() != theirs.GetId() {
		t.Fatalf("gone leftover %v, want %v resolved", got[1], theirs)
	}
	s.quiet(t)
}

// An open dialog is in the snapshot; a client that reconnects after it gets the resolution only.
func TestNeedsSnapshotShowsOpenDialogsAndResumes(t *testing.T) {
	approver := holdingApprover{Approver: &testutil.Approver{}, release: make(chan struct{})}
	f := newNeedsFixture(t, approver)
	early := f.watch(t, 0)
	early.next(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.request(t)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st, found, _ := f.store.LoadState(t.Context(), "secret/gh-token"); found && st.State == "Pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the store dialog never opened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The need opens while the dialog still waits for the person.
	early.change(t, berthv1.NeedEvent_NEED_EVENT_OPENED, storeNeed)
	first := f.watch(t, 0)
	snap := first.next(t).GetSnapshot()
	ids := make([]string, 0, len(snap.GetNeeds()))
	for _, n := range snap.GetNeeds() {
		ids = append(ids, n.GetId())
		if n.GetId() == storeNeed.GetId() && n.GetSecretName() != "gh-token" {
			t.Fatalf("store need %v", n)
		}
	}
	if !slices.Equal(ids, []string{storeNeed.GetId(), leftoverOf(300, "").GetId()}) {
		t.Fatalf("snapshot needs %v", ids)
	}
	first.cancel()
	if err := <-first.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled stream returned %v", err)
	}

	close(approver.release)
	<-done
	resumed := f.watch(t, snap.GetEventId())
	resumed.change(t, berthv1.NeedEvent_NEED_EVENT_RESOLVED, storeNeed)
	resumed.quiet(t)

	last, _ := f.store.LastEventID(t.Context())
	lost := f.watch(t, last+100).next(t).GetSnapshot()
	if lost.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_UNKNOWN_CURSOR || len(lost.GetNeeds()) != 1 {
		t.Fatalf("a cursor beyond the newest event got %v", lost)
	}
}

// A client further behind than the backlog gets a snapshot, counting the rows of every kind.
func TestNeedsSendASnapshotWhenBehind(t *testing.T) {
	f := newNeedsFixture(t, &testutil.Approver{})
	f.n.backlog = 2
	last, _ := f.store.LastEventID(t.Context())
	f.request(t)
	f.grant(t, "")
	behind := f.watch(t, last).next(t).GetSnapshot()
	if behind.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND || len(behind.GetNeeds()) != 1 {
		t.Fatalf("a client 4 events behind with a backlog of 2 got %v", behind)
	}
}

func TestNeedStreamsAreBoundedAndEndWithServing(t *testing.T) {
	f := newNeedsFixture(t, &testutil.Approver{})
	streams := make([]*needStream, 0, maxWatchers)
	for range maxWatchers {
		s := f.watch(t, 0)
		s.next(t)
		streams = append(streams, s)
	}
	err := f.n.WatchNeeds(t.Context(), 0, func(*berthv1.WatchNeedsResponse) error { return nil })
	if !errors.Is(err, api.ErrTooManyWatchers) {
		t.Fatalf("stream %d: %v, want ErrTooManyWatchers", maxWatchers+1, err)
	}
	f.n.closeWatches()
	for _, s := range streams {
		select {
		case err := <-s.done:
			if err != nil {
				t.Fatalf("stream ended with %v, want a clean end", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stream still open after berthd stopped serving")
		}
	}
}

func TestOnlyPromptsAndLeftoversAreNeeds(t *testing.T) {
	for _, tc := range []struct {
		rec  ports.EventRecord
		want berthv1.NeedKind
	}{
		{rec(1, "secret", "StoreRequested", "Pending", "Pending"), berthv1.NeedKind_NEED_KIND_STORE},
		{rec(2, "secret", "StoreRequested", "Failed", "Pending"), berthv1.NeedKind_NEED_KIND_STORE},
		{rec(3, "secret", "RotateRequested", "Stored", "Rotating"), berthv1.NeedKind_NEED_KIND_ROTATE},
		{rec(4, "secret", "RemoveRequested", "Stored", "Removing"), berthv1.NeedKind_NEED_KIND_REMOVE},
		{rec(5, "grant", "GrantRequested", "Requested", "Requested"), berthv1.NeedKind_NEED_KIND_GRANT},
		{rec(6, "session", "OwnerExited", "Active", "Leftover"), berthv1.NeedKind_NEED_KIND_LEFTOVER},
		{rec(7, "session", "Busy", "Leftover", "Leftover"), berthv1.NeedKind_NEED_KIND_UNSPECIFIED},
		{rec(8, "secret", "UseRefused", "Stored", "Stored"), berthv1.NeedKind_NEED_KIND_UNSPECIFIED},
		{rec(9, "proxysession", "Started", "Authorized", "Active"), berthv1.NeedKind_NEED_KIND_UNSPECIFIED},
	} {
		if got := opens(tc.rec); got != tc.want {
			t.Errorf("%s %s opens %v, want %v", tc.rec.Kind, tc.rec.Event, got, tc.want)
		}
	}
}

func rec(id int64, kind, event, from, to string) ports.EventRecord {
	return ports.EventRecord{ID: id, TransitionRecord: ports.TransitionRecord{Entity: kind + "/x", Kind: kind, Event: event, From: from, To: to}}
}
