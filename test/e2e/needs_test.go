package e2e

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/client"
)

type needsRun struct {
	msgs chan *berthv1.WatchNeedsResponse
	done chan error
}

// watchNeeds opens a WatchNeeds stream after the cursor and reads it in the background.
func watchNeeds(t *testing.T, c *client.Client, after int64) *needsRun {
	t.Helper()
	stream, err := c.Needs.WatchNeeds(t.Context(), &berthv1.WatchNeedsRequest{AfterEventId: after})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	r := &needsRun{msgs: make(chan *berthv1.WatchNeedsResponse, 256), done: make(chan error, 1)}
	go func() {
		for {
			m, err := stream.Receive()
			if err != nil {
				r.done <- err
				return
			}
			r.msgs <- m
		}
	}()
	return r
}

// until skips messages about other entities, since the Mac may hold leftover browsers of its own.
func (r *needsRun) until(t *testing.T, what string, ok func(*berthv1.WatchNeedsResponse) bool) *berthv1.WatchNeedsResponse {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if ok(m) {
				return m
			}
		case err := <-r.done:
			t.Fatalf("the stream ended before it %s: %v", what, err)
		case <-deadline:
			t.Fatalf("the stream never %s", what)
		}
	}
}

func changeFor(id string, event berthv1.NeedEvent, kind berthv1.NeedKind) func(*berthv1.WatchNeedsResponse) bool {
	return func(m *berthv1.WatchNeedsResponse) bool {
		c := m.GetChange()
		return c.GetEvent() == event && c.GetNeed().GetId() == id && c.GetNeed().GetKind() == kind
	}
}

// A store dialog the fake helper holds open is a need while it waits; it resolves when berthd
// stops, and a client that resumes after the cursor it had sees the resolution from the next
// berthd.
func TestNeedsStreamAStoreDialogAcrossARestart(t *testing.T) {
	h := home(t)
	d := start(t, h)
	c := waitServing(t, h)
	first := watchNeeds(t, c, 0)
	if snap := first.until(t, "sent a snapshot", func(m *berthv1.WatchNeedsResponse) bool { return m.GetSnapshot() != nil }).GetSnapshot(); snap.GetReason() != berthv1.SnapshotReason_SNAPSHOT_REASON_START {
		t.Fatalf("first message %v", snap)
	}
	openDialog(t, h, "e2e-need")
	opened := first.until(t, "opened the store need", changeFor("secret/e2e-need", berthv1.NeedEvent_NEED_EVENT_OPENED, berthv1.NeedKind_NEED_KIND_STORE))
	if name := opened.GetChange().GetNeed().GetSecretName(); name != "e2e-need" {
		t.Fatalf("store need names %q", name)
	}

	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("berthd exited with %v", err)
	}
	for _, p := range helperProcesses(t) {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	start(t, h)
	again := watchNeeds(t, waitServing(t, h), opened.GetChange().GetEventId())
	again.until(t, "resolved the store need", changeFor("secret/e2e-need", berthv1.NeedEvent_NEED_EVENT_RESOLVED, berthv1.NeedKind_NEED_KIND_STORE))
}

// A real browser whose owner exits is a leftover need until berth stop ends it.
func TestNeedsStreamALeftoverBrowser(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	r := watchNeeds(t, waitServing(t, h), 0)
	_, args := chromeArgs(t)
	quoted := make([]string, 0, len(args)+1)
	for _, a := range append([]string{chrome}, args...) {
		quoted = append(quoted, "'"+a+"'")
	}
	owner := exec.CommandContext(t.Context(), "/bin/sh", "-c", strings.Join(quoted, " ")+" >/dev/null 2>&1 & echo $!")
	out, err := owner.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("owner printed %q", out)
	}
	killOnCleanup(t, pid)
	s := waitSession(t, h, pid, "became a leftover", func(s *berthv1.Session) bool { return s.GetState() == berthv1.SessionState_SESSION_STATE_LEFTOVER })
	id := "session/" + s.GetId()
	opened := r.until(t, "opened the leftover need", changeFor(id, berthv1.NeedEvent_NEED_EVENT_OPENED, berthv1.NeedKind_NEED_KIND_LEFTOVER))
	if got := opened.GetChange().GetNeed().GetBrowser(); got != s.GetKind() {
		t.Fatalf("leftover need says %v, the session is %v", got, s.GetKind())
	}
	berth(t, h, "stop", s.GetId())
	r.until(t, "resolved the leftover need", changeFor(id, berthv1.NeedEvent_NEED_EVENT_RESOLVED, berthv1.NeedKind_NEED_KIND_LEFTOVER))

	// A client starting now finds no need for the stopped browser.
	snap := watchNeeds(t, waitServing(t, h), 0).until(t, "sent a snapshot", func(m *berthv1.WatchNeedsResponse) bool { return m.GetSnapshot() != nil })
	for _, n := range snap.GetSnapshot().GetNeeds() {
		if n.GetId() == id {
			t.Fatalf("the stopped browser is still a need: %v", n)
		}
	}
}
