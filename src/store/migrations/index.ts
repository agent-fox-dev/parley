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
