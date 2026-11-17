package berthd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/browsersig"
	"berth/internal/core/discovery"
	"berth/internal/core/machine"
	"berth/internal/core/ownerchain"
	"berth/internal/core/ports"
	"berth/internal/core/session"
)

const (
	scanInterval = 2 * time.Second
	// stopGrace is how long a browser has after SIGTERM before every process of it gets SIGKILL.
	stopGrace = 5 * time.Second
	// stopWait bounds how long StopSession waits for the browser to be gone.
	stopWait = 10 * time.Second
	// endedMax and endedAge bound what berthd keeps in memory about stopped sessions, whichever is
	// smaller, since anything that can start processes can churn through browsers.
	endedMax = 200
	endedAge = time.Hour
	// watchBacklog is how many session events a stream replays; a client further behind gets a
	// snapshot instead.
	watchBacklog = 1000
	// maxWatchers bounds the open session streams, since each holds a goroutine.
	maxWatchers = 16
)

type browsersDeps struct {
	Log    *slog.Logger
	Source ports.ProcessSource
	Store  ports.Store
	List   ports.EntityLister
	Events ports.EventLog
	Clock  ports.Clock
	UID    uint32
}

// browsers runs discovery: a scan every scanInterval, and at once when a watched browser exits,
// turning what it sees into session events. Facts about each session live here in memory and
// are rebuilt by every scan; only the machine's rows persist.
type browsers struct {
	browsersDeps
	ctx    context.Context
	actors *actors[session.State, session.Event, session.Effect]
	// scanner is used only under scanning.
	scanner *discovery.Scanner
	kick    chan struct{}

	mu sync.Mutex
	// live holds every session the last scan saw or that has not yet reached Stopped.
	live map[string]*tracked
	// ended holds recently stopped sessions, at most endedMax and none older than endedAge, for
	// ListSessions include_stopped; endedOrder is their ids, oldest first.
	ended      map[string]*tracked
	endedOrder []string
	// grace is stopGrace, shorter in tests.
	grace time.Duration
	// scanning admits one scan at a time.
	scanning sync.Mutex

	// watchers are the open session streams; a saved session transition wakes each one, and
	// closing ends them all when berthd stops serving.
	watchMu   sync.Mutex
	watchers  map[*sessionStream]struct{}
	closing   chan struct{}
	closeOnce sync.Once
	// backlog is watchBacklog, smaller in tests.
	backlog int
}

// sessionStream is one open WatchSessions stream. wake holds at most one pending wake, so an
// actor never waits.
type sessionStream struct {
	wake chan struct{}
}

type tracked struct {
	disc  discovery.Tracked
	obs   discovery.Observed
	since time.Time
	// version is the machine version disc.State and since come from, so a step that finishes
	// after a later one cannot put an older state back.
	version int64
}

func newBrowsers(ctx context.Context, d browsersDeps) *browsers {
	b := &browsers{
		browsersDeps: d, ctx: ctx, kick: make(chan struct{}, 1), grace: stopGrace,
		live: map[string]*tracked{}, ended: map[string]*tracked{}, scanner: discovery.NewScanner(d.Source, d.UID),
		watchers: map[*sessionStream]struct{}{}, closing: make(chan struct{}), backlog: watchBacklog,
	}
	b.actors = &actors[session.State, session.Event, session.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: session.Kind, initial: session.Initial, final: []session.State{session.Stopped}, transition: session.Transition, run: b.runSession,
		live:  map[string]*slot[session.State, session.Event, session.Effect]{},
		saved: func(ports.TransitionRecord) { b.wakeWatchers() },
	}
	return b
}

// recover scans once, then ends every session the database holds in a live state that the scan
// did not find, so a browser that exited while berthd was down is Stopped.
func (b *browsers) recover(ctx context.Context) error {
	if err := b.scan(ctx); err != nil {
		return err
	}
	rows, err := b.List.EntitiesOfKind(ctx, session.Kind)
	if err != nil {
		return err
	}
	b.mu.Lock()
	var gone []string
	for _, r := range rows {
		if session.State(r.State) == session.Stopped {
			continue
		}
		if _, seen := b.live[r.Entity[len(session.Entity("")):]]; !seen {
			gone = append(gone, r.Entity)
		}
	}
	b.mu.Unlock()
	for _, entity := range gone {
		if _, _, err := step(ctx, b.actors, entity, session.Event{Kind: session.Exited, Note: "gone at boot"}, nil, session.Exited); err != nil {
			return err
		}
	}
	return nil
}

// run scans until ctx ends.
func (b *browsers) run(ctx context.Context) {
	tick := time.NewTicker(scanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-b.kick:
		}
		if err := b.scan(ctx); err != nil && ctx.Err() == nil {
			b.Log.Warn("browser scan failed", "err", err)
		}
	}
}

func (b *browsers) rescan() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

func (b *browsers) scan(ctx context.Context) error {
	b.scanning.Lock()
	defer b.scanning.Unlock()
	seen, err := b.scanner.Scan(ctx)
	if err != nil {
		return err
	}
	now := b.Clock.Now()
	observed := make(map[string]bool, len(seen))
	for i := range seen {
		o := seen[i]
		observed[o.ID] = true
		t, err := b.track(ctx, o)
		if err != nil {
			return err
		}
		b.apply(ctx, o.ID, t, &o, now)
	}
	b.mu.Lock()
	var gone []string
	for id := range b.live {
		if !observed[id] {
			gone = append(gone, id)
		}
	}
	b.mu.Unlock()
	for _, id := range gone {
		b.mu.Lock()
		t := b.live[id]
		b.mu.Unlock()
		b.apply(ctx, id, t, nil, now)
	}
	return nil
}

// track returns the session's facts, creating them, and its actor and exit watch, on first sight.
func (b *browsers) track(ctx context.Context, o discovery.Observed) (*tracked, error) {
	b.mu.Lock()
	t, ok := b.live[o.ID]
	b.mu.Unlock()
	if ok {
		return t, nil
	}
	entity := session.Entity(o.ID)
	saved, found, err := b.Store.LoadState(ctx, entity)
	if err != nil {
		return nil, err
	}
	st := session.Initial
	if found {
		st = session.State(saved.State)
	}
	// The facts are set before the session is published, so a stop in between still sees whose
	// browser it is.
	t = &tracked{disc: discovery.Tracked{State: st, Unsaved: !found}, obs: o, since: b.Clock.Now(), version: saved.Version}
	if hist, err := b.Store.History(ctx, entity, 1); err == nil && len(hist) == 1 {
		t.since = hist[0].At
	}
	b.mu.Lock()
	b.live[o.ID] = t
	b.mu.Unlock()
	if exited, err := b.Source.WatchExit(ctx, o.Root.ProcessRef); err == nil {
		go func() {
			select {
			case <-exited:
				b.rescan()
			case <-ctx.Done():
			}
		}()
	}
	return t, nil
}

// state reads the persisted state, since only the actor's goroutine may read its own.
func (b *browsers) state(ctx context.Context, entity string) (session.State, error) {
	st, found, err := b.Store.LoadState(ctx, entity)
	if err != nil || !found {
		return session.Initial, err
	}
	return session.State(st.State), nil
}

// apply runs one session's pure step and sends what it returns; o is nil when the browser is gone.
func (b *browsers) apply(ctx context.Context, id string, t *tracked, o *discovery.Observed, now time.Time) {
	b.mu.Lock()
	next, events := discovery.Events(t.disc, o, b.scanner.Running, now)
	t.disc = next
	if o != nil {
		t.obs = *o
	}
	if o == nil && t.disc.State == session.Stopped {
		b.end(id, t)
	}
	b.mu.Unlock()
	for _, e := range events {
		b.send(ctx, id, e)
	}
}

// send applies one event and records the state it reached; an illegal event (a stop raced the
// scan) changes nothing.
func (b *browsers) send(ctx context.Context, id string, e session.Event) {
	rec, _, err := step(ctx, b.actors, session.Entity(id), e, nil, e.Kind)
	if err != nil {
		b.Log.Debug("session event not applied", "session", id, "event", e.Kind, "err", err)
		return
	}
	b.recorded(id, rec)
}

// recorded keeps the state a step reached, unless a later step's state is already kept: two
// steps on one session finish one after the other, but their callers can get here in any order.
func (b *browsers) recorded(id string, rec ports.TransitionRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.live[id]
	if !ok || rec.Version <= t.version {
		return
	}
	t.disc.State, t.since, t.version = session.State(rec.To), rec.At, rec.Version
	if t.disc.State == session.Stopped {
		b.end(id, t)
	}
}

// end moves a stopped session out of live and trims ended; its actor stopped itself. b.mu is held.
func (b *browsers) end(id string, t *tracked) {
	delete(b.live, id)
	b.ended[id] = t
	b.endedOrder = append(b.endedOrder, id)
	b.trimEnded()
}

// trimEnded drops stopped sessions beyond endedMax or older than endedAge. b.mu is held.
func (b *browsers) trimEnded() {
	cutoff := b.Clock.Now().Add(-endedAge)
	drop := 0
	for drop < len(b.endedOrder) {
		t := b.ended[b.endedOrder[drop]]
		if len(b.endedOrder)-drop <= endedMax && (t == nil || !t.since.Before(cutoff)) {
			break
		}
		delete(b.ended, b.endedOrder[drop])
		drop++
	}
	b.endedOrder = slices.Delete(b.endedOrder, 0, drop)
}

func (b *browsers) runSession(entity string) machine.Runner[session.Event, session.Effect] {
	id := entity[len(session.Entity("")):]
	return func(ctx context.Context, effect session.Effect) ([]session.Event, error) {
		b.mu.Lock()
		t, ok := b.live[id]
		var o discovery.Observed
		if ok {
			o = t.obs
		}
		b.mu.Unlock()
		if !ok {
			return nil, nil
		}
		switch effect.Kind {
		case session.SignalTerm:
			b.signal(ctx, o.Root.ProcessRef, ports.SignalTerm)
			time.AfterFunc(b.grace, func() {
				if st, err := b.state(b.ctx, entity); err == nil && st == session.Stopping {
					b.send(b.ctx, id, session.Event{Kind: session.StopTimedOut})
				}
			})
		case session.SignalKill:
			for _, m := range o.Members {
				if m.UID == b.UID {
					b.signal(ctx, m.ProcessRef, ports.SignalKill)
				}
			}
		}
		b.rescan()
		return nil, nil
	}
}

// signal ignores a process already gone: the scan or its exit watch reports the exit.
func (b *browsers) signal(ctx context.Context, p ports.ProcessRef, sig ports.Signal) {
	if err := b.Source.Signal(ctx, p, sig); err != nil && !errors.Is(err, ports.ErrProcessGone) {
		b.Log.Warn("could not signal a browser process", "pid", p.PID, "signal", sig, "err", err)
	}
}

// StopSession refuses what it must not stop, sends StopRequested and waits up to stopWait.
func (b *browsers) StopSession(ctx context.Context, req *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error) {
	id := req.GetId()
	b.mu.Lock()
	t, live := b.live[id]
	_, ended := b.ended[id]
	var o discovery.Observed
	var st session.State
	if live {
		o, st = t.obs, t.disc.State
	}
	b.mu.Unlock()
	if !live {
		if ended {
			return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
		}
		// A session trimmed from memory is still Stopped in the database.
		if st, found, err := b.Store.LoadState(ctx, session.Entity(id)); err == nil && found && session.State(st.State) == session.Stopped {
			return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
		}
		return nil, api.ErrSessionNotFound
	}
	switch {
	case o.OtherUser:
		return nil, errors.Join(api.ErrSessionNotStoppable, errors.New("this browser belongs to another user"))
	case (o.Owner.Kind == ownerchain.Person || o.Owner.Kind == ownerchain.App) && !req.GetForce():
		return nil, errors.Join(api.ErrSessionNotStoppable, errors.New("this browser belongs to the person or an app; pass force to stop it"))
	}
	if st != session.Stopping {
		// Illegal only when another stop or the exit got there first; the wait below sees either.
		b.send(ctx, id, session.Event{Kind: session.StopRequested})
	}
	deadline := time.NewTimer(stopWait)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		b.mu.Lock()
		_, done := b.ended[id]
		state := session.Stopping
		if t, ok := b.live[id]; ok {
			state = t.disc.State
		}
		b.mu.Unlock()
		if done {
			return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			return &berthv1.StopSessionResponse{State: stateProto(state)}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (b *browsers) ListSessions(_ context.Context, req *berthv1.ListSessionsRequest) (*berthv1.ListSessionsResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trimEnded()
	resp := &berthv1.ListSessionsResponse{}
	add := func(t *tracked) {
		s := sessionProto(t)
		if len(req.GetStates()) == 0 || slices.Contains(req.GetStates(), s.GetState()) {
			resp.Sessions = append(resp.Sessions, s)
		}
	}
	for _, t := range b.live {
		add(t)
	}
	if req.GetIncludeStopped() {
		for _, t := range b.ended {
			add(t)
		}
	}
	sortSessions(resp.Sessions)
	return resp, nil
}

func sortSessions(sessions []*berthv1.Session) {
	slices.SortFunc(sessions, func(x, y *berthv1.Session) int {
		return x.GetBrowser().GetStartedAt().AsTime().Compare(y.GetBrowser().GetStartedAt().AsTime())
	})
}

// WatchSessions streams session changes from the event table, which is the only queue: an
// actor saving a transition only wakes the watchers, and each reads the rows after its own
// cursor at its client's pace, so a slow client holds one goroutine and one page of rows, never
// an actor.
func (b *browsers) WatchSessions(ctx context.Context, after int64, send func(*berthv1.WatchSessionsResponse) error) error {
	w, err := b.watch()
	if err != nil {
		return err
	}
	defer b.unwatch(w)
	cursor, reason := after, berthv1.SnapshotReason_SNAPSHOT_REASON_UNSPECIFIED
	if after == 0 {
		reason = berthv1.SnapshotReason_SNAPSHOT_REASON_START
	} else if last, err := b.Events.LastEventID(ctx); err != nil {
		return err
	} else if after > last {
		reason = berthv1.SnapshotReason_SNAPSHOT_REASON_UNKNOWN_CURSOR
	}
	if reason != berthv1.SnapshotReason_SNAPSHOT_REASON_UNSPECIFIED {
		if cursor, err = b.snapshot(ctx, reason, send); err != nil {
			return err
		}
	}
	for {
		if cursor, err = b.catchUp(ctx, cursor, send); err != nil {
			return err
		}
		select {
		case <-w.wake:
		case <-b.closing:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// catchUp sends every session event after cursor, or a snapshot when there are more than the
// backlog, and returns the new cursor.
func (b *browsers) catchUp(ctx context.Context, cursor int64, send func(*berthv1.WatchSessionsResponse) error) (int64, error) {
	for {
		select {
		case <-b.closing:
			return cursor, nil
		default:
		}
		rows, err := b.Events.EventsAfter(ctx, session.Kind, cursor, b.backlog+1)
		if err != nil {
			return cursor, err
		}
		if len(rows) > b.backlog {
			if cursor, err = b.snapshot(ctx, berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND, send); err != nil {
				return cursor, err
			}
			continue
		}
		if len(rows) == 0 {
			return cursor, nil
		}
		for _, r := range rows {
			if err := send(b.change(r)); err != nil {
				return cursor, err
			}
			cursor = r.ID
		}
	}
}

// snapshot sends every live session and returns the cursor it is consistent with. The cursor is
// read before the states, so no state is older than it; a change after it that the snapshot
// already shows is applied again, which is harmless since a change carries the whole state. Only
// the live sessions' rows are read, so a snapshot costs the same however long the history.
func (b *browsers) snapshot(ctx context.Context, reason berthv1.SnapshotReason, send func(*berthv1.WatchSessionsResponse) error) (int64, error) {
	last, err := b.Events.LastEventID(ctx)
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	ids := make([]string, 0, len(b.live))
	for id := range b.live {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	snap := &berthv1.SessionsSnapshot{EventId: last, Reason: reason}
	for _, id := range ids {
		saved, found, err := b.Store.LoadState(ctx, session.Entity(id))
		if err != nil {
			return 0, err
		}
		b.mu.Lock()
		t, ok := b.live[id]
		if !ok {
			t, ok = b.ended[id]
		}
		var s *berthv1.Session
		if ok {
			s = sessionProto(t)
		}
		b.mu.Unlock()
		if s == nil {
			continue
		}
		if found {
			s.State = stateProto(session.State(saved.State))
		}
		snap.Sessions = append(snap.Sessions, s)
	}
	sortSessions(snap.Sessions)
	return last, send(&berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Snapshot{Snapshot: snap}})
}

// change turns one event row into a stream message: the state and its time from the row, the
// other facts from what berthd still holds about the session.
func (b *browsers) change(r ports.EventRecord) *berthv1.WatchSessionsResponse {
	id := strings.TrimPrefix(r.Entity, session.Entity(""))
	b.mu.Lock()
	t, ok := b.live[id]
	if !ok {
		t, ok = b.ended[id]
	}
	s := &berthv1.Session{Id: id}
	if ok {
		s = sessionProto(t)
	}
	b.mu.Unlock()
	s.State, s.StateSince = stateProto(session.State(r.To)), timestamppb.New(r.At)
	return &berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Change{Change: &berthv1.SessionChange{
		EventId: r.ID, Event: r.Event, Session: s,
	}}}
}

func (b *browsers) watch() (*sessionStream, error) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	if len(b.watchers) >= maxWatchers {
		return nil, api.ErrTooManyWatchers
	}
	w := &sessionStream{wake: make(chan struct{}, 1)}
	b.watchers[w] = struct{}{}
	return w, nil
}

func (b *browsers) unwatch(w *sessionStream) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	delete(b.watchers, w)
}

// wakeWatchers runs on an actor's goroutine after each saved session transition, so it never
// blocks: a watcher that has a wake pending already will read this row too.
func (b *browsers) wakeWatchers() {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	for w := range b.watchers {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// closeWatches ends every open stream, and any opened later, when berthd stops serving.
func (b *browsers) closeWatches() {
	b.closeOnce.Do(func() { close(b.closing) })
}

func (b *browsers) InspectSession(ctx context.Context, req *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error) {
	b.mu.Lock()
	t, ok := b.live[req.GetId()]
	if !ok {
		t, ok = b.ended[req.GetId()]
	}
	var resp *berthv1.InspectSessionResponse
	if ok {
		resp = &berthv1.InspectSessionResponse{Session: sessionProto(t)}
		for _, m := range t.obs.Members {
			role := berthv1.ProcessRole_PROCESS_ROLE_BROWSER
			if m.Helper {
				role = berthv1.ProcessRole_PROCESS_ROLE_HELPER
			}
			resp.Processes = append(resp.Processes, processProto(m.ProcessInfo, role))
		}
		for _, p := range t.obs.Chain {
			resp.OwnerChain = append(resp.OwnerChain, processProto(p, berthv1.ProcessRole_PROCESS_ROLE_ANCESTOR))
		}
	}
	b.mu.Unlock()
	if !ok {
		return nil, api.ErrSessionNotFound
	}
	limit := int(req.GetHistoryLimit())
	if limit == 0 {
		limit = 20
	}
	hist, err := b.Store.History(ctx, session.Entity(req.GetId()), limit)
	if err != nil {
		return nil, err
	}
	for _, r := range hist {
		resp.History = append(resp.History, &berthv1.Transition{
			Version: r.Version, Event: r.Event, FromState: r.From, ToState: r.To, At: timestamppb.New(r.At), Detail: r.Detail,
		})
	}
	return resp, nil
}

var _ api.Sessions = (*browsers)(nil)

func sessionProto(t *tracked) *berthv1.Session {
	o, sig := t.obs, t.obs.Signature
	s := &berthv1.Session{
		Id: o.ID, State: stateProto(t.disc.State), Kind: kindProto[sig.Kind], Headless: sig.Headless,
		Browser:      processProto(o.Root, berthv1.ProcessRole_PROCESS_ROLE_BROWSER),
		ProcessCount: uint32(len(o.Members)), //nolint:gosec // a process count fits
		OtherUser:    o.OtherUser, Uid: o.Root.UID, StateSince: timestamppb.New(t.since),
		Owner: &berthv1.SessionOwner{Kind: ownerProto[o.Owner.Kind], Name: o.Owner.Name},
	}
	if o.Owner.Process.PID > 0 {
		s.Owner.Process = processProto(o.Owner.Process, berthv1.ProcessRole_PROCESS_ROLE_ANCESTOR)
	}
	if o.OtherUser || !sig.ArgsKnown {
		s.ArgsUnreadable = !o.OtherUser
		return s
	}
	s.Driver, s.Cdp, s.UserDataDir = driverProto[sig.Driver], cdpProto[sig.CDP], sig.UserDataDir
	if sig.CDP == browsersig.CDPPort {
		s.CdpPort = uint32(sig.CDPPort) //nolint:gosec // Classify keeps ports within 0 to 65535
	}
	return s
}

func processProto(p ports.ProcessInfo, role berthv1.ProcessRole) *berthv1.SessionProcess {
	return &berthv1.SessionProcess{
		Pid: int32(p.PID), StartedAt: timestamppb.New(p.Start), Executable: p.Executable, Role: role, //nolint:gosec // macOS pids fit in 32 bits
		ExecutableChanged: p.ExecutableChanged,
	}
}

func stateProto(s session.State) berthv1.SessionState {
	switch s {
	case session.Discovered:
		return berthv1.SessionState_SESSION_STATE_DISCOVERED
	case session.Active:
		return berthv1.SessionState_SESSION_STATE_ACTIVE
	case session.Idle:
		return berthv1.SessionState_SESSION_STATE_IDLE
	case session.Leftover:
		return berthv1.SessionState_SESSION_STATE_LEFTOVER
	case session.Stopping:
		return berthv1.SessionState_SESSION_STATE_STOPPING
	case session.Stopped:
		return berthv1.SessionState_SESSION_STATE_STOPPED
	}
	return berthv1.SessionState_SESSION_STATE_UNSPECIFIED
}

var (
	kindProto = map[browsersig.Kind]berthv1.BrowserKind{
		browsersig.Chrome: berthv1.BrowserKind_BROWSER_KIND_CHROME, browsersig.ChromeForTesting: berthv1.BrowserKind_BROWSER_KIND_CHROME_FOR_TESTING,
		browsersig.Chromium: berthv1.BrowserKind_BROWSER_KIND_CHROMIUM, browsersig.Edge: berthv1.BrowserKind_BROWSER_KIND_EDGE,
		browsersig.Brave: berthv1.BrowserKind_BROWSER_KIND_BRAVE, browsersig.HeadlessShell: berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL,
	}
	driverProto = map[browsersig.Driver]berthv1.Driver{
		browsersig.DriverNone: berthv1.Driver_DRIVER_NONE, browsersig.Playwright: berthv1.Driver_DRIVER_PLAYWRIGHT,
		browsersig.Puppeteer: berthv1.Driver_DRIVER_PUPPETEER, browsersig.Automation: berthv1.Driver_DRIVER_AUTOMATION,
	}
	cdpProto = map[browsersig.CDP]berthv1.CdpMode{
		browsersig.CDPNone: berthv1.CdpMode_CDP_MODE_NONE, browsersig.CDPPipe: berthv1.CdpMode_CDP_MODE_PIPE, browsersig.CDPPort: berthv1.CdpMode_CDP_MODE_PORT,
	}
	ownerProto = map[ownerchain.Kind]berthv1.OwnerKind{
		ownerchain.Agent: berthv1.OwnerKind_OWNER_KIND_AGENT, ownerchain.Terminal: berthv1.OwnerKind_OWNER_KIND_TERMINAL,
		ownerchain.App: berthv1.OwnerKind_OWNER_KIND_APP, ownerchain.Person: berthv1.OwnerKind_OWNER_KIND_PERSON,
		ownerchain.None: berthv1.OwnerKind_OWNER_KIND_NONE, ownerchain.Process: berthv1.OwnerKind_OWNER_KIND_PROCESS,
	}
)

// ProfileInUse reports whether a live browser the scan saw uses dir as its profile folder,
// comparing cleaned absolute paths with symlinks resolved on both sides.
func (b *browsers) ProfileInUse(dir string) bool {
	want := profilePath(dir)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.live {
		if t.disc.State != session.Stopping && want != "" && profilePath(t.obs.Signature.UserDataDir) == want {
			return true
		}
	}
	return false
}

// StopUnder stops every live browser of this user whose profile folder is inside root, as a
// forced stop, and returns how many it asked to stop. Boot recovery uses it for the browsers a
// previous berthd launched on Berth's profiles and lost.
func (b *browsers) StopUnder(ctx context.Context, root string) int {
	prefix := profilePath(root)
	if prefix == "" {
		return 0
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	b.mu.Lock()
	var ids []string
	for id, t := range b.live {
		if !t.obs.OtherUser && strings.HasPrefix(profilePath(t.obs.Signature.UserDataDir), prefix) {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.send(ctx, id, session.Event{Kind: session.StopRequested, Note: "left on a Berth profile"})
	}
	return len(ids)
}

// profilePath is a profile folder as a cleaned absolute path with symlinks resolved where the
// folder exists, or "" for a relative path, whose base only its own process knows.
func profilePath(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}
