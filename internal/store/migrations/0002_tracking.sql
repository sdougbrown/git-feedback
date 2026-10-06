-- 0002_tracking.sql adds Stage 4's durable coordination tables: per-stream
-- cadence, attempt history, persisted rate gates, the complete HTTP cache,
-- and per-lease pacing metadata. It is additive; Stage 3 tables and their
-- contents are untouched.

CREATE TABLE attempts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    stream_id     INTEGER REFERENCES targets(stream_id),
    outcome       TEXT NOT NULL,
    error_code    TEXT,
    complete      INTEGER NOT NULL DEFAULT 0,
    observed_head TEXT,
    expected_head TEXT,
    at            TEXT NOT NULL,
    next_due      TEXT
);

CREATE INDEX attempts_stream_idx ON attempts (stream_id, id);

CREATE TABLE schedule (
    stream_id INTEGER PRIMARY KEY REFERENCES targets(stream_id),
    next_due  TEXT NOT NULL
);

-- Empty account is the bootstrap/host-wide scope. The reserved `secondary`
-- resource records account-wide secondary backoff.
CREATE TABLE rate_gates (
    host       TEXT NOT NULL,
    account    TEXT NOT NULL,
    resource   TEXT NOT NULL,
    remaining  INTEGER,
    rate_limit INTEGER,
    reset_ms   INTEGER,
    until_ms   INTEGER,
    PRIMARY KEY (host, account, resource)
);

CREATE TABLE http_cache (
    host           TEXT NOT NULL,
    account        TEXT NOT NULL,
    url            TEXT NOT NULL,
    representation TEXT NOT NULL,
    etag           TEXT NOT NULL,
    body           BLOB NOT NULL,
    link           TEXT,
    has_link       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (host, account, url, representation)
);

-- pace_ms is the last-request time for the scope; it persists across lease
-- release so restarts cannot evade request spacing.
ALTER TABLE leases ADD COLUMN pace_ms INTEGER;
