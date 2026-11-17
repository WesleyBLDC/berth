package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
)

func secretRequest(ctx context.Context, args []string, env Env) int {
	fs := newFlags("secret request", env)
	why := fs.String("why", "", "why you need the secret; shown to the person as your words")
	name, ok := parseNamed(fs, args)
	if !ok || *why == "" {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	return store(ctx, name, *why, env)
}

func secretAdd(ctx context.Context, args []string, env Env) int {
	name, ok := parseNamed(newFlags("secret add", env), args)
	if !ok {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	return store(ctx, name, addReason, env)
}

func store(ctx context.Context, name, reason string, env Env) int {
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: requestID(), Name: name, Reason: reason})
	if err != nil {
		return reportError(env.Stderr, err, name, "")
	}
	switch resp.GetOutcome() {
	case berthv1.StoreOutcome_STORE_OUTCOME_STORED:
		fmt.Fprintf(env.Stdout, "saved %s\n", name)
		return ExitOK
	case berthv1.StoreOutcome_STORE_OUTCOME_DENIED:
		fmt.Fprintln(env.Stderr, "denied")
		return ExitDenied
	case berthv1.StoreOutcome_STORE_OUTCOME_FAILED, berthv1.StoreOutcome_STORE_OUTCOME_UNSPECIFIED:
	}
	fmt.Fprintln(env.Stderr, "failed")
	return ExitFailed
}

func secretList(ctx context.Context, args []string, env Env) int {
	fs := newFlags("secret ls", env)
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.ListSecrets(ctx, &berthv1.ListSecretsRequest{})
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	if *asJSON {
		b, err := protojson.MarshalOptions{Multiline: true}.Marshal(resp)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "%s\n", b)
		return ExitOK
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tCONSUMERS\tLAST USED\tUSES")
	for _, s := range resp.GetSecrets() {
		var last *timestamppb.Timestamp
		var uses int64
		for _, cons := range s.GetConsumers() {
			uses += cons.GetUseCount()
			if lu := cons.GetLastUsedAt(); lu != nil && (last == nil || lu.AsTime().After(last.AsTime())) {
				last = lu
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\n", s.GetName(), enumWord(s.GetState().String(), "SECRET_STATE_"),
			len(s.GetConsumers()), lastUsed(last), uses)
	}
	_ = w.Flush()
	for _, s := range resp.GetSecrets() {
		if len(s.GetConsumers()) == 0 {
			continue
		}
		fmt.Fprintf(env.Stdout, "\n%s\n", s.GetName())
		w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		for _, cons := range s.GetConsumers() {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", cons.GetExePath(), shortHash(cons.GetSha256()), teamOf(cons),
				argsOf(cons.GetArgsGlob()), enumWord(cons.GetState().String(), "GRANT_STATE_"))
		}
		_ = w.Flush()
	}
	return ExitOK
}

func secretGrant(ctx context.Context, args []string, env Env) int {
	fs := newFlags("secret grant", env)
	exe := fs.String("exe", "", "the executable to pin, by path or name on PATH")
	glob := fs.String("args", "", "limit the arguments it may run with; empty allows any")
	interpreter := fs.Bool("allow-interpreter", false, "grant an interpreter (a shell, python, a #! script), which could print the secret")
	name, ok := parseNamed(fs, args)
	if !ok || *exe == "" {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	path, err := resolveExecutable(*exe)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.GrantSecret(ctx, &berthv1.GrantSecretRequest{
		RequestId: requestID(), Name: name, ExePath: path, ArgsGlob: *glob, AllowInterpreter: *interpreter,
	})
	if err != nil {
		return reportError(env.Stderr, err, name, path)
	}
	switch resp.GetOutcome() {
	case berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED:
		cons := resp.GetConsumer()
		fmt.Fprintf(env.Stdout, "granted %s to %s\n", name, cons.GetExePath())
		w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "  sha256\t%s\n", shortHash(cons.GetSha256()))
		fmt.Fprintf(w, "  team\t%s\n", teamOf(cons))
		fmt.Fprintf(w, "  args\t%s\n", argsOf(cons.GetArgsGlob()))
		_ = w.Flush()
		return ExitOK
	case berthv1.GrantOutcome_GRANT_OUTCOME_DENIED:
		fmt.Fprintln(env.Stderr, "denied")
		return ExitDenied
	case berthv1.GrantOutcome_GRANT_OUTCOME_FAILED, berthv1.GrantOutcome_GRANT_OUTCOME_UNSPECIFIED:
	}
	fmt.Fprintln(env.Stderr, "failed")
	return ExitFailed
}

func resolveExecutable(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("cannot find executable %s: %w", name, err)
	}
	return filepath.Abs(p)
}

func enumWord(s, prefix string) string { return strings.ToLower(strings.TrimPrefix(s, prefix)) }

func shortHash(h string) string {
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func teamOf(c *berthv1.Consumer) string {
	if c.GetTeamId() == "" {
		return "unsigned"
	}
	return c.GetTeamId()
}

func argsOf(glob string) string {
	if glob == "" {
		return "any args"
	}
	return glob
}

func lastUsed(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "never"
	}
	return ts.AsTime().Local().Format(time.DateTime)
}
