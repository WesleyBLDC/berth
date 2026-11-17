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
	Daemon  berthv1connect.DaemonServiceClient
	Debug   berthv1connect.DebugServiceClient
	Secrets berthv1connect.SecretsServiceClient
}

// Connect dials the socket and performs the protocol handshake before returning.
func Connect(ctx context.Context, socketPath, name string) (*Client, error) {
	return ConnectVerified(ctx, socketPath, name, nil)
}

// ConnectVerified is Connect, but verify sees every connection before a byte is sent and can
// refuse it, so a caller can make sure the real berthd answers rather than whoever listens on
// the socket.
func ConnectVerified(ctx context.Context, socketPath, name string, verify func(*net.UnixConn) error) (*Client, error) {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "unix", socketPath)
			if err != nil || verify == nil {
				return conn, err
			}
			uc, ok := conn.(*net.UnixConn)
			if !ok {
				_ = conn.Close()
				return nil, fmt.Errorf("%s is not a Unix socket", socketPath)
			}
			if err := verify(uc); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		},
	}
	c := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://berthd"))
	cl := &Client{
		Daemon:  berthv1connect.NewDaemonServiceClient(c),
		Debug:   berthv1connect.NewDebugServiceClient(c),
		Secrets: berthv1connect.NewSecretsServiceClient(c),
	}
	if _, err := cl.Daemon.Handshake(ctx, &berthv1.HandshakeRequest{ProtocolVersion: api.ProtocolVersion, ClientName: name}); err != nil {
		if api.ReasonOf(err) == berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH {
			return nil, fmt.Errorf("version mismatch: %w", err)
		}
		return nil, fmt.Errorf("cannot reach berthd at %s: %w", socketPath, err)
	}
	return cl, nil
}
