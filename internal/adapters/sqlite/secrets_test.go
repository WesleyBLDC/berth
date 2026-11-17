package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berth/internal/adapters/sqlite"
	"berth/internal/core/ports"
)

func TestSecretAndGrantRecordsRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	first := time.UnixMicro(1_700_000_000_000_000).UTC()
	if err := s.AddSecret(ctx, "gh-token", first); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret(ctx, "gh-token", first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	exe := ports.Executable{Path: "/usr/bin/true", SHA256: [32]byte{0xab, 1}, TeamID: "T", SigningID: "S"}
	id1, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "gh-token", Exe: exe, ArgsGlob: "-c *", PassEnv: []string{"GH_HOST", "AWS_REGION"}, CreatedAt: first})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "gh-token", Exe: exe, CreatedAt: first})
	if err != nil {
		t.Fatal(err)
	}
	used := first.Add(time.Minute)
	if err := s.CountUse(ctx, id1, used); err != nil {
		t.Fatal(err)
	}
	if err := s.CountUse(ctx, 999, used); err == nil {
		t.Fatal("counting a use on a missing grant succeeded")
	}
	_ = s.Close()

	// Reopen, so the rows are read back from the file.
	s, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	secrets, err := s.Secrets(ctx)
	if err != nil || len(secrets) != 1 || secrets[0].Name != "gh-token" || !secrets[0].CreatedAt.Equal(first) {
		t.Fatalf("secrets %+v, %v", secrets, err)
	}
	grants, err := s.Grants(ctx, "gh-token")
	if err != nil || len(grants) != 2 {
		t.Fatalf("grants %+v, %v", grants, err)
	}
	if grants[0].ID != id2 || !grants[0].LastUsedAt.IsZero() || grants[0].UseCount != 0 {
		t.Fatalf("newest grant %+v", grants[0])
	}
	g := grants[1]
	if g.ID != id1 || g.Exe != exe || g.ArgsGlob != "-c *" || g.UseCount != 1 || !g.LastUsedAt.Equal(used) ||
		strings.Join(g.PassEnv, ",") != "GH_HOST,AWS_REGION" || grants[0].PassEnv != nil {
		t.Fatalf("used grant %+v", g)
	}
}
