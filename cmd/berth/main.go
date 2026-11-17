// berth is the command line client for berthd.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/native"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/socket"
	"berth/internal/cli"
	"berth/internal/client"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Env{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Connect: func(ctx context.Context) (*client.Client, error) {
			home, err := paths.Home()
			if err != nil {
				return nil, err
			}
			return client.Connect(ctx, paths.Socket(home), "berth-cli")
		},
		ConnectTrusted: connectTrusted,
		Read:           read,
	}))
}

// connectTrusted connects only when the process answering on the socket is the signed berthd
// beside this berth, judged by the kernel's live code identity, so a server an agent started
// at BERTH_HOME can never hand berth run a ticket.
func connectTrusted(ctx context.Context) (*client.Client, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	daemonPath, err := paths.Beside("berthd")
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.DiscardHandler)
	daemon := codetrust.New(ctx, daemonPath, codetrust.DaemonSigningID, daemonPinner(execid.New(execid.DefaultCodesign)),
		peer.NewCode(), clock.System{}, log)
	sock := paths.Socket(home)
	return client.ConnectVerified(ctx, sock, "berth-cli", func(c *net.UnixConn) error {
		pid, err := socket.KernelPeerPID(c)
		if err != nil {
			return err
		}
		token, err := socket.KernelPeerToken(c)
		if err != nil {
			return err
		}
		if !daemon.Runs(ctx, ports.Peer{PID: pid, AuditToken: token}) {
			return fmt.Errorf("the server on %s is not the signed berthd beside this berth; run make build and start bin/berthd", sock)
		}
		return nil
	})
}

// read hands the ticket to the berth-native beside berth, the only helper berthd trusts.
func read(ctx context.Context, ticket []byte) ([]byte, error) {
	var t ports.Ticket
	if len(ticket) != len(t) {
		return nil, errors.New("berthd sent a ticket of the wrong length")
	}
	copy(t[:], ticket)
	helper, err := nativeHelper()
	if err != nil {
		return nil, err
	}
	return native.Read(ctx, helper, t)
}
