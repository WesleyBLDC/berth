// Command fakehelper speaks the berth-native helper protocol (docs/plans/e2-terminal-secrets.md)
// without a dialog, Touch ID or the Keychain, so Go tests and e2e never need the Swift binary.
//
// Usage: fakehelper store|confirm|read, with the request on stdin exactly as berth-native.
//
// berthd starts the helper with only HOME, PATH and BERTH_HOME, so the fake takes its switches
// from a state folder, and from environment variables only when it is started directly:
//
//	BERTH_FAKE_NATIVE_DIR     the state folder; default $BERTH_HOME/fake-native
//	BERTH_FAKE_NATIVE_ANSWER  overrides "answer" from config.json
//
// The state folder holds:
//
//	config.json     {"answer":"approve","value":"<base64>"}; missing means approve with no value
//	values/<name>   one file per stored secret, its content the raw value; store refuses (failed)
//	                when the file exists, like the Keychain item berth.<name>
//	requests.jsonl  one line appended per run: {"command":"store","env":["HOME",...],"request":{...}};
//	                for read the request is only {"redeemed":true|false}, never the ticket or value
//
// answer is one of:
//
//	approve  store writes values/<name> from config "value" and answers stored; confirm answers
//	         approved; read redeems the ticket and prints values/<name>
//	deny     store and confirm answer denied; read exits 3
//	fail     store and confirm answer failed with an error; read exits 3
//	crash    exits 2 with nothing on stdout
//	garbage  prints a line that is not JSON and exits 0
//	hang     sleeps until killed, for timeout tests
//
// read redeems for real: it POSTs Connect unary JSON over HTTP/1.1 to $BERTH_HOME/berthd.sock
// (/berth.v1.SecretsService/RedeemUseTicket) and reads values/<name> for the name berthd returns.
// Any read failure prints nothing on stdout and exits 3.
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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"berth/internal/paths"
)

const (
	exitCrash   = 2
	exitRefused = 3
)

type config struct {
	Answer string `json:"answer"`
	Value  []byte `json:"value"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: fakehelper store|confirm|read")
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
		return read(dir, cfg, line, stdout, stderr)
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
	if err := json.Unmarshal(line, &req); err != nil {
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

func read(dir string, cfg config, line []byte, stdout, stderr io.Writer) int {
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
	name, err := redeem(ctx, ticket)
	_ = record(dir, "read", map[string]bool{"redeemed": err == nil})
	if err != nil {
		fmt.Fprintln(stderr, "fakehelper: redeem:", err)
		return exitRefused
	}
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
	if _, err := stdout.Write(value); err != nil {
		return exitRefused
	}
	return 0
}

// redeem is the same Connect unary JSON call the Swift helper makes, with no generated code.
func redeem(ctx context.Context, ticket []byte) (string, error) {
	home, err := paths.Home()
	if err != nil {
		return "", err
	}
	sock := paths.Socket(home)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{
		"requestId": hex.EncodeToString(id),
		"ticket":    base64.StdEncoding.EncodeToString(ticket),
	})
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://berthd/berth.v1.SecretsService/RedeemUseTicket", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("berthd answered %s", resp.Status)
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode reply: %w", err)
	}
	return out.Name, nil
}
