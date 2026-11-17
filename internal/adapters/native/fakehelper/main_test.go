package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// berthd answers RedeemUseTicket on a temporary BERTH_HOME socket; it checks the request is
// the Connect unary JSON call the protocol fixes.
func fakeBerthd(t *testing.T, wantTicket []byte, name string) (home string, calls *atomic.Int32) {
	t.Helper()
	home, err := os.MkdirTemp("", "bfh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(home, "berthd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	calls = new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /berth.v1.SecretsService/RedeemUseTicket", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "content type", http.StatusUnsupportedMediaType)
			return
		}
		var req struct {
			RequestID string `json:"requestId"`
			Ticket    string `json:"ticket"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.RequestID) < 8 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		got, err := base64.StdEncoding.DecodeString(req.Ticket)
		if err != nil || !bytes.Equal(got, wantTicket) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"code":"failed_precondition"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return home, calls
}

func ticketLine(b []byte) *strings.Reader { return strings.NewReader(hex.EncodeToString(b) + "\n") }

func TestReadRedeemsTicketAndPrintsValue(t *testing.T) {
	ticket := bytes.Repeat([]byte{7}, 32)
	home, calls := fakeBerthd(t, ticket, "gh-token")
	t.Setenv("BERTH_HOME", home)
	values := filepath.Join(home, "fake-native", "values")
	if err := os.MkdirAll(values, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := []byte("canary-3f9a")
	if err := os.WriteFile(filepath.Join(values, "gh-token"), canary, 0o600); err != nil {
		t.Fatal(err)
	}
	var value, stdout, stderr bytes.Buffer
	useRecipient(t, &value, nil)
	if code := run([]string{"read"}, ticketLine(ticket), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !bytes.Equal(value.Bytes(), canary) || stdout.Len() != 0 {
		t.Fatalf("the value went somewhere other than the value channel")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("berthd saw %d calls, want 1", n)
	}
	log, err := os.ReadFile(filepath.Join(home, "fake-native", "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, canary) || !bytes.Contains(log, []byte(`"redeemed":true`)) {
		t.Fatalf("request log is wrong: %s", log)
	}
}

func TestReadRefusedTicketPrintsNothing(t *testing.T) {
	home, _ := fakeBerthd(t, bytes.Repeat([]byte{7}, 32), "gh-token")
	t.Setenv("BERTH_HOME", home)
	var value, stdout, stderr bytes.Buffer
	useRecipient(t, &value, nil)
	if code := run([]string{"read"}, ticketLine(bytes.Repeat([]byte{8}, 32)), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit %d, want %d", code, exitRefused)
	}
	if value.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("output on refusal")
	}
}

func TestStoreRefusesExistingItem(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BERTH_FAKE_NATIVE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"answer":"approve","value":"Y2FuYXJ5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := `{"name":"n","reason":"r","requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}}` + "\n"
	for i, want := range []string{`{"outcome":"stored"}`, `{"error":"item exists","outcome":"failed"}`} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"store"}, strings.NewReader(req), &stdout, &stderr); code != 0 {
			t.Fatalf("store %d exit %d", i, code)
		}
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Fatalf("store %d: %s, want %s", i, got, want)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "values", "n"))
	if err != nil || string(got) != "canary" {
		t.Fatalf("stored value wrong: %v", err)
	}
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

// useRecipient stands in for the value channel check, so these tests reach redeem.
func useRecipient(t *testing.T, buf *bytes.Buffer, err error) {
	t.Helper()
	old := recipient
	recipient = func(context.Context) (io.WriteCloser, error) {
		if err != nil {
			return nil, err
		}
		return nopCloser{buf}, nil
	}
	t.Cleanup(func() { recipient = old })
}

func TestReadChecksTheReceiverBeforeRedeeming(t *testing.T) {
	ticket := bytes.Repeat([]byte{7}, 32)
	home, calls := fakeBerthd(t, ticket, "gh-token")
	t.Setenv("BERTH_HOME", home)
	var stdout, stderr bytes.Buffer
	useRecipient(t, nil, errors.New("not berth"))
	if code := run([]string{"read"}, ticketLine(ticket), &stdout, &stderr); code != exitRefused || calls.Load() != 0 {
		t.Fatalf("exit %d, redeem calls %d", code, calls.Load())
	}
}

func TestRecipientMustBeTheParent(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs := os.NewFile(uintptr(fds[0]), "theirs")
	defer func() { _ = theirs.Close() }()
	f := os.NewFile(uintptr(fds[1]), "mine")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// This process is the peer, so naming another parent must fail on the pid alone.
	err = checkRecipient(t.Context(), conn.(*net.UnixConn), os.Getpid()+1)
	if err == nil || !strings.Contains(err.Error(), "is not the parent") {
		t.Fatalf("want a parent refusal, got %v", err)
	}
}
