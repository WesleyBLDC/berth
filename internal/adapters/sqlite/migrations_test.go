package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"berth/internal/core/ports"
)

// openAt builds a database that stopped at migration version, as an older berthd left it.
func openAt(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	names, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names[:version] {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestProxyGrantsMigrationKeepsGrantIDs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	old := openAt(t, path, 3)
	sum := strings.Repeat("ab", 32)
	for _, q := range []string{
		`INSERT INTO secrets (name, created_at) VALUES ('tok', 1)`,
		`INSERT INTO grants (id, secret_name, exe_path, sha256, created_at, pass_env) VALUES (1, 'tok', '/bin/a', '` + sum + `', 1, 'HOME_X')`,
		`INSERT INTO grants (id, secret_name, exe_path, sha256, created_at, use_count, last_used_at) VALUES (5, 'tok', '/bin/b', '` + sum + `', 2, 3, 4)`,
		// A deleted grant leaves the counter at 9; its entity grant/9 may still be in machine_state.
		`INSERT INTO grants (id, secret_name, exe_path, sha256, created_at) VALUES (9, 'tok', '/bin/c', '` + sum + `', 3)`,
		`DELETE FROM grants WHERE id = 9`,
	} {
		if _, err := old.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = old.Close()

	s := open(t, path)
	grants, err := s.Grants(ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 || grants[0].ID != 5 || grants[1].ID != 1 {
		t.Fatalf("grants after migration = %+v, want ids 5 and 1", grants)
	}
	if grants[0].Exe.Path != "/bin/b" || grants[0].UseCount != 3 || grants[0].LastUsedAt.IsZero() || grants[1].PassEnv[0] != "HOME_X" {
		t.Fatalf("grant facts changed: %+v", grants)
	}
	id, err := s.AddGrant(ctx, ports.GrantRecord{Secret: "tok", Exe: ports.Executable{Path: "/bin/d"}, CreatedAt: time.Unix(5, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if id <= 9 {
		t.Fatalf("next grant id = %d, want above the old counter 9", id)
	}

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var kinds string
	if err := raw.QueryRowContext(t.Context(), `SELECT group_concat(DISTINCT kind) FROM grants`).Scan(&kinds); err != nil || kinds != "exe" {
		t.Fatalf("kinds = %q %v, want exe", kinds, err)
	}
	proxy := `INSERT INTO grants (secret_name, kind, proxy_host, proxy_port, proxy_header, created_at) VALUES ('tok', 'proxy', 'api.example.com', 443, 'Authorization', 6)`
	if _, err := raw.ExecContext(t.Context(), proxy); err != nil {
		t.Fatalf("valid proxy grant refused: %v", err)
	}
	for name, q := range map[string]string{
		"proxy with an exe path": `INSERT INTO grants (secret_name, kind, exe_path, proxy_host, proxy_port, proxy_header, created_at) VALUES ('tok', 'proxy', '/bin/a', 'h', 443, 'X', 7)`,
		"proxy without a port":   `INSERT INTO grants (secret_name, kind, proxy_host, proxy_header, created_at) VALUES ('tok', 'proxy', 'h', 'X', 7)`,
		"exe with a proxy host":  `INSERT INTO grants (secret_name, exe_path, sha256, proxy_host, created_at) VALUES ('tok', '/bin/a', '` + sum + `', 'h', 7)`,
		"unknown kind":           `INSERT INTO grants (secret_name, kind, created_at) VALUES ('tok', 'mcp', 7)`,
	} {
		if _, err := raw.ExecContext(t.Context(), q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO proxy_sessions (id, proxy_pid, created_at) VALUES ('`+strings.Repeat("0", 32)+`', 42, 8)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO proxy_session_grants (session_id, grant_id) VALUES ('`+strings.Repeat("0", 32)+`', 5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO proxy_session_grants (session_id, grant_id) VALUES ('`+strings.Repeat("0", 32)+`', 9)`); err == nil {
		t.Error("a session held a grant that does not exist")
	}
}
