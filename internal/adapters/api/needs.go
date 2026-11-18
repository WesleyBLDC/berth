package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
)

// Needs is what waits on the person: open dialogs, Touch ID prompts and leftover browsers.
type Needs interface {
	// WatchNeeds calls send for every message of the stream until ctx ends, send fails, or berthd
	// stops serving, when it returns nil.
	WatchNeeds(ctx context.Context, afterEventID int64, send func(*berthv1.WatchNeedsResponse) error) error
}

func (h *handlers) WatchNeeds(ctx context.Context, req *berthv1.WatchNeedsRequest, stream berthv1connect.NeedsServiceWatchNeedsServerStream) error {
	if err := validate(req); err != nil {
		return err
	}
	var sendErr error
	err := h.d.Needs.WatchNeeds(ctx, req.GetAfterEventId(), func(resp *berthv1.WatchNeedsResponse) error {
		sendErr = stream.Send(resp)
		return sendErr
	})
	if sendErr != nil {
		return sendErr
	}
	return needsError(err)
}

func needsError(err error) error {
	if ce := commonError(err); ce != nil {
		return ce
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrTooManyWatchers):
		return withReason(connect.NewError(connect.CodeResourceExhausted, "too many need streams are open; close one and try again"),
			berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	}
	return connect.NewError(connect.CodeInternal, "could not read what waits on the person")
}
