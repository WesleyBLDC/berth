// Package sqlite stores state machines, their audit trail and the request log in one SQLite file.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"berth/internal/core/ports"

	_ "modernc.org/sqlite" // registers the pure Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	db *sql.DB
}

var (
	_ ports.Store      = (*Store)(nil)
	_ ports.RequestLog = (*Store)(nil)
	_ ports.EventLog   = (*Store)(nil)
)

// Open opens or creates the database and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	// synchronous=FULL: a transition reported as saved survives power loss.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SaveTransition writes the new state and the event row in one transaction, so a crash can
// never leave one without the other. A stale version is rejected with ErrVersionConflict.
func (s *Store) SaveTransition(ctx context.Context, rec ports.TransitionRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	at := rec.At.UnixMicro()
	var res sql.Result
	if rec.Version == 1 {
		res, err = tx.ExecContext(ctx,
			`INSERT INTO machine_state (entity, kind, state, version, updated_at) VALUES (?, ?, ?, 1, ?)
			 ON CONFLICT (entity) DO NOTHING`,
			rec.Entity, rec.Kind, rec.To, at)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE machine_state SET state = ?, version = ?, updated_at = ? WHERE entity = ? AND version = ?`,
			rec.To, rec.Version, at, rec.Entity, rec.Version-1)
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ports.ErrVersionConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO machine_event (entity, kind, version, event, from_state, to_state, detail, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Entity, rec.Kind, rec.Version, rec.Event, rec.From, rec.To, rec.Detail, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadState(ctx context.Context, entity string) (ports.EntityState, bool, error) {
	st := ports.EntityState{Entity: entity}
	err := s.db.QueryRowContext(ctx, `SELECT kind, state, version FROM machine_state WHERE entity = ?`, entity).
		Scan(&st.Kind, &st.State, &st.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false, nil
	}
	return st, err == nil, err
}

func (s *Store) EntitiesOfKind(ctx context.Context, kind string) ([]ports.EntityState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT entity, state, version FROM machine_state WHERE kind = ? ORDER BY entity`, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.EntityState
	for rows.Next() {
		st := ports.EntityState{Kind: kind}
		if err := rows.Scan(&st.Entity, &st.State, &st.Version); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// History returns the newest transitions first.
func (s *Store) History(ctx context.Context, entity string, limit int) ([]ports.TransitionRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, version, event, from_state, to_state, detail, at FROM machine_event
		 WHERE entity = ? ORDER BY version DESC LIMIT ?`, entity, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.TransitionRecord
	for rows.Next() {
		rec := ports.TransitionRecord{Entity: entity}
		var at int64
		if err := rows.Scan(&rec.Kind, &rec.Version, &rec.Event, &rec.From, &rec.To, &rec.Detail, &at); err != nil {
			return nil, err
		}
		rec.At = time.UnixMicro(at).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) LastEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(id) FROM machine_event`).Scan(&id)
	return id.Int64, err
}

// EventsAfter walks the primary key from after, so it needs no index of its own.
func (s *Store) EventsAfter(ctx context.Context, kind string, after int64, limit int) ([]ports.EventRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, entity, version, event, from_state, to_state, detail, at FROM machine_event
		 WHERE id > ? AND kind = ? ORDER BY id LIMIT ?`, after, kind, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ports.EventRecord
	for rows.Next() {
		rec := ports.EventRecord{TransitionRecord: ports.TransitionRecord{Kind: kind}}
		var at int64
		if err := rows.Scan(&rec.ID, &rec.Entity, &rec.Version, &rec.Event, &rec.From, &rec.To, &rec.Detail, &at); err != nil {
			return nil, err
		}
		rec.At = time.UnixMicro(at).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) Recall(ctx context.Context, principal, requestID string) (string, []byte, bool, error) {
	var method string
	var response []byte
	err := s.db.QueryRowContext(ctx, `SELECT method, response FROM request_log WHERE principal = ? AND request_id = ?`, principal, requestID).
		Scan(&method, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	return method, response, err == nil, err
}

func (s *Store) Remember(ctx context.Context, principal, requestID, method string, response []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO request_log (principal, request_id, method, response, at) VALUES (?, ?, ?, ?, ?)`,
		principal, requestID, method, response, at.UnixMicro())
	return err
}
