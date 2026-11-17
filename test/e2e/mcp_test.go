package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
)

// mcpClient drives a berth-mcp process over its stdin and stdout.
type mcpClient struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	all strings.Builder
}

func (c *mcpClient) rpc(id int, method string, params any) json.RawMessage {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	if !c.out.Scan() {
		c.t.Fatalf("%s: berth-mcp ended its output", method)
	}
	c.all.Write(c.out.Bytes())
	var r struct {
		Result json.RawMessage
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(c.out.Bytes(), &r); err != nil || r.Error != nil {
		c.t.Fatalf("%s: %v %+v", method, err, r.Error)
	}
	return r.Result
}

func (c *mcpClient) tool(id int, name string, args map[string]any) map[string]string {
	c.t.Helper()
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := json.Unmarshal(c.rpc(id, "tools/call", map[string]any{"name": name, "arguments": args}), &res); err != nil || res.IsError || len(res.Content) != 1 {
		c.t.Fatalf("%s: %v %+v", name, err, res)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// TestBerthMCPOpensAndClosesAnAgentBrowser is M4 item 8 end to end: berth-mcp, started with an
// agent session's token file, answers initialize and tools/list over stdio, opens a browser
// through the real agent socket with its token in a file, and closes it.
func TestBerthMCPOpensAndClosesAnAgentBrowser(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	_, sock := startWithAgent(t, h, os.Getuid(), "-browser", chrome)
	id, token, file := newAgentSession(t, h)
	tmp := tempDir(t, "berth-mcp-tmp-")

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(binDir, "berth-mcp"))
	cmd.Env = append(append(os.Environ(), "TMPDIR="+tmp), agentEnv(sock, file)...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	c := &mcpClient{t: t, in: stdin, out: bufio.NewScanner(stdout)}

	var init struct{ ProtocolVersion string }
	_ = json.Unmarshal(c.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "e2e", "version": "1"}}), &init)
	if init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize answered protocol %q", init.ProtocolVersion)
	}
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var list struct{ Tools []struct{ Name string } }
	_ = json.Unmarshal(c.rpc(2, "tools/list", nil), &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "browser_open,browser_close,secret_list,secret_request" {
		t.Fatalf("tools = %v", names)
	}

	opened := c.tool(3, "browser_open", map[string]any{})
	cdpToken := readFile(t, opened["token_file"])
	if !strings.HasPrefix(opened["token_file"], tmp) || len(cdpToken) != 64 || opened["token_header"] != "X-Berth-Token" {
		t.Fatalf("browser_open = %v", opened)
	}
	if code := status(t, opened["cdp_endpoint"]+"/json/version", cdpToken); code != http.StatusOK {
		t.Fatalf("the endpoint with the token file's token: %d", code)
	}
	if b := browserByID(t, h, opened["id"]); b.GetAgentSessionId() != id || b.GetState() != berthv1.BrowserState_BROWSER_STATE_READY {
		t.Fatalf("listed %v", b)
	}

	if closed := c.tool(4, "browser_close", map[string]any{"id": opened["id"]}); closed["state"] != "closed" {
		t.Fatalf("browser_close = %v", closed)
	}
	if _, err := os.Stat(opened["token_file"]); !os.IsNotExist(err) {
		t.Fatal("browser_close left the token file")
	}
	if b := browserByID(t, h, opened["id"]); b.GetState() != berthv1.BrowserState_BROWSER_STATE_CLOSED {
		t.Fatalf("after browser_close the browser is %v", b.GetState())
	}

	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("berth-mcp did not exit when its stdin closed")
	}
	newCanaryChecker(t, cdpToken, token, strings.TrimPrefix(token, "berth_at_")).checkHome(h, []string{c.all.String(), stderr.String()})
}
