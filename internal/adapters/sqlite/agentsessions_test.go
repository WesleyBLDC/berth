package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"berth/internal/adapters/sqlite"
	"berth/internal/core/ports"
)

func TestAgentSessionRecordsBindOnce(t *testing.T) {
	ctx := t.Context()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.UnixMicro(1_700_000_000_000_000).UTC()
	r := ports.AgentSessionRecord{
		ID: "0123456789abcdef0123456789abcdef", Name: "claude", TokenHash: [32]byte{1, 2, 3},
		CreatedAt: created, ExpiresAt: created.Add(time.Hour),
		Root: ports.ProcessRef{PID: 9, Start: created}, // ignored on add
	}
	if err := s.AddAgentSession(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.AgentSessionByToken(ctx, r.TokenHash)
	if err != nil || !found || got.ID != r.ID || got.Name != "claude" || !got.ExpiresAt.Equal(r.ExpiresAt) || got.Root != (ports.ProcessRef{}) {
		t.Fatalf("by token: %+v %v %v", got, found, err)
	}
	if _, found, _ := s.AgentSessionByToken(ctx, [32]byte{9}); found {
		t.Fatal("another hash found the session")
	}
	first := ports.ProcessRef{PID: 40, Start: time.UnixMicro(1_700_000_000_123_456)}
	if root, err := s.BindAgentSession(ctx, r.ID, first); err != nil || !root.Same(first) {
		t.Fatalf("first bind: %+v %v", root, err)
	}
	// A second first call loses and learns the tree that won.
	if root, err := s.BindAgentSession(ctx, r.ID, ports.ProcessRef{PID: 77, Start: created}); err != nil || !root.Same(first) {
		t.Fatalf("second bind: %+v %v", root, err)
	}
	all, err := s.AgentSessions(ctx)
	if err != nil || len(all) != 1 || !all[0].Root.Same(first) {
		t.Fatalf("list: %+v %v", all, err)
	}
	if err := s.AddAgentSession(ctx, ports.AgentSessionRecord{ID: "fedcba9876543210fedcba9876543210", TokenHash: r.TokenHash, CreatedAt: created, ExpiresAt: created}); err == nil {
		t.Fatal("two sessions share a token hash")
	}
}
