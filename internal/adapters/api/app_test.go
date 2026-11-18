package api_test

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/ports"
)

// TestAppSurface pins the methods only the signed Berth.app may call. None is marked until the
// screen and control methods land; listing and watching stay open to the owner socket, and no
// method may be both an agent method and an app method.
func TestAppSurface(t *testing.T) {
	var app []string
	eachMethod(func(m protoreflect.MethodDescriptor) {
		if !proto.GetExtension(m.Options(), berthv1.E_App).(bool) {
			return
		}
		app = append(app, string(m.FullName()))
		if proto.GetExtension(m.Options(), berthv1.E_Agent).(bool) {
			t.Errorf("%s is marked both agent and app", m.FullName())
		}
	})
	slices.Sort(app)
	if want := []string(nil); !slices.Equal(app, want) {
		t.Fatalf("app methods = %v, want %v", app, want)
	}
}

// markedMethod builds a method descriptor that carries the app mark, since no berth.v1 method
// does yet.
func markedMethod(t *testing.T) protoreflect.MethodDescriptor {
	t.Helper()
	opts := &descriptorpb.MethodOptions{}
	proto.SetExtension(opts, berthv1.E_App, true)
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("apptest.proto"), Package: proto.String("apptest"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/protobuf/empty.proto"},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Screen"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name: proto.String("Watch"), InputType: proto.String(".google.protobuf.Empty"),
				OutputType: proto.String(".google.protobuf.Empty"), Options: opts,
			}},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Services().Get(0).Methods().Get(0)
}

func TestAppOnlyRefusesEveryoneButTheApp(t *testing.T) {
	app := ports.Peer{PID: 77}
	var asked []ports.Peer
	isApp := func(_ context.Context, p ports.Peer) bool {
		asked = append(asked, p)
		return p == app
	}
	ran := 0
	gate := api.AppOnly(isApp)(func(context.Context, connect.Spec, connect.ServerStream) error { ran++; return nil })
	marked := connect.Spec{Schema: markedMethod(t), Procedure: "/apptest.Screen/Watch"}

	err := gate(api.WithPeer(t.Context(), ports.Peer{PID: 78}), marked, nil)
	if connect.CodeOf(err) != connect.CodePermissionDenied || api.ReasonOf(err) != berthv1.ErrorReason_ERROR_REASON_NOT_BERTH_APP || ran != 0 {
		t.Fatalf("another peer: %v (%v), handler ran %d times", err, api.ReasonOf(err), ran)
	}
	if err := gate(api.WithPeer(t.Context(), app), marked, nil); err != nil || ran != 1 {
		t.Fatalf("the app: %v, handler ran %d times", err, ran)
	}

	// Every berth.v1 method today is unmarked, so it is served without asking who calls.
	asked = nil
	eachMethod(func(m protoreflect.MethodDescriptor) {
		spec := connect.Spec{Schema: m, Procedure: "/" + string(m.Parent().FullName()) + "/" + string(m.Name())}
		if err := gate(api.WithPeer(t.Context(), ports.Peer{PID: 78}), spec, nil); err != nil {
			t.Errorf("%s refused: %v", m.FullName(), err)
		}
	})
	if len(asked) != 0 {
		t.Fatalf("an unmarked method checked the caller %d times", len(asked))
	}
}
