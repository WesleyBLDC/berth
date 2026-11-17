package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
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

// AddGrant writes only the columns of g's kind; the table's CHECKs refuse a mixed row.
func (s *Store) AddGrant(ctx context.Context, g ports.GrantRecord) (int64, error) {
	kind := g.Kind
	if kind == "" {
		kind = ports.ConsumerExe
	}
	var sum, cdhash string
	if kind == ports.ConsumerExe {
		sum = hex.EncodeToString(g.Exe.SHA256[:])
		if g.Exe.CDHash != (ports.CDHash{}) {
			cdhash = hex.EncodeToString(g.Exe.CDHash[:])
		}
	}
	p := g.Proxy
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO grants (secret_name, kind, exe_path, sha256, team_id, signing_id, args_glob, pass_env,
		 proxy_host, proxy_port, proxy_header, proxy_value_prefix, proxy_value_suffix, proxy_path_glob, proxy_methods,
		 proxy_placeholder_env, created_at, cdhash, signature) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Secret, string(kind), g.Exe.Path, sum, g.Exe.TeamID, g.Exe.SigningID, g.ArgsGlob, strings.Join(g.PassEnv, ","),
		p.Host, p.Port, p.HeaderName, p.ValuePrefix, p.ValueSuffix, p.PathGlob, strings.Join(p.Methods, ","),
		p.PlaceholderEnv, g.CreatedAt.UnixMicro(), cdhash, string(g.Exe.Signature))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const grantColumns = `id, secret_name, kind, exe_path, sha256, team_id, signing_id, args_glob, pass_env,
	proxy_host, proxy_port, proxy_header, proxy_value_prefix, proxy_value_suffix, proxy_path_glob, proxy_methods,
	proxy_placeholder_env, created_at, last_used_at, use_count, cdhash, signature`

func scanGrant(row interface{ Scan(...any) error }) (ports.GrantRecord, error) {
	var g ports.GrantRecord
	var kind, sum, passEnv, methods, cdhash, signature string
	var created int64
	var lastUsed sql.NullInt64
	p := &g.Proxy
	if err := row.Scan(&g.ID, &g.Secret, &kind, &g.Exe.Path, &sum, &g.Exe.TeamID, &g.Exe.SigningID, &g.ArgsGlob, &passEnv,
		&p.Host, &p.Port, &p.HeaderName, &p.ValuePrefix, &p.ValueSuffix, &p.PathGlob, &methods,
		&p.PlaceholderEnv, &created, &lastUsed, &g.UseCount, &cdhash, &signature); err != nil {
		return g, err
	}
	g.Kind = ports.ConsumerKind(kind)
	g.Exe.Signature = ports.Signature(signature)
	if g.Kind == ports.ConsumerExe {
		raw, err := hex.DecodeString(sum)
		if err != nil || len(raw) != len(g.Exe.SHA256) {
			return g, fmt.Errorf("grant %d has a malformed sha256", g.ID)
		}
		copy(g.Exe.SHA256[:], raw)
		if cdhash != "" {
			raw, err := hex.DecodeString(cdhash)
			if err != nil || len(raw) != len(g.Exe.CDHash) {
				return g, fmt.Errorf("grant %d has a malformed cdhash", g.ID)
			}
			copy(g.Exe.CDHash[:], raw)
		}
	}
	if passEnv != "" {
		g.PassEnv = strings.Split(passEnv, ",")
	}
	if methods != "" {
		p.Methods = strings.Split(methods, ",")
	}
	g.CreatedAt = time.UnixMicro(created).UTC()
	if lastUsed.Valid {
		g.LastUsedAt = time.UnixMicro(lastUsed.Int64).UTC()
	}
	return g, nil
}

func (s *Store) Grants(ctx context.Context, secret string) ([]ports.GrantRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+grantColumns+` FROM grants WHERE secret_name = ? ORDER BY id DESC`, secret)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.GrantRecord
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) Grant(ctx context.Context, id int64) (ports.GrantRecord, bool, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ports.GrantRecord{}, false, nil
	}
	if err != nil {
		return ports.GrantRecord{}, false, err
	}
	return g, true, nil
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
