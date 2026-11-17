package api_test

import (
	"context"
	"errors"
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

type fakeSessions struct {
	err   error
	stops int
}

func (f *fakeSessions) ListSessions(context.Context, *berthv1.ListSessionsRequest) (*berthv1.ListSessionsResponse, error) {
	return &berthv1.ListSessionsResponse{Sessions: []*berthv1.Session{{Id: "1-2"}}}, f.err
}

func (f *fakeSessions) InspectSession(_ context.Context, req *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.InspectSessionResponse{Session: &berthv1.Session{Id: req.GetId()}}, nil
}

func (f *fakeSessions) StopSession(context.Context, *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error) {
	f.stops++
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
}

func (f *fakeSessions) WatchSessions(_ context.Context, after int64, send func(*berthv1.WatchSessionsResponse) error) error {
	if f.err != nil {
		return f.err
	}
	return send(&berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Snapshot{Snapshot: &berthv1.SessionsSnapshot{EventId: after + 1}}})
}

func setupSessions(t *testing.T, fake *fakeSessions) berthv1connect.SessionsServiceClient {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := connect.NewServer()
	api.Register(api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { return nil },
		Sessions:    fake,
	}, server)
	return berthv1connect.NewSessionsServiceClient(connect.NewClient(connectinprocess.New(server)))
}

func TestSessionsValidateAndType(t *testing.T) {
	fake := &fakeSessions{}
	c := setupSessions(t, fake)
	if resp, err := c.ListSessions(t.Context(), &berthv1.ListSessionsRequest{}); err != nil || len(resp.GetSessions()) != 1 {
		t.Fatalf("list: %v %v", resp, err)
	}
	for _, id := range []string{"", "12", "a-1", "12-34-56", "1-2\n"} {
		if _, err := c.InspectSession(t.Context(), &berthv1.InspectSessionRequest{Id: id}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("inspect %q: %v, want InvalidArgument", id, err)
		}
	}
	if _, err := c.ListSessions(t.Context(), &berthv1.ListSessionsRequest{States: []berthv1.SessionState{berthv1.SessionState_SESSION_STATE_UNSPECIFIED}}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("list with UNSPECIFIED: %v", err)
	}
	cases := []struct {
		err    error
		code   connect.Code
		reason berthv1.ErrorReason
	}{
		{api.ErrSessionNotFound, connect.CodeNotFound, berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_FOUND},
		{api.ErrSessionNotStoppable, connect.CodeFailedPrecondition, berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_STOPPABLE},
		{errors.New("kill 123: /Users/someone/profile"), connect.CodeInternal, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
	}
	for _, tc := range cases {
		fake.err = tc.err
		_, err := c.InspectSession(t.Context(), &berthv1.InspectSessionRequest{Id: "1-2"})
		if connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
			t.Errorf("%v: got %v (%v)", tc.err, err, api.ReasonOf(err))
		}
	}
}

func TestStopSessionRunsOncePerRequestID(t *testing.T) {
	fake := &fakeSessions{}
	c := setupSessions(t, fake)
	req := &berthv1.StopSessionRequest{RequestId: "req-stop-0001", Id: "1-2"}
	for range 2 {
		resp, err := c.StopSession(t.Context(), req)
		if err != nil || resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
			t.Fatalf("stop: %v %v", resp, err)
		}
	}
	if fake.stops != 1 {
		t.Fatalf("stop ran %d times, want 1", fake.stops)
	}
}

func TestWatchSessionsValidatesAndStreams(t *testing.T) {
	fake := &fakeSessions{}
	c := setupSessions(t, fake)
	recv := func(after int64) (*berthv1.WatchSessionsResponse, error) {
		stream, err := c.WatchSessions(t.Context(), &berthv1.WatchSessionsRequest{AfterEventId: after})
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
	if _, err := recv(0); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("too many streams: %v, want ResourceExhausted", err)
	}
}
