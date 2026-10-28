import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { pushBranchesOut } from '../repo/retire.js';
import { queueCopyIn } from '../jobs/copy_in.js';
import { type WorkspaceRecord, updateWorkspace } from '../store/workspaces.js';

/**
 * Throwing a repository away and copying it again, as the last resort
 * for one that cannot be repaired.
 *
 * What makes it safe is the order: everything the repository holds
 * that origin does not goes out first, so a reclone loses no commit
 * that exists only inside Parley. That push is best effort — a
 * repository too broken to read is exactly the case this route is for,
 * and refusing to recover it because the push failed would leave the
 * workspace stuck.
 */
export interface RecloneDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
  jobs?: JobQueue;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

/** Whether a reclone may run, from the workspace's own state. */
export function recloneRefusal(workspace: WorkspaceRecord): HttpError | undefined {
  if (workspace.status === 'archived') {
    return new HttpError(400, 'this workspace is archived; reactivate it instead');
  }
  if (workspace.status === 'pending' || workspace.status === 'cloning') {
    return new HttpError(409, 'a copy of this repository is already under way');
  }
  return undefined;
}

/**
 * Pushes out what it can, discards the repository and starts the copy
 * again. The workspace returns to pending and follows the new copy.
 */
export async function recloneWorkspace(
  deps: RecloneDeps,
  workspace: WorkspaceRecord,
): Promise<void> {
  const repository = repositoryOf(workspace);

  const credential =
    deps.secrets === undefined
      ? undefined
      : await resolveGitCredential(
          deps.secrets,
          workspaceScope(workspace.slug),
          'origin',
          deps.log,
        );

  // Best effort, and its outcome is not the route's answer: the push
  // may well fail for the same reason the repository needs recloning.
  await pushBranchesOut(deps.repos, {
    repository,
    gitUrl: workspace.git_url,
    ...(credential === undefined ? {} : { credential }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
    ...(deps.log === undefined ? {} : { log: deps.log }),
  });

  try {
    await deps.repos.remove(repository);
    // An empty one in its place, because the copy fills a repository
    // rather than making one.
    await deps.repos.create(repository);
  } catch (err) {
    throw new HttpError(
      500,
      `the repository could not be discarded: ${err instanceof Error ? err.message : 'the store refused'}`,
    );
  }

  try {
    await updateWorkspace(
      deps.store,
      workspace.slug,
      {
        status: 'pending',
        sync_error: null,
        upstream_head_sha: null,
        clone_error: null,
        head_sha: null,
      },
      deps.clock.now().toISOString(),
    );
  } catch (err) {
    throw new HttpError(
      500,
      `the workspace could not be updated: ${err instanceof Error ? err.message : 'the store refused'}`,
    );
  }

  if (deps.jobs !== undefined) {
    await queueCopyIn(deps.jobs, workspace.slug);
  }
}
