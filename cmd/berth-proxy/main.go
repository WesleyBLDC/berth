// berth-proxy is the per run HTTPS proxy berth run --proxy starts: it writes one secret header
// into requests to the granted hosts only. The logic lives in internal/proxy.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/native"
	"berth/internal/peertrust"
	"berth/internal/proxy"
	"berth/internal/proxyctl"
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
		Read:         native.Reader(nativeHelper),
		Roots:        roots(),
		AllowLocal:   allowLocal,
		SystemBundle: proxyctl.SystemBundle,
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
