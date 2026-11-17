package sqlite_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
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
	exe := ports.Executable{Path: "/usr/bin/true", SHA256: [32]byte{0xab, 1}, TeamID: "T", SigningID: "S", CDHash: ports.CDHash{0xcd, 2}}
	unpinned := exe
	unpinned.CDHash = ports.CDHash{}
	id1, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "gh-token", Exe: exe, ArgsGlob: "-c *", PassEnv: []string{"GH_HOST", "AWS_REGION"}, CreatedAt: first})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "gh-token", Exe: unpinned, CreatedAt: first})
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
	if grants[0].ID != id2 || grants[0].Exe != unpinned || !grants[0].LastUsedAt.IsZero() || grants[0].UseCount != 0 {
		t.Fatalf("newest grant %+v", grants[0])
	}
	g := grants[1]
	if g.ID != id1 || g.Exe != exe || g.ArgsGlob != "-c *" || g.UseCount != 1 || !g.LastUsedAt.Equal(used) ||
		strings.Join(g.PassEnv, ",") != "GH_HOST,AWS_REGION" || grants[0].PassEnv != nil {
		t.Fatalf("used grant %+v", g)
	}
}

func TestGrantLookupRoundTripsBothKinds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMicro(1_700_000_000_000_000).UTC()
	if err := s.AddSecret(ctx, "openai", at); err != nil {
		t.Fatal(err)
	}
	exe := ports.GrantRecord{
		Secret: "openai", Kind: ports.ConsumerExe, CreatedAt: at, ArgsGlob: "*", PassEnv: []string{"HOME_DIR"},
		Exe: ports.Executable{Path: "/usr/bin/true", SHA256: [32]byte{7}, TeamID: "T", SigningID: "S"},
	}
	proxy := ports.GrantRecord{
		Secret: "openai", Kind: ports.ConsumerProxy, CreatedAt: at,
		Proxy: ports.ProxyTarget{
			Host: "api.openai.com", Port: 443, HeaderName: "Authorization", ValuePrefix: "Bearer ", ValueSuffix: ";",
			PathGlob: "/v1/*", Methods: []string{"GET", "POST"}, PlaceholderEnv: "OPENAI_API_KEY",
		},
	}
	exeID, err := s.AddGrant(ctx, exe)
	if err != nil {
		t.Fatal(err)
	}
	proxyID, err := s.AddGrant(ctx, proxy)
	if err != nil {
		t.Fatal(err)
	}
	// An empty Kind is stored as exe.
	legacy := exe
	legacy.Kind = ""
	legacyID, err := s.AddGrant(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	// A proxy grant carrying exe fields breaks the table's CHECK.
	mixed := proxy
	mixed.ArgsGlob = "*"
	if _, err := s.AddGrant(ctx, mixed); err == nil {
		t.Fatal("a proxy grant with an args glob was stored")
	}
	_ = s.Close()

	s, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, c := range []struct {
		id   int64
		want ports.GrantRecord
	}{{exeID, exe}, {proxyID, proxy}, {legacyID, exe}} {
		got, ok, err := s.Grant(ctx, c.id)
		if err != nil || !ok {
			t.Fatalf("grant %d: %v %v", c.id, ok, err)
		}
		c.want.ID = c.id
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("grant %d\n got %+v\nwant %+v", c.id, got, c.want)
		}
	}
	listed, err := s.Grants(ctx, "openai")
	if err != nil || len(listed) != 3 || listed[1].ID != proxyID || !reflect.DeepEqual(listed[1].Proxy, proxy.Proxy) {
		t.Fatalf("grants %+v, %v", listed, err)
	}
	if _, ok, err := s.Grant(ctx, 999); ok || err != nil {
		t.Fatalf("missing grant: %v %v", ok, err)
	}
}

func TestProxySessionRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMicro(1_700_000_000_000_000).UTC()
	if err := s.AddSecret(ctx, "openai", at); err != nil {
		t.Fatal(err)
	}
	target := ports.ProxyTarget{Host: "api.openai.com", Port: 443, HeaderName: "Authorization"}
	g1, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "openai", Kind: ports.ConsumerProxy, Proxy: target, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "openai", Kind: ports.ConsumerProxy, Proxy: target, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	// EndedAt and the counts are ignored on add.
	if err := s.AddProxySession(ctx, ports.ProxySessionRecord{ID: a, PID: 41, Grants: []int64{g2, g1}, CreatedAt: at, Injected: 9, EndedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProxySession(ctx, ports.ProxySessionRecord{ID: b, PID: 42, Grants: []int64{g2}, CreatedAt: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProxySession(ctx, ports.ProxySessionRecord{ID: a, PID: 43, CreatedAt: at}); err == nil {
		t.Fatal("a duplicate session id was stored")
	}
	// An unknown grant fails the foreign key and leaves no session behind.
	c := strings.Repeat("c", 32)
	if err := s.AddProxySession(ctx, ports.ProxySessionRecord{ID: c, PID: 44, Grants: []int64{999}, CreatedAt: at}); err == nil {
		t.Fatal("a session linked to a missing grant was stored")
	}
	if _, ok, err := s.ProxySession(ctx, c); ok || err != nil {
		t.Fatalf("half stored session: %v %v", ok, err)
	}

	open, err := s.OpenProxySessions(ctx)
	if err != nil || !slices.Equal(open, []string{a, b}) {
		t.Fatalf("open %v, %v", open, err)
	}
	if ids, err := s.ProxySessionsForGrant(ctx, g1); err != nil || !slices.Equal(ids, []string{a}) {
		t.Fatalf("for g1 %v, %v", ids, err)
	}
	if ids, err := s.ProxySessionsForGrant(ctx, g2); err != nil || !slices.Equal(ids, []string{a, b}) {
		t.Fatalf("for g2 %v, %v", ids, err)
	}

	ended := at.Add(time.Minute)
	if err := s.EndProxySession(ctx, a, 3, 1, ended); err != nil {
		t.Fatal(err)
	}
	if err := s.EndProxySession(ctx, a, 5, 5, ended); err == nil {
		t.Fatal("a session ended twice")
	}
	if err := s.EndProxySession(ctx, c, 0, 0, ended); err == nil {
		t.Fatal("a missing session ended")
	}
	_ = s.Close()

	s, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, ok, err := s.ProxySession(ctx, a)
	want := ports.ProxySessionRecord{ID: a, PID: 41, Grants: []int64{g1, g2}, CreatedAt: at, EndedAt: ended, Injected: 3, Refused: 1}
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("session a\n got %+v %v %v\nwant %+v", got, ok, err, want)
	}
	got, ok, err = s.ProxySession(ctx, b)
	want = ports.ProxySessionRecord{ID: b, PID: 42, Grants: []int64{g2}, CreatedAt: at.Add(time.Second)}
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("session b\n got %+v %v %v\nwant %+v", got, ok, err, want)
	}
	if open, err := s.OpenProxySessions(ctx); err != nil || !slices.Equal(open, []string{b}) {
		t.Fatalf("open after end %v, %v", open, err)
	}
	if ids, err := s.ProxySessionsForGrant(ctx, g1); err != nil || len(ids) != 0 {
		t.Fatalf("for g1 after end %v, %v", ids, err)
	}
}
