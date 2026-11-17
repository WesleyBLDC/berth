package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
)

// Sessions is the daemon's browser discovery: what its scans saw, and stopping a browser.
type Sessions interface {
	ListSessions(ctx context.Context, req *berthv1.ListSessionsRequest) (*berthv1.ListSessionsResponse, error)
	InspectSession(ctx context.Context, req *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error)
	StopSession(ctx context.Context, req *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error)
}

var (
	ErrSessionNotFound     = errors.New("session not found")
	ErrSessionNotStoppable = errors.New("session not stoppable")
)

func (h *handlers) ListSessions(ctx context.Context, req *berthv1.ListSessionsRequest) (*berthv1.ListSessionsResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	resp, err := h.d.Sessions.ListSessions(ctx, req)
	return resp, sessionError(err)
}

func (h *handlers) InspectSession(ctx context.Context, req *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	resp, err := h.d.Sessions.InspectSession(ctx, req)
	return resp, sessionError(err)
}

func (h *handlers) StopSession(ctx context.Context, req *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "StopSession", req.GetRequestId(), &berthv1.StopSessionResponse{}, func() (*berthv1.StopSessionResponse, error) {
		resp, err := h.d.Sessions.StopSession(ctx, req)
		return resp, sessionError(err)
	})
}

func sessionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrSessionNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "no browser session has that id"), berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_FOUND)
	case errors.Is(err, ErrSessionNotStoppable):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, err.Error()), berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_STOPPABLE)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "gave up")
	}
	return connect.NewError(connect.CodeInternal, "could not read browser sessions")
}
