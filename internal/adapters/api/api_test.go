package api_test

import (
	"context"
	"path/filepath"
	"slices"
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
	api.Register(server, api.Deps{
		Version: "test", PID: 1, StartedAt: time.Now(), Socket: "/tmp/x.sock",
		Store: store, Requests: store, Clock: testutil.NewClock(),
		RequestStop: func(context.Context) error { f.stops++; return nil },
	})
	client := connect.NewClient(connectinprocess.New(server))
	f.daemon = berthv1connect.NewDaemonServiceClient(client)
	f.debug = berthv1connect.NewDebugServiceClient(client)
	return f
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
	protoregistry.GlobalFiles.RangeFilesByPackage("berth.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			svc := fd.Services().Get(i)
			for j := range svc.Methods().Len() {
				m := svc.Methods().Get(j)
				if proto.GetExtension(m.Options(), berthv1.E_Agent).(bool) {
					agent = append(agent, string(m.FullName()))
				}
			}
		}
		return true
	})
	slices.Sort(agent)
	if want := []string{"berth.v1.DaemonService.Handshake"}; !slices.Equal(agent, want) {
		t.Fatalf("agent methods = %v, want %v", agent, want)
	}
}
