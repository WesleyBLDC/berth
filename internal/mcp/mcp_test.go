package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
)

// toolsSHA256 pins tools.json: a changed tool definition must change this on purpose, since MCP
// clients may pin the definitions they approved.
const toolsSHA256 = "4e1966c903f8022d3831ac44efec4bab729f391448a40c7aa4712afe6e0b78c2"

// surface maps every agent method to the tool exposing it, or to why berth-mcp leaves it out, so
// a newly marked agent method fails here until it is decided.
var surface = map[string]string{
	"berth.v1.BrowsersService.OpenAgentBrowser":  "browser_open",
	"berth.v1.BrowsersService.CloseAgentBrowser": "browser_close",
	"berth.v1.BrowsersService.ListAgentBrowsers": "browser_list",
	"berth.v1.SecretsService.ListSecrets":        "secret_list",
	"berth.v1.SecretsService.RequestSecret":      "secret_request",
	"berth.v1.DaemonService.Handshake":           "-the client's connect sends it",
	"berth.v1.SecretsService.AuthorizeUse":       "-its ticket only berth-native redeems, for a command berth run starts",
	"berth.v1.SecretsService.AuthorizeProxy":     "-berth-proxy's own handshake",
	"berth.v1.SecretsService.WatchProxySession":  "-berth-proxy's own handshake",
	"berth.v1.SecretsService.EndProxySession":    "-berth-proxy's own handshake",
	"berth.v1.SecretsService.StartProxy":         "-it needs a command to run through the proxy, which would make berth-mcp a shell",
}

func TestToolsCoverTheAgentSurface(t *testing.T) {
	var agent []string
	protoregistry.GlobalFiles.RangeFilesByPackage("berth.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			for j := range fd.Services().Get(i).Methods().Len() {
				m := fd.Services().Get(i).Methods().Get(j)
				if proto.GetExtension(m.Options(), berthv1.E_Agent).(bool) {
					agent = append(agent, string(m.FullName()))
				}
			}
		}
		return true
	})
	var mapped, want []string
	for m, tool := range surface {
		mapped = append(mapped, m)
		if !strings.HasPrefix(tool, "-") {
			want = append(want, tool)
		}
	}
	slices.Sort(agent)
	slices.Sort(mapped)
	if !slices.Equal(agent, mapped) {
		t.Fatalf("agent methods %v, decided for berth-mcp %v", agent, mapped)
	}
	var defs []struct{ Name string }
	if err := json.Unmarshal(tools, &defs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}
	if sum := sha256.Sum256(tools); hex.EncodeToString(sum[:]) != toolsSHA256 {
		t.Fatalf("tools.json changed (sha256 %x); update toolsSHA256 if that was meant", sum)
	}
}

const cdpToken = "cdp-token-canary-mcp-0c4f"

type fakeDaemon struct {
	berthv1connect.UnimplementedBrowsersServiceHandler
	berthv1connect.UnimplementedSecretsServiceHandler
	closeErr error
	reason   string
	opens    atomic.Int32
}

func (f *fakeDaemon) ListAgentBrowsers(context.Context, *berthv1.ListAgentBrowsersRequest) (*berthv1.ListAgentBrowsersResponse, error) {
	return &berthv1.ListAgentBrowsersResponse{Browsers: []*berthv1.AgentBrowser{
		{Id: "0123456789abcdef", State: berthv1.BrowserState_BROWSER_STATE_READY, CdpEndpoint: "http://127.0.0.1:4321"},
		{Id: "fedcba9876543210"},
	}}, nil
}

func (f *fakeDaemon) OpenAgentBrowser(context.Context, *berthv1.OpenAgentBrowserRequest) (*berthv1.OpenAgentBrowserResponse, error) {
	f.opens.Add(1)
	return &berthv1.OpenAgentBrowserResponse{
		Browser:  &berthv1.AgentBrowser{Id: "0123456789abcdef", State: berthv1.BrowserState_BROWSER_STATE_READY, CdpEndpoint: "http://127.0.0.1:4321"},
		CdpToken: cdpToken,
	}, nil
}

func (f *fakeDaemon) CloseAgentBrowser(_ context.Context, req *berthv1.CloseAgentBrowserRequest) (*berthv1.CloseAgentBrowserResponse, error) {
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	return &berthv1.CloseAgentBrowserResponse{Browser: &berthv1.AgentBrowser{Id: req.GetId(), State: berthv1.BrowserState_BROWSER_STATE_CLOSED}}, nil
}

func (f *fakeDaemon) ListSecrets(context.Context, *berthv1.ListSecretsRequest) (*berthv1.ListSecretsResponse, error) {
	return &berthv1.ListSecretsResponse{Secrets: []*berthv1.SecretSummary{
		{Name: "github-token", Consumers: []*berthv1.Consumer{{ExePath: "/usr/bin/git"}}},
	}}, nil
}

func (f *fakeDaemon) RequestSecret(_ context.Context, req *berthv1.RequestSecretRequest) (*berthv1.RequestSecretResponse, error) {
	f.reason = req.GetReason()
	return &berthv1.RequestSecretResponse{Name: req.GetName(), Outcome: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, nil
}

type session struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	all strings.Builder
}

func start(t *testing.T, fake *fakeDaemon) (*session, string) {
	t.Helper()
	s, dir, _ := startServer(t, fake)
	return s, dir
}

func startServer(t *testing.T, fake *fakeDaemon) (*session, string, *Server) {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterBrowsersServiceHandler(server, fake)
	berthv1connect.RegisterSecretsServiceHandler(server, fake)
	cl := connect.NewClient(connectinprocess.New(server))
	dir := t.TempDir()
	s := &Server{Browsers: berthv1connect.NewBrowsersServiceClient(cl), Secrets: berthv1connect.NewSecretsServiceClient(cl), TokenDir: dir, Version: "test"}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(t.Context(), inR, outW); _ = outW.Close(); close(done) }()
	// Draining the output lets a failed test end even with replies it never read.
	t.Cleanup(func() { _ = inW.Close(); go func() { _, _ = io.Copy(io.Discard, outR) }(); <-done })
	return &session{t: t, in: inW, out: bufio.NewScanner(outR)}, dir, s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (s *session) read() response {
	s.t.Helper()
	if !s.out.Scan() {
		s.t.Fatal("berth-mcp ended its output")
	}
	s.all.Write(s.out.Bytes())
	var r response
	if err := json.Unmarshal(s.out.Bytes(), &r); err != nil {
		s.t.Fatal(err)
	}
	return r
}

// call sends a tools/call and returns the decoded text content and isError.
func (s *session) call(id int, tool, args string) (map[string]any, bool) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`)
	r := s.read()
	if r.Error != nil {
		s.t.Fatalf("%s: %+v", tool, r.Error)
	}
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		s.t.Fatalf("%s result %s: %v", tool, r.Result, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		s.t.Fatal(err)
	}
	return out, res.IsError
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestServeOverStdio(t *testing.T) {
	fake := &fakeDaemon{}
	s, dir := start(t, fake)

	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	var init struct {
		ProtocolVersion string
		Capabilities    map[string]any
	}
	if r := s.read(); json.Unmarshal(r.Result, &init) != nil || init.ProtocolVersion != "2025-06-18" || init.Capabilities["tools"] == nil {
		t.Fatalf("initialize = %s", r.Result)
	}
	// A notification gets no reply: the next line answers the ping.
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if r := s.read(); string(r.ID) != `"p"` {
		t.Fatalf("ping = %+v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var list struct{ Tools []struct{ Name string } }
	if r := s.read(); json.Unmarshal(r.Result, &list) != nil || len(list.Tools) != 5 {
		t.Fatalf("tools/list = %s", r.Result)
	}

	opened, isErr := s.call(3, "browser_open", `{}`)
	file, _ := opened["token_file"].(string)
	if isErr || opened["id"] != "0123456789abcdef" || opened["cdp_endpoint"] != "http://127.0.0.1:4321" || !strings.HasPrefix(file, dir) {
		t.Fatalf("browser_open = %v", opened)
	}
	st, err := os.Stat(file)
	b, _ := os.ReadFile(file)
	if err != nil || st.Mode().Perm() != 0o600 || string(b) != cdpToken {
		t.Fatalf("token file: %v %v", st, err)
	}
	if listed, isErr := s.call(40, "browser_list", `{}`); isErr || !strings.Contains(mustJSON(t, listed), `"id":"0123456789abcdef"`) || strings.Contains(mustJSON(t, listed), cdpToken) {
		t.Fatalf("browser_list = %v", listed)
	}
	if listed, _ := s.call(41, "browser_list", `{}`); !strings.Contains(mustJSON(t, listed), `"id":"fedcba9876543210","state":"starting"`) {
		t.Fatalf("browser_list = %v; a browser with no state yet must read as starting", listed)
	}
	if closed, isErr := s.call(4, "browser_close", `{"id":"0123456789abcdef"}`); isErr || closed["state"] != "closed" {
		t.Fatalf("browser_close = %v", closed)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("browser_close must remove the token file")
	}

	if names, isErr := s.call(5, "secret_list", `{}`); isErr || len(names) != 1 || names["names"].([]any)[0] != "github-token" {
		t.Fatalf("secret_list = %v; it must give names only", names)
	}
	if got, isErr := s.call(6, "secret_request", `{"name":"api-key","why":"to call the API"}`); isErr || got["outcome"] != "stored" || fake.reason != "to call the API" {
		t.Fatalf("secret_request = %v", got)
	}
	if got, isErr := s.call(7, "browser_close", `{"id":"0123456789abcdef","extra":1}`); !isErr {
		t.Fatalf("unknown argument fields must be refused: %v", got)
	}
	fake.closeErr = connect.NewError(connect.CodeNotFound, "no managed browser has that id")
	if got, isErr := s.call(8, "browser_close", `{"id":"fedcba9876543210"}`); !isErr || !strings.Contains(got["error"].(string), "no managed browser") {
		t.Fatalf("a berthd refusal = %v", got)
	}

	s.send(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	if r := s.read(); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("an unknown tool = %+v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":10,"method":"resources/list"}`)
	if r := s.read(); r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("an unknown method = %+v", r)
	}
	s.send(`not json`)
	if r := s.read(); r.Error == nil || r.Error.Code != -32600 {
		t.Fatalf("a malformed line = %+v", r)
	}
	if strings.Contains(s.all.String(), cdpToken) {
		t.Fatal("the CDP token reached a tool result")
	}
}

// The server's end removes every token file it left.
func TestServeRemovesTokenFilesWhenItEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, dir := start(t, &fakeDaemon{})
		opened, _ := s.call(1, "browser_open", `{}`)
		_ = s.in.Close()
		file := opened["token_file"].(string)
		synctest.Wait()
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("token file %s in %s survived the server", file, dir)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A notification never runs a method: a tools/call without an id opens nothing and gets no reply.
func TestNotificationsRunNothing(t *testing.T) {
	fake := &fakeDaemon{}
	s, _ := start(t, fake)
	s.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"browser_open","arguments":{}}}`)
	// A call that started after the notification and went through berthd too ends after it would.
	s.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_list","arguments":{}}}`)
	if r := s.read(); string(r.ID) != "1" {
		t.Fatalf("the first reply = %+v, want browser_list's", r)
	}
	if n := fake.opens.Load(); n != 0 {
		t.Fatalf("a notification opened %d browsers", n)
	}
}

// A line over 1 MiB gets an error reply, and the session goes on.
func TestOversizedLineIsRefusedNotFatal(t *testing.T) {
	s, _ := start(t, &fakeDaemon{})
	go func() {
		_, _ = io.WriteString(s.in, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", maxMessage)+`"}}`+"\n")
	}()
	if r := s.read(); r.Error == nil || r.Error.Code != -32600 {
		t.Fatalf("an oversized line = %+v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r := s.read(); string(r.ID) != "2" || r.Error != nil {
		t.Fatalf("after an oversized line, ping = %+v", r)
	}
}

// Once closed (berth-mcp's signal path), no token file is written, so none outlives the process.
func TestNoTokenFileAfterClose(t *testing.T) {
	s, dir, srv := startServer(t, &fakeDaemon{})
	srv.Close()
	if got, isErr := s.call(1, "browser_open", `{}`); !isErr {
		t.Fatalf("browser_open after Close = %v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("token folders after Close: %v", entries)
	}
}
