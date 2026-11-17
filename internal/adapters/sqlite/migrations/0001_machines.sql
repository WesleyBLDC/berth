CREATE TABLE machine_state (
    entity     TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    state      TEXT NOT NULL,
    version    INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE machine_event (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    entity     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    version    INTEGER NOT NULL,
    event      TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    at         INTEGER NOT NULL,
    UNIQUE (entity, version)
) STRICT;

CREATE TABLE request_log (
    request_id TEXT PRIMARY KEY,
    method     TEXT NOT NULL,
    response   BLOB NOT NULL,
    at         INTEGER NOT NULL
) STRICT;
