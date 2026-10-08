-- 0004_snapshots_stream_idx.sql indexes snapshots by stream so the
-- prior-thread lookup walks one stream's history newest-first instead of
-- scanning the table across all streams.
CREATE INDEX IF NOT EXISTS snapshots_stream_idx ON snapshots (stream_id, id DESC);