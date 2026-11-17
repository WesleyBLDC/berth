// berth-proxy is the per run HTTPS proxy berth run --proxy starts: it writes one secret header
// into requests to the granted hosts only. The logic lives in internal/proxy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/native"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/socket"
	"berth/internal/client"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
	"berth/internal/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	code := proxy.Run(ctx, proxy.Deps{
		Environ:      os.Environ(),
		ExtraEnv:     extraEnv,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		Connect:      connectTrusted,
		Read:         read,
		Roots:        roots(),
		AllowLocal:   allowLocal,
		SystemBundle: "/etc/ssl/cert.pem",
	})
	stop()
	os.Exit(code)
}

// connectTrusted connects only when the process answering on the socket is the signed berthd
// beside this berth-proxy, judged by the kernel's live code identity, the same check berth run
// makes, so a server an agent started at BERTH_HOME can never hand the proxy a ticket.
func connectTrusted(ctx context.Context) (berthv1connect.SecretsServiceClient, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	daemonPath, err := paths.Beside("berthd")
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.DiscardHandler)
	daemon := codetrust.New(ctx, daemonPath, codetrust.DaemonSigningID, filePinner(execid.New(execid.DefaultCodesign)),
		peer.NewCode(), clock.System{}, log)
	sock := paths.Socket(home)
	cl, err := client.ConnectVerified(ctx, sock, "berth-proxy", func(c *net.UnixConn) error {
		pid, err := socket.KernelPeerPID(c)
		if err != nil {
			return err
		}
		token, err := socket.KernelPeerToken(c)
		if err != nil {
			return err
		}
		if !daemon.Runs(ctx, ports.Peer{PID: pid, AuditToken: token}) {
			return fmt.Errorf("the server on %s is not the signed berthd beside this berth-proxy", sock)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cl.Secrets, nil
}

// read hands the ticket to the berth-native beside berth-proxy, which writes the value only over
// a socket whose peer the kernel shows is its parent, running the signed berth-proxy.
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
