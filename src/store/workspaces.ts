import type { RelationalStore, Row } from '../platform/index.js';

export type WorkspaceStatus = 'pending' | 'cloning' | 'ready' | 'failed' | 'archived';

export interface WorkspaceRecord {
  slug: string;
  git_url: string;
  branch: string | null;
  owner_id: string;
  org_id: string | null;
  status: WorkspaceStatus;
  display_name: string;
  description: string;
  head_sha: string | null;
  clone_error: string | null;
  upstream_head_sha: string | null;
  last_sync_at: string | null;
  sync_error: string | null;
  workspace_mode: 'standard' | 'carry_patch';
  upstream_url: string | null;
  integration_branch: string | null;
  settings: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullableText = (value: unknown): string | null => (typeof value === 'string' ? value : null);

const STATUSES: readonly WorkspaceStatus[] = ['pending', 'cloning', 'ready', 'failed', 'archived'];

function toWorkspace(row: Row): WorkspaceRecord {
  const status = text(row['status']) as WorkspaceStatus;
  let settings: Record<string, unknown> = {};
  try {
    const parsed: unknown = JSON.parse(text(row['settings']) || '{}');
    settings =
      typeof parsed === 'object' && parsed !== null ? (parsed as Record<string, unknown>) : {};
  } catch {
    settings = {};
  }

  return {
    slug: text(row['slug']),
    git_url: text(row['git_url']),
    branch: nullableText(row['branch']),
    owner_id: text(row['owner_id']),
    org_id: nullableText(row['org_id']),
    status: STATUSES.includes(status) ? status : 'pending',
    display_name: text(row['display_name']),
    description: text(row['description']),
    head_sha: nullableText(row['head_sha']),
    clone_error: nullableText(row['clone_error']),
    upstream_head_sha: nullableText(row['upstream_head_sha']),
    last_sync_at: nullableText(row['last_sync_at']),
    sync_error: nullableText(row['sync_error']),
    workspace_mode: text(row['workspace_mode']) === 'carry_patch' ? 'carry_patch' : 'standard',
    upstream_url: nullableText(row['upstream_url']),
    integration_branch: nullableText(row['integration_branch']),
    settings,
    created_at: text(row['created_at']),
    updated_at: text(row['updated_at']),
  };
}

export async function findWorkspace(
  store: RelationalStore,
  slug: string,
): Promise<WorkspaceRecord | undefined> {
  const rows = await store.all('SELECT * FROM workspaces WHERE slug = ?', [slug]);
  return rows[0] === undefined ? undefined : toWorkspace(rows[0]);
}

/**
 * The workspaces a caller may see: their own and those of the
 * organizations they belong to, newest first and by slug where two share
 * an instant. An admin token passes no owner and sees them all.
 */
export async function listVisibleWorkspaces(
  store: RelationalStore,
  visibility: { ownerId?: string; orgIds?: readonly string[]; includeArchived: boolean },
): Promise<WorkspaceRecord[]> {
  const conditions: string[] = [];
  const params: unknown[] = [];

  if (visibility.ownerId !== undefined) {
    const orgIds = visibility.orgIds ?? [];
    const placeholders = orgIds.map(() => '?').join(', ');
    conditions.push(
      orgIds.length === 0 ? 'owner_id = ?' : `(owner_id = ? OR org_id IN (${placeholders}))`,
    );
    params.push(visibility.ownerId, ...orgIds);
  }
  if (!visibility.includeArchived) {
    conditions.push("status != 'archived'");
  }

  const where = conditions.length === 0 ? '' : `WHERE ${conditions.join(' AND ')}`;
  const rows = await store.all(
    `SELECT * FROM workspaces ${where} ORDER BY created_at DESC, slug ASC`,
    params,
  );
  return rows.map(toWorkspace);
}

export async function insertWorkspace(
  store: RelationalStore,
  workspace: WorkspaceRecord,
): Promise<void> {
  await store.run(
    `INSERT INTO workspaces (slug, git_url, branch, owner_id, org_id, status, display_name,
       description, head_sha, clone_error, upstream_head_sha, last_sync_at, sync_error,
       workspace_mode, upstream_url, integration_branch, settings, created_at, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      workspace.slug,
      workspace.git_url,
      workspace.branch,
      workspace.owner_id,
      workspace.org_id,
      workspace.status,
      workspace.display_name,
      workspace.description,
      workspace.head_sha,
      workspace.clone_error,
      workspace.upstream_head_sha,
      workspace.last_sync_at,
      workspace.sync_error,
      workspace.workspace_mode,
      workspace.upstream_url,
      workspace.integration_branch,
      JSON.stringify(workspace.settings),
      workspace.created_at,
      workspace.updated_at,
    ],
  );
}

export { toWorkspace };

/** The columns a workspace update may change. */
export interface WorkspaceChanges {
  display_name?: string;
  description?: string;
  org_id?: string | null;
  status?: WorkspaceStatus;
  settings?: Record<string, unknown>;
  head_sha?: string | null;
  clone_error?: string | null;
  /** What a sync leaves behind: where origin stands, when, and why not. */
  upstream_head_sha?: string | null;
  last_sync_at?: string | null;
  sync_error?: string | null;
  branch?: string | null;
}

export async function updateWorkspace(
  store: RelationalStore,
  slug: string,
  changes: WorkspaceChanges,
  at: string,
): Promise<void> {
  const assignments: string[] = [];
  const params: unknown[] = [];
  for (const [column, value] of Object.entries(changes)) {
    if (value === undefined) {
      continue;
    }
    assignments.push(`${column} = ?`);
    params.push(column === 'settings' ? JSON.stringify(value) : value);
  }
  assignments.push('updated_at = ?');
  params.push(at, slug);
  await store.run(`UPDATE workspaces SET ${assignments.join(', ')} WHERE slug = ?`, params);
}
