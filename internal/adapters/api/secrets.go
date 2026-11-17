package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/ports"
)

// Secrets is the daemon's secret and grant logic. peer is the caller's pid as the kernel
// reported it for the connection, or 0 when unknown.
type Secrets interface {
	RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest, peer int) (*berthv1.RequestSecretResponse, error)
	ListSecrets(ctx context.Context) (*berthv1.ListSecretsResponse, error)
	GrantSecret(ctx context.Context, req *berthv1.GrantSecretRequest, peer int) (*berthv1.GrantSecretResponse, error)
	AuthorizeUse(ctx context.Context, req *berthv1.AuthorizeUseRequest, peer int) (*berthv1.AuthorizeUseResponse, error)
	// TrustRedeemer returns ErrPeerNotTrusted unless peer is the signed berth-native helper.
	TrustRedeemer(ctx context.Context, peer int) error
	RedeemUseTicket(ctx context.Context, ticket ports.Ticket, peer int) (*berthv1.RedeemUseTicketResponse, error)
}

// Errors Secrets returns for outcomes a client must react to; anything else is internal.
var (
	ErrSecretExists       = errors.New("secret exists")
	ErrSecretNotFound     = errors.New("secret not found")
	ErrExecutableNotFound = errors.New("executable not found")
	ErrTicketInvalid      = errors.New("ticket invalid")
	ErrPeerNotTrusted     = errors.New("peer not trusted")
	ErrBadGlob            = errors.New("malformed args glob")
	ErrCallerUnknown      = errors.New("caller unknown")
)

type peerKey struct{}

// WithPeerPID records the connection's peer pid, from the socket, for the handlers.
func WithPeerPID(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, peerKey{}, pid)
}

func peerPID(ctx context.Context) int {
	pid, _ := ctx.Value(peerKey{}).(int)
	return pid
}

func (h *handlers) RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest) (*berthv1.RequestSecretResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RequestSecret", req.GetRequestId(), &berthv1.RequestSecretResponse{}, func() (*berthv1.RequestSecretResponse, error) {
		resp, err := h.d.Secrets.RequestSecret(ctx, req, peerPID(ctx))
		return resp, secretsError(err)
	})
}

func (h *handlers) ListSecrets(ctx context.Context, req *berthv1.ListSecretsRequest) (*berthv1.ListSecretsResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	resp, err := h.d.Secrets.ListSecrets(ctx)
	return resp, secretsError(err)
}

func (h *handlers) GrantSecret(ctx context.Context, req *berthv1.GrantSecretRequest) (*berthv1.GrantSecretResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "GrantSecret", req.GetRequestId(), &berthv1.GrantSecretResponse{}, func() (*berthv1.GrantSecretResponse, error) {
		resp, err := h.d.Secrets.GrantSecret(ctx, req, peerPID(ctx))
		return resp, secretsError(err)
	})
}

func (h *handlers) AuthorizeUse(ctx context.Context, req *berthv1.AuthorizeUseRequest) (*berthv1.AuthorizeUseResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	// Tickets live only in memory, so the request log keeps the outcome and drops the ticket;
	// a retry learns the outcome but must ask again for a ticket.
	return onceKeeping(ctx, h, "AuthorizeUse", req.GetRequestId(), &berthv1.AuthorizeUseResponse{},
		func() (*berthv1.AuthorizeUseResponse, error) {
			resp, err := h.d.Secrets.AuthorizeUse(ctx, req, peerPID(ctx))
			return resp, secretsError(err)
		},
		func(resp *berthv1.AuthorizeUseResponse) *berthv1.AuthorizeUseResponse {
			kept := proto.CloneOf(resp)
			kept.Ticket = nil
			return kept
		})
}

func (h *handlers) RedeemUseTicket(ctx context.Context, req *berthv1.RedeemUseTicketRequest) (*berthv1.RedeemUseTicketResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	// The peer is checked before the request log, so another process replaying a request id
	// learns nothing from it.
	peer := peerPID(ctx)
	if err := h.d.Secrets.TrustRedeemer(ctx, peer); err != nil {
		return nil, secretsError(err)
	}
	var ticket ports.Ticket
	copy(ticket[:], req.GetTicket())
	return once(ctx, h, "RedeemUseTicket", req.GetRequestId(), &berthv1.RedeemUseTicketResponse{}, func() (*berthv1.RedeemUseTicketResponse, error) {
		resp, err := h.d.Secrets.RedeemUseTicket(ctx, ticket, peer)
		return resp, secretsError(err)
	})
}

// secretsError maps the Secrets errors to typed Connect errors. Messages are fixed text, so no
// detail from deeper layers (paths, helper output) reaches a caller.
func secretsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrSecretExists):
		return withReason(connect.NewError(connect.CodeAlreadyExists, "a secret with this name is already stored"),
			berthv1.ErrorReason_ERROR_REASON_SECRET_EXISTS)
	case errors.Is(err, ErrSecretNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "no stored secret with this name"),
			berthv1.ErrorReason_ERROR_REASON_SECRET_NOT_FOUND)
	case errors.Is(err, ErrExecutableNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "berthd cannot read this executable"),
			berthv1.ErrorReason_ERROR_REASON_EXECUTABLE_NOT_FOUND)
	case errors.Is(err, ErrTicketInvalid):
		return withReason(connect.NewError(connect.CodePermissionDenied, "the ticket is unknown, expired or used"),
			berthv1.ErrorReason_ERROR_REASON_TICKET_INVALID)
	case errors.Is(err, ErrPeerNotTrusted):
		return withReason(connect.NewError(connect.CodePermissionDenied, "only the signed berth-native helper may redeem tickets"),
			berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED)
	case errors.Is(err, ErrBadGlob):
		return connect.NewError(connect.CodeInvalidArgument, "args_glob is not a valid pattern")
	case errors.Is(err, ErrCallerUnknown):
		return connect.NewError(connect.CodePermissionDenied, "berthd could not identify the calling process")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "the call ended before berthd had an outcome")
	}
	return connect.NewError(connect.CodeInternal, "berthd could not complete the secrets request")
}
