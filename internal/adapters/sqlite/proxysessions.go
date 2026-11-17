package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"berth/internal/core/ports"
)

var _ ports.ProxySessionRecords = (*Store)(nil)

func (s *Store) AddProxySession(ctx context.Context, r ports.ProxySessionRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_sessions (id, proxy_pid, created_at) VALUES (?, ?, ?)`,
		r.ID, r.PID, r.CreatedAt.UnixMicro()); err != nil {
		return err
	}
	for _, g := range r.Grants {
		if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_session_grants (session_id, grant_id) VALUES (?, ?)`, r.ID, g); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ProxySession(ctx context.Context, id string) (ports.ProxySessionRecord, bool, error) {
	r := ports.ProxySessionRecord{ID: id}
	var created int64
	var ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT proxy_pid, created_at, ended_at, injected, refused FROM proxy_sessions WHERE id = ?`, id).
		Scan(&r.PID, &created, &ended, &r.Injected, &r.Refused)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ProxySessionRecord{}, false, nil
	}
	if err != nil {
		return ports.ProxySessionRecord{}, false, err
	}
	r.CreatedAt = time.UnixMicro(created).UTC()
	if ended.Valid {
		r.EndedAt = time.UnixMicro(ended.Int64).UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT grant_id FROM proxy_session_grants WHERE session_id = ? ORDER BY grant_id`, id)
	if err != nil {
		return ports.ProxySessionRecord{}, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			return ports.ProxySessionRecord{}, false, err
		}
		r.Grants = append(r.Grants, g)
	}
	if err := rows.Err(); err != nil {
		return ports.ProxySessionRecord{}, false, err
	}
	return r, true, nil
}

func (s *Store) ProxySessionsForGrant(ctx context.Context, grantID int64) ([]string, error) {
	return s.sessionIDs(ctx, `SELECT s.id FROM proxy_sessions s JOIN proxy_session_grants g ON g.session_id = s.id
		WHERE g.grant_id = ? AND s.ended_at IS NULL ORDER BY s.created_at, s.id`, grantID)
}

func (s *Store) OpenProxySessions(ctx context.Context) ([]string, error) {
	return s.sessionIDs(ctx, `SELECT id FROM proxy_sessions WHERE ended_at IS NULL ORDER BY created_at, id`)
}

func (s *Store) sessionIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) EndProxySession(ctx context.Context, id string, injected, refused int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE proxy_sessions SET ended_at = ?, injected = ?, refused = ? WHERE id = ? AND ended_at IS NULL`,
		at.UnixMicro(), injected, refused, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("no open proxy session %s", id)
	}
	return nil
}
