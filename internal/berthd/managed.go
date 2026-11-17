package berthd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/browser"
	"berth/internal/core/browsersig"
	"berth/internal/core/discovery"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
)

const (
	// launchTimeout bounds a launch until the browser answers over its pipe; Chrome for Testing
	// takes about 1.5 seconds on a fresh profile, and a first run after an update longer.
	launchTimeout = 30 * time.Second
	// closeGrace is how long a browser has after Browser.close before its process group is killed.
	closeGrace = 5 * time.Second
	// closeWait bounds how long CloseBrowser waits for the browser to be closed.
	closeWait = 10 * time.Second
	// closedMax bounds what berthd keeps in memory about closed browsers.
	closedMax = 200
)

// CDPProxy opens the token gated loopback endpoint of one ready browser.
type CDPProxy interface {
	Serve(id string, conn ports.BrowserConn, tokenHash [32]byte) (endpoint string, stop func(), err error)
}

// profileSessions is what the managed browsers need from discovery: whether a browser started
// outside Berth already uses a profile folder, and stopping the browsers a previous berthd left
// on Berth's profiles.
type profileSessions interface {
	ProfileInUse(dir string) bool
	StopUnder(ctx context.Context, root string) int
}

type managedDeps struct {
	Log     *slog.Logger
	Store   ports.Store
	List    ports.EntityLister
	Clock   ports.Clock
	Driver  ports.BrowserDriver
	Proxy   CDPProxy
	Seen    profileSessions
	Home    string
	Browser string
	// AgentLive reports whether an agent session still serves calls; nil treats every session
	// as live.
	AgentLive func(ctx context.Context, id string) bool
}

// managed runs the browsers Berth launches itself. Facts about each live in memory beside its
// actor; only the machine's rows persist. A profile is held exactly while a browser naming it is
// live, so the profile lock is that browser's state, not a separate machine.
type managed struct {
	managedDeps
	ctx    context.Context
	actors *actors[browser.State, browser.Event, browser.Effect]
	grace  time.Duration
	launch time.Duration

	mu sync.Mutex
	// live holds every browser not yet Closed; closed the newest closed ones, closedOrder oldest
	// first.
	live        map[string]*managedBrowser
	closed      map[string]*managedBrowser
	closedOrder []string
	// profiles maps a held profile to the browser holding it.
	profiles map[string]string
	stopping bool
	// opened holds when each agent session opened its browsers in the last hour, and seq the
	// number of its last profile.
	opened map[string][]time.Time
	seq    map[string]int
}

type managedBrowser struct {
	id, profile, executable string
	headless                bool
	tokenHash               [32]byte
	// agent is the agent session an agent browser belongs to; empty for the owner's.
	agent string

	state     browser.State
	since     time.Time
	conn      ports.BrowserConn
	product   string
	endpoint  string
	stopProxy func()
	reason    string
}

func newManaged(ctx context.Context, d managedDeps) *managed {
	m := &managed{
		managedDeps: d, ctx: ctx, grace: closeGrace, launch: launchTimeout,
		live: map[string]*managedBrowser{}, closed: map[string]*managedBrowser{}, profiles: map[string]string{},
		opened: map[string][]time.Time{}, seq: map[string]int{},
	}
	m.actors = &actors[browser.State, browser.Event, browser.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: browser.Kind, initial: browser.Initial, final: []browser.State{browser.Closed}, transition: browser.Transition, run: m.runBrowser,
		live:  map[string]*slot[browser.State, browser.Event, browser.Effect]{},
		saved: m.saved,
	}
	return m
}

func (m *managed) profilesRoot() string { return filepath.Join(m.Home, "profiles") }

// recover ends every browser a previous run left live, then stops the browsers still running on
// Berth's profiles, since Chrome for Testing outlives a closed pipe.
func (m *managed) recover(ctx context.Context) error {
	rows, err := m.List.EntitiesOfKind(ctx, browser.Kind)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if browser.State(r.State) == browser.Closed || browser.State(r.State) == browser.Requested {
			continue
		}
		if _, _, err := step(ctx, m.actors, r.Entity, browser.Event{Kind: browser.Exited, Note: "gone at boot"}, nil, browser.Exited); err != nil {
			return err
		}
	}
	if n := m.Seen.StopUnder(ctx, m.profilesRoot()); n > 0 {
		m.Log.Warn("stopping browsers a previous berthd left on Berth profiles", "count", n)
	}
	m.removeLeftAgentProfiles()
	return nil
}

// saved keeps each browser's facts in step with its machine, and lets go of a closed browser's
// profile and proxy. It runs on the actor's goroutine and must not block.
func (m *managed) saved(rec ports.TransitionRecord) {
	id := strings.TrimPrefix(rec.Entity, browser.Entity(""))
	m.mu.Lock()
	b, ok := m.live[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	// An agent profile is gone before its browser reads as closed, so a caller that saw it
	// closed never finds the profile; the process has exited by then.
	if browser.State(rec.To) == browser.Closed && b.agent != "" {
		m.removeProfile(b.profile)
	}
	m.mu.Lock()
	if _, ok := m.live[id]; !ok {
		m.mu.Unlock()
		return
	}
	b.state, b.since = browser.State(rec.To), rec.At
	var stop func()
	if b.state == browser.Closed {
		b.reason = closeReason(rec)
		stop, b.stopProxy, b.endpoint = b.stopProxy, nil, ""
		delete(m.live, id)
		if m.profiles[b.profile] == id {
			delete(m.profiles, b.profile)
		}
		m.closed[id] = b
		m.closedOrder = append(m.closedOrder, id)
		for len(m.closedOrder) > closedMax {
			delete(m.closed, m.closedOrder[0])
			m.closedOrder = m.closedOrder[1:]
		}
	}
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func closeReason(rec ports.TransitionRecord) string {
	switch browser.EventKind(rec.Event) {
	case browser.LaunchFailed:
		return "launch failed: " + rec.Detail
	case browser.Exited:
		if rec.From == string(browser.Closing) {
			return "closed"
		}
		if rec.Detail != "" {
			return rec.Detail
		}
		return "exited"
	case browser.Launch, browser.Started, browser.CloseRequested, browser.CloseTimedOut:
	}
	return rec.Event
}

func (m *managed) get(id string) (*managedBrowser, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.live[id]
	return b, ok
}

func (m *managed) runBrowser(entity string) machine.Runner[browser.Event, browser.Effect] {
	id := strings.TrimPrefix(entity, browser.Entity(""))
	return func(ctx context.Context, effect browser.Effect) ([]browser.Event, error) {
		b, ok := m.get(id)
		if !ok {
			return nil, nil
		}
		switch effect.Kind {
		case browser.Spawn:
			return m.spawn(ctx, b), nil
		case browser.CloseBrowser:
			m.mu.Lock()
			conn := b.conn
			m.mu.Unlock()
			if conn != nil {
				callCtx, cancel := context.WithTimeout(ctx, time.Second)
				// The reply never matters: the exit watch reports the outcome.
				_, _ = conn.Call(callCtx, "", "Browser.close", nil)
				cancel()
			}
			time.AfterFunc(m.grace, func() {
				if st, found, err := m.Store.LoadState(m.ctx, entity); err == nil && found && browser.State(st.State) == browser.Closing {
					m.send(m.ctx, id, browser.Event{Kind: browser.CloseTimedOut})
				}
			})
		case browser.KillGroup:
			m.mu.Lock()
			conn := b.conn
			m.mu.Unlock()
			if conn != nil {
				if err := conn.Kill(); err != nil {
					m.Log.Warn("could not kill a managed browser", "browser", id, "err", err)
				}
			}
		}
		return nil, nil
	}
}

// spawn starts the browser, waits for it to answer over the pipe, and opens its proxy; on any
// failure it leaves no process behind.
func (m *managed) spawn(ctx context.Context, b *managedBrowser) []browser.Event {
	failed := func(note string) []browser.Event {
		return []browser.Event{{Kind: browser.LaunchFailed, Note: note}}
	}
	m.mu.Lock()
	stopping := m.stopping
	m.mu.Unlock()
	if stopping {
		return failed("berthd is stopping")
	}
	launchCtx, cancel := context.WithTimeout(ctx, m.launch)
	defer cancel()
	conn, err := m.Driver.Launch(launchCtx, ports.BrowserLaunch{
		Executable: b.executable, ProfileDir: filepath.Join(m.profilesRoot(), b.profile), Headless: b.headless,
	})
	if err != nil {
		m.Log.Warn("browser did not start", "browser", b.id, "err", err)
		return failed("the browser did not start")
	}
	m.mu.Lock()
	b.conn = conn
	m.mu.Unlock()
	kill := func() {
		_ = conn.Kill()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
		}
	}
	raw, err := conn.Call(launchCtx, "", "Browser.getVersion", nil)
	if err != nil {
		kill()
		return failed("the browser did not answer over its pipe")
	}
	var version struct {
		Product string `json:"product"`
	}
	_ = json.Unmarshal(raw, &version)
	var endpoint string
	var stop func()
	if m.Proxy != nil {
		if endpoint, stop, err = m.Proxy.Serve(b.id, conn, b.tokenHash); err != nil {
			kill()
			return failed("the CDP proxy could not listen")
		}
	}
	m.mu.Lock()
	b.product, b.endpoint, b.stopProxy = discovery.Clip(version.Product), endpoint, stop
	m.mu.Unlock()
	go func() {
		<-conn.Done()
		m.send(m.ctx, b.id, browser.Event{Kind: browser.Exited})
	}()
	return []browser.Event{{Kind: browser.Started, Note: fmt.Sprintf("pid %d", conn.Process().PID)}}
}

// send applies one event; an illegal one (a close raced the exit) changes nothing.
func (m *managed) send(ctx context.Context, id string, e browser.Event) {
	if _, _, err := step(ctx, m.actors, browser.Entity(id), e, nil, e.Kind); err != nil {
		m.Log.Debug("browser event not applied", "browser", id, "event", e.Kind, "err", err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// executable picks the browser to run: the request's, which must be a Chromium family browser
// by its path, else berthd's -browser flag, else the first one found.
func (m *managed) executable(requested string) (string, error) {
	if requested == "" {
		requested = m.Browser
	}
	if requested == "" {
		requested = findBrowser()
	}
	if requested == "" {
		return "", errors.Join(api.ErrBrowserLaunchFailed, errors.New("no Chromium build found; install Chrome for Testing or name one"))
	}
	sig, ok := browsersig.Classify(requested, nil, false)
	if !ok || sig.Role != browsersig.RoleBrowser || !slices.Contains([]browsersig.Kind{browsersig.Chrome, browsersig.ChromeForTesting, browsersig.Chromium, browsersig.HeadlessShell}, sig.Kind) {
		return "", errors.Join(api.ErrBrowserLaunchFailed, errors.New("the executable is not a Chrome, Chrome for Testing, Chromium or headless shell browser"))
	}
	if st, err := os.Stat(requested); err != nil || st.IsDir() {
		return "", errors.Join(api.ErrBrowserLaunchFailed, errors.New("the executable does not exist"))
	}
	return requested, nil
}

// findBrowser returns the first Chromium build found, preferring the ones made for automation.
func findBrowser() string {
	home, _ := os.UserHomeDir()
	for _, glob := range []string{
		filepath.Join(home, ".cache/puppeteer/chrome/*/chrome-mac-*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"),
		filepath.Join(home, "Library/Caches/ms-playwright/chromium-*/chrome-mac*/Chromium.app/Contents/MacOS/Chromium"),
		filepath.Join(home, "Library/Caches/ms-playwright/chromium-*/chrome-mac*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"),
		"/Applications/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	} {
		matches, _ := filepath.Glob(glob)
		if len(matches) > 0 {
			slices.Sort(matches)
			return matches[len(matches)-1]
		}
	}
	return ""
}

func (m *managed) LaunchBrowser(ctx context.Context, req *berthv1.LaunchBrowserRequest) (*berthv1.LaunchBrowserResponse, error) {
	if strings.HasPrefix(req.GetProfile(), agentProfilePrefix) {
		return nil, errors.Join(api.ErrBrowserLaunchFailed, errors.New("profile names starting with agent- are kept for agent browsers"))
	}
	exe, err := m.executable(req.GetExecutable())
	if err != nil {
		return nil, err
	}
	b, token := m.newBrowser(req.GetProfile(), exe, req.GetHeadless(), "")
	if err := m.start(ctx, b, nil); err != nil {
		return nil, err
	}
	return &berthv1.LaunchBrowserResponse{Browser: m.proto(b), CdpToken: token}, nil
}

// newBrowser makes a browser's facts and its proxy token, of which berthd keeps only the hash.
func (m *managed) newBrowser(profile, exe string, headless bool, agent string) (*managedBrowser, string) {
	token := randomHex(32)
	return &managedBrowser{
		id: randomHex(8), profile: profile, executable: exe, headless: headless, agent: agent,
		tokenHash: sha256.Sum256([]byte(token)), state: browser.Requested, since: m.Clock.Now(),
	}, token
}

// start takes b's profile and launches it, answering once it is ready or failed. admit, when set,
// runs under the same lock that takes the profile, so a limit it checks cannot be raced past.
func (m *managed) start(ctx context.Context, b *managedBrowser, admit func() error) error {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return errors.Join(api.ErrBrowserLaunchFailed, errors.New("berthd is stopping"))
	}
	if admit != nil {
		if err := admit(); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	dir := filepath.Join(m.profilesRoot(), b.profile)
	switch {
	case m.profiles[b.profile] != "":
		holder := m.profiles[b.profile]
		m.mu.Unlock()
		return errors.Join(api.ErrProfileBusy, fmt.Errorf("browser %s already uses profile %s", holder, b.profile))
	case m.Seen.ProfileInUse(dir):
		m.mu.Unlock()
		return errors.Join(api.ErrProfileBusy, fmt.Errorf("a browser started outside Berth uses profile %s", b.profile))
	}
	m.profiles[b.profile], m.live[b.id] = b.id, b
	m.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.release(b)
		return err
	}
	// The launch runs on berthd's context, so its outcome is always seen: a caller that gives up
	// cannot leave a browser starting that nobody closes. Whoever takes the result owns it.
	res := make(chan error, 1)
	go func() { res <- m.settleLaunch(b) }() //nolint:contextcheck // see above
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		go func() {
			if err := <-res; err == nil {
				m.send(m.ctx, b.id, browser.Event{Kind: browser.CloseRequested, Note: "the caller gave up"})
			}
		}()
		return ctx.Err()
	}
}

// settleLaunch applies Launch to b and waits for its outcome, on berthd's context. An agent browser
// whose session ended meanwhile is closed here, since the session's end hook may have run while
// the browser had no row yet.
func (m *managed) settleLaunch(b *managedBrowser) error {
	rec, _, err := step(m.ctx, m.actors, browser.Entity(b.id), browser.Event{Kind: browser.Launch}, nil,
		browser.Started, browser.LaunchFailed, browser.Exited)
	if err != nil {
		// Only berthd stopping fails a step on its own context; boot recovers any row left.
		if m.state(b.id) == browser.Requested {
			m.release(b)
		}
		return err
	}
	// The waiter hears of a transition just before the saved hook runs, so record it here too.
	m.saved(rec)
	if b.agent != "" && !m.agentLive(m.ctx, b.agent) {
		m.forgetAgent(b.agent)
		if m.state(b.id) != browser.Closed {
			m.send(m.ctx, b.id, browser.Event{Kind: browser.CloseRequested, Note: "agent session ended"})
		}
		return api.ErrAgentTokenInvalid
	}
	if pb := m.proto(b); pb.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
		return errors.Join(api.ErrBrowserLaunchFailed, errors.New(pb.GetCloseReason()))
	}
	return nil
}

// release lets go of a browser that never got a stored row.
func (m *managed) release(b *managedBrowser) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, b.id)
	if m.profiles[b.profile] == b.id {
		delete(m.profiles, b.profile)
	}
}

func (m *managed) state(id string) browser.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.live[id]; ok {
		return b.state
	}
	return browser.Closed
}

func (m *managed) CloseBrowser(ctx context.Context, req *berthv1.CloseBrowserRequest) (*berthv1.CloseBrowserResponse, error) {
	b, err := m.closeAndWait(ctx, m.find(req.GetId()))
	if err != nil {
		return nil, err
	}
	return &berthv1.CloseBrowserResponse{Browser: m.proto(b)}, nil
}

// find returns the live or recently closed browser with this id, or nil.
func (m *managed) find(id string) *managedBrowser {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.live[id]; ok {
		return b
	}
	return m.closed[id]
}

// closeAndWait asks b to close and waits up to closeWait for it to be closed; a closed browser
// answers as it is.
func (m *managed) closeAndWait(ctx context.Context, b *managedBrowser) (*managedBrowser, error) {
	if b == nil {
		return nil, api.ErrBrowserNotFound
	}
	if st := m.state(b.id); st == browser.Closed {
		return b, nil
	} else if st != browser.Closing {
		m.send(ctx, b.id, browser.Event{Kind: browser.CloseRequested})
	}
	deadline := time.NewTimer(closeWait)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for m.state(b.id) != browser.Closed {
		select {
		case <-poll.C:
		case <-deadline.C:
			return b, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b, nil
}

func (m *managed) ListBrowsers(_ context.Context, req *berthv1.ListBrowsersRequest) (*berthv1.ListBrowsersResponse, error) {
	m.mu.Lock()
	all := make([]*managedBrowser, 0, len(m.live)+len(m.closed))
	for _, b := range m.live {
		all = append(all, b)
	}
	if req.GetIncludeClosed() {
		for _, b := range m.closed {
			all = append(all, b)
		}
	}
	m.mu.Unlock()
	resp := &berthv1.ListBrowsersResponse{}
	for _, b := range all {
		resp.Browsers = append(resp.Browsers, m.proto(b))
	}
	slices.SortFunc(resp.Browsers, func(x, y *berthv1.ManagedBrowser) int {
		return strings.Compare(x.GetProfile()+x.GetId(), y.GetProfile()+y.GetId())
	})
	return resp, nil
}

// stop closes every live browser for berthd's shutdown: Browser.close first, then the process
// groups of whatever is left once timeout passes.
func (m *managed) stop(ctx context.Context, timeout time.Duration) {
	m.mu.Lock()
	m.stopping = true
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if m.state(id) != browser.Closing {
				m.send(ctx, id, browser.Event{Kind: browser.CloseRequested, Note: "berthd stopping"})
			}
		})
	}
	wg.Wait()
	for m.liveCount() > 0 && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	var left []ports.BrowserConn
	for _, b := range m.live {
		if b.conn != nil {
			left = append(left, b.conn)
		}
	}
	m.mu.Unlock()
	for _, c := range left {
		_ = c.Kill()
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *managed) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

func (m *managed) proto(b *managedBrowser) *berthv1.ManagedBrowser {
	m.mu.Lock()
	defer m.mu.Unlock()
	pb := &berthv1.ManagedBrowser{
		Id: b.id, Profile: b.profile, State: browserStateProto(b.state), Executable: b.executable,
		Headless: b.headless, Product: b.product, CdpEndpoint: b.endpoint, StateSince: timestamppb.New(b.since),
		CloseReason: b.reason, AgentSessionId: b.agent,
	}
	if b.conn != nil {
		pb.SessionId = discovery.ID(b.conn.Process())
	}
	return pb
}

func browserStateProto(s browser.State) berthv1.BrowserState {
	switch s {
	case browser.Requested, browser.Launching:
		return berthv1.BrowserState_BROWSER_STATE_LAUNCHING
	case browser.Ready:
		return berthv1.BrowserState_BROWSER_STATE_READY
	case browser.Closing:
		return berthv1.BrowserState_BROWSER_STATE_CLOSING
	case browser.Closed:
		return berthv1.BrowserState_BROWSER_STATE_CLOSED
	}
	return berthv1.BrowserState_BROWSER_STATE_UNSPECIFIED
}
