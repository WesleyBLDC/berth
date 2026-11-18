package berthd

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/ports"
	"berth/internal/core/session"
)

// WatchSessions streams session changes from the event table (see serveStream).
func (b *browsers) WatchSessions(ctx context.Context, after int64, send func(*berthv1.WatchSessionsResponse) error) error {
	return serveStream(ctx, b.streams, feed[*berthv1.WatchSessionsResponse]{
		events: b.Events, backlog: b.backlog, snapshot: b.snapshot,
		rows: func(ctx context.Context, after int64, limit int) ([]ports.EventRecord, error) {
			return b.Events.EventsAfter(ctx, session.Kind, after, limit)
		},
		change: func(r ports.EventRecord) (*berthv1.WatchSessionsResponse, bool) { return b.change(r), true },
	}, after, send)
}

// snapshot holds every live session and the cursor it is consistent with. The cursor is
// read before the states, so no state is older than it; a change after it that the snapshot
// already shows is applied again, which is harmless since a change carries the whole state. Only
// the live sessions' rows are read, so a snapshot costs the same however long the history.
func (b *browsers) snapshot(ctx context.Context, reason berthv1.SnapshotReason) (*berthv1.WatchSessionsResponse, int64, error) {
	last, err := b.Events.LastEventID(ctx)
	if err != nil {
		return nil, 0, err
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
			return nil, 0, err
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
	return &berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Snapshot{Snapshot: snap}}, last, nil
}

// leftovers is every live session saved as Leftover, as a need, for WatchNeeds' snapshot; like
// snapshot it reads only the live sessions' rows.
func (b *browsers) leftovers(ctx context.Context) ([]*berthv1.Need, error) {
	b.mu.Lock()
	ids := make([]string, 0, len(b.live))
	for id := range b.live {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	slices.Sort(ids)
	var out []*berthv1.Need
	for _, id := range ids {
		saved, found, err := b.Store.LoadState(ctx, session.Entity(id))
		if err != nil {
			return nil, err
		}
		if found && session.State(saved.State) == session.Leftover {
			out = append(out, b.leftoverNeed(id))
		}
	}
	return out, nil
}

// leftoverNeed is a LEFTOVER need for a session with the facts berthd still holds about it:
// the owner's name as last seen and the browser's kind.
func (b *browsers) leftoverNeed(id string) *berthv1.Need {
	need := &berthv1.Need{Id: session.Entity(id), Kind: berthv1.NeedKind_NEED_KIND_LEFTOVER}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.live[id]
	if !ok {
		t, ok = b.ended[id]
	}
	if ok {
		need.OwnerName, need.Browser = t.obs.Owner.Name, kindProto[t.obs.Signature.Kind]
	}
	return need
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

// closeWatches ends every open stream, and any opened later, when berthd stops serving.
func (b *browsers) closeWatches() { b.streams.close() }
