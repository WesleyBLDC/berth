// berthd is the Berth daemon.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"berth/internal/adapters/execid"
	"berth/internal/berthd"
	"berth/internal/logging"
	"berth/internal/paths"
	"berth/internal/peertrust"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	log := slog.New(logging.NewRedacting(slog.NewTextHandler(os.Stderr, nil)))
	home, err := paths.Home()
	if err != nil {
		log.Error("no home", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := berthd.Run(ctx, berthd.Config{
		Home: home, Version: version, Log: log,
		Pinner: peertrust.Pinner(execid.New(execid.DefaultCodesign)), AllowLocalProxyTargets: peertrust.E2E,
	})
	if err != nil {
		log.Error("berthd failed", "err", err)
	}
	return code
}
