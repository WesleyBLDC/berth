package berthd

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"testing"

	"berth/internal/adapters/sqlite"
	"berth/internal/core/agentsession"
	"berth/internal/core/browser"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
	"berth/internal/core/session"
	"berth/internal/testutil"
)

// modelWorld is the outside world the fake effects act on: the child processes a fake browser
// spawn starts, which nothing may leave running once its entity ended.
type modelWorld struct {
	mu       sync.Mutex
	children map[string]*modelChild
}

type modelChild struct {
	cmd    *exec.Cmd
	reaped chan struct{}
}

func newModelWorld(t *testing.T) *modelWorld {
	w := &modelWorld{children: map[string]*modelChild{}}
	t.Cleanup(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for entity, c := range w.children {
			select {
			case <-c.reaped:
			default:
				t.Errorf("%s left child %d running at the end of the walk", entity, c.cmd.Process.Pid)
				_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
				<-c.reaped
			}
		}
	})
	return w
}

// spawn starts a child in its own process group, as a managed browser is.
func (w *modelWorld) spawn(entity string) error {
	// Never tied to a context: only the walk may end it, so a leak shows.
	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	c := &modelChild{cmd: cmd, reaped: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.reaped) }()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.children[entity] = c
	return nil
}

// kill signals the entity's process group and returns at once; the child is reaped beside it.
func (w *modelWorld) kill(entity string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.children[entity]; ok {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (w *modelWorld) running(entity string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.children[entity]
	if !ok {
		return false
	}
	select {
	case <-c.reaped:
		return false
	default:
		return true
	}
}

func entities(f func(string) string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = f(fmt.Sprintf("m%02d", i))
	}
	return out
}

func names[K ~string](kinds ...K) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func secretSpec(t *testing.T, n int) modelSpec[secret.State, secret.Event, secret.Effect] {
	return modelSpec[secret.State, secret.Event, secret.Effect]{
		name: "secret", kit: newSecrets(t.Context(), secretsDeps{}).secrets,
		states: secret.States, events: secret.AllEvents, entities: entities(secret.Entity, n),
		note: func(e secret.Event, tok string) secret.Event { e.Note = tok; return e },
		until: func(e secret.Event) []string {
			return map[secret.EventKind][]string{
				secret.StoreRequested:  names(secret.StoreSucceeded, secret.StoreDenied, secret.StoreFailed),
				secret.RotateRequested: names(secret.RotateSucceeded, secret.RotateDenied, secret.RotateFailed),
				secret.RemoveRequested: names(secret.RemoveSucceeded, secret.RemoveDenied, secret.RemoveFailed),
			}[e.Kind]
		},
		effect: func(_ *modelWorld, _ string, f secret.Effect, pick func(int) int) []secret.Event {
			outcomes := map[secret.EffectKind][]secret.EventKind{
				secret.PromptStore:  {secret.StoreSucceeded, secret.StoreDenied, secret.StoreFailed},
				secret.PromptRotate: {secret.RotateSucceeded, secret.RotateDenied, secret.RotateFailed},
				secret.PromptRemove: {secret.RemoveSucceeded, secret.RemoveDenied, secret.RemoveFailed},
			}[f.Kind]
			if len(outcomes) == 0 {
				return nil
			}
			return []secret.Event{{Kind: outcomes[pick(len(outcomes))]}}
		},
	}
}

func grantSpec(t *testing.T, n int) modelSpec[grant.State, grant.Event, grant.Effect] {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = grant.Entity(int64(i + 1))
	}
	return modelSpec[grant.State, grant.Event, grant.Effect]{
		name: "grant", kit: newSecrets(t.Context(), secretsDeps{}).grants,
		states: grant.States, events: grant.AllEvents, entities: ids,
		note: func(e grant.Event, tok string) grant.Event { e.Note = tok; return e },
		until: func(e grant.Event) []string {
			if e.Kind == grant.GrantRequested {
				return names(grant.GrantApproved, grant.GrantDenied, grant.GrantFailed)
			}
			return nil
		},
		effect: func(_ *modelWorld, _ string, f grant.Effect, pick func(int) int) []grant.Event {
			if f.Kind != grant.PromptPower {
				return nil
			}
			outcomes := []grant.EventKind{grant.GrantApproved, grant.GrantDenied, grant.GrantFailed}
			return []grant.Event{{Kind: outcomes[pick(len(outcomes))]}}
		},
	}
}

func proxySpec(t *testing.T, n int) modelSpec[proxysession.State, proxysession.Event, proxysession.Effect] {
	return modelSpec[proxysession.State, proxysession.Event, proxysession.Effect]{
		name: "proxy", kit: newSecrets(t.Context(), secretsDeps{}).sessions,
		states: proxysession.States, events: proxysession.AllEvents, entities: entities(proxysession.Entity, n),
		note: func(e proxysession.Event, tok string) proxysession.Event { e.Note = tok; return e },
	}
}

func agentSpec(t *testing.T, n int) modelSpec[agentsession.State, agentsession.Event, agentsession.Effect] {
	return modelSpec[agentsession.State, agentsession.Event, agentsession.Effect]{
		name: "agent", kit: newAgents(t.Context(), agentsDeps{}).sessions,
		states: agentsession.States, events: agentsession.AllEvents, entities: entities(agentsession.Entity, n),
		note: func(e agentsession.Event, tok string) agentsession.Event { e.Note = tok; return e },
	}
}

func sessionSpec(t *testing.T, n int) modelSpec[session.State, session.Event, session.Effect] {
	return modelSpec[session.State, session.Event, session.Effect]{
		name: "session", kit: newBrowsers(t.Context(), browsersDeps{}).actors,
		states: session.States, events: session.AllEvents, entities: entities(session.Entity, n),
		note: func(e session.Event, tok string) session.Event { e.Note = tok; return e },
	}
}

// browserSpec spawns a real child per launch: the effect kills it on a failed launch and on
// KillGroup, and the saved hook kills it once the browser is Closed, as the shell's do.
func browserSpec(t *testing.T, n int) modelSpec[browser.State, browser.Event, browser.Effect] {
	return modelSpec[browser.State, browser.Event, browser.Effect]{
		name: "browser", kit: newManaged(t.Context(), managedDeps{}).actors,
		states: browser.States, events: browser.AllEvents, entities: entities(browser.Entity, n),
		note: func(e browser.Event, tok string) browser.Event { e.Note = tok; return e },
		until: func(e browser.Event) []string {
			if e.Kind == browser.Launch {
				return names(browser.Started, browser.LaunchFailed, browser.Exited)
			}
			return nil
		},
		effect: func(w *modelWorld, entity string, f browser.Effect, pick func(int) int) []browser.Event {
			switch f.Kind {
			case browser.Spawn:
				if err := w.spawn(entity); err != nil || pick(3) == 0 {
					w.kill(entity)
					return []browser.Event{{Kind: browser.LaunchFailed}}
				}
				return []browser.Event{{Kind: browser.Started}}
			case browser.KillGroup:
				w.kill(entity)
			case browser.CloseBrowser:
			}
			return nil
		},
		saved: func(w *modelWorld, rec ports.TransitionRecord) {
			if browser.State(rec.To) == browser.Closed {
				w.kill(rec.Entity)
			}
		},
		idle: func(w *modelWorld, entity string, s browser.State) error {
			if (s == browser.Requested || s == browser.Closed) && w.running(entity) {
				return fmt.Errorf("%s rests in %s with its child still running", entity, s)
			}
			return nil
		},
	}
}

// endBrowsers is boot recovery's part for the walk: every browser left live is told it exited,
// after which no child may remain.
func endBrowsers(r *modelRun[browser.State, browser.Event, browser.Effect]) {
	for _, entity := range r.spec.entities {
		if r.state(entity) != browser.Closed {
			r.do(context.Background(), r.current(), entity, browser.Event{Kind: browser.Exited})
		}
	}
	r.settle("after ending every browser")
	for _, entity := range r.spec.entities {
		if r.world.running(entity) {
			r.t.Errorf("%s left its child running after it was Closed", entity)
		}
	}
}

func memoryStore(*testing.T) ports.Store { return testutil.NewStore() }

func sqliteStore(t *testing.T) ports.Store {
	s, err := sqlite.Open(t.Context(), t.TempDir()+"/berth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The budget is each test's share of make check: a sequential walk on SQLite, then concurrent
// phases on the memory store, one of which stops berthd part way, and one on SQLite.
const (
	sequentialSteps = 120
	walkers         = 4
	perWalker       = 100
	sqliteWalker    = 25
)

func runModel[S ~string, E machine.Event, F any](t *testing.T, spec func(*testing.T, int) modelSpec[S, E, F], seed uint64, end func(*modelRun[S, E, F])) {
	t.Run("sequential", func(t *testing.T) {
		r := newModelRun(t, spec(t, 3), seed, sqliteStore(t))
		r.walk(machinetest.Steps(t, sequentialSteps))
		if end != nil {
			end(r)
		}
	})
	t.Run("concurrent", func(t *testing.T) { runConcurrent(t, spec, seed, end) })
	t.Run("concurrent sqlite", func(t *testing.T) {
		r := newModelRun(t, spec(t, 2), seed, sqliteStore(t))
		r.concurrent(1, walkers, machinetest.Steps(t, sqliteWalker), false)
		if end != nil {
			end(r)
		}
	})
}

func runConcurrent[S ~string, E machine.Event, F any](t *testing.T, spec func(*testing.T, int) modelSpec[S, E, F], seed uint64, end func(*modelRun[S, E, F])) {
	r := newModelRun(t, spec(t, 2), seed, memoryStore(t))
	per := machinetest.Steps(t, perWalker)
	r.concurrent(1, walkers, per, false)
	r.concurrent(2, walkers, per, true)
	r.concurrent(3, walkers, per, false)
	if end != nil {
		end(r)
	}
}

func TestModelSecret(t *testing.T) {
	runModel(t, secretSpec, machinetest.Seed(t), nil)
}

func TestModelGrant(t *testing.T) {
	runModel(t, grantSpec, machinetest.Seed(t), nil)
}

func TestModelProxySession(t *testing.T) {
	runModel(t, proxySpec, machinetest.Seed(t), nil)
}

func TestModelAgentSession(t *testing.T) {
	runModel(t, agentSpec, machinetest.Seed(t), nil)
}

func TestModelSession(t *testing.T) {
	runModel(t, sessionSpec, machinetest.Seed(t), nil)
}

func TestModelBrowser(t *testing.T) {
	runModel(t, browserSpec, machinetest.Seed(t), endBrowsers)
}

// TestModelRegressions replays each seed that once found a bug. Goroutines interleave anew on
// every run, so a seed replays its commands, not its schedule; each runs a few times, and the
// deterministic test beside each fix (actors_race_test.go) pins the bug itself.
func TestModelRegressions(t *testing.T) {
	for _, c := range []struct {
		name string
		seed uint64
		run  func(*testing.T, uint64)
	}{
		{"secret refusal answered unavailable", 1795143668884585000, func(t *testing.T, seed uint64) { runConcurrent(t, secretSpec, seed, nil) }},
		{"idle actor for an ended proxy session", 1795143902571008000, func(t *testing.T, seed uint64) { runConcurrent(t, proxySpec, seed, nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			for range 4 {
				c.run(t, c.seed)
			}
		})
	}
}
