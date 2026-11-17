-- One row per agent session. Only the token's SHA-256 is kept, never the token.
-- State lives in machine_state under the entity agent/<id>. The root columns stay empty until
-- the first accepted call binds the session to a process tree, and are written only once.
CREATE TABLE agent_sessions (
    id         TEXT PRIMARY KEY CHECK (length(id) = 32),
    name       TEXT NOT NULL,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    root_pid   INTEGER CHECK (root_pid > 0),
    root_start INTEGER,
    CHECK ((root_pid IS NULL) = (root_start IS NULL))
) STRICT;
