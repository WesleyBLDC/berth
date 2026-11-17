package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/ports"
)

const sessionID = "0123456789abcdef0123456789abcdef"

func (f *fakeSecrets) GrantProxy(_ context.Context, req *berthv1.GrantProxyRequest, _ ports.Peer) (*berthv1.GrantProxyResponse, error) {
	if f.lifecycleErr != nil {
		return nil, f.lifecycleErr
	}
	f.grantTarget = req.GetTarget()
	return &berthv1.GrantProxyResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED}, nil
}

func (f *fakeSecrets) RevokeGrant(context.Context, *berthv1.RevokeGrantRequest, ports.Peer) (*berthv1.RevokeGrantResponse, error) {
	if f.lifecycleErr != nil {
		return nil, f.lifecycleErr
	}
	return &berthv1.RevokeGrantResponse{State: berthv1.GrantState_GRANT_STATE_REVOKED}, nil
}

func (f *fakeSecrets) RemoveSecret(context.Context, *berthv1.RemoveSecretRequest, ports.Peer) (*berthv1.RemoveSecretResponse, error) {
	f.removes++
	if f.lifecycleErr != nil {
		return nil, f.lifecycleErr
	}
	return &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED}, nil
}

func (f *fakeSecrets) RotateSecret(context.Context, *berthv1.RotateSecretRequest, ports.Peer) (*berthv1.RotateSecretResponse, error) {
	if f.lifecycleErr != nil {
		return nil, f.lifecycleErr
	}
	return &berthv1.RotateSecretResponse{Outcome: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, nil
}

func (f *fakeSecrets) TrustProxy(context.Context, ports.Peer) error { return f.proxyTrustErr }

func (f *fakeSecrets) AuthorizeProxy(context.Context, *berthv1.AuthorizeProxyRequest, ports.Peer) (*berthv1.AuthorizeProxyResponse, error) {
	f.proxyAuthorize++
	return &berthv1.AuthorizeProxyResponse{
		Outcome:   berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED,
		SessionId: sessionID,
		Tickets:   []*berthv1.ProxyTicket{{Name: "gh-token", Ticket: bytes.Repeat([]byte{9}, 32)}},
	}, nil
}

func (f *fakeSecrets) WatchProxySession(_ context.Context, _ string, _ ports.Peer, send func(*berthv1.ProxySessionEvent) error) error {
	if err := send(&berthv1.ProxySessionEvent{Kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING}); err != nil {
		return err
	}
	return send(&berthv1.ProxySessionEvent{
		Kind:       berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP,
		StopReason: berthv1.ProxyStopReason_PROXY_STOP_REASON_GRANT_REVOKED,
	})
}

func (f *fakeSecrets) EndProxySession(context.Context, *berthv1.EndProxySessionRequest, ports.Peer) (*berthv1.EndProxySessionResponse, error) {
	f.ends++
	return &berthv1.EndProxySessionResponse{}, nil
}

func (f *fakeSecrets) StartProxy(ctx context.Context, _ *berthv1.StartProxyRequest, send func(*berthv1.StartProxyResponse) error) error {
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	if err := send(&berthv1.StartProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, SessionId: sessionID, Port: 4123}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func target() *berthv1.ProxyTarget {
	return &berthv1.ProxyTarget{Host: "api.example.com", HeaderName: "Authorization", ValuePrefix: "Bearer ", Methods: []string{"GET", "POST"}, PlaceholderEnv: "OPENAI_API_KEY"}
}

func TestLifecycleErrorsAreTyped(t *testing.T) {
	cases := []struct {
		err    error
		code   connect.Code
		reason berthv1.ErrorReason
	}{
		{api.ErrBusy, connect.CodeFailedPrecondition, berthv1.ErrorReason_ERROR_REASON_BUSY},
		{api.ErrBadProxyTarget, connect.CodeInvalidArgument, berthv1.ErrorReason_ERROR_REASON_BAD_PROXY_TARGET},
		{api.ErrGrantNotFound, connect.CodeNotFound, berthv1.ErrorReason_ERROR_REASON_GRANT_NOT_FOUND},
		{connect.NewError(connect.CodeUnimplemented, "detail from deep inside"), connect.CodeUnimplemented, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
	}
	for i, tc := range cases {
		f := setupSecrets(t, &fakeSecrets{lifecycleErr: tc.err})
		id := "req-life-000" + string(rune('a'+i))
		calls := map[string]error{}
		_, calls["GrantProxy"] = f.secrets.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{RequestId: id + "g", Name: "gh-token", Target: target()})
		_, calls["RevokeGrant"] = f.secrets.RevokeGrant(t.Context(), &berthv1.RevokeGrantRequest{RequestId: id + "v", GrantId: 3})
		_, calls["RemoveSecret"] = f.secrets.RemoveSecret(t.Context(), &berthv1.RemoveSecretRequest{RequestId: id + "m", Name: "gh-token"})
		_, calls["RotateSecret"] = f.secrets.RotateSecret(t.Context(), &berthv1.RotateSecretRequest{RequestId: id + "r", Name: "gh-token"})
		for method, err := range calls {
			if connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
				t.Errorf("%s with %v: got %v (%v)", method, tc.err, err, api.ReasonOf(err))
			}
			var ce *connect.Error
			if connect.CodeOf(err) == connect.CodeUnimplemented && errors.As(err, &ce) && bytes.Contains([]byte(ce.Message()), []byte("deep inside")) {
				t.Errorf("%s leaked detail: %v", method, err)
			}
		}
	}
}

func TestGrantProxyValidatesTheTarget(t *testing.T) {
	bad := map[string]func(*berthv1.ProxyTarget){
		"uppercase host":          func(p *berthv1.ProxyTarget) { p.Host = "API.example.com" },
		"host with a port":        func(p *berthv1.ProxyTarget) { p.Host = "api.example.com:443" },
		"port above 65535":        func(p *berthv1.ProxyTarget) { p.Port = 65536 },
		"header with a space":     func(p *berthv1.ProxyTarget) { p.HeaderName = "X Key" },
		"Host header":             func(p *berthv1.ProxyTarget) { p.HeaderName = "Host" },
		"transfer-encoding":       func(p *berthv1.ProxyTarget) { p.HeaderName = "Transfer-Encoding" },
		"proxy header":            func(p *berthv1.ProxyTarget) { p.HeaderName = "Proxy-Authorization" },
		"CR LF in the prefix":     func(p *berthv1.ProxyTarget) { p.ValuePrefix = "Bearer\r\nX-Evil: 1\r\n" },
		"NUL in the suffix":       func(p *berthv1.ProxyTarget) { p.ValueSuffix = "\x00" },
		"long prefix":             func(p *berthv1.ProxyTarget) { p.ValuePrefix = string(bytes.Repeat([]byte("a"), 129)) },
		"space in the path glob":  func(p *berthv1.ProxyTarget) { p.PathGlob = "/v1/ x" },
		"lowercase method":        func(p *berthv1.ProxyTarget) { p.Methods = []string{"get"} },
		"repeated method":         func(p *berthv1.ProxyTarget) { p.Methods = []string{"GET", "GET"} },
		"placeholder lowercase":   func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "openai_key" },
		"placeholder with digits": func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "1KEY" },
	}
	fake := &fakeSecrets{}
	f := setupSecrets(t, fake)
	for name, edit := range bad {
		p := target()
		edit(p)
		_, err := f.secrets.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{RequestId: "req-bad-" + name, Name: "gh-token", Target: p})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", name, err)
		}
	}
	if _, err := f.secrets.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{RequestId: "req-no-target", Name: "gh-token"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing target: %v", err)
	}
	if fake.grantTarget != nil {
		t.Fatal("an invalid target reached the service")
	}
	resp, err := f.secrets.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{RequestId: "req-good-0001", Name: "gh-token", Target: target()})
	if err != nil || resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED {
		t.Fatalf("valid target: %v %v", resp, err)
	}
	if fake.grantTarget.GetPort() != 443 {
		t.Fatalf("port 0 reached the service as %d, want 443", fake.grantTarget.GetPort())
	}
	p := target()
	p.Port = 8443
	if _, err := f.secrets.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{RequestId: "req-good-0002", Name: "gh-token", Target: p}); err != nil || fake.grantTarget.GetPort() != 8443 {
		t.Fatalf("explicit port: %v, %d", err, fake.grantTarget.GetPort())
	}
}

func TestRemoveSecretRunsOncePerRequestID(t *testing.T) {
	fake := &fakeSecrets{}
	f := setupSecrets(t, fake)
	for range 2 {
		resp, err := f.secrets.RemoveSecret(t.Context(), &berthv1.RemoveSecretRequest{RequestId: "req-rm-000001", Name: "gh-token"})
		if err != nil || resp.GetOutcome() != berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED {
			t.Fatalf("remove: %v %v", resp, err)
		}
	}
	if fake.removes != 1 {
		t.Fatalf("removes = %d, want 1", fake.removes)
	}
}

func TestProxyMethodsCheckThePeerBeforeTheRequestLog(t *testing.T) {
	fake := &fakeSecrets{proxyTrustErr: api.ErrPeerNotTrusted}
	f := setupSecrets(t, fake)
	if _, err := f.secrets.RemoveSecret(t.Context(), &berthv1.RemoveSecretRequest{RequestId: "req-taken-001", Name: "gh-token"}); err != nil {
		t.Fatal(err)
	}
	// The id is taken by RemoveSecret, so reading the log first would answer REQUEST_ID_REUSED.
	_, err := f.secrets.AuthorizeProxy(t.Context(), &berthv1.AuthorizeProxyRequest{RequestId: "req-taken-001", Names: []string{"gh-token"}})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Errorf("AuthorizeProxy: %v", err)
	}
	_, err = f.secrets.EndProxySession(t.Context(), &berthv1.EndProxySessionRequest{RequestId: "req-taken-001", SessionId: sessionID})
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Errorf("EndProxySession: %v", err)
	}
	stream, err := f.secrets.WatchProxySession(t.Context(), &berthv1.WatchProxySessionRequest{SessionId: sessionID})
	if err == nil {
		_, err = stream.Receive()
	}
	if api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED {
		t.Errorf("WatchProxySession: %v", err)
	}
	if fake.proxyAuthorize != 0 || fake.ends != 0 {
		t.Fatalf("an untrusted peer reached the service: authorize %d, end %d", fake.proxyAuthorize, fake.ends)
	}
}

func TestAuthorizeProxyNeverRecordsATicket(t *testing.T) {
	fake := &fakeSecrets{}
	f := setupSecrets(t, fake)
	req := &berthv1.AuthorizeProxyRequest{RequestId: "req-proxy-001", Names: []string{"gh-token"}}
	first, err := f.secrets.AuthorizeProxy(t.Context(), req)
	if err != nil || len(first.GetTickets()) != 1 || len(first.GetTickets()[0].GetTicket()) != 32 {
		t.Fatalf("first: %v %v", first, err)
	}
	again, err := f.secrets.AuthorizeProxy(t.Context(), req)
	if err != nil || fake.proxyAuthorize != 1 || again.GetSessionId() != sessionID {
		t.Fatalf("retry: %v %v, calls %d", again, err, fake.proxyAuthorize)
	}
	if len(again.GetTickets()) != 1 || len(again.GetTickets()[0].GetTicket()) != 0 {
		t.Fatalf("retry returned a recorded ticket: %v", again.GetTickets())
	}
}

func TestWatchProxySessionStreamsEvents(t *testing.T) {
	f := setupSecrets(t, &fakeSecrets{})
	stream, err := f.secrets.WatchProxySession(t.Context(), &berthv1.WatchProxySessionRequest{SessionId: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var kinds []berthv1.ProxySessionEventKind
	for {
		resp, err := stream.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v", err)
			}
			break
		}
		kinds = append(kinds, resp.GetEvent().GetKind())
	}
	if len(kinds) != 2 || kinds[1] != berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP {
		t.Fatalf("events = %v", kinds)
	}
	bad, err := f.secrets.WatchProxySession(t.Context(), &berthv1.WatchProxySessionRequest{SessionId: "not-hex"})
	if err == nil {
		_, err = bad.Receive()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed session id: %v", err)
	}
}

func TestStartProxyStreamsAndTypesItsErrors(t *testing.T) {
	fake := &fakeSecrets{}
	f := setupSecrets(t, fake)
	if _, err := receiveFirst(t, f, &berthv1.StartProxyRequest{RequestId: "req-start-01"}); connect.CodeOf(err) != connect.CodeInvalidArgument || fake.starts != 0 {
		t.Fatalf("no names: %v, %d starts", err, fake.starts)
	}
	resp, err := receiveFirst(t, f, &berthv1.StartProxyRequest{RequestId: "req-start-02", Names: []string{"gh-token"}})
	if err != nil || resp.GetSessionId() != sessionID || resp.GetPort() != 4123 {
		t.Fatalf("first message %v %v", resp, err)
	}
	for _, tc := range []struct {
		err    error
		code   connect.Code
		reason berthv1.ErrorReason
	}{
		{api.ErrRateLimited, connect.CodeResourceExhausted, berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED},
		{api.ErrProxyNotStarted, connect.CodeUnavailable, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
	} {
		fake.startErr = tc.err
		_, err := receiveFirst(t, f, &berthv1.StartProxyRequest{RequestId: "req-start-" + tc.code.String(), Names: []string{"gh-token"}})
		if connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
			t.Errorf("%v: got %v (%v)", tc.err, err, api.ReasonOf(err))
		}
	}
}

// receiveFirst opens a StartProxy stream, takes its first message and closes it.
func receiveFirst(t *testing.T, f secretsFixture, req *berthv1.StartProxyRequest) (*berthv1.StartProxyResponse, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := f.secrets.StartProxy(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	return stream.Receive()
}
