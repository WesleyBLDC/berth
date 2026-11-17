package api_test

import (
	"bytes"
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

type fakeBrowsers struct {
	err      error
	launches int
	session  string
}

func (f *fakeBrowsers) LaunchBrowser(_ context.Context, req *berthv1.LaunchBrowserRequest) (*berthv1.LaunchBrowserResponse, error) {
	f.launches++
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.LaunchBrowserResponse{
		Browser:  &berthv1.ManagedBrowser{Id: "0123456789abcdef", Profile: req.GetProfile(), State: berthv1.BrowserState_BROWSER_STATE_READY},
		CdpToken: "cdp-token-canary-5d1e",
	}, nil
}

func (f *fakeBrowsers) ListBrowsers(context.Context, *berthv1.ListBrowsersRequest) (*berthv1.ListBrowsersResponse, error) {
	return &berthv1.ListBrowsersResponse{}, f.err
}

func (f *fakeBrowsers) CloseBrowser(_ context.Context, req *berthv1.CloseBrowserRequest) (*berthv1.CloseBrowserResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.CloseBrowserResponse{Browser: &berthv1.ManagedBrowser{Id: req.GetId(), State: berthv1.BrowserState_BROWSER_STATE_CLOSED}}, nil
}

func (f *fakeBrowsers) OpenAgentBrowser(ctx context.Context, _ *berthv1.OpenAgentBrowserRequest) (*berthv1.OpenAgentBrowserResponse, error) {
	f.launches++
	f.session = api.AgentSession(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.OpenAgentBrowserResponse{
		Browser:  &berthv1.AgentBrowser{Id: "0123456789abcdef", State: berthv1.BrowserState_BROWSER_STATE_READY},
		CdpToken: "cdp-token-canary-77a2",
	}, nil
}

func (f *fakeBrowsers) CloseAgentBrowser(ctx context.Context, req *berthv1.CloseAgentBrowserRequest) (*berthv1.CloseAgentBrowserResponse, error) {
	f.session = api.AgentSession(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &berthv1.CloseAgentBrowserResponse{Browser: &berthv1.AgentBrowser{Id: req.GetId(), State: berthv1.BrowserState_BROWSER_STATE_CLOSED}}, nil
}

// asAgent marks every call as one from the agent session id, as AgentToken does past the token.
func asAgent(id string) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(api.WithAgentSession(ctx, id), spec, stream)
		}
	}
}

func setupBrowsers(t *testing.T, fake *fakeBrowsers, interceptors ...connect.ServerInterceptor) (berthv1connect.BrowsersServiceClient, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := connect.NewServer(interceptors...)
	api.Register(api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { return nil },
		Browsers:    fake,
	}, server)
	return berthv1connect.NewBrowsersServiceClient(connect.NewClient(connectinprocess.New(server))), store
}

// The token is in the first reply only: the request log keeps the reply without it, so a retry
// gets the browser and no token.
func TestLaunchTokenOnlyInTheFirstReply(t *testing.T) {
	fake := &fakeBrowsers{}
	c, store := setupBrowsers(t, fake)
	req := &berthv1.LaunchBrowserRequest{RequestId: "request-1", Profile: "work"}
	first, err := c.LaunchBrowser(t.Context(), req)
	if err != nil || first.GetCdpToken() != "cdp-token-canary-5d1e" {
		t.Fatalf("first = %v %v", first, err)
	}
	again, err := c.LaunchBrowser(t.Context(), req)
	if err != nil || again.GetCdpToken() != "" || again.GetBrowser().GetId() != "0123456789abcdef" || fake.launches != 1 {
		t.Fatalf("retry = %v %v, launches %d", again, err, fake.launches)
	}
	_, stored, found, err := store.Recall(t.Context(), api.Principal(t.Context()), "request-1")
	if err != nil || !found {
		t.Fatalf("recall: %v %v", found, err)
	}
	if len(stored) == 0 || bytes.Contains(stored, []byte("cdp-token-canary-5d1e")) {
		t.Fatal("the request log must not keep the CDP token")
	}
}

func TestBrowsersValidateAndType(t *testing.T) {
	fake := &fakeBrowsers{}
	c, _ := setupBrowsers(t, fake)
	for _, req := range []*berthv1.LaunchBrowserRequest{
		{RequestId: "request-1", Profile: "../escape"},
		{RequestId: "request-1", Profile: "Work"},
		{RequestId: "request-1", Profile: "work", Executable: "relative/chrome"},
		{RequestId: "short", Profile: "work"},
	} {
		if _, err := c.LaunchBrowser(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("launch %v = %v, want InvalidArgument", req, err)
		}
	}
	if _, err := c.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{RequestId: "request-2", Id: "XYZ"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("close with a bad id = %v", err)
	}
	for _, tc := range []struct {
		err    error
		reason berthv1.ErrorReason
	}{
		{errors.Join(api.ErrProfileBusy, errors.New("browser x already uses profile work")), berthv1.ErrorReason_ERROR_REASON_PROFILE_BUSY},
		{api.ErrBrowserLaunchFailed, berthv1.ErrorReason_ERROR_REASON_BROWSER_LAUNCH_FAILED},
	} {
		fake.err = tc.err
		_, err := c.LaunchBrowser(t.Context(), &berthv1.LaunchBrowserRequest{RequestId: "request-" + tc.reason.String(), Profile: "work"})
		if api.ReasonOf(err) != tc.reason {
			t.Fatalf("launch with %v = %v, want %v", tc.err, err, tc.reason)
		}
	}
	fake.err = api.ErrBrowserNotFound
	if _, err := c.CloseBrowser(t.Context(), &berthv1.CloseBrowserRequest{RequestId: "request-3", Id: "0123456789abcdef"}); api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_BROWSER_NOT_FOUND {
		t.Fatalf("close = %v", err)
	}
}

// The agent browser methods act for the caller's agent session, keep the token out of the request
// log like LaunchBrowser, and refuse a call without an agent session (the owner socket).
func TestAgentBrowserMethods(t *testing.T) {
	fake := &fakeBrowsers{}
	owner, _ := setupBrowsers(t, fake)
	if _, err := owner.OpenAgentBrowser(t.Context(), &berthv1.OpenAgentBrowserRequest{RequestId: "request-1"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("open with no agent session = %v, want FailedPrecondition", err)
	}
	if _, err := owner.CloseAgentBrowser(t.Context(), &berthv1.CloseAgentBrowserRequest{RequestId: "request-1", Id: "0123456789abcdef"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("close with no agent session = %v, want FailedPrecondition", err)
	}
	if fake.launches != 0 {
		t.Fatal("a refused call reached the daemon")
	}

	c, store := setupBrowsers(t, fake, asAgent("session-a"))
	req := &berthv1.OpenAgentBrowserRequest{RequestId: "request-1"}
	first, err := c.OpenAgentBrowser(t.Context(), req)
	if err != nil || first.GetCdpToken() != "cdp-token-canary-77a2" || fake.session != "session-a" {
		t.Fatalf("first = %v %v, session %q", first, err, fake.session)
	}
	again, err := c.OpenAgentBrowser(t.Context(), req)
	if err != nil || again.GetCdpToken() != "" || again.GetBrowser().GetId() != "0123456789abcdef" || fake.launches != 1 {
		t.Fatalf("retry = %v %v, launches %d", again, err, fake.launches)
	}
	_, stored, found, err := store.Recall(t.Context(), api.Principal(api.WithAgentSession(t.Context(), "session-a")), "request-1")
	if err != nil || !found || len(stored) == 0 || bytes.Contains(stored, []byte("cdp-token-canary-77a2")) {
		t.Fatalf("the request log must keep the reply without the token: found %v, err %v", found, err)
	}
	if _, err := c.CloseAgentBrowser(t.Context(), &berthv1.CloseAgentBrowserRequest{RequestId: "request-2", Id: "XYZ"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("close with a bad id = %v", err)
	}
	for _, tc := range []struct {
		err    error
		code   connect.Code
		reason berthv1.ErrorReason
	}{
		{api.ErrRateLimited, connect.CodeResourceExhausted, berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED},
		{api.ErrAgentTokenInvalid, connect.CodeUnauthenticated, berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID},
		{api.ErrBrowserLaunchFailed, connect.CodeFailedPrecondition, berthv1.ErrorReason_ERROR_REASON_BROWSER_LAUNCH_FAILED},
	} {
		fake.err = tc.err
		_, err := c.OpenAgentBrowser(t.Context(), &berthv1.OpenAgentBrowserRequest{RequestId: "request-" + tc.reason.String()})
		if connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
			t.Fatalf("open with %v = %v, want %v and %v", tc.err, err, tc.code, tc.reason)
		}
	}
	fake.err = api.ErrBrowserNotFound
	if _, err := c.CloseAgentBrowser(t.Context(), &berthv1.CloseAgentBrowserRequest{RequestId: "request-3", Id: "0123456789abcdef"}); api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_BROWSER_NOT_FOUND {
		t.Fatalf("close = %v", err)
	}
}
