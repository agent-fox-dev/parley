import { HttpError } from '../http/errors.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import { retireBindingsOf } from '../store/bindings.js';
import { type WorkspaceRecord, updateWorkspace } from '../store/workspaces.js';
import { workspaceScope } from '../platform/secrets.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import type { LogSink } from '../http/router.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { pushOutOnArchive, removeRepository } from '../repo/retire.js';
import { repositoryOf } from '../repo/repository.js';
import { terminateSessions } from '../session/terminate.js';
import { withWorkspaceLock } from './lock.js';

/**
 * Archives a workspace. It takes the lock, because archiving is one of the
 * operations that work on the repository; a workspace another operation is
 * using answers 409 workspace_busy at once. Bound tokens are left alone.
 */
export interface RetirementDeps {
  repos?: RepositoryStore;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

export async function archiveWorkspace(
  store: RelationalStore,
  locks: Locks,
  clock: Clock,
  workspace: WorkspaceRecord,
  deps: RetirementDeps = {},
): Promise<void> {
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'this workspace is already archived');
  }
  if (workspace.status === 'cloning') {
    throw new HttpError(409, 'this workspace is still cloning');
  }

  await withWorkspaceLock(locks, workspace.slug, async () => {
    // Branches pushed to this service exist only here, so they go out to
    // origin before the repository is let go. A push that cannot be made
    // keeps the repository; the workspace is archived either way.
    if (deps.repos !== undefined && deps.secrets !== undefined) {
      const credential = await resolveGitCredential(
        deps.secrets,
        workspaceScope(workspace.slug),
        'origin',
        deps.log,
      );
      await pushOutOnArchive(deps.repos, {
        repository: repositoryOf(workspace),
        gitUrl: workspace.git_url,
        ...(credential === undefined ? {} : { credential }),
        ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
        ...(deps.log === undefined ? {} : { log: deps.log }),
      });
    }

    await updateWorkspace(store, workspace.slug, { status: 'archived' }, clock.now().toISOString());

    // An agent whose workspace is archived under it will not report
    // that it finished, so the hub records what happened to it.
    await terminateSessions(store, clock, workspace.slug);
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
  deps: RetirementDeps = {},
): Promise<void> {
  if (workspace.status !== 'archived') {
    throw new HttpError(409, 'this workspace is not archived; archive it first');
  }

  const at = clock.now().toISOString();
  // Ended before the record goes, so nothing is left pointing at a
  // workspace that is not there.
  await terminateSessions(store, clock, workspace.slug);

  await store.batch([
    ...retireBindingsOf(workspace.slug, at),
    {
      sql: 'DELETE FROM entries WHERE scope = ?',
      params: [workspaceScope(workspace.slug)],
    },
    { sql: 'DELETE FROM workspaces WHERE slug = ?', params: [workspace.slug] },
    {
      sql: 'DELETE FROM repository_usage WHERE repository = ?',
      params: [repositoryOf(workspace)],
    },
  ]);

  // The record is gone, so the repository goes too. It happens after the
  // transaction: a store that cannot answer must not keep the workspace.
  if (deps.repos !== undefined) {
    await removeRepository(deps.repos, repositoryOf(workspace));
  }
}
