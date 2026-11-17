package e2e

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
)

// rawAgentBrowsers is a BrowsersService client on the agent socket with no handshake, sending
// token in the agent header when it is not empty.
func rawAgentBrowsers(sock, token string) berthv1connect.BrowsersServiceClient {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return berthv1connect.NewBrowsersServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://berthd"),
		func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
				if info, ok := connect.CallInfoForClientContext(ctx); ok && token != "" {
					info.RequestHeader().Set(api.AgentTokenHeader, token)
				}
				return next(ctx, spec)
			}
		}))
}

// openAgentBrowser runs berth browser open in agent mode and returns what it printed; it kills
// only the browser it started, by pid and process group, when the test ends.
func openAgentBrowser(t *testing.T, h string, env []string, outputs *[]string) launched {
	t.Helper()
	out, code := cliRunEnv(t, h, env, outputs, "browser", "open")
	if code != 0 {
		t.Fatalf("browser open: %d %q", code, out)
	}
	var l launched
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "browser":
			l.id = f[1]
		case len(f) == 2 && f[0] == "endpoint":
			l.endpoint = f[1]
		case len(f) == 2 && f[0] == "token":
			l.token = f[1]
		}
	}
	b := browserByID(t, h, l.id)
	if b == nil || l.endpoint == "" || l.token == "" {
		t.Fatalf("browser open printed %q; listed %v", out, b)
	}
	l.pid, _ = strconv.Atoi(strings.SplitN(b.GetSessionId(), "-", 2)[0])
	if l.pid <= 0 {
		t.Fatalf("session id %q names no pid", b.GetSessionId())
	}
	t.Cleanup(func() { _ = syscall.Kill(-l.pid, syscall.SIGKILL) })
	return l
}

// waitClosed waits for a managed browser to be closed, its process group gone and its profile
// folder deleted.
func waitClosed(t *testing.T, h string, l launched, profile string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; {
		b := browserByID(t, h, l.id)
		_, statErr := os.Stat(profile)
		groupGone := errors.Is(syscall.Kill(-l.pid, 0), syscall.ESRCH)
		if b.GetState() == berthv1.BrowserState_BROWSER_STATE_CLOSED && groupGone && errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser %s: state %v, process group gone %v, profile %v", l.id, b.GetState(), groupGone, statErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAgentBrowserThroughTheAgentSocket is M4 item 7 end to end, with this test's own uid as the
// agent: an agent opens a headless browser through the agent socket with its session token,
// drives it through the CDP proxy, and closes it; another session cannot close it; the limits
// hold; revoking the session closes its browsers and deletes their profiles.
func TestAgentBrowserThroughTheAgentSocket(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	_, sock := startWithAgent(t, h, os.Getuid(), "-browser", chrome)
	idA, tokenA, fileA := newAgentSession(t, h)
	idB, tokenB, fileB := newAgentSession(t, h)
	envA, envB := agentEnv(sock, fileA), agentEnv(sock, fileB)
	profileOf := func(n int) string { return filepath.Join(h, "profiles", "agent-"+idA+"-"+strconv.Itoa(n)) }
	// Outputs that never printed a token, for the canary check at the end.
	var outputs, printed []string
	var cdpTokens []string

	t.Run("no token or a forged one opens nothing", func(t *testing.T) {
		for name, tok := range map[string]string{"no token": "", "forged token": "berth_at_" + strings.Repeat("A", 43)} {
			_, err := rawAgentBrowsers(sock, tok).OpenAgentBrowser(t.Context(), &berthv1.OpenAgentBrowserRequest{RequestId: "raw-open-0001"})
			if connect.CodeOf(err) != connect.CodeUnauthenticated || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
				t.Fatalf("%s: %v (%v)", name, err, api.ReasonOf(err))
			}
		}
		if out := berth(t, h, "browser", "ls", "--all", "--json"); strings.Contains(out, `"id"`) {
			t.Fatalf("a refused open started a browser: %s", out)
		}
	})

	l := openAgentBrowser(t, h, envA, &printed)
	cdpTokens = append(cdpTokens, l.token)

	t.Run("the owner sees it with its agent session, headless on its own profile", func(t *testing.T) {
		b := browserByID(t, h, l.id)
		if b.GetAgentSessionId() != idA || !b.GetHeadless() || b.GetProfile() != "agent-"+idA+"-1" || b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
			t.Fatalf("listed %v", b)
		}
		if st, err := os.Stat(profileOf(1)); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("profile folder: %v %v", st, err)
		}
		if out := berth(t, h, "browser", "ls"); !strings.Contains(out, idA) {
			t.Fatalf("berth browser ls does not show the agent session:\n%s", out)
		}
	})

	t.Run("Playwright drives it through the proxy", func(t *testing.T) {
		module := findNodeModule(t, "BERTH_E2E_PLAYWRIGHT_CORE", "playwright-core")
		driveBrowser(t, h, module, playwrightScript, "berth via playwright", l.endpoint, l.token)
		requireNoRefusals(t, h, filepath.Base(module))
	})

	t.Run("another session cannot close it or open over its limit", func(t *testing.T) {
		out, code := cliRunEnv(t, h, envB, &outputs, "browser", "close", l.id)
		if code != 1 || !strings.Contains(out, "no managed browser has id "+l.id) {
			t.Fatalf("close with another session's token: %d %q", code, out)
		}
		if b := browserByID(t, h, l.id); b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
			t.Fatalf("after another session's close the browser is %v", b.GetState())
		}
		// The owner's own browsers are out of reach too.
		owners := launchBrowser(t, h, chrome, "owner")
		cdpTokens = append(cdpTokens, owners.token)
		if out, code := cliRunEnv(t, h, envA, &outputs, "browser", "close", owners.id); code != 1 || !strings.Contains(out, "no managed browser has id") {
			t.Fatalf("an agent closing the owner's browser: %d %q", code, out)
		}
		// An agent lists only its own session's browsers.
		if out, code := cliRunEnv(t, h, envA, &outputs, "browser", "ls"); code != 0 || !strings.Contains(out, l.id) || strings.Contains(out, owners.id) {
			t.Fatalf("session A's list: %d %q", code, out)
		}
		if out, code := cliRunEnv(t, h, envB, &outputs, "browser", "ls"); code != 0 || strings.Contains(out, l.id) || strings.Contains(out, owners.id) {
			t.Fatalf("session B's list: %d %q", code, out)
		}
		berth(t, h, "browser", "close", owners.id)
	})

	t.Run("its own session closes it and the profile goes", func(t *testing.T) {
		if out, code := cliRunEnv(t, h, envA, &outputs, "browser", "close", l.id); code != 0 || out != "browser "+l.id+" is closed\n" {
			t.Fatalf("close: %d %q", code, out)
		}
		waitClosed(t, h, l, profileOf(1))
	})

	t.Run("at most two open, and revoking the session closes them", func(t *testing.T) {
		two := []launched{openAgentBrowser(t, h, envA, &printed), openAgentBrowser(t, h, envA, &printed)}
		for _, b := range two {
			cdpTokens = append(cdpTokens, b.token)
		}
		out, code := cliRunEnv(t, h, envA, &outputs, "browser", "open")
		if code != 1 || !strings.Contains(out, "has 2 browsers open, or opened 10 in the last hour") {
			t.Fatalf("a third open browser: %d %q", code, out)
		}
		other := openAgentBrowser(t, h, envB, &printed)
		cdpTokens = append(cdpTokens, other.token)

		berth(t, h, "agent", "session", "revoke", idA)
		waitClosed(t, h, two[0], profileOf(2))
		waitClosed(t, h, two[1], profileOf(3))
		if b := browserByID(t, h, other.id); b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
			t.Fatalf("another session's browser is %v after the revoke", b.GetState())
		}
		if out, code := cliRunEnv(t, h, envA, &outputs, "browser", "open"); code != 1 || !strings.Contains(out, "refused the agent token") {
			t.Fatalf("open after the revoke: %d %q", code, out)
		}
		berth(t, h, "agent", "session", "revoke", idB)
		waitClosed(t, h, other, filepath.Join(h, "profiles", "agent-"+idB+"-1"))
	})

	// Neither session token nor any CDP token reaches berthd's log, a database file, or an
	// output other than the one that printed it.
	check := newCanaryChecker(t, append(cdpTokens, tokenA, tokenB, strings.TrimPrefix(tokenA, "berth_at_"))...)
	check.checkHome(h, outputs)
	for i, o := range printed {
		if strings.Contains(o, tokenA) || strings.Contains(o, tokenB) {
			t.Errorf("browser open output %d holds the session token", i)
		}
	}
}
