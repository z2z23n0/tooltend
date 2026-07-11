CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    root_path TEXT NOT NULL UNIQUE,
    root_fingerprint TEXT NOT NULL,
    selected INTEGER NOT NULL DEFAULT 0 CHECK (selected IN (0, 1)),
    discovered_via TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT NOT NULL
);

CREATE TABLE sources (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('git','npm','pypi','homebrew','http','local','unknown')),
    locator TEXT NOT NULL,
    subdir TEXT NOT NULL DEFAULT '',
    package_name TEXT NOT NULL DEFAULT '',
    identity_hash TEXT NOT NULL UNIQUE,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE source_trust (
    source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
    level TEXT NOT NULL CHECK (level IN ('unknown','local','verified','pinned')),
    approved_by TEXT NOT NULL DEFAULT '',
    approved_at TEXT NOT NULL
);

CREATE TABLE components (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('skill','plugin','hook','stdio_mcp','http_mcp','cli')),
    name TEXT NOT NULL,
    source_id TEXT REFERENCES sources(id) ON DELETE SET NULL,
    logical_key TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE bindings (
    id TEXT PRIMARY KEY,
    component_id TEXT NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    host TEXT NOT NULL CHECK (host IN ('codex','claude','system')),
    project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('global','project')),
    install_path TEXT NOT NULL,
    install_method TEXT NOT NULL DEFAULT '',
    managed INTEGER NOT NULL DEFAULT 0 CHECK (managed IN (0, 1)),
    classification TEXT NOT NULL CHECK (classification IN ('clean','customized','detached','unknown')),
    observed_hash TEXT NOT NULL DEFAULT '',
    observed_version TEXT NOT NULL DEFAULT '',
    trust_hash TEXT NOT NULL DEFAULT '',
    active_generation_id TEXT,
    last_seen_at TEXT NOT NULL,
    UNIQUE(host, install_path),
    CHECK ((scope = 'project' AND project_id IS NOT NULL) OR (scope = 'global' AND project_id IS NULL))
);

CREATE TABLE policies (
    binding_id TEXT PRIMARY KEY REFERENCES bindings(id) ON DELETE CASCADE,
    track_channel TEXT NOT NULL CHECK (track_channel IN ('stable','latest','main','semver','exact')),
    constraint_text TEXT NOT NULL DEFAULT '',
    apply_mode TEXT NOT NULL CHECK (apply_mode IN ('auto','manual','ignore')),
    notify_mode TEXT NOT NULL CHECK (notify_mode IN ('all','failures','none')),
    local_cap_mode TEXT NOT NULL CHECK (local_cap_mode IN ('auto','manual','ignore')),
    updated_at TEXT NOT NULL
);

CREATE TABLE objects (
    hash TEXT PRIMARY KEY CHECK (length(hash) = 64),
    kind TEXT NOT NULL CHECK (kind IN ('blob','tree','bundle')),
    size INTEGER NOT NULL CHECK (size >= 0),
    relative_path TEXT NOT NULL,
    verified_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE baselines (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    resolved_ref TEXT NOT NULL,
    tree_hash TEXT NOT NULL REFERENCES objects(hash) ON DELETE RESTRICT,
    created_at TEXT NOT NULL
);

CREATE TABLE overlays (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    baseline_id TEXT NOT NULL REFERENCES baselines(id) ON DELETE CASCADE,
    tree_hash TEXT NOT NULL REFERENCES objects(hash) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (status IN ('clean','customized','conflict')),
    created_at TEXT NOT NULL
);

CREATE TABLE dependencies (
    id TEXT PRIMARY KEY,
    from_component_id TEXT NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    to_component_id TEXT REFERENCES components(id) ON DELETE SET NULL,
    package_identity TEXT NOT NULL,
    constraint_text TEXT NOT NULL DEFAULT '',
    evidence_path TEXT NOT NULL,
    evidence_line INTEGER NOT NULL DEFAULT 0 CHECK (evidence_line >= 0),
    explicit INTEGER NOT NULL DEFAULT 1 CHECK (explicit IN (0, 1))
);

CREATE TABLE generations (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    candidate_id TEXT,
    resolved_ref TEXT NOT NULL,
    tree_hash TEXT NOT NULL REFERENCES objects(hash) ON DELETE RESTRICT,
    integrity_hash TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('prepared','active','inactive','failed','original')),
    created_at TEXT NOT NULL,
    activated_at TEXT
);

CREATE TABLE candidates (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    resolved_ref TEXT NOT NULL,
    upstream_tree_hash TEXT REFERENCES objects(hash) ON DELETE RESTRICT,
    baseline_id TEXT REFERENCES baselines(id) ON DELETE SET NULL,
    overlay_id TEXT REFERENCES overlays(id) ON DELETE SET NULL,
    merged_tree_hash TEXT REFERENCES objects(hash) ON DELETE RESTRICT,
    candidate_hash TEXT NOT NULL UNIQUE CHECK (length(candidate_hash) = 64),
    status TEXT NOT NULL CHECK (status IN ('available','staging','verified','merging','validating','needs_review','ready','activating','active','rolled_back','failed','superseded')),
    review_class TEXT NOT NULL DEFAULT 'none' CHECK (review_class IN ('none','semantic_skill_only','human_required')),
    failure_code TEXT NOT NULL DEFAULT '',
    failure_summary TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE review_bundles (
    id TEXT PRIMARY KEY,
    candidate_id TEXT NOT NULL UNIQUE REFERENCES candidates(id) ON DELETE CASCADE,
    candidate_hash TEXT NOT NULL CHECK (length(candidate_hash) = 64),
    object_hash TEXT NOT NULL REFERENCES objects(hash) ON DELETE RESTRICT,
    risk_types_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(risk_types_json)),
    created_at TEXT NOT NULL
);

CREATE TABLE reviews (
    id TEXT PRIMARY KEY,
    candidate_id TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
    candidate_hash TEXT NOT NULL CHECK (length(candidate_hash) = 64),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('human','agent')),
    verdict TEXT NOT NULL CHECK (verdict IN ('safe','conflict','uncertain')),
    risk_type TEXT NOT NULL,
    summary TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE activation_intents (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    candidate_id TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
    old_generation_id TEXT REFERENCES generations(id) ON DELETE RESTRICT,
    new_generation_id TEXT NOT NULL REFERENCES generations(id) ON DELETE RESTRICT,
    expected_pointer TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('prepared','pointer_switched','committed','rolled_back')),
    error_code TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE receipts (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES bindings(id) ON DELETE CASCADE,
    candidate_id TEXT REFERENCES candidates(id) ON DELETE SET NULL,
    action TEXT NOT NULL CHECK (action IN ('update','rollback','adopt')),
    old_generation_id TEXT REFERENCES generations(id) ON DELETE SET NULL,
    new_generation_id TEXT REFERENCES generations(id) ON DELETE SET NULL,
    from_ref TEXT NOT NULL DEFAULT '',
    to_ref TEXT NOT NULL DEFAULT '',
    candidate_hash TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('succeeded','rolled_back','failed')),
    summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(summary_json)),
    created_at TEXT NOT NULL
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    binding_id TEXT REFERENCES bindings(id) ON DELETE CASCADE,
    candidate_id TEXT REFERENCES candidates(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT NOT NULL,
    lease_until TEXT,
    error_code TEXT NOT NULL DEFAULT '',
    error_summary TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE hook_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at TEXT NOT NULL,
    host TEXT NOT NULL CHECK (host IN ('codex','claude','system')),
    event_type TEXT NOT NULL,
    project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    installer TEXT NOT NULL DEFAULT '',
    package_identity TEXT NOT NULL DEFAULT '',
    requested_version TEXT NOT NULL DEFAULT '',
    correlation_hash TEXT UNIQUE,
    processed_at TEXT
);

CREATE TABLE notifications (
    candidate_hash TEXT NOT NULL CHECK (length(candidate_hash) = 64),
    kind TEXT NOT NULL,
    queued_at TEXT NOT NULL,
    shown_at TEXT,
    PRIMARY KEY(candidate_hash, kind)
);

CREATE TABLE scans (
    id TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed')),
    started_at TEXT NOT NULL,
    finished_at TEXT
);

CREATE INDEX idx_components_source ON components(source_id);
CREATE INDEX idx_bindings_component ON bindings(component_id);
CREATE INDEX idx_bindings_project ON bindings(project_id);
CREATE INDEX idx_baselines_binding_created ON baselines(binding_id, created_at DESC);
CREATE INDEX idx_overlays_binding_created ON overlays(binding_id, created_at DESC);
CREATE INDEX idx_dependencies_from ON dependencies(from_component_id);
CREATE INDEX idx_generations_binding_state ON generations(binding_id, state);
CREATE INDEX idx_candidates_binding_status ON candidates(binding_id, status);
CREATE INDEX idx_reviews_candidate_created ON reviews(candidate_id, created_at DESC);
CREATE INDEX idx_activation_unfinished ON activation_intents(phase) WHERE phase IN ('prepared','pointer_switched');
CREATE INDEX idx_receipts_binding_created ON receipts(binding_id, created_at DESC);
CREATE INDEX idx_tasks_due ON tasks(status, next_attempt_at);
CREATE INDEX idx_hook_events_unprocessed ON hook_events(processed_at, occurred_at);
CREATE INDEX idx_notifications_pending ON notifications(shown_at, queued_at);
