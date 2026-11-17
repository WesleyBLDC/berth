package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	// The folders berth agent setup makes: a parent no one else can write, and the socket folder
	// with the agent group and mode 0710; berthd only checks them.
	sockDir := filepath.Join(dir, "b", "u")
	if err := os.MkdirAll(sockDir, 0o755); err != nil { //nolint:gosec // setup makes the parent 0755
		t.Fatal(err)
	}
	if err := os.Chown(sockDir, -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sockDir, 0o710); err != nil { //nolint:gosec // the mode setup gives it
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir, "agent.sock")
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
		"ListSessions": func() error {
			_, err := agent.Sessions.ListSessions(ctx, &berthv1.ListSessionsRequest{})
			return err
		},
		"InspectSession": func() error {
			_, err := agent.Sessions.InspectSession(ctx, &berthv1.InspectSessionRequest{Id: "1-1"})
			return err
		},
		"StopSession": func() error {
			_, err := agent.Sessions.StopSession(ctx, &berthv1.StopSessionRequest{RequestId: "agent-stop-0002", Id: "1-1"})
			return err
		},
		"WatchSessions": func() error {
			stream, err := agent.Sessions.WatchSessions(ctx, &berthv1.WatchSessionsRequest{})
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			_, err = stream.Receive()
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

// TestRequestLogKeepsEachCallersReplies reuses the owner's request id from the agent socket: the
// agent must get its own call acted on, never the reply berthd stored for the owner.
func TestRequestLogKeepsEachCallersReplies(t *testing.T) {
	h := home(t)
	_, sock := startWithAgent(t, h, os.Getuid())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := waitServing(t, h)
	agent, err := client.Connect(ctx, sock, "e2e-agent")
	if err != nil {
		t.Fatal(err)
	}

	const id = "shared-request-0001"
	first, err := owner.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: id, Name: "owner-secret", Reason: "owner"})
	if err != nil || first.GetName() != "owner-secret" {
		t.Fatalf("owner store: %v %v", first, err)
	}
	got, err := agent.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: id, Name: "agent-secret", Reason: "agent"})
	if err != nil {
		t.Fatalf("agent store with the owner's request id: %v", err)
	}
	if got.GetName() != "agent-secret" {
		t.Fatalf("the agent got the owner's stored reply: %v", got)
	}
	list, err := owner.Secrets.ListSecrets(ctx, &berthv1.ListSecretsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list.GetSecrets() {
		names = append(names, s.GetName())
	}
	if !slices.Contains(names, "owner-secret") || !slices.Contains(names, "agent-secret") {
		t.Fatalf("both stores should have acted, secrets are %v", names)
	}

	// A retry from the same caller still replays its own reply.
	again, err := owner.Secrets.RequestSecret(ctx, &berthv1.RequestSecretRequest{RequestId: id, Name: "owner-secret", Reason: "owner"})
	if err != nil || again.GetName() != "owner-secret" || again.GetOutcome() != first.GetOutcome() {
		t.Fatalf("owner retry: %v %v", again, err)
	}
}

func TestAgentSocketWithoutSetupStopsBerthd(t *testing.T) {
	h := home(t)
	dir, err := os.MkdirTemp("", "berth-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := start(t, h, "-agent-uid", strconv.Itoa(os.Getuid()), "-agent-gid", strconv.Itoa(os.Getgid()),
		"-agent-socket", filepath.Join(dir, "b", "u", "agent.sock"))
	_ = d.cmd.Wait()
	log, _ := os.ReadFile(filepath.Join(h, "berthd.log"))
	if !strings.Contains(string(log), "run berth agent setup") {
		t.Fatalf("berthd did not say to run setup; log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); !os.IsNotExist(err) {
		t.Fatalf("berthd created the folder setup owns: %v", err)
	}
}
