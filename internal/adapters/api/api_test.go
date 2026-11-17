package api_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/testutil"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	berthv1 "berth/gen/berth/v1"
)

type fixture struct {
	daemon berthv1connect.DaemonServiceClient
	debug  berthv1connect.DebugServiceClient
	stops  int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &fixture{}
	server := connect.NewServer()
	api.Register(api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { f.stops++; return nil },
	}, server)
	client := connect.NewClient(connectinprocess.New(server))
	f.daemon = berthv1connect.NewDaemonServiceClient(client)
	f.debug = berthv1connect.NewDebugServiceClient(client)
	return f
}

// TestMappersAgree: berthd not recording an outcome, a deadline and a cancellation answer the same
// in every service, so a client reads them once.
func TestMappersAgree(t *testing.T) {
	for name, mapErr := range api.ErrorMappers {
		for _, tc := range []struct {
			err    error
			code   connect.Code
			reason berthv1.ErrorReason
		}{
			{fmt.Errorf("step: %w", api.ErrUnavailable), connect.CodeUnavailable, berthv1.ErrorReason_ERROR_REASON_NOT_SERVING},
			{fmt.Errorf("prompt: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
			{context.Canceled, connect.CodeCanceled, berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
		} {
			if err := mapErr(tc.err); connect.CodeOf(err) != tc.code || api.ReasonOf(err) != tc.reason {
				t.Errorf("%s(%v) = %v (%v), want %v (%v)", name, tc.err, err, api.ReasonOf(err), tc.code, tc.reason)
			}
		}
		if err := mapErr(nil); err != nil {
			t.Errorf("%s(nil) = %v", name, err)
		}
	}
}

func TestGetStateOfAnUnknownEntityIsTyped(t *testing.T) {
	f := setup(t)
	_, err := f.debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: "secret/none"})
	if connect.CodeOf(err) != connect.CodeNotFound || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_ENTITY_NOT_FOUND {
		t.Fatalf("GetState of an unknown entity: %v (%v)", err, api.ReasonOf(err))
	}
}

func TestProtocolMismatchIsATypedError(t *testing.T) {
	f := setup(t)
	if _, err := f.daemon.Handshake(context.Background(), &berthv1.HandshakeRequest{ProtocolVersion: api.ProtocolVersion}); err != nil {
		t.Fatalf("matching version: %v", err)
	}
	_, err := f.daemon.Handshake(context.Background(), &berthv1.HandshakeRequest{ProtocolVersion: 99})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH {
		t.Fatalf("got %v (%v)", err, api.ReasonOf(err))
	}
}

func TestRepeatedRequestIDRunsOnce(t *testing.T) {
	f := setup(t)
	req := &berthv1.ShutdownRequest{RequestId: "req-0001-abcd"}
	first, err := f.daemon.Shutdown(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.daemon.Shutdown(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.stops != 1 {
		t.Fatalf("stop ran %d times, want 1", f.stops)
	}
	if !proto.Equal(first, second) {
		t.Fatalf("retry returned a different response: %v vs %v", first, second)
	}
}

func TestInvalidRequestIsRejectedBeforeTheCore(t *testing.T) {
	f := setup(t)
	_, err := f.daemon.Shutdown(context.Background(), &berthv1.ShutdownRequest{RequestId: "short"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || f.stops != 0 {
		t.Fatalf("got %v, stops %d", err, f.stops)
	}
}

// TestAgentSurface pins the methods agents may call. Adding one must be a deliberate change here.
func TestAgentSurface(t *testing.T) {
	var agent []string
	eachMethod(func(m protoreflect.MethodDescriptor) {
		if proto.GetExtension(m.Options(), berthv1.E_Agent).(bool) {
			agent = append(agent, string(m.FullName()))
		}
	})
	slices.Sort(agent)
	want := []string{
		"berth.v1.BrowsersService.CloseAgentBrowser",
		"berth.v1.BrowsersService.ListAgentBrowsers",
		"berth.v1.BrowsersService.OpenAgentBrowser",
		"berth.v1.DaemonService.Handshake",
		"berth.v1.SecretsService.AuthorizeProxy",
		"berth.v1.SecretsService.AuthorizeUse",
		"berth.v1.SecretsService.EndProxySession",
		"berth.v1.SecretsService.ListSecrets",
		"berth.v1.SecretsService.RequestSecret",
		"berth.v1.SecretsService.StartProxy",
		"berth.v1.SecretsService.WatchProxySession",
	}
	if !slices.Equal(agent, want) {
		t.Fatalf("agent methods = %v, want %v", agent, want)
	}

	// The agent socket's server must serve exactly that list and refuse the rest.
	var served []string
	gate := api.AgentOnly()(func(context.Context, connect.Spec, connect.ServerStream) error { return nil })
	eachMethod(func(m protoreflect.MethodDescriptor) {
		spec := connect.Spec{Schema: m, Procedure: "/" + string(m.Parent().FullName()) + "/" + string(m.Name())}
		err := gate(t.Context(), spec, nil)
		switch {
		case err == nil:
			served = append(served, string(m.FullName()))
		case connect.CodeOf(err) != connect.CodePermissionDenied || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_NOT_AGENT_METHOD:
			t.Errorf("%s refused with %v (%v), want PermissionDenied and NOT_AGENT_METHOD", m.FullName(), err, api.ReasonOf(err))
		}
	})
	slices.Sort(served)
	if !slices.Equal(served, want) {
		t.Fatalf("agent socket serves %v, want %v", served, want)
	}
	if err := gate(t.Context(), connect.Spec{Procedure: "/berth.v1.Unknown/Method"}, nil); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a procedure without a schema got %v, want PermissionDenied", err)
	}
}

func eachMethod(f func(m protoreflect.MethodDescriptor)) {
	protoregistry.GlobalFiles.RangeFilesByPackage("berth.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			svc := fd.Services().Get(i)
			for j := range svc.Methods().Len() {
				f(svc.Methods().Get(j))
			}
		}
		return true
	})
}

// TestNoFieldCarriesAValue pins the only places that could hold secret material: bytes fields
// (tickets and a pinned cdhash, which are not values), fields named for a value (the proxy
// header's fixed text, and a placeholder's stand in through its map entry) and fields named for
// a token (the agent session token, returned once).
func TestNoFieldCarriesAValue(t *testing.T) {
	allowed := []string{
		"berth.v1.AuthorizeUseResponse.cdhash",
		"berth.v1.AuthorizeUseResponse.ticket",
		"berth.v1.CreateAgentSessionResponse.token",
		"berth.v1.LaunchBrowserResponse.cdp_token",
		"berth.v1.OpenAgentBrowserResponse.cdp_token",
		"berth.v1.ProxyTarget.value_prefix",
		"berth.v1.ProxyTarget.value_suffix",
		"berth.v1.ProxyTicket.ticket",
		"berth.v1.RedeemUseTicketRequest.ticket",
		"berth.v1.StartProxyResponse.PlaceholdersEntry.value",
	}
	var found []string
	var walk func(md protoreflect.MessageDescriptor)
	seen := map[protoreflect.FullName]bool{}
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] || md.ParentFile().Package() != "berth.v1" {
			return
		}
		seen[md.FullName()] = true
		for i := range md.Fields().Len() {
			fd := md.Fields().Get(i)
			name := string(fd.Name())
			if fd.Kind() == protoreflect.BytesKind || strings.Contains(name, "value") || strings.Contains(name, "password") || strings.Contains(name, "plaintext") || strings.Contains(name, "token") {
				found = append(found, string(fd.FullName()))
			}
			if fd.Message() != nil {
				walk(fd.Message())
			}
		}
	}
	eachMethod(func(m protoreflect.MethodDescriptor) {
		walk(m.Input())
		walk(m.Output())
	})
	slices.Sort(found)
	if !slices.Equal(found, allowed) {
		t.Fatalf("fields that could carry a value = %v, want %v", found, allowed)
	}
}

// TestMutatingMethodsTakeARequestID keeps every method that changes something idempotent.
func TestMutatingMethodsTakeARequestID(t *testing.T) {
	readOnly := []string{"Handshake", "Status", "GetState", "ListSecrets", "WatchProxySession", "ListSessions", "InspectSession", "WatchSessions", "ListAgentSessions", "ListBrowsers", "ListAgentBrowsers"}
	eachMethod(func(m protoreflect.MethodDescriptor) {
		if slices.Contains(readOnly, string(m.Name())) {
			return
		}
		fd := m.Input().Fields().ByName("request_id")
		if fd == nil || fd.Number() != 1 || fd.Kind() != protoreflect.StringKind {
			t.Errorf("%s has no string request_id = 1", m.FullName())
		}
	})
}
