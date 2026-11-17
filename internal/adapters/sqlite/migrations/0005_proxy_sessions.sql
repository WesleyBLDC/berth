-- One row per berth-proxy run. Counts only: never paths, request details or values.
-- State lives in machine_state under the entity proxy/<id>.
CREATE TABLE proxy_sessions (
    id         TEXT PRIMARY KEY CHECK (length(id) = 32),
    proxy_pid  INTEGER NOT NULL CHECK (proxy_pid > 0),
    created_at INTEGER NOT NULL,
    ended_at   INTEGER,
    injected   INTEGER NOT NULL DEFAULT 0 CHECK (injected >= 0),
    refused    INTEGER NOT NULL DEFAULT 0 CHECK (refused >= 0)
) STRICT;

CREATE TABLE proxy_session_grants (
    session_id TEXT NOT NULL REFERENCES proxy_sessions (id),
    grant_id   INTEGER NOT NULL REFERENCES grants (id),
    PRIMARY KEY (session_id, grant_id)
) STRICT;

CREATE INDEX proxy_session_grants_by_grant ON proxy_session_grants (grant_id);
