package berthd

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/core/secret"
	"berth/internal/core/session"
)

type needsDeps struct {
	Events ports.EventLog
	List   ports.EntityLister
	// Grant reads a grant's row, for the name of the secret it waits to be granted.
	Grant func(ctx context.Context, id int64) (ports.GrantRecord, bool, error)
	// Leftovers lists the live sessions saved as Leftover, and Leftover is one session's need.
	Leftovers func(ctx context.Context) ([]*berthv1.Need, error)
	Leftover  func(id string) *berthv1.Need
	// Streams is woken by every saved secret, grant and session transition.
	Streams *watchSet
}

// needs serves WatchNeeds: what waits on the person, read from the secret, grant and session
// machines' rows. It keeps no state of its own; the event table is the queue, as for sessions.
type needs struct {
	needsDeps
	// backlog is watchBacklog, smaller in tests.
	backlog int
}

var _ api.Needs = (*needs)(nil)

func newNeeds(d needsDeps) *needs { return &needs{needsDeps: d, backlog: watchBacklog} }

// needKinds are the machines whose rows open and resolve needs.
var needKinds = []string{secret.Kind, grant.Kind, session.Kind}

func (n *needs) WatchNeeds(ctx context.Context, after int64, send func(*berthv1.WatchNeedsResponse) error) error {
	return serveStream(ctx, n.Streams, feed[*berthv1.WatchNeedsResponse]{
		events: n.Events, backlog: n.backlog, rows: n.rows, snapshot: n.snapshot,
		change: func(r ports.EventRecord) (*berthv1.WatchNeedsResponse, bool) { return n.change(ctx, r) },
	}, after, send)
}

// rows reads each kind's rows after the cursor and merges them in id order. A kind with more
// than limit-1 rows makes the result longer than the backlog, so the stream sends a snapshot,
// and a result within the backlog holds every row of every kind after the cursor.
func (n *needs) rows(ctx context.Context, after int64, limit int) ([]ports.EventRecord, error) {
	var all []ports.EventRecord
	for _, kind := range needKinds {
		rows, err := n.Events.EventsAfter(ctx, kind, after, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	slices.SortFunc(all, func(a, b ports.EventRecord) int { return cmp.Compare(a.ID, b.ID) })
	return all, nil
}

// waiting is the need an entity of kind waits on in state, or none.
func waiting(kind, state string) berthv1.NeedKind {
	switch {
	case kind == secret.Kind && state == string(secret.Pending):
		return berthv1.NeedKind_NEED_KIND_STORE
	case kind == secret.Kind && state == string(secret.Rotating):
		return berthv1.NeedKind_NEED_KIND_ROTATE
	case kind == secret.Kind && state == string(secret.Removing):
		return berthv1.NeedKind_NEED_KIND_REMOVE
	case kind == grant.Kind && state == string(grant.Requested):
		return berthv1.NeedKind_NEED_KIND_GRANT
	case kind == session.Kind && state == string(session.Leftover):
		return berthv1.NeedKind_NEED_KIND_LEFTOVER
	}
	return berthv1.NeedKind_NEED_KIND_UNSPECIFIED
}

// opens is the need a transition starts, or none. A prompt starts on its request event, which
// for a new secret or grant loops on the initial state; a leftover starts when a session
// becomes one.
func opens(r ports.EventRecord) berthv1.NeedKind {
	switch {
	case r.Kind == secret.Kind && r.Event == string(secret.StoreRequested),
		r.Kind == secret.Kind && r.Event == string(secret.RotateRequested),
		r.Kind == secret.Kind && r.Event == string(secret.RemoveRequested),
		r.Kind == grant.Kind && r.Event == string(grant.GrantRequested),
		r.Kind == session.Kind && r.To == string(session.Leftover) && r.From != r.To:
		return waiting(r.Kind, r.To)
	}
	return berthv1.NeedKind_NEED_KIND_UNSPECIFIED
}

// change is the message for one row: a need opened, a need resolved, or nothing.
func (n *needs) change(ctx context.Context, r ports.EventRecord) (*berthv1.WatchNeedsResponse, bool) {
	event, kind := berthv1.NeedEvent_NEED_EVENT_OPENED, opens(r)
	if kind == berthv1.NeedKind_NEED_KIND_UNSPECIFIED {
		was := waiting(r.Kind, r.From)
		if was == berthv1.NeedKind_NEED_KIND_UNSPECIFIED || waiting(r.Kind, r.To) != berthv1.NeedKind_NEED_KIND_UNSPECIFIED {
			return nil, false
		}
		event, kind = berthv1.NeedEvent_NEED_EVENT_RESOLVED, was
	}
	return &berthv1.WatchNeedsResponse{Update: &berthv1.WatchNeedsResponse_Change{Change: &berthv1.NeedChange{
		EventId: r.ID, Event: event, Need: n.need(ctx, r.Entity, kind),
	}}}, true
}

// need gathers the facts of one need. A fact berthd cannot read now is left empty: a client
// still learns that something waits.
func (n *needs) need(ctx context.Context, entity string, kind berthv1.NeedKind) *berthv1.Need {
	need := &berthv1.Need{Id: entity, Kind: kind}
	_, id, _ := strings.Cut(entity, "/")
	switch kind {
	case berthv1.NeedKind_NEED_KIND_STORE, berthv1.NeedKind_NEED_KIND_ROTATE, berthv1.NeedKind_NEED_KIND_REMOVE:
		need.SecretName = id
	case berthv1.NeedKind_NEED_KIND_GRANT:
		if gid, err := strconv.ParseInt(id, 10, 64); err == nil {
			if g, ok, err := n.Grant(ctx, gid); err == nil && ok {
				need.SecretName = g.Secret
			}
		}
	case berthv1.NeedKind_NEED_KIND_LEFTOVER:
		return n.Leftover(id)
	case berthv1.NeedKind_NEED_KIND_UNSPECIFIED:
	}
	return need
}

// snapshot holds every open need and the cursor it is consistent with. The cursor is read before
// the states, so no state is older than it; a change after it that the snapshot already shows is
// applied again, which a client ignores. Secrets and grants are read by kind, sessions only for
// the live ones.
func (n *needs) snapshot(ctx context.Context, reason berthv1.SnapshotReason) (*berthv1.WatchNeedsResponse, int64, error) {
	last, err := n.Events.LastEventID(ctx)
	if err != nil {
		return nil, 0, err
	}
	snap := &berthv1.NeedsSnapshot{EventId: last, Reason: reason}
	for _, kind := range []string{secret.Kind, grant.Kind} {
		rows, err := n.List.EntitiesOfKind(ctx, kind)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			if k := waiting(kind, r.State); k != berthv1.NeedKind_NEED_KIND_UNSPECIFIED {
				snap.Needs = append(snap.Needs, n.need(ctx, r.Entity, k))
			}
		}
	}
	leftovers, err := n.Leftovers(ctx)
	if err != nil {
		return nil, 0, err
	}
	snap.Needs = append(snap.Needs, leftovers...)
	return &berthv1.WatchNeedsResponse{Update: &berthv1.WatchNeedsResponse_Snapshot{Snapshot: snap}}, last, nil
}

func (n *needs) closeWatches() { n.Streams.close() }
