package berthd

import (
	"context"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/ports"
	"berth/internal/core/session"
)

// sessionStream is one open WatchSessions stream. wake holds at most one pending wake, so an
// actor never waits.
type sessionStream struct {
	wake chan struct{}
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
