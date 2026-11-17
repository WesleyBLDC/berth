package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/client"
)

var (
	sessionLine = regexp.MustCompile(`(?m)^session ([0-9a-f]{32}) expires `)
	tokenLine   = regexp.MustCompile(`(?m)^token (berth_at_[A-Za-z0-9_-]{43})$`)
)

// newAgentSession issues a session with berth agent session new and writes its token to a file
// only this user can read, as the person would for the agent; it returns the id, the token and
// the file.
func newAgentSession(t *testing.T, h string, args ...string) (id, token, file string) {
	t.Helper()
	out := berth(t, h, append([]string{"agent", "session", "new", "--name", "e2e"}, args...)...)
	s, tok := sessionLine.FindStringSubmatch(out), tokenLine.FindStringSubmatch(out)
	if s == nil || tok == nil {
		t.Fatalf("berth agent session new printed no session or token (%d bytes)", len(out))
	}
	file = filepath.Join(tempDir(t, "berth-token-"), "token")
	if err := os.WriteFile(file, []byte(tok[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s[1], tok[1], file
}

// agentEnv points berth at the agent socket with the token file.
func agentEnv(sock, file string) []string {
	return []string{"BERTH_AGENT_SOCKET=" + sock, "BERTH_AGENT_TOKEN_FILE=" + file}
}

// orphanRun runs berth with args from a process launchd adopted, so it shares this test's uid but
// not its process tree, and returns berth's output and exit code.
func orphanRun(t *testing.T, h string, env []string, args ...string) (string, int) {
	t.Helper()
	dir := tempDir(t, "berth-orphan-")
	out, code := filepath.Join(dir, "out"), filepath.Join(dir, "code")
	// The subshell outlives sh, so launchd adopts it before it execs berth.
	script := `( sleep 1; "$BERTH_BIN" "$@" >"$OUT" 2>&1; echo $? >"$CODE.tmp"; mv "$CODE.tmp" "$CODE" ) &`
	cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Env = append(append(os.Environ(), "BERTH_HOME="+h, "BERTH_BIN="+filepath.Join(binDir, "berth"), "OUT="+out, "CODE="+code), env...)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(code); err == nil { //nolint:gosec // the test's own folder
			var n int
			_, _ = fmt.Sscanf(string(b), "%d", &n)
			return readFile(t, out), n
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the orphaned berth never finished")
	return "", 0
}

func TestAgentSessionTokenBindsToItsProcessTree(t *testing.T) {
	h := home(t)
	d, sock := startWithAgent(t, h, os.Getuid())
	var outputs []string
	id, token, file := newAgentSession(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// No token: even the handshake is refused.
	_, err := client.Connect(ctx, sock, "e2e-no-token")
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
		t.Fatalf("no token: %v (%v)", err, api.ReasonOf(err))
	}
	if _, err := client.ConnectAgent(ctx, sock, "e2e-forged", "berth_at_"+strings.Repeat("A", 43)); api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
		t.Fatalf("forged token: %v", err)
	}

	// The first call binds the session to this test's process tree; a second process in the
	// tree is accepted too.
	for i := range 2 {
		if out, code := cliRunEnv(t, h, agentEnv(sock, file), &outputs, "secret", "ls"); code != 0 {
			t.Fatalf("agent secret ls %d: %d %s", i, code, out)
		}
	}
	list := berth(t, h, "agent", "session", "ls")
	outputs = append(outputs, list)
	if !regexp.MustCompile(id + `\s+e2e\s+bound\s+[0-9]+`).MatchString(list) {
		t.Fatalf("session not bound:\n%s", list)
	}

	// The same token from a process of the same user outside the tree is refused.
	out, code := orphanRun(t, h, agentEnv(sock, file), "secret", "ls")
	outputs = append(outputs, out)
	if code != 1 || !strings.Contains(out, "refused the agent token") {
		t.Fatalf("orphan with the token: %d %s", code, out)
	}
	if !strings.Contains(readFile(t, filepath.Join(h, "berthd.log")), "caller outside the session's process tree") {
		t.Fatal("berthd did not log why it refused the orphan")
	}

	// Revoked: refused from inside the tree as well.
	outputs = append(outputs, berth(t, h, "agent", "session", "revoke", id))
	if out, code := cliRunEnv(t, h, agentEnv(sock, file), &outputs, "secret", "ls"); code != 1 || !strings.Contains(out, "refused the agent token") {
		t.Fatalf("revoked token: %d %s", code, out)
	}

	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	_ = d.cmd.Wait()
	newCanaryChecker(t, token, strings.TrimPrefix(token, "berth_at_")).checkHome(h, outputs)
}

func TestAgentSessionTokenExpires(t *testing.T) {
	h := home(t)
	_, sock := startWithAgent(t, h, os.Getuid())
	var outputs []string
	id, _, file := newAgentSession(t, h, "--ttl", "2s")
	if out, code := cliRunEnv(t, h, agentEnv(sock, file), &outputs, "secret", "ls"); code != 0 {
		t.Fatalf("before expiry: %d %s", code, out)
	}
	time.Sleep(2500 * time.Millisecond)
	if out, code := cliRunEnv(t, h, agentEnv(sock, file), &outputs, "secret", "ls"); code != 1 || !strings.Contains(out, "refused the agent token") {
		t.Fatalf("after expiry: %d %s", code, out)
	}
	if list := berth(t, h, "agent", "session", "ls"); !regexp.MustCompile(id + `\s+e2e\s+expired`).MatchString(list) {
		t.Fatalf("session not expired:\n%s", list)
	}
}

// TestAgentStoreRequestsAreRateLimited holds three store dialogs open, which is the limit, then
// lets them go and finds the hourly limit at ten.
func TestAgentStoreRequestsAreRateLimited(t *testing.T) {
	h := home(t)
	d, sock := startWithAgent(t, h, os.Getuid())
	owner := waitServing(t, h)
	writeFake(t, h, map[string]any{"answer": "hang"})
	_, token, _ := newAgentSession(t, h)
	agent, err := client.ConnectAgent(t.Context(), sock, "e2e-agent", token)
	if err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context, name string) error {
		_, err := agent.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: "store-" + name, Name: name, Reason: "rate limit test"})
		return err
	}

	held, release := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for _, name := range []string{"e2e-open-1", "e2e-open-2", "e2e-open-3"} {
		wg.Go(func() { _ = request(held, name) })
		waitPending(t, owner, "secret/"+name)
	}
	err = request(t.Context(), "e2e-open-4")
	if connect.CodeOf(err) != connect.CodeResourceExhausted || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("a fourth open store: %v (%v)", err, api.ReasonOf(err))
	}
	if _, found := entityStateOK(t, owner, "secret/e2e-open-4"); found {
		t.Fatal("the refused store reached the secret machine")
	}

	// The agent gives up on the three; seven more stores fit in the hour, then none.
	release()
	wg.Wait()
	writeFake(t, h, approving("unused"))
	for i := range 7 {
		if err := request(t.Context(), fmt.Sprintf("e2e-more-%d", i)); err != nil {
			t.Fatalf("store %d of the hour: %v", i+4, err)
		}
	}
	err = request(t.Context(), "e2e-eleventh")
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("an eleventh store in the hour: %v", err)
	}
	// The owner is never limited.
	if _, err := owner.Secrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "owner-store-1", Name: "e2e-owner", Reason: "owner"}); err != nil {
		t.Fatalf("owner store: %v", err)
	}

	// A clean stop ends the hung dialogs and their helpers.
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	_ = d.cmd.Wait()
	for _, p := range helperProcesses(t) {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}

// waitPending waits until the entity exists and is Pending.
func waitPending(t *testing.T, c *client.Client, entity string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := entityStateOK(t, c, entity); st == "Pending" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became Pending", entity)
}

// entityStateOK reads an entity's state, reporting whether berthd has one.
func entityStateOK(t *testing.T, c *client.Client, entity string) (string, bool) {
	t.Helper()
	st, err := c.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: entity})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return st.GetState(), true
}
