package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/paths"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"berth/internal/adapters/api"
)

// cliRun runs berth and returns its combined output and exit code; outputs collects every
// output so the test can prove the canary never appeared in one.
func cliRun(t *testing.T, h string, outputs *[]string, args ...string) (string, int) {
	t.Helper()
	return cliRunEnv(t, h, nil, outputs, args...)
}

// cliRunEnv is cliRun with extra variables on berth's own environment.
func cliRunEnv(t *testing.T, h string, extra []string, outputs *[]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(binDir, "berth"), args...)
	cmd.Env = append(append(os.Environ(), "BERTH_HOME="+h), extra...)
	out, err := cmd.CombinedOutput()
	*outputs = append(*outputs, string(out))
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("berth %v: %v", args, err)
	return "", 0
}

// helperRead runs the fake berth-native read the way berth run does, from this test process.
func helperRead(t *testing.T, h string, ticket []byte) ([]byte, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(binDir, "berth-native"), "read")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "BERTH_HOME=" + h}
	cmd.Stdin = strings.NewReader(hex.EncodeToString(ticket) + "\n")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), 0
	case errors.As(err, &exit):
		return stdout.Bytes(), exit.ExitCode()
	}
	t.Fatalf("helper read: %v", err)
	return nil, 0
}

// consumerCopy copies the compiled consumer into its own folder, so the test can change its bytes.
func consumerCopy(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-exe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	exe := filepath.Join(dir, "consumer")
	if err := os.WriteFile(exe, []byte(readFile(t, filepath.Join(binDir, "consumer"))), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	return exe
}

// grantedShell writes a #! script, the kind of interpreter a grant must flag.
func grantedShell(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-exe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	exe := filepath.Join(dir, "granted-sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	return exe
}

func TestSecretStoreGrantRunAndRefusals(t *testing.T) {
	h := home(t)
	canary := "canary-e2e-" + rand.Text()
	fakeDir := filepath.Join(h, "fake-native")
	if err := os.MkdirAll(fakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"answer": "approve", "value": base64.StdEncoding.EncodeToString([]byte(canary))})
	if err := os.WriteFile(filepath.Join(fakeDir, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	d := start(t, h)
	c := waitServing(t, h)
	var outputs []string
	var logs []string

	out, code := cliRun(t, h, &outputs, "secret", "request", "e2e-token", "--why", "the e2e test needs it")
	if code != 0 || out != "saved e2e-token\n" {
		t.Fatalf("request: %d %q", code, out)
	}
	// The dialog was shown facts berthd learned from the kernel about the berth process.
	var shown struct {
		Command string   `json:"command"`
		Env     []string `json:"env"`
		Request struct {
			Requester struct {
				Executable string `json:"executable"`
			} `json:"requester"`
		} `json:"request"`
	}
	line, _, _ := strings.Cut(readFile(t, filepath.Join(fakeDir, "requests.jsonl")), "\n")
	if err := json.Unmarshal([]byte(line), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Command != "store" || filepath.Base(shown.Request.Requester.Executable) != "berth" ||
		strings.Join(shown.Env, ",") != "BERTH_HOME,HOME,PATH" {
		t.Fatalf("store prompt: %+v", shown)
	}
	if out, code = cliRun(t, h, &outputs, "secret", "ls"); code != 0 || !strings.Contains(out, "e2e-token") || !strings.Contains(out, "stored") {
		t.Fatalf("ls: %d %q", code, out)
	}

	exe := consumerCopy(t)
	out, code = cliRun(t, h, &outputs, "secret", "grant", "e2e-token", "--exe", exe, "--pass-env", "E2E_PASS", "--pass-env", "NODE_OPTIONS")
	if code != 0 || !strings.Contains(out, "granted e2e-token") || !strings.Contains(out, "env     E2E_PASS\n") {
		t.Fatalf("grant: %d %q", code, out)
	}

	// The child sees the exact value (its hash matches) while berth masks the value itself. Of
	// the variables set on berth, only the one the grant lists passes: NODE_OPTIONS is dropped
	// even though listed, and GIT_CONFIG_COUNT was never granted.
	sum := sha256.Sum256([]byte(canary))
	steer := []string{"E2E_PASS=granted", "NODE_OPTIONS=--require /tmp/evil.js", "GIT_CONFIG_COUNT=1"}
	out, code = cliRunEnv(t, h, steer, &outputs, "run", "--env", "K=e2e-token", "--", exe)
	want := hex.EncodeToString(sum[:]) + "\nE2E_PASS=granted\nNODE_OPTIONS unset\nGIT_CONFIG_COUNT unset\n****"
	if code != 0 || out != want {
		t.Fatalf("run: %d %q\nwant %q", code, out, want)
	}

	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", "/bin/echo", "hi"); code != 5 || !strings.Contains(out, "no grant") {
		t.Fatalf("other executable: %d %q", code, out)
	}

	// An interpreter can print the secret, so granting one takes a deliberate flag, and the
	// Touch ID prompt says so.
	script := grantedShell(t)
	if out, code = cliRun(t, h, &outputs, "secret", "grant", "e2e-token", "--exe", script); code != 1 || !strings.Contains(out, "is an interpreter") {
		t.Fatalf("interpreter without the flag: %d %q", code, out)
	}
	if out, code = cliRun(t, h, &outputs, "secret", "grant", "e2e-token", "--exe", script, "--allow-interpreter"); code != 0 {
		t.Fatalf("interpreter with the flag: %d %q", code, out)
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(fakeDir, "requests.jsonl"))), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"command":"confirm"`) || !strings.Contains(last, `"interpreter":true`) {
		t.Fatalf("confirm prompt for the interpreter: %s", last)
	}

	// The read attack: a process that is not the signed berth gets no ticket, so it has
	// nothing to hand a berth-native it starts itself.
	ctx := t.Context()
	_, err := c.Secrets.AuthorizeUse(ctx, &berthv1.AuthorizeUseRequest{RequestId: "e2e-authorize-1", Name: "e2e-token", ExePath: exe})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Fatalf("authorize from a process that is not berth: %v", err)
	}
	if value, code := helperRead(t, h, make([]byte, 32)); code != 3 || len(value) != 0 {
		t.Fatalf("helper read with a made up ticket: exit %d, %d bytes", code, len(value))
	}

	// This test process is not berth-native either. Its request id is already in the log
	// (from RequestSecret, which any process may call), so reading the log first would answer
	// REQUEST_ID_REUSED instead of PEER_NOT_TRUSTED.
	if _, err := c.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: "e2e-request-2", Name: "e2e-other", Reason: "log entry"}); err != nil {
		t.Fatal(err)
	}
	_, err = c.Secrets.RedeemUseTicket(ctx, &berthv1.RedeemUseTicketRequest{RequestId: "e2e-request-2", Ticket: make([]byte, 32)})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Fatalf("untrusted redeem: %v", err)
	}

	f, err := os.OpenFile(exe, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the test's own copy
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("changed")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", exe); code != 6 || !strings.Contains(out, "changed since it was granted") {
		t.Fatalf("changed executable: %d %q", code, out)
	}
	if out, code = cliRun(t, h, &outputs, "secret", "ls"); code != 0 || !strings.Contains(out, "stale") {
		t.Fatalf("ls after change: %d %q", code, out)
	}

	log, _ := os.ReadFile(filepath.Join(h, "berthd.log"))
	logs = append(logs, string(log))

	// Restart: state, grants and use counts come back from the database.
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
	start(t, h)
	c = waitServing(t, h)
	list, err := c.Secrets.ListSecrets(ctx, &berthv1.ListSecretsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(exe)
	var cons *berthv1.Consumer
	for _, sec := range list.GetSecrets() {
		if sec.GetName() != "e2e-token" {
			continue
		}
		if sec.GetState() != berthv1.SecretState_SECRET_STATE_STORED || len(sec.GetConsumers()) != 2 {
			t.Fatalf("after restart: %v", sec)
		}
		for _, c := range sec.GetConsumers() {
			if c.GetExePath() == resolved {
				cons = c
			}
		}
	}
	if cons == nil || cons.GetState() != berthv1.GrantState_GRANT_STATE_STALE || cons.GetUseCount() != 1 || cons.GetLastUsedAt() == nil {
		t.Fatalf("grant after restart: %v in %v", cons, list)
	}
	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", exe); code != 6 {
		t.Fatalf("stale after restart: %d %q", code, out)
	}
	log, _ = os.ReadFile(filepath.Join(h, "berthd.log"))
	logs = append(logs, string(log))

	// The canary lives only in the fake Keychain (fake-native/values) and its config.
	forms := []string{canary, hex.EncodeToString([]byte(canary)), base64.StdEncoding.EncodeToString([]byte(canary))}
	check := func(where string, b []byte) {
		for _, form := range forms {
			if bytes.Contains(b, []byte(form)) {
				t.Errorf("canary found in %s", where)
			}
		}
	}
	for i, o := range outputs {
		check("CLI output "+strconv.Itoa(i), []byte(o))
	}
	for _, l := range logs {
		check("berthd log", []byte(l))
	}
	entries, err := os.ReadDir(h)
	if err != nil {
		t.Fatal(err)
	}
	var dbFiles int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "berth.db") {
			b, err := os.ReadFile(filepath.Join(h, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			check(e.Name(), b)
			dbFiles++
		}
	}
	if dbFiles == 0 {
		t.Fatal("no database files found to check")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// helperProcesses lists running processes started from this test's berth-native.
func helperProcesses(t *testing.T) []int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		t.Fatal(err)
	}
	// berthd starts the helper by its resolved path (/private/var for a temporary folder).
	dir, err := filepath.EvalSymlinks(binDir)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pid, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.TrimSpace(comm) == filepath.Join(dir, "berth-native") {
			n, _ := strconv.Atoi(pid)
			pids = append(pids, n)
		}
	}
	return pids
}

// openDialog starts berth secret request against a helper that never answers, and waits until
// berthd has recorded the secret as Pending.
func openDialog(t *testing.T, h, name string) *exec.Cmd {
	t.Helper()
	fakeDir := filepath.Join(h, "fake-native")
	if err := os.MkdirAll(fakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "config.json"), []byte(`{"answer":"hang"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := waitServing(t, h)
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(binDir, "berth"), "secret", "request", name, "--why", "shutdown test")
	cmd.Env = append(os.Environ(), "BERTH_HOME="+h)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := c.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: "secret/" + name})
		if err == nil && st.GetState() == "Pending" && len(helperProcesses(t)) > 0 {
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the dialog never opened")
	return nil
}

func lastSecretEvent(t *testing.T, h, name string) *berthv1.Transition {
	t.Helper()
	c := waitServing(t, h)
	st, err := c.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: "secret/" + name, HistoryLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return st.GetHistory()[0]
}

func TestStopWithAnOpenStoreDialog(t *testing.T) {
	h := home(t)
	d := start(t, h)
	request := openDialog(t, h, "e2e-open")
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
	if pids := helperProcesses(t); len(pids) != 0 {
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		t.Fatalf("berth-native still running after berthd exited: %v", pids)
	}
	err := request.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 4 {
		t.Fatalf("berth secret request ended with %v, want exit 4 (failed)", err)
	}
	start(t, h)
	if ev := lastSecretEvent(t, h, "e2e-open"); ev.GetToState() != "Failed" || ev.GetDetail() != "berthd stopped while the prompt was open" {
		t.Fatalf("last event %v", ev)
	}
}

func TestBootEndsAStoreACrashLeftPending(t *testing.T) {
	h := home(t)
	d := start(t, h)
	openDialog(t, h, "e2e-crash")
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	_ = d.cmd.Wait()
	// A crash cannot clean up its helper; this test owns it, so it ends it.
	for _, p := range helperProcesses(t) {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	start(t, h)
	if ev := lastSecretEvent(t, h, "e2e-crash"); ev.GetToState() != "Failed" || ev.GetDetail() != "interrupted" {
		t.Fatalf("last event %v", ev)
	}
}

// fakeDaemon is a server an agent could start: it answers the API on a socket in its own
// BERTH_HOME and would hand out tickets.
type fakeDaemon struct {
	berthv1connect.UnimplementedDaemonServiceHandler
	berthv1connect.UnimplementedSecretsServiceHandler
	authorized atomic.Int32
}

func (f *fakeDaemon) Handshake(context.Context, *berthv1.HandshakeRequest) (*berthv1.HandshakeResponse, error) {
	return &berthv1.HandshakeResponse{ProtocolVersion: api.ProtocolVersion, DaemonVersion: "fake"}, nil
}

func (f *fakeDaemon) AuthorizeUse(context.Context, *berthv1.AuthorizeUseRequest) (*berthv1.AuthorizeUseResponse, error) {
	f.authorized.Add(1)
	return &berthv1.AuthorizeUseResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, Ticket: make([]byte, 32)}, nil
}

func TestRunRefusesAServerThatIsNotTheSignedBerthd(t *testing.T) {
	h := home(t)
	fake := &fakeDaemon{}
	server := connect.NewServer()
	berthv1connect.RegisterDaemonServiceHandler(server, fake)
	berthv1connect.RegisterSecretsServiceHandler(server, fake)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithProtoJSON())
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", paths.Socket(h))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var outputs []string
	out, code := cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", filepath.Join(binDir, "consumer"))
	if code != 1 || !strings.Contains(out, "not the signed berthd") {
		t.Fatalf("run against a fake berthd: %d %q", code, out)
	}
	if n := fake.authorized.Load(); n != 0 {
		t.Fatalf("berth asked the fake berthd for %d tickets", n)
	}
}

// TestCopiedBerthRefusesAWrapperHelper copies the signed berth and berthd into a folder of the
// agent's, beside a wrapper script named berth-native that would save the value to a file. berth
// must refuse before asking for a ticket, and the value must reach nothing.
func TestCopiedBerthRefusesAWrapperHelper(t *testing.T) {
	h := home(t)
	canary := "canary-copy-" + rand.Text()
	fakeDir := filepath.Join(h, "fake-native")
	if err := os.MkdirAll(fakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"answer": "approve", "value": base64.StdEncoding.EncodeToString([]byte(canary))})
	if err := os.WriteFile(filepath.Join(fakeDir, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, h)
	c := waitServing(t, h)
	var outputs []string
	if out, code := cliRun(t, h, &outputs, "secret", "request", "e2e-copy", "--why", "copy test"); code != 0 {
		t.Fatalf("request: %d %q", code, out)
	}
	exe := consumerCopy(t)
	if out, code := cliRun(t, h, &outputs, "secret", "grant", "e2e-copy", "--exe", exe); code != 0 {
		t.Fatalf("grant: %d %q", code, out)
	}

	dir, err := os.MkdirTemp("", "berth-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, name := range []string{"berth", "berthd"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(readFile(t, filepath.Join(binDir, name))), 0o700); err != nil { //nolint:gosec // a copy of the test binary
			t.Fatal(err)
		}
	}
	leak := filepath.Join(dir, "leak")
	wrapper := "#!/bin/sh\n\"" + filepath.Join(binDir, "berth-native") + "\" \"$@\" > \"" + leak + "\"\ncat \"" + leak + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "berth-native"), []byte(wrapper), 0o700); err != nil { //nolint:gosec // the attacker's wrapper
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "berth"), "run", "--env", "K=e2e-copy", "--", exe)
	cmd.Env = append(os.Environ(), "BERTH_HOME="+h)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "will not start this berth-native") {
		t.Fatalf("copied berth with a wrapper helper: %v %q", err, out)
	}
	if _, err := os.Stat(leak); !os.IsNotExist(err) {
		t.Fatalf("the wrapper ran: %v", err)
	}
	list, err := c.Secrets.ListSecrets(t.Context(), &berthv1.ListSecretsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.GetSecrets() {
		for _, cons := range s.GetConsumers() {
			if cons.GetUseCount() != 0 {
				t.Fatalf("a ticket was spent before the helper was refused: %v", cons)
			}
		}
	}
	everything := string(out) + strings.Join(outputs, "") + readFile(t, filepath.Join(h, "berthd.log"))
	for _, e := range mustReadDir(t, h) {
		if strings.HasPrefix(e, "berth.db") {
			everything += readFile(t, filepath.Join(h, e))
		}
	}
	if strings.Contains(everything, canary) {
		t.Fatal("the canary leaked")
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
