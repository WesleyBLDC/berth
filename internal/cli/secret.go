package cli

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
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
			state := enumWord(cons.GetState().String(), "GRANT_STATE_")
			if cons.GetKind() == berthv1.ConsumerKind_CONSUMER_KIND_PROXY {
				t := cons.GetProxy()
				fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\n", cons.GetGrantId(), hostPort(t), t.GetHeaderName(),
					methodsOf(t.GetMethods()), pathOf(t.GetPathGlob()), state)
				continue
			}
			fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\n", cons.GetGrantId(), cons.GetExePath(), shortHash(cons.GetSha256()),
				teamOf(cons), argsOf(cons.GetArgsGlob()), state)
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
	var passEnv stringList
	fs.Var(&passEnv, "pass-env", "an environment variable berth run may also pass to it; repeat for more")
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
		RequestId: requestID(), Name: name, ExePath: path, ArgsGlob: *glob, AllowInterpreter: *interpreter, PassEnv: passEnv,
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
		if len(cons.GetPassEnv()) > 0 {
			fmt.Fprintf(w, "  env\t%s\n", strings.Join(cons.GetPassEnv(), ", "))
		}
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

func secretProxy(ctx context.Context, args []string, env Env) int {
	fs := newFlags("secret proxy", env)
	host := fs.String("host", "", "the HTTPS host, as a DNS name")
	header := fs.String("header", "", "the request header that carries the secret, such as Authorization")
	port := fs.Uint("port", 443, "the HTTPS port")
	prefix := fs.String("prefix", "", `written before the value, such as "Bearer "`)
	suffix := fs.String("suffix", "", "written after the value")
	path := fs.String("path", "", "limit requests to paths matching this glob; empty allows any")
	placeholder := fs.String("placeholder-env", "", "an environment variable berth run sets to a stand in for the value")
	var methods stringList
	fs.Var(&methods, "method", "limit requests to this HTTP method; repeat for more; none allows any")
	name, ok := parseNamed(fs, args)
	if !ok || *host == "" || *header == "" || *port == 0 || *port > 65535 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.GrantProxy(ctx, &berthv1.GrantProxyRequest{RequestId: requestID(), Name: name, Target: &berthv1.ProxyTarget{
		Host: *host, Port: uint32(*port), HeaderName: *header, ValuePrefix: *prefix, ValueSuffix: *suffix, //nolint:gosec // bounded above
		PathGlob: *path, Methods: methods, PlaceholderEnv: *placeholder,
	}})
	if err != nil {
		return reportError(env.Stderr, err, name, *host)
	}
	switch resp.GetOutcome() {
	case berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED:
		cons := resp.GetConsumer()
		t := cons.GetProxy()
		fmt.Fprintf(env.Stdout, "granted %s to the proxy for %s\n", name, hostPort(t))
		w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "  grant\t%d\n", cons.GetGrantId())
		fmt.Fprintf(w, "  header\t%s: %s%s%s\n", t.GetHeaderName(), t.GetValuePrefix(), maskReplacement, t.GetValueSuffix())
		fmt.Fprintf(w, "  methods\t%s\n", methodsOf(t.GetMethods()))
		fmt.Fprintf(w, "  path\t%s\n", pathOf(t.GetPathGlob()))
		if t.GetPlaceholderEnv() != "" {
			fmt.Fprintf(w, "  placeholder\t%s\n", t.GetPlaceholderEnv())
		}
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

func secretRevoke(ctx context.Context, args []string, env Env) int {
	if len(args) != 1 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.RevokeGrant(ctx, &berthv1.RevokeGrantRequest{RequestId: requestID(), GrantId: id})
	if err != nil {
		return reportError(env.Stderr, err, "", args[0])
	}
	if resp.GetState() != berthv1.GrantState_GRANT_STATE_REVOKED {
		fmt.Fprintf(env.Stderr, "grant %d is %s, not revoked\n", id, enumWord(resp.GetState().String(), "GRANT_STATE_"))
		return ExitFailed
	}
	fmt.Fprintf(env.Stdout, "revoked grant %d\n", id)
	return ExitOK
}

func secretRemove(ctx context.Context, args []string, env Env) int {
	name, ok := parseNamed(newFlags("secret rm", env), args)
	if !ok {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.RemoveSecret(ctx, &berthv1.RemoveSecretRequest{RequestId: requestID(), Name: name})
	if err != nil {
		return reportError(env.Stderr, err, name, "")
	}
	switch resp.GetOutcome() {
	case berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED:
		fmt.Fprintf(env.Stdout, "removed %s\n", name)
		ids := resp.GetRevokedGrantIds()
		if len(ids) == 0 {
			fmt.Fprintln(env.Stdout, "revoked no grants")
			return ExitOK
		}
		words := make([]string, len(ids))
		for i, id := range ids {
			words[i] = strconv.FormatInt(id, 10)
		}
		fmt.Fprintf(env.Stdout, "revoked grants %s\n", strings.Join(words, ", "))
		return ExitOK
	case berthv1.RemoveOutcome_REMOVE_OUTCOME_DENIED:
		fmt.Fprintln(env.Stderr, "denied")
		return ExitDenied
	case berthv1.RemoveOutcome_REMOVE_OUTCOME_FAILED, berthv1.RemoveOutcome_REMOVE_OUTCOME_UNSPECIFIED:
	}
	fmt.Fprintln(env.Stderr, "failed")
	return ExitFailed
}

func secretRotate(ctx context.Context, args []string, env Env) int {
	name, ok := parseNamed(newFlags("secret rotate", env), args)
	if !ok {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Secrets.RotateSecret(ctx, &berthv1.RotateSecretRequest{RequestId: requestID(), Name: name})
	if err != nil {
		return reportError(env.Stderr, err, name, "")
	}
	switch resp.GetOutcome() {
	case berthv1.StoreOutcome_STORE_OUTCOME_STORED:
		fmt.Fprintf(env.Stdout, "rotated %s; runs already going keep the old value until they end\n", name)
		return ExitOK
	case berthv1.StoreOutcome_STORE_OUTCOME_DENIED:
		fmt.Fprintln(env.Stderr, "denied")
		return ExitDenied
	case berthv1.StoreOutcome_STORE_OUTCOME_FAILED, berthv1.StoreOutcome_STORE_OUTCOME_UNSPECIFIED:
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

// teamOf names who signed an executable consumer. Apple's platform binaries carry no team but
// are signed, so only code with no valid signature reads as unsigned.
func teamOf(c *berthv1.Consumer) string {
	switch c.GetSignature() {
	case berthv1.SignatureKind_SIGNATURE_KIND_TEAM:
		return c.GetTeamId()
	case berthv1.SignatureKind_SIGNATURE_KIND_APPLE_PLATFORM:
		return "Apple (platform)"
	case berthv1.SignatureKind_SIGNATURE_KIND_AD_HOC:
		return "ad hoc"
	case berthv1.SignatureKind_SIGNATURE_KIND_UNSIGNED:
		return "unsigned"
	case berthv1.SignatureKind_SIGNATURE_KIND_OTHER:
		return "no team"
	case berthv1.SignatureKind_SIGNATURE_KIND_UNSPECIFIED:
	}
	if c.GetTeamId() != "" {
		return c.GetTeamId()
	}
	return "unknown"
}

func argsOf(glob string) string {
	if glob == "" {
		return "any args"
	}
	return glob
}

func hostPort(t *berthv1.ProxyTarget) string {
	return net.JoinHostPort(t.GetHost(), strconv.FormatUint(uint64(t.GetPort()), 10))
}

func methodsOf(m []string) string {
	if len(m) == 0 {
		return "any method"
	}
	return strings.Join(m, ",")
}

func pathOf(glob string) string {
	if glob == "" {
		return "any path"
	}
	return glob
}

func lastUsed(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "never"
	}
	return ts.AsTime().Local().Format(time.DateTime)
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}
