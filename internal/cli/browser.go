package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	berthv1 "berth/gen/berth/v1"
)

// launchTimeout covers berthd's own 30 second wait for the browser to answer over its pipe.
const launchTimeout = 40 * time.Second

var (
	profileNameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	browserID       = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func browserCommand(ctx context.Context, args []string, env Env) int {
	if len(args) >= 1 {
		switch args[0] {
		case "launch":
			return browserLaunch(ctx, args[1:], env)
		case "open":
			return browserOpen(ctx, args[1:], env)
		case "ls":
			return browserList(ctx, args[1:], env)
		case "close":
			return browserClose(ctx, args[1:], env)
		}
	}
	fmt.Fprint(env.Stderr, usage)
	return ExitUsage
}

// browserLaunch prints the endpoint and the token once; with --token-file the token goes only
// into a new file only the person can read, so it never sits in scrollback or a command line.
func browserLaunch(ctx context.Context, args []string, env Env) int {
	fs := newFlags("browser launch", env)
	headless := fs.Bool("headless", false, "run the browser headless")
	exe := fs.String("executable", "", "the Chromium family browser to run (default: berthd picks one)")
	tokenFile := fs.String("token-file", "", "write the CDP token to this new file (mode 0600) instead of printing it")
	profile, ok := parseNamed(fs, args)
	if !ok || !profileNameRule.MatchString(profile) {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	resp, err := c.Browsers.LaunchBrowser(ctx, &berthv1.LaunchBrowserRequest{
		RequestId: requestID(), Profile: profile, Headless: *headless, Executable: *exe,
	})
	if err != nil {
		return reportError(env.Stderr, err, "", profile)
	}
	b := resp.GetBrowser()
	fmt.Fprintf(env.Stdout, "browser %s on profile %s is %s\n", b.GetId(), b.GetProfile(), browserStateName(b.GetState()))
	fmt.Fprintf(env.Stdout, "endpoint %s\n", b.GetCdpEndpoint())
	if *tokenFile != "" {
		if err := writeNewFile(*tokenFile, []byte(resp.GetCdpToken()+"\n")); err != nil {
			fmt.Fprintf(env.Stderr, "could not write the token file: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "token written to %s\n", *tokenFile)
	} else {
		fmt.Fprintf(env.Stdout, "token %s\n", resp.GetCdpToken())
	}
	fmt.Fprintln(env.Stderr, "the token is shown only now; send it in the X-Berth-Token header, for example Playwright MCP's --cdp-header")
	return ExitOK
}

// browserOpen is an agent's browser: headless, on a fresh profile berthd deletes when it closes,
// and closed when the agent session ends. It prints like launch.
func browserOpen(ctx context.Context, args []string, env Env) int {
	fs := newFlags("browser open", env)
	tokenFile := fs.String("token-file", "", "write the CDP token to this new file (mode 0600) instead of printing it")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	if !env.Agent {
		fmt.Fprintln(env.Stderr, "berth browser open is for agents, through BERTH_AGENT_SOCKET; the person uses berth browser launch")
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	resp, err := c.Browsers.OpenAgentBrowser(ctx, &berthv1.OpenAgentBrowserRequest{RequestId: requestID()})
	if err != nil {
		return reportError(env.Stderr, err, "", "")
	}
	b := resp.GetBrowser()
	fmt.Fprintf(env.Stdout, "browser %s is %s\n", b.GetId(), browserStateName(b.GetState()))
	fmt.Fprintf(env.Stdout, "endpoint %s\n", b.GetCdpEndpoint())
	if *tokenFile != "" {
		if err := writeNewFile(*tokenFile, []byte(resp.GetCdpToken()+"\n")); err != nil {
			fmt.Fprintf(env.Stderr, "could not write the token file: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "token written to %s\n", *tokenFile)
	} else {
		fmt.Fprintf(env.Stdout, "token %s\n", resp.GetCdpToken())
	}
	fmt.Fprintln(env.Stderr, "the token is shown only now; send it in the X-Berth-Token header; berth browser close <id> ends the browser, and so does the end of this agent session")
	return ExitOK
}

// writeNewFile creates path for the person alone: never over an existing file and never
// through a symlink, so the token cannot be steered into a file someone else can read.
func writeNewFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the person names the file
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func browserList(ctx context.Context, args []string, env Env) int {
	fs := newFlags("browser ls", env)
	all := fs.Bool("all", false, "also list browsers that closed since berthd started")
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	resp, err := c.Browsers.ListBrowsers(ctx, &berthv1.ListBrowsersRequest{IncludeClosed: *all})
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
	fmt.Fprintln(w, "ID\tPROFILE\tSTATE\tSESSION\tAGENT SESSION\tHEADLESS\tENDPOINT\tPRODUCT")
	for _, b := range resp.GetBrowsers() {
		state := browserStateName(b.GetState())
		if b.GetCloseReason() != "" {
			state += " (" + shown(b.GetCloseReason()) + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.GetId(), b.GetProfile(), state, dash(b.GetSessionId()),
			dash(b.GetAgentSessionId()), yesNo(b.GetHeadless()), dash(b.GetCdpEndpoint()), dash(shown(b.GetProduct())))
	}
	_ = w.Flush()
	return ExitOK
}

func browserClose(ctx context.Context, args []string, env Env) int {
	if len(args) != 1 || !browserID.MatchString(args[0]) {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	var state berthv1.BrowserState
	if env.Agent {
		resp, err := c.Browsers.CloseAgentBrowser(ctx, &berthv1.CloseAgentBrowserRequest{RequestId: requestID(), Id: args[0]})
		if err != nil {
			return reportError(env.Stderr, err, "", args[0])
		}
		state = resp.GetBrowser().GetState()
	} else {
		resp, err := c.Browsers.CloseBrowser(ctx, &berthv1.CloseBrowserRequest{RequestId: requestID(), Id: args[0]})
		if err != nil {
			return reportError(env.Stderr, err, "", args[0])
		}
		state = resp.GetBrowser().GetState()
	}
	fmt.Fprintf(env.Stdout, "browser %s is %s\n", args[0], browserStateName(state))
	return ExitOK
}

func browserStateName(s berthv1.BrowserState) string {
	return enumWord(s.String(), "BROWSER_STATE_")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
