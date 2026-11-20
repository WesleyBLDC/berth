package berthd

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"berth/internal/core/daemon"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// The daemon runs on the machine kit's Actor directly, not on the actors kit, so its walk drives
// one actor: each step sends a command and waits for its outcome, and the actor restarts over the
// same store now and then, as berthd does.
func TestModelDaemon(t *testing.T) {
	seed := machinetest.Seed(t)
	rng := machinetest.Rand(seed, "daemon")
	store := sqliteStore(t)
	until := map[daemon.EventKind][]string{
		daemon.Boot:          names(daemon.Recovered, daemon.Failed),
		daemon.StopRequested: names(daemon.Drained, daemon.Failed),
	}
	var (
		mu         sync.Mutex
		pre, post  = map[int64]int{}, map[int64]int{}
		outcome    = make(chan error, 64)
		awaited    []string
		effectSeq  int
		modelState = daemon.Starting
		rows       int64
	)
	tell := func(rec ports.TransitionRecord, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil || slices.Contains(awaited, rec.Event) {
			outcome <- err
		}
	}
	start := func() (*machine.Actor[daemon.State, daemon.Event, daemon.Effect], context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(t.Context())
		a, err := machine.NewActor(ctx, machine.Config[daemon.State, daemon.Event, daemon.Effect]{
			Entity: daemon.Entity, Kind: daemon.Kind, Initial: daemon.Starting, Transition: daemon.Transition,
			Store: store, Clock: testutil.NewClock(),
			Run: func(_ context.Context, f daemon.Effect) ([]daemon.Event, error) {
				mu.Lock()
				effectSeq++
				pick := machinetest.Rand(seed, "effect-"+strconv.Itoa(effectSeq)).IntN(2)
				mu.Unlock()
				switch f.Kind {
				case daemon.Recover:
					return []daemon.Event{{Kind: []daemon.EventKind{daemon.Recovered, daemon.Failed}[pick]}}, nil
				case daemon.StopServing:
					return []daemon.Event{{Kind: []daemon.EventKind{daemon.Drained, daemon.Failed}[pick]}}, nil
				case daemon.StartServing, daemon.Exit:
				}
				return nil, nil
			},
			Rejected: func(err error) { tell(ports.TransitionRecord{}, err) },
			Saved: func(rec ports.TransitionRecord) {
				mu.Lock()
				defer mu.Unlock()
				pre[rec.Version]++
			},
			Applied: func(rec ports.TransitionRecord) {
				mu.Lock()
				post[rec.Version]++
				mu.Unlock()
				tell(rec, nil)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		return a, cancel, done
	}
	actor, cancel, done := start()
	for i := range machinetest.Steps(t, 200) {
		if rng.IntN(25) == 0 {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("seed %d step %d: actor stopped with %v", seed, i, err)
			}
			actor, cancel, done = start()
			if got := actor.State(); got != modelState {
				t.Fatalf("seed %d step %d: restarted in %s, the model says %s", seed, i, got, modelState)
			}
		}
		e := daemon.AllEvents[rng.IntN(len(daemon.AllEvents))]
		if legal := machinetest.Legal(modelState, daemon.AllEvents, daemon.Transition); rng.IntN(4) != 0 {
			e = legal[rng.IntN(len(legal))]
		}
		next, _, terr := daemon.Transition(modelState, e)
		mu.Lock()
		awaited = until[e.Kind]
		if awaited == nil {
			awaited = names(e.Kind)
		}
		mu.Unlock()
		if err := actor.Send(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		var err error
		select {
		case err = <-outcome:
		case <-time.After(5 * time.Second):
			t.Fatalf("seed %d step %d: %s in %s never settled", seed, i, e.Kind, modelState)
		}
		// Only the newest rows: a step saves at most a command and its follow up.
		recent, herr := store.History(t.Context(), daemon.Entity, 4)
		if herr != nil {
			t.Fatal(herr)
		}
		slices.Reverse(recent)
		history := make([]ports.TransitionRecord, rows, rows+4)
		for _, row := range recent {
			if row.Version > rows {
				history = append(history, row)
			}
		}
		if terr != nil {
			if !errors.Is(err, machine.ErrIllegal) || int64(len(history)) != rows {
				t.Fatalf("seed %d step %d: illegal %s in %s answered %v with %d new rows", seed, i, e.Kind, modelState, err, int64(len(history))-rows)
			}
			continue
		}
		if err != nil || int64(len(history)) <= rows || daemon.State(history[rows].To) != next {
			t.Fatalf("seed %d step %d: %s in %s answered %v, rows %v", seed, i, e.Kind, modelState, err, history[rows:])
		}
		for _, row := range history[rows:] {
			want, _, err := daemon.Transition(daemon.State(row.From), daemon.Event{Kind: daemon.EventKind(row.Event)})
			if err != nil || daemon.State(row.From) != modelState || daemon.State(row.To) != want {
				t.Fatalf("seed %d step %d: row v%d %s %s to %s, the model is in %s", seed, i, row.Version, row.Event, row.From, row.To, modelState)
			}
			modelState = want
		}
		rows = int64(len(history))
		if st, _, _ := store.LoadState(t.Context(), daemon.Entity); daemon.State(st.State) != modelState || st.Version != rows {
			t.Fatalf("seed %d step %d: saved %s v%d, the model says %s v%d", seed, i, st.State, st.Version, modelState, rows)
		}
		mu.Lock()
		for v := int64(1); v <= rows; v++ {
			if pre[v] != 1 || post[v] != 1 {
				mu.Unlock()
				t.Fatalf("seed %d step %d: v%d told Saved %d and Applied %d times", seed, i, v, pre[v], post[v])
			}
		}
		if len(pre) != int(rows) || len(post) != int(rows) {
			mu.Unlock()
			t.Fatalf("seed %d step %d: hooks told of %d and %d transitions, %d saved", seed, i, len(pre), len(post), rows)
		}
		mu.Unlock()
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("seed %d: actor stopped with %v", seed, err)
	}
}
