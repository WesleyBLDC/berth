package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"berth/internal/core/ports"
)

var _ ports.AgentSessionRecords = (*Store)(nil)

const agentSessionColumns = `id, name, token_hash, created_at, expires_at, root_pid, root_start`

func (s *Store) AddAgentSession(ctx context.Context, r ports.AgentSessionRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_sessions (id, name, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.TokenHash[:], r.CreatedAt.UnixMicro(), r.ExpiresAt.UnixMicro())
	return err
}

func (s *Store) AgentSession(ctx context.Context, id string) (ports.AgentSessionRecord, bool, error) {
	return s.agentSession(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions WHERE id = ?`, id)
}

func (s *Store) AgentSessionByToken(ctx context.Context, hash [32]byte) (ports.AgentSessionRecord, bool, error) {
	return s.agentSession(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions WHERE token_hash = ?`, hash[:])
}

func (s *Store) agentSession(ctx context.Context, query string, arg any) (ports.AgentSessionRecord, bool, error) {
	r, err := scanAgentSession(s.db.QueryRowContext(ctx, query, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return ports.AgentSessionRecord{}, false, nil
	}
	if err != nil {
		return ports.AgentSessionRecord{}, false, err
	}
	return r, true, nil
}

func (s *Store) AgentSessions(ctx context.Context) ([]ports.AgentSessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.AgentSessionRecord
	for rows.Next() {
		r, err := scanAgentSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) BindAgentSession(ctx context.Context, id string, root ports.ProcessRef) (ports.ProcessRef, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET root_pid = ?, root_start = ? WHERE id = ? AND root_pid IS NULL`,
		root.PID, root.Start.UnixMicro(), id); err != nil {
		return ports.ProcessRef{}, err
	}
	r, found, err := s.AgentSession(ctx, id)
	switch {
	case err != nil:
		return ports.ProcessRef{}, err
	case !found:
		return ports.ProcessRef{}, fmt.Errorf("no agent session %s", id)
	}
	return r.Root, nil
}

func scanAgentSession(row interface{ Scan(...any) error }) (ports.AgentSessionRecord, error) {
	var r ports.AgentSessionRecord
	var hash []byte
	var created, expires int64
	var rootPID, rootStart sql.NullInt64
	if err := row.Scan(&r.ID, &r.Name, &hash, &created, &expires, &rootPID, &rootStart); err != nil {
		return ports.AgentSessionRecord{}, err
	}
	if len(hash) != len(r.TokenHash) {
		return ports.AgentSessionRecord{}, fmt.Errorf("agent session %s has a malformed token hash", r.ID)
	}
	copy(r.TokenHash[:], hash)
	r.CreatedAt = time.UnixMicro(created).UTC()
	r.ExpiresAt = time.UnixMicro(expires).UTC()
	if rootPID.Valid && rootStart.Valid {
		r.Root = ports.ProcessRef{PID: int(rootPID.Int64), Start: time.UnixMicro(rootStart.Int64)}
	}
	return r, nil
}
