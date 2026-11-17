package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
)

// Browsers is the daemon's managed browsers: launching Chromium over a private pipe, listing
// and closing it.
type Browsers interface {
	LaunchBrowser(ctx context.Context, req *berthv1.LaunchBrowserRequest) (*berthv1.LaunchBrowserResponse, error)
	ListBrowsers(ctx context.Context, req *berthv1.ListBrowsersRequest) (*berthv1.ListBrowsersResponse, error)
	CloseBrowser(ctx context.Context, req *berthv1.CloseBrowserRequest) (*berthv1.CloseBrowserResponse, error)
	// OpenAgentBrowser and CloseAgentBrowser act for the agent session AgentSession(ctx) names,
	// which is never empty when they are called.
	OpenAgentBrowser(ctx context.Context, req *berthv1.OpenAgentBrowserRequest) (*berthv1.OpenAgentBrowserResponse, error)
	CloseAgentBrowser(ctx context.Context, req *berthv1.CloseAgentBrowserRequest) (*berthv1.CloseAgentBrowserResponse, error)
}

var (
	ErrProfileBusy         = errors.New("profile busy")
	ErrBrowserNotFound     = errors.New("browser not found")
	ErrBrowserLaunchFailed = errors.New("browser launch failed")
)

func (h *handlers) LaunchBrowser(ctx context.Context, req *berthv1.LaunchBrowserRequest) (*berthv1.LaunchBrowserResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	// The request log keeps the browser and drops the token, so the token exists only in this reply.
	return onceKeeping(ctx, h, "LaunchBrowser", req.GetRequestId(), &berthv1.LaunchBrowserResponse{},
		func() (*berthv1.LaunchBrowserResponse, error) {
			resp, err := h.d.Browsers.LaunchBrowser(ctx, req)
			return resp, browserError(err)
		},
		func(resp *berthv1.LaunchBrowserResponse) *berthv1.LaunchBrowserResponse {
			kept := proto.CloneOf(resp)
			kept.CdpToken = ""
			return kept
		})
}

func (h *handlers) ListBrowsers(ctx context.Context, req *berthv1.ListBrowsersRequest) (*berthv1.ListBrowsersResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	resp, err := h.d.Browsers.ListBrowsers(ctx, req)
	return resp, browserError(err)
}

func (h *handlers) CloseBrowser(ctx context.Context, req *berthv1.CloseBrowserRequest) (*berthv1.CloseBrowserResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "CloseBrowser", req.GetRequestId(), &berthv1.CloseBrowserResponse{}, func() (*berthv1.CloseBrowserResponse, error) {
		resp, err := h.d.Browsers.CloseBrowser(ctx, req)
		return resp, browserError(err)
	})
}

func (h *handlers) OpenAgentBrowser(ctx context.Context, req *berthv1.OpenAgentBrowserRequest) (*berthv1.OpenAgentBrowserResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	if AgentSession(ctx) == "" {
		return nil, errNoAgentSession
	}
	return onceKeeping(ctx, h, "OpenAgentBrowser", req.GetRequestId(), &berthv1.OpenAgentBrowserResponse{},
		func() (*berthv1.OpenAgentBrowserResponse, error) {
			resp, err := h.d.Browsers.OpenAgentBrowser(ctx, req)
			return resp, browserError(err)
		},
		func(resp *berthv1.OpenAgentBrowserResponse) *berthv1.OpenAgentBrowserResponse {
			kept := proto.CloneOf(resp)
			kept.CdpToken = ""
			return kept
		})
}

func (h *handlers) CloseAgentBrowser(ctx context.Context, req *berthv1.CloseAgentBrowserRequest) (*berthv1.CloseAgentBrowserResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	if AgentSession(ctx) == "" {
		return nil, errNoAgentSession
	}
	return once(ctx, h, "CloseAgentBrowser", req.GetRequestId(), &berthv1.CloseAgentBrowserResponse{}, func() (*berthv1.CloseAgentBrowserResponse, error) {
		resp, err := h.d.Browsers.CloseAgentBrowser(ctx, req)
		return resp, browserError(err)
	})
}

// errNoAgentSession answers the agent browser methods on the owner socket, where no call carries
// an agent session; the owner launches and closes browsers with LaunchBrowser and CloseBrowser.
var errNoAgentSession = connect.NewError(connect.CodeFailedPrecondition, "agent browsers belong to an agent session; the owner uses LaunchBrowser and CloseBrowser")

// browserError keeps the daemon's explanation for the typed refusals, which hold only facts
// berthd chose (a profile name it validated, a browser id it minted, a fixed phrase).
func browserError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrProfileBusy):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, err.Error()), berthv1.ErrorReason_ERROR_REASON_PROFILE_BUSY)
	case errors.Is(err, ErrBrowserNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "no managed browser has that id"), berthv1.ErrorReason_ERROR_REASON_BROWSER_NOT_FOUND)
	case errors.Is(err, ErrRateLimited):
		return withReason(connect.NewError(connect.CodeResourceExhausted,
			"this agent session has 2 browsers open, or opened 10 in the last hour; close one, or open new contexts in a browser it has"),
			berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	case errors.Is(err, ErrAgentTokenInvalid):
		return agentsError(err)
	case errors.Is(err, ErrBrowserLaunchFailed):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, err.Error()), berthv1.ErrorReason_ERROR_REASON_BROWSER_LAUNCH_FAILED)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "gave up")
	}
	return connect.NewError(connect.CodeInternal, "could not complete the browser request")
}
