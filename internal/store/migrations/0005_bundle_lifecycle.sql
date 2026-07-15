CREATE TABLE bundles (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    recipe_id TEXT NOT NULL,
    recipe_version TEXT NOT NULL,
    recipe_source TEXT NOT NULL CHECK (recipe_source IN ('builtin','local','fallback')),
    lifecycle_owner TEXT NOT NULL CHECK (lifecycle_owner IN ('tooltend','delegated','host-owned','app-owned','workspace-linked','unresolved')),
    config_state TEXT NOT NULL DEFAULT 'unconfigured' CHECK (config_state IN ('unconfigured','configured')),
    confidence TEXT NOT NULL CHECK (confidence IN ('high','medium','low','unresolved')),
    current_release_id TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    discovered_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);

CREATE TABLE bundle_releases (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    version TEXT NOT NULL,
    resolved_ref TEXT NOT NULL DEFAULT '',
    manifest_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(manifest_json)),
    status TEXT NOT NULL CHECK (status IN ('observed','resolved','staged','active','superseded','failed')),
    created_at TEXT NOT NULL,
    UNIQUE(bundle_id, version, resolved_ref)
);

CREATE TABLE bundle_artifacts (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    release_id TEXT REFERENCES bundle_releases(id) ON DELETE SET NULL,
    recipe_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('cli','skill','hook','app','config','embedded_binary','plugin','mcp')),
    name TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    required INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0,1)),
    driver TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    UNIQUE(bundle_id, recipe_key)
);

CREATE TABLE installations (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    artifact_id TEXT REFERENCES bundle_artifacts(id) ON DELETE SET NULL,
    driver TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    package_identity TEXT NOT NULL DEFAULT '',
    source_identity TEXT NOT NULL DEFAULT '',
    observed_version TEXT NOT NULL DEFAULT '',
    observed_hash TEXT NOT NULL DEFAULT '',
    lifecycle_owner TEXT NOT NULL CHECK (lifecycle_owner IN ('tooltend','delegated','host-owned','app-owned','workspace-linked','unresolved')),
    managed INTEGER NOT NULL DEFAULT 0 CHECK (managed IN (0,1)),
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    last_seen_at TEXT NOT NULL,
    UNIQUE(driver, normalized_path, package_identity, source_identity)
);

CREATE TABLE consumer_bindings (
    id TEXT PRIMARY KEY,
    installation_id TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    binding_id TEXT REFERENCES bindings(id) ON DELETE SET NULL,
    host TEXT NOT NULL CHECK (host IN ('codex','claude','system')),
    project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('global','project')),
    config_path TEXT NOT NULL DEFAULT '',
    config_pointer TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT NOT NULL,
    UNIQUE(installation_id, host, project_id, config_path, config_pointer)
);

CREATE TABLE bundle_policies (
    bundle_id TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('auto','manual','observe','ignore')),
    recipe_trusted INTEGER NOT NULL DEFAULT 0 CHECK (recipe_trusted IN (0,1)),
    updated_at TEXT NOT NULL
);

CREATE TABLE bundle_transactions (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE RESTRICT,
    from_release_id TEXT REFERENCES bundle_releases(id) ON DELETE SET NULL,
    to_release_id TEXT REFERENCES bundle_releases(id) ON DELETE SET NULL,
    status TEXT NOT NULL CHECK (status IN ('prepared','staging','activating','committed','rolling_back','rolled_back','failed')),
    stage_only INTEGER NOT NULL DEFAULT 0 CHECK (stage_only IN (0,1)),
    error_code TEXT NOT NULL DEFAULT '',
    error_summary TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
);

CREATE TABLE bundle_transaction_steps (
    id TEXT PRIMARY KEY,
    transaction_id TEXT NOT NULL REFERENCES bundle_transactions(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    artifact_id TEXT REFERENCES bundle_artifacts(id) ON DELETE SET NULL,
    installation_id TEXT REFERENCES installations(id) ON DELETE SET NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','staged','activating','activated','healthy','compensating','compensated','failed')),
    command_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(command_json)),
    rollback_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(rollback_json)),
    before_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(before_json)),
    after_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(after_json)),
    error_code TEXT NOT NULL DEFAULT '',
    error_summary TEXT NOT NULL DEFAULT '',
    started_at TEXT,
    completed_at TEXT,
    UNIQUE(transaction_id, ordinal)
);

CREATE TABLE bundle_receipts (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    transaction_id TEXT REFERENCES bundle_transactions(id) ON DELETE SET NULL,
    release_id TEXT REFERENCES bundle_releases(id) ON DELETE SET NULL,
    action TEXT NOT NULL CHECK (action IN ('update','rollback','observe')),
    status TEXT NOT NULL CHECK (status IN ('succeeded','rolled_back','failed')),
    summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(summary_json)),
    created_at TEXT NOT NULL
);

CREATE TABLE bundle_health_checks (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    artifact_id TEXT REFERENCES bundle_artifacts(id) ON DELETE SET NULL,
    installation_id TEXT REFERENCES installations(id) ON DELETE SET NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('healthy','warning','failed','unknown')),
    summary TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL
);

CREATE TABLE bundle_tasks (
    id TEXT PRIMARY KEY,
    bundle_id TEXT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    installation_id TEXT REFERENCES installations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
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

CREATE INDEX idx_bundles_state_owner ON bundles(config_state, lifecycle_owner);
CREATE INDEX idx_bundle_artifacts_bundle ON bundle_artifacts(bundle_id, ordinal);
CREATE INDEX idx_installations_bundle ON installations(bundle_id);
CREATE INDEX idx_consumer_bindings_installation ON consumer_bindings(installation_id);
CREATE INDEX idx_bundle_transactions_unfinished ON bundle_transactions(status) WHERE status IN ('prepared','staging','activating','rolling_back');
CREATE INDEX idx_bundle_receipts_created ON bundle_receipts(bundle_id, created_at DESC);
CREATE INDEX idx_bundle_health_latest ON bundle_health_checks(bundle_id, checked_at DESC);
CREATE INDEX idx_bundle_tasks_due ON bundle_tasks(status, next_attempt_at);
