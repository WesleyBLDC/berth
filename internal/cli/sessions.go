package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	berthv1 "berth/gen/berth/v1"
)

// stopTimeout covers berthd's own 10 second wait for the browser to go.
const stopTimeout = 15 * time.Second

func sessionList(ctx context.Context, args []string, env Env) int {
	fs := newFlags("ps", env)
	all := fs.Bool("all", false, "also list sessions that stopped since berthd started")
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	return listSessions(ctx, env, &berthv1.ListSessionsRequest{IncludeStopped: *all}, *asJSON)
}

func listSessions(ctx context.Context, env Env, req *berthv1.ListSessionsRequest, asJSON bool) int {
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Sessions.ListSessions(ctx, req)
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	if asJSON {
		b, err := protojson.MarshalOptions{Multiline: true}.Marshal(resp)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "%s\n", b)
		return ExitOK
	}
	printSessions(env, resp.GetSessions())
	return ExitOK
}

func printSessions(env Env, sessions []*berthv1.Session) {
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tBROWSER\tDRIVER\tHEADLESS\tCDP\tOWNER\tPROCS")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", s.GetId(), stateName(s.GetState()), kindName(s.GetKind()),
			driverName(s.GetDriver()), headlessName(s), cdpName(s), ownerName(s), s.GetProcessCount())
	}
	_ = w.Flush()
}

func sessionInspect(ctx context.Context, args []string, env Env) int {
	fs := newFlags("inspect", env)
	limit := fs.Uint("n", 10, "how many transitions to show, at most 500")
	id, ok := parseNamed(fs, args)
	if !ok || *limit > 500 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Sessions.InspectSession(ctx, &berthv1.InspectSessionRequest{Id: id, HistoryLimit: uint32(*limit)}) //nolint:gosec // bounded to 500 above
	if err != nil {
		return reportError(env.Stderr, err, "", id)
	}
	s := resp.GetSession()
	fmt.Fprintf(env.Stdout, "session %s: %s since %s\n\n", s.GetId(), stateName(s.GetState()), when(s.GetStateSince().AsTime()))
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "browser\t%s, pid %d, started %s\n", kindName(s.GetKind()), s.GetBrowser().GetPid(), when(s.GetBrowser().GetStartedAt().AsTime()))
	fmt.Fprintf(w, "executable\t%s\n", s.GetBrowser().GetExecutable())
	fmt.Fprintf(w, "driver\t%s\n", driverName(s.GetDriver()))
	fmt.Fprintf(w, "headless\t%s\n", headlessName(s))
	fmt.Fprintf(w, "cdp\t%s\n", cdpName(s))
	profile := s.GetUserDataDir()
	switch {
	case s.GetOtherUser():
		profile = "?"
	case profile == "":
		profile = "default"
	}
	fmt.Fprintf(w, "profile\t%s\n", profile)
	fmt.Fprintf(w, "owner\t%s\n", ownerName(s))
	_ = w.Flush()
	fmt.Fprintln(env.Stdout)
	w = tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tROLE\tEXECUTABLE")
	for _, p := range resp.GetProcesses() {
		fmt.Fprintf(w, "%d\t%s\t%s\n", p.GetPid(), roleName(p.GetRole()), p.GetExecutable())
	}
	for _, p := range resp.GetOwnerChain() {
		fmt.Fprintf(w, "%d\t%s\t%s\n", p.GetPid(), roleName(p.GetRole()), p.GetExecutable())
	}
	_ = w.Flush()
	fmt.Fprintln(env.Stdout)
	var hist bytes.Buffer
	w = tabwriter.NewWriter(&hist, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "V\tWHEN\tEVENT\tFROM\tTO\tDETAIL")
	for _, t := range resp.GetHistory() {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.GetVersion(), when(t.GetAt().AsTime()), t.GetEvent(), t.GetFromState(), t.GetToState(), t.GetDetail())
	}
	_ = w.Flush()
	// An empty detail leaves the padding of the column before it, which nobody wants to see.
	for line := range strings.Lines(hist.String()) {
		fmt.Fprintln(env.Stdout, strings.TrimRight(line, " \n"))
	}
	return ExitOK
}

func sessionStop(ctx context.Context, args []string, env Env) int {
	fs := newFlags("stop", env)
	force := fs.Bool("force", false, "stop a browser owned by you or an app")
	id, ok := parseNamed(fs, args)
	if !ok {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	return stopOne(ctx, env, c.Sessions.StopSession, id, *force)
}

type stopFunc func(context.Context, *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error)

func stopOne(ctx context.Context, env Env, stop stopFunc, id string, force bool) int {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	resp, err := stop(ctx, &berthv1.StopSessionRequest{RequestId: requestID(), Id: id, Force: force})
	if err != nil {
		return reportError(env.Stderr, err, "", id)
	}
	if resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
		fmt.Fprintf(env.Stderr, "session %s is still %s after 10 seconds\n", id, stateName(resp.GetState()))
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "stopped %s\n", id)
	return ExitOK
}

func sessionLeftovers(ctx context.Context, args []string, env Env) int {
	fs := newFlags("leftovers", env)
	stop := fs.Bool("stop", false, "stop every leftover browser")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	req := &berthv1.ListSessionsRequest{States: []berthv1.SessionState{berthv1.SessionState_SESSION_STATE_LEFTOVER}}
	if !*stop {
		return listSessions(ctx, env, req, false)
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Sessions.ListSessions(ctx, req)
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	code := ExitOK
	for _, s := range resp.GetSessions() {
		if s.GetOtherUser() {
			fmt.Fprintf(env.Stdout, "skipped %s: it belongs to another user\n", s.GetId())
			continue
		}
		if stopOne(ctx, env, c.Sessions.StopSession, s.GetId(), false) != ExitOK {
			code = ExitError
		}
	}
	if len(resp.GetSessions()) == 0 {
		fmt.Fprintln(env.Stdout, "no leftover browsers")
	}
	return code
}

func when(t time.Time) string { return t.Local().Format(time.DateTime) }

func stateName(s berthv1.SessionState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "SESSION_STATE_"))
}

func kindName(k berthv1.BrowserKind) string {
	switch k {
	case berthv1.BrowserKind_BROWSER_KIND_CHROME:
		return "Chrome"
	case berthv1.BrowserKind_BROWSER_KIND_CHROME_FOR_TESTING:
		return "Chrome for Testing"
	case berthv1.BrowserKind_BROWSER_KIND_CHROMIUM:
		return "Chromium"
	case berthv1.BrowserKind_BROWSER_KIND_EDGE:
		return "Edge"
	case berthv1.BrowserKind_BROWSER_KIND_BRAVE:
		return "Brave"
	case berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL:
		return "headless shell"
	case berthv1.BrowserKind_BROWSER_KIND_UNSPECIFIED:
	}
	return "?"
}

func driverName(d berthv1.Driver) string {
	if d == berthv1.Driver_DRIVER_UNSPECIFIED {
		return "?"
	}
	return strings.ToLower(strings.TrimPrefix(d.String(), "DRIVER_"))
}

func headlessName(s *berthv1.Session) string {
	switch {
	case s.GetHeadless():
		return "yes"
	case s.GetOtherUser():
		return "?"
	}
	return "no"
}

func cdpName(s *berthv1.Session) string {
	switch s.GetCdp() {
	case berthv1.CdpMode_CDP_MODE_PIPE:
		return "pipe"
	case berthv1.CdpMode_CDP_MODE_PORT:
		if s.GetCdpPort() == 0 {
			return "port auto"
		}
		return fmt.Sprintf("port %d", s.GetCdpPort())
	case berthv1.CdpMode_CDP_MODE_NONE:
		return "none"
	case berthv1.CdpMode_CDP_MODE_UNSPECIFIED:
	}
	return "?"
}

func ownerName(s *berthv1.Session) string {
	if s.GetOtherUser() {
		return fmt.Sprintf("unmanaged, other user (uid %d)", s.GetUid())
	}
	o := s.GetOwner()
	switch o.GetKind() {
	case berthv1.OwnerKind_OWNER_KIND_AGENT:
		return fmt.Sprintf("agent %s (pid %d)", o.GetName(), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_TERMINAL:
		return fmt.Sprintf("terminal %s (pid %d)", o.GetName(), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_APP:
		return fmt.Sprintf("app %s (pid %d)", o.GetName(), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_PROCESS:
		return fmt.Sprintf("process %s (pid %d)", o.GetName(), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_PERSON:
		return "you"
	case berthv1.OwnerKind_OWNER_KIND_NONE:
		return "none, its owner exited"
	case berthv1.OwnerKind_OWNER_KIND_UNSPECIFIED:
	}
	return "?"
}

func roleName(r berthv1.ProcessRole) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "PROCESS_ROLE_"))
}
