package berthd

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// slowLoadStore holds every LoadState of one entity until release is closed.
type slowLoadStore struct {
	*testutil.Store
	entity  string
	loading chan struct{}
	release chan struct{}
	once    sync.Once
	loads   atomic.Int32
}

func (s *slowLoadStore) LoadState(ctx context.Context, entity string) (ports.EntityState, bool, error) {
	if entity == s.entity {
		s.loads.Add(1)
		s.once.Do(func() { close(s.loading) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ports.EntityState{}, false, ctx.Err()
		}
	}
	return s.Store.LoadState(ctx, entity)
}

type slowState string

type loadEvent struct{}

func (loadEvent) Name() string { return "X" }

func loadTransition(s slowState, _ loadEvent) (slowState, []string, error) { return s, nil, nil }

// One slow load must not hold up the other entities of its machine, and callers that arrive
// while it runs must share the one actor it starts rather than load a second.
func TestSlowLoadBlocksOnlyItsOwnEntity(t *testing.T) {
	store := &slowLoadStore{Store: testutil.NewStore(), entity: "load/slow", loading: make(chan struct{}), release: make(chan struct{})}
	kit := &actors[slowState, loadEvent, string]{
		ctx: t.Context(), log: slog.New(slog.DiscardHandler), store: store, clock: testutil.NewClock(),
		kind: "load", initial: "A", transition: loadTransition,
		run: func(string) machine.Runner[loadEvent, string] {
			return func(context.Context, string) ([]loadEvent, error) { return nil, nil }
		},
		live: map[string]*slot[slowState, loadEvent, string]{},
	}
	slow := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := step(t.Context(), kit, "load/slow", loadEvent{}, nil, "X")
			slow <- err
		}()
	}
	<-store.loading

	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	fast := make(chan error, 1)
	go func() {
		_, _, err := step(t.Context(), kit, "load/fast", loadEvent{}, nil, "X")
		fast <- err
	}()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a step on another entity waited on the slow load")
	}

	release()
	for range 2 {
		if err := <-slow; err != nil {
			t.Fatalf("slow step: %v", err)
		}
	}
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("the slow entity was loaded %d times, want once", n)
	}
	if st, _, _ := store.Store.LoadState(t.Context(), "load/slow"); st.Version != 2 {
		t.Fatalf("slow entity version %d, want 2 from one actor", st.Version)
	}
}
