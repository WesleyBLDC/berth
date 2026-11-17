package berthd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/proxyctl"
	"berth/internal/proxyproc"
	"berth/internal/testutil"
)

// startedCredential is low entropy on purpose, so secret scanners do not flag it.
const startedCredential = "dddddddddddddddddddddddddddddddd"

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

// fakeStarter plays the berth-proxy berthd starts: it authorizes and watches its session through
// the fixture's secrets as the real one does on the owner socket, prints the control line once
// it serves, and ends its session when its stdin closes or berthd stops it.
type fakeStarter struct {
	f      *fixture
	caFile string
	// hang makes the proxy never answer; skipAuthorize makes it print a control line without
	// ever holding a session; ignoreStdin makes it keep running after its stdin closes; failAtOnce
	// makes it exit before it reads anything.
	hang, skipAuthorize, ignoreStdin, failAtOnce bool

	mu     sync.Mutex
	modes  []string
	pids   []int
	next   int
	killed map[int]bool
	// recorded says, per pid, whether berthd still held its record when the process exited;
	// reaped says whether berthd reaped it.
	recorded map[int]bool
	reaped   map[int]bool
}

func newFakeStarter(t *testing.T, f *fixture) *fakeStarter {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte(testCA), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := &fakeStarter{f: f, caFile: ca, next: proxyPID, killed: map[int]bool{}, recorded: map[int]bool{}, reaped: map[int]bool{}}
	f.s.StartProxyProcess = fs.start
	return fs
}

func (fs *fakeStarter) start(ctx context.Context) (*proxyproc.Process, error) {
	fs.mu.Lock()
	pid := fs.next
	fs.next++
	fs.pids = append(fs.pids, pid)
	fs.mu.Unlock()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exited, killed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer func() {
			// A process that exited breaks both its pipes.
			_ = inR.CloseWithError(io.ErrClosedPipe)
			_ = outW.Close()
			fs.mu.Lock()
			fs.recorded[pid] = fs.f.s.startedFor(pid) != ""
			fs.mu.Unlock()
			close(exited)
			// kqueue reports the exit before the parent reaps.
			fs.f.exits.exit(pid)
		}()
		if fs.failAtOnce {
			return
		}
		in := bufio.NewReader(inR)
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req proxyctl.Request
		if json.Unmarshal(line, &req) != nil || fs.hang {
			_, _ = io.Copy(io.Discard, in)
			return
		}
		fs.mu.Lock()
		fs.modes = append(fs.modes, req.Mode)
		fs.mu.Unlock()
		session := ""
		stop := make(chan struct{})
		if !fs.skipAuthorize {
			resp, err := fs.f.s.AuthorizeProxy(ctx, &berthv1.AuthorizeProxyRequest{Names: req.Secrets}, peerOf(pid))
			if err != nil || resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
				return
			}
			session = resp.GetSessionId()
			serving := make(chan struct{})
			go func() {
				_ = fs.f.s.WatchProxySession(ctx, session, peerOf(pid), func(e *berthv1.ProxySessionEvent) error {
					if e.GetKind() == berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING {
						close(serving)
					} else {
						close(stop)
					}
					return nil
				})
			}()
			<-serving
		}
		ctl, _ := json.Marshal(proxyctl.Control{
			Port: 4123, Credential: startedCredential, CAFile: fs.caFile, BundleFile: fs.caFile,
			Placeholders: map[string]string{"API_KEY": "berth-proxy-placeholder"},
		})
		_, _ = outW.Write(append(ctl, '\n'))
		closed := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, in)
			if !fs.ignoreStdin {
				close(closed)
			}
		}()
		select {
		case <-closed:
		case <-stop:
		case <-killed:
			// A killed process takes a moment to die, so a record dropped before the exit shows.
			time.Sleep(20 * time.Millisecond)
		}
		if session != "" {
			_, _ = fs.f.s.EndProxySession(ctx, &berthv1.EndProxySessionRequest{SessionId: session}, peerOf(pid))
		}
	}()
	return &proxyproc.Process{
		PID: pid, Stdin: inW, Stdout: outR,
		Wait: func() error {
			<-exited
			fs.mu.Lock()
			fs.reaped[pid] = true
			fs.mu.Unlock()
			return nil
		},
		Kill: func() error {
			once.Do(func() {
				fs.mu.Lock()
				fs.killed[pid] = true
				fs.mu.Unlock()
				close(killed)
			})
			if !fs.ignoreStdin {
				_ = inR.CloseWithError(errors.New("killed"))
			}
			return nil
		},
	}, nil
}

func (fs *fakeStarter) starts() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.pids)
}

// started runs StartProxy as the agent socket would and returns its first message and a channel
// with its end.
func started(ctx context.Context, t *testing.T, f *fixture, names ...string) (*berthv1.StartProxyResponse, <-chan error) {
	t.Helper()
	first := make(chan *berthv1.StartProxyResponse, 1)
	done := make(chan error, 1)
	go func() {
		done <- f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: names}, func(r *berthv1.StartProxyResponse) error {
			first <- r
			return nil
		})
	}()
	select {
	case r := <-first:
		return r, done
	case err := <-done:
		return nil, chanOf(err)
	case <-time.After(5 * time.Second):
		t.Fatal("StartProxy sent nothing")
		return nil, nil
	}
}

func chanOf(err error) <-chan error {
	c := make(chan error, 1)
	c <- err
	return c
}

func ended(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("StartProxy did not return")
		return nil
	}
}

const agentSession = "0123456789abcdef0123456789abcdef"

func agentFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	f.grantProxy(t)
	f.s.AgentLive = func(_ context.Context, id string) bool { return id == agentSession }
	return f
}

// TestStartProxyRunsTheProxyForTheAgentSession: berthd's own proxy serves the agent with the
// facts its child needs, its session names the agent session, and the agent session's end stops it.
func TestStartProxyRunsTheProxyForTheAgentSession(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	resp, done := started(api.WithAgentSession(t.Context(), agentSession), t, f, "gh-token")
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED || len(resp.GetSessionId()) != 32 || resp.GetPort() != 4123 ||
		resp.GetCredential() != startedCredential || resp.GetCaPem() != testCA || resp.GetPlaceholders()["API_KEY"] != "berth-proxy-placeholder" {
		t.Fatalf("first message %v", resp)
	}
	rec, ok, err := f.store.ProxySession(t.Context(), resp.GetSessionId())
	if err != nil || !ok || rec.PID != proxyPID || rec.AgentSession != agentSession {
		t.Fatalf("session row %+v %v %v", rec, ok, err)
	}
	fs.mu.Lock()
	modes := slices.Clone(fs.modes)
	fs.mu.Unlock()
	if !slices.Equal(modes, []string{proxyctl.ModeAgent}) {
		t.Fatalf("berthd started its proxy in modes %q, want agent", modes)
	}
	f.s.stopAgentSessions(t.Context(), agentSession)
	if err := ended(t, done); err != nil {
		t.Fatal(err)
	}
	f.eventually(t, "proxy/"+resp.GetSessionId(), "Ended")
	if fs.starts() != 1 || f.s.startedFor(proxyPID) != "" {
		t.Fatalf("%d starts, pid still recorded for %q", fs.starts(), f.s.startedFor(proxyPID))
	}
}

// TestStartProxyStopsWhenTheStreamCloses: the caller exiting (its stream closing) ends the
// proxy's session cleanly.
func TestStartProxyStopsWhenTheStreamCloses(t *testing.T) {
	f := agentFixture(t)
	newFakeStarter(t, f)
	ctx, cancel := context.WithCancel(api.WithAgentSession(t.Context(), agentSession))
	resp, done := started(ctx, t, f, "gh-token")
	cancel()
	if err := ended(t, done); err != nil {
		t.Fatal(err)
	}
	f.eventually(t, "proxy/"+resp.GetSessionId(), "Ended")
}

func TestStartProxyWithoutAGrantStartsNothing(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	resp, done := started(api.WithAgentSession(t.Context(), agentSession), t, f, "gh-token", "other")
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT || resp.GetSessionId() != "" || resp.GetCredential() != "" {
		t.Fatalf("first message %v", resp)
	}
	if err := ended(t, done); err != nil || fs.starts() != 0 {
		t.Fatalf("end %v, %d starts", err, fs.starts())
	}
}

// TestStartProxyRefusesAControlLineWithoutASession: a process berthd started that never held a
// trusted session gets nothing passed on and is killed.
func TestStartProxyRefusesAControlLineWithoutASession(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	fs.skipAuthorize = true
	resp, done := started(api.WithAgentSession(t.Context(), agentSession), t, f, "gh-token")
	if err := ended(t, done); resp != nil || !errors.Is(err, api.ErrProxyNotStarted) {
		t.Fatalf("got %v, %v", resp, err)
	}
	if !fs.killed[proxyPID] {
		t.Fatal("the process was not killed")
	}
}

// TestStartProxyLimitsEachAgentSession: one agent session can have berthd run at most
// maxStartedProxies proxies at once; the owner has no limit.
func TestStartProxyLimitsEachAgentSession(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	fs.hang = true
	ctx, cancel := context.WithCancel(api.WithAgentSession(t.Context(), agentSession))
	var wg sync.WaitGroup
	for range maxStartedProxies {
		wg.Go(func() {
			_ = f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(*berthv1.StartProxyResponse) error { return nil })
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for fs.starts() < maxStartedProxies && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	err := f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(*berthv1.StartProxyResponse) error { return nil })
	if !errors.Is(err, api.ErrRateLimited) || fs.starts() != maxStartedProxies {
		t.Fatalf("one start too many: %v, %d starts", err, fs.starts())
	}
	ownerCtx, stopOwner := context.WithCancel(t.Context())
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- f.s.StartProxy(ownerCtx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(*berthv1.StartProxyResponse) error { return nil })
	}()
	for fs.starts() < maxStartedProxies+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.starts() != maxStartedProxies+1 {
		t.Fatalf("the owner was limited: %d starts", fs.starts())
	}
	stopOwner()
	cancel()
	wg.Wait()
	if err := <-ownerDone; err != nil && !errors.Is(err, api.ErrProxyNotStarted) {
		t.Fatal(err)
	}
	// Every slot is free again once those calls ended.
	f.s.mu.Lock()
	n := len(f.s.starting)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d agent sessions still hold starts", n)
	}
}

// TestStartProxyKillsAProxyThatIgnoresItsStdin: a proxy that keeps running after its stream
// closed is killed after stopWait, and berthd drops its record only after the exit and reaps it
// only after that.
func TestStartProxyKillsAProxyThatIgnoresItsStdin(t *testing.T) {
	f := agentFixture(t)
	f.s.stopWait = 50 * time.Millisecond
	fs := newFakeStarter(t, f)
	fs.ignoreStdin = true
	ctx, cancel := context.WithCancel(api.WithAgentSession(t.Context(), agentSession))
	resp, done := started(ctx, t, f, "gh-token")
	cancel()
	if err := ended(t, done); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.killed[proxyPID] || !fs.reaped[proxyPID] || !fs.recorded[proxyPID] {
		t.Fatalf("killed %v, reaped %v, record held until the exit %v", fs.killed[proxyPID], fs.reaped[proxyPID], fs.recorded[proxyPID])
	}
	if f.s.startedFor(proxyPID) != "" {
		t.Fatal("the record outlived the call")
	}
	f.eventually(t, "proxy/"+resp.GetSessionId(), "Ended")
}

// TestStartProxyForgetsOnlyItsOwnRecord: a call ending never drops a record a later call made
// for the same pid.
func TestStartProxyForgetsOnlyItsOwnRecord(t *testing.T) {
	f := agentFixture(t)
	first, later := &startRecord{agent: "first"}, &startRecord{agent: "later"}
	f.s.started[proxyPID] = later
	f.s.forget(proxyPID, first)
	if f.s.startedFor(proxyPID) != "later" {
		t.Fatal("the first call dropped the later call's record")
	}
	f.s.forget(proxyPID, later)
	if f.s.startedFor(proxyPID) != "" {
		t.Fatal("the later call's record stayed")
	}
}

// TestStartProxyLimitsStartsPerHour: one agent session can have berthd start maxStartsPerHour
// proxies in any rolling hour, failed ones included.
func TestStartProxyLimitsStartsPerHour(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	fs.failAtOnce = true
	ctx := api.WithAgentSession(t.Context(), agentSession)
	start := func() error {
		return f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(*berthv1.StartProxyResponse) error { return nil })
	}
	for i := range maxStartsPerHour {
		if err := start(); !errors.Is(err, api.ErrProxyNotStarted) {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if err := start(); !errors.Is(err, api.ErrRateLimited) || fs.starts() != maxStartsPerHour {
		t.Fatalf("one start too many in the hour: %v, %d starts", err, fs.starts())
	}
	f.clock.Advance(time.Hour)
	if err := start(); !errors.Is(err, api.ErrProxyNotStarted) || fs.starts() != maxStartsPerHour+1 {
		t.Fatalf("an hour later: %v, %d starts", err, fs.starts())
	}
}

// TestStartProxyCountsRefusalsAgainstTheLimit: a call refused for want of a grant counts too, so
// an agent session cannot add UseRefused rows without limit.
func TestStartProxyCountsRefusalsAgainstTheLimit(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	f.s.AgentLive = func(_ context.Context, id string) bool { return id == agentSession }
	fs := newFakeStarter(t, f)
	ctx := api.WithAgentSession(t.Context(), agentSession)
	for i := range maxStartsPerHour + 3 {
		var got *berthv1.StartProxyResponse
		err := f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(r *berthv1.StartProxyResponse) error { got = r; return nil })
		switch {
		case i < maxStartsPerHour && (err != nil || got.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT):
			t.Fatalf("call %d: %v %v", i, got, err)
		case i >= maxStartsPerHour && !errors.Is(err, api.ErrRateLimited):
			t.Fatalf("call %d past the limit: %v %v", i, got, err)
		}
	}
	refused := 0
	for _, r := range f.history(t, "secret/gh-token") {
		if r.Event == "UseRefused" {
			refused++
		}
	}
	if fs.starts() != 0 || refused > maxStartsPerHour {
		t.Fatalf("%d starts, %d refusals recorded, want at most %d", fs.starts(), refused, maxStartsPerHour)
	}
}

func TestStartProxyWithAnUnreadableCAStopsTheProxy(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	fs.caFile = filepath.Join(t.TempDir(), "missing.pem")
	resp, done := started(api.WithAgentSession(t.Context(), agentSession), t, f, "gh-token")
	if err := ended(t, done); resp != nil || !errors.Is(err, api.ErrProxyNotStarted) {
		t.Fatalf("got %v, %v", resp, err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.killed[proxyPID] || !fs.reaped[proxyPID] {
		t.Fatal("the proxy was not killed and reaped")
	}
}

// TestStartProxyWhoseFirstSendFailsStopsTheProxy: a caller gone before the facts reach it gets
// its proxy ended.
func TestStartProxyWhoseFirstSendFailsStopsTheProxy(t *testing.T) {
	f := agentFixture(t)
	newFakeStarter(t, f)
	gone := errors.New("caller gone")
	err := f.s.StartProxy(api.WithAgentSession(t.Context(), agentSession), &berthv1.StartProxyRequest{Names: []string{"gh-token"}},
		func(*berthv1.StartProxyResponse) error { return gone })
	if !errors.Is(err, gone) {
		t.Fatalf("got %v", err)
	}
	open, err := f.store.OpenProxySessions(t.Context())
	if err != nil || len(open) != 0 {
		t.Fatalf("open sessions %v %v", open, err)
	}
}

// An agent session's end drops its start times, so the limiter keeps nothing for it.
func TestAgentSessionEndDropsItsProxyStartTimes(t *testing.T) {
	f := agentFixture(t)
	fs := newFakeStarter(t, f)
	fs.failAtOnce = true
	ctx := api.WithAgentSession(t.Context(), agentSession)
	_ = f.s.StartProxy(ctx, &berthv1.StartProxyRequest{Names: []string{"gh-token"}}, func(*berthv1.StartProxyResponse) error { return nil })
	f.s.mu.Lock()
	n := len(f.s.startTimes[agentSession])
	f.s.mu.Unlock()
	if n != 1 {
		t.Fatalf("start times before the end = %d, want 1", n)
	}
	f.s.stopAgentSessions(t.Context(), agentSession)
	f.s.mu.Lock()
	_, kept := f.s.startTimes[agentSession]
	f.s.mu.Unlock()
	if kept {
		t.Fatal("an ended agent session's start times must be dropped")
	}
}
