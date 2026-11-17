// Package peertrust is how a Berth binary recognises its signed peers in this build: the pin rule
// berthd applies to berth, berth-native and berth-proxy, and the check berth and berth-proxy make
// that the signed berthd is the one answering the socket. Release builds trust only Apple's chain
// and Berth's team; binaries built with -tags berth_e2e accept ad hoc signed test binaries by
// identifier, and no environment variable or flag changes which rule applies.
package peertrust

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/socket"
	"berth/internal/client"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

// ConnectDaemon connects only when the process answering on the socket is the signed berthd
// beside the running binary, judged by the kernel's live code identity, so a server an agent
// started at BERTH_HOME can never hand out a ticket. self names the running binary in the error.
func ConnectDaemon(ctx context.Context, clientName, self string) (*client.Client, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	daemonPath, err := paths.Beside("berthd")
	if err != nil {
		return nil, err
	}
	daemon := codetrust.New(ctx, daemonPath, codetrust.DaemonSigningID, Pinner(execid.New(execid.DefaultCodesign)),
		peer.NewCode(), clock.System{}, slog.New(slog.DiscardHandler))
	sock := paths.Socket(home)
	return client.ConnectVerified(ctx, sock, clientName, func(c *net.UnixConn) error {
		pid, err := socket.KernelPeerPID(c)
		if err != nil {
			return err
		}
		token, err := socket.KernelPeerToken(c)
		if err != nil {
			return err
		}
		if !daemon.Runs(ctx, ports.Peer{PID: pid, AuditToken: token}) {
			return fmt.Errorf("the server on %s is not the signed berthd beside this %s; run make build and start bin/berthd", sock, self)
		}
		return nil
	})
}
