-- A grant pins either an executable (kind 'exe') or an HTTPS target berth-proxy writes one header
-- to (kind 'proxy'). SQLite cannot add CHECKs to a table, so grants is rebuilt with its ids, and
-- the autoincrement counter is carried over so a new id is never one an old grant entity used.
CREATE TABLE grants_new (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    secret_name           TEXT NOT NULL REFERENCES secrets (name),
    kind                  TEXT NOT NULL DEFAULT 'exe' CHECK (kind IN ('exe', 'proxy')),
    exe_path              TEXT NOT NULL DEFAULT '',
    sha256                TEXT NOT NULL DEFAULT '',
    team_id               TEXT NOT NULL DEFAULT '',
    signing_id            TEXT NOT NULL DEFAULT '',
    args_glob             TEXT NOT NULL DEFAULT '',
    pass_env              TEXT NOT NULL DEFAULT '',
    -- Proxy targets; methods is comma separated, since a method token cannot contain a comma.
    proxy_host            TEXT NOT NULL DEFAULT '' CHECK (length(proxy_host) <= 253),
    proxy_port            INTEGER NOT NULL DEFAULT 0,
    proxy_header          TEXT NOT NULL DEFAULT '' CHECK (length(proxy_header) <= 64),
    proxy_value_prefix    TEXT NOT NULL DEFAULT '' CHECK (length(proxy_value_prefix) <= 128),
    proxy_value_suffix    TEXT NOT NULL DEFAULT '' CHECK (length(proxy_value_suffix) <= 128),
    proxy_path_glob       TEXT NOT NULL DEFAULT '' CHECK (length(proxy_path_glob) <= 1024),
    proxy_methods         TEXT NOT NULL DEFAULT '',
    proxy_placeholder_env TEXT NOT NULL DEFAULT '' CHECK (length(proxy_placeholder_env) <= 64),
    created_at            INTEGER NOT NULL,
    last_used_at          INTEGER,
    use_count             INTEGER NOT NULL DEFAULT 0,
    CHECK (kind <> 'exe' OR (
        exe_path <> '' AND length(sha256) = 64
        AND proxy_host = '' AND proxy_port = 0 AND proxy_header = '' AND proxy_value_prefix = ''
        AND proxy_value_suffix = '' AND proxy_path_glob = '' AND proxy_methods = '' AND proxy_placeholder_env = ''
    )),
    CHECK (kind <> 'proxy' OR (
        proxy_host <> '' AND proxy_port BETWEEN 1 AND 65535 AND proxy_header <> ''
        AND exe_path = '' AND sha256 = '' AND team_id = '' AND signing_id = '' AND args_glob = '' AND pass_env = ''
    ))
) STRICT;

INSERT INTO grants_new (id, secret_name, kind, exe_path, sha256, team_id, signing_id, args_glob, pass_env, created_at, last_used_at, use_count)
SELECT id, secret_name, 'exe', exe_path, sha256, team_id, signing_id, args_glob, pass_env, created_at, last_used_at, use_count
FROM grants;

DELETE FROM sqlite_sequence WHERE name = 'grants_new';
INSERT INTO sqlite_sequence (name, seq) SELECT 'grants_new', seq FROM sqlite_sequence WHERE name = 'grants';

DROP TABLE grants;
ALTER TABLE grants_new RENAME TO grants;

CREATE INDEX grants_by_consumer ON grants (secret_name, exe_path);
CREATE INDEX grants_by_target ON grants (secret_name, proxy_host) WHERE kind = 'proxy';
