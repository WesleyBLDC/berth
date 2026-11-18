// Command fakehelper speaks the berth-native helper protocol (docs/plans/e2-terminal-secrets.md,
// and the E2b additions in docs/plans/e2b-header-proxy.md) without a dialog, Touch ID or the
// Keychain, so Go tests and e2e never need the Swift binary.
//
// Usage: fakehelper store|confirm|read|rotate|remove, with the request on stdin exactly as berth-native.
//
// berthd starts the helper with only HOME, PATH and BERTH_HOME, so the fake takes its switches
// from a state folder, and from environment variables only when it is started directly:
//
//	BERTH_FAKE_NATIVE_DIR     the state folder; default $BERTH_HOME/fake-native
//	BERTH_FAKE_NATIVE_ANSWER  overrides "answer" from config.json
//
// The state folder holds:
//
//	config.json     {"answer":"approve","value":"<base64>","trust_parent":""}; missing means
//	                approve with no value and the real parent check
//	values/<name>   one file per stored secret, its content the raw value; store refuses (failed)
//	                when the file exists, like the Keychain item berth.<name>
//	requests.jsonl  one line appended per run: {"command":"store","env":["HOME",...],"request":{...}};
//	                for read the request is only {"redeemed":true|false}, and for rotate and remove
//	                {"name":...,"requester":{...},"redeemed":true|false}: never a ticket or value
//
// answer is one of:
//
//	approve  store writes values/<name> from config "value" and answers stored; confirm answers
//	         approved; read redeems the ticket and prints values/<name>; rotate overwrites an
//	         existing values/<name> with "value" and answers stored (failed when it is missing);
//	         remove deletes values/<name> and answers removed, also when it was already gone
//	deny     store, confirm and rotate answer denied, remove answers denied; read exits 3
//	fail     store, confirm, rotate and remove answer failed with an error; read exits 3
//	crash    exits 2 with nothing on stdout
//	garbage  prints a line that is not JSON and exits 0
//	hang     sleeps until killed, for timeout tests
//
// trust_parent is "", "cli" or "proxy". Empty applies berth-native's code requirement to the
// value channel's peer; "cli" or "proxy" still requires fd 3 to be a Unix stream socket whose
// peer is the parent, but takes the parent to be that kind of consumer without checking its
// signature, so tests run by an unsigned binary can reach the consumer kind rule.
//
// read checks its receiver first: fd 3 must be a Unix stream socket whose peer is its parent
// running the signed berth or berth-proxy beside it (see openRecipient). It then redeems for
// real: it POSTs Connect unary JSON over HTTP/1.1 to $BERTH_HOME/berthd.sock
// (/berth.v1.SecretsService/RedeemUseTicket). berthd's answer must say TICKET_ACTION_USE and the
// consumer kind of the parent (CONSUMER_KIND_EXECUTABLE for berth, CONSUMER_KIND_PROXY for
// berth-proxy); then it reads values/<name> for the name berthd returns and writes it to fd 3.
// Any read failure writes nothing to fd 3 and exits 3.
//
// rotate and remove redeem their action ticket the same way before the answer is applied, and
// answer failed unless berthd says TICKET_ACTION_ROTATE or TICKET_ACTION_REMOVE for the same name.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"berth/internal/adapters/clock"
	"berth/internal/adapters/execid"
	"berth/internal/adapters/peer"
	"berth/internal/adapters/socket"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

const (
	exitCrash   = 2
	exitRefused = 3
)

type config struct {
	Answer      string `json:"answer"`
	Value       []byte `json:"value"`
	TrustParent string `json:"trust_parent"`
}

// Consumer kinds and ticket actions as Connect JSON spells the proto enums.
const (
	kindExecutable = "CONSUMER_KIND_EXECUTABLE"
	kindProxy      = "CONSUMER_KIND_PROXY"
	actionUse      = "TICKET_ACTION_USE"
	actionRemove   = "TICKET_ACTION_REMOVE"
	actionRotate   = "TICKET_ACTION_ROTATE"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: fakehelper store|confirm|read|rotate|remove")
		return exitCrash
	}
	command := args[0]
	dir, err := stateDir()
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper:", err)
		return exitCrash
	}
	cfg, err := loadConfig(dir)
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper:", err)
		return exitCrash
	}
	line, err := bufio.NewReader(stdin).ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper: read request:", err)
		return exitCrash
	}
	switch cfg.Answer {
	case "crash":
		fmt.Fprintln(stderr, "fakehelper: crashing on purpose")
		return exitCrash
	case "garbage":
		fmt.Fprintln(stdout, "this is not json")
		return 0
	case "hang":
		time.Sleep(time.Hour)
		return exitCrash
	}
	switch command {
	case "store":
		return store(dir, cfg, line, stdout, stderr)
	case "confirm":
		return confirm(dir, cfg, line, stdout, stderr)
	case "read":
		return read(dir, cfg, line, stderr)
	case "rotate", "remove":
		return lifecycle(dir, cfg, command, line, stdout, stderr)
	}
	fmt.Fprintln(stderr, "fakehelper: unknown command", command)
	return exitCrash
}

func stateDir() (string, error) {
	if d := os.Getenv("BERTH_FAKE_NATIVE_DIR"); d != "" {
		return d, nil
	}
	home, err := paths.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "fake-native"), nil
}

func loadConfig(dir string) (config, error) {
	cfg := config{Answer: "approve"}
	b, err := os.ReadFile(filepath.Join(dir, "config.json")) //nolint:gosec // the fake's own state folder
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return cfg, err
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("config.json: %w", err)
		}
	}
	if a := os.Getenv("BERTH_FAKE_NATIVE_ANSWER"); a != "" {
		cfg.Answer = a
	}
	if !slices.Contains([]string{"approve", "deny", "fail", "crash", "garbage", "hang"}, cfg.Answer) {
		return cfg, fmt.Errorf("unknown answer %q", cfg.Answer)
	}
	if !slices.Contains([]string{"", "cli", "proxy"}, cfg.TrustParent) {
		return cfg, fmt.Errorf("unknown trust_parent %q", cfg.TrustParent)
	}
	return cfg, nil
}

// record lets tests check what the helper was shown and that its environment was scrubbed.
func record(dir, command string, request any) error {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		env = append(env, k)
	}
	slices.Sort(env)
	line, err := json.Marshal(map[string]any{"command": command, "env": env, "request": request})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the fake's own state folder
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func reply(stdout io.Writer, outcome, errText string) int {
	out := map[string]string{"outcome": outcome}
	if errText != "" {
		out["error"] = errText
	}
	b, _ := json.Marshal(out)
	_, _ = stdout.Write(append(b, '\n'))
	return 0
}

// validName keeps a berthd supplied name from escaping the values folder.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func store(dir string, cfg config, line []byte, stdout, stderr io.Writer) int {
	var req struct {
		Name      string          `json:"name"`
		Reason    string          `json:"reason"`
		Requester json.RawMessage `json:"requester"`
	}
	if err := json.Unmarshal(line, &req); err != nil || !validName(req.Name) {
		fmt.Fprintln(stderr, "fakehelper: bad store request")
		return exitCrash
	}
	if err := record(dir, "store", req); err != nil {
		fmt.Fprintln(stderr, "fakehelper:", err)
		return exitCrash
	}
	switch cfg.Answer {
	case "deny":
		return reply(stdout, "denied", "")
	case "fail":
		return reply(stdout, "failed", "fake failure")
	}
	values := filepath.Join(dir, "values")
	if err := os.MkdirAll(values, 0o700); err != nil {
		return reply(stdout, "failed", "cannot create values folder")
	}
	f, err := os.OpenFile(filepath.Join(values, req.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // validName keeps the name inside the values folder
	if errors.Is(err, os.ErrExist) {
		return reply(stdout, "failed", "item exists")
	}
	if err != nil {
		return reply(stdout, "failed", "cannot create item")
	}
	if _, err := f.Write(cfg.Value); err != nil {
		_ = f.Close()
		return reply(stdout, "failed", "cannot write item")
	}
	if err := f.Close(); err != nil {
		return reply(stdout, "failed", "cannot write item")
	}
	return reply(stdout, "stored", "")
}

func confirm(dir string, cfg config, line []byte, stdout, stderr io.Writer) int {
	var req map[string]any
	if err := json.Unmarshal(line, &req); err != nil || !validConfirm(req) {
		fmt.Fprintln(stderr, "fakehelper: bad confirm request")
		return exitCrash
	}
	if err := record(dir, "confirm", req); err != nil {
		fmt.Fprintln(stderr, "fakehelper:", err)
		return exitCrash
	}
	switch cfg.Answer {
	case "deny":
		return reply(stdout, "denied", "")
	case "fail":
		return reply(stdout, "failed", "fake failure")
	}
	return reply(stdout, "approved", "")
}

// validConfirm mirrors berth-native's decodeConfirm: a grant names a consumer, a proxy grant a
// target with a host, a header name and a port from 1 to 65535.
func validConfirm(req map[string]any) bool {
	secret, _ := req["secret"].(string)
	if !validName(secret) {
		return false
	}
	switch req["action"] {
	case "grant":
		_, ok := req["consumer"].(map[string]any)
		return ok
	case "proxy":
		target, _ := req["target"].(map[string]any)
		host, _ := target["host"].(string)
		header, _ := target["header_name"].(string)
		port, _ := target["port"].(float64)
		return host != "" && header != "" && port >= 1 && port <= 65535
	}
	return false
}

func read(dir string, cfg config, line []byte, stderr io.Writer) int {
	ticket, err := hex.DecodeString(strings.TrimSuffix(string(line), "\n"))
	if err != nil || len(ticket) != 32 {
		fmt.Fprintln(stderr, "fakehelper: bad ticket")
		return exitRefused
	}
	if cfg.Answer != "approve" {
		_ = record(dir, "read", map[string]bool{"redeemed": false})
		fmt.Fprintln(stderr, "fakehelper: read refused by answer", cfg.Answer)
		return exitRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Like berth-native: the receiver is checked before the ticket is redeemed.
	out, parent, err := recipient(ctx, cfg.TrustParent)
	if err != nil {
		_ = record(dir, "read", map[string]bool{"redeemed": false})
		fmt.Fprintln(stderr, "fakehelper: value channel:", err)
		return exitRefused
	}
	defer func() { _ = out.Close() }()
	t, err := redeem(ctx, ticket)
	_ = record(dir, "read", map[string]bool{"redeemed": err == nil})
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper: redeem:", err)
		return exitRefused
	}
	if err := useRule(parent, t); err != nil {
		fmt.Fprintln(stderr, "fakehelper:", err)
		return exitRefused
	}
	name := t.Name
	if !validName(name) {
		fmt.Fprintln(stderr, "fakehelper: berthd returned a bad name")
		return exitRefused
	}
	value, err := os.ReadFile(filepath.Join(dir, "values", name)) //nolint:gosec // validName keeps the name inside the values folder
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper: no item for that name")
		return exitRefused
	}
	defer clear(value)
	if _, err := out.Write(value); err != nil {
		return exitRefused
	}
	return 0
}

// useRule is berth-native's receiver rule after the redeem: the ticket must be for reading, by
// the kind of consumer the parent proved to be.
func useRule(parent string, t redeemed) error {
	if t.Action != actionUse {
		return errors.New("the ticket is not for reading a value")
	}
	if t.ConsumerKind != parent {
		return fmt.Errorf("the ticket is for %q, the parent is %s", t.ConsumerKind, parent)
	}
	return nil
}

func lifecycle(dir string, cfg config, command string, line []byte, stdout, stderr io.Writer) int {
	var req struct {
		Name      string          `json:"name"`
		Ticket    string          `json:"ticket"`
		Requester json.RawMessage `json:"requester"`
	}
	if err := json.Unmarshal(line, &req); err != nil || !validName(req.Name) {
		fmt.Fprintln(stderr, "fakehelper: bad", command, "request")
		return exitCrash
	}
	ticket, err := hex.DecodeString(req.Ticket)
	if err != nil || len(ticket) != 32 || req.Ticket != strings.ToLower(req.Ticket) {
		fmt.Fprintln(stderr, "fakehelper: bad ticket")
		return exitCrash
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Like berth-native: the action ticket is redeemed before any prompt, which answer stands for.
	t, err := redeem(ctx, ticket)
	rec := map[string]any{"name": req.Name, "requester": req.Requester, "redeemed": err == nil}
	if recErr := record(dir, command, rec); recErr != nil {
		fmt.Fprintln(stderr, "fakehelper:", recErr)
		return exitCrash
	}
	if err != nil {
		return reply(stdout, "failed", "redeem: "+err.Error())
	}
	want := map[string]string{"rotate": actionRotate, "remove": actionRemove}[command]
	if t.Action != want {
		return reply(stdout, "failed", "the ticket is not for "+command)
	}
	if t.Name != req.Name {
		return reply(stdout, "failed", "the ticket is for another secret")
	}
	switch cfg.Answer {
	case "deny":
		return reply(stdout, "denied", "")
	case "fail":
		return reply(stdout, "failed", "fake failure")
	}
	item := filepath.Join(dir, "values", req.Name)
	if command == "remove" {
		if err := os.Remove(item); err != nil && !errors.Is(err, os.ErrNotExist) {
			return reply(stdout, "failed", "cannot delete item")
		}
		return reply(stdout, "removed", "")
	}
	f, err := os.OpenFile(item, os.O_WRONLY|os.O_TRUNC, 0) //nolint:gosec // validName keeps the name inside the values folder
	if errors.Is(err, os.ErrNotExist) {
		return reply(stdout, "failed", "item not found")
	}
	if err != nil {
		return reply(stdout, "failed", "cannot open item")
	}
	if _, err := f.Write(cfg.Value); err != nil {
		_ = f.Close()
		return reply(stdout, "failed", "cannot write item")
	}
	if err := f.Close(); err != nil {
		return reply(stdout, "failed", "cannot write item")
	}
	return reply(stdout, "stored", "")
}

// recipient opens the value channel and says which consumer kind its peer proved to be; tests
// replace it.
var recipient = openRecipient

// openRecipient applies berth-native's receiver rule to fd 3: it must be a connected Unix
// stream socket whose peer, by the kernel's audit token, is this process's parent and is
// running the signed berth CLI or berth-proxy beside the helper. The fake pins them the way the
// e2e build of berthd does (identifier only, no Apple chain); the live check still needs the
// hardened runtime. trust names the parent's kind instead of checking its signature.
func openRecipient(ctx context.Context, trust string) (io.WriteCloser, string, error) {
	f := os.NewFile(3, "berth-value")
	if f == nil {
		return nil, "", errors.New("no value channel on fd 3")
	}
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("no value channel: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		_ = f.Close()
		return nil, "", errors.New("the value channel is not a socket")
	}
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil, "", err
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok || uc.LocalAddr().Network() != "unix" {
		_ = conn.Close()
		return nil, "", errors.New("the value channel is not a Unix stream socket")
	}
	kind, err := checkRecipient(ctx, uc, os.Getppid(), trust)
	if err != nil {
		_ = uc.Close()
		return nil, "", err
	}
	return uc, kind, nil
}

// checkRecipient returns the consumer kind the value channel's peer proved to be.
func checkRecipient(ctx context.Context, uc *net.UnixConn, parent int, trust string) (string, error) {
	// For a socketpair the peer is whoever holds the other end, which is how we know who receives.
	pid, err := socket.KernelPeerPID(uc)
	if err != nil {
		return "", err
	}
	token, err := socket.KernelPeerToken(uc)
	if err != nil {
		return "", err
	}
	if pid != parent {
		return "", fmt.Errorf("the value channel's peer %d is not the parent %d", pid, parent)
	}
	switch trust {
	case "cli":
		return kindExecutable, nil
	case "proxy":
		return kindProxy, nil
	}
	x := execid.New(execid.DefaultCodesign)
	pin := func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, `identifier "`+identifier+`"`)
	}
	for _, p := range []struct{ file, identifier, kind string }{
		{"berth", codetrust.CLISigningID, kindExecutable},
		{"berth-proxy", codetrust.ProxySigningID, kindProxy},
	} {
		path, err := paths.Beside(p.file)
		if err != nil {
			return "", err
		}
		trusted := codetrust.New(ctx, path, p.identifier, pin, peer.NewCode(), clock.System{}, slog.New(slog.DiscardHandler))
		if trusted.Runs(ctx, ports.Peer{PID: pid, AuditToken: token}) {
			return p.kind, nil
		}
	}
	return "", errors.New("the value channel's peer is not the signed berth or berth-proxy")
}

// redeemed is berthd's answer; an absent kind or action (the proto zero value) is "".
type redeemed struct {
	Name         string `json:"name"`
	ConsumerKind string `json:"consumerKind"`
	Action       string `json:"action"`
}

// redeem is the same Connect unary JSON call the Swift helper makes, with no generated code.
func redeem(ctx context.Context, ticket []byte) (redeemed, error) {
	home, err := paths.Home()
	if err != nil {
		return redeemed{}, err
	}
	sock := paths.Socket(home)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return redeemed{}, err
	}
	body, err := json.Marshal(map[string]string{
		"requestId": hex.EncodeToString(id),
		"ticket":    base64.StdEncoding.EncodeToString(ticket),
	})
	if err != nil {
		return redeemed{}, err
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://berthd/berth.v1.SecretsService/RedeemUseTicket", bytes.NewReader(body))
	if err != nil {
		return redeemed{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return redeemed{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return redeemed{}, fmt.Errorf("berthd answered %s", resp.Status)
	}
	var out redeemed
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return redeemed{}, fmt.Errorf("decode reply: %w", err)
	}
	return out, nil
}
