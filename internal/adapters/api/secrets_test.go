package api_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
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

type fakeSecrets struct {
	storeErr  error
	block     chan struct{}
	trustErr  error
	redeems   int
	authorize int
}

func (f *fakeSecrets) RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest, _ int) (*berthv1.RequestSecretResponse, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	return &berthv1.RequestSecretResponse{Name: req.GetName(), Outcome: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, nil
}

func (f *fakeSecrets) ListSecrets(context.Context) (*berthv1.ListSecretsResponse, error) {
	return &berthv1.ListSecretsResponse{}, nil
}

func (f *fakeSecrets) GrantSecret(context.Context, *berthv1.GrantSecretRequest, int) (*berthv1.GrantSecretResponse, error) {
	return nil, api.ErrExecutableNotFound
}

func (f *fakeSecrets) AuthorizeUse(context.Context, *berthv1.AuthorizeUseRequest, int) (*berthv1.AuthorizeUseResponse, error) {
	f.authorize++
	return &berthv1.AuthorizeUseResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, Ticket: bytes.Repeat([]byte{7}, 32)}, nil
}

func (f *fakeSecrets) TrustRedeemer(context.Context, int) error { return f.trustErr }

func (f *fakeSecrets) RedeemUseTicket(context.Context, ports.Ticket, int) (*berthv1.RedeemUseTicketResponse, error) {
	f.redeems++
	return &berthv1.RedeemUseTicketResponse{Name: "gh-token"}, nil
}

type secretsFixture struct {
	secrets berthv1connect.SecretsServiceClient
	daemon  berthv1connect.DaemonServiceClient
	store   *sqlite.Store
}

func setupSecrets(t *testing.T, fake *fakeSecrets) secretsFixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := connect.NewServer()
	api.Register(server, api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { return nil },
		Secrets:     fake,
	})
	client := connect.NewClient(connectinprocess.New(server))
	return secretsFixture{
		secrets: berthv1connect.NewSecretsServiceClient(client),
		daemon:  berthv1connect.NewDaemonServiceClient(client),
		store:   store,
	}
}

func TestSecretsErrorsAreTyped(t *testing.T) {
	cases := []struct {
		err    error
		code   connect.Code
		reason berthv1.ErrorReason
	}{
		{api.ErrSecretExists, connect.CodeAlreadyExists, berthv1.ErrorReason_ERROR_REASON_SECRET_EXISTS},
		{api.ErrSecretNotFound, connect.CodeNotFound, berthv1.ErrorReason_ERROR_REASON_SECRET_NOT_FOUND},
		{api.ErrTicketInvalid, connect.CodePermissionDenied, berthv1.ErrorReason_ERROR_REASON_TICKET_INVALID},
		{errors.New("helper said /secret/path"), connect.CodeInternal, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
	}
	for _, tc := range cases {
		f := setupSecrets(t, &fakeSecrets{storeErr: tc.err})
		_, err := f.secrets.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{RequestId: "req-0001-abcd", Name: "gh-token", Reason: "r"})
		if connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
			t.Fatalf("%v: got %v (%v)", tc.err, err, api.ReasonOf(err))
		}
		var ce *connect.Error
		if errors.As(err, &ce) && bytes.Contains([]byte(ce.Message()), []byte("/secret/path")) {
			t.Fatalf("internal detail leaked: %v", err)
		}
	}
	f := setupSecrets(t, &fakeSecrets{})
	_, err := f.secrets.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{RequestId: "req-0002-abcd", Name: "gh-token", ExePath: "/bin/x"})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_EXECUTABLE_NOT_FOUND {
		t.Fatalf("grant: %v", err)
	}
}

func TestRedeemChecksThePeerBeforeTheRequestLog(t *testing.T) {
	fake := &fakeSecrets{trustErr: api.ErrPeerNotTrusted}
	f := setupSecrets(t, fake)
	if _, err := f.secrets.ListSecrets(t.Context(), &berthv1.ListSecretsRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.secrets.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{RequestId: "req-used-0001", Name: "gh-token", ExePath: "/bin/x"}); err != nil {
		t.Fatal(err)
	}
	// The id is taken by AuthorizeUse, so reading the log first would answer REQUEST_ID_REUSED.
	_, err := f.secrets.RedeemUseTicket(t.Context(), &berthv1.RedeemUseTicketRequest{RequestId: "req-used-0001", Ticket: make([]byte, 32)})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED || fake.redeems != 0 {
		t.Fatalf("got %v, redeems %d", err, fake.redeems)
	}
	fake.trustErr = nil
	resp, err := f.secrets.RedeemUseTicket(t.Context(), &berthv1.RedeemUseTicketRequest{RequestId: "req-redeem-01", Ticket: make([]byte, 32)})
	if err != nil || resp.GetName() != "gh-token" {
		t.Fatalf("trusted redeem %v, %v", resp, err)
	}
}

func TestAuthorizeUseNeverRecordsTheTicket(t *testing.T) {
	fake := &fakeSecrets{}
	f := setupSecrets(t, fake)
	req := &berthv1.AuthorizeUseRequest{RequestId: "req-auth-0001", Name: "gh-token", ExePath: "/bin/x"}
	first, err := f.secrets.AuthorizeUse(t.Context(), req)
	if err != nil || len(first.GetTicket()) != 32 {
		t.Fatalf("first %v, %v", first, err)
	}
	_, stored, found, err := f.store.Recall(t.Context(), req.GetRequestId())
	if err != nil || !found || bytes.Contains(stored, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("request log holds the ticket: found %v, %v", found, err)
	}
	replay, err := f.secrets.AuthorizeUse(t.Context(), req)
	if err != nil || fake.authorize != 1 || replay.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED || len(replay.GetTicket()) != 0 {
		t.Fatalf("replay %v, %v, authorize ran %d times", replay, err, fake.authorize)
	}
}

func TestOpenStoreDialogDoesNotBlockOtherCalls(t *testing.T) {
	fake := &fakeSecrets{block: make(chan struct{})}
	f := setupSecrets(t, fake)
	done := make(chan error, 1)
	go func() {
		_, err := f.secrets.RequestSecret(context.Background(), &berthv1.RequestSecretRequest{RequestId: "req-slow-0001", Name: "gh-token", Reason: "r"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := f.daemon.Shutdown(ctx, &berthv1.ShutdownRequest{RequestId: "req-stop-0001"}); err != nil {
		t.Fatalf("shutdown waited on the open dialog: %v", err)
	}
	close(fake.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
