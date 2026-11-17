// Package ports defines what the core needs from the outside world. Adapters implement these.
package ports

import (
	"context"
	"errors"
	"time"
)

// Clock is the only source of time in the core, so tests control it.
type Clock interface {
	Now() time.Time
}

// TransitionRecord is one state change, written together with the entity's new state.
type TransitionRecord struct {
	Entity  string
	Kind    string
	Version int64
	Event   string
	From    string
	To      string
	At      time.Time
	Detail  string
}

// EntityState is the persisted current state of one entity.
type EntityState struct {
	Entity  string
	Kind    string
	State   string
	Version int64
}

// ErrVersionConflict means another writer moved the entity first.
var ErrVersionConflict = errors.New("version conflict")

// Store persists state machines. SaveTransition writes the new state and its event row atomically.
type Store interface {
	SaveTransition(ctx context.Context, rec TransitionRecord) error
	LoadState(ctx context.Context, entity string) (EntityState, bool, error)
	History(ctx context.Context, entity string, limit int) ([]TransitionRecord, error)
}

// RequestLog makes mutating calls idempotent by remembering the first response per principal
// and request ID, so a reply stored for one caller is never returned to another.
type RequestLog interface {
	Recall(ctx context.Context, principal, requestID string) (method string, response []byte, found bool, err error)
	Remember(ctx context.Context, principal, requestID, method string, response []byte, at time.Time) error
}
