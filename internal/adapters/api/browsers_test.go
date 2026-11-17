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

func setupBrowsers(t *testing.T, fake *fakeBrowsers) (berthv1connect.BrowsersServiceClient, *sqlite.Store) {
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
