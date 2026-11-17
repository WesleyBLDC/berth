package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sys/unix"

	spikev1 "berth/spike/go/gen/spike/v1"
	"berth/spike/go/gen/spike/v1/spikev1connect"
)

type server struct {
	screen *screencaster
}

func (s *server) Count(ctx context.Context, req *spikev1.CountRequest, stream spikev1connect.SpikeServiceCountServerStream) error {
	payload := make([]byte, 256)
	for i := int32(1); i <= req.GetCount(); i++ {
		if err := stream.Send(&spikev1.CountResponse{Seq: i, Payload: payload}); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) SendInput(ctx context.Context, stream spikev1connect.SpikeServiceSendInputServerStream) (*spikev1.SendInputResponse, error) {
	var received int32
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return &spikev1.SendInputResponse{Received: received}, nil
		}
		if err != nil {
			return nil, err
		}
		received++
		if msg.GetSeq() != received {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("out of order: got %d want %d", msg.GetSeq(), received))
		}
	}
}

func (s *server) WatchScreen(ctx context.Context, req *spikev1.WatchScreenRequest, stream spikev1connect.SpikeServiceWatchScreenServerStream) error {
	if s.screen == nil {
		return connect.NewError(connect.CodeUnavailable, "start the server with -chrome to stream frames")
	}
	return s.screen.watch(ctx, stream)
}

// peerCheck refuses connections from any other user; socket file mode alone is not enough.
type peerCheckListener struct {
	net.Listener
	uid int
}

func (l peerCheckListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			continue
		}
		raw, err := uc.SyscallConn()
		if err != nil {
			conn.Close()
			continue
		}
		var cred *unix.Xucred
		var credErr error
		if err := raw.Control(func(fd uintptr) {
			cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		}); err != nil || credErr != nil || int(cred.Uid) != l.uid {
			slog.Warn("refused peer", "err", errors.Join(err, credErr))
			conn.Close()
			continue
		}
		return conn, nil
	}
}

func main() {
	sock := flag.String("socket", filepath.Join(os.Getenv("HOME"), "Library/Application Support/Berth/spike.sock"), "socket path")
	chrome := flag.Bool("chrome", false, "launch headless Chrome and serve its screencast")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*sock), 0o700); err != nil {
		slog.Error("mkdir", "err", err)
		os.Exit(1)
	}
	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	if err := os.Chmod(*sock, 0o600); err != nil {
		slog.Error("chmod", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &server{}
	if *chrome {
		sc, err := startScreencaster(ctx)
		if err != nil {
			slog.Error("chrome", "err", err)
			os.Exit(1)
		}
		defer sc.close()
		srv.screen = sc
	}

	cs := connect.NewServer()
	spikev1connect.RegisterSpikeServiceHandler(cs, srv)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, cs, connecthttp.WithGRPC(), connecthttp.WithProtoJSON())
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: mux, Protocols: protocols}
	go func() {
		<-ctx.Done()
		slog.Info("stopping")
		_ = hs.Close()
	}()
	slog.Info("serving", "socket", *sock)
	if err := hs.Serve(peerCheckListener{Listener: ln, uid: os.Getuid()}); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "err", err)
	}
}
