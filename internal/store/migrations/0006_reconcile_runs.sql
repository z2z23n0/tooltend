ALTER TABLE notifications ADD COLUMN message TEXT NOT NULL DEFAULT '';

CREATE TABLE reconcile_runs (
    id TEXT PRIMARY KEY,
    reason TEXT NOT NULL CHECK (reason IN ('scheduled','kick','command')),
    status TEXT NOT NULL CHECK (status IN ('running','succeeded','incomplete','failed')),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    error_code TEXT NOT NULL DEFAULT '',
    summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(summary_json))
);

CREATE INDEX idx_reconcile_runs_started ON reconcile_runs(started_at DESC);
