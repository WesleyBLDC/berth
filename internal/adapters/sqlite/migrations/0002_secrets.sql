-- Values never live here: only names and the facts that pin a consumer.
-- State lives in machine_state under the entities secret/<name> and grant/<id>.
CREATE TABLE secrets (
    name       TEXT PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 128),
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE grants (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    secret_name  TEXT NOT NULL REFERENCES secrets (name),
    exe_path     TEXT NOT NULL,
    sha256       TEXT NOT NULL CHECK (length(sha256) = 64),
    team_id      TEXT NOT NULL DEFAULT '',
    signing_id   TEXT NOT NULL DEFAULT '',
    args_glob    TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    use_count    INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX grants_by_consumer ON grants (secret_name, exe_path);
