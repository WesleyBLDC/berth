package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/core/ports"
)

// Secrets is the daemon's secret and grant logic. peer is the caller as the kernel reported it
// for the connection; its zero value means unknown.
type Secrets interface {
	RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest, peer ports.Peer) (*berthv1.RequestSecretResponse, error)
	ListSecrets(ctx context.Context) (*berthv1.ListSecretsResponse, error)
	GrantSecret(ctx context.Context, req *berthv1.GrantSecretRequest, peer ports.Peer) (*berthv1.GrantSecretResponse, error)
	AuthorizeUse(ctx context.Context, req *berthv1.AuthorizeUseRequest, peer ports.Peer) (*berthv1.AuthorizeUseResponse, error)
	// TrustRedeemer returns ErrPeerNotTrusted unless peer is the signed berth-native helper.
	TrustRedeemer(ctx context.Context, peer ports.Peer) error
	RedeemUseTicket(ctx context.Context, ticket ports.Ticket, peer ports.Peer) (*berthv1.RedeemUseTicketResponse, error)
	// GrantProxy gets a target whose port the handler already turned from 0 into 443.
	GrantProxy(ctx context.Context, req *berthv1.GrantProxyRequest, peer ports.Peer) (*berthv1.GrantProxyResponse, error)
	RevokeGrant(ctx context.Context, req *berthv1.RevokeGrantRequest, peer ports.Peer) (*berthv1.RevokeGrantResponse, error)
	RemoveSecret(ctx context.Context, req *berthv1.RemoveSecretRequest, peer ports.Peer) (*berthv1.RemoveSecretResponse, error)
	RotateSecret(ctx context.Context, req *berthv1.RotateSecretRequest, peer ports.Peer) (*berthv1.RotateSecretResponse, error)
	// TrustProxy returns ErrPeerNotTrusted unless peer is the signed berth-proxy.
	TrustProxy(ctx context.Context, peer ports.Peer) error
	AuthorizeProxy(ctx context.Context, req *berthv1.AuthorizeProxyRequest, peer ports.Peer) (*berthv1.AuthorizeProxyResponse, error)
	// WatchProxySession sends the session's events until it stops or ctx ends. It returns
	// ErrPeerNotTrusted for an unknown session or one another audit token authorized.
	WatchProxySession(ctx context.Context, sessionID string, peer ports.Peer, send func(*berthv1.ProxySessionEvent) error) error
	// EndProxySession returns ErrPeerNotTrusted like WatchProxySession.
	EndProxySession(ctx context.Context, req *berthv1.EndProxySessionRequest, peer ports.Peer) (*berthv1.EndProxySessionResponse, error)
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
	ErrInterpreter        = errors.New("executable is an interpreter")
	ErrBusy               = errors.New("secret busy")
	ErrBadProxyTarget     = errors.New("proxy target not grantable")
	ErrGrantNotFound      = errors.New("grant not found")
)

type peerKey struct{}

// WithPeer records the connection's peer, from the socket, for the handlers.
func WithPeer(ctx context.Context, p ports.Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

func peerOf(ctx context.Context) ports.Peer {
	p, _ := ctx.Value(peerKey{}).(ports.Peer)
	return p
}

func (h *handlers) RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest) (*berthv1.RequestSecretResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RequestSecret", req.GetRequestId(), &berthv1.RequestSecretResponse{}, func() (*berthv1.RequestSecretResponse, error) {
		// Inside once, so a retry that replays the stored reply opens nothing and costs nothing.
		release, err := h.admitStore(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		resp, err := h.d.Secrets.RequestSecret(ctx, req, peerOf(ctx))
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
		resp, err := h.d.Secrets.GrantSecret(ctx, req, peerOf(ctx))
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
			resp, err := h.d.Secrets.AuthorizeUse(ctx, req, peerOf(ctx))
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
	peer := peerOf(ctx)
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

func (h *handlers) GrantProxy(ctx context.Context, req *berthv1.GrantProxyRequest) (*berthv1.GrantProxyResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "GrantProxy", req.GetRequestId(), &berthv1.GrantProxyResponse{}, func() (*berthv1.GrantProxyResponse, error) {
		resp, err := h.d.Secrets.GrantProxy(ctx, withDefaultPort(req), peerOf(ctx))
		return resp, secretsError(err)
	})
}

// withDefaultPort returns req with a target port of 0 read as 443, so the core never sees 0.
func withDefaultPort(req *berthv1.GrantProxyRequest) *berthv1.GrantProxyRequest {
	if req.GetTarget().GetPort() != 0 {
		return req
	}
	out := proto.CloneOf(req)
	out.Target.Port = 443
	return out
}

func (h *handlers) RevokeGrant(ctx context.Context, req *berthv1.RevokeGrantRequest) (*berthv1.RevokeGrantResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RevokeGrant", req.GetRequestId(), &berthv1.RevokeGrantResponse{}, func() (*berthv1.RevokeGrantResponse, error) {
		resp, err := h.d.Secrets.RevokeGrant(ctx, req, peerOf(ctx))
		return resp, secretsError(err)
	})
}

func (h *handlers) RemoveSecret(ctx context.Context, req *berthv1.RemoveSecretRequest) (*berthv1.RemoveSecretResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RemoveSecret", req.GetRequestId(), &berthv1.RemoveSecretResponse{}, func() (*berthv1.RemoveSecretResponse, error) {
		resp, err := h.d.Secrets.RemoveSecret(ctx, req, peerOf(ctx))
		return resp, secretsError(err)
	})
}

func (h *handlers) RotateSecret(ctx context.Context, req *berthv1.RotateSecretRequest) (*berthv1.RotateSecretResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RotateSecret", req.GetRequestId(), &berthv1.RotateSecretResponse{}, func() (*berthv1.RotateSecretResponse, error) {
		resp, err := h.d.Secrets.RotateSecret(ctx, req, peerOf(ctx))
		return resp, secretsError(err)
	})
}

func (h *handlers) AuthorizeProxy(ctx context.Context, req *berthv1.AuthorizeProxyRequest) (*berthv1.AuthorizeProxyResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	peer := peerOf(ctx)
	if err := h.d.Secrets.TrustProxy(ctx, peer); err != nil {
		return nil, secretsError(err)
	}
	// Like AuthorizeUse, the request log keeps the outcome and session but never a ticket.
	return onceKeeping(ctx, h, "AuthorizeProxy", req.GetRequestId(), &berthv1.AuthorizeProxyResponse{},
		func() (*berthv1.AuthorizeProxyResponse, error) {
			resp, err := h.d.Secrets.AuthorizeProxy(ctx, req, peer)
			return resp, secretsError(err)
		},
		func(resp *berthv1.AuthorizeProxyResponse) *berthv1.AuthorizeProxyResponse {
			kept := proto.CloneOf(resp)
			for _, t := range kept.GetTickets() {
				t.Ticket = nil
			}
			return kept
		})
}

func (h *handlers) WatchProxySession(ctx context.Context, req *berthv1.WatchProxySessionRequest, stream berthv1connect.SecretsServiceWatchProxySessionServerStream) error {
	if err := validate(req); err != nil {
		return err
	}
	peer := peerOf(ctx)
	if err := h.d.Secrets.TrustProxy(ctx, peer); err != nil {
		return secretsError(err)
	}
	err := h.d.Secrets.WatchProxySession(ctx, req.GetSessionId(), peer, func(e *berthv1.ProxySessionEvent) error {
		return stream.Send(&berthv1.WatchProxySessionResponse{Event: e})
	})
	return secretsError(err)
}

func (h *handlers) EndProxySession(ctx context.Context, req *berthv1.EndProxySessionRequest) (*berthv1.EndProxySessionResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	peer := peerOf(ctx)
	if err := h.d.Secrets.TrustProxy(ctx, peer); err != nil {
		return nil, secretsError(err)
	}
	return once(ctx, h, "EndProxySession", req.GetRequestId(), &berthv1.EndProxySessionResponse{}, func() (*berthv1.EndProxySessionResponse, error) {
		resp, err := h.d.Secrets.EndProxySession(ctx, req, peer)
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
		return withReason(connect.NewError(connect.CodePermissionDenied, "berthd trusts only the signed berth binaries for this call"),
			berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED)
	case errors.Is(err, ErrInterpreter):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, "the executable is an interpreter, which could print the secret; set allow_interpreter to grant it anyway"),
			berthv1.ErrorReason_ERROR_REASON_INTERPRETER)
	case errors.Is(err, ErrBusy):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, "the secret is waiting for the person on another request; try again when it finishes"),
			berthv1.ErrorReason_ERROR_REASON_BUSY)
	case errors.Is(err, ErrBadProxyTarget):
		return withReason(connect.NewError(connect.CodeInvalidArgument, "berthd will not grant this proxy target"),
			berthv1.ErrorReason_ERROR_REASON_BAD_PROXY_TARGET)
	case errors.Is(err, ErrGrantNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "no grant has this id"),
			berthv1.ErrorReason_ERROR_REASON_GRANT_NOT_FOUND)
	case errors.Is(err, ErrBadGlob):
		return connect.NewError(connect.CodeInvalidArgument, "args_glob is not a valid pattern")
	case errors.Is(err, ErrCallerUnknown):
		return connect.NewError(connect.CodePermissionDenied, "berthd could not identify the calling process")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "the call ended before berthd had an outcome")
	case connect.CodeOf(err) == connect.CodeUnimplemented:
		return connect.NewError(connect.CodeUnimplemented, "berthd does not serve this method yet")
	}
	return connect.NewError(connect.CodeInternal, "berthd could not complete the secrets request")
}
