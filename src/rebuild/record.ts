import type { RelationalStore } from '../platform/index.js';
import type { Row } from '../platform/store.js';

/**
 * A rebuild job and what it has found so far.
 *
 * It is a record rather than a return value because a rebuild outlives
 * the request that asked for it: the client is given an id and reads
 * the results as they arrive.
 */
/**
 * A job that failed for a reason worth retrying is dead-lettered
 * rather than simply failed: the two are told apart because only one
 * of them may be queued again.
 */
export type RebuildStatus =
  'queued' | 'running' | 'completed' | 'failed' | 'dead_letter' | 'cancelled';
export type RebuildStrategy = 'rebase' | 'merge';
export type FailMode = 'fail_fast' | 'continue';

/** What became of one patch in one rebuild. */
export interface PatchOutcome {
  patch_id: string;
  branch_name: string;
  position: number;
  outcome: 'applied' | 'skipped' | 'conflict' | 'failed';
  /** Why it was skipped, or what went wrong. */
  reason?: string;
  conflict_files?: string[];
}

export interface RebuildRecord {
  id: string;
  workspace_slug: string;
  status: RebuildStatus;
  strategy: RebuildStrategy;
  fail_mode: FailMode;
  /** The user who asked, or `system` where the hub asked itself. */
  submitted_by: string;
  upstream_head: string | null;
  previous_integration_head: string | null;
  new_integration_head: string | null;
  integration_branch_pushed: boolean;
  error: string | null;
  results: PatchOutcome[];
  follow_up_of: string | null;
  queued_at: string;
  started_at: string | null;
  finished_at: string | null;
}

/**
 * Who asked for a rebuild the hub asked itself for. The reference
 * names the reason as well as the asker, because a follow-up and a
 * push-triggered rebuild are both the system and a person reading the
 * list wants to know which.
 */
export const STALE_SNAPSHOT = 'system:stale-snapshot';
export const AFTER_PUSH = 'system:patch-push';
export const AFTER_SYNC = 'system:sync';
export const AFTER_RESET = 'system:reset-to-origin';

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullable = (value: unknown): string | null => (typeof value === 'string' ? value : null);

function toRebuild(row: Row): RebuildRecord {
  return {
    id: text(row['id']),
    workspace_slug: text(row['workspace_slug']),
    status: text(row['status']) as RebuildStatus,
    strategy: text(row['strategy']) as RebuildStrategy,
    fail_mode: text(row['fail_mode']) as FailMode,
    submitted_by: text(row['submitted_by']),
    upstream_head: nullable(row['upstream_head']),
    previous_integration_head: nullable(row['previous_integration_head']),
    new_integration_head: nullable(row['new_integration_head']),
    integration_branch_pushed: Number(row['integration_branch_pushed'] ?? 0) === 1,
    error: nullable(row['error']),
    results: JSON.parse(text(row['results']) || '[]') as PatchOutcome[],
    follow_up_of: nullable(row['follow_up_of']),
    queued_at: text(row['queued_at']),
    started_at: nullable(row['started_at']),
    finished_at: nullable(row['finished_at']),
  };
}

export interface NewRebuild {
  id: string;
  workspace_slug: string;
  strategy: RebuildStrategy;
  fail_mode: FailMode;
  submitted_by: string;
  follow_up_of?: string;
}

export async function insertRebuild(
  store: RelationalStore,
  rebuild: NewRebuild,
  at: string,
): Promise<void> {
  await store.run(
    `INSERT INTO rebuilds
       (id, workspace_slug, status, strategy, fail_mode, submitted_by, follow_up_of, queued_at)
     VALUES (?, ?, 'queued', ?, ?, ?, ?, ?)`,
    [
      rebuild.id,
      rebuild.workspace_slug,
      rebuild.strategy,
      rebuild.fail_mode,
      rebuild.submitted_by,
      rebuild.follow_up_of ?? null,
      at,
    ],
  );
}

export async function rebuildById(
  store: RelationalStore,
  id: string,
): Promise<RebuildRecord | undefined> {
  const rows = await store.all('SELECT * FROM rebuilds WHERE id = ?', [id]);
  return rows[0] === undefined ? undefined : toRebuild(rows[0]);
}

export async function rebuildsOf(store: RelationalStore, slug: string): Promise<RebuildRecord[]> {
  const rows = await store.all(
    'SELECT * FROM rebuilds WHERE workspace_slug = ? ORDER BY queued_at DESC, id DESC',
    [slug],
  );
  return rows.map((row) => toRebuild(row));
}

/** The rebuild of a workspace that is still to run, or running. */
export async function pendingRebuild(
  store: RelationalStore,
  slug: string,
): Promise<RebuildRecord | undefined> {
  return (await rebuildsOf(store, slug)).find(
    (one) => one.status === 'queued' || one.status === 'running',
  );
}

export interface RebuildChanges {
  status?: RebuildStatus;
  upstream_head?: string | null;
  previous_integration_head?: string | null;
  new_integration_head?: string | null;
  integration_branch_pushed?: boolean;
  error?: string | null;
  results?: PatchOutcome[];
  started_at?: string | null;
  finished_at?: string | null;
}

export async function updateRebuild(
  store: RelationalStore,
  id: string,
  changes: RebuildChanges,
): Promise<void> {
  const assignments: string[] = [];
  const params: unknown[] = [];
  for (const [column, value] of Object.entries(changes)) {
    if (value === undefined) {
      continue;
    }
    assignments.push(`${column} = ?`);
    params.push(
      column === 'results'
        ? JSON.stringify(value)
        : typeof value === 'boolean'
          ? Number(value)
          : value,
    );
  }
  if (assignments.length === 0) {
    return;
  }
  params.push(id);
  await store.run(`UPDATE rebuilds SET ${assignments.join(', ')} WHERE id = ?`, params);
}
