// Package berthd is the daemon's imperative shell: it opens adapters, runs every machine's actors
// and performs their effects. Decisions about states live in internal/core; the shell keeps what
// the effects need between events (in memory facts, tickets, limits, timers and process handles).
package berthd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"berth/internal/adapters/api"
	"berth/internal/adapters/cdppipe"
	"berth/internal/adapters/cdpproxy"
	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/native"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/procscan"
	"berth/internal/adapters/socket"
	"berth/internal/adapters/sqlite"
	"berth/internal/adapters/ticket"
	"berth/internal/codetrust"
	"berth/internal/core/daemon"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/paths"
	"berth/internal/proxyproc"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sys/unix"
)

type Config struct {
	Home    string
	Version string
	Log     *slog.Logger
	// Helper, CLI and Proxy are the berth-native, berth and berth-proxy paths; empty means
	// beside berthd.
	Helper string
	CLI    string
	Proxy  string
	// App is Berth.app's executable, the only caller of methods marked (berth.v1.app); empty
	// means ../MacOS/Berth from berthd's folder, where the bundle puts it (Contents/Helpers).
	App string
	// Pinner decides which files berthd trusts as the berth CLI, berth-native, berth-proxy and
	// Berth.app; it is required. cmd/berthd passes peertrust.Pinner, which is
	// codetrust.SignedPinner (Apple's chain, Berth's team and identifier) in every release build,
	// and nothing reads the environment for it.
	Pinner codetrust.Pinner
	// AllowLocalProxyTargets lets a proxy grant name localhost or an IP literal; only binaries
	// built with -tags berth_e2e set it, for the test servers on loopback.
	AllowLocalProxyTargets bool
	// Agent turns on the agent socket; nil leaves it off.
	Agent *AgentSocket
	// Browser is the Chromium executable managed browsers run; empty means the first one found.
	Browser string
}

// AgentSocket is the second socket, for the agent user: only peers with UID connect, and only
// methods marked (berth.v1.agent) are served. GID is the group that may reach the socket file,
// and the daemon's user must belong to it.
type AgentSocket struct {
	Path string
	UID  uint32
	GID  uint32
}

// NewAgentSocket checks berthd's agent flags, where -1 means unset; with none set the agent
// socket is off and it returns nil. An empty path means the default under /Users/Shared.
func NewAgentSocket(uid, gid int, path string) (*AgentSocket, error) {
	switch {
	case uid < 0 && gid < 0 && path == "":
		return nil, nil
	case uid < 0:
		return nil, errors.New("-agent-gid and -agent-socket need -agent-uid")
	case uid == 0:
		return nil, errors.New("the agent user cannot be root")
	case gid < 0:
		return nil, errors.New("-agent-uid needs -agent-gid")
	case uid > math.MaxUint32 || gid > math.MaxUint32:
		return nil, errors.New("user and group IDs must fit in 32 bits")
	}
	if path == "" {
		path = paths.AgentSocket(os.Getuid())
	}
	return &AgentSocket{Path: path, UID: uint32(uid), GID: uint32(gid)}, nil
}

type shell struct {
	cfg     Config
	log     *slog.Logger
	store   *sqlite.Store
	actor   *machine.Actor[daemon.State, daemon.Event, daemon.Effect]
	servers []*http.Server
	sockets []string
	sock    string
	exit    chan int
	start   time.Time
	// machineCtx outlives the signal context so background sends can still reach the actor.
	machineCtx context.Context
	secrets    *secrets
	browsers   *browsers
	agents     *agents
	managed    *managed
	needs      *needs
	// app is Berth.app, pinned on the first call to a method marked app.
	app *codetrust.File
	// stopScans ends the browser scan loop StartServing began.
	stopScans context.CancelFunc
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
	if cfg.Pinner == nil {
		return 1, errors.New("berthd needs a Pinner to know which binaries to trust")
	}
	pin := cfg.Pinner
	execs := execid.New(execid.DefaultCodesign)
	// Secret, grant and session transitions wake the WatchNeeds streams.
	needStreams := newWatchSet()
	if s.secrets, err = newSecretsFor(machineCtx, cfg, store, execs, pin, needStreams.wakeAll); err != nil {
		return 1, err
	}
	appPath, err := appExecutable(cfg)
	if err != nil {
		return 1, fmt.Errorf("find Berth.app: %w", err)
	}
	s.app = codetrust.Deferred(appPath, codetrust.AppSigningID, pin, peer.NewCode(), clock.System{}, cfg.Log)
	procs, err := procscan.New()
	if err != nil {
		return 1, err
	}
	var agentUID uint32
	if cfg.Agent != nil {
		agentUID = cfg.Agent.UID
	}
	s.agents = newAgents(machineCtx, agentsDeps{
		Log: cfg.Log, Store: store, Records: store, Clock: clock.System{}, Lineage: procs, Code: peer.NewCode(), AgentUID: agentUID,
		StopProxies: s.secrets.stopAgentSessions, WatchExit: procs.WatchExit,
		// s.managed is made below; the hook runs only once sessions can end, after boot started.
		CloseBrowsers: func(ctx context.Context, id string) { s.managed.closeAgent(ctx, id) },
	})
	s.secrets.AgentLive = s.agents.live
	s.browsers = newBrowsers(machineCtx, browsersDeps{
		Log: cfg.Log, Source: procs, Store: store, List: store, Events: store, Clock: clock.System{},
		UID:     uint32(os.Getuid()), //nolint:gosec // user IDs fit in 32 bits
		Changed: needStreams.wakeAll,
	})
	s.needs = newNeeds(needsDeps{
		Events: store, List: store, Grant: store.Grant, Leftovers: s.browsers.leftovers, Leftover: s.browsers.leftoverNeed, Streams: needStreams,
	})
	s.managed = newManaged(machineCtx, managedDeps{
		Log: cfg.Log, Store: store, List: store, Clock: clock.System{}, Driver: cdppipe.Driver{},
		Proxy: &cdpproxy.Proxy{Log: cfg.Log},
		Seen:  s.browsers, Home: cfg.Home, Browser: cfg.Browser, AgentLive: s.agents.live,
	})
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
		// Agent sessions first, so a proxy session of one that ended gets its explicit stop
		// before secrets recovery marks every open proxy session Lost.
		if err := s.agents.recover(ctx); err != nil {
			return []daemon.Event{{Kind: daemon.Failed, Note: "recover agent sessions: " + err.Error()}}, nil
		}
		if err := s.secrets.recover(ctx); err != nil {
			return []daemon.Event{{Kind: daemon.Failed, Note: "recover secrets: " + err.Error()}}, nil
		}
		if err := s.browsers.recover(ctx); err != nil {
			return []daemon.Event{{Kind: daemon.Failed, Note: "recover browser sessions: " + err.Error()}}, nil
		}
		if err := s.managed.recover(ctx); err != nil {
			return []daemon.Event{{Kind: daemon.Failed, Note: "recover managed browsers: " + err.Error()}}, nil
		}
		note := fmt.Sprintf("previous run ended in %s", effect.Previous)
		if effect.Unclean {
			note += ": unclean shutdown"
			s.log.Warn("recovering from an unclean shutdown", "previous", effect.Previous)
		}
		return []daemon.Event{{Kind: daemon.Recovered, Note: note}}, nil
	case daemon.StartServing:
		if err := s.serve(ctx); err != nil {
			s.log.Error("cannot serve", "err", err)
			return []daemon.Event{{Kind: daemon.Failed, Note: err.Error()}}, nil
		}
		scanCtx, stop := context.WithCancel(ctx)
		s.stopScans = stop
		go s.browsers.run(scanCtx)
		return nil, nil
	case daemon.StopServing:
		if s.stopScans != nil {
			s.stopScans()
		}
		// Managed browsers close while their exits can still be recorded; launchd's SIGTERM must
		// not leave a Chrome process group behind.
		s.managed.stop(ctx, managedStopTimeout)
		// Open session and need streams end first, or the server shutdown below would wait out its grace.
		s.browsers.closeWatches()
		s.needs.closeWatches()
		// Proxy sessions are told to stop and open prompts end first, while the server still
		// takes their EndProxySession calls and their callers still get an answer.
		if !s.secrets.stop(stopPromptsTimeout) {
			s.log.Warn("open prompts or proxy sessions did not record an ending in time")
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for _, server := range s.servers {
			if err := server.Shutdown(shutdownCtx); err != nil {
				s.log.Warn("forced server close", "err", err)
				_ = server.Close()
			}
		}
		for _, sock := range s.sockets {
			_ = os.Remove(sock)
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

// managedStopTimeout bounds how long a stop waits for managed browsers to close before it kills
// their process groups.
const managedStopTimeout = 5 * time.Second

// stopPromptsTimeout bounds how long a stop waits for open prompts to record their ending.
const stopPromptsTimeout = 5 * time.Second

// appExecutable is where Berth.app's executable must be for berthd to trust it: in the bundle,
// berthd is Contents/Helpers/berthd and the app Contents/MacOS/Berth.
func appExecutable(cfg Config) (string, error) {
	if cfg.App != "" {
		return cfg.App, nil
	}
	return paths.Beside(filepath.Join("..", "MacOS", "Berth"))
}

func newSecretsFor(ctx context.Context, cfg Config, store *sqlite.Store, execs *execid.Identity, pin codetrust.Pinner, changed func()) (*secrets, error) {
	helper, cli, proxy := cfg.Helper, cfg.CLI, cfg.Proxy
	for _, f := range []struct {
		path *string
		name string
	}{{&helper, "berth-native"}, {&cli, "berth"}, {&proxy, "berth-proxy"}} {
		if *f.path != "" {
			continue
		}
		p, err := paths.Beside(f.name)
		if err != nil {
			return nil, fmt.Errorf("find %s: %w", f.name, err)
		}
		*f.path = p
	}
	code := peer.NewCode()
	trusted := func(path, identifier string) *codetrust.File {
		return codetrust.New(ctx, path, identifier, pin, code, clock.System{}, cfg.Log)
	}
	return newSecrets(ctx, secretsDeps{
		Log: cfg.Log, Store: store, Records: store, Sessions: store, Clock: clock.System{},
		Approver: native.New(helper), Execs: execs, Peers: peer.New(), Tickets: ticket.New(),
		CLI: trusted(cli, codetrust.CLISigningID), Helper: trusted(helper, codetrust.HelperSigningID),
		Proxy: trusted(proxy, codetrust.ProxySigningID), PID: os.Getpid(), Exits: procscan.WatchPID,
		StartProxyProcess: func(ctx context.Context) (*proxyproc.Process, error) { return proxyproc.Start(ctx, proxy, os.Stderr) },
		AllowLocalTargets: cfg.AllowLocalProxyTargets, Changed: changed,
	}), nil
}

func (s *shell) serve(ctx context.Context) error {
	ln, err := socket.Listen(ctx, s.sock, uint32(os.Getuid()), socket.KernelPeerUID, s.log) //nolint:gosec // user IDs fit in 32 bits
	if err != nil {
		return err
	}
	owner := connect.NewServer(api.AppOnly(s.app.Runs))
	servers := []*connect.Server{owner}
	var agentLn net.Listener
	var agent *connect.Server
	if a := s.cfg.Agent; a != nil {
		if agentLn, err = socket.ListenShared(ctx, a.Path, a.UID, a.GID, socket.KernelPeerUID, s.log); err != nil {
			_ = ln.Close()
			return fmt.Errorf("agent socket: %w", err)
		}
		agent = connect.NewServer(api.AgentOnly(), api.AppOnly(s.app.Runs), api.AgentToken(s.agents))
		servers = append(servers, agent)
	}
	// Status reports an empty path when the kernel will not give it; nothing decides on it.
	self, _ := paths.Self()
	api.Register(api.Deps{
		Version: s.cfg.Version, PID: os.Getpid(), StartedAt: s.start, Socket: s.sock, Executable: self,
		Store: s.store, Requests: s.store, Clock: clock.System{}, Log: s.log,
		RequestStop: func(ctx context.Context) error {
			return s.actor.Send(ctx, daemon.Event{Kind: daemon.StopRequested, Note: "Shutdown call"})
		},
		Secrets: s.secrets, Sessions: s.browsers, Agents: s.agents, Browsers: s.managed, Needs: s.needs,
	}, servers...)
	s.listen(ln, s.sock, owner, false)
	if agent != nil {
		s.listen(agentLn, s.cfg.Agent.Path, agent, true)
	}
	return nil
}

func (s *shell) listen(ln net.Listener, sock string, server *connect.Server, agent bool) {
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithGRPC(), connecthttp.WithProtoJSON())
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)
	hs := &http.Server{
		Handler: mux, Protocols: protocols, ReadHeaderTimeout: 5 * time.Second,
		// The listener hands over only connections whose uid, pid and audit token the kernel gave.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if sc, ok := c.(*socket.Conn); ok {
				ctx = api.WithCaller(ctx, api.Caller{Agent: agent, UID: sc.UID})
				return api.WithPeer(ctx, ports.Peer{PID: sc.PID, AuditToken: sc.AuditToken})
			}
			return ctx
		},
	}
	s.servers = append(s.servers, hs)
	s.sockets = append(s.sockets, sock)
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = s.actor.Send(s.machineCtx, daemon.Event{Kind: daemon.Failed, Note: "server: " + err.Error()})
		}
	}()
	s.log.Info("serving", "socket", sock)
}
