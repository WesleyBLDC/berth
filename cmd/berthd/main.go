// berthd is the Berth daemon.
package main

import (
	"context"
	"flag"
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
	agentUID := flag.Int("agent-uid", -1, "user ID of the agent user; turns on the agent socket")
	agentGID := flag.Int("agent-gid", -1, "group that may reach the agent socket; berthd's user must belong to it")
	agentSock := flag.String("agent-socket", "", "agent socket path (default /Users/Shared/Berth/<your uid>/agent.sock)")
	flag.Parse()
	agent, err := berthd.NewAgentSocket(*agentUID, *agentGID, *agentSock)
	if err != nil {
		log.Error("bad agent socket flags", "err", err)
		return 2
	}
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
		Agent: agent,
	})
	if err != nil {
		log.Error("berthd failed", "err", err)
	}
	return code
}
