import type { RelationalStore } from '../platform/index.js';
import type { Row } from '../platform/store.js';

/**
 * A merge job: a rebase of one branch onto another, and a
 * fast-forward of the target to the result.
 *
 * It is a record rather than a request because it waits its turn for
 * the workspace. Every other operation answers `409 workspace_busy`
 * when the workspace is taken; a merge has somewhere to wait, so it
 * waits, and the client follows it by its id.
 */
export type MergeStatus =
  'queued' | 'running' | 'completed' | 'failed' | 'dead_letter' | 'cancelled';

export interface MergeRecord {
  id: string;
  workspace_slug: string;
  target_branch: string;
  source_ref: string;
  status: MergeStatus;
  /** The target's head before the merge; null until the job completes. */
  base_sha: string | null;
  /** The target's head after it; null until the job completes. */
  merged_sha: string | null;
  conflict_files: string[];
  error: string | null;
  retry_count: number;
  submitted_by: string;
  started_at: string | null;
  created_at: string;
  updated_at: string;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullable = (value: unknown): string | null => (typeof value === 'string' ? value : null);

function toMerge(row: Row): MergeRecord {
  return {
    id: text(row['id']),
    workspace_slug: text(row['workspace_slug']),
    target_branch: text(row['target_branch']),
    source_ref: text(row['source_ref']),
    status: text(row['status']) as MergeStatus,
    base_sha: nullable(row['base_sha']),
    merged_sha: nullable(row['merged_sha']),
    conflict_files: JSON.parse(text(row['conflict_files']) || '[]') as string[],
    error: nullable(row['error']),
    retry_count: Number(row['retry_count'] ?? 0),
    submitted_by: text(row['submitted_by']),
    started_at: nullable(row['started_at']),
    created_at: text(row['created_at']),
    updated_at: text(row['updated_at']),
  };
}

export interface NewMerge {
  id: string;
  workspace_slug: string;
  target_branch: string;
  source_ref: string;
  submitted_by: string;
}

export async function insertMerge(
  store: RelationalStore,
  merge: NewMerge,
  at: string,
): Promise<void> {
  await store.run(
    `INSERT INTO merges
       (id, workspace_slug, target_branch, source_ref, status, submitted_by, created_at, updated_at)
     VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
    [
      merge.id,
      merge.workspace_slug,
      merge.target_branch,
      merge.source_ref,
      merge.submitted_by,
      at,
      at,
    ],
  );
}

export async function mergeById(
  store: RelationalStore,
  id: string,
): Promise<MergeRecord | undefined> {
  const rows = await store.all('SELECT * FROM merges WHERE id = ?', [id]);
  return rows[0] === undefined ? undefined : toMerge(rows[0]);
}

/** A workspace's jobs, newest first and capped as the reference caps them. */
export async function mergesOf(
  store: RelationalStore,
  slug: string,
  limit = 200,
): Promise<MergeRecord[]> {
  const rows = await store.all(
    'SELECT * FROM merges WHERE workspace_slug = ? ORDER BY created_at DESC, id DESC LIMIT ?',
    [slug, limit],
  );
  return rows.map((row) => toMerge(row));
}

/**
 * Whether the same merge is already owed. Two jobs for one source and
 * one target would do the same work twice and race for the branch.
 */
export async function duplicateMerge(
  store: RelationalStore,
  slug: string,
  target: string,
  source: string,
): Promise<MergeRecord | undefined> {
  return (await mergesOf(store, slug)).find(
    (one) =>
      one.target_branch === target &&
      one.source_ref === source &&
      (one.status === 'queued' || one.status === 'running'),
  );
}

export interface MergeChanges {
  status?: MergeStatus;
  base_sha?: string | null;
  merged_sha?: string | null;
  conflict_files?: string[];
  error?: string | null;
  retry_count?: number;
  started_at?: string | null;
}

export async function updateMerge(
  store: RelationalStore,
  id: string,
  changes: MergeChanges,
  at: string,
): Promise<void> {
  const assignments: string[] = [];
  const params: unknown[] = [];
  for (const [column, value] of Object.entries(changes)) {
    if (value === undefined) {
      continue;
    }
    assignments.push(`${column} = ?`);
    params.push(column === 'conflict_files' ? JSON.stringify(value) : value);
  }
  assignments.push('updated_at = ?');
  params.push(at, id);
  await store.run(`UPDATE merges SET ${assignments.join(', ')} WHERE id = ?`, params);
}

/** The job as a client reads it. */
export function serialiseMerge(merge: MergeRecord): Record<string, unknown> {
  return {
    id: merge.id,
    workspace_slug: merge.workspace_slug,
    target_branch: merge.target_branch,
    source_ref: merge.source_ref,
    status: merge.status,
    base_sha: merge.base_sha,
    merged_sha: merge.merged_sha,
    conflict_files: merge.conflict_files,
    error: merge.error,
    retry_count: merge.retry_count,
    submitted_by: merge.submitted_by,
    created_at: merge.created_at,
    updated_at: merge.updated_at,
  };
}
