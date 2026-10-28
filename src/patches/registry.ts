import type { RelationalStore } from '../platform/index.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * Which branches of a workspace are patches.
 *
 * The git server needs this to decide a push, and the sync and branch
 * push routes need the same answer, so the question is asked in one
 * place. What a patch otherwise is — its position, its status, whether
 * it has landed upstream — belongs to the carry-patch package.
 */
export interface PatchRecord {
  workspace_slug: string;
  name: string;
  deleted_at: string | null;
}

/** The patches of a workspace that have not been deleted. */
export async function registeredPatches(store: RelationalStore, slug: string): Promise<string[]> {
  const rows = await store.all<{ name: string }>(
    'SELECT name FROM patches WHERE workspace_slug = ? AND deleted_at IS NULL ORDER BY name',
    [slug],
  );
  return rows.map((row) => row.name);
}

/**
 * Whether a ref is a registered patch branch: a branch of a carry-patch
 * workspace that names one of its patches, and is not the branch the
 * patches are rebuilt onto.
 */
export function patchBranchOf(
  workspace: Pick<WorkspaceRecord, 'workspace_mode' | 'integration_branch'>,
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

/** Adds a patch to a workspace's list. */
export async function addPatch(
  store: RelationalStore,
  slug: string,
  name: string,
  at: string,
): Promise<void> {
  await store.run(
    'INSERT OR REPLACE INTO patches (workspace_slug, name, deleted_at, created_at) VALUES (?, ?, NULL, ?)',
    [slug, name, at],
  );
}
