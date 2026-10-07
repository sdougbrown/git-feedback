-- 0001_init.sql establishes the Stage 3 core tables. Stage 3 owns this file;
-- Stage 4 adds 0002_tracking.sql additively. Schema version bookkeeping lives
-- in the Go migration runner (single-row schema_version table).

CREATE TABLE targets (
    stream_id           INTEGER PRIMARY KEY,
    target_id           TEXT NOT NULL,
    account             TEXT NOT NULL,
    host                TEXT NOT NULL,
    target_json         TEXT NOT NULL,
    current_snapshot_id INTEGER,
    UNIQUE (target_id, account)
);

CREATE TABLE snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    stream_id       INTEGER NOT NULL REFERENCES targets(stream_id),
    head            TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    body            TEXT NOT NULL,
    collected_start TEXT NOT NULL,
    collected_end   TEXT NOT NULL
);

CREATE TABLE objects (
    stream_id        INTEGER NOT NULL REFERENCES targets(stream_id),
    kind             TEXT NOT NULL,
    provider_id      TEXT NOT NULL,
    counter          INTEGER NOT NULL,
    present          INTEGER NOT NULL,
    last_fingerprint TEXT NOT NULL,
    PRIMARY KEY (stream_id, kind, provider_id)
);

CREATE TABLE revisions (
    stream_id   INTEGER NOT NULL REFERENCES targets(stream_id),
    kind        TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    counter     INTEGER NOT NULL,
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    PRIMARY KEY (stream_id, kind, provider_id, counter)
);

CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    stream_id   INTEGER NOT NULL REFERENCES targets(stream_id),
    kind        TEXT NOT NULL,
    object_kind TEXT NOT NULL,
    object_id   TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    url         TEXT NOT NULL,
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    observed_at TEXT NOT NULL,
    head_before TEXT,
    head_after  TEXT,
    UNIQUE (stream_id, object_kind, object_id, revision)
);

CREATE INDEX events_stream_idx ON events (stream_id, id);

CREATE TABLE acks (
    consumer  TEXT NOT NULL,
    stream_id INTEGER NOT NULL REFERENCES targets(stream_id),
    event_id  INTEGER NOT NULL REFERENCES events(id),
    PRIMARY KEY (consumer, stream_id, event_id)
);

CREATE INDEX acks_stream_event_idx ON acks (stream_id, event_id);

CREATE TABLE observations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    stream_id   INTEGER NOT NULL REFERENCES targets(stream_id),
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    observed_at TEXT NOT NULL
);

CREATE TABLE leases (
    host      TEXT NOT NULL,
    account   TEXT NOT NULL,
    token     TEXT,
    expiry_ms INTEGER,
    PRIMARY KEY (host, account)
);
