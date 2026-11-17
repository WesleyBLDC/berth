// Package mcp is berth-mcp's server: the Model Context Protocol over stdio (one JSON-RPC 2.0
// message per line), exposing a fixed set of tools over the agent socket for agents without a
// shell. Tool results carry names, states and endpoints only: never a secret value, and never a
// token, which goes into a file the result names.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
)

// tools is the tools/list reply's tool array, fixed text pinned by a golden test, so a client
// that pins tool definitions sees them change only with a change here.
//
//go:embed tools.json
var tools []byte

// protocolVersions are the MCP revisions this server speaks, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// maxMessage bounds one line from the client.
const maxMessage = 1 << 20

// Server answers one MCP client. Browsers and Secrets are clients on the agent socket, which
// send the session token with every call; TokenDir is where browser_open writes CDP tokens.
type Server struct {
	Browsers berthv1connect.BrowsersServiceClient
	Secrets  berthv1connect.SecretsServiceClient
	TokenDir string
	Version  string

	out sync.Mutex
	mu  sync.Mutex
	// tokenDirs holds the folder of each open browser's token file, removed at its close and
	// when the server ends; closed refuses new ones once it has.
	tokenDirs map[string]string
	closed    bool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from in until it ends or ctx does, answering each on out; calls run
// concurrently, since a store request waits for the person.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		s.Close()
	}()
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		raw, tooLong, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if tooLong {
			s.reply(out, nil, nil, &rpcError{Code: -32600, Message: "message larger than 1 MiB"})
			continue
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			s.reply(out, nil, nil, &rpcError{Code: -32600, Message: "not a JSON-RPC 2.0 request"})
			continue
		}
		// A notification (no id) gets no reply, so it never runs a method: a tool with side
		// effects must be a request the client sees answered. The client's own notifications
		// (initialized, cancelled) need nothing from this server.
		if req.ID == nil {
			continue
		}
		wg.Go(func() {
			result, rerr := s.handle(ctx, req)
			s.reply(out, req.ID, result, rerr)
		})
	}
}

// readLine reads one line; a line over maxMessage is read to its end and reported as too long,
// so one oversized message costs an error reply, not the session.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxMessage {
			tooLong, line = true, nil
		} else if !tooLong {
			line = append(line, chunk...)
		}
		switch {
		case err == nil:
			return line, tooLong, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && (len(line) > 0 || tooLong):
			return line, tooLong, nil
		default:
			return nil, false, err
		}
	}
}

func (s *Server) reply(out io.Writer, id json.RawMessage, result any, rerr *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		msg["id"] = nil
	}
	if rerr != nil {
		msg["error"] = rerr
	} else {
		msg["result"] = result
	}
	b, err := json.Marshal(msg)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: -32603, Message: "could not encode the reply"}})
	}
	s.out.Lock()
	defer s.out.Unlock()
	_, _ = out.Write(append(b, '\n'))
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "berth-mcp", "version": s.Version},
			"instructions":    "Berth's agent tools. Secrets can be requested and listed by name, never read. browser_open writes the CDP token to a file and returns its path; send the token in the X-Berth-Token header.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": json.RawMessage(tools)}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "tools/call needs a name and arguments"}
		}
		out, err := s.call(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcError{Code: -32602, Message: "unknown tool"}
		}
		if err != nil {
			return toolResult(map[string]string{"error": err.Error()}, true), nil
		}
		return toolResult(out, false), nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}

var errUnknownTool = errors.New("unknown tool")

func toolResult(v any, isError bool) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
		"isError": isError,
	}
}

// call runs one tool. Its arguments are decoded strictly, and berthd validates them again.
func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "browser_open":
		if err := strict(raw, &struct{}{}); err != nil {
			return nil, err
		}
		resp, err := s.Browsers.OpenAgentBrowser(ctx, &berthv1.OpenAgentBrowserRequest{RequestId: requestID()})
		if err != nil {
			return nil, err
		}
		file, err := s.writeToken(resp.GetBrowser().GetId(), resp.GetCdpToken())
		if err != nil {
			return nil, err
		}
		return map[string]string{ //nolint:gosec // the path of the token file and the header's name, never the token
			"id": resp.GetBrowser().GetId(), "cdp_endpoint": resp.GetBrowser().GetCdpEndpoint(),
			"token_file": file, "token_header": "X-Berth-Token",
		}, nil
	case "browser_close":
		var args struct {
			ID string `json:"id"`
		}
		if err := strict(raw, &args); err != nil {
			return nil, err
		}
		resp, err := s.Browsers.CloseAgentBrowser(ctx, &berthv1.CloseAgentBrowserRequest{RequestId: requestID(), Id: args.ID})
		if err != nil {
			return nil, err
		}
		s.removeToken(args.ID)
		return map[string]string{"id": args.ID, "state": stateWord(resp.GetBrowser().GetState())}, nil
	case "browser_list":
		if err := strict(raw, &struct{}{}); err != nil {
			return nil, err
		}
		resp, err := s.Browsers.ListAgentBrowsers(ctx, &berthv1.ListAgentBrowsersRequest{})
		if err != nil {
			return nil, err
		}
		browsers := []map[string]string{}
		for _, b := range resp.GetBrowsers() {
			browsers = append(browsers, map[string]string{"id": b.GetId(), "state": stateWord(b.GetState()), "cdp_endpoint": b.GetCdpEndpoint()})
		}
		return map[string]any{"browsers": browsers}, nil
	case "secret_list":
		if err := strict(raw, &struct{}{}); err != nil {
			return nil, err
		}
		resp, err := s.Secrets.ListSecrets(ctx, &berthv1.ListSecretsRequest{})
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, sec := range resp.GetSecrets() {
			names = append(names, sec.GetName())
		}
		return map[string][]string{"names": names}, nil
	case "secret_request":
		var args struct {
			Name string `json:"name"`
			Why  string `json:"why"`
		}
		if err := strict(raw, &args); err != nil {
			return nil, err
		}
		resp, err := s.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: requestID(), Name: args.Name, Reason: args.Why})
		if err != nil {
			return nil, err
		}
		outcome := map[berthv1.StoreOutcome]string{
			berthv1.StoreOutcome_STORE_OUTCOME_STORED: "stored", berthv1.StoreOutcome_STORE_OUTCOME_DENIED: "denied",
		}[resp.GetOutcome()]
		if outcome == "" {
			outcome = "failed"
		}
		return map[string]string{"name": args.Name, "outcome": outcome}, nil
	}
	return nil, errUnknownTool
}

// strict decodes tool arguments, refusing unknown fields; absent arguments are an empty object.
func strict(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

// writeToken puts a CDP token in a new 0600 file in a new 0700 folder, so it reaches the CDP
// client without entering the model's context or a command line. It runs under s.mu, so Close
// either removes the file or finds the server closed before the file exists.
func (s *Server) writeToken(id, token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("berth-mcp is stopping")
	}
	dir, err := os.MkdirTemp(s.TokenDir, "berth-cdp-")
	if err != nil {
		return "", fmt.Errorf("could not make a folder for the CDP token: %w", err)
	}
	path := filepath.Join(dir, "token")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // a fresh folder of our own
	if err == nil {
		_, err = f.WriteString(token)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("could not write the CDP token file: %w", err)
	}
	if s.tokenDirs == nil {
		s.tokenDirs = map[string]string{}
	}
	s.tokenDirs[id] = dir
	return path, nil
}

func (s *Server) removeToken(id string) {
	s.mu.Lock()
	dir := s.tokenDirs[id]
	delete(s.tokenDirs, id)
	s.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// Close removes every token file the server wrote and writes no more; Serve calls it when it
// returns, and berth-mcp on a signal.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, dir := range s.tokenDirs {
		_ = os.RemoveAll(dir)
		delete(s.tokenDirs, id)
	}
}

// stateWord names a browser's state; a browser whose launch has not reported a state yet is
// starting, never an empty word.
func stateWord(st berthv1.BrowserState) string {
	return map[berthv1.BrowserState]string{
		berthv1.BrowserState_BROWSER_STATE_UNSPECIFIED: "starting", berthv1.BrowserState_BROWSER_STATE_LAUNCHING: "launching", berthv1.BrowserState_BROWSER_STATE_READY: "ready",
		berthv1.BrowserState_BROWSER_STATE_CLOSING: "closing", berthv1.BrowserState_BROWSER_STATE_CLOSED: "closed",
	}[st]
}

func requestID() string { return "mcp-" + rand.Text() }
