import type { RelationalStore } from '../../platform/index.js';

/** A named set of statements applied together, once, in order. */
export interface Migration {
  name: string;
  statements: readonly string[];
}

const LEDGER =
  'CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)';

/**
 * The schema, in the order it was added. Both targets run exactly these
 * statements, so a deployment on Cloudflare and one on a host hold the same
 * tables.
 */
export const MIGRATIONS: readonly Migration[] = [
  {
    name: '001_identity',
    statements: [
      `CREATE TABLE users (
         id TEXT PRIMARY KEY,
         username TEXT NOT NULL UNIQUE,
         email TEXT NOT NULL,
         full_name TEXT NOT NULL DEFAULT '',
         status TEXT NOT NULL DEFAULT 'active',
         role TEXT NOT NULL DEFAULT 'user',
         provider TEXT NOT NULL,
         provider_id TEXT NOT NULL,
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL,
         UNIQUE (provider, provider_id)
       )`,
      `CREATE TABLE credentials (
         id TEXT PRIMARY KEY,
         kind TEXT NOT NULL,
         user_id TEXT NOT NULL,
         name TEXT NOT NULL DEFAULT '',
         secret_hash TEXT NOT NULL,
         permissions TEXT NOT NULL DEFAULT '',
         created_at TEXT NOT NULL,
         expires_at TEXT,
         revoked_at TEXT
       )`,
      'CREATE INDEX credentials_by_user ON credentials (user_id, kind)',
      `CREATE TABLE orgs (
         id TEXT PRIMARY KEY,
         name TEXT NOT NULL UNIQUE,
         slug TEXT NOT NULL UNIQUE,
         url TEXT NOT NULL DEFAULT '',
         owner_id TEXT NOT NULL,
         status TEXT NOT NULL DEFAULT 'active',
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL
       )`,
      `CREATE TABLE org_members (
         org_id TEXT NOT NULL,
         user_id TEXT NOT NULL,
         role TEXT NOT NULL DEFAULT 'member',
         created_at TEXT NOT NULL,
         PRIMARY KEY (org_id, user_id)
       )`,
    ],
  },
  {
    name: '002_workspaces',
    statements: [
      `CREATE TABLE workspaces (
         slug TEXT PRIMARY KEY,
         git_url TEXT NOT NULL,
         branch TEXT,
         owner_id TEXT NOT NULL,
         org_id TEXT,
         status TEXT NOT NULL DEFAULT 'pending',
         display_name TEXT NOT NULL DEFAULT '',
         description TEXT NOT NULL DEFAULT '',
         head_sha TEXT,
         clone_error TEXT,
         upstream_head_sha TEXT,
         last_sync_at TEXT,
         sync_error TEXT,
         workspace_mode TEXT NOT NULL DEFAULT 'standard',
         upstream_url TEXT,
         integration_branch TEXT,
         settings TEXT NOT NULL DEFAULT '{}',
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL
       )`,
      'CREATE INDEX workspaces_by_owner ON workspaces (owner_id)',
      'CREATE INDEX workspaces_by_org ON workspaces (org_id)',
      `CREATE TABLE workspace_secrets (
         scope TEXT NOT NULL,
         key TEXT NOT NULL,
         sealed TEXT NOT NULL,
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL,
         PRIMARY KEY (scope, key)
       )`,
    ],
  },
  {
    name: '003_locks',
    statements: [
      `CREATE TABLE workspace_locks (
         key TEXT PRIMARY KEY,
         holder TEXT NOT NULL,
         taken_at TEXT NOT NULL,
         expires_at TEXT NOT NULL
       )`,
    ],
  },
  {
    name: '004_token_bindings',
    statements: [
      `CREATE TABLE token_bindings (
         token_id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         created_at TEXT NOT NULL
       )`,
      'CREATE INDEX token_bindings_by_workspace ON token_bindings (workspace_slug)',
    ],
  },
  {
    name: '005_entries',
    statements: [
      // One table for both kinds, so the key, size, count and ordering
      // rules cannot drift between them. A secret's value is sealed, a
      // variable's is not; what a route may return is the only difference
      // the service makes between them.
      `CREATE TABLE entries (
         scope TEXT NOT NULL,
         kind TEXT NOT NULL,
         key TEXT NOT NULL,
         key_folded TEXT NOT NULL,
         value TEXT NOT NULL,
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL,
         PRIMARY KEY (scope, kind, key_folded)
       )`,
      `INSERT INTO entries (scope, kind, key, key_folded, value, created_at, updated_at)
       SELECT scope, 'secret', key, lower(key), sealed, created_at, updated_at
       FROM workspace_secrets`,
      'DROP TABLE workspace_secrets',
    ],
  },
  {
    name: '006_jobs',
    statements: [
      `CREATE TABLE jobs (
         id TEXT PRIMARY KEY,
         kind TEXT NOT NULL,
         payload TEXT NOT NULL,
         state TEXT NOT NULL,
         attempts INTEGER NOT NULL DEFAULT 0,
         reason TEXT,
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL,
         claimed_until TEXT
       )`,
      'CREATE INDEX jobs_by_state ON jobs (state, created_at)',
    ],
  },
  {
    name: '007_repository_usage',
    statements: [
      `CREATE TABLE repository_usage (
         repository TEXT PRIMARY KEY,
         bytes INTEGER NOT NULL DEFAULT 0,
         updated_at TEXT NOT NULL
       )`,
    ],
  },
  {
    name: '008_resolutions',
    statements: [
      `CREATE TABLE resolutions (
         repository TEXT NOT NULL,
         identity TEXT NOT NULL,
         path TEXT,
         resolved INTEGER NOT NULL DEFAULT 0,
         content TEXT,
         recorded_at TEXT NOT NULL,
         PRIMARY KEY (repository, identity)
       )`,
      'CREATE INDEX resolutions_by_path ON resolutions (repository, path)',
    ],
  },
  {
    // What the git server needs to know a patch branch when it sees
    // one. The patches themselves — their order, their status, whether
    // they landed upstream — belong to the carry-patch package, which
    // adds those columns when it needs them.
    name: '009_patches',
    statements: [
      `CREATE TABLE patches (
         workspace_slug TEXT NOT NULL,
         name TEXT NOT NULL,
         deleted_at TEXT,
         created_at TEXT NOT NULL,
         PRIMARY KEY (workspace_slug, name)
       )`,
    ],
  },
  {
    // The rest of a patch, now that there is a package that carries
    // one: where it sits in the stack, how it is faring, what it
    // relates to upstream and how it stood against the fork at the
    // last sync. The table is rebuilt rather than altered column by
    // column, because the id is a new primary key.
    name: '010_patch_stack',
    statements: [
      'DROP TABLE patches',
      `CREATE TABLE patches (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         branch_name TEXT NOT NULL,
         position INTEGER NOT NULL,
         status TEXT NOT NULL DEFAULT 'active',
         conflict_files TEXT,
         upstream_pr_url TEXT,
         description TEXT,
         deleted_at TEXT,
         added_at TEXT NOT NULL,
         updated_at TEXT NOT NULL,
         origin_sync_state TEXT,
         origin_sha TEXT,
         origin_synced_at TEXT
       )`,
      'CREATE UNIQUE INDEX patches_by_branch ON patches (workspace_slug, branch_name)',
      'CREATE INDEX patches_by_position ON patches (workspace_slug, position)',
    ],
  },
  {
    // A rebuild is a job rather than a request: the work is far longer
    // than one request may take, so a client is given an id and polls
    // it. Its per-patch results are written as each patch finishes, so
    // a poller sees movement rather than a spinner.
    name: '011_rebuilds',
    statements: [
      `CREATE TABLE rebuilds (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         status TEXT NOT NULL,
         strategy TEXT NOT NULL,
         fail_mode TEXT NOT NULL,
         submitted_by TEXT NOT NULL,
         upstream_head TEXT,
         previous_integration_head TEXT,
         new_integration_head TEXT,
         integration_branch_pushed INTEGER NOT NULL DEFAULT 0,
         error TEXT,
         results TEXT NOT NULL DEFAULT '[]',
         follow_up_of TEXT,
         queued_at TEXT NOT NULL,
         started_at TEXT,
         finished_at TEXT
       )`,
      'CREATE INDEX rebuilds_by_workspace ON rebuilds (workspace_slug, queued_at)',
    ],
  },
  {
    // A merge job, which is a rebase of a source onto a target and a
    // fast-forward of the target. It waits its turn for the
    // workspace rather than being refused, so unlike every other
    // operation it has somewhere to wait: this table.
    name: '012_merges',
    statements: [
      `CREATE TABLE merges (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         target_branch TEXT NOT NULL,
         source_ref TEXT NOT NULL,
         status TEXT NOT NULL,
         base_sha TEXT,
         merged_sha TEXT,
         conflict_files TEXT NOT NULL DEFAULT '[]',
         error TEXT,
         retry_count INTEGER NOT NULL DEFAULT 0,
         submitted_by TEXT NOT NULL,
         started_at TEXT,
         created_at TEXT NOT NULL,
         updated_at TEXT NOT NULL
       )`,
      'CREATE INDEX merges_by_workspace ON merges (workspace_slug, created_at)',
    ],
  },
  {
    // One agent run against one workspace, and what it spent. A
    // session is a cost record rather than an audit trail: what the
    // agent did belongs to the audit package.
    name: '013_sessions',
    statements: [
      `CREATE TABLE sessions (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         credential_id TEXT,
         credential_type TEXT NOT NULL,
         created_by TEXT,
         run_id TEXT,
         node_id TEXT,
         archetype TEXT,
         model TEXT,
         metadata TEXT,
         status TEXT NOT NULL DEFAULT 'active',
         error_message TEXT,
         duration_ms INTEGER,
         cache_creation_input_tokens INTEGER,
         started_at TEXT NOT NULL,
         ended_at TEXT
       )`,
      'CREATE INDEX sessions_by_workspace ON sessions (workspace_slug, started_at)',
      'CREATE INDEX sessions_by_run ON sessions (run_id)',
      `CREATE TABLE session_usage (
         id TEXT PRIMARY KEY,
         session_id TEXT NOT NULL,
         model TEXT NOT NULL,
         input_tokens INTEGER NOT NULL DEFAULT 0,
         output_tokens INTEGER NOT NULL DEFAULT 0,
         cache_read_tokens INTEGER NOT NULL DEFAULT 0,
         recorded_at TEXT NOT NULL
       )`,
      'CREATE INDEX usage_by_session ON session_usage (session_id, recorded_at)',
    ],
  },
  {
    // What the service itself did. The columns are the fields the
    // reference's event envelope names, so a query answers from the
    // row rather than from the metadata it happens to carry.
    name: '014_audit',
    statements: [
      `CREATE TABLE hub_events (
         id TEXT PRIMARY KEY,
         event_type TEXT NOT NULL,
         timestamp TEXT NOT NULL,
         severity TEXT NOT NULL DEFAULT 'info',
         workspace TEXT,
         actor_id TEXT NOT NULL DEFAULT '',
         actor_type TEXT NOT NULL DEFAULT '',
         resource_type TEXT NOT NULL DEFAULT '',
         resource_id TEXT NOT NULL DEFAULT '',
         action TEXT NOT NULL DEFAULT '',
         metadata TEXT NOT NULL DEFAULT '{}'
       )`,
      'CREATE INDEX hub_events_by_time ON hub_events (timestamp, id)',
      'CREATE INDEX hub_events_by_workspace ON hub_events (workspace, timestamp)',
      'CREATE INDEX hub_events_by_type ON hub_events (event_type, timestamp)',
      // One row per bound token, holding the window its last refusal
      // event opened and how many refusals that event now covers.
      `CREATE TABLE bound_token_refusals (
         token_id TEXT PRIMARY KEY,
         event_id TEXT NOT NULL,
         window_started_at TEXT NOT NULL,
         suppressed INTEGER NOT NULL DEFAULT 0
       )`,
    ],
  },
  {
    // What an agent reports about one run. Five tables rather than one
    // with a kind column: the fields differ, the reads differ, and a
    // run's traces are read far more often than anything else here.
    name: '015_run_records',
    statements: [
      `CREATE TABLE run_events (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         run_id TEXT NOT NULL,
         node_id TEXT,
         session_id TEXT,
         timestamp TEXT NOT NULL,
         created_at TEXT NOT NULL,
         event_type TEXT NOT NULL,
         severity TEXT NOT NULL DEFAULT 'info',
         payload TEXT
       )`,
      'CREATE INDEX run_events_by_run ON run_events (workspace_slug, run_id, timestamp)',
      `CREATE TABLE run_session_outcomes (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         run_id TEXT NOT NULL,
         node_id TEXT,
         session_id TEXT,
         timestamp TEXT NOT NULL,
         created_at TEXT NOT NULL,
         status TEXT NOT NULL,
         duration_ms INTEGER,
         token_usage TEXT
       )`,
      'CREATE INDEX run_outcomes_by_run ON run_session_outcomes (workspace_slug, run_id, timestamp)',
      `CREATE TABLE run_tool_calls (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         run_id TEXT NOT NULL,
         node_id TEXT,
         session_id TEXT,
         called_at TEXT NOT NULL,
         created_at TEXT NOT NULL,
         tool_name TEXT NOT NULL,
         duration_ms INTEGER,
         input TEXT,
         output TEXT
       )`,
      'CREATE INDEX run_calls_by_run ON run_tool_calls (workspace_slug, run_id, called_at)',
      `CREATE TABLE run_tool_errors (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         run_id TEXT NOT NULL,
         node_id TEXT,
         session_id TEXT,
         failed_at TEXT NOT NULL,
         created_at TEXT NOT NULL,
         tool_name TEXT NOT NULL,
         error_code TEXT,
         error_msg TEXT NOT NULL
       )`,
      'CREATE INDEX run_errors_by_run ON run_tool_errors (workspace_slug, run_id, failed_at)',
      `CREATE TABLE run_traces (
         id TEXT PRIMARY KEY,
         workspace_slug TEXT NOT NULL,
         run_id TEXT NOT NULL,
         node_id TEXT,
         session_id TEXT,
         timestamp TEXT NOT NULL,
         created_at TEXT NOT NULL,
         event_type TEXT NOT NULL,
         sequence INTEGER,
         data TEXT
       )`,
      'CREATE INDEX run_traces_by_node ON run_traces (workspace_slug, run_id, node_id, timestamp)',
      // A run has at most one, which the primary key is what enforces.
      `CREATE TABLE run_postmortems (
         run_id TEXT NOT NULL,
         workspace_slug TEXT NOT NULL,
         schema_version INTEGER NOT NULL DEFAULT 1,
         run_status TEXT NOT NULL,
         started_at TEXT NOT NULL,
         completed_at TEXT NOT NULL,
         task_summary TEXT NOT NULL,
         cost_summary TEXT NOT NULL,
         blocked_tasks TEXT NOT NULL DEFAULT '[]',
         session_history TEXT NOT NULL DEFAULT '[]',
         ingested_at TEXT NOT NULL,
         PRIMARY KEY (workspace_slug, run_id)
       )`,
    ],
  },
  {
    // How a recorded event reaches the streams watching for it. A row
    // is a delivery rather than a record: the log is hub_events and
    // run_events, and these are swept once no stream could want them.
    name: '016_event_bus',
    statements: [
      `CREATE TABLE event_bus (
         id TEXT PRIMARY KEY,
         event_id TEXT NOT NULL,
         event_type TEXT NOT NULL,
         source TEXT NOT NULL,
         timestamp TEXT NOT NULL,
         workspace TEXT,
         severity TEXT NOT NULL DEFAULT 'info',
         published_at TEXT NOT NULL
       )`,
      'CREATE INDEX event_bus_by_published ON event_bus (published_at, id)',
    ],
  },
];

/**
 * Brings a store up to the current schema. Applying a migration and
 * recording it happen in one batch, so a store never ends up with the
 * tables but no record of them.
 */
export async function migrate(store: RelationalStore, at: Date = new Date()): Promise<void> {
  await store.run(LEDGER);
  const applied = new Set(
    (await store.all<{ name: string }>('SELECT name FROM schema_migrations')).map(
      (row) => row.name,
    ),
  );

  for (const migration of MIGRATIONS) {
    if (applied.has(migration.name)) {
      continue;
    }
    await store.batch([
      ...migration.statements.map((sql) => ({ sql })),
      {
        sql: 'INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)',
        params: [migration.name, at.toISOString()],
      },
    ]);
  }
}
