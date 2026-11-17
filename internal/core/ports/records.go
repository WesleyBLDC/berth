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

// ConsumerKind is what a grant pins; the values are stored data in grants.kind.
type ConsumerKind string

const (
	ConsumerExe   ConsumerKind = "exe"
	ConsumerProxy ConsumerKind = "proxy"
)

// ProxyTarget is the one header on one HTTPS host and port that berth-proxy may write a value to.
type ProxyTarget struct {
	// Host is a lowercase DNS name.
	Host string
	// Port is 1 to 65535; the API turns 0 into 443 before it reaches the core.
	Port        int
	HeaderName  string
	ValuePrefix string
	ValueSuffix string
	// PathGlob is empty when any path is allowed.
	PathGlob string
	// Methods is empty when any method is allowed.
	Methods []string
	// PlaceholderEnv is empty when the child gets no placeholder variable.
	PlaceholderEnv string
}

// GrantRecord is the pinned consumer behind one grant entity; its state lives in the grant machine.
// Exe, ArgsGlob and PassEnv are set only for ConsumerExe, Proxy only for ConsumerProxy.
type GrantRecord struct {
	ID     int64
	Secret string
	// Kind is ConsumerExe or ConsumerProxy; an empty Kind is read as ConsumerExe.
	Kind     ConsumerKind
	Exe      Executable
	Proxy    ProxyTarget
	ArgsGlob string
	// PassEnv names the environment variables berth run may pass beyond the allowlist.
	PassEnv   []string
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

// ProxySessionRecord is one berth-proxy run; its state lives in the proxy session machine.
// It holds counts only, never paths, request details or values.
type ProxySessionRecord struct {
	// ID is 32 lowercase hex characters; the entity is proxy/<ID>.
	ID string
	// PID is the berth-proxy process that authorized the session.
	PID int
	// Grants are the proxy grants the session holds tickets for.
	Grants    []int64
	CreatedAt time.Time
	// EndedAt is zero until the session is Ended or Lost.
	EndedAt  time.Time
	Injected int64
	Refused  int64
}

// ProxySessionRecords keeps proxy session facts.
type ProxySessionRecords interface {
	// AddProxySession stores r with its grants; EndedAt and the counts are ignored.
	AddProxySession(ctx context.Context, r ProxySessionRecord) error
	ProxySession(ctx context.Context, id string) (ProxySessionRecord, bool, error)
	// ProxySessionsForGrant lists the ids of sessions holding the grant that have not ended.
	ProxySessionsForGrant(ctx context.Context, grantID int64) ([]string, error)
	// OpenProxySessions lists the ids of sessions that have not ended, for boot recovery.
	OpenProxySessions(ctx context.Context) ([]string, error)
	// EndProxySession records the counts and the end time once; a second call is an error.
	EndProxySession(ctx context.Context, id string, injected, refused int64, at time.Time) error
}
