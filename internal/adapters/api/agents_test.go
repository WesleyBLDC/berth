package api_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

type fakeAgents struct {
	mu      sync.Mutex
	open    int
	admits  int
	creates int
}

func (f *fakeAgents) CreateAgentSession(context.Context, *berthv1.CreateAgentSessionRequest) (*berthv1.CreateAgentSessionResponse, error) {
	f.creates++
	return &berthv1.CreateAgentSessionResponse{Session: &berthv1.AgentSession{Id: "s1"}, Token: "berth_at_secret"}, nil
}

func (f *fakeAgents) ListAgentSessions(context.Context) (*berthv1.ListAgentSessionsResponse, error) {
	return &berthv1.ListAgentSessionsResponse{}, nil
}

func (f *fakeAgents) RevokeAgentSession(context.Context, *berthv1.RevokeAgentSessionRequest) (*berthv1.RevokeAgentSessionResponse, error) {
	return nil, api.ErrAgentSessionNotFound
}

func (f *fakeAgents) Authenticate(_ context.Context, token string, _ ports.Peer) (string, error) {
	switch token {
	case "good-1":
		return "s1", nil
	case "good-2":
		return "s2", nil
	}
	return "", api.ErrAgentTokenInvalid
}

// AdmitStore lets one store dialog be open at a time.
func (f *fakeAgents) AdmitStore(context.Context, string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open >= 1 {
		return nil, api.ErrRateLimited
	}
	f.open++
	f.admits++
	return func() { f.mu.Lock(); f.open--; f.mu.Unlock() }, nil
}

type agentFixture struct {
	agentServer *connect.Server
	owner       berthv1connect.AgentServiceClient
	fake        *fakeAgents
	secrets     *fakeSecrets
}

func setupAgents(t *testing.T) *agentFixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &agentFixture{fake: &fakeAgents{}, secrets: &fakeSecrets{}}
	owner := connect.NewServer()
	f.agentServer = connect.NewServer(api.AgentOnly(), api.AgentToken(f.fake))
	api.Register(api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { return nil },
		Secrets:     f.secrets, Agents: f.fake,
	}, owner, f.agentServer)
	f.owner = berthv1connect.NewAgentServiceClient(connect.NewClient(connectinprocess.New(owner)))
	return f
}

// agent is a client of the agent server that sends token, unless it is empty.
func (f *agentFixture) agent(token string) (berthv1connect.SecretsServiceClient, berthv1connect.DaemonServiceClient) {
	c := connect.NewClient(connectinprocess.New(f.agentServer), func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok && token != "" {
				info.RequestHeader().Set(api.AgentTokenHeader, token)
			}
			return next(ctx, spec)
		}
	})
	return berthv1connect.NewSecretsServiceClient(c), berthv1connect.NewDaemonServiceClient(c)
}

func TestAgentSocketNeedsAValidToken(t *testing.T) {
	f := setupAgents(t)
	for _, token := range []string{"", "forged"} {
		secrets, daemon := f.agent(token)
		_, err := secrets.ListSecrets(t.Context(), &berthv1.ListSecretsRequest{})
		if connect.CodeOf(err) != connect.CodeUnauthenticated || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
			t.Errorf("ListSecrets with token %q: %v (%v)", token, err, api.ReasonOf(err))
		}
		_, err = daemon.Handshake(t.Context(), &berthv1.HandshakeRequest{ProtocolVersion: api.ProtocolVersion})
		if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID {
			t.Errorf("Handshake with token %q: %v", token, err)
		}
		// A method closed to agents is refused as closed, token or not.
		_, err = daemon.Status(t.Context(), &berthv1.StatusRequest{})
		if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_NOT_AGENT_METHOD {
			t.Errorf("Status with token %q: %v", token, err)
		}
	}
	secrets, daemon := f.agent("good-1")
	if _, err := secrets.ListSecrets(t.Context(), &berthv1.ListSecretsRequest{}); err != nil {
		t.Fatalf("ListSecrets with a valid token: %v", err)
	}
	if _, err := daemon.Status(t.Context(), &berthv1.StatusRequest{}); api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_NOT_AGENT_METHOD {
		t.Fatalf("a valid token opened Status: %v", err)
	}
}

func TestAgentStoreRequestsAreLimitedPerSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := setupAgents(t)
		f.secrets.block = make(chan struct{})
		secrets, _ := f.agent("good-1")
		first := make(chan error, 1)
		go func() {
			_, err := secrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "store-0001", Name: "a", Reason: "r"})
			first <- err
		}()
		synctest.Wait()
		f.fake.mu.Lock()
		open := f.fake.open
		f.fake.mu.Unlock()
		if open != 1 {
			t.Fatal("the first store never opened")
		}
		_, err := secrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "store-0002", Name: "b", Reason: "r"})
		if connect.CodeOf(err) != connect.CodeResourceExhausted || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
			t.Fatalf("a store over the limit: %v (%v)", err, api.ReasonOf(err))
		}
		close(f.secrets.block)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		// A retry replays the stored reply without taking a slot.
		if _, err := secrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "store-0001", Name: "a", Reason: "r"}); err != nil {
			t.Fatal(err)
		}
		if f.fake.admits != 1 {
			t.Fatalf("admitted %d stores, want 1", f.fake.admits)
		}
		// The owner socket carries no session and is never limited.
		ownerSecrets := setupSecrets(t, &fakeSecrets{}).secrets
		for i := range 3 {
			if _, err := ownerSecrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "owner-000" + string(rune('1'+i)), Name: "o", Reason: "r"}); err != nil {
				t.Fatalf("owner store %d: %v", i, err)
			}
		}
	})
}

func TestEachAgentSessionKeepsItsOwnReplies(t *testing.T) {
	f := setupAgents(t)
	one, _ := f.agent("good-1")
	two, _ := f.agent("good-2")
	const id = "shared-0001"
	if got, err := one.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: id, Name: "one", Reason: "r"}); err != nil || got.GetName() != "one" {
		t.Fatalf("session one: %v %v", got, err)
	}
	got, err := two.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: id, Name: "two", Reason: "r"})
	if err != nil || got.GetName() != "two" {
		t.Fatalf("session two got another session's reply: %v %v", got, err)
	}
}

func TestCreateAgentSessionNeverRecordsTheToken(t *testing.T) {
	f := setupAgents(t)
	req := &berthv1.CreateAgentSessionRequest{RequestId: "create-0001"}
	first, err := f.owner.CreateAgentSession(t.Context(), req)
	if err != nil || first.GetToken() != "berth_at_secret" {
		t.Fatalf("create: %v %v", first, err)
	}
	again, err := f.owner.CreateAgentSession(t.Context(), req)
	if err != nil || again.GetToken() != "" || again.GetSession().GetId() != "s1" || f.fake.creates != 1 {
		t.Fatalf("retry: %v %v, creates %d", again, err, f.fake.creates)
	}
	_, err = f.owner.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: "0123456789abcdef0123456789abcdef"})
	if connect.CodeOf(err) != connect.CodeNotFound || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_AGENT_SESSION_NOT_FOUND {
		t.Fatalf("revoke unknown: %v", err)
	}
}
