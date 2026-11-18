package api_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/testutil"
)

type fakeNeeds struct{ err error }

func (f *fakeNeeds) WatchNeeds(_ context.Context, after int64, send func(*berthv1.WatchNeedsResponse) error) error {
	if f.err != nil {
		return f.err
	}
	return send(&berthv1.WatchNeedsResponse{Update: &berthv1.WatchNeedsResponse_Snapshot{Snapshot: &berthv1.NeedsSnapshot{EventId: after + 1}}})
}

func TestWatchNeedsValidatesAndStreams(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake := &fakeNeeds{}
	server := connect.NewServer()
	api.Register(api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(), Needs: fake,
	}, server)
	c := berthv1connect.NewNeedsServiceClient(connect.NewClient(connectinprocess.New(server)))
	recv := func(after int64) (*berthv1.WatchNeedsResponse, error) {
		stream, err := c.WatchNeeds(t.Context(), &berthv1.WatchNeedsRequest{AfterEventId: after})
		if err != nil {
			return nil, err
		}
		defer func() { _ = stream.Close() }()
		return stream.Receive()
	}
	if got, err := recv(41); err != nil || got.GetSnapshot().GetEventId() != 42 {
		t.Fatalf("stream: %v %v", got, err)
	}
	if _, err := recv(-1); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative cursor: %v, want InvalidArgument", err)
	}
	fake.err = api.ErrTooManyWatchers
	if _, err := recv(0); connect.CodeOf(err) != connect.CodeResourceExhausted || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("too many streams: %v, want ResourceExhausted and RATE_LIMITED", err)
	}
}
