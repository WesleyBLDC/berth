// Package api serves berth.v1 over Connect. Handlers validate, then call the core through
// narrow functions; they hold no state of their own.
package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/core/ports"
)

// ProtocolVersion changes only when the wire contract breaks (a new berth.vN package).
const ProtocolVersion = 1

// Deps is everything the handlers need from the daemon.
type Deps struct {
	Version   string
	PID       int
	StartedAt time.Time
	Socket    string
	Store     ports.Store
	Requests  ports.RequestLog
	Clock     ports.Clock
	// RequestStop asks the daemon machine to stop; it must not block.
	RequestStop func(ctx context.Context) error
}

type handlers struct {
	berthv1connect.UnimplementedDaemonServiceHandler
	berthv1connect.UnimplementedDebugServiceHandler
	d        Deps
	mutating sync.Mutex
}

// Register mounts every service on server.
func Register(server *connect.Server, d Deps) {
	h := &handlers{d: d}
	berthv1connect.RegisterDaemonServiceHandler(server, h)
	berthv1connect.RegisterDebugServiceHandler(server, h)
}

func (h *handlers) Handshake(_ context.Context, req *berthv1.HandshakeRequest) (*berthv1.HandshakeResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	if req.GetProtocolVersion() != ProtocolVersion {
		return nil, withReason(connect.Errorf(connect.CodeFailedPrecondition,
			"client speaks protocol %d, berthd speaks %d; update the older one", req.GetProtocolVersion(), ProtocolVersion),
			berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH)
	}
	return &berthv1.HandshakeResponse{ProtocolVersion: ProtocolVersion, DaemonVersion: h.d.Version}, nil
}

func (h *handlers) Status(ctx context.Context, _ *berthv1.StatusRequest) (*berthv1.StatusResponse, error) {
	st, _, err := h.d.Store.LoadState(ctx, "daemon")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "could not read daemon state")
	}
	return &berthv1.StatusResponse{
		State:         st.State,
		StateVersion:  st.Version,
		Pid:           int32(h.d.PID), //nolint:gosec // macOS process IDs fit in 32 bits
		StartedAt:     timestamppb.New(h.d.StartedAt),
		SocketPath:    h.d.Socket,
		DaemonVersion: h.d.Version,
	}, nil
}

func (h *handlers) Shutdown(ctx context.Context, req *berthv1.ShutdownRequest) (*berthv1.ShutdownResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "Shutdown", req.GetRequestId(), &berthv1.ShutdownResponse{}, func() (*berthv1.ShutdownResponse, error) {
		if err := h.d.RequestStop(ctx); err != nil {
			return nil, withReason(connect.NewError(connect.CodeUnavailable, "berthd is not accepting a stop right now"),
				berthv1.ErrorReason_ERROR_REASON_NOT_SERVING)
		}
		return &berthv1.ShutdownResponse{AcceptedAt: timestamppb.New(h.d.Clock.Now())}, nil
	})
}

func (h *handlers) GetState(ctx context.Context, req *berthv1.GetStateRequest) (*berthv1.GetStateResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	st, found, err := h.d.Store.LoadState(ctx, req.GetEntity())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "could not read state")
	}
	if !found {
		return nil, connect.Errorf(connect.CodeNotFound, "no entity %q", req.GetEntity())
	}
	limit := int(req.GetHistoryLimit())
	if limit == 0 {
		limit = 20
	}
	hist, err := h.d.Store.History(ctx, req.GetEntity(), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "could not read history")
	}
	resp := &berthv1.GetStateResponse{Entity: st.Entity, Kind: st.Kind, State: st.State, Version: st.Version}
	for _, r := range hist {
		resp.History = append(resp.History, &berthv1.Transition{
			Version: r.Version, Event: r.Event, FromState: r.From, ToState: r.To, At: timestamppb.New(r.At), Detail: r.Detail,
		})
	}
	return resp, nil
}

// once runs do at most once per request ID and replays the stored response on a retry.
func once[T proto.Message](ctx context.Context, h *handlers, method, requestID string, empty T, do func() (T, error)) (T, error) {
	h.mutating.Lock()
	defer h.mutating.Unlock()
	var zero T
	storedMethod, stored, found, err := h.d.Requests.Recall(ctx, requestID)
	if err != nil {
		return zero, connect.NewError(connect.CodeInternal, "could not read the request log")
	}
	if found {
		if storedMethod != method {
			return zero, withReason(connect.Errorf(connect.CodeInvalidArgument, "request id was already used for %s", storedMethod),
				berthv1.ErrorReason_ERROR_REASON_REQUEST_ID_REUSED)
		}
		if err := proto.Unmarshal(stored, empty); err != nil {
			return zero, connect.NewError(connect.CodeInternal, "stored response is unreadable")
		}
		return empty, nil
	}
	resp, err := do()
	if err != nil {
		return zero, err
	}
	raw, err := proto.Marshal(resp)
	if err != nil {
		return zero, connect.NewError(connect.CodeInternal, "could not encode response")
	}
	if err := h.d.Requests.Remember(ctx, requestID, method, raw, h.d.Clock.Now()); err != nil {
		return zero, connect.NewError(connect.CodeInternal, "could not record the request")
	}
	return resp, nil
}

func validate(msg proto.Message) error {
	if err := protovalidate.Validate(msg); err != nil {
		var ve *protovalidate.ValidationError
		if errors.As(err, &ve) {
			return connect.NewError(connect.CodeInvalidArgument, ve.Error())
		}
		return connect.NewError(connect.CodeInternal, "could not validate the request")
	}
	return nil
}

func withReason(e *connect.Error, reason berthv1.ErrorReason) *connect.Error {
	if detail, err := connectproto.NewErrorDetail(&berthv1.ErrorInfo{Reason: reason}); err == nil {
		return e.WithDetail(detail)
	}
	return e
}

// ReasonOf extracts the typed reason from an error, for clients.
func ReasonOf(err error) berthv1.ErrorReason {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED
	}
	for _, d := range ce.Details() {
		if msg, err := connectproto.UnmarshalErrorDetail(d); err == nil {
			if info, ok := msg.(*berthv1.ErrorInfo); ok {
				return info.GetReason()
			}
		}
	}
	return berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}
