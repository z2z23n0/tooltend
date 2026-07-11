CREATE TABLE adoption_intents (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('file','hook','runtime_cli','runtime_stdio')),
    plan_json TEXT NOT NULL CHECK (json_valid(plan_json)),
    plan_hash TEXT NOT NULL CHECK (length(plan_hash) = 64),
    phase TEXT NOT NULL CHECK (phase IN ('prepared','switched','finalizing','committed','rolled_back','blocked')),
    error_code TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_adoption_unfinished_binding
ON adoption_intents(binding_id)
WHERE phase IN ('prepared','switched','blocked');

CREATE INDEX idx_adoption_recovery
ON adoption_intents(phase,started_at)
WHERE phase IN ('prepared','switched','blocked');

CREATE UNIQUE INDEX idx_activation_unfinished_binding
ON activation_intents(binding_id)
WHERE phase IN ('prepared','pointer_switched');
