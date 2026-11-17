package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
)

// agentProxyRun is berth run --proxy in agent mode, in the background, so the test knows berth's
// own pid and can kill it.
type agentProxyRun struct {
	cmd  *exec.Cmd
	out  *bytes.Buffer
	done chan struct{}
}

func startAgentRun(t *testing.T, h string, env []string, args ...string) *agentProxyRun {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(binDir, "berth"), args...)
	cmd.Env = append(append(os.Environ(), "BERTH_HOME="+h), env...)
	r := &agentProxyRun{cmd: cmd, out: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = r.out, r.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(r.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-r.done })
	return r
}

func (r *agentProxyRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.done:
		return r.cmd.ProcessState.ExitCode()
	case <-time.After(30 * time.Second):
		t.Fatalf("berth run did not exit; output %q", r.out.String())
		return 0
	}
}

// parentOf reads a process's parent pid from ps; 0 means it is gone.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// waitGone waits up to within for a process to exit.
func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d still runs after %s", pid, within)
}

// rawStartProxy calls StartProxy on the agent socket without the handshake, sending token in the
// agent header when it is not empty.
func rawStartProxy(t *testing.T, sock, token string) error {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	cl := berthv1connect.NewSecretsServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://berthd"),
		func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
				if info, ok := connect.CallInfoForClientContext(ctx); ok && token != "" {
					info.RequestHeader().Set(api.AgentTokenHeader, token)
				}
				return next(ctx, spec)
			}
		}))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := cl.StartProxy(ctx, &berthv1.StartProxyRequest{RequestId: "raw-start-" + rand.Text(), Names: []string{"e2e-key"}})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = stream.Receive()
	return err
}

// TestAgentProxyRunsUnderBerthd is E3 item 6 end to end, with this test's own uid as the agent:
// berth run --proxy in agent mode gets injection through a berth-proxy whose parent is berthd,
// the value reaches only the granted server, and closing berth or ending the agent session stops
// the proxy.
func TestAgentProxyRunsUnderBerthd(t *testing.T) {
	h := home(t)
	canary := "canary-agent-proxy-" + rand.Text()
	writeFake(t, h, approving(canary))
	d, sock := startWithAgent(t, h, os.Getuid())
	owner := waitServing(t, h)
	var outputs []string
	if out, code := cliRun(t, h, &outputs, "secret", "request", "e2e-key", "--why", "agent proxy"); code != 0 {
		t.Fatalf("request: %d %q", code, out)
	}
	a := newUpstream(t, leafCert(t, testRoot.cert, testRoot.key))
	b := newUpstream(t, leafCert(t, testRoot.cert, testRoot.key))
	selfSigned := newUpstream(t, leafCert(t, nil, nil))
	grantProxy(t, h, &outputs, "e2e-key", a.port)
	id, token, file := newAgentSession(t, h)
	agentTmp := tempDir(t, "berth-agent-tmp-")
	env := append(agentEnv(sock, file), "TMPDIR="+agentTmp, "E2E_EXTRA_CA="+testRoot.pemFile)
	check := newCanaryChecker(t, canary, token, strings.TrimPrefix(token, "berth_at_"))

	t.Run("a call without a valid token is refused and starts nothing", func(t *testing.T) {
		for name, tok := range map[string]string{"no token": "", "forged token": "berth_at_" + strings.Repeat("A", 43)} {
			err := rawStartProxy(t, sock, tok)
			if connect.CodeOf(err) != connect.CodeUnauthenticated || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
				t.Fatalf("%s: %v (%v)", name, err, api.ReasonOf(err))
			}
		}
		forged := filepath.Join(tempDir(t, "berth-token-"), "token")
		if err := os.WriteFile(forged, []byte("berth_at_"+strings.Repeat("B", 43)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := cliRunEnv(t, h, agentEnv(sock, forged), &outputs, "run", "--proxy", "e2e-key", "--", "/usr/bin/true")
		if code != 1 || !strings.Contains(out, "refused the agent token") {
			t.Fatalf("berth run with a forged token: %d %q", code, out)
		}
		if rows := sessions(t, h); len(rows) != 0 {
			t.Fatalf("a refused call made proxy sessions %+v", rows)
		}
	})

	t.Run("the canary run injects only on the granted host", func(t *testing.T) {
		caFiles := 0
		a.onHit(func(string) { caFiles = check.checkDir("agent CA folder during the run", agentTmp) })
		out, code := cliRunEnv(t, h, env, &outputs, "run", "--proxy", "e2e-key", "--",
			filepath.Join(binDir, "proxyconsumer"), "canary", a.addr, b.addr, selfSigned.addr)
		a.onHit(nil)
		if code != 0 {
			t.Fatalf("run: %d %q", code, out)
		}
		got := lines(out)
		if g := a.got("/a"); got["A"] != "200" || len(g) != 1 || g[0] != "Bearer "+canary {
			t.Errorf("A: status %s, %d headers, injected %v", got["A"], len(g), len(g) == 1 && g[0] == "Bearer "+canary)
		}
		// berthd's proxy runs as the owner, so it dials nothing it holds no grant for.
		if g := b.got("/b"); !strings.HasPrefix(got["B"], "error") || len(g) != 0 || got["OTHER"] != "403" {
			t.Errorf("B: status %s, CONNECT %s, headers %q", got["B"], got["OTHER"], g)
		}
		if got["PLAIN"] != "405" || got["WRONG"] != "407" || got["NOAUTH"] != "407" {
			t.Errorf("plain %q, wrong credential %q, no credential %q", got["PLAIN"], got["WRONG"], got["NOAUTH"])
		}
		var childEnv []string
		if err := json.Unmarshal([]byte(got["ENV"]), &childEnv); err != nil {
			t.Fatalf("environment dump: %v", err)
		}
		vars := map[string]string{}
		for _, kv := range childEnv {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		// The CA files are the agent's own copies, in its TMPDIR, not berth-proxy's folder.
		if vars["E2E_KEY"] != "berth-proxy-placeholder" || !strings.HasPrefix(vars["HTTPS_PROXY"], "http://berth:") ||
			!strings.HasPrefix(vars["SSL_CERT_FILE"], agentTmp) || !strings.HasPrefix(vars["NODE_EXTRA_CA_CERTS"], agentTmp) {
			t.Errorf("child environment: %v", vars)
		}
		if caFiles < 2 {
			t.Errorf("read %d files from the agent's CA folder during the run, want ca.pem and bundle.pem", caFiles)
		}
		if n := check.checkDir("agent CA folder after the run", agentTmp); n != 0 {
			t.Errorf("%d files left in the agent's CA folder", n)
		}
		// berth does not wait for berthd's proxy: closing the stream is all it does, and berthd
		// then ends the session.
		rows := sessions(t, h)
		if len(rows) != 1 {
			t.Fatalf("sessions %+v", rows)
		}
		waitState(t, owner, "proxy/"+rows[0].id, "Ended")
		if rows = sessions(t, h); !rows[0].ended || rows[0].injected != 1 {
			t.Fatalf("sessions %+v", rows)
		}
	})

	loop := []string{"run", "--proxy", "e2e-key", "--", filepath.Join(binDir, "proxyconsumer"), "loop", a.addr}

	t.Run("berthd is the proxy's parent, and killing berth stops the proxy", func(t *testing.T) {
		hit := firstHit(a)
		run := startAgentRun(t, h, env, loop...)
		waitHit(t, hit)
		rows := sessions(t, h)
		s := rows[len(rows)-1]
		if parent := parentOf(t, s.pid); parent != d.cmd.Process.Pid || parent == run.cmd.Process.Pid {
			t.Fatalf("berth-proxy %d has parent %d; berthd is %d, berth is %d", s.pid, parent, d.cmd.Process.Pid, run.cmd.Process.Pid)
		}
		if err := run.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		// The stream closes with berth, so berthd closes the proxy's stdin at once; the proxy ends its
		// session within its own 2 second end timeout, and berthd kills it after 5 seconds at most.
		waitState(t, owner, "proxy/"+s.id, "Ended")
		waitGone(t, s.pid, 10*time.Second)
		outputs = append(outputs, run.out.String())
	})

	t.Run("ending the agent session stops its proxy", func(t *testing.T) {
		hit := firstHit(a)
		run := startAgentRun(t, h, env, loop...)
		waitHit(t, hit)
		rows := sessions(t, h)
		s := rows[len(rows)-1]
		outputs = append(outputs, berth(t, h, "agent", "session", "revoke", id))
		waitState(t, owner, "proxy/"+s.id, "Ended")
		st, err := owner.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: "proxy/" + s.id, HistoryLimit: 5})
		if err != nil {
			t.Fatal(err)
		}
		stoppedByAgent := false
		for _, tr := range st.GetHistory() {
			stoppedByAgent = stoppedByAgent || tr.GetDetail() == "AgentSessionEnded"
		}
		if !stoppedByAgent {
			t.Fatalf("the session did not stop for its agent session: %v", st.GetHistory())
		}
		waitGone(t, s.pid, 10*time.Second)
		if code := run.wait(t); code != 0 {
			t.Fatalf("run: %d %q", code, run.out.String())
		}
		outputs = append(outputs, run.out.String())
	})

	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	_ = d.cmd.Wait()
	check.checkHome(h, outputs)
}
