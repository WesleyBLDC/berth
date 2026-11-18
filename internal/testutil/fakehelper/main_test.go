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

// berthd answers RedeemUseTicket on a temporary BERTH_HOME socket with answer; it checks the
// request is the Connect unary JSON call the protocol fixes.
func fakeBerthd(t *testing.T, wantTicket []byte, answer map[string]string) (home string, calls *atomic.Int32) {
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
		_ = json.NewEncoder(w).Encode(answer)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return home, calls
}

func ticketLine(b []byte) *strings.Reader { return strings.NewReader(hex.EncodeToString(b) + "\n") }

func useTicket(name, kind string) map[string]string {
	return map[string]string{"name": name, "consumerKind": kind, "action": actionUse}
}

func TestReadRedeemsTicketAndPrintsValue(t *testing.T) {
	ticket := bytes.Repeat([]byte{7}, 32)
	home, calls := fakeBerthd(t, ticket, useTicket("gh-token", kindExecutable))
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
	useRecipient(t, &value, kindExecutable, nil)
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
	home, _ := fakeBerthd(t, bytes.Repeat([]byte{7}, 32), useTicket("gh-token", kindExecutable))
	t.Setenv("BERTH_HOME", home)
	var value, stdout, stderr bytes.Buffer
	useRecipient(t, &value, kindExecutable, nil)
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

// useRecipient stands in for the value channel check, so these tests reach redeem with a
// parent of kind.
func useRecipient(t *testing.T, buf *bytes.Buffer, kind string, err error) {
	t.Helper()
	old := recipient
	recipient = func(context.Context, string) (io.WriteCloser, string, error) {
		if err != nil {
			return nil, "", err
		}
		return nopCloser{buf}, kind, nil
	}
	t.Cleanup(func() { recipient = old })
}

func TestReadChecksTheReceiverBeforeRedeeming(t *testing.T) {
	ticket := bytes.Repeat([]byte{7}, 32)
	home, calls := fakeBerthd(t, ticket, useTicket("gh-token", kindExecutable))
	t.Setenv("BERTH_HOME", home)
	var stdout, stderr bytes.Buffer
	useRecipient(t, nil, "", errors.New("not berth"))
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
	for _, trust := range []string{"", "cli", "proxy"} {
		_, err = checkRecipient(t.Context(), conn.(*net.UnixConn), os.Getpid()+1, trust)
		if err == nil || !strings.Contains(err.Error(), "is not the parent") {
			t.Fatalf("trust %q: want a parent refusal, got %v", trust, err)
		}
	}
	// The test binary is the parent here but neither signed berth nor berth-proxy.
	if _, err := checkRecipient(t.Context(), conn.(*net.UnixConn), os.Getpid(), ""); err == nil {
		t.Fatal("an unsigned parent passed")
	}
	if kind, err := checkRecipient(t.Context(), conn.(*net.UnixConn), os.Getpid(), "proxy"); err != nil || kind != kindProxy {
		t.Fatalf("trust proxy: %q, %v", kind, err)
	}
}

func TestReadNeedsTheParentsConsumerKindAndUse(t *testing.T) {
	cases := []struct {
		parent string
		answer map[string]string
		ok     bool
	}{
		{kindExecutable, useTicket("k", kindExecutable), true},
		{kindProxy, useTicket("k", kindProxy), true},
		{kindExecutable, useTicket("k", kindProxy), false},
		{kindProxy, useTicket("k", kindExecutable), false},
		{kindProxy, map[string]string{"name": "k"}, false},
		{kindExecutable, map[string]string{"name": "k"}, false},
		{kindExecutable, map[string]string{"name": "k", "consumerKind": kindExecutable, "action": actionRemove}, false},
		{kindProxy, map[string]string{"name": "k", "consumerKind": kindProxy}, false},
	}
	ticket := bytes.Repeat([]byte{7}, 32)
	for _, c := range cases {
		home, _ := fakeBerthd(t, ticket, c.answer)
		t.Setenv("BERTH_HOME", home)
		if err := os.MkdirAll(filepath.Join(home, "fake-native", "values"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "fake-native", "values", "k"), []byte("canary-0c4d"), 0o600); err != nil {
			t.Fatal(err)
		}
		var value, stdout, stderr bytes.Buffer
		useRecipient(t, &value, c.parent, nil)
		code := run([]string{"read"}, ticketLine(ticket), &stdout, &stderr)
		if (code == 0) != c.ok || (value.Len() > 0) != c.ok {
			t.Fatalf("%s parent, answer %v: exit %d, %d value bytes", c.parent, c.answer, code, value.Len())
		}
	}
}

func lifecycleLine(name string, ticket []byte) *strings.Reader {
	return strings.NewReader(`{"name":"` + name + `","ticket":"` + hex.EncodeToString(ticket) + `","requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}}` + "\n")
}

// lifecycleHome serves answer for ticket and plants values/k, with answer as the fake's prompt.
func lifecycleHome(t *testing.T, ticket []byte, berthd map[string]string, answer string) (string, *atomic.Int32) {
	t.Helper()
	home, calls := fakeBerthd(t, ticket, berthd)
	t.Setenv("BERTH_HOME", home)
	t.Setenv("BERTH_FAKE_NATIVE_ANSWER", answer)
	dir := filepath.Join(home, "fake-native")
	if err := os.MkdirAll(filepath.Join(dir, "values"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"value":"bmV3LWNhbmFyeQ=="}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values", "k"), []byte("old-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, calls
}

func runLifecycle(t *testing.T, command, name string, ticket []byte) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{command}, lifecycleLine(name, ticket), &stdout, &stderr); code != 0 {
		t.Fatalf("%s exit %d: %s", command, code, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func TestLifecycleRedeemsBeforeTheAnswer(t *testing.T) {
	ticket := bytes.Repeat([]byte{9}, 32)
	for _, command := range []string{"rotate", "remove"} {
		for name, berthd := range map[string]map[string]string{
			"wrong ticket":  nil,
			"wrong action":  {"name": "k", "action": actionUse},
			"crossed":       {"name": "k", "action": map[string]string{"rotate": actionRemove, "remove": actionRotate}[command]},
			"other secret":  {"name": "other", "action": map[string]string{"rotate": actionRotate, "remove": actionRemove}[command]},
			"no action set": {"name": "k"},
		} {
			sent := ticket
			if berthd == nil {
				sent = bytes.Repeat([]byte{1}, 32)
				berthd = map[string]string{"name": "k", "action": actionRemove}
			}
			// deny would answer denied if the prompt came first; approve would change the item.
			for _, answer := range []string{"deny", "approve"} {
				home, calls := lifecycleHome(t, ticket, berthd, answer)
				got := runLifecycle(t, command, "k", sent)
				if !strings.Contains(got, `"outcome":"failed"`) || calls.Load() != 1 {
					t.Fatalf("%s, %s, %s: %s after %d redeems", command, name, answer, got, calls.Load())
				}
				if v, err := os.ReadFile(filepath.Join(home, "fake-native", "values", "k")); err != nil || string(v) != "old-canary" {
					t.Fatalf("%s, %s, %s: the item changed", command, name, answer)
				}
			}
		}
	}
}

func TestLifecycleAnswers(t *testing.T) {
	ticket := bytes.Repeat([]byte{9}, 32)
	rotate := map[string]string{"name": "k", "action": actionRotate}
	remove := map[string]string{"name": "k", "action": actionRemove}
	cases := []struct {
		command, answer string
		berthd          map[string]string
		want, value     string
	}{
		{"rotate", "approve", rotate, `{"outcome":"stored"}`, "new-canary"},
		{"rotate", "deny", rotate, `{"outcome":"denied"}`, "old-canary"},
		{"rotate", "fail", rotate, `{"error":"fake failure","outcome":"failed"}`, "old-canary"},
		{"remove", "approve", remove, `{"outcome":"removed"}`, ""},
		{"remove", "deny", remove, `{"outcome":"denied"}`, "old-canary"},
		{"remove", "fail", remove, `{"error":"fake failure","outcome":"failed"}`, "old-canary"},
	}
	for _, c := range cases {
		home, _ := lifecycleHome(t, ticket, c.berthd, c.answer)
		if got := runLifecycle(t, c.command, "k", ticket); got != c.want {
			t.Fatalf("%s %s: %s, want %s", c.command, c.answer, got, c.want)
		}
		v, err := os.ReadFile(filepath.Join(home, "fake-native", "values", "k"))
		if string(v) != c.value || (c.value == "") != errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s %s: item %q, %v", c.command, c.answer, v, err)
		}
		log, err := os.ReadFile(filepath.Join(home, "fake-native", "requests.jsonl"))
		if err != nil || bytes.Contains(log, []byte(hex.EncodeToString(ticket))) || bytes.Contains(log, []byte("canary")) {
			t.Fatalf("%s %s: the request log holds a ticket or value: %s", c.command, c.answer, log)
		}
	}
}

func TestRemoveOfAMissingItemIsRemovedAndRotateFails(t *testing.T) {
	ticket := bytes.Repeat([]byte{9}, 32)
	home, _ := lifecycleHome(t, ticket, map[string]string{"name": "k", "action": actionRemove}, "approve")
	if err := os.Remove(filepath.Join(home, "fake-native", "values", "k")); err != nil {
		t.Fatal(err)
	}
	if got := runLifecycle(t, "remove", "k", ticket); got != `{"outcome":"removed"}` {
		t.Fatalf("remove: %s", got)
	}
	lifecycleHome(t, ticket, map[string]string{"name": "gone", "action": actionRotate}, "approve")
	if got := runLifecycle(t, "rotate", "gone", ticket); got != `{"error":"item not found","outcome":"failed"}` {
		t.Fatalf("rotate: %s", got)
	}
}

func TestConfirmChecksTheAction(t *testing.T) {
	t.Setenv("BERTH_FAKE_NATIVE_DIR", t.TempDir())
	req := `"requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}`
	for line, want := range map[string]int{
		`{"action":"grant","secret":"k","consumer":{"path":"/x"},` + req + `}`:                                             0,
		`{"action":"proxy","secret":"k","target":{"host":"api.example.com","port":443,"header_name":"X-Key"},` + req + `}`: 0,
		`{"action":"proxy","secret":"k","target":{"host":"api.example.com","port":0,"header_name":"X-Key"},` + req + `}`:   exitCrash,
		`{"action":"proxy","secret":"k","target":{"host":"","port":443,"header_name":"X-Key"},` + req + `}`:                exitCrash,
		`{"action":"proxy","secret":"k",` + req + `}`:                                                                      exitCrash,
		`{"action":"read","secret":"k",` + req + `}`:                                                                       exitCrash,
	} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"confirm"}, strings.NewReader(line+"\n"), &stdout, &stderr); code != want {
			t.Fatalf("%s: exit %d, want %d", line, code, want)
		}
	}
}
