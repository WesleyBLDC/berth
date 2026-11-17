package berthd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/codetrust"
	"berth/internal/core/envpolicy"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
	"berth/internal/proxyctl"
	"berth/internal/proxyproc"
)

const (
	// ticketLife is how long a use ticket stays redeemable.
	ticketLife = 30 * time.Second
	// promptTimeout bounds a dialog or Touch ID prompt the person walked away from.
	promptTimeout = 5 * time.Minute
	// noteSecretRemoved ends a grant whose secret was removed before it became Active.
	noteSecretRemoved = "secret removed"
)

// interpreters are executables that run other code, so a grant to one lets whatever it runs
// print the secret. Names with versions (python3.12, perl5.34, ruby3.3) match by prefix.
var (
	interpreterNames = []string{
		"sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh", "env", "node", "deno", "bun", "osascript", "php", "lua",
		"tclsh", "expect", "awk", "gawk", "xargs", "find", "make", "vim", "nvim", "emacs", "java", "swift", "ssh",
		// git runs hooks, aliases and configured commands.
		"git",
	}
	interpreterPrefixes = []string{"python", "perl", "ruby"}
)

func isInterpreter(exe ports.Executable) bool {
	if exe.Shebang {
		return true
	}
	base := filepath.Base(exe.Path)
	if slices.Contains(interpreterNames, base) {
		return true
	}
	for _, p := range interpreterPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

type secretsDeps struct {
	Log      *slog.Logger
	Store    ports.Store
	Records  ports.SecretRecords
	Clock    ports.Clock
	Approver ports.Approver
	Execs    ports.ExecIdentity
	Peers    ports.PeerFacts
	Tickets  ports.TicketSource
	Sessions ports.ProxySessionRecords
	// CLI, Helper and Proxy are the signed berth, berth-native and berth-proxy beside berthd.
	CLI    *codetrust.File
	Helper *codetrust.File
	Proxy  *codetrust.File
	// PID is berthd's own; rotate and remove tickets are bound to it, since berthd starts the
	// helper that redeems them.
	PID int
	// Exits reports when a process exits (kqueue NOTE_EXIT in berthd).
	Exits func(ctx context.Context, pid int) (<-chan struct{}, error)
	// AllowLocalTargets accepts localhost and IP literal proxy grants; only berth_e2e builds.
	AllowLocalTargets bool
	// StartProxyProcess starts the berth-proxy beside berthd for StartProxy; nil refuses it.
	StartProxyProcess func(ctx context.Context) (*proxyproc.Process, error)
	// AgentLive reports whether an agent session still serves calls; nil means none ever does.
	// berthd sets it once both secrets and agents exist, before serving.
	AgentLive func(ctx context.Context, id string) bool
}

// secrets runs one actor per secret and per grant and performs their effects. It implements
// api.Secrets; every decision about states is in the core machines.
type secrets struct {
	secretsDeps
	// ctx outlives any one call, so a dialog keeps going when the caller that opened it leaves.
	ctx context.Context
	// stopping closes when the daemon stops, which cancels every open prompt.
	stopping chan struct{}
	stopOnce sync.Once
	secrets  *actors[secret.State, secret.Event, secret.Effect]
	grants   *actors[grant.State, grant.Event, grant.Effect]
	sessions *actors[proxysession.State, proxysession.Event, proxysession.Effect]
	// readyTimeout is how long a proxy has from AuthorizeProxy to opening its watch stream before
	// its session is Lost.
	readyTimeout time.Duration

	mu      sync.Mutex
	flights map[string]*flight
	live    map[ports.Ticket]liveTicket
	running map[string]*proxySession
	// started maps the pid of each berth-proxy berthd started to the call that started it and
	// its agent session ("" for the owner); starting and startTimes count each agent session's
	// running starts and its starts in the last hour, for its limits. All are in memory.
	started    map[int]*startRecord
	starting   map[string]int
	startTimes map[string][]time.Time
	// stopWait is how long a started proxy has to exit after its stream closed.
	stopWait time.Duration
}

type flight struct {
	done    chan struct{}
	outcome ports.StoreOutcome
	err     error
}

type useRequest struct {
	grantID int64
	name    string
	sha256  [32]byte
	pid     int
	kind    ports.ConsumerKind
}

type issued struct {
	ticket  ports.Ticket
	expires time.Time
	err     error
}

// liveTicket binds a ticket to what it may do when redeemed; berth-native refuses a ticket whose
// consumer kind is not its parent's.
type liveTicket struct {
	// grantID is the grant a use ticket was issued on, so a revoke can drop it; 0 for actions.
	grantID int64
	name    string
	sha256  [32]byte
	pid     int
	kind    ports.ConsumerKind
	action  ports.TicketAction
	expires time.Time
}

var (
	consumerKinds = map[ports.ConsumerKind]berthv1.ConsumerKind{
		ports.ConsumerExe:   berthv1.ConsumerKind_CONSUMER_KIND_EXECUTABLE,
		ports.ConsumerProxy: berthv1.ConsumerKind_CONSUMER_KIND_PROXY,
	}
	signatureKinds = map[ports.Signature]berthv1.SignatureKind{
		ports.SignatureUnsigned: berthv1.SignatureKind_SIGNATURE_KIND_UNSIGNED,
		ports.SignatureAdHoc:    berthv1.SignatureKind_SIGNATURE_KIND_AD_HOC,
		ports.SignatureApple:    berthv1.SignatureKind_SIGNATURE_KIND_APPLE_PLATFORM,
		ports.SignatureTeam:     berthv1.SignatureKind_SIGNATURE_KIND_TEAM,
		ports.SignatureOther:    berthv1.SignatureKind_SIGNATURE_KIND_OTHER,
	}
	ticketActions = map[ports.TicketAction]berthv1.TicketAction{
		ports.TicketUse:    berthv1.TicketAction_TICKET_ACTION_USE,
		ports.TicketRemove: berthv1.TicketAction_TICKET_ACTION_REMOVE,
		ports.TicketRotate: berthv1.TicketAction_TICKET_ACTION_ROTATE,
	}
)

var _ api.Secrets = (*secrets)(nil)

func newSecrets(ctx context.Context, d secretsDeps) *secrets {
	s := &secrets{
		secretsDeps: d, ctx: ctx, stopping: make(chan struct{}), readyTimeout: proxyctl.ReadyTimeout,
		flights: map[string]*flight{}, live: map[ports.Ticket]liveTicket{}, running: map[string]*proxySession{},
		started: map[int]*startRecord{}, starting: map[string]int{}, startTimes: map[string][]time.Time{},
		stopWait: proxyctl.StopTimeout,
	}
	s.secrets = &actors[secret.State, secret.Event, secret.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: secret.Kind, initial: secret.Initial, transition: secret.Transition, run: s.runSecret,
		live: map[string]*slot[secret.State, secret.Event, secret.Effect]{},
	}
	s.grants = &actors[grant.State, grant.Event, grant.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: grant.Kind, initial: grant.Initial, transition: grant.Transition, run: s.runGrant,
		live: map[string]*slot[grant.State, grant.Event, grant.Effect]{},
	}
	s.sessions = &actors[proxysession.State, proxysession.Event, proxysession.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: proxysession.Kind, initial: proxysession.Initial, transition: proxysession.Transition, run: s.runSession,
		live: map[string]*slot[proxysession.State, proxysession.Event, proxysession.Effect]{},
	}
	return s
}

// recover ends what a previous run left open: proxy sessions become Lost, since their proxies
// lost their watch streams with it; a dialog that died with it fails as "interrupted"; and the
// grants of a removed secret are revoked, in case the run ended before it revoked them all.
func (s *secrets) recover(ctx context.Context) error {
	if err := s.recoverSessions(ctx); err != nil {
		return err
	}
	rows, err := s.Records.Secrets(ctx)
	if err != nil {
		return err
	}
	interrupted := map[secret.State]secret.EventKind{
		secret.Pending:  secret.StoreFailed,
		secret.Rotating: secret.RotateFailed,
		secret.Removing: secret.RemoveFailed,
	}
	for _, r := range rows {
		entity := secret.Entity(r.Name)
		st, err := s.state(ctx, entity, "")
		if err != nil {
			return err
		}
		if secret.State(st) == secret.Removed {
			s.revokeAll(ctx, r.Name, proxysession.StopSecretRemoved)
			continue
		}
		kind, ok := interrupted[secret.State(st)]
		if !ok {
			continue
		}
		if _, _, err := step(ctx, s.secrets, entity, secret.Event{Kind: kind, Note: "interrupted"}, nil, kind); err != nil {
			return fmt.Errorf("end interrupted %s of %s: %w", st, r.Name, err)
		}
	}
	return nil
}

// stop tells every running proxy session to stop and gives the proxies a moment to end their
// sessions, then cancels open prompts, which kills their helper processes, and waits up to
// timeout in all for each to record its ending, so no store is left Pending by a clean stop.
func (s *secrets) stop(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	sessionsDone := s.stopAllSessions(min(timeout, sessionStopWait))
	s.stopOnce.Do(func() { close(s.stopping) })
	secretsDone := s.secrets.wait(max(time.Until(deadline), 0))
	grantsDone := s.grants.wait(max(time.Until(deadline), 0))
	return s.sessions.wait(max(time.Until(deadline), 0)) && grantsDone && secretsDone && sessionsDone
}

func (s *secrets) stopped() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// cancelOnStop ends a prompt's context when the daemon stops, or returns when the prompt ends.
func (s *secrets) cancelOnStop(ctx context.Context, cancel context.CancelFunc) {
	select {
	case <-s.stopping:
		cancel()
	case <-ctx.Done():
	}
}

// promptNote is the audit note for a prompt that did not succeed.
func (s *secrets) promptNote() string {
	if s.stopped() {
		return "berthd stopped while the prompt was open"
	}
	return "the helper reported a failure"
}

func (s *secrets) RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest, peer ports.Peer) (*berthv1.RequestSecretResponse, error) {
	name := req.GetName()
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return nil, err
	}
	outcome, err := s.storeOnce(ctx, ports.StorePrompt{Name: name, Reason: req.GetReason(), Requester: facts})
	if err != nil {
		return nil, err
	}
	resp := &berthv1.RequestSecretResponse{Name: name, Outcome: berthv1.StoreOutcome_STORE_OUTCOME_FAILED}
	switch outcome {
	case ports.StoreStored:
		resp.Outcome = berthv1.StoreOutcome_STORE_OUTCOME_STORED
	case ports.StoreDenied:
		resp.Outcome = berthv1.StoreOutcome_STORE_OUTCOME_DENIED
	case ports.StoreFailed:
	}
	return resp, nil
}

// storeOnce holds one store dialog per name at a time: a caller who arrives while one is open
// gets that dialog's outcome instead of opening a second.
func (s *secrets) storeOnce(ctx context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	s.mu.Lock()
	f, joined := s.flights[p.Name]
	if !joined {
		f = &flight{done: make(chan struct{})}
		s.flights[p.Name] = f
		go func() {
			f.outcome, f.err = s.store(p)
			s.mu.Lock()
			delete(s.flights, p.Name)
			s.mu.Unlock()
			close(f.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.outcome, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *secrets) store(p ports.StorePrompt) (ports.StoreOutcome, error) {
	ctx := s.ctx
	entity := secret.Entity(p.Name)
	state, err := s.state(ctx, entity, string(secret.Initial))
	if err != nil {
		return "", err
	}
	switch secret.State(state) {
	case secret.Stored:
		return "", api.ErrSecretExists
	case secret.Rotating, secret.Removing:
		return "", api.ErrBusy
	case secret.Pending, secret.Denied, secret.Failed, secret.Removed:
	}
	if err := s.Records.AddSecret(ctx, p.Name, s.Clock.Now()); err != nil {
		return "", err
	}
	rec, _, err := step(ctx, s.secrets, entity, secret.Event{Kind: secret.StoreRequested}, p,
		secret.StoreSucceeded, secret.StoreDenied, secret.StoreFailed)
	if err != nil {
		return "", err
	}
	return map[secret.EventKind]ports.StoreOutcome{
		secret.StoreSucceeded: ports.StoreStored,
		secret.StoreDenied:    ports.StoreDenied,
		secret.StoreFailed:    ports.StoreFailed,
	}[secret.EventKind(rec.Event)], nil
}

func (s *secrets) runSecret(entity string) machine.Runner[secret.Event, secret.Effect] {
	return func(ctx context.Context, effect secret.Effect) ([]secret.Event, error) {
		switch effect.Kind {
		case secret.PromptRotate:
			return s.promptRotate(ctx, entity), nil
		case secret.PromptRemove:
			return s.promptRemove(ctx, entity), nil
		case secret.RevokeAll:
			s.secrets.output(entity, s.revokeAll(ctx, strings.TrimPrefix(entity, "secret/"), proxysession.StopSecretRemoved))
			return nil, nil
		case secret.PromptStore:
			p, ok := s.secrets.input(entity).(ports.StorePrompt)
			if !ok {
				return []secret.Event{{Kind: secret.StoreFailed, Note: "no prompt to show"}}, nil
			}
			ctx, cancel := context.WithTimeout(ctx, promptTimeout)
			defer cancel()
			go s.cancelOnStop(ctx, cancel)
			outcome, err := s.Approver.StoreSecret(ctx, p)
			if err != nil {
				s.Log.Warn("store prompt failed", "secret", p.Name, "err", err)
			}
			switch outcome {
			case ports.StoreStored:
				return []secret.Event{{Kind: secret.StoreSucceeded}}, nil
			case ports.StoreDenied:
				return []secret.Event{{Kind: secret.StoreDenied}}, nil
			case ports.StoreFailed:
			}
			return []secret.Event{{Kind: secret.StoreFailed, Note: s.promptNote()}}, nil
		}
		return nil, fmt.Errorf("unknown secret effect %q", effect.Kind)
	}
}

// ListSecrets answers an agent session with each secret's name and state alone: the consumers
// describe the owner's executables, hosts, headers and habits, which no agent needs to use a
// secret.
func (s *secrets) ListSecrets(ctx context.Context) (*berthv1.ListSecretsResponse, error) {
	rows, err := s.Records.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	agent := api.AgentSession(ctx) != ""
	resp := &berthv1.ListSecretsResponse{}
	for _, r := range rows {
		state, err := s.state(ctx, secret.Entity(r.Name), string(secret.Initial))
		if err != nil {
			return nil, err
		}
		if agent {
			resp.Secrets = append(resp.Secrets, &berthv1.SecretSummary{Name: r.Name, State: secretState(secret.State(state))})
			continue
		}
		summary := &berthv1.SecretSummary{Name: r.Name, State: secretState(secret.State(state)), CreatedAt: timestamppb.New(r.CreatedAt)}
		grants, err := s.Records.Grants(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			gs, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
			if err != nil {
				return nil, err
			}
			summary.Consumers = append(summary.Consumers, consumer(g, grant.State(gs)))
		}
		resp.Secrets = append(resp.Secrets, summary)
	}
	return resp, nil
}

func (s *secrets) GrantSecret(ctx context.Context, req *berthv1.GrantSecretRequest, peer ports.Peer) (*berthv1.GrantSecretResponse, error) {
	name, glob := req.GetName(), req.GetArgsGlob()
	if _, err := path.Match(glob, ""); err != nil {
		return nil, api.ErrBadGlob
	}
	if err := s.requireStored(ctx, name); err != nil {
		return nil, err
	}
	exe, err := s.identify(ctx, req.GetExePath())
	if err != nil {
		return nil, err
	}
	interpreter := isInterpreter(exe)
	if interpreter && !req.GetAllowInterpreter() {
		return nil, api.ErrInterpreter
	}
	// berth run starts only a child whose live cdhash is this one, so a grant without one could
	// never be used.
	if exe.CDHash == (ports.CDHash{}) {
		s.Log.Info("executable has no code directory hash to pin", "path", exe.Path)
		return nil, api.ErrExecutableNotFound
	}
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return nil, err
	}
	passEnv := envpolicy.Filter(req.GetPassEnv())
	g := ports.GrantRecord{Secret: name, Exe: exe, ArgsGlob: glob, PassEnv: passEnv, CreatedAt: s.Clock.Now()}
	if g.ID, err = s.Records.AddGrant(ctx, g); err != nil {
		return nil, err
	}
	entity := grant.Entity(g.ID)
	prompt := ports.PowerPrompt{
		Action: ports.PowerGrant, Secret: name, Consumer: exe, ArgsGlob: glob, Interpreter: interpreter, PassEnv: passEnv, Requester: facts,
	}
	rec, _, err := step(ctx, s.grants, entity, grant.Event{Kind: grant.GrantRequested}, prompt,
		grant.GrantApproved, grant.GrantDenied, grant.GrantFailed)
	if err != nil {
		return nil, err
	}
	if grant.EventKind(rec.Event) == grant.GrantApproved {
		return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED, Consumer: consumer(g, grant.Active)}, nil
	}
	if grant.EventKind(rec.Event) == grant.GrantDenied {
		return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_DENIED}, nil
	}
	return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_FAILED}, nil
}

func (s *secrets) runGrant(entity string) machine.Runner[grant.Event, grant.Effect] {
	return func(ctx context.Context, effect grant.Effect) ([]grant.Event, error) {
		switch effect.Kind {
		case grant.PromptPower:
			p, ok := s.grants.input(entity).(ports.PowerPrompt)
			if !ok {
				return []grant.Event{{Kind: grant.GrantFailed, Note: "no prompt to show"}}, nil
			}
			// A remove can start between the grant call's check and this prompt, or finish while
			// the prompt is open; an approval must never land on a secret whose value is going.
			if !s.servesUses(ctx, p.Secret) {
				return []grant.Event{{Kind: grant.GrantFailed, Note: noteSecretRemoved}}, nil
			}
			ctx, cancel := context.WithTimeout(ctx, promptTimeout)
			defer cancel()
			go s.cancelOnStop(ctx, cancel)
			outcome, err := s.Approver.ConfirmPower(ctx, p)
			if err != nil {
				s.Log.Warn("grant prompt failed", "secret", p.Secret, "err", err)
			}
			switch outcome {
			case ports.PowerApproved:
				if !s.servesUses(ctx, p.Secret) {
					return []grant.Event{{Kind: grant.GrantFailed, Note: noteSecretRemoved}}, nil
				}
				return []grant.Event{{Kind: grant.GrantApproved}}, nil
			case ports.PowerDenied:
				return []grant.Event{{Kind: grant.GrantDenied}}, nil
			case ports.PowerFailed:
			}
			return []grant.Event{{Kind: grant.GrantFailed, Note: s.promptNote()}}, nil
		case grant.IssueTicket:
			s.issue(ctx, entity)
			return nil, nil
		}
		return nil, fmt.Errorf("unknown grant effect %q", effect.Kind)
	}
}

// issue makes the ticket for the use its step carried in and counts the use. A failure is
// handed back to the step, which answers with an internal error; the grant stays Active.
func (s *secrets) issue(ctx context.Context, entity string) {
	u, ok := s.grants.input(entity).(useRequest)
	if !ok {
		s.grants.output(entity, issued{err: errors.New("no use request in this step")})
		return
	}
	t, err := s.Tickets.NewTicket(ctx)
	if err != nil {
		s.grants.output(entity, issued{err: fmt.Errorf("new ticket: %w", err)})
		return
	}
	now := s.Clock.Now()
	if err := s.Records.CountUse(ctx, u.grantID, now); err != nil {
		s.grants.output(entity, issued{err: fmt.Errorf("count use: %w", err)})
		return
	}
	expires := now.Add(ticketLife)
	s.mu.Lock()
	s.live[t] = liveTicket{grantID: u.grantID, name: u.name, sha256: u.sha256, pid: u.pid, kind: u.kind, action: ports.TicketUse, expires: expires}
	s.mu.Unlock()
	s.grants.output(entity, issued{ticket: t, expires: expires})
}

func (s *secrets) AuthorizeUse(ctx context.Context, req *berthv1.AuthorizeUseRequest, peer ports.Peer) (*berthv1.AuthorizeUseResponse, error) {
	name := req.GetName()
	// A ticket lets its holder start berth-native and read the value, so only the signed berth
	// CLI, which puts the value straight into its child, may get one.
	if !s.CLI.Runs(ctx, peer) {
		return nil, api.ErrPeerNotTrusted
	}
	if usable, err := s.usable(ctx, name); err != nil {
		return nil, err
	} else if !usable {
		return nil, api.ErrSecretNotFound
	}
	// Identified here, never taken from the client, and matched by the resolved path.
	exe, err := s.identify(ctx, req.GetExePath())
	if err != nil {
		return nil, err
	}
	grants, err := s.Records.Grants(ctx, name)
	if err != nil {
		return nil, err
	}
	args := strings.Join(req.GetArgv(), " ")
	var match *ports.GrantRecord
	var refusing *ports.GrantRecord
	stale, argsRefused := false, false
	for i := range grants {
		g := &grants[i]
		if kindOf(*g) != ports.ConsumerExe || g.Exe.Path != exe.Path {
			continue
		}
		st, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
		if err != nil {
			return nil, err
		}
		switch grant.State(st) {
		case grant.Active:
			// A grant from before cdhash pins, or a script whose interpreter changed, is stale too.
			if g.Exe.SHA256 != exe.SHA256 || g.Exe.CDHash != exe.CDHash {
				note := "sha256 now " + shortHex(exe.SHA256)
				if g.Exe.SHA256 == exe.SHA256 {
					note = "cdhash now " + hex.EncodeToString(exe.CDHash[:6])
				}
				if _, _, err := step(ctx, s.grants, grant.Entity(g.ID), grant.Event{Kind: grant.HashChanged, Note: note}, nil, grant.HashChanged); err != nil {
					return nil, err
				}
				stale = true
				continue
			}
			if match == nil && argsMatch(g.ArgsGlob, args) {
				match = g
			} else if match == nil {
				argsRefused = true
			}
		case grant.Stale:
			stale = true
			if refusing == nil {
				refusing = g
			}
		case grant.Denied, grant.Revoked:
			if refusing == nil {
				refusing = g
			}
		case grant.Requested:
		}
	}
	if match != nil {
		return s.authorize(ctx, *match, exe, peer)
	}
	outcome := berthv1.UseOutcome_USE_OUTCOME_NO_GRANT
	if stale {
		outcome = berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT
	}
	switch {
	case refusing != nil:
		_, _, err = step(ctx, s.grants, grant.Entity(refusing.ID), grant.Event{Kind: grant.UseRefused}, nil, grant.UseRefused)
	case stale:
		// The HashChanged row already records this refusal.
	default:
		note := "no grant for " + exe.Path
		if argsRefused {
			note = "arguments do not match any grant for " + exe.Path
		}
		_, _, err = step(ctx, s.secrets, secret.Entity(name), secret.Event{Kind: secret.UseRefused, Note: note}, nil, secret.UseRefused)
	}
	if err != nil {
		return nil, err
	}
	return &berthv1.AuthorizeUseResponse{Outcome: outcome}, nil
}

func (s *secrets) authorize(ctx context.Context, g ports.GrantRecord, exe ports.Executable, peer ports.Peer) (*berthv1.AuthorizeUseResponse, error) {
	got, err := s.issueFor(ctx, useRequest{grantID: g.ID, name: g.Secret, sha256: exe.SHA256, pid: peer.PID, kind: kindOf(g)})
	if err != nil {
		return nil, err
	}
	return &berthv1.AuthorizeUseResponse{
		Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, Ticket: got.ticket[:], ExpiresAt: timestamppb.New(got.expires),
		PassEnv: envpolicy.Filter(g.PassEnv), Cdhash: g.Exe.CDHash[:],
	}, nil
}

// issueFor sends UseAuthorized to the grant and returns the ticket its IssueTicket effect made.
func (s *secrets) issueFor(ctx context.Context, use useRequest) (issued, error) {
	entity := grant.Entity(use.grantID)
	_, out, err := step(ctx, s.grants, entity, grant.Event{Kind: grant.UseAuthorized}, use, grant.UseAuthorized)
	if err != nil {
		return issued{}, err
	}
	got, ok := out.(issued)
	if !ok {
		return issued{}, errors.New("no ticket was issued")
	}
	if got.err != nil {
		s.Log.Error("ticket not issued", "entity", entity, "err", got.err)
		return issued{}, got.err
	}
	return got, nil
}

func kindOf(g ports.GrantRecord) ports.ConsumerKind {
	if g.Kind == "" {
		return ports.ConsumerExe
	}
	return g.Kind
}

// TrustRedeemer accepts only a peer running the signed berth-native beside berthd, judged by
// the kernel's live code identity rather than any path.
func (s *secrets) TrustRedeemer(ctx context.Context, peer ports.Peer) error {
	if !s.Helper.Runs(ctx, peer) {
		return api.ErrPeerNotTrusted
	}
	return nil
}

// RedeemUseTicket spends a ticket. It is gone after the first attempt, even one that fails a
// later check, so a ticket is never tried twice.
func (s *secrets) RedeemUseTicket(ctx context.Context, ticket ports.Ticket, peer ports.Peer) (*berthv1.RedeemUseTicketResponse, error) {
	now := s.Clock.Now()
	s.mu.Lock()
	t, ok := s.live[ticket]
	delete(s.live, ticket)
	for k, v := range s.live {
		if now.After(v.expires) {
			delete(s.live, k)
		}
	}
	s.mu.Unlock()
	if !ok || now.After(t.expires) {
		return nil, api.ErrTicketInvalid
	}
	// The helper redeeming must be a child of the berth process the ticket was issued to,
	// which is how berth run starts it.
	facts, err := s.Peers.Facts(ctx, peer.PID)
	if err != nil || len(facts.Tree) == 0 || facts.Tree[0].PID != t.pid {
		s.Log.Warn("ticket redeemed by a helper the caller did not start", "pid", peer.PID)
		return nil, api.ErrTicketInvalid
	}
	return &berthv1.RedeemUseTicketResponse{Name: t.name, ConsumerKind: consumerKinds[t.kind], Action: ticketActions[t.action]}, nil
}

func (s *secrets) facts(ctx context.Context, peer ports.Peer) (ports.Requester, error) {
	if peer.PID <= 0 {
		return ports.Requester{}, api.ErrCallerUnknown
	}
	r, err := s.Peers.Facts(ctx, peer.PID)
	if err != nil {
		s.Log.Warn("cannot describe the caller", "pid", peer.PID, "err", err)
		return ports.Requester{}, api.ErrCallerUnknown
	}
	return r, nil
}

func (s *secrets) identify(ctx context.Context, exePath string) (ports.Executable, error) {
	exe, err := s.Execs.Identify(ctx, exePath)
	if err != nil {
		s.Log.Info("cannot identify executable", "path", exePath, "err", err)
		return ports.Executable{}, api.ErrExecutableNotFound
	}
	return exe, nil
}

// requireStored answers BUSY while a dialog is open on the secret and SECRET_NOT_FOUND when it
// holds no value, so a grant, rotate or remove starts only on a Stored secret.
func (s *secrets) requireStored(ctx context.Context, name string) error {
	st, err := s.state(ctx, secret.Entity(name), "")
	if err != nil {
		return err
	}
	switch secret.State(st) {
	case secret.Stored:
		return nil
	case secret.Pending, secret.Rotating, secret.Removing:
		return api.ErrBusy
	case secret.Denied, secret.Failed, secret.Removed:
	}
	return api.ErrSecretNotFound
}

// usable reports whether a secret serves uses: Stored, or Rotating, which keeps the old value
// until the new one is in.
func (s *secrets) usable(ctx context.Context, name string) (bool, error) {
	st, err := s.state(ctx, secret.Entity(name), "")
	if err != nil {
		return false, err
	}
	return secret.State(st) == secret.Stored || secret.State(st) == secret.Rotating, nil
}

// servesUses is usable for an effect, which has no caller to hand an error to: a store that
// cannot be read counts as not usable.
func (s *secrets) servesUses(ctx context.Context, name string) bool {
	ok, err := s.usable(ctx, name)
	if err != nil {
		s.Log.Error("cannot read the secret's state", "secret", name, "err", err)
	}
	return ok
}

// state reads an entity's persisted state; actors save before they act, so this is current.
func (s *secrets) state(ctx context.Context, entity, initial string) (string, error) {
	st, found, err := s.Store.LoadState(ctx, entity)
	if err != nil {
		return "", err
	}
	if !found {
		return initial, nil
	}
	return st.State, nil
}

// argsMatch applies a grant's glob to the arguments joined by spaces; an empty glob allows any.
func argsMatch(glob, args string) bool {
	if glob == "" {
		return true
	}
	ok, err := path.Match(glob, args)
	return err == nil && ok
}

func shortHex(sum [32]byte) string { return hex.EncodeToString(sum[:6]) }

func consumer(g ports.GrantRecord, st grant.State) *berthv1.Consumer {
	kind := kindOf(g)
	c := &berthv1.Consumer{
		GrantId: g.ID, State: grantState(st), Kind: consumerKinds[kind], ExePath: g.Exe.Path,
		TeamId: g.Exe.TeamID, SigningId: g.Exe.SigningID, ArgsGlob: g.ArgsGlob,
		CreatedAt: timestamppb.New(g.CreatedAt), UseCount: g.UseCount, PassEnv: g.PassEnv,
	}
	if kind == ports.ConsumerProxy {
		c.Proxy = proxyTarget(g.Proxy)
	} else {
		c.Sha256 = hex.EncodeToString(g.Exe.SHA256[:])
		c.Signature = signatureKinds[g.Exe.Signature]
	}
	if !g.LastUsedAt.IsZero() {
		c.LastUsedAt = timestamppb.New(g.LastUsedAt)
	}
	return c
}

func secretState(s secret.State) berthv1.SecretState {
	switch s {
	case secret.Pending:
		return berthv1.SecretState_SECRET_STATE_PENDING
	case secret.Stored:
		return berthv1.SecretState_SECRET_STATE_STORED
	case secret.Denied:
		return berthv1.SecretState_SECRET_STATE_DENIED
	case secret.Failed:
		return berthv1.SecretState_SECRET_STATE_FAILED
	case secret.Rotating:
		return berthv1.SecretState_SECRET_STATE_ROTATING
	case secret.Removing:
		return berthv1.SecretState_SECRET_STATE_REMOVING
	case secret.Removed:
		return berthv1.SecretState_SECRET_STATE_REMOVED
	}
	return berthv1.SecretState_SECRET_STATE_UNSPECIFIED
}

func grantState(s grant.State) berthv1.GrantState {
	switch s {
	case grant.Requested:
		return berthv1.GrantState_GRANT_STATE_REQUESTED
	case grant.Active:
		return berthv1.GrantState_GRANT_STATE_ACTIVE
	case grant.Denied:
		return berthv1.GrantState_GRANT_STATE_DENIED
	case grant.Stale:
		return berthv1.GrantState_GRANT_STATE_STALE
	case grant.Revoked:
		return berthv1.GrantState_GRANT_STATE_REVOKED
	}
	return berthv1.GrantState_GRANT_STATE_UNSPECIFIED
}
