package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	berthv1 "berth/gen/berth/v1"
)

type launched struct {
	id, endpoint, token string
	pid                 int
}

// launchBrowser runs berth browser launch and kills only the browser it started, by pid and
// process group, when the test ends.
func launchBrowser(t *testing.T, h, chrome, profile string) launched {
	t.Helper()
	out := berth(t, h, "browser", "launch", profile, "--headless", "--executable", chrome)
	var l launched
	for _, line := range strings.Split(out, "\n") {
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
	if b == nil || b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY || l.endpoint == "" || l.token == "" {
		t.Fatalf("launch output:\n%s\nlisted %v", out, b)
	}
	pid, _ := strconv.Atoi(strings.SplitN(b.GetSessionId(), "-", 2)[0])
	if pid <= 0 {
		t.Fatalf("session id %q names no pid", b.GetSessionId())
	}
	l.pid = pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return l
}

func browserByID(t *testing.T, h, id string) *berthv1.ManagedBrowser {
	t.Helper()
	var resp berthv1.ListBrowsersResponse
	if err := protojson.Unmarshal([]byte(berth(t, h, "browser", "ls", "--all", "--json")), &resp); err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.GetBrowsers() {
		if b.GetId() == id {
			return b
		}
	}
	return nil
}

type cdpClient struct {
	t    *testing.T
	ws   *websocket.Conn
	next int
}

func dialCDP(t *testing.T, l launched) *cdpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.endpoint+"/json/version", nil)
	req.Header.Set("X-Berth-Token", l.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var version struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&version)
	_ = resp.Body.Close()
	if err != nil || version.URL == "" {
		t.Fatalf("/json/version: %v", err)
	}
	// A connection that just ended holds the slot until its state is undone.
	var ws *websocket.Conn
	for {
		var wsResp *http.Response
		ws, wsResp, err = websocket.Dial(ctx, version.URL, &websocket.DialOptions{HTTPHeader: http.Header{"X-Berth-Token": {l.token}}})
		if wsResp != nil && wsResp.Body != nil {
			_ = wsResp.Body.Close()
		}
		if err == nil {
			break
		}
		if wsResp == nil || wsResp.StatusCode != http.StatusConflict || ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ws.SetReadLimit(64 << 20)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &cdpClient{t: t, ws: ws}
}

type cdpReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one command and reads until its reply, skipping events.
func (c *cdpClient) call(session, method string, params any) cdpReply {
	c.t.Helper()
	c.next++
	msg := map[string]any{"id": c.next, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			c.t.Fatalf("%s: %v", method, err)
		}
		var r cdpReply
		if json.Unmarshal(raw, &r) == nil && r.ID == c.next {
			return r
		}
	}
}

func (c *cdpClient) ok(session, method string, params any, into any) {
	c.t.Helper()
	r := c.call(session, method, params)
	if r.Error != nil {
		c.t.Fatalf("%s refused: %s", method, r.Error.Message)
	}
	if into != nil {
		if err := json.Unmarshal(r.Result, into); err != nil {
			c.t.Fatal(err)
		}
	}
}

func TestManagedBrowserThroughTheCDPProxy(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	l := launchBrowser(t, h, chrome, "e2e")

	profile := filepath.Join(h, "profiles", "e2e")
	s := waitSession(t, h, l.pid, "listed with its pipe and Berth profile", func(s *berthv1.Session) bool {
		return s.GetCdp() == berthv1.CdpMode_CDP_MODE_PIPE && s.GetUserDataDir() == profile
	})
	if s.GetCdpPort() != 0 {
		t.Fatalf("a managed browser must have no debugging port: %v", s)
	}

	if code := status(t, l.endpoint+"/json/version", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code := status(t, l.endpoint+"/json/version", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", code)
	}

	c := dialCDP(t, l)
	var target struct{ TargetID string }
	c.ok("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct{ SessionID string }
	c.ok("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	c.ok(attached.SessionID, "Page.navigate", map[string]any{"url": "data:text/html,<title>berth through the proxy</title>"}, nil)
	for deadline := time.Now().Add(10 * time.Second); ; {
		var res struct {
			Result struct{ Value string }
		}
		c.ok(attached.SessionID, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &res)
		if res.Result.Value == "berth through the proxy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title = %q", res.Result.Value)
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, method := range []string{"SystemInfo.getInfo", "Browser.close"} {
		r := c.call("", method, map[string]any{})
		if r.Error == nil || !strings.Contains(r.Error.Message, "refused by Berth: "+method) {
			t.Fatalf("%s: %+v, want the proxy's refusal", method, r)
		}
	}
	if b := browserByID(t, h, l.id); b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
		t.Fatalf("after the refused Browser.close the browser is %v", b.GetState())
	}

	var outputs []string
	if out, code := cliRun(t, h, &outputs, "browser", "launch", "e2e", "--headless", "--executable", chrome); code != 1 || !strings.Contains(out, "profile e2e is in use") {
		t.Fatalf("second launch on the profile: %d %s", code, out)
	}

	if out := berth(t, h, "browser", "close", l.id); out != "browser "+l.id+" is closed\n" {
		t.Fatalf("close: %q", out)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if err := syscall.Kill(-l.pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a process of the closed browser's group is still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := status(t, l.endpoint+"/json/version", l.token); code != 0 {
		t.Fatalf("the closed browser's proxy still answers: %d", code)
	}

	again := launchBrowser(t, h, chrome, "e2e")
	berth(t, h, "browser", "close", again.id)

	// The token exists only in launch's own output: never in the log or the database.
	newCanaryChecker(t, l.token, again.token).checkHome(h, nil)
}

// The browser outlives each CDP connection, so what one connection turned on must not reach the
// next: a real Chromium left with auto attach waiting for the debugger and Fetch on would pause
// every new page, and its sessions would stay attached.
func TestAnEndedCDPConnectionLeavesNothingToTheNext(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	l := launchBrowser(t, h, chrome, "reset")

	first := dialCDP(t, l)
	first.ok("", "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}, nil)
	first.ok("", "Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*"}}}, nil)
	first.ok("", "Target.createTarget", map[string]any{"url": "about:blank"}, nil)
	_ = first.ws.CloseNow()

	next := dialCDP(t, l)
	var targets struct {
		TargetInfos []struct {
			Type     string `json:"type"`
			Attached bool   `json:"attached"`
		} `json:"targetInfos"`
	}
	next.ok("", "Target.getTargets", map[string]any{}, &targets)
	for _, ti := range targets.TargetInfos {
		if ti.Type == "page" && ti.Attached {
			t.Fatalf("a page is still attached after the connection that attached it ended: %+v", targets)
		}
	}
	var target struct{ TargetID string }
	next.ok("", "Target.createTarget", map[string]any{"url": "data:text/html,<title>fresh</title>"}, &target)
	var attached struct{ SessionID string }
	next.ok("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var res struct {
			Result struct{ Value string }
		}
		next.ok(attached.SessionID, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &res)
		if res.Result.Value == "fresh" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a new page never loaded after the previous connection ended: title %q", res.Result.Value)
		}
	}
	berth(t, h, "browser", "close", l.id)
}

// status is the HTTP status of a GET with the token header, or 0 when nothing answers.
func status(t *testing.T, url, token string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("X-Berth-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// SIGTERM to berthd closes every managed browser, so launchd never leaves one behind.
func TestStoppingBerthdClosesManagedBrowsers(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	d := start(t, h)
	waitServing(t, h)
	l := launchBrowser(t, h, chrome, "stop")
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("berthd exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("berthd did not stop")
	}
	if err := syscall.Kill(-l.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("berthd stopped and left its managed browser running")
	}
}
