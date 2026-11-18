package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
	"berth/internal/core/ports"
	"berth/internal/spawn"
	"berth/internal/testutil"
)

const canary = "canary-7f3a9c-not-a-secret"

func TestRunMasksTheValueAndPassesTheExitCode(t *testing.T) {
	fake := &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}
	var handed []byte
	read := func(_ context.Context, ticket []byte) ([]byte, error) {
		if len(ticket) != 32 {
			t.Errorf("ticket length %d", len(ticket))
		}
		handed = []byte(canary)
		return handed, nil
	}
	r := invoke(t, fake, read, "run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c",
		`printf %s "$KEY"; printf %s "$KEY" >&2; exit 7`)
	if r.code != 7 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	for name, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr} {
		if strings.Contains(out, canary) || out != maskReplacement {
			t.Fatalf("%s = %q", name, out)
		}
	}
	if fake.useExe != "/bin/sh" || strings.Join(fake.useArgv, " ") != `-c printf %s "$KEY"; printf %s "$KEY" >&2; exit 7` {
		t.Fatalf("authorized %q %q", fake.useExe, fake.useArgv)
	}
	if strings.Trim(string(handed), "\x00") != "" {
		t.Fatalf("value buffer not zeroed: %q", handed)
	}
}

func TestRunRefusalDoesNotStartTheChild(t *testing.T) {
	cases := []struct {
		outcome berthv1.UseOutcome
		code    int
		says    string
	}{
		{berthv1.UseOutcome_USE_OUTCOME_NO_GRANT, ExitNoGrant, "no grant lets /bin/sh use gh-token"},
		{berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT, ExitStaleGrant, "need one fresh berth secret grant"},
	}
	for _, tc := range cases {
		t.Run(tc.outcome.String(), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			read := func(context.Context, []byte) ([]byte, error) {
				t.Error("a refused run read a value")
				return nil, nil
			}
			r := invoke(t, &fakeSecrets{use: tc.outcome}, read, "run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c", "touch "+marker)
			if r.code != tc.code || !strings.Contains(r.stderr, tc.says) ||
				!strings.Contains(r.stderr, "berth secret grant gh-token --exe /bin/sh") {
				t.Fatalf("code %d, stderr %q", r.code, r.stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the child ran: %v", err)
			}
		})
	}
}

// TestRunForwardsSIGTERMButNotSIGINT signals berth alone (not its process group): SIGTERM must
// reach the child, SIGINT must not, because a terminal Ctrl-C reaches the child directly.
func TestRunForwardsSIGTERMButNotSIGINT(t *testing.T) {
	log := filepath.Join(t.TempDir(), "signals")
	script := `trap 'echo INT >> "$1"' INT; trap 'echo TERM >> "$1"; exit 0' TERM; echo ready >> "$1"; while :; do sleep 0.05; done`
	var stdout, stderr strings.Builder
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}
	done := make(chan int, 1)
	go func() {
		done <- startChild(t.Context(), "/bin/sh", []string{"-c", script, "sh", log}, []string{"PATH=/bin:/usr/bin"}, env)
	}()
	waitFor := func(want string) string {
		t.Helper()
		var b []byte
		if !testutil.Eventually(func() bool { b, _ = os.ReadFile(log); return strings.Contains(string(b), want) }) {
			t.Fatalf("child never logged %q; log %q", want, b)
		}
		return string(b)
	}
	waitFor("ready")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	got := waitFor("TERM")
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(got, "INT") {
		t.Fatalf("SIGINT was forwarded to the child; log %q", got)
	}
}

func TestRunFromAnUnsignedBerthSaysToBuild(t *testing.T) {
	fake := &fakeSecrets{useErr: reasonError(t, berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED)}
	read := func(context.Context, []byte) ([]byte, error) {
		t.Error("a refused run read a value")
		return nil, nil
	}
	r := invoke(t, fake, read, "run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c", "true")
	if r.code != ExitError || !strings.Contains(r.stderr, "run make build") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

// TestRunPassesOnlyTheAllowlistAndGrantedNames sets variables that steer tools (NODE_OPTIONS,
// GIT_CONFIG_*) on berth's own environment; only the allowlist and the grant's names reach the child.
func TestRunPassesOnlyTheAllowlistAndGrantedNames(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("E2E_GRANTED", "yes")
	t.Setenv("E2E_NOT_GRANTED", "no")
	fake := &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, passEnv: []string{"E2E_GRANTED", "NODE_OPTIONS"}}
	read := func(context.Context, []byte) ([]byte, error) { return []byte("v"), nil }
	r := invoke(t, fake, read, "run", "--env", "KEY=gh-token", "--", "/usr/bin/env")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	for _, banned := range []string{"NODE_OPTIONS", "GIT_CONFIG_COUNT", "E2E_NOT_GRANTED"} {
		if strings.Contains(r.stdout, banned+"=") {
			t.Fatalf("%s reached the child:\n%s", banned, r.stdout)
		}
	}
	for _, want := range []string{"E2E_GRANTED=yes", "KEY=****", "PATH="} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("child env missing %s:\n%s", want, r.stdout)
		}
	}
}

// TestRunKillsAChildThatIsNotTheGrantedCode: berthd pinned one cdhash, the kernel loaded
// another image, so the child is killed while still suspended and never runs.
func TestRunKillsAChildThatIsNotTheGrantedCode(t *testing.T) {
	cases := map[string][]byte{
		"another image": bytes.Repeat([]byte{0xee}, 20),
		"no pin":        {},
	}
	for name, cdhash := range cases {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			read := func(context.Context, []byte) ([]byte, error) { return []byte(canary), nil }
			r := invoke(t, &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, cdhash: cdhash}, read,
				"run", "--env", "KEY=gh-token", "--", "/usr/bin/touch", marker)
			if r.code != ExitStaleGrant || !strings.Contains(r.stderr, "berth secret grant gh-token --exe /usr/bin/touch") {
				t.Fatalf("code %d, stderr %q", r.code, r.stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the child ran: %v", err)
			}
		})
	}
}

// TestRunKillsAChildWhoseSignatureIsNotValid: the right cdhash is not enough; the kernel must
// also call the code's signature valid, or the child is killed before it runs.
func TestRunKillsAChildWhoseSignatureIsNotValid(t *testing.T) {
	t.Cleanup(func() { childCode = spawn.Live })
	childCode = func(pid int) (ports.LiveCode, error) {
		live, err := spawn.Live(pid)
		live.Status &^= ports.CSValid
		return live, err
	}
	marker := filepath.Join(t.TempDir(), "started")
	read := func(context.Context, []byte) ([]byte, error) { return []byte(canary), nil }
	r := invoke(t, &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}, read, "run", "--env", "KEY=gh-token", "--", "/usr/bin/touch", marker)
	if r.code != ExitStaleGrant {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the child ran: %v", err)
	}
}

// TestRunPassesStdinAndArgv: a suspended start still hands the child berth's stdin and its
// arguments, with the executable as argv[0].
func TestRunPassesStdinAndArgv(t *testing.T) {
	fake := &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}
	read := func(context.Context, []byte) ([]byte, error) { return []byte("v"), nil }
	var out, errOut bytes.Buffer
	server := connect.NewServer()
	berthv1connect.RegisterSecretsServiceHandler(server, fake)
	cl := &client.Client{Secrets: berthv1connect.NewSecretsServiceClient(connect.NewClient(connectinprocess.New(server)))}
	code := Run(t.Context(), []string{"run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c", `read line; printf '%s %s' "$0" "$line"`}, Env{
		Stdin: strings.NewReader("from stdin\n"), Stdout: &out, Stderr: &errOut,
		ConnectTrusted: func(context.Context) (*client.Client, error) { return cl, nil }, Read: read,
	})
	if code != 0 || out.String() != "/bin/sh from stdin" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

// TestRunChecksAScriptByItsInterpreter: the kernel runs a #! script's interpreter, so that is the
// cdhash the suspended child shows and the one berthd pins; the script's own bytes are not covered.
func TestRunChecksAScriptByItsInterpreter(t *testing.T) {
	script := filepath.Join(t.TempDir(), "deploy")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	read := func(context.Context, []byte) ([]byte, error) { return []byte("v"), nil }
	r := invoke(t, &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}, read, "run", "--env", "KEY=gh-token", "--", script)
	if r.code != 0 || r.stdout != "ran" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestRunRefusesAValueWithANUL(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	read := func(context.Context, []byte) ([]byte, error) { return []byte("ab\x00cd"), nil }
	r := invoke(t, &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}, read, "run", "--env", "KEY=gh-token", "--", "/usr/bin/touch", marker)
	if r.code != ExitError || !strings.Contains(r.stderr, "secret gh-token contains a NUL byte") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the child ran: %v", err)
	}
}
