package berthd

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
)

func (s *secrets) GrantProxy(ctx context.Context, req *berthv1.GrantProxyRequest, peer ports.Peer) (*berthv1.GrantProxyResponse, error) {
	name := req.GetName()
	target := targetOf(req.GetTarget())
	if !s.AllowLocalTargets && localHost(target.Host) {
		return nil, api.ErrBadProxyTarget
	}
	if err := s.requireStored(ctx, name); err != nil {
		return nil, err
	}
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return nil, err
	}
	g := ports.GrantRecord{Secret: name, Kind: ports.ConsumerProxy, Proxy: target, CreatedAt: s.Clock.Now()}
	if g.ID, err = s.Records.AddGrant(ctx, g); err != nil {
		return nil, err
	}
	prompt := ports.PowerPrompt{Action: ports.PowerProxyGrant, Secret: name, Proxy: target, Requester: facts}
	rec, _, err := step(ctx, s.grants, grant.Entity(g.ID), grant.Event{Kind: grant.GrantRequested}, prompt,
		grant.GrantApproved, grant.GrantDenied, grant.GrantFailed)
	if err != nil {
		return nil, err
	}
	if grant.EventKind(rec.Event) == grant.GrantApproved {
		return &berthv1.GrantProxyResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED, Consumer: consumer(g, grant.Active)}, nil
	}
	if grant.EventKind(rec.Event) == grant.GrantDenied {
		return &berthv1.GrantProxyResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_DENIED}, nil
	}
	return &berthv1.GrantProxyResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_FAILED}, nil
}

// localHost is a name that reaches this Mac or a private network rather than a public API: an IP
// literal, an all numeric last label (which resolvers read as an address, like 127.1), or
// localhost.
func localHost(host string) bool {
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	last := host[strings.LastIndexByte(host, '.')+1:]
	return strings.Trim(last, "0123456789") == ""
}

// RevokeGrant needs no Touch ID: it only lowers privilege. A grant that cannot be revoked
// (Requested or Denied) answers its state, and so does one already Revoked.
func (s *secrets) RevokeGrant(ctx context.Context, req *berthv1.RevokeGrantRequest, _ ports.Peer) (*berthv1.RevokeGrantResponse, error) {
	id := req.GetGrantId()
	if _, ok, err := s.Records.Grant(ctx, id); err != nil {
		return nil, err
	} else if !ok {
		return nil, api.ErrGrantNotFound
	}
	_, _, err := step(ctx, s.grants, grant.Entity(id), grant.Event{Kind: grant.Revoke, Note: "revoked by berth secret revoke"}, nil, grant.Revoke)
	if err != nil && !errors.Is(err, machine.ErrIllegal) {
		return nil, err
	}
	s.stopSessions(ctx, id, proxysession.StopGrantRevoked)
	st, err := s.state(ctx, grant.Entity(id), string(grant.Initial))
	if err != nil {
		return nil, err
	}
	return &berthv1.RevokeGrantResponse{State: grantState(grant.State(st))}, nil
}

func (s *secrets) RemoveSecret(ctx context.Context, req *berthv1.RemoveSecretRequest, peer ports.Peer) (*berthv1.RemoveSecretResponse, error) {
	rec, out, err := s.lifecycle(ctx, req.GetName(), peer, secret.RemoveRequested, secret.RemoveSucceeded, secret.RemoveDenied, secret.RemoveFailed)
	if err != nil {
		return nil, err
	}
	if secret.EventKind(rec.Event) == secret.RemoveSucceeded {
		ids, _ := out.([]int64)
		return &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED, RevokedGrantIds: ids}, nil
	}
	if secret.EventKind(rec.Event) == secret.RemoveDenied {
		return &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_DENIED}, nil
	}
	return &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_FAILED}, nil
}

func (s *secrets) RotateSecret(ctx context.Context, req *berthv1.RotateSecretRequest, peer ports.Peer) (*berthv1.RotateSecretResponse, error) {
	rec, _, err := s.lifecycle(ctx, req.GetName(), peer, secret.RotateRequested, secret.RotateSucceeded, secret.RotateDenied, secret.RotateFailed)
	if err != nil {
		return nil, err
	}
	if secret.EventKind(rec.Event) == secret.RotateSucceeded {
		return &berthv1.RotateSecretResponse{Outcome: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, nil
	}
	if secret.EventKind(rec.Event) == secret.RotateDenied {
		return &berthv1.RotateSecretResponse{Outcome: berthv1.StoreOutcome_STORE_OUTCOME_DENIED}, nil
	}
	return &berthv1.RotateSecretResponse{Outcome: berthv1.StoreOutcome_STORE_OUTCOME_FAILED}, nil
}

// lifecycle answers BUSY or SECRET_NOT_FOUND before it sends a rotate or remove the secret's
// table would refuse, then waits for the prompt's outcome.
func (s *secrets) lifecycle(ctx context.Context, name string, peer ports.Peer, request secret.EventKind, until ...secret.EventKind) (ports.TransitionRecord, any, error) {
	if err := s.requireStored(ctx, name); err != nil {
		return ports.TransitionRecord{}, nil, err
	}
	// A grant waiting for Touch ID would make the person approve one thing while removing
	// another; the grant's own check and revokeAll make sure it never ends Active either way.
	grants, err := s.Records.Grants(ctx, name)
	if err != nil {
		return ports.TransitionRecord{}, nil, err
	}
	for _, g := range grants {
		gs, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
		if err != nil {
			return ports.TransitionRecord{}, nil, err
		}
		if grant.State(gs) == grant.Requested {
			return ports.TransitionRecord{}, nil, api.ErrBusy
		}
	}
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return ports.TransitionRecord{}, nil, err
	}
	rec, out, err := step(ctx, s.secrets, secret.Entity(name), secret.Event{Kind: request},
		ports.LifecyclePrompt{Name: name, Requester: facts}, until...)
	if errors.Is(err, machine.ErrIllegal) {
		// Another rotate or remove got there first.
		return ports.TransitionRecord{}, nil, api.ErrBusy
	}
	return rec, out, err
}

func (s *secrets) promptRotate(ctx context.Context, entity string) []secret.Event {
	p, ticket, ok := s.lifecyclePrompt(ctx, entity, ports.TicketRotate)
	if !ok {
		return []secret.Event{{Kind: secret.RotateFailed, Note: "no prompt to show"}}
	}
	defer s.dropTicket(ticket)
	ctx, cancel := context.WithTimeout(ctx, promptTimeout)
	defer cancel()
	go s.cancelOnStop(ctx, cancel)
	outcome, err := s.Approver.RotateSecret(ctx, p, ticket)
	if err != nil {
		s.Log.Warn("rotate prompt failed", "secret", p.Name, "err", err)
	}
	switch outcome {
	case ports.StoreStored:
		return []secret.Event{{Kind: secret.RotateSucceeded}}
	case ports.StoreDenied:
		return []secret.Event{{Kind: secret.RotateDenied}}
	case ports.StoreFailed:
	}
	return []secret.Event{{Kind: secret.RotateFailed, Note: s.promptNote()}}
}

func (s *secrets) promptRemove(ctx context.Context, entity string) []secret.Event {
	p, ticket, ok := s.lifecyclePrompt(ctx, entity, ports.TicketRemove)
	if !ok {
		return []secret.Event{{Kind: secret.RemoveFailed, Note: "no prompt to show"}}
	}
	defer s.dropTicket(ticket)
	ctx, cancel := context.WithTimeout(ctx, promptTimeout)
	defer cancel()
	go s.cancelOnStop(ctx, cancel)
	outcome, err := s.Approver.RemoveSecret(ctx, p, ticket)
	if err != nil {
		s.Log.Warn("remove prompt failed", "secret", p.Name, "err", err)
	}
	switch outcome {
	case ports.RemoveRemoved:
		return []secret.Event{{Kind: secret.RemoveSucceeded}}
	case ports.RemoveDenied:
		return []secret.Event{{Kind: secret.RemoveDenied}}
	case ports.RemoveFailed:
	}
	return []secret.Event{{Kind: secret.RemoveFailed, Note: s.promptNote()}}
}

// lifecyclePrompt reads the step's prompt and makes the action ticket berth-native redeems before
// it shows anything; the ticket is bound to berthd's pid, since berthd starts that helper.
func (s *secrets) lifecyclePrompt(ctx context.Context, entity string, action ports.TicketAction) (ports.LifecyclePrompt, ports.Ticket, bool) {
	p, ok := s.secrets.input(entity).(ports.LifecyclePrompt)
	if !ok {
		return p, ports.Ticket{}, false
	}
	t, err := s.Tickets.NewTicket(ctx)
	if err != nil {
		s.Log.Error("action ticket not issued", "entity", entity, "err", err)
		return p, ports.Ticket{}, false
	}
	s.mu.Lock()
	s.live[t] = liveTicket{name: p.Name, pid: s.PID, action: action, expires: s.Clock.Now().Add(promptTimeout)}
	s.mu.Unlock()
	return p, t, true
}

func (s *secrets) dropTicket(t ports.Ticket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, t)
}

// revokeAll revokes every grant of a removed secret, drops its live use tickets and stops the
// proxy sessions holding its grants, and returns the ids it revoked, lowest first. A failure is
// logged and the rest carry on: boot recovery finishes what one run could not.
func (s *secrets) revokeAll(ctx context.Context, name string, reason proxysession.StopReason) []int64 {
	s.mu.Lock()
	for t, l := range s.live {
		if l.name == name && l.action == ports.TicketUse {
			delete(s.live, t)
		}
	}
	s.mu.Unlock()
	grants, err := s.Records.Grants(ctx, name)
	if err != nil {
		s.Log.Error("cannot list grants to revoke", "secret", name, "err", err)
		return nil
	}
	var ids []int64
	for _, g := range grants {
		st, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
		if err != nil {
			s.Log.Error("cannot read grant state", "grant", g.ID, "err", err)
			continue
		}
		if grant.State(st) == grant.Requested {
			if st, err = s.endRequested(ctx, g.ID); err != nil {
				s.Log.Error("cannot end a requested grant", "grant", g.ID, "err", err)
				continue
			}
		}
		if grant.State(st) == grant.Active || grant.State(st) == grant.Stale {
			_, _, err := step(ctx, s.grants, grant.Entity(g.ID), grant.Event{Kind: grant.Revoke, Note: noteSecretRemoved}, nil, grant.Revoke)
			if err != nil {
				s.Log.Error("cannot revoke grant", "grant", g.ID, "err", err)
				continue
			}
			ids = append(ids, g.ID)
		}
		s.stopSessions(ctx, g.ID, reason)
	}
	slices.Sort(ids)
	return ids
}

// endRequested fails a grant still waiting for Touch ID. Its step waits for the prompt to finish,
// and when the prompt got there first the grant is Denied or Active, which the caller then
// revokes; it returns the grant's state after.
func (s *secrets) endRequested(ctx context.Context, id int64) (string, error) {
	_, _, err := step(ctx, s.grants, grant.Entity(id), grant.Event{Kind: grant.GrantFailed, Note: noteSecretRemoved}, nil, grant.GrantFailed)
	if err != nil && !errors.Is(err, machine.ErrIllegal) {
		return "", err
	}
	return s.state(ctx, grant.Entity(id), string(grant.Initial))
}

func targetOf(t *berthv1.ProxyTarget) ports.ProxyTarget {
	return ports.ProxyTarget{
		Host: t.GetHost(), Port: int(t.GetPort()), HeaderName: t.GetHeaderName(),
		ValuePrefix: t.GetValuePrefix(), ValueSuffix: t.GetValueSuffix(), PathGlob: t.GetPathGlob(),
		Methods: slices.Clone(t.GetMethods()), PlaceholderEnv: t.GetPlaceholderEnv(),
	}
}

func proxyTarget(t ports.ProxyTarget) *berthv1.ProxyTarget {
	return &berthv1.ProxyTarget{
		Host: t.Host, Port: uint32(t.Port), HeaderName: t.HeaderName, //nolint:gosec // ports are 1 to 65535
		ValuePrefix: t.ValuePrefix, ValueSuffix: t.ValueSuffix, PathGlob: t.PathGlob,
		Methods: slices.Clone(t.Methods), PlaceholderEnv: t.PlaceholderEnv,
	}
}
