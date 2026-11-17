package berthd

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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
)

type browsersDeps struct {
	Log    *slog.Logger
	Source ports.ProcessSource
	Store  ports.Store
	List   ports.EntityLister
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
	kick   chan struct{}

	mu sync.Mutex
	// live holds every session the last scan saw or that has not yet reached Stopped.
	live map[string]*tracked
	// ended holds sessions that stopped since berthd started, for ListSessions include_stopped.
	ended map[string]*tracked
	// scanning admits one scan at a time.
	scanning sync.Mutex
}

type tracked struct {
	disc  discovery.Tracked
	obs   discovery.Observed
	since time.Time
}

func newBrowsers(ctx context.Context, d browsersDeps) *browsers {
	b := &browsers{
		browsersDeps: d, ctx: ctx, kick: make(chan struct{}, 1),
		live: map[string]*tracked{}, ended: map[string]*tracked{},
	}
	b.actors = &actors[session.State, session.Event, session.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: session.Kind, initial: session.Initial, transition: session.Transition, run: b.runSession,
		live: map[string]*slot[session.State, session.Event, session.Effect]{},
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
	seen, err := discovery.Scan(ctx, b.Source, b.UID)
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
	st, err := b.state(ctx, entity)
	if err != nil {
		return nil, err
	}
	t = &tracked{disc: discovery.Tracked{State: st}, since: b.Clock.Now()}
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
	next, events := discovery.Events(t.disc, o, now)
	t.disc = next
	if o != nil {
		t.obs = *o
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
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.live[id]
	if !ok {
		return
	}
	t.disc.State, t.since = session.State(rec.To), rec.At
	if t.disc.State == session.Stopped {
		delete(b.live, id)
		b.ended[id] = t
	}
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
			time.AfterFunc(stopGrace, func() {
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
	slices.SortFunc(resp.Sessions, func(x, y *berthv1.Session) int {
		return x.GetBrowser().GetStartedAt().AsTime().Compare(y.GetBrowser().GetStartedAt().AsTime())
	})
	return resp, nil
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
