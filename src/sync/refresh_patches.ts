import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { descendsFrom } from '../git/ancestry.js';
import { type Remote, fetchPack } from '../git/transport.js';
import type { PatchRecord } from '../patch/list.js';

/**
 * Bringing each patch branch to the fork's.
 *
 * It is the same decision `reset-to-origin` makes for one branch,
 * applied to the stack by a sync, with one difference: a branch the
 * two have both moved is only replaced where the workspace's
 * divergence policy says to. Under `report` it is left alone and
 * named, because a workspace that would rather be told than
 * overwritten has said so.
 */
export type RefreshAction =
  'none' | 'created' | 'fast_forwarded' | 'replaced' | 'missing_on_origin';
export type OriginState = 'in_sync' | 'diverged' | 'missing_on_origin';

export interface RefreshedPatch {
  branch_name: string;
  action: RefreshAction;
  state: OriginState;
  local_sha?: string;
  origin_sha?: string;
  replaced_sha?: string;
}

/** A refresh that could not be made, named as the reference names it. */
export class RefreshFailure extends Error {
  constructor(
    readonly branch: string,
    readonly stage: 'update' | 'compare' | 'worktree',
  ) {
    super(
      stage === 'compare'
        ? `failed to compare patch branch ${branch} with origin`
        : stage === 'worktree'
          ? `failed to reset working tree for patch branch ${branch}`
          : `failed to update patch branch ${branch}`,
    );
    this.name = 'RefreshFailure';
  }
}

export interface RefreshDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
  repository: string;
  remote: Remote;
}

/**
 * Refreshes one patch branch and records how it now stands. The
 * record is written as each branch is settled, so a failure part way
 * leaves the branches already done describing themselves truthfully.
 */
export async function refreshPatchBranch(
  deps: RefreshDeps,
  patch: PatchRecord,
  options: { policy: 'replace' | 'report'; onFork: ReadonlyMap<string, string> },
): Promise<RefreshedPatch> {
  const branch = patch.branch_name;
  const ref = `refs/heads/${branch}`;
  const at = deps.clock.now().toISOString();
  const theirs = options.onFork.get(ref);

  const here = (await deps.repos.listRefs(deps.repository)).find((one) => one.name === ref)?.oid;

  const settle = async (
    action: RefreshAction,
    state: OriginState,
    extra: { replaced?: string } = {},
  ): Promise<RefreshedPatch> => {
    await deps.store.run(
      'UPDATE patches SET origin_sync_state = ?, origin_sha = ?, origin_synced_at = ?, updated_at = ? WHERE id = ?',
      [state, theirs ?? null, at, at, patch.id],
    );
    return {
      branch_name: branch,
      action,
      state,
      ...(here === undefined ? {} : { local_sha: action === 'none' ? here : (theirs ?? here) }),
      ...(theirs === undefined ? {} : { origin_sha: theirs }),
      ...(extra.replaced === undefined ? {} : { replaced_sha: extra.replaced }),
    };
  };

  if (theirs === undefined) {
    return settle('missing_on_origin', 'missing_on_origin');
  }
  if (here === theirs) {
    return settle('none', 'in_sync');
  }

  let pack: Uint8Array;
  try {
    pack = await fetchPack(deps.remote, {
      wants: [theirs],
      ...(here === undefined ? {} : { haves: [here] }),
    });
  } catch {
    throw new RefreshFailure(branch, 'compare');
  }

  const forward = here !== undefined && descendsFrom(pack, theirs, here);
  if (here !== undefined && !forward && options.policy === 'report') {
    // Told, not overwritten: the workspace asked for that.
    return settle('none', 'diverged');
  }

  const action: RefreshAction =
    here === undefined ? 'created' : forward ? 'fast_forwarded' : 'replaced';
  try {
    await deps.repos.receivePack(deps.repository, pack, [
      { name: ref, oldOid: here ?? null, newOid: theirs, force: true },
      ...(action === 'replaced' && here !== undefined
        ? [
            {
              name: `refs/hub/replaced/${branch}`,
              oldOid: null,
              newOid: here,
              force: true,
            },
          ]
        : []),
    ]);
  } catch {
    // A branch another operation changed meanwhile is not overwritten.
    throw new RefreshFailure(branch, 'update');
  }

  return settle(
    action,
    'in_sync',
    action === 'replaced' && here !== undefined ? { replaced: here } : {},
  );
}

/** Clears what a workspace that owns its patches has no business keeping. */
export async function clearOriginFields(
  store: RelationalStore,
  slug: string,
  at: string,
): Promise<void> {
  await store.run(
    'UPDATE patches SET origin_sync_state = NULL, origin_sha = NULL, origin_synced_at = NULL, updated_at = ? WHERE workspace_slug = ?',
    [at, slug],
  );
}
