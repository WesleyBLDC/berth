// Package berthd is the daemon's imperative shell: it opens adapters, runs the daemon machine,
// and performs the machine's effects. All decisions live in internal/core.
package berthd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"berth/internal/adapters/api"
	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/native"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/socket"
	"berth/internal/adapters/sqlite"
	"berth/internal/adapters/ticket"
	"berth/internal/core/daemon"
	"berth/internal/core/machine"
	"berth/internal/paths"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sys/unix"
)

type Config struct {
	Home    string
	Version string
	Log     *slog.Logger
	// Helper is the berth-native path; empty means berth-native beside the berthd executable.
	Helper string
	// TrustHelper decides which peer may redeem use tickets. Nil means the signed release
	// helper (SignedHelper); only Go test code sets anything else, never the environment.
	TrustHelper TrustHelper
}

type shell struct {
	cfg    Config
	log    *slog.Logger
	store  *sqlite.Store
	actor  *machine.Actor[daemon.State, daemon.Event, daemon.Effect]
	server *http.Server
	ln     net.Listener
	sock   string
	exit   chan int
	start  time.Time
	// machineCtx outlives the signal context so background sends can still reach the actor.
	machineCtx context.Context
	secrets    *secrets
}

// Run blocks until the daemon machine reaches Stopped, and returns its exit code.
// Cancelling ctx (SIGTERM, SIGINT) asks the machine to stop cleanly.
func Run(ctx context.Context, cfg Config) (int, error) {
	// A core dump could contain secrets that passed through memory.
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})

	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return 1, err
	}
	store, err := sqlite.Open(ctx, paths.Database(cfg.Home))
	if err != nil {
		return 1, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// The machine keeps running after ctx ends so it can drain, so it gets its own context.
	machineCtx, stopMachine := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMachine()

	s := &shell{
		cfg: cfg, log: cfg.Log, store: store, sock: paths.Socket(cfg.Home),
		exit: make(chan int, 1), start: clock.System{}.Now(), machineCtx: machineCtx,
	}
	if s.secrets, err = newSecretsFor(machineCtx, cfg, store); err != nil {
		return 1, err
	}
	s.actor, err = machine.NewActor(ctx, machine.Config[daemon.State, daemon.Event, daemon.Effect]{
		Entity: daemon.Entity, Kind: daemon.Kind, Initial: daemon.Starting,
		Transition: daemon.Transition, Run: s.run, Store: store, Clock: clock.System{},
		Rejected: func(err error) { s.log.Warn("ignored event", "err", err) },
	})
	if err != nil {
		return 1, err
	}

	machineErr := make(chan error, 1)
	go func() { machineErr <- s.actor.Run(machineCtx) }()

	if err := s.actor.Send(machineCtx, daemon.Event{Kind: daemon.Boot, Note: fmt.Sprintf("pid %d, version %s", os.Getpid(), cfg.Version)}); err != nil {
		return 1, err
	}
	signalled := ctx.Done()
	for {
		select {
		case code := <-s.exit:
			return code, nil
		case err := <-machineErr:
			return 1, fmt.Errorf("daemon machine stopped: %w", err)
		case <-signalled:
			signalled = nil
			s.log.Info("stop requested by signal")
			_ = s.actor.Send(machineCtx, daemon.Event{Kind: daemon.StopRequested, Note: "signal"})
		}
	}
}

func (s *shell) run(ctx context.Context, effect daemon.Effect) ([]daemon.Event, error) {
	switch effect.Kind {
	case daemon.Recover:
		note := fmt.Sprintf("previous run ended in %s", effect.Previous)
		if effect.Unclean {
			note += ": unclean shutdown"
			s.log.Warn("recovering from an unclean shutdown", "previous", effect.Previous)
		}
		return []daemon.Event{{Kind: daemon.Recovered, Note: note}}, nil
	case daemon.StartServing:
		if err := s.serve(ctx); err != nil {
			return []daemon.Event{{Kind: daemon.Failed, Note: err.Error()}}, nil
		}
		return nil, nil
	case daemon.StopServing:
		if s.server != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				s.log.Warn("forced server close", "err", err)
				_ = s.server.Close()
			}
			_ = os.Remove(s.sock)
		}
		return []daemon.Event{{Kind: daemon.Drained}}, nil
	case daemon.Exit:
		select {
		case s.exit <- effect.Code:
		default:
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown effect %q", effect.Kind)
}

func newSecretsFor(ctx context.Context, cfg Config, store *sqlite.Store) (*secrets, error) {
	helper := cfg.Helper
	if helper == "" {
		var err error
		if helper, err = paths.Beside("berth-native"); err != nil {
			return nil, fmt.Errorf("find berth-native: %w", err)
		}
	}
	execs := execid.New(execid.DefaultCodesign)
	trust := cfg.TrustHelper
	if trust == nil {
		trust = SignedHelper(execs)
	}
	return newSecrets(ctx, secretsDeps{
		Log: cfg.Log, Store: store, Records: store, Clock: clock.System{},
		Approver: native.New(helper), Execs: execs, Peers: peer.New(), Tickets: ticket.New(), Trust: trust,
	}), nil
}

func (s *shell) serve(ctx context.Context) error {
	ln, err := socket.Listen(ctx, s.sock, uint32(os.Getuid()), socket.KernelPeerUID, s.log) //nolint:gosec // user IDs fit in 32 bits
	if err != nil {
		return err
	}
	server := connect.NewServer()
	api.Register(server, api.Deps{
		Version: s.cfg.Version, PID: os.Getpid(), StartedAt: s.start, Socket: s.sock,
		Store: s.store, Requests: s.store, Clock: clock.System{},
		RequestStop: func(ctx context.Context) error {
			return s.actor.Send(ctx, daemon.Event{Kind: daemon.StopRequested, Note: "Shutdown call"})
		},
		Secrets: s.secrets,
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithGRPC(), connecthttp.WithProtoJSON())
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)
	s.server = &http.Server{
		Handler: mux, Protocols: protocols, ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if uc, ok := c.(*net.UnixConn); ok {
				if pid, err := socket.KernelPeerPID(uc); err == nil {
					return api.WithPeerPID(ctx, pid)
				}
			}
			return ctx
		},
	}
	s.ln = ln
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = s.actor.Send(s.machineCtx, daemon.Event{Kind: daemon.Failed, Note: "server: " + err.Error()})
		}
	}()
	s.log.Info("serving", "socket", s.sock)
	return nil
}
