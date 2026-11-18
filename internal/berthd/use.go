package berthd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/envpolicy"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/core/secret"
)

type useRequest struct {
	grantID int64
	name    string
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
	pid     int
	kind    ports.ConsumerKind
	action  ports.TicketAction
	expires time.Time
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
	s.live[t] = liveTicket{grantID: u.grantID, name: u.name, pid: u.pid, kind: u.kind, action: ports.TicketUse, expires: expires}
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
		return s.authorize(ctx, *match, peer)
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

func (s *secrets) authorize(ctx context.Context, g ports.GrantRecord, peer ports.Peer) (*berthv1.AuthorizeUseResponse, error) {
	got, err := s.issueFor(ctx, useRequest{grantID: g.ID, name: g.Secret, pid: peer.PID, kind: kindOf(g)})
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

// argsMatch applies a grant's glob to the arguments joined by spaces; an empty glob allows any.
func argsMatch(glob, args string) bool {
	if glob == "" {
		return true
	}
	ok, err := path.Match(glob, args)
	return err == nil && ok
}

func shortHex(sum [32]byte) string { return hex.EncodeToString(sum[:6]) }
