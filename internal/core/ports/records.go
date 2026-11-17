package ports

import (
	"context"
	"time"
)

// SecretRecord is a secret's name row; its state lives in the secret machine.
type SecretRecord struct {
	Name      string
	CreatedAt time.Time
}

// GrantRecord is the pinned consumer behind one grant entity; its state lives in the grant machine.
type GrantRecord struct {
	ID        int64
	Secret    string
	Exe       Executable
	ArgsGlob  string
	CreatedAt time.Time
	// LastUsedAt is zero when the grant was never used.
	LastUsedAt time.Time
	UseCount   int64
}

// SecretRecords keeps secret names and grant facts, never a value.
type SecretRecords interface {
	// AddSecret records a name; a name already present keeps its first created time.
	AddSecret(ctx context.Context, name string, at time.Time) error
	Secrets(ctx context.Context) ([]SecretRecord, error)
	// AddGrant stores g without its ID, use count or last use, and returns the new ID.
	AddGrant(ctx context.Context, g GrantRecord) (int64, error)
	// Grants lists a secret's grants, newest first.
	Grants(ctx context.Context, secret string) ([]GrantRecord, error)
	CountUse(ctx context.Context, id int64, at time.Time) error
}
