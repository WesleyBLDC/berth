package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/client"
)

// startWithAgent runs berthd with the agent socket on for agentUID, and returns its path.
func startWithAgent(t *testing.T, h string, agentUID int) (*daemonProc, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b", "u", "agent.sock")
	d := start(t, h, "-agent-uid", strconv.Itoa(agentUID), "-agent-gid", strconv.Itoa(os.Getgid()), "-agent-socket", sock)
	waitServing(t, h)
	return d, sock
}

func TestAgentSocketServesOnlyAgentMethods(t *testing.T) {
	h := home(t)
	// One user plays the agent: the socket accepts this test's own uid.
	d, sock := startWithAgent(t, h, os.Getuid())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	agent, err := client.Connect(ctx, sock, "e2e-agent")
	if err != nil {
		t.Fatalf("Handshake, an agent method, failed on the agent socket: %v", err)
	}
	if _, err := agent.Secrets.ListSecrets(ctx, &berthv1.ListSecretsRequest{}); err != nil {
		t.Fatalf("ListSecrets, an agent method, failed on the agent socket: %v", err)
	}
	for name, call := range map[string]func() error{
		"Status": func() error { _, err := agent.Daemon.Status(ctx, &berthv1.StatusRequest{}); return err },
		"GetState": func() error {
			_, err := agent.Debug.GetState(ctx, &berthv1.GetStateRequest{Entity: "daemon"})
			return err
		},
		"Shutdown": func() error {
			_, err := agent.Daemon.Shutdown(ctx, &berthv1.ShutdownRequest{RequestId: "agent-stop-0001"})
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodePermissionDenied || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_NOT_AGENT_METHOD {
			t.Errorf("%s on the agent socket: got %v (%v), want PermissionDenied and NOT_AGENT_METHOD", name, err, api.ReasonOf(err))
		}
	}

	// The owner socket still serves everything, and the refused Shutdown did not stop berthd.
	owner := waitServing(t, h)
	if _, err := owner.Debug.GetState(ctx, &berthv1.GetStateRequest{Entity: "daemon"}); err != nil {
		t.Fatalf("GetState on the owner socket: %v", err)
	}

	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("agent socket left behind: %v", err)
	}
}

func TestAgentSocketRefusesEveryOtherUser(t *testing.T) {
	h := home(t)
	_, sock := startWithAgent(t, h, os.Getuid()+1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := client.Connect(ctx, sock, "e2e-not-agent"); err == nil {
		t.Fatal("a uid other than the agent uid completed a Handshake on the agent socket")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the agent socket is not there, so the refusal proves nothing: %v", err)
	}
}
