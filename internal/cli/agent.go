package cli

import (
	"context"
	"fmt"
	"regexp"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	berthv1 "berth/gen/berth/v1"
)

var agentSessionID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func agentCommand(ctx context.Context, args []string, env Env) int {
	if len(args) >= 2 && args[0] == "session" {
		switch args[1] {
		case "new":
			return agentSessionNew(ctx, args[2:], env)
		case "ls":
			return agentSessionList(ctx, args[2:], env)
		case "revoke":
			return agentSessionRevoke(ctx, args[2:], env)
		}
	}
	fmt.Fprint(env.Stderr, usage)
	return ExitUsage
}

// agentSessionNew prints the token once, on its own line, so the person can hand it to the agent
// through a file; berthd cannot show it again.
func agentSessionNew(ctx context.Context, args []string, env Env) int {
	fs := newFlags("agent session new", env)
	name := fs.String("name", "", "a label for the session")
	ttl := fs.Duration("ttl", 0, "how long the token stays valid; default 12h, at most 168h")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *ttl < 0 || *ttl > 7*24*time.Hour || *ttl%time.Second != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Agents.CreateAgentSession(ctx, &berthv1.CreateAgentSessionRequest{
		RequestId: requestID(), Name: *name, TtlSeconds: uint32(*ttl / time.Second), //nolint:gosec // bounded to 7 days above
	})
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	s := resp.GetSession()
	fmt.Fprintf(env.Stdout, "session %s expires %s\n", s.GetId(), s.GetExpiresAt().AsTime().Local().Format(time.DateTime))
	fmt.Fprintf(env.Stdout, "token %s\n", resp.GetToken())
	fmt.Fprintln(env.Stderr, "the token is shown only now: save it to a file only the agent user can read, and point BERTH_AGENT_TOKEN_FILE at it")
	return ExitOK
}

func agentSessionList(ctx context.Context, args []string, env Env) int {
	fs := newFlags("agent session ls", env)
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Agents.ListAgentSessions(ctx, &berthv1.ListAgentSessionsRequest{})
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	if *asJSON {
		b, err := protojson.MarshalOptions{Multiline: true}.Marshal(resp)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return ExitError
		}
		fmt.Fprintln(env.Stdout, string(b))
		return ExitOK
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATE\tROOT PID\tEXPIRES")
	for _, s := range resp.GetSessions() {
		root := "-"
		if s.GetRootPid() > 0 {
			root = fmt.Sprint(s.GetRootPid())
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.GetId(), s.GetName(), enumWord(s.GetState().String(), "AGENT_SESSION_STATE_"),
			root, s.GetExpiresAt().AsTime().Local().Format(time.DateTime))
	}
	_ = w.Flush()
	return ExitOK
}

func agentSessionRevoke(ctx context.Context, args []string, env Env) int {
	if len(args) != 1 || !agentSessionID.MatchString(args[0]) {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Agents.RevokeAgentSession(ctx, &berthv1.RevokeAgentSessionRequest{RequestId: requestID(), Id: args[0]})
	if err != nil {
		return reportError(env.Stderr, err, "", args[0])
	}
	fmt.Fprintf(env.Stdout, "agent session %s is %s\n", args[0], enumWord(resp.GetSession().GetState().String(), "AGENT_SESSION_STATE_"))
	return ExitOK
}
