package testutil

import (
	"context"
	"slices"
	"strings"
	"sync"

	"berth/internal/core/ports"
)

// Store is an in memory ports.Store that also records the order of saves.
type Store struct {
	mu     sync.Mutex
	states map[string]ports.EntityState
	events []ports.TransitionRecord
	// FailSave makes the next SaveTransition fail.
	FailSave error
}

func NewStore() *Store { return &Store{states: map[string]ports.EntityState{}} }

func (s *Store) SaveTransition(_ context.Context, rec ports.TransitionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.FailSave; err != nil {
		s.FailSave = nil
		return err
	}
	if s.states[rec.Entity].Version != rec.Version-1 {
		return ports.ErrVersionConflict
	}
	s.states[rec.Entity] = ports.EntityState{Entity: rec.Entity, Kind: rec.Kind, State: rec.To, Version: rec.Version}
	s.events = append(s.events, rec)
	return nil
}

func (s *Store) LoadState(_ context.Context, entity string) (ports.EntityState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[entity]
	return st, ok, nil
}

func (s *Store) History(_ context.Context, entity string, limit int) ([]ports.TransitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.TransitionRecord
	for _, e := range slices.Backward(s.events) {
		if e.Entity == entity && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Store) EntitiesOfKind(_ context.Context, kind string) ([]ports.EntityState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.EntityState
	for _, st := range s.states {
		if st.Kind == kind {
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b ports.EntityState) int { return strings.Compare(a.Entity, b.Entity) })
	return out, nil
}

// Events returns every saved transition in order.
func (s *Store) Events() []ports.TransitionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}
