package berthd

import (
	"context"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/ports"
)

// The E2b lifecycle and proxy methods answer Unimplemented until E2b item 6 wires them and
// replaces this file.

func errUnwired() error {
	return connect.NewError(connect.CodeUnimplemented, "not wired yet")
}

func (s *secrets) GrantProxy(context.Context, *berthv1.GrantProxyRequest, ports.Peer) (*berthv1.GrantProxyResponse, error) {
	return nil, errUnwired()
}

func (s *secrets) RevokeGrant(context.Context, *berthv1.RevokeGrantRequest, ports.Peer) (*berthv1.RevokeGrantResponse, error) {
	return nil, errUnwired()
}

func (s *secrets) RemoveSecret(context.Context, *berthv1.RemoveSecretRequest, ports.Peer) (*berthv1.RemoveSecretResponse, error) {
	return nil, errUnwired()
}

func (s *secrets) RotateSecret(context.Context, *berthv1.RotateSecretRequest, ports.Peer) (*berthv1.RotateSecretResponse, error) {
	return nil, errUnwired()
}

func (s *secrets) TrustProxy(context.Context, ports.Peer) error { return errUnwired() }

func (s *secrets) AuthorizeProxy(context.Context, *berthv1.AuthorizeProxyRequest, ports.Peer) (*berthv1.AuthorizeProxyResponse, error) {
	return nil, errUnwired()
}

func (s *secrets) WatchProxySession(context.Context, string, ports.Peer, func(*berthv1.ProxySessionEvent) error) error {
	return errUnwired()
}

func (s *secrets) EndProxySession(context.Context, *berthv1.EndProxySessionRequest, ports.Peer) (*berthv1.EndProxySessionResponse, error) {
	return nil, errUnwired()
}
