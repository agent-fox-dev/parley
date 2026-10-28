import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { fetchPack, lsRefs } from '../git/transport.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * Finding the branch a patch names.
 *
 * It is looked for in three places, in one order, and the first that
 * has it wins: the workspace's own repository, the fork's copy already
 * fetched, and — only where the workspace takes its patch branches
 * from the fork — the fork itself. Which one answered is reported,
 * because "the branch exists" and "the branch was just created from
 * the fork" are different facts to the person who asked.
 */
export type BranchResolution = 'local' | 'origin_tracking' | 'origin_fetch' | 'skipped';

/** Where a fetched copy of the fork's branches is kept. */
export const ORIGIN_TRACKING = 'refs/hub/origin/';

export interface ResolveDeps {
  repos: RepositoryStore;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

/**
 * Resolves one branch, creating it where it was found elsewhere.
 * Anything written is written before the patch is added, so a batch
 * that is refused later leaves the branch behind — which the next
 * attempt finds as `local`, as the reference says it should.
 */
export async function resolveBranch(
  deps: ResolveDeps,
  workspace: WorkspaceRecord,
  branch: string,
  options: { fromOrigin: boolean },
): Promise<BranchResolution> {
  const repository = repositoryOf(workspace);
  const ref = `refs/heads/${branch}`;
  const refs = await deps.repos.listRefs(repository);

  if (refs.some((one) => one.name === ref)) {
    return 'local';
  }

  const tracked = refs.find((one) => one.name === `${ORIGIN_TRACKING}${branch}`);
  if (tracked !== undefined) {
    await deps.repos.updateRefs(repository, [
      { name: ref, oldOid: null, newOid: tracked.oid, force: true },
    ]);
    return 'origin_tracking';
  }

  if (!options.fromOrigin) {
    throw new HttpError(400, `branch "${branch}" does not exist in this workspace`);
  }

  // Only a workspace whose patches come from the fork goes to the
  // fork for one.
  const credential =
    deps.secrets === undefined
      ? undefined
      : await resolveGitCredential(
          deps.secrets,
          workspaceScope(workspace.slug),
          'origin',
          deps.log,
        );
  const authorization = authorizationFor(credential);
  const remote = {
    url: workspace.git_url.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };

  let tip: string | undefined;
  try {
    tip = (await lsRefs(remote)).find((one) => one.name === ref)?.oid;
  } catch {
    throw new HttpError(502, `the fetch from the fork failed`, 'origin_fetch_failed');
  }
  if (tip === undefined) {
    throw new HttpError(400, `branch "${branch}" does not exist in this workspace or on the fork`);
  }

  try {
    const pack = await fetchPack(remote, { wants: [tip] });
    await deps.repos.receivePack(repository, pack, [
      { name: ref, oldOid: null, newOid: tip, force: true },
      // The fork's copy is kept too, so the next add finds it without
      // going back to the fork.
      { name: `${ORIGIN_TRACKING}${branch}`, oldOid: null, newOid: tip, force: true },
    ]);
  } catch (err) {
    if (err instanceof HttpError) {
      throw err;
    }
    throw new HttpError(502, 'the fetch from the fork failed', 'origin_fetch_failed');
  }
  return 'origin_fetch';
}
