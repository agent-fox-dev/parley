import type { Clock, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore, StoredRef } from '../platform/repostore.js';
import { afterPatchBranchPush } from '../rebuild/trigger.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import type { RefOutcome } from './push.js';

/**
 * What happens once a push has been decided.
 *
 * `head_sha` is what every client reads to know where a workspace
 * stands, so it is refreshed after every push — including one whose
 * every ref was refused, because a refusal is still a moment at which
 * the service looked at the repository and can say what it holds.
 *
 * A push that moved a registered patch branch also leaves the stack
 * out of date, so it queues a rebuild. That happens after the refresh
 * and never blocks the push's answer: the client has its report
 * already, and the rebuild is a job.
 */
export interface AfterPushDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  repository: string;
  slug: string;
  clock: Clock;
  /** Where a rebuild is queued, for a workspace that carries patches. */
  jobs?: JobQueue;
  workspace?: WorkspaceRecord;
}

/**
 * Which ref HEAD follows: the repository's main, else its master, else
 * the first branch it has. The advertisement answers the same way, so
 * what a client is told HEAD is and what the workspace reports are the
 * same thing by construction.
 */
export function headRef(refs: readonly StoredRef[]): StoredRef | undefined {
  const heads = refs.filter((ref) => ref.name.startsWith('refs/heads/'));
  return (
    heads.find((ref) => ref.name === 'refs/heads/main') ??
    heads.find((ref) => ref.name === 'refs/heads/master') ??
    heads[0]
  );
}

/**
 * Refreshes the workspace's head_sha from the repository. Only that
 * column is written: a push is not an edit of the workspace, so the
 * record's own updated_at is left where it was.
 */
export function refreshHead(
  deps: AfterPushDeps,
): (outcomes: readonly RefOutcome[]) => Promise<void> {
  return async (outcomes) => {
    const head = headRef(await deps.repos.listRefs(deps.repository));
    await deps.store.run('UPDATE workspaces SET head_sha = ? WHERE slug = ?', [
      head?.oid ?? null,
      deps.slug,
    ]);

    // A push that moved a registered patch branch leaves the stack
    // out of date, so the workspace owes a rebuild.
    if (deps.workspace === undefined) {
      return;
    }
    await afterPatchBranchPush(
      {
        store: deps.store,
        clock: deps.clock,
        ...(deps.jobs === undefined ? {} : { jobs: deps.jobs }),
      },
      deps.workspace,
      outcomes.filter((one) => one.ok).map((one) => one.name),
    );
  };
}
