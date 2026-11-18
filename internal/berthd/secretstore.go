package berthd

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
)

type flight struct {
	done    chan struct{}
	outcome ports.StoreOutcome
	err     error
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
			s.secrets.output(entity, s.revokeAll(ctx, entityID(entity, secret.Entity), proxysession.StopSecretRemoved))
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
