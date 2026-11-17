package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"berth/internal/adapters/sqlite"
	"berth/internal/core/ports"
)

func open(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func rec(version int64, from, to string) ports.TransitionRecord {
	return ports.TransitionRecord{Entity: "e1", Kind: "test", Version: version, Event: "Go", From: from, To: to, At: time.Unix(1_800_000_000, 0)}
}

func TestSaveLoadHistory(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "b.db"))
	for i, step := range [][2]string{{"", "A"}, {"A", "B"}, {"B", "C"}} {
		if err := s.SaveTransition(ctx, rec(int64(i+1), step[0], step[1])); err != nil {
			t.Fatal(err)
		}
	}
	st, found, err := s.LoadState(ctx, "e1")
	if err != nil || !found || st.State != "C" || st.Version != 3 {
		t.Fatalf("load: %+v %v %v", st, found, err)
	}
	h, err := s.History(ctx, "e1", 10)
	if err != nil || len(h) != 3 || h[0].To != "C" || h[2].To != "A" {
		t.Fatalf("history: %+v %v", h, err)
	}
}

func TestEntitiesOfKind(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "b.db"))
	for _, r := range []ports.TransitionRecord{
		{Entity: "session/2-1", Kind: "session", Version: 1, Event: "Busy", To: "Active"},
		{Entity: "session/1-1", Kind: "session", Version: 1, Event: "Quiet", To: "Idle"},
		{Entity: "daemon", Kind: "daemon", Version: 1, Event: "Boot", To: "Recovering"},
	} {
		if err := s.SaveTransition(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.EntitiesOfKind(ctx, "session")
	if err != nil || len(got) != 2 || got[0].Entity != "session/1-1" || got[0].State != "Idle" || got[1].Entity != "session/2-1" {
		t.Fatalf("sessions: %+v %v", got, err)
	}
}

func TestStaleVersionIsRejectedAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "b.db"))
	if err := s.SaveTransition(ctx, rec(1, "", "A")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTransition(ctx, rec(1, "", "B")); !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("duplicate first write: %v", err)
	}
	if err := s.SaveTransition(ctx, rec(3, "A", "C")); !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("skipped version: %v", err)
	}
	h, _ := s.History(ctx, "e1", 10)
	if len(h) != 1 {
		t.Fatalf("rejected writes left %d event rows, want 1", len(h))
	}
}

func TestRequestLog(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "b.db"))
	if _, _, found, _ := s.Recall(ctx, "owner", "req-1"); found {
		t.Fatal("found before remember")
	}
	if err := s.Remember(ctx, "owner", "req-1", "Shutdown", []byte("first"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(ctx, "owner", "req-1", "Shutdown", []byte("second"), time.Now()); err == nil {
		t.Fatal("a request id was stored twice")
	}
	method, resp, found, err := s.Recall(ctx, "owner", "req-1")
	if err != nil || !found || method != "Shutdown" || string(resp) != "first" {
		t.Fatalf("recall: %q %q %v %v", method, resp, found, err)
	}
	// Another principal's request id is its own: it finds nothing and may store its own reply.
	if _, _, found, _ := s.Recall(ctx, "agent", "req-1"); found {
		t.Fatal("another principal found the owner's reply")
	}
	if err := s.Remember(ctx, "agent", "req-1", "Shutdown", []byte("agent"), time.Now()); err != nil {
		t.Fatalf("another principal could not store its own reply: %v", err)
	}
}

// TestKilledWriterLeavesNoHalfTransition runs a child process that writes transitions as fast
// as it can, kills it with SIGKILL mid stream, and checks that state and events still agree.
func TestKilledWriterLeavesNoHalfTransition(t *testing.T) {
	if os.Getenv("BERTH_CRASH_CHILD") != "" {
		churn(os.Getenv("BERTH_CRASH_CHILD"))
		return
	}
	db := filepath.Join(t.TempDir(), "crash.db")
	for round := range 3 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestKilledWriterLeavesNoHalfTransition$")
		cmd.Env = append(os.Environ(), "BERTH_CRASH_CHILD="+db)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(150+round*100) * time.Millisecond)
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()

		s, err := sqlite.Open(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		st, found, err := s.LoadState(context.Background(), "churn")
		if err != nil || !found {
			t.Fatalf("round %d: no state after crash: %v", round, err)
		}
		h, err := s.History(context.Background(), "churn", 1)
		if err != nil || len(h) != 1 {
			t.Fatalf("round %d: history: %v", round, err)
		}
		if h[0].Version != st.Version || h[0].To != st.State {
			t.Fatalf("round %d: state %s v%d but newest event %s v%d", round, st.State, st.Version, h[0].To, h[0].Version)
		}
		var events int64
		all, _ := s.History(context.Background(), "churn", 1_000_000)
		events = int64(len(all))
		if events != st.Version {
			t.Fatalf("round %d: %d event rows for version %d", round, events, st.Version)
		}
		_ = s.Close()
		t.Logf("round %d: consistent at version %d", round, st.Version)
	}
}

func churn(path string) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		os.Exit(2)
	}
	st, found, _ := s.LoadState(ctx, "churn")
	v := int64(0)
	if found {
		v = st.Version
	}
	for {
		v++
		r := ports.TransitionRecord{Entity: "churn", Kind: "test", Version: v, Event: "Tick", From: fmt.Sprint(v - 1), To: fmt.Sprint(v), At: time.Now()}
		if err := s.SaveTransition(ctx, r); err != nil {
			os.Exit(3)
		}
	}
}

// TestMigrationsAreContiguous catches two agents taking the same migration number, or a gap.
func TestMigrationsAreContiguous(t *testing.T) {
	names, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no migrations: %v", err)
	}
	for i, name := range names {
		want := fmt.Sprintf("%04d_", i+1)
		if !strings.HasPrefix(filepath.Base(name), want) {
			t.Fatalf("migration %d is %s; numbers must run 0001, 0002, ... with no gaps or repeats", i+1, filepath.Base(name))
		}
	}
}
