package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
)

// cliRun runs berth and returns its combined output and exit code; outputs collects every
// output so the test can prove the canary never appeared in one.
func cliRun(t *testing.T, h string, outputs *[]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(binDir, "berth"), args...)
	cmd.Env = append(os.Environ(), "BERTH_HOME="+h)
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

// grantedShell writes a private executable that runs /bin/sh, so the test can change its bytes.
// It is a script because macOS kills a copied platform binary such as /bin/sh.
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

	exe := grantedShell(t)
	if out, code = cliRun(t, h, &outputs, "secret", "grant", "e2e-token", "--exe", exe); code != 0 || !strings.Contains(out, "granted e2e-token") {
		t.Fatalf("grant: %d %q", code, out)
	}

	// The child sees the exact value (its hash matches) while berth masks the value itself.
	sum := sha256.Sum256([]byte(canary))
	out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", exe, "-c", `printf %s "$K" | /usr/bin/shasum -a 256; printf %s "$K"`)
	if code != 0 || !strings.Contains(out, hex.EncodeToString(sum[:])) || !strings.HasSuffix(out, "****") {
		t.Fatalf("run: %d %q", code, out)
	}

	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", "/bin/echo", "hi"); code != 5 || !strings.Contains(out, "no grant") {
		t.Fatalf("other executable: %d %q", code, out)
	}

	// A ticket redeems once, through the helper, for the process it was issued to.
	ctx := t.Context()
	auth, err := c.Secrets.AuthorizeUse(ctx, &berthv1.AuthorizeUseRequest{RequestId: "e2e-authorize-1", Name: "e2e-token", ExePath: exe})
	if err != nil || auth.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		t.Fatalf("authorize: %v %v", auth, err)
	}
	value, code := helperRead(t, h, auth.GetTicket())
	if code != 0 || !bytes.Equal(value, []byte(canary)) {
		t.Fatalf("first redeem exit %d, value matches %v", code, bytes.Equal(value, []byte(canary)))
	}
	clear(value)
	if value, code = helperRead(t, h, auth.GetTicket()); code != 3 || len(value) != 0 {
		t.Fatalf("replayed ticket exit %d, %d bytes", code, len(value))
	}

	// This test process is not berth-native. The reused request id proves the peer is
	// checked before the request log, which would otherwise answer REQUEST_ID_REUSED.
	_, err = c.Secrets.RedeemUseTicket(ctx, &berthv1.RedeemUseTicketRequest{RequestId: "e2e-authorize-1", Ticket: auth.GetTicket()})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Fatalf("untrusted redeem: %v", err)
	}

	f, err := os.OpenFile(exe, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the test's own copy
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("# changed\n")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", exe, "-c", "true"); code != 6 || !strings.Contains(out, "changed since it was granted") {
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
	secrets := list.GetSecrets()
	if len(secrets) != 1 || secrets[0].GetState() != berthv1.SecretState_SECRET_STATE_STORED || len(secrets[0].GetConsumers()) != 1 {
		t.Fatalf("after restart: %v", list)
	}
	cons := secrets[0].GetConsumers()[0]
	if cons.GetState() != berthv1.GrantState_GRANT_STATE_STALE || cons.GetUseCount() != 2 || cons.GetLastUsedAt() == nil {
		t.Fatalf("grant after restart: %v", cons)
	}
	if out, code = cliRun(t, h, &outputs, "run", "--env", "K=e2e-token", "--", exe, "-c", "true"); code != 6 {
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
