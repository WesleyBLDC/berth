// berth-proxy is the per run HTTPS proxy berth run --proxy starts: it writes one secret header
// into requests to the granted hosts only. The logic lives in internal/proxy.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/native"
	"berth/internal/core/ports"
	"berth/internal/peertrust"
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

// connectTrusted refuses any server but the signed berthd beside this berth-proxy, the same
// check berth run makes.
func connectTrusted(ctx context.Context) (berthv1connect.SecretsServiceClient, error) {
	cl, err := peertrust.ConnectDaemon(ctx, "berth-proxy", "berth-proxy")
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
