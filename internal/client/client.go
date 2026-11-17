// Package client connects Go tools (CLI, TUI, MCP shim) to berthd over its Unix socket.
package client

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
)

type Client struct {
	Daemon berthv1connect.DaemonServiceClient
	Debug  berthv1connect.DebugServiceClient
}

// Connect dials the socket and performs the protocol handshake before returning.
func Connect(ctx context.Context, socketPath, name string) (*Client, error) {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	c := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://berthd"))
	cl := &Client{Daemon: berthv1connect.NewDaemonServiceClient(c), Debug: berthv1connect.NewDebugServiceClient(c)}
	if _, err := cl.Daemon.Handshake(ctx, &berthv1.HandshakeRequest{ProtocolVersion: api.ProtocolVersion, ClientName: name}); err != nil {
		if api.ReasonOf(err) == berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH {
			return nil, fmt.Errorf("version mismatch: %w", err)
		}
		return nil, fmt.Errorf("cannot reach berthd at %s: %w", socketPath, err)
	}
	return cl, nil
}
