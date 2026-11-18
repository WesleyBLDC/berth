package berthd

import (
	"context"
	"sync"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/ports"
)

const (
	// watchBacklog is how many events a stream replays; a client further behind gets a snapshot
	// instead.
	watchBacklog = 1000
	// maxWatchers bounds the open streams of one kind, since each holds a goroutine.
	maxWatchers = 16
)

// watchSet is the open streams of one kind. A saved transition wakes each one, and close ends
// them all when berthd stops serving.
type watchSet struct {
	mu        sync.Mutex
	streams   map[*eventWatcher]struct{}
	closing   chan struct{}
	closeOnce sync.Once
}

// eventWatcher is one open stream. wake holds at most one pending wake, so an actor never waits.
type eventWatcher struct {
	wake chan struct{}
}

func newWatchSet() *watchSet {
	return &watchSet{streams: map[*eventWatcher]struct{}{}, closing: make(chan struct{})}
}

func (ws *watchSet) add() (*eventWatcher, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.streams) >= maxWatchers {
		return nil, api.ErrTooManyWatchers
	}
	w := &eventWatcher{wake: make(chan struct{}, 1)}
	ws.streams[w] = struct{}{}
	return w, nil
}

func (ws *watchSet) remove(w *eventWatcher) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	delete(ws.streams, w)
}

// wakeAll runs on an actor's goroutine after a saved transition, so it never blocks: a watcher
// that has a wake pending already will read this row too.
func (ws *watchSet) wakeAll() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for w := range ws.streams {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// close ends every open stream, and any opened later.
func (ws *watchSet) close() {
	ws.closeOnce.Do(func() { close(ws.closing) })
}

// feed is what one kind of stream reads: the event rows after a cursor, a snapshot with the
// cursor it is consistent with, and the message for one row, or false for a row it skips.
type feed[M any] struct {
	events   ports.EventLog
	backlog  int
	rows     func(ctx context.Context, after int64, limit int) ([]ports.EventRecord, error)
	snapshot func(ctx context.Context, reason berthv1.SnapshotReason) (M, int64, error)
	change   func(r ports.EventRecord) (M, bool)
}

// serveStream serves one stream from the event table, which is the only queue: an actor saving a
// transition only wakes the watchers, and each reads the rows after its own cursor at its
// client's pace, so a slow client holds one goroutine and one page of rows, never an actor.
func serveStream[M any](ctx context.Context, ws *watchSet, f feed[M], after int64, send func(M) error) error {
	w, err := ws.add()
	if err != nil {
		return err
	}
	defer ws.remove(w)
	cursor, reason := after, berthv1.SnapshotReason_SNAPSHOT_REASON_UNSPECIFIED
	if after == 0 {
		reason = berthv1.SnapshotReason_SNAPSHOT_REASON_START
	} else if last, err := f.events.LastEventID(ctx); err != nil {
		return err
	} else if after > last {
		reason = berthv1.SnapshotReason_SNAPSHOT_REASON_UNKNOWN_CURSOR
	}
	if reason != berthv1.SnapshotReason_SNAPSHOT_REASON_UNSPECIFIED {
		if cursor, err = sendSnapshot(ctx, f, reason, send); err != nil {
			return err
		}
	}
	for {
		if cursor, err = catchUp(ctx, ws, f, cursor, send); err != nil {
			return err
		}
		select {
		case <-w.wake:
		case <-ws.closing:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func sendSnapshot[M any](ctx context.Context, f feed[M], reason berthv1.SnapshotReason, send func(M) error) (int64, error) {
	m, cursor, err := f.snapshot(ctx, reason)
	if err != nil {
		return 0, err
	}
	return cursor, send(m)
}

// catchUp sends every change after cursor, or a snapshot when more rows than the backlog wait,
// and returns the new cursor.
func catchUp[M any](ctx context.Context, ws *watchSet, f feed[M], cursor int64, send func(M) error) (int64, error) {
	for {
		select {
		case <-ws.closing:
			return cursor, nil
		default:
		}
		rows, err := f.rows(ctx, cursor, f.backlog+1)
		if err != nil {
			return cursor, err
		}
		if len(rows) > f.backlog {
			if cursor, err = sendSnapshot(ctx, f, berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND, send); err != nil {
				return cursor, err
			}
			continue
		}
		if len(rows) == 0 {
			return cursor, nil
		}
		for _, r := range rows {
			if m, ok := f.change(r); ok {
				if err := send(m); err != nil {
					return cursor, err
				}
			}
			cursor = r.ID
		}
	}
}
