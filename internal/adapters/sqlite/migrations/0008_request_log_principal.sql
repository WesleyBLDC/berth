-- A stored reply belongs to the caller that made the call: a request id is unique only within one
-- principal (the socket, the peer uid and the process), so one caller can never replay another's
-- reply. Rows from before this migration get a principal no caller has, which only costs a retry
-- that straddles the upgrade its replay.
CREATE TABLE request_log_new (
    principal  TEXT NOT NULL,
    request_id TEXT NOT NULL,
    method     TEXT NOT NULL,
    response   BLOB NOT NULL,
    at         INTEGER NOT NULL,
    PRIMARY KEY (principal, request_id)
) STRICT;

INSERT INTO request_log_new (principal, request_id, method, response, at)
SELECT 'legacy', request_id, method, response, at FROM request_log;

DROP TABLE request_log;
ALTER TABLE request_log_new RENAME TO request_log;
