package berthd

// Model based tests of the actors kit: random walks of commands per machine through the real
// actors (actors.go) and the machine kit, with fake effects, against a model that replays the
// machine's own transition table. machinetest.Walk proves each table; these walks prove the kit
// that runs it: one live actor per entity, hooks once per saved transition, ended actors gone,
// steps serialized, and the persisted state equal to the model's. A failing walk logs its seed;
// BERTH_MODEL_SEED replays it, and BERTH_MODEL_STEPS lengthens a walk (make model-long).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berth/internal/adapters/api"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// modelSpec tells the walk about one machine; the kit fields (kind, initial, transition, final)
// come from the shell's own actors, so the walk runs exactly what berthd runs.
type modelSpec[S ~string, E machine.Event, F any] struct {
	name     string
	kit      *actors[S, E, F]
	states   []S
	events   []E
	entities []string
	// note returns e carrying the token that names the command or follow up it belongs to.
	note func(e E, token string) E
	// until names the events a step for e waits on, as the shell's own step calls do; nil means
	// the event itself.
	until func(e E) []string
	// effect performs one effect in the fake world and returns its follow up events, choosing an
	// outcome with pick.
	effect func(w *modelWorld, entity string, f F, pick func(int) int) []E
	// saved frees what an entity held once a transition is saved and its effects ran, as the
	// shell's saved hooks do.
	saved func(w *modelWorld, rec ports.TransitionRecord)
	// idle reports what an entity resting in s must not hold, such as a child process.
	idle func(w *modelWorld, entity string, s S) error
}

type recKey struct {
	entity  string
	version int64
}

// modelRun is one walk of one machine over one store.
type modelRun[S ~string, E machine.Event, F any] struct {
	t     *testing.T
	spec  modelSpec[S, E, F]
	seed  uint64
	inner ports.Store
	world *modelWorld
	label string
	// jitter makes effects pause a little, so cancellations and stops land mid step.
	jitter atomic.Bool

	mu        sync.Mutex
	kit       *actors[S, E, F]
	cancel    context.CancelFunc
	instance  int
	stopped   map[int]bool
	logs      map[int]*lockedBuffer
	persisted map[string]ports.TransitionRecord
	savedBy   map[recKey]int
	hookPre   map[recKey][]ports.TransitionRecord
	hookPost  map[recKey][]ports.TransitionRecord
	created   map[string]int
	effects   map[string]int
	inEffect  map[string]bool
	owner     map[string]string // token to entity
	parent    map[string]string // follow up token to its command's token
	seq       int
	cache     map[string][]ports.TransitionRecord
	faults    []string
	trace     []string
}

func newModelRun[S ~string, E machine.Event, F any](t *testing.T, spec modelSpec[S, E, F], seed uint64, store ports.Store) *modelRun[S, E, F] {
	t.Helper()
	if machinetest.Walk(t, spec.states, spec.events, spec.kit.transition) == 0 {
		t.Fatalf("%s has no legal pair to walk", spec.name)
	}
	r := &modelRun[S, E, F]{
		t: t, spec: spec, seed: seed, inner: store, world: newModelWorld(t),
		label:   fmt.Sprintf("%s-%d-%d", spec.name, seed, time.Now().UnixNano()),
		stopped: map[int]bool{}, logs: map[int]*lockedBuffer{}, persisted: map[string]ports.TransitionRecord{},
		savedBy: map[recKey]int{}, hookPre: map[recKey][]ports.TransitionRecord{}, hookPost: map[recKey][]ports.TransitionRecord{},
		created: map[string]int{}, effects: map[string]int{}, inEffect: map[string]bool{},
		owner: map[string]string{}, parent: map[string]string{},
	}
	r.start()
	t.Cleanup(func() { r.stop(false) })
	return r
}

// start brings up a fresh berthd: a new actors kit over the same store, as after a restart.
func (r *modelRun[S, E, F]) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instance++
	logs := &lockedBuffer{}
	r.logs[r.instance] = logs
	r.cancel = cancel
	// A save that failed as berthd stopped may still have landed, so the store is the truth.
	for _, entity := range r.spec.entities {
		if rows := r.history(entity); len(rows) > 0 {
			r.persisted[entity] = rows[len(rows)-1]
		} else {
			delete(r.persisted, entity)
		}
	}
	base := r.spec.kit
	r.kit = &actors[S, E, F]{
		ctx: ctx, log: slog.New(slog.NewTextHandler(logs, nil)), store: modelStore{r.inner, r.onSave}, clock: testutil.NewClock(),
		kind: base.kind, initial: base.initial, final: base.final, transition: r.transition, run: r.runner,
		live:      map[string]*slot[S, E, F]{},
		saved:     r.onSaved,
		persisted: r.onPersisted,
	}
}

// stop cancels berthd's context and waits until every actor goroutine of the walk has ended; a
// stop mid walk lets hooks of the transitions in flight go untold.
func (r *modelRun[S, E, F]) stop(midWalk bool) {
	r.mu.Lock()
	cancel := r.cancel
	if midWalk {
		r.stopped[r.instance] = true
	}
	r.mu.Unlock()
	cancel()
	if !testutil.Eventually(func() bool { return r.actorGoroutines() == 0 }) {
		r.fault("%d actor goroutines still run after berthd stopped", r.actorGoroutines())
	}
}

func (r *modelRun[S, E, F]) restart(midWalk bool) {
	r.stop(midWalk)
	r.start()
	r.tracef("restart")
}

func (r *modelRun[S, E, F]) current() *actors[S, E, F] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kit
}

func (r *modelRun[S, E, F]) fault(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, fmt.Sprintf(format, args...))
}

func (r *modelRun[S, E, F]) tracef(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
	if len(r.trace) > 40 {
		r.trace = r.trace[len(r.trace)-40:]
	}
}

// failOnFaults stops the walk with every fault found, the seed and the last commands.
func (r *modelRun[S, E, F]) failOnFaults(where string) {
	r.t.Helper()
	r.mu.Lock()
	faults, trace := slices.Clone(r.faults), slices.Clone(r.trace)
	r.mu.Unlock()
	if len(faults) == 0 {
		return
	}
	r.t.Fatalf("%s walk, seed %d (replay with %s=%d), %s:\n  %s\nlast commands:\n  %s",
		r.spec.name, r.seed, machinetest.SeedEnv, r.seed, where, strings.Join(faults, "\n  "), strings.Join(trace, "\n  "))
}

func (r *modelRun[S, E, F]) token(entity, parent string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	tok := "c" + strconv.Itoa(r.seq)
	if parent != "" {
		tok = "f" + strconv.Itoa(r.seq)
		r.parent[tok] = parent
	}
	r.owner[tok] = entity
	return tok
}

func detail(e machine.Event) string {
	if d, ok := e.(machine.Detailed); ok {
		return d.Detail()
	}
	return ""
}

// transition wraps the machine's table: an actor must always start from its entity's saved
// state, which a second actor, or one that outlived its stop, would not.
func (r *modelRun[S, E, F]) transition(s S, e E) (S, []F, error) {
	tok := detail(e)
	r.mu.Lock()
	entity, ok := r.owner[tok]
	want := r.spec.kit.initial
	if p, found := r.persisted[entity]; found {
		want = S(p.To)
	}
	r.mu.Unlock()
	if !ok {
		r.fault("an actor applied %s with token %q that no command issued", e.Name(), tok)
	} else if s != want {
		r.fault("an actor of %s applied %s in %s while its saved state is %s: a second or stale actor", entity, e.Name(), s, want)
	}
	return r.spec.kit.transition(s, e)
}

func (r *modelRun[S, E, F]) onSave(rec ports.TransitionRecord, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil:
		r.persisted[rec.Entity] = rec
		r.savedBy[recKey{rec.Entity, rec.Version}] = r.instance
	case errors.Is(err, ports.ErrVersionConflict):
		r.faults = append(r.faults, fmt.Sprintf("version conflict saving %s v%d %s: two actors wrote one entity", rec.Entity, rec.Version, rec.Event))
	}
}

func (r *modelRun[S, E, F]) onPersisted(rec ports.TransitionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := recKey{rec.Entity, rec.Version}
	r.hookPre[k] = append(r.hookPre[k], rec)
}

func (r *modelRun[S, E, F]) onSaved(rec ports.TransitionRecord) {
	r.mu.Lock()
	k := recKey{rec.Entity, rec.Version}
	r.hookPost[k] = append(r.hookPost[k], rec)
	r.mu.Unlock()
	if r.spec.saved != nil {
		r.spec.saved(r.world, rec)
	}
}

// runner is the kit's runner factory, called once for every actor the kit starts.
func (r *modelRun[S, E, F]) runner(entity string) machine.Runner[E, F] {
	r.mu.Lock()
	r.created[entity]++
	kit := r.kit
	r.mu.Unlock()
	return func(_ context.Context, f F) ([]E, error) {
		r.mu.Lock()
		if r.inEffect[entity] {
			r.faults = append(r.faults, fmt.Sprintf("two effects of %s ran at once", entity))
		}
		r.inEffect[entity] = true
		n := r.effects[entity]
		r.effects[entity]++
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			r.inEffect[entity] = false
			r.mu.Unlock()
		}()
		// Echo the step's input, so the step can prove it got its own data back.
		in := kit.input(entity)
		kit.output(entity, in)
		rng := machinetest.Rand(r.seed, entity+"#"+strconv.Itoa(n))
		if r.jitter.Load() {
			time.Sleep(time.Duration(rng.IntN(200)) * time.Microsecond)
		}
		var follow []E
		if r.spec.effect != nil {
			follow = r.spec.effect(r.world, entity, f, rng.IntN)
		}
		parent, _ := in.(string)
		if parent == "" {
			parent = "?"
		}
		for i := range follow {
			follow[i] = r.spec.note(follow[i], r.token(entity, parent))
		}
		return follow, nil
	}
}

func (r *modelRun[S, E, F]) until(e E) []string {
	if r.spec.until != nil {
		if u := r.spec.until(e); u != nil {
			return u
		}
	}
	return []string{e.Name()}
}

type modelStep[E machine.Event] struct {
	entity, token string
	event         E
	until         []string
	rec           ports.TransitionRecord
	out           any
	err           error
	cancellable   bool
	// from and to are the entity's saved versions before the step began and after it returned,
	// the window in which its event met the entity.
	from, to int64
}

// do runs one command as a shell call would: a step with the token as its input.
func (r *modelRun[S, E, F]) do(ctx context.Context, kit *actors[S, E, F], entity string, e E) modelStep[E] {
	tok := r.token(entity, "")
	e = r.spec.note(e, tok)
	st := modelStep[E]{entity: entity, token: tok, event: e, until: r.until(e), from: r.version(entity)}
	pprof.Do(ctx, pprof.Labels("berthmodel", r.label), func(ctx context.Context) {
		st.rec, st.out, st.err = step(ctx, kit, entity, e, any(tok), st.until...)
	})
	st.to = r.version(entity)
	r.tracef("%s %s %s -> %s %v", tok, entity, e.Name(), st.rec.Event, st.err)
	return st
}

func (r *modelRun[S, E, F]) version(entity string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persisted[entity].Version
}

func (r *modelRun[S, E, F]) history(entity string) []ports.TransitionRecord {
	rows, err := r.inner.History(context.Background(), entity, 1<<20)
	if err != nil {
		r.t.Fatalf("history of %s: %v", entity, err)
	}
	slices.Reverse(rows)
	return rows
}

func (r *modelRun[S, E, F]) state(entity string) S {
	st, found, err := r.inner.LoadState(context.Background(), entity)
	if err != nil {
		r.t.Fatalf("load %s: %v", entity, err)
	}
	if !found {
		return r.spec.kit.initial
	}
	return S(st.State)
}

func (r *modelRun[S, E, F]) final(s S) bool { return slices.Contains(r.spec.kit.final, s) }

func (r *modelRun[S, E, F]) sample(name string) (E, bool) {
	for _, e := range r.spec.events {
		if e.Name() == name {
			return e, true
		}
	}
	var zero E
	return zero, false
}

// pick chooses an event for an entity resting in s: mostly one the table accepts, sometimes any.
func (r *modelRun[S, E, F]) pick(rng *rand.Rand, s S) E {
	if legal := machinetest.Legal(s, r.spec.events, r.spec.kit.transition); len(legal) > 0 && rng.IntN(4) != 0 {
		return legal[rng.IntN(len(legal))]
	}
	return r.spec.events[rng.IntN(len(r.spec.events))]
}

// actorGoroutines counts the walk's actor goroutines, which inherit the label of the step that
// started them.
func (r *modelRun[S, E, F]) actorGoroutines() int {
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		r.t.Fatalf("goroutine profile: %v", err)
	}
	n := 0
	for _, rec := range strings.Split(buf.String(), "\n\n") {
		if !strings.Contains(rec, `"berthmodel":"`+r.label+`"`) || !strings.Contains(rec, "machine.(*Actor[") || !strings.Contains(rec, ").Run+") {
			continue
		}
		// The first record follows the profile's header line.
		for line := range strings.SplitSeq(rec, "\n") {
			if count, _, ok := strings.Cut(line, " @ "); ok {
				if c, err := strconv.Atoi(count); err == nil {
					n += c
				}
				break
			}
		}
	}
	return n
}

// liveEntities lists the entities the current kit holds an actor for.
func (r *modelRun[S, E, F]) liveEntities() []string {
	kit := r.current()
	kit.mu.Lock()
	defer kit.mu.Unlock()
	var out []string
	for e := range kit.live {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// settle waits until the walk is at rest and checks what must hold there: every hook told, no
// actor and no goroutine for an entity in a final state, one goroutine per live actor, and
// nothing an entity in its state must not hold.
func (r *modelRun[S, E, F]) settle(where string, touched ...string) {
	r.t.Helper()
	rows := r.rows(touched...)
	var problems []string
	// Polled finely: the saved hook runs just after the step's waiter hears its outcome.
	ok := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Microsecond) {
		if problems = r.restProblems(rows); len(problems) == 0 {
			ok = true
			break
		}
	}
	if !ok {
		for _, p := range problems {
			r.fault("%s", p)
		}
	}
	r.checkHistory(rows)
	r.failOnFaults(where)
}

// rows reads the saved transitions, oldest first, of the touched entities, or of all when none
// is named; a sequential step touches one, so the others keep the rows read before.
func (r *modelRun[S, E, F]) rows(touched ...string) map[string][]ports.TransitionRecord {
	if len(touched) == 0 || r.cache == nil {
		touched = r.spec.entities
	}
	out := map[string][]ports.TransitionRecord{}
	for _, entity := range r.spec.entities {
		if slices.Contains(touched, entity) {
			out[entity] = r.history(entity)
		} else {
			out[entity] = r.cache[entity]
		}
	}
	r.cache = out
	return out
}

// last is an entity's state by its rows.
func (r *modelRun[S, E, F]) last(rows []ports.TransitionRecord) S {
	if len(rows) == 0 {
		return r.spec.kit.initial
	}
	return S(rows[len(rows)-1].To)
}

func (r *modelRun[S, E, F]) restProblems(rows map[string][]ports.TransitionRecord) []string {
	var out []string
	live := r.liveEntities()
	for _, entity := range live {
		if s := r.last(rows[entity]); r.final(s) {
			out = append(out, fmt.Sprintf("%s rests in final state %s with a live actor", entity, s))
		}
	}
	if g := r.actorGoroutines(); g != len(live) {
		out = append(out, fmt.Sprintf("%d actor goroutines for %d live actors %v", g, len(live), live))
	}
	r.mu.Lock()
	stopped := maps.Clone(r.stopped)
	r.mu.Unlock()
	for _, entity := range r.spec.entities {
		for _, row := range rows[entity] {
			k := recKey{row.Entity, row.Version}
			r.mu.Lock()
			pre, post, by, known := len(r.hookPre[k]), len(r.hookPost[k]), r.savedBy[k], r.savedBy[k] != 0
			r.mu.Unlock()
			// A transition saved while berthd stopped may miss its hooks, never get two.
			relaxed := !known || stopped[by]
			if relaxed && (pre > 1 || post > 1) || !relaxed && (pre != 1 || post != 1) {
				out = append(out, fmt.Sprintf("%s v%d %s: persisted hook told %d times, saved hook %d", row.Entity, row.Version, row.Event, pre, post))
			}
		}
		if r.spec.idle != nil {
			if err := r.spec.idle(r.world, entity, r.last(rows[entity])); err != nil {
				out = append(out, err.Error())
			}
		}
	}
	return out
}

// checkHistory replays every entity's saved rows through the table: each row is legal from the
// one before, versions count up, the saved state is the last row, each token is applied once, a
// follow up comes straight after its command, and every hook told matches a saved row.
func (r *modelRun[S, E, F]) checkHistory(all map[string][]ports.TransitionRecord) {
	seen := map[string]bool{}
	saved := map[recKey]ports.TransitionRecord{}
	for _, entity := range r.spec.entities {
		rows := all[entity]
		state, prevTok := r.spec.kit.initial, ""
		for i, row := range rows {
			saved[recKey{row.Entity, row.Version}] = row
			e, ok := r.sample(row.Event)
			switch {
			case !ok:
				r.fault("%s v%d: unknown event %s", entity, row.Version, row.Event)
			case row.Version != int64(i+1):
				r.fault("%s: row %d has version %d", entity, i, row.Version)
			case S(row.From) != state:
				r.fault("%s v%d %s: from %s, but the model is in %s", entity, row.Version, row.Event, row.From, state)
			default:
				next, _, err := r.spec.kit.transition(state, e)
				if err != nil || S(row.To) != next {
					r.fault("%s v%d %s: %s to %s, the model says %s (%v)", entity, row.Version, row.Event, row.From, row.To, next, err)
				}
			}
			state = S(row.To)
			r.mu.Lock()
			owner, parent := r.owner[row.Detail], r.parent[row.Detail]
			prevParent := r.parent[prevTok]
			r.mu.Unlock()
			if owner != entity {
				r.fault("%s v%d %s carries token %q of %q", entity, row.Version, row.Event, row.Detail, owner)
			}
			if seen[row.Detail] {
				r.fault("%s v%d %s: token %s applied twice", entity, row.Version, row.Event, row.Detail)
			}
			seen[row.Detail] = true
			if parent != "" && prevTok != parent && prevParent != parent {
				r.fault("%s v%d %s: follow up of %s came after %s, another step got between", entity, row.Version, row.Event, parent, prevTok)
			}
			prevTok = row.Detail
		}
		st, found, err := r.inner.LoadState(context.Background(), entity)
		switch {
		case err != nil:
			r.fault("load %s: %v", entity, err)
		case len(rows) == 0 && found:
			r.fault("%s has a state row and no events", entity)
		case len(rows) > 0 && (S(st.State) != state || st.Version != int64(len(rows))):
			r.fault("%s is saved as %s v%d, the model says %s v%d", entity, st.State, st.Version, state, len(rows))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, hooks := range []map[recKey][]ports.TransitionRecord{r.hookPre, r.hookPost} {
		for k, recs := range hooks {
			row, ok := saved[k]
			for _, rec := range recs {
				if !ok || rec.Event != row.Event || rec.From != row.From || rec.To != row.To || rec.Detail != row.Detail {
					r.faults = append(r.faults, fmt.Sprintf("a hook was told of %s v%d %s %s to %s, which is not the saved row", rec.Entity, rec.Version, rec.Event, rec.From, rec.To))
				}
			}
		}
	}
	for i, logs := range r.logs {
		if s := logs.String(); s != "" && !r.stopped[i] {
			r.faults = append(r.faults, "an actor stopped without berthd stopping: "+s)
		}
	}
}

// walk issues steps one at a time, predicting each outcome from the model before checking it.
func (r *modelRun[S, E, F]) walk(steps int) {
	r.t.Helper()
	rng := machinetest.Rand(r.seed, "walk")
	model := map[string]S{}
	for _, entity := range r.spec.entities {
		model[entity] = r.state(entity)
	}
	for i := range steps {
		if rng.IntN(50) == 0 {
			r.restart(false)
			continue
		}
		entity := r.spec.entities[rng.IntN(len(r.spec.entities))]
		s := model[entity]
		e := r.pick(rng, s)
		kit := r.current()
		saved, _, err := r.inner.LoadState(context.Background(), entity)
		if err != nil {
			r.t.Fatal(err)
		}
		r.mu.Lock()
		created := r.created[entity]
		r.mu.Unlock()
		hadActor := slices.Contains(r.liveEntities(), entity)

		st := r.do(context.Background(), kit, entity, e)

		rows := r.history(entity)[saved.Version:]
		next, _, terr := r.spec.kit.transition(s, e)
		if terr != nil {
			if !errors.Is(st.err, machine.ErrIllegal) || len(rows) != 0 {
				r.fault("step %d: %s in %s is illegal, got %v and %d rows", i, e.Name(), s, st.err, len(rows))
			}
		} else {
			r.checkLegalStep(i, st, s, next, rows)
			if len(rows) > 0 {
				model[entity] = S(rows[len(rows)-1].To)
			}
		}
		if got := r.state(entity); got != model[entity] {
			r.fault("step %d: %s is saved as %s, the model says %s", i, entity, got, model[entity])
		}
		r.mu.Lock()
		started := r.created[entity] - created
		r.mu.Unlock()
		if want := map[bool]int{true: 0, false: 1}[hadActor]; started != want {
			r.fault("step %d: %s started %d actors, want %d (it had one: %v)", i, entity, started, want, hadActor)
		}
		// An ended actor is forgotten before the step waiting on it returns.
		if r.final(model[entity]) && slices.Contains(r.liveEntities(), entity) {
			r.fault("step %d: %s ended in %s but its actor is still live when the step returned", i, entity, model[entity])
		}
		r.settle(fmt.Sprintf("after step %d (%s %s)", i, entity, e.Name()), entity)
	}
}

func (r *modelRun[S, E, F]) checkLegalStep(i int, st modelStep[E], s, next S, rows []ports.TransitionRecord) {
	if st.err != nil || len(rows) == 0 {
		r.fault("step %d: %s in %s is legal, got %v and %d rows", i, st.event.Name(), s, st.err, len(rows))
		return
	}
	if rows[0].Detail != st.token || S(rows[0].From) != s || S(rows[0].To) != next {
		r.fault("step %d: first row %s %s to %s (%s), want %s %s to %s (%s)", i, rows[0].Event, rows[0].From, rows[0].To, rows[0].Detail, st.event.Name(), s, next, st.token)
	}
	for _, row := range rows {
		if slices.Contains(st.until, row.Event) {
			if row.Version != st.rec.Version || row.Event != st.rec.Event || row.Detail != st.rec.Detail {
				r.fault("step %d: returned %s v%d, want the first awaited row %s v%d", i, st.rec.Event, st.rec.Version, row.Event, row.Version)
			}
			break
		}
	}
	if st.out != nil && st.out != any(st.token) {
		r.fault("step %d: got output %v, want its own input %s", i, st.out, st.token)
	}
}

// concurrent runs walkers goroutines of steps each on few entities at once, with a share of
// callers giving up mid step; with stop, berthd stops part way and starts again after.
func (r *modelRun[S, E, F]) concurrent(phase, walkers, perWalker int, stop bool) {
	r.t.Helper()
	r.jitter.Store(true)
	defer r.jitter.Store(false)
	kit := r.current()
	var (
		mu    sync.Mutex
		done  int
		steps []modelStep[E]
		wg    sync.WaitGroup
	)
	stopAt := -1
	if stop {
		stopAt = 1 + machinetest.Rand(r.seed, fmt.Sprintf("stop-%d", phase)).IntN(walkers*perWalker)
	}
	stopped := make(chan struct{})
	for g := range walkers {
		wg.Go(func() {
			rng := machinetest.Rand(r.seed, fmt.Sprintf("walker-%d-%d", phase, g))
			for range perWalker {
				entity := r.spec.entities[rng.IntN(len(r.spec.entities))]
				r.mu.Lock()
				s := r.spec.kit.initial
				if p, ok := r.persisted[entity]; ok {
					s = S(p.To)
				}
				r.mu.Unlock()
				e := r.pick(rng, s)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				cancellable := rng.IntN(6) == 0
				if cancellable {
					wait := time.Duration(rng.IntN(300)) * time.Microsecond
					go func() { time.Sleep(wait); cancel() }()
				}
				st := r.do(ctx, kit, entity, e)
				cancel()
				st.cancellable = cancellable
				mu.Lock()
				steps = append(steps, st)
				done++
				if done == stopAt {
					go func() { r.stop(true); close(stopped) }()
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if stop {
		<-stopped
		r.stop(true)
		r.start()
	}
	r.checkSteps(steps, stop)
	r.settle(fmt.Sprintf("after concurrent phase %d (stop %v)", phase, stop))
}

// checkSteps checks each concurrent step's answer: an outcome is a saved row of its own command,
// awaited, returned to no other step; an illegal one saved nothing; berthd's own errors come
// only from a stop, and the caller's only from a cancel.
func (r *modelRun[S, E, F]) checkSteps(steps []modelStep[E], stop bool) {
	rows := map[recKey]ports.TransitionRecord{}
	tokens := map[string]bool{}
	all := r.rows()
	for _, entity := range r.spec.entities {
		for _, row := range all[entity] {
			rows[recKey{row.Entity, row.Version}] = row
			tokens[row.Detail] = true
		}
	}
	returned := map[recKey]string{}
	for _, st := range steps {
		switch {
		case st.err == nil:
			k := recKey{st.rec.Entity, st.rec.Version}
			row, ok := rows[k]
			r.mu.Lock()
			parent := r.parent[row.Detail]
			r.mu.Unlock()
			switch {
			case !ok || row.Event != st.rec.Event || row.Detail != st.rec.Detail:
				r.fault("%s %s returned %s v%d, which is not a saved row", st.token, st.event.Name(), st.rec.Event, st.rec.Version)
			case !slices.Contains(st.until, row.Event):
				r.fault("%s %s returned %s, which it does not wait on", st.token, st.event.Name(), row.Event)
			case row.Detail != st.token && parent != st.token:
				r.fault("%s %s returned %s v%d of %s: another step's outcome", st.token, st.event.Name(), row.Event, row.Version, row.Detail)
			}
			if other, dup := returned[k]; dup {
				r.fault("%s and %s both returned %s v%d", other, st.token, st.rec.Entity, st.rec.Version)
			}
			returned[k] = st.token
			if st.out != nil && st.out != any(st.token) {
				r.fault("%s got output %v: another step's data", st.token, st.out)
			}
		case errors.Is(st.err, machine.ErrIllegal):
			if tokens[st.token] {
				r.fault("%s %s answered illegal but was saved", st.token, st.event.Name())
			}
			if !stop && r.legalThroughout(all[st.entity], st) {
				r.fault("%s %s on %s answered illegal, yet the table accepts it in every state between v%d and v%d", st.token, st.event.Name(), st.entity, st.from, st.to)
			}
		case errors.Is(st.err, api.ErrUnavailable):
			if !stop {
				r.fault("%s %s answered unavailable with berthd running", st.token, st.event.Name())
			}
		case errors.Is(st.err, context.Canceled) && st.cancellable:
		case errors.Is(st.err, context.DeadlineExceeded):
			r.fault("%s %s on %s hung for 10 seconds", st.token, st.event.Name(), st.entity)
		default:
			r.fault("%s %s: unexpected error %v", st.token, st.event.Name(), st.err)
		}
	}
}

// legalThroughout reports whether the table accepts st's event in every state its entity was
// in while the step ran, so no answer of illegal can be right.
func (r *modelRun[S, E, F]) legalThroughout(rows []ports.TransitionRecord, st modelStep[E]) bool {
	if st.from > int64(len(rows)) || st.to > int64(len(rows)) {
		return false
	}
	states := []S{r.last(rows[:st.from])}
	for _, row := range rows[st.from:st.to] {
		states = append(states, S(row.To))
	}
	for _, s := range states {
		if _, _, err := r.spec.kit.transition(s, st.event); err != nil {
			return false
		}
	}
	return true
}

// modelStore wraps the real store and tells the walk of every save.
type modelStore struct {
	ports.Store
	onSave func(ports.TransitionRecord, error)
}

func (s modelStore) SaveTransition(ctx context.Context, rec ports.TransitionRecord) error {
	err := s.Store.SaveTransition(ctx, rec)
	s.onSave(rec, err)
	return err
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
