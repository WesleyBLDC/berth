package api

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/ports"
)

// AgentTokenHeader carries an agent session's token on every call to the agent socket. A header
// keeps the token out of URLs and messages, so it never reaches the request log.
const AgentTokenHeader = "Berth-Agent-Token" //nolint:gosec // a header name, not a credential

// Agents is the daemon's agent session logic.
type Agents interface {
	CreateAgentSession(ctx context.Context, req *berthv1.CreateAgentSessionRequest) (*berthv1.CreateAgentSessionResponse, error)
	ListAgentSessions(ctx context.Context) (*berthv1.ListAgentSessionsResponse, error)
	RevokeAgentSession(ctx context.Context, req *berthv1.RevokeAgentSessionRequest) (*berthv1.RevokeAgentSessionResponse, error)
	// Authenticate returns the id of the live session token belongs to, when peer is the agent
	// user inside that session's process tree; otherwise ErrAgentTokenInvalid.
	Authenticate(ctx context.Context, token string, peer ports.Peer) (string, error)
	// AdmitStore holds one of the session's store dialog slots until release is called, or
	// returns ErrRateLimited.
	AdmitStore(ctx context.Context, sessionID string) (release func(), err error)
}

var (
	ErrAgentTokenInvalid    = errors.New("agent token invalid")
	ErrRateLimited          = errors.New("rate limited")
	ErrAgentSessionNotFound = errors.New("agent session not found")
)

type sessionKey struct{}

// AgentSession is the agent session the call's token belongs to, or "" off the agent socket.
func AgentSession(ctx context.Context) string {
	s, _ := ctx.Value(sessionKey{}).(string)
	return s
}

// WithAgentSession records the agent session a call's token belongs to.
func WithAgentSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// AgentToken refuses every call without the token of a live agent session presented from that
// session's process tree, and hands the session to the handlers, for the server behind the agent
// socket. It runs after AgentOnly, so a method closed to agents is refused the same way with or
// without a token.
func AgentToken(agents Agents) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			token := ""
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				token = info.RequestHeader().Get(AgentTokenHeader)
			}
			if token == "" || agents == nil {
				return agentsError(ErrAgentTokenInvalid)
			}
			id, err := agents.Authenticate(ctx, token, peerOf(ctx))
			if err != nil {
				return agentsError(err)
			}
			return next(WithAgentSession(ctx, id), spec, stream)
		}
	}
}

func (h *handlers) CreateAgentSession(ctx context.Context, req *berthv1.CreateAgentSessionRequest) (*berthv1.CreateAgentSessionResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	// The request log keeps the session and drops the token, so the token exists only in this reply.
	return onceKeeping(ctx, h, "CreateAgentSession", req.GetRequestId(), &berthv1.CreateAgentSessionResponse{},
		func() (*berthv1.CreateAgentSessionResponse, error) {
			resp, err := h.d.Agents.CreateAgentSession(ctx, req)
			return resp, agentsError(err)
		},
		func(resp *berthv1.CreateAgentSessionResponse) *berthv1.CreateAgentSessionResponse {
			kept := proto.CloneOf(resp)
			kept.Token = ""
			return kept
		})
}

func (h *handlers) ListAgentSessions(ctx context.Context, req *berthv1.ListAgentSessionsRequest) (*berthv1.ListAgentSessionsResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	resp, err := h.d.Agents.ListAgentSessions(ctx)
	return resp, agentsError(err)
}

func (h *handlers) RevokeAgentSession(ctx context.Context, req *berthv1.RevokeAgentSessionRequest) (*berthv1.RevokeAgentSessionResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	return once(ctx, h, "RevokeAgentSession", req.GetRequestId(), &berthv1.RevokeAgentSessionResponse{}, func() (*berthv1.RevokeAgentSessionResponse, error) {
		resp, err := h.d.Agents.RevokeAgentSession(ctx, req)
		return resp, agentsError(err)
	})
}

// admitStore holds a store dialog slot for the call's agent session; calls on the owner socket
// carry no session and are never limited.
func (h *handlers) admitStore(ctx context.Context) (func(), error) {
	id := AgentSession(ctx)
	if id == "" {
		return func() {}, nil
	}
	if h.d.Agents == nil {
		return nil, agentsError(ErrAgentTokenInvalid)
	}
	release, err := h.d.Agents.AdmitStore(ctx, id)
	if err != nil {
		return nil, agentsError(err)
	}
	return release, nil
}

func agentsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAgentTokenInvalid):
		return withReason(connect.NewError(connect.CodeUnauthenticated,
			"no valid agent session token: it is missing, unknown, expired or revoked, its session ended, or this process is outside the session's process tree"),
			berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID)
	case errors.Is(err, ErrRateLimited):
		return withReason(connect.NewError(connect.CodeResourceExhausted,
			"this agent session has too many store requests waiting for the person, or made too many in the last hour; try again later"),
			berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	case errors.Is(err, ErrAgentSessionNotFound):
		return withReason(connect.NewError(connect.CodeNotFound, "no agent session has this id"),
			berthv1.ErrorReason_ERROR_REASON_AGENT_SESSION_NOT_FOUND)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "the call ended before berthd had an outcome")
	}
	return connect.NewError(connect.CodeInternal, "berthd could not complete the agent session request")
}
