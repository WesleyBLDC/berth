package berthd

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/envpolicy"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
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
