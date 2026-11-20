package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
		return printJSON(env, resp)
	}
	printSessions(env, resp.GetSessions())
	return ExitOK
}

func printSessions(env Env, sessions []*berthv1.Session) {
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tBROWSER\tDRIVER\tHEADLESS\tCDP\tOWNER\tPROCS")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", shown(s.GetId()), stateName(s.GetState()), kindName(s.GetKind()),
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
		return reportError(env.Stderr, err, "", shown(id))
	}
	s := resp.GetSession()
	fmt.Fprintf(env.Stdout, "session %s: %s since %s\n\n", shown(s.GetId()), stateName(s.GetState()), when(s.GetStateSince().AsTime()))
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "browser\t%s, pid %d, started %s\n", kindName(s.GetKind()), s.GetBrowser().GetPid(), when(s.GetBrowser().GetStartedAt().AsTime()))
	fmt.Fprintf(w, "executable\t%s\n", executableName(s.GetBrowser()))
	fmt.Fprintf(w, "driver\t%s\n", driverName(s.GetDriver()))
	fmt.Fprintf(w, "headless\t%s\n", headlessName(s))
	fmt.Fprintf(w, "cdp\t%s\n", cdpName(s))
	fmt.Fprintf(w, "profile\t%s\n", profileName(s))
	fmt.Fprintf(w, "arguments\t%s\n", argsName(s))
	fmt.Fprintf(w, "owner\t%s\n", ownerName(s))
	_ = w.Flush()
	fmt.Fprintln(env.Stdout)
	printProcesses(env.Stdout, slices.Concat(resp.GetProcesses(), resp.GetOwnerChain()))
	fmt.Fprintln(env.Stdout)
	printHistory(env.Stdout, resp.GetHistory())
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
		return reportError(env.Stderr, err, "", shown(id))
	}
	if resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED {
		fmt.Fprintf(env.Stderr, "session %s is still %s after 10 seconds\n", shown(id), stateName(resp.GetState()))
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "stopped %s\n", shown(id))
	return ExitOK
}

func sessionLeftovers(ctx context.Context, args []string, env Env) int {
	fs := newFlags("leftovers", env)
	stop := fs.Bool("stop", false, "stop every leftover browser")
	force := fs.Bool("force", false, "with --stop, also stop leftovers owned by you or an app")
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
			fmt.Fprintf(env.Stdout, "skipped %s: it belongs to another user\n", shown(s.GetId()))
			continue
		}
		if stopOne(ctx, env, c.Sessions.StopSession, s.GetId(), *force) != ExitOK {
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
	case s.GetOtherUser() || s.GetArgsUnreadable():
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
		return fmt.Sprintf("agent %s (pid %d)", shown(o.GetName()), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_TERMINAL:
		return fmt.Sprintf("terminal %s (pid %d)", shown(o.GetName()), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_APP:
		return fmt.Sprintf("app %s (pid %d)", shown(o.GetName()), o.GetProcess().GetPid())
	case berthv1.OwnerKind_OWNER_KIND_PROCESS:
		return fmt.Sprintf("process %s (pid %d)", shown(o.GetName()), o.GetProcess().GetPid())
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

func profileName(s *berthv1.Session) string {
	switch {
	case s.GetOtherUser() || s.GetArgsUnreadable():
		return "?"
	case s.GetUserDataDir() == "":
		return "default"
	}
	return shown(s.GetUserDataDir())
}

func argsName(s *berthv1.Session) string {
	switch {
	case s.GetOtherUser():
		return "another user's, not read"
	case s.GetArgsUnreadable():
		return "unreadable"
	}
	return "read"
}

// printJSON prints a response as JSON, for --json, with every rune that is not graphic escaped:
// protojson escapes C0 controls but prints C1 controls, DEL and format characters such as bidi
// overrides as they are, and a terminal acts on those. Outside its strings JSON holds only ASCII
// punctuation, spaces and newlines, so escaping by rune keeps the document the same.
func printJSON(env Env, m proto.Message) int {
	b, err := protojson.MarshalOptions{Multiline: true}.Marshal(m)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	fmt.Fprintln(env.Stdout, graphicJSON(b))
	return ExitOK
}

// graphicJSON escapes every rune of protojson's output that is neither graphic nor a newline.
func graphicJSON(b []byte) string {
	var out strings.Builder
	for _, r := range string(b) {
		if r == '\n' || unicode.IsGraphic(r) {
			out.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&out, `\u%04x`, u)
		}
	}
	return out.String()
}

func printProcesses(out io.Writer, procs []*berthv1.SessionProcess) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tROLE\tEXECUTABLE")
	for _, p := range procs {
		fmt.Fprintf(w, "%d\t%s\t%s\n", p.GetPid(), roleName(p.GetRole()), executableName(p))
	}
	_ = w.Flush()
}

func executableName(p *berthv1.SessionProcess) string {
	if p.GetExecutableChanged() {
		return shown(p.GetExecutable()) + " (replaced or deleted since)"
	}
	return shown(p.GetExecutable())
}

func printHistory(out io.Writer, hist []*berthv1.Transition) {
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "V\tWHEN\tEVENT\tFROM\tTO\tDETAIL")
	for _, t := range hist {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.GetVersion(), when(t.GetAt().AsTime()), shown(t.GetEvent()), shown(t.GetFromState()), shown(t.GetToState()), shown(t.GetDetail()))
	}
	_ = w.Flush()
	// An empty detail leaves the padding of the column before it, which nobody wants to see.
	for line := range strings.Lines(b.String()) {
		fmt.Fprintln(out, strings.TrimRight(line, " \n"))
	}
}

// shown makes text a process chose (paths, profile folders, names) safe to print: a string with
// any control byte (ESC, CR, LF, other C0 and C1, DEL), format character or invalid UTF-8 is
// printed quoted with Go escapes, so it cannot move the cursor, recolour or fake lines in the
// terminal. Ordinary paths print as they are. Text that begins with a quote is quoted too, so
// "x\n" typed out cannot pass for an x and a line break.
func shown(text string) string {
	if utf8.ValidString(text) && !strings.HasPrefix(text, `"`) && !strings.ContainsFunc(text, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return text
	}
	return quoted(text)
}

// quoted is text always quoted, with Go escapes (strconv.QuoteToGraphic), and a leading
// combining mark or variation selector escaped too, since it would join the opening quote.
// Berth.app's quoted is checked against it.
func quoted(text string) string {
	q := strconv.QuoteToGraphic(text)
	r, size := utf8.DecodeRuneInString(q[1:])
	if !unicode.Is(unicode.M, r) {
		return q
	}
	esc := fmt.Sprintf(`\u%04x`, r)
	if r > 0xffff {
		esc = fmt.Sprintf(`\U%08x`, r)
	}
	return `"` + esc + q[1+size:]
}
