-- Schema of a gortex graph store, captured by TestHelperCaptureUpgradeSchema
-- from a store written by a schema v43 build indexing this repository at v0.64.10.
-- user_version: 43

CREATE TABLE nodes (
    id            TEXT NOT NULL,
    view_gen      INTEGER NOT NULL DEFAULT 0,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    qual_name     TEXT NOT NULL DEFAULT '',
    file_path     TEXT NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    start_column  INTEGER NOT NULL DEFAULT 0,
    end_column    INTEGER NOT NULL DEFAULT 0,
    language      TEXT NOT NULL DEFAULT '',
    repo_prefix   TEXT NOT NULL DEFAULT '',
    workspace_id  TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    signature     TEXT,
    visibility    TEXT,
    doc           TEXT,
    external      INTEGER,
    return_type   TEXT,
    is_async      INTEGER,
    is_static     INTEGER,
    is_abstract   INTEGER,
    is_exported   INTEGER,
    updated_at    INTEGER,
    data_class    TEXT,
    clone_sig     TEXT,
    meta          BLOB, semantic_type TEXT, semantic_source TEXT, entry_point INTEGER, entry_point_kind TEXT, search_signature TEXT, search_qual_name TEXT, search_doc TEXT, search_metadata_suppressed INTEGER, section_text TEXT, is_stub INTEGER GENERATED ALWAYS AS (
    CASE WHEN
        id LIKE 'stdlib::%' OR id LIKE 'builtin::%' OR id LIKE 'external_call::%' OR id LIKE 'module::%'
        OR (
            instr(id, '::') > 0 AND (
                substr(id, instr(id, '::') + 2) LIKE 'stdlib::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'builtin::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'external_call::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'module::%'
            )
        )
    THEN 1 ELSE 0 END
) VIRTUAL, file_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN file_path = '' THEN NULL
         WHEN instr(replace(file_path, '\', '/'), '/') = 0 THEN '.'
         WHEN rtrim(rtrim(replace(file_path, '\', '/'), replace(replace(file_path, '\', '/'), '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(replace(file_path, '\', '/'), replace(replace(file_path, '\', '/'), '/', '')), '/') END
) VIRTUAL,
    PRIMARY KEY (id, view_gen)
) WITHOUT ROWID
;

CREATE TABLE edges (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    view_gen INTEGER NOT NULL DEFAULT 0,
    meta             BLOB, is_unresolved INTEGER GENERATED ALWAYS AS (
    CASE WHEN (to_id >= 'unresolved::' AND to_id < 'unresolved:;') OR to_id LIKE '%::unresolved::%' THEN 1 ELSE 0 END
) VIRTUAL, member_receiver TEXT GENERATED ALWAYS AS (
    CASE WHEN kind = 'member_of' AND instr(to_id, '::') > 1
              AND substr(to_id, length(rtrim(to_id, replace(to_id, ':', ''))) + 1) <> ''
         THEN substr(to_id, length(rtrim(to_id, replace(to_id, ':', ''))) + 1) ELSE NULL END
) VIRTUAL, member_receiver_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN kind <> 'member_of' OR instr(to_id, '::') <= 1
              OR substr(to_id, length(rtrim(to_id, replace(to_id, ':', ''))) + 1) = '' THEN NULL
         WHEN instr(replace(substr(to_id, 1, length(rtrim(to_id, replace(to_id, ':', ''))) - 2), '\', '/'), '/') = 0 THEN '.'
         WHEN rtrim(rtrim(replace(substr(to_id, 1, length(rtrim(to_id, replace(to_id, ':', ''))) - 2), '\', '/'),
                         replace(replace(substr(to_id, 1, length(rtrim(to_id, replace(to_id, ':', ''))) - 2), '\', '/'), '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(replace(substr(to_id, 1, length(rtrim(to_id, replace(to_id, ':', ''))) - 2), '\', '/'),
                         replace(replace(substr(to_id, 1, length(rtrim(to_id, replace(to_id, ':', ''))) - 2), '\', '/'), '/', '')), '/') END
) VIRTUAL, from_repo TEXT GENERATED ALWAYS AS (
    CASE WHEN instr(from_id, '/') > 1 THEN substr(from_id, 1, instr(from_id, '/') - 1) ELSE '' END
) VIRTUAL, to_repo_unresolved TEXT GENERATED ALWAYS AS (
    CASE WHEN to_id >= 'unresolved::' AND to_id < 'unresolved:;' THEN ''
         WHEN instr(to_id, '::unresolved::') > 1 THEN substr(to_id, 1, instr(to_id, '::unresolved::') - 1)
         ELSE NULL END
) VIRTUAL, resolve_terminal INTEGER, resolve_terminal_reason TEXT, semantic_source TEXT,
    UNIQUE(from_id, to_id, kind, file_path, line, view_gen)
)
;

CREATE VIRTUAL TABLE symbol_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens)
;

CREATE VIRTUAL TABLE content_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, file_path UNINDEXED, ordinal UNINDEXED, body)
;

CREATE TABLE file_index_failures (
    view_gen          INTEGER NOT NULL DEFAULT 0,
    repo_prefix       TEXT NOT NULL,
    file_path         TEXT NOT NULL,
    error             TEXT NOT NULL,
    permission_denied INTEGER NOT NULL DEFAULT 0,
    workspace_id      TEXT NOT NULL DEFAULT '',
    project_id        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix, file_path)
) WITHOUT ROWID
;

CREATE TABLE file_mtimes (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    repo_prefix TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    PRIMARY KEY (view_gen, repo_prefix, file_path)
) WITHOUT ROWID
;

CREATE TABLE repo_index_state (
    view_gen           INTEGER NOT NULL DEFAULT 0,
    repo_prefix        TEXT NOT NULL,
    indexed_sha        TEXT NOT NULL DEFAULT '',
    dirty              INTEGER NOT NULL DEFAULT 0,
    indexed_at         INTEGER NOT NULL DEFAULT 0,
    workspace_fp       TEXT NOT NULL DEFAULT '',
    node_count         INTEGER NOT NULL DEFAULT 0,
    edge_count         INTEGER NOT NULL DEFAULT 0,
    extractor_versions TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix)
) WITHOUT ROWID
;

CREATE TABLE enrichment_state (
    view_gen     INTEGER NOT NULL DEFAULT 0,
    repo_prefix  TEXT NOT NULL,
    provider     TEXT NOT NULL,
    indexed_sha  TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    coverage     REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (view_gen, repo_prefix, provider)
) WITHOUT ROWID
;

CREATE TABLE contract_state (
    view_gen       INTEGER NOT NULL DEFAULT 0,
    repo_prefix    TEXT NOT NULL,
    indexed_sha    TEXT NOT NULL DEFAULT '',
    completed_at   INTEGER NOT NULL DEFAULT 0,
    contract_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (view_gen, repo_prefix)
) WITHOUT ROWID
;

CREATE TABLE clone_shingles (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    shingles    BLOB,
    signature   TEXT,
    token_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE clone_corpus_state (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    repo_prefix TEXT NOT NULL,
    PRIMARY KEY (view_gen, repo_prefix)
) WITHOUT ROWID
;

CREATE TABLE constant_values (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    value       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE semantic_binding_types (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL,
    line        INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL DEFAULT '',
    type_name   TEXT NOT NULL,
    PRIMARY KEY (view_gen, repo_prefix, file_path, line, name)
) WITHOUT ROWID
;

CREATE TABLE files (
    view_gen     INTEGER NOT NULL DEFAULT 0,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    file_path    TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    node_count   INTEGER NOT NULL DEFAULT 0,
    errors       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix, file_path)
) WITHOUT ROWID
;

CREATE TABLE ref_facts (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    repo_prefix TEXT NOT NULL DEFAULT '',
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref_name    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    origin      TEXT NOT NULL DEFAULT '',
    tier        TEXT NOT NULL DEFAULT '',
    candidates  TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    lang        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix, from_id, to_id, kind, line)
) WITHOUT ROWID
;

CREATE TABLE vectors (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    parent_id   TEXT NOT NULL DEFAULT '',
    dims        INTEGER NOT NULL,
    vec         BLOB NOT NULL,
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE churn_enrichment (
    view_gen       INTEGER NOT NULL DEFAULT 0,
    node_id        TEXT NOT NULL,
    repo_prefix    TEXT NOT NULL DEFAULT '',
    commit_count   INTEGER NOT NULL DEFAULT 0,
    age_days       INTEGER NOT NULL DEFAULT 0,
    churn_rate     REAL NOT NULL DEFAULT 0,
    last_author    TEXT NOT NULL DEFAULT '',
    last_commit_at TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    computed_at    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE coverage_enrichment (
    view_gen     INTEGER NOT NULL DEFAULT 0,
    node_id      TEXT NOT NULL,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    coverage_pct REAL NOT NULL DEFAULT 0,
    num_stmt     INTEGER NOT NULL DEFAULT 0,
    hit          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE release_enrichment (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    added_in    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE blame_enrichment (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    ts          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE symbol_fts_state (
    view_gen      INTEGER NOT NULL DEFAULT 0,
    repo_prefix   TEXT NOT NULL,
    normalization TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix)
) WITHOUT ROWID
;

CREATE TABLE symbol_fts_rowid (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    node_id     TEXT NOT NULL,
    repo_prefix TEXT NOT NULL DEFAULT '',
    fts_rowid   INTEGER NOT NULL,
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE content_fts_rowid (
    view_gen    INTEGER NOT NULL DEFAULT 0,
    fts_rowid   INTEGER PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE generation_file_masks (
    view_gen       INTEGER NOT NULL,
    repo_prefix    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    ownership_mode TEXT NOT NULL,
    PRIMARY KEY (view_gen, repo_prefix, file_path)
) WITHOUT ROWID
;

CREATE TABLE generation_node_tombstones (
    view_gen INTEGER NOT NULL,
    node_id  TEXT NOT NULL,
    claim_kind TEXT NOT NULL DEFAULT 'legacy_tombstone',
    PRIMARY KEY (view_gen, node_id)
) WITHOUT ROWID
;

CREATE TABLE generation_edge_sources (
    view_gen       INTEGER NOT NULL,
    source_id      TEXT NOT NULL,
    ownership_mode TEXT NOT NULL,
    PRIMARY KEY (view_gen, source_id)
) WITHOUT ROWID
;

CREATE TABLE generation_producer_completeness (
    view_gen INTEGER NOT NULL,
    producer TEXT NOT NULL,
    state    TEXT NOT NULL,
    reason   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, producer)
) WITHOUT ROWID
;

CREATE TABLE generation_contract_work (
    view_gen INTEGER NOT NULL,
    token TEXT NOT NULL,
    origin_generation INTEGER NOT NULL,
    checkout_id TEXT NOT NULL,
    repo_prefix TEXT NOT NULL,
    file_path TEXT NOT NULL,
    input_version TEXT NOT NULL,
    input_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL,
    scope TEXT NOT NULL,
    PRIMARY KEY (view_gen, token)
) WITHOUT ROWID
;

CREATE TABLE generation_contract_input_state (
 view_gen INTEGER NOT NULL,
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 previous_input_version TEXT NOT NULL,
 previous_input_fingerprint TEXT NOT NULL,
 accepted INTEGER NOT NULL CHECK(accepted IN (0,1)),
 PRIMARY KEY(view_gen, repo_prefix, checkout_id)
) WITHOUT ROWID
;

CREATE TABLE generation_contract_boundary_receipt (
 view_gen INTEGER NOT NULL,repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,file_path TEXT NOT NULL,
 version TEXT NOT NULL,fingerprint TEXT NOT NULL,source_fingerprint TEXT NOT NULL,accepted INTEGER NOT NULL,
 receipt BLOB NOT NULL,
 PRIMARY KEY(view_gen,repo_prefix,checkout_id,file_path)
) WITHOUT ROWID
;

CREATE TABLE generation_contract_boundary_keys (
 view_gen INTEGER NOT NULL,key_kind TEXT NOT NULL,lookup_key TEXT NOT NULL,
 repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,file_path TEXT NOT NULL,
 PRIMARY KEY(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path)
) WITHOUT ROWID
;

CREATE TABLE generation_contract_boundary_baseline (
 view_gen INTEGER NOT NULL,repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,
 version TEXT NOT NULL,fingerprint TEXT NOT NULL,
 PRIMARY KEY(view_gen,repo_prefix,checkout_id)
) WITHOUT ROWID
;

CREATE TABLE contract_attachments (
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 payload_generation INTEGER NOT NULL,
 PRIMARY KEY(repo_prefix, checkout_id, input_version, input_fingerprint)
) WITHOUT ROWID
;

CREATE TABLE contract_attachment_work (
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 work_checkout_id TEXT NOT NULL,
 attachment_version TEXT NOT NULL,
 attachment_fingerprint TEXT NOT NULL,
 token TEXT NOT NULL,
 origin_generation INTEGER NOT NULL,
 file_path TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 scope TEXT NOT NULL,
 PRIMARY KEY(repo_prefix, checkout_id, attachment_version, attachment_fingerprint, token)
) WITHOUT ROWID
;

CREATE TABLE contract_attachment_inputs (
 repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,input_version TEXT NOT NULL,input_fingerprint TEXT NOT NULL,
 source_generation INTEGER NOT NULL,source_repo TEXT NOT NULL,source_checkout TEXT NOT NULL,source_version TEXT NOT NULL,source_fingerprint TEXT NOT NULL,source_accepted INTEGER NOT NULL,source_found INTEGER NOT NULL,source_effective INTEGER NOT NULL,
 PRIMARY KEY(repo_prefix,checkout_id,input_version,input_fingerprint,source_generation,source_repo,source_checkout)
) WITHOUT ROWID
;

CREATE TABLE generation_input_manifest (
    view_gen       INTEGER NOT NULL,
    file_path      TEXT NOT NULL,
    entry_state    TEXT NOT NULL,
    admission      TEXT NOT NULL,
    mode           TEXT NOT NULL DEFAULT '',
    content_sha256 TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, file_path)
) WITHOUT ROWID
;

CREATE TABLE generation_input_manifest_meta (
    view_gen         INTEGER PRIMARY KEY,
    manifest_version INTEGER NOT NULL,
    is_full          INTEGER NOT NULL,
    entry_count      INTEGER NOT NULL,
    policy_digest    TEXT NOT NULL
)
;

CREATE TABLE generation_derivation_stamps (
    view_gen           INTEGER NOT NULL,
    pass               TEXT NOT NULL,
    derivation_version INTEGER NOT NULL,
    stamped_at         INTEGER NOT NULL,
    PRIMARY KEY (view_gen, pass)
) WITHOUT ROWID
;

CREATE TABLE analysis_generations (
    generation_id               INTEGER PRIMARY KEY AUTOINCREMENT,
    format_version              INTEGER NOT NULL,
    build_revision              INTEGER NOT NULL,
    created_at_unix             INTEGER NOT NULL,
    state                       INTEGER NOT NULL CHECK (state IN (0, 1, 2)),
    node_count                  INTEGER NOT NULL CHECK (node_count >= 0),
    community_count             INTEGER NOT NULL CHECK (community_count >= 0),
    process_count               INTEGER NOT NULL CHECK (process_count >= 0),
    concept_count               INTEGER NOT NULL CHECK (concept_count >= 0),
    pagerank_max                REAL NOT NULL DEFAULT 0,
    authority_max               REAL NOT NULL DEFAULT 0,
    hub_max                     REAL NOT NULL DEFAULT 0,
    modularity                  REAL NOT NULL DEFAULT 0,
    processes_truncated         INTEGER NOT NULL DEFAULT 0 CHECK (processes_truncated IN (0, 1)),
    processes_truncation_reason TEXT NOT NULL DEFAULT '',
    view_gen                    INTEGER NOT NULL DEFAULT 0
)
;

CREATE TABLE analysis_active_generation (
    view_gen      INTEGER NOT NULL DEFAULT 0,
    slot          INTEGER NOT NULL CHECK (slot = 1),
    generation_id INTEGER NOT NULL UNIQUE
        REFERENCES analysis_generations(generation_id) ON DELETE RESTRICT,
    PRIMARY KEY (view_gen, slot)
)
;

CREATE TABLE analysis_generation_components (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    component     TEXT NOT NULL,
    row_count     INTEGER NOT NULL CHECK (row_count >= 0),
    sealed_at_unix INTEGER NOT NULL,
    PRIMARY KEY (generation_id, component)
) WITHOUT ROWID
;

CREATE TABLE analysis_communities (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    community_id  TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    hub            TEXT NOT NULL DEFAULT '',
    parent_id      TEXT NOT NULL DEFAULT '',
    size           INTEGER NOT NULL CHECK (size >= 0),
    cohesion       REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (generation_id, community_id)
) WITHOUT ROWID
;

CREATE TABLE analysis_community_files (
    generation_id INTEGER NOT NULL,
    community_id  TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    file_path     TEXT NOT NULL,
    PRIMARY KEY (generation_id, community_id, ordinal),
    FOREIGN KEY (generation_id, community_id)
        REFERENCES analysis_communities(generation_id, community_id) ON DELETE CASCADE
) WITHOUT ROWID
;

CREATE TABLE analysis_nodes (
    id            INTEGER PRIMARY KEY,
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL,
    community_id  TEXT,
    pagerank      REAL NOT NULL DEFAULT 0,
    authority     REAL NOT NULL DEFAULT 0,
    hub           REAL NOT NULL DEFAULT 0,
    UNIQUE (generation_id, node_id),
    FOREIGN KEY (generation_id, community_id)
        REFERENCES analysis_communities(generation_id, community_id)
)
;

CREATE TABLE analysis_processes (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    process_id     TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    entry_point    TEXT NOT NULL DEFAULT '',
    step_count     INTEGER NOT NULL CHECK (step_count >= 0),
    score          REAL NOT NULL DEFAULT 0,
    truncated      INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
    PRIMARY KEY (generation_id, process_id)
) WITHOUT ROWID
;

CREATE TABLE analysis_process_files (
    generation_id INTEGER NOT NULL,
    process_id    TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    file_path     TEXT NOT NULL,
    PRIMARY KEY (generation_id, process_id, ordinal),
    FOREIGN KEY (generation_id, process_id)
        REFERENCES analysis_processes(generation_id, process_id) ON DELETE CASCADE
) WITHOUT ROWID
;

CREATE TABLE analysis_process_steps (
    generation_id INTEGER NOT NULL,
    process_id    TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    node_rowid    INTEGER NOT NULL
        REFERENCES analysis_nodes(id),
    depth         INTEGER NOT NULL CHECK (depth >= 0),
    PRIMARY KEY (generation_id, process_id, ordinal),
    FOREIGN KEY (generation_id, process_id)
        REFERENCES analysis_processes(generation_id, process_id) ON DELETE CASCADE
) WITHOUT ROWID
;

CREATE TABLE analysis_concepts (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    token         TEXT NOT NULL,
    in_vocabulary INTEGER NOT NULL CHECK (in_vocabulary IN (0, 1)),
    PRIMARY KEY (generation_id, token)
) WITHOUT ROWID
;

CREATE TABLE analysis_concept_relations (
    generation_id INTEGER NOT NULL,
    token         TEXT NOT NULL,
    related_token TEXT NOT NULL,
    rank          INTEGER NOT NULL CHECK (rank >= 0),
    PRIMARY KEY (generation_id, token, rank, related_token),
    FOREIGN KEY (generation_id, token)
        REFERENCES analysis_concepts(generation_id, token) ON DELETE CASCADE,
    FOREIGN KEY (generation_id, related_token)
        REFERENCES analysis_concepts(generation_id, token) ON DELETE CASCADE
) WITHOUT ROWID
;

CREATE TABLE analysis_blobs (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    component     TEXT NOT NULL,
    payload       BLOB NOT NULL,
    PRIMARY KEY (generation_id, component)
) WITHOUT ROWID
;

CREATE TABLE repository_families (
    family_id           TEXT PRIMARY KEY,
    common_dir_identity TEXT NOT NULL UNIQUE,
    display_remote      TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL,
    primary_epoch       INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT 0,
    last_seen           INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID
;

CREATE TABLE checkouts (
    checkout_id                 TEXT PRIMARY KEY,
    incarnation                 TEXT NOT NULL,
    family_id                   TEXT NOT NULL
        REFERENCES repository_families(family_id) ON DELETE RESTRICT,
    root_path                   TEXT NOT NULL,
    git_dir                     TEXT NOT NULL,
    admin_name                  TEXT NOT NULL,
    state                       TEXT NOT NULL,
    desired_mode                TEXT NOT NULL,
    effective_mode              TEXT NOT NULL,
    locked                      INTEGER NOT NULL DEFAULT 0,
    prunable                    INTEGER NOT NULL DEFAULT 0,
    head_ref                    TEXT NOT NULL DEFAULT '',
    head_commit                 TEXT NOT NULL DEFAULT '',
    head_tree                   TEXT NOT NULL DEFAULT '',
    last_accessible             INTEGER NOT NULL DEFAULT 0,
    unavailable_since           INTEGER NOT NULL DEFAULT 0,
    availability_deadline       INTEGER NOT NULL DEFAULT 0,
    removal_detected_at         INTEGER NOT NULL DEFAULT 0,
    removal_deadline            INTEGER NOT NULL DEFAULT 0,
    removal_evidence            TEXT NOT NULL DEFAULT '',
    active_intent_transition_id TEXT,
    last_seen                   INTEGER NOT NULL DEFAULT 0,
    last_error                  TEXT NOT NULL DEFAULT '',
    UNIQUE (family_id, admin_name, incarnation)
) WITHOUT ROWID
;

CREATE TABLE tracking_intents (
    intent_id      TEXT PRIMARY KEY,
    checkout_id    TEXT NOT NULL REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    source_kind    TEXT NOT NULL,
    source_locator TEXT NOT NULL,
    active         INTEGER NOT NULL,
    created_at     INTEGER NOT NULL DEFAULT 0,
    revoked_at     INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    UNIQUE (checkout_id, source_kind, source_locator)
) WITHOUT ROWID
;

CREATE TABLE intent_transitions (
    transition_id        TEXT PRIMARY KEY,
    checkout_id          TEXT NOT NULL UNIQUE
        REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    cause                TEXT NOT NULL,
    prior_desired_mode   TEXT,
    prior_effective_mode TEXT,
    requested_mode       TEXT,
    prior_checkout_state TEXT,
    source_snapshot_hash TEXT,
    state                TEXT NOT NULL,
    created_at           INTEGER NOT NULL DEFAULT 0,
    last_progress        INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE checkout_path_evidence (
    checkout_id                    TEXT PRIMARY KEY
        REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    root_path_identity             TEXT NOT NULL DEFAULT '',
    root_volume_kind               TEXT NOT NULL DEFAULT '',
    root_volume_token              TEXT NOT NULL DEFAULT '',
    nearest_existing_ancestor_path TEXT NOT NULL DEFAULT '',
    ancestor_volume_kind           TEXT NOT NULL DEFAULT '',
    ancestor_volume_token          TEXT NOT NULL DEFAULT '',
    common_dir_volume_kind         TEXT NOT NULL DEFAULT '',
    common_dir_volume_token        TEXT NOT NULL DEFAULT '',
    sampled_at                     INTEGER NOT NULL DEFAULT 0,
    sample_generation              INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID
;

CREATE TABLE dedicated_graphs (
    graph_id             TEXT PRIMARY KEY,
    owner_checkout_id    TEXT UNIQUE
        REFERENCES checkouts(checkout_id) ON DELETE RESTRICT,
    repo_prefix          TEXT NOT NULL UNIQUE,
    family_id            TEXT NOT NULL
        REFERENCES repository_families(family_id) ON DELETE RESTRICT,
    is_primary_base      INTEGER NOT NULL DEFAULT 0,
    active_generation_id INTEGER,
    state                TEXT NOT NULL
) WITHOUT ROWID
;

CREATE TABLE view_generations (
    generation_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_kind             TEXT NOT NULL,
    graph_id               TEXT NOT NULL DEFAULT '',
    layer_id               TEXT,
    checkout_id            TEXT,
    generation_kind        TEXT NOT NULL,
    base_generation_id     INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    lower_view_fingerprint TEXT NOT NULL DEFAULT '',
    tree_oid               TEXT NOT NULL DEFAULT '',
    provenance_commit_oid  TEXT,
    config_hash            TEXT NOT NULL DEFAULT '',
    extractor_versions     TEXT NOT NULL DEFAULT '',
    resolver_version       TEXT NOT NULL DEFAULT '',
    dependency_revision    TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL,
    covered_files          INTEGER NOT NULL DEFAULT 0,
    affected_files         INTEGER NOT NULL DEFAULT 0,
    storage_bytes          INTEGER NOT NULL DEFAULT 0,
    completeness           TEXT NOT NULL DEFAULT '',
    created_at             INTEGER NOT NULL DEFAULT 0,
    published_at           INTEGER NOT NULL DEFAULT 0,
    last_selected          INTEGER NOT NULL DEFAULT 0,
    error                  TEXT NOT NULL DEFAULT ''
)
;

CREATE TABLE view_layers (
    layer_id      TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    graph_id      TEXT NOT NULL,
    checkout_id   TEXT,
    target_ref    TEXT,
    target_commit TEXT NOT NULL DEFAULT '',
    target_tree   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE checkout_routes (
    checkout_id          TEXT PRIMARY KEY
        REFERENCES checkouts(checkout_id) ON DELETE RESTRICT,
    graph_id             TEXT NOT NULL,
    commit_generation_id INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    dirty_generation_id  INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    route_epoch          INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL
) WITHOUT ROWID
;

CREATE TABLE ref_views (
    ref_view_id               TEXT PRIMARY KEY,
    graph_id                  TEXT NOT NULL,
    selector_kind             TEXT NOT NULL,
    selector_value            TEXT NOT NULL,
    desired_ref               TEXT NOT NULL DEFAULT '',
    desired_commit            TEXT NOT NULL DEFAULT '',
    desired_tree              TEXT NOT NULL DEFAULT '',
    active_generation_id      INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    active_ref                TEXT,
    active_commit             TEXT,
    active_tree               TEXT,
    enrichment_profile        TEXT NOT NULL,
    desired_build_fingerprint TEXT NOT NULL DEFAULT '',
    active_build_fingerprint  TEXT,
    route_epoch               INTEGER NOT NULL DEFAULT 0,
    state                     TEXT NOT NULL,
    exact_view                INTEGER NOT NULL DEFAULT 1,
    last_resolved             INTEGER NOT NULL DEFAULT 0,
    last_selected             INTEGER NOT NULL DEFAULT 0,
    last_error                TEXT NOT NULL DEFAULT '',
    UNIQUE (graph_id, selector_kind, selector_value, enrichment_profile)
) WITHOUT ROWID
;

CREATE TABLE ref_view_builds (
    build_id             TEXT PRIMARY KEY,
    ref_view_id          TEXT NOT NULL REFERENCES ref_views(ref_view_id) ON DELETE CASCADE,
    desired_ref          TEXT NOT NULL DEFAULT '',
    desired_commit       TEXT NOT NULL DEFAULT '',
    desired_tree         TEXT NOT NULL DEFAULT '',
    base_generation_id   INTEGER,
    enrichment_profile   TEXT NOT NULL DEFAULT '',
    build_fingerprint    TEXT NOT NULL,
    generation_id        INTEGER,
    captured_route_epoch INTEGER NOT NULL,
    state                TEXT NOT NULL,
    build_token          TEXT NOT NULL UNIQUE,
    created_at           INTEGER NOT NULL DEFAULT 0,
    last_progress        INTEGER NOT NULL DEFAULT 0,
    error                TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE cleanup_journal (
    cleanup_id        TEXT PRIMARY KEY,
    opaque_target_ids TEXT NOT NULL,
    reason            TEXT NOT NULL,
    phase             TEXT NOT NULL,
    grace_deadline    INTEGER NOT NULL DEFAULT 0,
    primary_epoch     INTEGER NOT NULL DEFAULT 0,
    last_progress     INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE dedicated_base_publications (
	graph_id TEXT PRIMARY KEY REFERENCES dedicated_graphs(graph_id) ON DELETE CASCADE,
	owner_checkout_id TEXT NOT NULL,
	owner_incarnation TEXT NOT NULL,
	owner_generation_floor INTEGER NOT NULL,
	repo_prefix TEXT NOT NULL,
	family_id TEXT NOT NULL,
	authority_epoch INTEGER NOT NULL,
	authority_token TEXT NOT NULL,
	desired_epoch INTEGER NOT NULL DEFAULT 0,
	tree_oid TEXT NOT NULL DEFAULT '',
	config_hash TEXT NOT NULL DEFAULT '',
	extractor_versions TEXT NOT NULL DEFAULT '',
	resolver_version TEXT NOT NULL DEFAULT '',
	dependency_revision TEXT NOT NULL DEFAULT '',
	attempt_token TEXT NOT NULL DEFAULT '',
	attempt_state TEXT NOT NULL DEFAULT 'idle',
	generation_id INTEGER NOT NULL DEFAULT 0,
	base_generation_id INTEGER NOT NULL DEFAULT 0,
	layer_id TEXT NOT NULL DEFAULT '',
	lower_view_fingerprint TEXT NOT NULL DEFAULT '',
	expected_active_generation_id INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID
;

CREATE TABLE generation_row_counts (
    view_gen INTEGER PRIMARY KEY,
    nodes    INTEGER NOT NULL DEFAULT 0,
    edges    INTEGER NOT NULL DEFAULT 0
)
;

CREATE TABLE generation_row_counts_state (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    seeded       INTEGER NOT NULL,
    installed_at INTEGER NOT NULL,
    seeded_at    INTEGER NOT NULL DEFAULT 0
)
;

CREATE TABLE symbol_fts_generation_revisions (
    view_gen INTEGER PRIMARY KEY,
    rev      INTEGER NOT NULL
)
;

CREATE INDEX generation_contract_work_scope
 ON generation_contract_work(view_gen, repo_prefix, checkout_id, token)
;

CREATE INDEX contract_attachment_payload ON contract_attachments(payload_generation)
;

CREATE INDEX contract_input_identity ON generation_contract_input_state(repo_prefix, checkout_id, input_version, input_fingerprint)
;

CREATE INDEX contract_work_pending_scope ON generation_contract_work(view_gen,repo_prefix,checkout_id,state,token)
;

CREATE INDEX contract_attachment_work_token ON contract_attachment_work(repo_prefix, work_checkout_id, token)
;

CREATE INDEX contract_attachment_input_source ON contract_attachment_inputs(source_repo,source_version,source_fingerprint,source_found,source_effective)
;

CREATE INDEX contract_boundary_keys_file ON generation_contract_boundary_keys(view_gen,repo_prefix,checkout_id,file_path,key_kind,lookup_key)
;

CREATE INDEX contract_boundary_receipt_stamps ON generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,fingerprint,source_fingerprint,accepted)
;

CREATE INDEX analysis_generations_by_state
    ON analysis_generations(state, generation_id DESC)
;

CREATE INDEX analysis_nodes_by_pagerank
    ON analysis_nodes(generation_id, pagerank DESC, id ASC)
;

CREATE INDEX analysis_nodes_by_authority
    ON analysis_nodes(generation_id, authority DESC, id ASC)
;

CREATE INDEX analysis_nodes_by_hub
    ON analysis_nodes(generation_id, hub DESC, id ASC)
;

CREATE INDEX analysis_nodes_by_community
    ON analysis_nodes(generation_id, community_id, node_id)
;

CREATE INDEX analysis_process_step_node_fk
    ON analysis_process_steps(node_rowid)
;

CREATE INDEX analysis_concept_relations_by_related
    ON analysis_concept_relations(generation_id, related_token, rank, token)
;

CREATE UNIQUE INDEX dedicated_graphs_primary_base
    ON dedicated_graphs(family_id) WHERE is_primary_base = 1
;

CREATE INDEX view_generations_by_graph_state
    ON view_generations(graph_id, state, generation_id DESC)
;

CREATE INDEX view_generations_by_base
    ON view_generations(base_generation_id) WHERE base_generation_id IS NOT NULL
;

CREATE INDEX checkout_routes_by_commit_generation
    ON checkout_routes(commit_generation_id) WHERE commit_generation_id IS NOT NULL
;

CREATE INDEX checkout_routes_by_dirty_generation
    ON checkout_routes(dirty_generation_id) WHERE dirty_generation_id IS NOT NULL
;

CREATE INDEX ref_views_by_active_generation
    ON ref_views(active_generation_id) WHERE active_generation_id IS NOT NULL
;

CREATE UNIQUE INDEX ref_view_builds_single_inflight
    ON ref_view_builds(ref_view_id, desired_tree, base_generation_id, build_fingerprint)
    WHERE state = 'building'
;

CREATE INDEX nodes_by_repo_kind ON nodes(repo_prefix, kind)
;

CREATE INDEX nodes_by_generation ON nodes(view_gen, id)
;

CREATE INDEX nodes_name_candidates ON nodes(view_gen, repo_prefix, length(name), id, name, kind, file_path, workspace_id, project_id) WHERE name <> ''
;

CREATE INDEX edges_by_generation ON edges(view_gen, id)
;

CREATE INDEX edges_contract_owner_generation ON edges(view_gen, kind) WHERE kind IN ('provides','consumes','handles_route')
;

CREATE INDEX nodes_contract_seed_candidates ON nodes(repo_prefix,kind,view_gen,id,language,meta) WHERE kind IN ('field','type','interface','method')
;

CREATE INDEX nodes_repo_files ON nodes(repo_prefix, workspace_id, language, file_path, id) WHERE kind = 'file'
;

CREATE INDEX edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1
;

CREATE INDEX edges_fnvalue_prefixed ON edges(view_gen, to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'
;

CREATE INDEX nodes_go_receiver_type ON nodes(repo_prefix, file_dir, name, id) WHERE language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''
;

CREATE INDEX nodes_by_qual ON nodes(qual_name) WHERE qual_name <> ''
;

CREATE INDEX edges_external ON edges(kind) WHERE (to_id GLOB 'dep::*' OR to_id GLOB 'external::*' OR to_id GLOB 'stdlib::*' OR to_id GLOB '*::stdlib::*' OR to_id GLOB 'external-call::*')
;

CREATE INDEX nodes_missing_workspace_slugs ON nodes(repo_prefix)
			WHERE workspace_id = '' OR project_id = ''
;

CREATE INDEX clone_shingles_by_repo
    ON clone_shingles(view_gen, repo_prefix, node_id)
;

CREATE INDEX constant_values_by_file ON constant_values(view_gen, repo_prefix, file_path)
;

CREATE INDEX files_with_errors ON files(view_gen, repo_prefix) WHERE errors <> ''
;

CREATE INDEX ref_facts_by_file ON ref_facts(view_gen, repo_prefix, file_path)
;

CREATE INDEX ref_facts_by_target ON ref_facts(view_gen, repo_prefix, to_id)
;

CREATE INDEX vectors_by_repo ON vectors(view_gen, repo_prefix, node_id)
;

CREATE INDEX churn_by_repo ON churn_enrichment(view_gen, repo_prefix) WHERE repo_prefix <> ''
;

CREATE INDEX coverage_by_repo ON coverage_enrichment(view_gen, repo_prefix) WHERE repo_prefix <> ''
;

CREATE INDEX release_by_repo ON release_enrichment(view_gen, repo_prefix) WHERE repo_prefix <> ''
;

CREATE INDEX blame_by_repo ON blame_enrichment(view_gen, repo_prefix) WHERE repo_prefix <> ''
;

CREATE UNIQUE INDEX symbol_fts_rowid_by_rowid
    ON symbol_fts_rowid(fts_rowid)
;

CREATE INDEX symbol_fts_rowid_by_repo
    ON symbol_fts_rowid(view_gen, repo_prefix, fts_rowid)
;

CREATE INDEX symbol_fts_rowid_by_generation
    ON symbol_fts_rowid(view_gen, fts_rowid)
;

CREATE INDEX content_fts_rowid_by_repo_file
    ON content_fts_rowid(view_gen, repo_prefix, file_path, fts_rowid)
;

CREATE INDEX content_fts_rowid_by_file
    ON content_fts_rowid(view_gen, file_path, fts_rowid)
;

CREATE INDEX nodes_stats_histogram ON nodes(view_gen, kind, language)
;

CREATE INDEX nodes_by_name ON nodes(name, view_gen, id, repo_prefix, kind, file_path)
;

CREATE INDEX nodes_by_kind ON nodes(kind, view_gen)
;

CREATE INDEX nodes_by_file ON nodes(file_path, view_gen)
;

CREATE INDEX nodes_by_repo ON nodes(repo_prefix, view_gen)
;

CREATE INDEX nodes_by_effective_workspace ON nodes(COALESCE(NULLIF(workspace_id, ''), repo_prefix), view_gen, kind, id)
;

CREATE INDEX nodes_deadcode_eligible_workspace ON nodes(COALESCE(NULLIF(workspace_id, ''), repo_prefix), view_gen, kind, id, external, workspace_id, repo_prefix) WHERE external IS NOT 1
;

CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name, view_gen) WHERE name <> ''
;

CREATE INDEX edges_by_from ON edges(view_gen, from_id, kind)
;

CREATE INDEX edges_by_from_line ON edges(view_gen, from_id, line)
;

CREATE INDEX edges_by_from_line_kind ON edges(view_gen, from_id, line, kind)
;

CREATE INDEX edges_by_to ON edges(view_gen, to_id, kind)
;

CREATE INDEX edges_by_kind ON edges(kind)
;

CREATE INDEX edges_by_file ON edges(file_path, kind)
;

CREATE TRIGGER generation_row_counts_edges_delete AFTER DELETE ON edges BEGIN INSERT INTO generation_row_counts(view_gen, edges) VALUES (OLD.view_gen, -1) ON CONFLICT(view_gen) DO UPDATE SET edges = edges + (-1); END
;

CREATE TRIGGER generation_row_counts_edges_insert AFTER INSERT ON edges BEGIN INSERT INTO generation_row_counts(view_gen, edges) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET edges = edges + (1); END
;

CREATE TRIGGER generation_row_counts_edges_move AFTER UPDATE OF view_gen ON edges WHEN OLD.view_gen IS NOT NEW.view_gen BEGIN INSERT INTO generation_row_counts(view_gen, edges) VALUES (OLD.view_gen, -1) ON CONFLICT(view_gen) DO UPDATE SET edges = edges + (-1); INSERT INTO generation_row_counts(view_gen, edges) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET edges = edges + (1); END
;

CREATE TRIGGER generation_row_counts_nodes_delete AFTER DELETE ON nodes BEGIN INSERT INTO generation_row_counts(view_gen, nodes) VALUES (OLD.view_gen, -1) ON CONFLICT(view_gen) DO UPDATE SET nodes = nodes + (-1); END
;

CREATE TRIGGER generation_row_counts_nodes_insert AFTER INSERT ON nodes BEGIN INSERT INTO generation_row_counts(view_gen, nodes) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET nodes = nodes + (1); END
;

CREATE TRIGGER generation_row_counts_nodes_move AFTER UPDATE OF view_gen ON nodes WHEN OLD.view_gen IS NOT NEW.view_gen BEGIN INSERT INTO generation_row_counts(view_gen, nodes) VALUES (OLD.view_gen, -1) ON CONFLICT(view_gen) DO UPDATE SET nodes = nodes + (-1); INSERT INTO generation_row_counts(view_gen, nodes) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET nodes = nodes + (1); END
;

CREATE TRIGGER symbol_fts_generation_revisions_delete AFTER DELETE ON symbol_fts_rowid BEGIN INSERT INTO symbol_fts_generation_revisions(view_gen, rev) VALUES (OLD.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; END
;

CREATE TRIGGER symbol_fts_generation_revisions_insert AFTER INSERT ON symbol_fts_rowid BEGIN INSERT INTO symbol_fts_generation_revisions(view_gen, rev) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; END
;

CREATE TRIGGER symbol_fts_generation_revisions_update AFTER UPDATE ON symbol_fts_rowid BEGIN INSERT INTO symbol_fts_generation_revisions(view_gen, rev) VALUES (OLD.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; INSERT INTO symbol_fts_generation_revisions(view_gen, rev) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; END
;
