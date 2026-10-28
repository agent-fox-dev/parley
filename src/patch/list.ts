import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import type { Row } from '../platform/store.js';

/**
 * The ordered list of branches a carry-patch workspace carries on top
 * of its upstream.
 *
 * Two things about it are load-bearing everywhere else: the positions
 * are 1 to n with no gap, so "apply them in order" needs no
 * interpretation, and the branch is immutable, so a patch is the same
 * patch however its position or status changes.
 */
export type PatchStatus = 'active' | 'merged_upstream' | 'conflict' | 'disabled' | 'deleted';

export interface PatchRecord {
  id: string;
  workspace_slug: string;
  branch_name: string;
  position: number;
  status: PatchStatus;
  conflict_files: string[] | null;
  upstream_pr_url: string | null;
  description: string | null;
  deleted_at: string | null;
  added_at: string;
  updated_at: string;
  origin_sync_state: 'in_sync' | 'diverged' | 'missing_on_origin' | null;
  origin_sha: string | null;
  origin_synced_at: string | null;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullable = (value: unknown): string | null => (typeof value === 'string' ? value : null);

function toPatch(row: Row): PatchRecord {
  const files = nullable(row['conflict_files']);
  return {
    id: text(row['id']),
    workspace_slug: text(row['workspace_slug']),
    branch_name: text(row['branch_name']),
    position: Number(row['position'] ?? 0),
    status: text(row['status']) as PatchStatus,
    conflict_files: files === null ? null : (JSON.parse(files) as string[]),
    upstream_pr_url: nullable(row['upstream_pr_url']),
    description: nullable(row['description']),
    deleted_at: nullable(row['deleted_at']),
    added_at: text(row['added_at']),
    updated_at: text(row['updated_at']),
    origin_sync_state: nullable(row['origin_sync_state']) as PatchRecord['origin_sync_state'],
    origin_sha: nullable(row['origin_sha']),
    origin_synced_at: nullable(row['origin_synced_at']),
  };
}

/** Every patch of a workspace in position order, deleted ones included. */
export async function allPatches(store: RelationalStore, slug: string): Promise<PatchRecord[]> {
  const rows = await store.all(
    'SELECT * FROM patches WHERE workspace_slug = ? ORDER BY position, added_at',
    [slug],
  );
  return rows.map((row) => toPatch(row));
}

/** The patches a client sees: everything but the deleted ones. */
export async function livePatches(store: RelationalStore, slug: string): Promise<PatchRecord[]> {
  return (await allPatches(store, slug)).filter((patch) => patch.status !== 'deleted');
}

export async function patchById(
  store: RelationalStore,
  slug: string,
  id: string,
): Promise<PatchRecord | undefined> {
  const rows = await store.all('SELECT * FROM patches WHERE workspace_slug = ? AND id = ?', [
    slug,
    id,
  ]);
  return rows[0] === undefined ? undefined : toPatch(rows[0]);
}

export async function patchByBranch(
  store: RelationalStore,
  slug: string,
  branch: string,
): Promise<PatchRecord | undefined> {
  const rows = await store.all(
    'SELECT * FROM patches WHERE workspace_slug = ? AND branch_name = ?',
    [slug, branch],
  );
  return rows[0] === undefined ? undefined : toPatch(rows[0]);
}

export interface NewPatch {
  branch_name: string;
  position?: number;
  upstream_pr_url?: string;
  description?: string;
}

/**
 * Inserts a patch at a position, moving everything from there down.
 * A position past the end, and none at all, append: there is no gap
 * to leave, because the list is always 1 to n.
 */
export async function insertPatch(
  store: RelationalStore,
  slug: string,
  patch: NewPatch,
  at: string,
  id: string,
): Promise<PatchRecord> {
  const live = await livePatches(store, slug);
  const wanted = patch.position ?? live.length + 1;
  const position = Math.min(Math.max(wanted, 1), live.length + 1);

  await store.batch([
    {
      sql: 'UPDATE patches SET position = position + 1, updated_at = ? WHERE workspace_slug = ? AND status != ? AND position >= ?',
      params: [at, slug, 'deleted', position],
    },
    {
      sql: `INSERT INTO patches
              (id, workspace_slug, branch_name, position, status, upstream_pr_url, description, added_at, updated_at)
            VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
      params: [
        id,
        slug,
        patch.branch_name,
        position,
        patch.upstream_pr_url ?? null,
        patch.description ?? null,
        at,
        at,
      ],
    },
  ]);

  const added = await patchById(store, slug, id);
  if (added === undefined) {
    throw new HttpError(500, 'the patch could not be read back after it was added');
  }
  return added;
}

/**
 * Closes any gap the list has, so the positions read 1 to n in the
 * order they already have. Every change that can leave a hole ends
 * here rather than each one doing its own arithmetic.
 */
export async function closePositions(
  store: RelationalStore,
  slug: string,
  at: string,
): Promise<void> {
  const live = await livePatches(store, slug);
  const moved = live
    .map((patch, index) => ({ patch, position: index + 1 }))
    .filter((one) => one.patch.position !== one.position);
  if (moved.length === 0) {
    return;
  }
  await store.batch(
    moved.map((one) => ({
      sql: 'UPDATE patches SET position = ?, updated_at = ? WHERE id = ?',
      params: [one.position, at, one.patch.id],
    })),
  );
}

/**
 * The branch names of a workspace's patches that are still carried.
 * The git server reads it to know a patch branch when a push names
 * one, and the branch push route to know which branches it will not
 * send.
 */
export async function registeredPatches(store: RelationalStore, slug: string): Promise<string[]> {
  return (await livePatches(store, slug)).map((patch) => patch.branch_name);
}

/**
 * Whether a ref is a registered patch branch: a branch of a
 * carry-patch workspace that names one of its patches, and is not the
 * branch the patches are rebuilt onto.
 */
export function patchBranchOf(
  workspace: { workspace_mode: string; integration_branch: string | null },
  ref: string,
  patches: readonly string[],
): string | undefined {
  if (workspace.workspace_mode !== 'carry_patch' || !ref.startsWith('refs/heads/')) {
    return undefined;
  }
  const branch = ref.slice('refs/heads/'.length);
  if (branch === workspace.integration_branch || !patches.includes(branch)) {
    return undefined;
  }
  return branch;
}

/**
 * The fields of a patch a client may change. The branch is not among
 * them: a patch is the same patch however it is otherwise amended,
 * and a branch_name in a body is ignored rather than refused.
 */
export interface PatchChanges {
  position?: number;
  status?: PatchStatus;
  description?: string | null;
  upstream_pr_url?: string | null;
}

/**
 * Applies a change and then closes any gap it left, so the list is
 * contiguous after every operation rather than only after the ones
 * that remembered to tidy up.
 */
export async function changePatch(
  store: RelationalStore,
  slug: string,
  patch: PatchRecord,
  changes: PatchChanges,
  at: string,
): Promise<PatchRecord> {
  const live = await livePatches(store, slug);
  const assignments: string[] = [];
  const params: unknown[] = [];

  for (const [column, value] of Object.entries(changes)) {
    if (value === undefined || column === 'position') {
      continue;
    }
    assignments.push(`${column} = ?`);
    params.push(value);
  }

  // A move is the other patches shifting around this one, so it is
  // done by rewriting the order rather than by arithmetic on one row.
  if (changes.position !== undefined) {
    const without = live.filter((one) => one.id !== patch.id);
    const to = Math.min(Math.max(changes.position, 1), without.length + 1);
    const order = [...without.slice(0, to - 1), patch, ...without.slice(to - 1)];
    await store.batch(
      order.map((one, index) => ({
        sql: 'UPDATE patches SET position = ?, updated_at = ? WHERE id = ?',
        params: [index + 1, at, one.id],
      })),
    );
  }

  if (assignments.length > 0) {
    assignments.push('updated_at = ?');
    params.push(at, patch.id);
    await store.run(`UPDATE patches SET ${assignments.join(', ')} WHERE id = ?`, params);
  }

  await closePositions(store, slug, at);
  const changed = await patchById(store, slug, patch.id);
  if (changed === undefined) {
    throw new HttpError(500, 'the patch could not be read back after it changed');
  }
  return changed;
}

/** The patch as every route that returns one returns it. */
export function serialisePatch(
  patch: PatchRecord,
  extra: { replaced_sha?: string } = {},
): Record<string, unknown> {
  return {
    id: patch.id,
    workspace_slug: patch.workspace_slug,
    branch_name: patch.branch_name,
    position: patch.position,
    status: patch.status,
    ...(patch.conflict_files === null ? {} : { conflict_files: patch.conflict_files }),
    upstream_pr_url: patch.upstream_pr_url,
    description: patch.description,
    deleted_at: patch.deleted_at,
    added_at: patch.added_at,
    updated_at: patch.updated_at,
    ...(patch.origin_sync_state === null ? {} : { origin_sync_state: patch.origin_sync_state }),
    ...(patch.origin_sha === null ? {} : { origin_sha: patch.origin_sha }),
    ...(patch.origin_synced_at === null ? {} : { origin_synced_at: patch.origin_synced_at }),
    ...extra,
  };
}
