// Package client connects Go tools (CLI, TUI, MCP shim) to berthd over its Unix socket.
package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
)

type Client struct {
	Daemon   berthv1connect.DaemonServiceClient
	Debug    berthv1connect.DebugServiceClient
	Secrets  berthv1connect.SecretsServiceClient
	Sessions berthv1connect.SessionsServiceClient
	Agents   berthv1connect.AgentServiceClient
}

// Connect dials the socket and performs the protocol handshake before returning.
func Connect(ctx context.Context, socketPath, name string) (*Client, error) {
	return ConnectVerified(ctx, socketPath, name, nil)
}

// ConnectVerified is Connect, but verify sees every connection before a byte is sent and can
// refuse it, so a caller can make sure the real berthd answers rather than whoever listens on
// the socket.
func ConnectVerified(ctx context.Context, socketPath, name string, verify func(*net.UnixConn) error) (*Client, error) {
	return dial(ctx, socketPath, name, verify)
}

// ConnectAgent dials the agent socket and sends token, an agent session's token, in a header on
// every call, never in a message or a URL.
func ConnectAgent(ctx context.Context, socketPath, name, token string) (*Client, error) {
	return dial(ctx, socketPath, name, nil, func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.RequestHeader().Set(api.AgentTokenHeader, token)
			}
			return next(ctx, spec)
		}
	})
}

func dial(ctx context.Context, socketPath, name string, verify func(*net.UnixConn) error, interceptors ...connect.ClientInterceptor) (*Client, error) {
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
	c := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://berthd"), interceptors...)
	cl := &Client{
		Daemon:   berthv1connect.NewDaemonServiceClient(c),
		Debug:    berthv1connect.NewDebugServiceClient(c),
		Secrets:  berthv1connect.NewSecretsServiceClient(c),
		Sessions: berthv1connect.NewSessionsServiceClient(c),
		Agents:   berthv1connect.NewAgentServiceClient(c),
	}
	if _, err := cl.Daemon.Handshake(ctx, &berthv1.HandshakeRequest{ProtocolVersion: api.ProtocolVersion, ClientName: name}); err != nil {
		if reason := api.ReasonOf(err); reason == berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH {
			return nil, fmt.Errorf("version mismatch: %w", err)
		} else if reason == berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
			return nil, fmt.Errorf("berthd refused the agent token: %w", err)
		}
		return nil, fmt.Errorf("cannot reach berthd at %s: %w", socketPath, err)
	}
	return cl, nil
}

// ReadAgentToken reads an agent session token from a file only its owner can read, so the token
// never has to be an argument, which any process can see, or an environment variable.
func ReadAgentToken(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the person names the file
	if err != nil {
		return "", fmt.Errorf("agent token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("agent token file: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	switch {
	case !st.Mode().IsRegular():
		return "", fmt.Errorf("agent token file %s is not a regular file", path)
	case !ok || int(sys.Uid) != os.Getuid():
		return "", fmt.Errorf("agent token file %s belongs to another user", path)
	case st.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("agent token file %s can be read by others; chmod 600 it", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", fmt.Errorf("agent token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("agent token file %s is empty", path)
	}
	return token, nil
}
