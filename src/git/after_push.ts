import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore, StoredRef } from '../platform/repostore.js';
import type { RefOutcome } from './push.js';

/**
 * What happens once a push has been decided.
 *
 * `head_sha` is what every client reads to know where a workspace
 * stands, so it is refreshed after every push — including one whose
 * every ref was refused, because a refusal is still a moment at which
 * the service looked at the repository and can say what it holds.
 *
 * Nothing else happens here. Queuing a rebuild for an accepted patch
 * branch is the carry-patch package's, and this is the place it goes:
 * the outcomes say which refs were taken, and the workspace's
 * `auto_rebuild_after_push` setting says whether to.
 */
export interface AfterPushDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  repository: string;
  slug: string;
  clock: Clock;
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
  return async () => {
    const head = headRef(await deps.repos.listRefs(deps.repository));
    await deps.store.run('UPDATE workspaces SET head_sha = ? WHERE slug = ?', [
      head?.oid ?? null,
      deps.slug,
    ]);
  };
}
