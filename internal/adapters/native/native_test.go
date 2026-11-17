package native_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berth/internal/adapters/native"
	"berth/internal/core/ports"
)

var helper string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bnh")
	if err != nil {
		panic(err)
	}
	helper = filepath.Join(dir, "fakehelper")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", helper, "./fakehelper")
	if out, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		panic(fmt.Sprintf("build fake helper: %v\n%s", err, out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// berthHome makes a short BERTH_HOME (the socket path must stay under 104 bytes) holding the
// fake helper's config, and plants a variable that must never reach the helper.
func berthHome(t *testing.T, answer string) string {
	t.Helper()
	home, err := os.MkdirTemp("", "bnt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("BERTH_HOME", home)
	t.Setenv("BERTH_LEAK_CANARY", "must-not-reach-helper")
	t.Setenv("BERTH_FAKE_NATIVE_ANSWER", "deny")
	cfg := fmt.Sprintf(`{"answer":%q,"value":%q}`, answer, base64.StdEncoding.EncodeToString([]byte("canary-71c2")))
	if err := os.MkdirAll(filepath.Join(home, "fake-native"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "fake-native", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

type logged struct {
	Command string          `json:"command"`
	Env     []string        `json:"env"`
	Request json.RawMessage `json:"request"`
}

func requests(t *testing.T, home string) []logged {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "fake-native", "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []logged
	for line := range bytes.Lines(b) {
		var l logged
		if err := json.Unmarshal(line, &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

var requester = ports.Requester{
	PID:        4242,
	Executable: "/opt/agent/bin/agent",
	Tree:       []ports.Process{{PID: 4000, Executable: "/bin/zsh"}, {PID: 1, Executable: "/sbin/launchd"}},
	WorkingDir: "/Users/someone/project",
}

func TestStoreSecretSendsProtocolRequestWithScrubbedEnv(t *testing.T) {
	home := berthHome(t, "approve")
	got, err := native.New(helper).StoreSecret(t.Context(), ports.StorePrompt{Name: "gh-token", Reason: "push PRs", Requester: requester})
	if err != nil || got != ports.StoreStored {
		t.Fatalf("got %q, %v; want stored", got, err)
	}
	reqs := requests(t, home)
	if len(reqs) != 1 || reqs[0].Command != "store" {
		t.Fatalf("requests = %+v", reqs)
	}
	want := `{"name":"gh-token","reason":"push PRs","requester":{"pid":4242,"executable":"/opt/agent/bin/agent","tree":[{"pid":4000,"executable":"/bin/zsh"},{"pid":1,"executable":"/sbin/launchd"}],"working_dir":"/Users/someone/project"}}`
	if string(reqs[0].Request) != want {
		t.Fatalf("request\n got %s\nwant %s", reqs[0].Request, want)
	}
	for _, k := range reqs[0].Env {
		if k != "HOME" && k != "PATH" && k != "BERTH_HOME" {
			t.Fatalf("helper saw %s; the environment must be scrubbed", k)
		}
	}
}

func TestConfirmPowerSendsConsumerFacts(t *testing.T) {
	home := berthHome(t, "approve")
	var sum [32]byte
	sum[0], sum[31] = 0xab, 0x01
	p := ports.PowerPrompt{
		Action:      ports.PowerGrant,
		Secret:      "gh-token",
		Consumer:    ports.Executable{Path: "/usr/local/bin/gh", SHA256: sum, TeamID: "TEAM123456", SigningID: "com.github.gh"},
		ArgsGlob:    "pr *",
		Interpreter: true,
		Requester:   ports.Requester{PID: 9, Executable: "/x", WorkingDir: "/"},
	}
	got, err := native.New(helper).ConfirmPower(t.Context(), p)
	if err != nil || got != ports.PowerApproved {
		t.Fatalf("got %q, %v; want approved", got, err)
	}
	want := `{"action":"grant","args_glob":"pr *","consumer":{"path":"/usr/local/bin/gh","sha256":"ab` + strings.Repeat("0", 60) + `01","signing_id":"com.github.gh","team_id":"TEAM123456"},"interpreter":true,"requester":{"executable":"/x","pid":9,"tree":[],"working_dir":"/"},"secret":"gh-token"}`
	if got := string(requests(t, home)[0].Request); got != want {
		t.Fatalf("request\n got %s\nwant %s", got, want)
	}
}

func TestOutcomes(t *testing.T) {
	cases := []struct {
		answer    string
		wantStore ports.StoreOutcome
		wantPower ports.PowerOutcome
		wantErr   bool
	}{
		{"approve", ports.StoreStored, ports.PowerApproved, false},
		{"deny", ports.StoreDenied, ports.PowerDenied, false},
		{"fail", ports.StoreFailed, ports.PowerFailed, true},
		{"crash", ports.StoreFailed, ports.PowerFailed, true},
		{"garbage", ports.StoreFailed, ports.PowerFailed, true},
	}
	for _, c := range cases {
		t.Run(c.answer, func(t *testing.T) {
			berthHome(t, c.answer)
			a := native.New(helper)
			s, err := a.StoreSecret(t.Context(), ports.StorePrompt{Name: "n", Reason: "r"})
			if s != c.wantStore || (err != nil) != c.wantErr {
				t.Fatalf("store: %q, %v", s, err)
			}
			p, err := a.ConfirmPower(t.Context(), ports.PowerPrompt{Action: ports.PowerGrant, Secret: "n"})
			if p != c.wantPower || (err != nil) != c.wantErr {
				t.Fatalf("confirm: %q, %v", p, err)
			}
		})
	}
}

func TestTimeoutKillsHelper(t *testing.T) {
	berthHome(t, "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := native.New(helper).StoreSecret(ctx, ports.StorePrompt{Name: "n", Reason: "r"})
	if got != ports.StoreFailed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %q, %v; want failed with deadline", got, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("helper was not killed promptly")
	}
}

func TestMissingHelperFails(t *testing.T) {
	berthHome(t, "approve")
	got, err := native.New(filepath.Join(t.TempDir(), "absent")).StoreSecret(t.Context(), ports.StorePrompt{Name: "n", Reason: "r"})
	if got != ports.StoreFailed || err == nil {
		t.Fatalf("got %q, %v; want failed with an error", got, err)
	}
}

// serveRedeem answers RedeemUseTicket on $BERTH_HOME/berthd.sock with name for any ticket.
func serveRedeem(t *testing.T, home, name string) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(home, "berthd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func TestReadReturnsValue(t *testing.T) {
	home := berthHome(t, "approve")
	serveRedeem(t, home, "gh-token")
	values := filepath.Join(home, "fake-native", "values")
	if err := os.MkdirAll(values, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := bytes.Repeat([]byte("canary-9d"), 200) // longer than the first buffer, to exercise growth
	if err := os.WriteFile(filepath.Join(values, "gh-token"), canary, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := native.Read(t.Context(), helper, ports.Ticket{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, canary) {
		t.Fatalf("value differs from the stored one")
	}
}

func TestReadRefused(t *testing.T) {
	berthHome(t, "deny")
	got, err := native.Read(t.Context(), helper, ports.Ticket{})
	if err == nil || got != nil {
		t.Fatalf("want a refusal, got %d bytes, %v", len(got), err)
	}
}

// script writes a stand in helper, for outputs the fake helper never produces.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat >/dev/null\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // the stand in must be executable
		t.Fatal(err)
	}
	return p
}

func TestReadErrorNeverIncludesStdout(t *testing.T) {
	berthHome(t, "approve")
	for _, body := range []string{"printf canary-55e1; exit 3", "printf canary-55e1; exit 1", "printf canary-55e1 >&1; kill -9 $$"} {
		got, err := native.Read(t.Context(), script(t, body), ports.Ticket{})
		if err == nil || got != nil {
			t.Fatalf("%s: want an error and no value", body)
		}
		if strings.Contains(err.Error(), "canary-55e1") {
			t.Fatalf("%s: error leaks stdout: %v", body, err)
		}
	}
}

func TestReadEmptyOutputIsAnError(t *testing.T) {
	berthHome(t, "approve")
	if got, err := native.Read(t.Context(), script(t, "exit 0"), ports.Ticket{}); err == nil || got != nil {
		t.Fatalf("want an error for empty output")
	}
}
