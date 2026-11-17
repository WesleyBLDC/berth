package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"berth/internal/core/ports"
)

var _ ports.SecretRecords = (*Store)(nil)

func (s *Store) AddSecret(ctx context.Context, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`,
		name, at.UnixMicro())
	return err
}

func (s *Store) Secrets(ctx context.Context) ([]ports.SecretRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.SecretRecord
	for rows.Next() {
		var r ports.SecretRecord
		var at int64
		if err := rows.Scan(&r.Name, &at); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMicro(at).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AddGrant(ctx context.Context, g ports.GrantRecord) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO grants (secret_name, exe_path, sha256, team_id, signing_id, args_glob, pass_env, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Secret, g.Exe.Path, hex.EncodeToString(g.Exe.SHA256[:]), g.Exe.TeamID, g.Exe.SigningID, g.ArgsGlob,
		strings.Join(g.PassEnv, ","), g.CreatedAt.UnixMicro())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Grants(ctx context.Context, secret string) ([]ports.GrantRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, exe_path, sha256, team_id, signing_id, args_glob, pass_env, created_at, last_used_at, use_count
		 FROM grants WHERE secret_name = ? ORDER BY id DESC`, secret)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.GrantRecord
	for rows.Next() {
		g := ports.GrantRecord{Secret: secret}
		var sum, passEnv string
		var created int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&g.ID, &g.Exe.Path, &sum, &g.Exe.TeamID, &g.Exe.SigningID, &g.ArgsGlob, &passEnv, &created, &lastUsed, &g.UseCount); err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(sum)
		if err != nil || len(raw) != len(g.Exe.SHA256) {
			return nil, fmt.Errorf("grant %d has a malformed sha256", g.ID)
		}
		copy(g.Exe.SHA256[:], raw)
		if passEnv != "" {
			g.PassEnv = strings.Split(passEnv, ",")
		}
		g.CreatedAt = time.UnixMicro(created).UTC()
		if lastUsed.Valid {
			g.LastUsedAt = time.UnixMicro(lastUsed.Int64).UTC()
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) CountUse(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE grants SET use_count = use_count + 1, last_used_at = ? WHERE id = ?`, at.UnixMicro(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("no grant %d", id)
	}
	return nil
}
