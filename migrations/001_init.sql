-- migrations/001_init.sql
-- Idempotent schema initialization for the Concurrent Health Monitor.

CREATE TABLE IF NOT EXISTS targets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS checks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    target_id UUID NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    status_code INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    error_message TEXT,
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Serve RecentStats (WHERE target_id = $1 AND checked_at >= $2) and the FK
-- cascade delete with an index instead of scanning every historical row as
-- the checks table grows. Postgres does not index FK columns automatically.
CREATE INDEX IF NOT EXISTS checks_target_id_checked_at_idx
    ON checks (target_id, checked_at DESC);
