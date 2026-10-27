import { HttpError } from '../http/errors.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import { retireBindingsOf } from '../store/bindings.js';
import { type WorkspaceRecord, updateWorkspace } from '../store/workspaces.js';
import { workspaceScope } from '../platform/secrets.js';
import { withWorkspaceLock } from './lock.js';

/**
 * Archives a workspace. It takes the lock, because archiving is one of the
 * operations that work on the repository; a workspace another operation is
 * using answers 409 workspace_busy at once. Bound tokens are left alone.
 */
export async function archiveWorkspace(
  store: RelationalStore,
  locks: Locks,
  clock: Clock,
  workspace: WorkspaceRecord,
): Promise<void> {
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'this workspace is already archived');
  }
  if (workspace.status === 'cloning') {
    throw new HttpError(409, 'this workspace is still cloning');
  }

  await withWorkspaceLock(locks, workspace.slug, async () => {
    await updateWorkspace(store, workspace.slug, { status: 'archived' }, clock.now().toISOString());
  });
}

/** Returns an archived workspace to pending, so its clone is made again. */
export async function reactivateWorkspace(
  store: RelationalStore,
  clock: Clock,
  workspace: WorkspaceRecord,
): Promise<void> {
  if (workspace.status !== 'archived') {
    throw new HttpError(409, 'this workspace is not archived');
  }
  await updateWorkspace(store, workspace.slug, { status: 'pending' }, clock.now().toISOString());
}

/**
 * Deletes an archived workspace for good. The token revocation, the
 * bindings, the workspace's secrets and the record itself go in one
 * transaction, so a failure leaves a workspace whose tokens still work
 * rather than tokens revoked for a workspace that is still there.
 */
export async function deleteWorkspaceAndTokens(
  store: RelationalStore,
  clock: Clock,
  workspace: WorkspaceRecord,
): Promise<void> {
  if (workspace.status !== 'archived') {
    throw new HttpError(409, 'this workspace is not archived; archive it first');
  }

  const at = clock.now().toISOString();
  await store.batch([
    ...retireBindingsOf(workspace.slug, at),
    {
      sql: 'DELETE FROM workspace_secrets WHERE scope = ?',
      params: [workspaceScope(workspace.slug)],
    },
    { sql: 'DELETE FROM workspaces WHERE slug = ?', params: [workspace.slug] },
  ]);
}
