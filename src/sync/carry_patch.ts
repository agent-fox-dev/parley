import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { newIdentifier } from '../auth/credentials.js';
import { descendsFrom } from '../git/ancestry.js';
import { type Remote, fetchPack, lsRefs } from '../git/transport.js';
import { queueRebuild } from '../jobs/rebuild.js';
import { livePatches } from '../patch/list.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { insertRebuild, pendingRebuild } from '../rebuild/record.js';
import { type WorkspaceRecord, updateWorkspace } from '../store/workspaces.js';
import { effectiveSettings } from '../workspace/settings.js';
import {
  type RefreshedPatch,
  RefreshFailure,
  clearOriginFields,
  refreshPatchBranch,
} from './refresh_patches.js';
import { type Detection, historyOf, landedUpstream } from './merged_upstream.js';

/**
 * Syncing a workspace that carries patches.
 *
 * It is not the standard sync with extras: it fetches a different
 * remote, moves no branch of its own, and reports its findings rather
 * than an outcome. The steps are numbered in the reference and they
 * are numbered here, because a failure at step 5 has to leave exactly
 * what the earlier steps reached and nothing of the later ones.
 */
export interface CarryPatchDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
  jobs?: JobQueue;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

export interface CarryPatchSync {
  patches_merged: string[];
  rebuild_triggered: boolean;
  rebuild_job_id?: string;
  force_push_detected: boolean;
  origin_fetched: boolean;
  patches_synced?: RefreshedPatch[];
  patches_diverged?: string[];
}

/** A remote of this workspace, with whatever credential it stored. */
async function remoteFor(
  deps: CarryPatchDeps,
  workspace: WorkspaceRecord,
  which: 'origin' | 'upstream',
): Promise<Remote> {
  const url =
    which === 'upstream' ? (workspace.upstream_url ?? workspace.git_url) : workspace.git_url;
  if (deps.secrets === undefined) {
    return {
      url: url.replace(/\/$/, ''),
      ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
    };
  }
  const credential = await resolveGitCredential(
    deps.secrets,
    workspaceScope(workspace.slug),
    which,
    deps.log,
  );
  const authorization = authorizationFor(credential);
  return {
    url: url.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };
}

export async function syncCarryPatch(
  deps: CarryPatchDeps,
  workspace: WorkspaceRecord,
): Promise<CarryPatchSync> {
  const repository = repositoryOf(workspace);
  const settings = effectiveSettings(workspace.settings);
  const fromOrigin = settings['patch_branch_source'] === 'origin';
  const at = deps.clock.now().toISOString();

  // 1. The fork's credentials, where the fork is in play at all.
  if (fromOrigin && deps.secrets !== undefined) {
    const credential = await resolveGitCredential(
      deps.secrets,
      workspaceScope(workspace.slug),
      'origin',
      deps.log,
    );
    if (credential === undefined) {
      throw new HttpError(502, 'failed to resolve origin credentials');
    }
  }

  // 2. Upstream, which is the base every rebuild starts from.
  const upstream = await remoteFor(deps, workspace, 'upstream');
  let upstreamRefs;
  try {
    upstreamRefs = await lsRefs(upstream);
  } catch {
    throw new HttpError(502, 'the fetch from upstream failed');
  }
  const branch = workspace.branch ?? 'main';
  const upstreamHead =
    upstreamRefs.find((ref) => ref.name === `refs/heads/${branch}`)?.oid ??
    upstreamRefs.find((ref) => ref.name === 'refs/heads/main')?.oid;
  if (upstreamHead === undefined) {
    throw new HttpError(502, 'upstream has no branch to rebuild onto');
  }

  let upstreamPack: Uint8Array = new Uint8Array(0);
  if (upstreamHead !== workspace.upstream_head_sha) {
    try {
      upstreamPack = await fetchPack(upstream, {
        wants: [upstreamHead],
        ...(workspace.upstream_head_sha === null ? {} : { haves: [workspace.upstream_head_sha] }),
      });
      await deps.repos.receivePack(repository, upstreamPack, [
        { name: `refs/hub/upstream/${branch}`, oldOid: null, newOid: upstreamHead, force: true },
      ]);
    } catch {
      throw new HttpError(502, 'the fetch from upstream failed');
    }
  }

  // 3. The fork, where the patches come from it.
  let originFetched = false;
  const onFork = new Map<string, string>();
  let fork: Remote | undefined;
  if (fromOrigin) {
    fork = await remoteFor(deps, workspace, 'origin');
    try {
      for (const ref of await lsRefs(fork)) {
        onFork.set(ref.name, ref.oid);
      }
    } catch {
      throw new HttpError(502, 'origin fetch failed', 'origin_fetch_failed');
    }
    originFetched = true;
  }

  // 4. A rewritten upstream is reported, not refused.
  const advanced = upstreamHead !== workspace.upstream_head_sha;
  const forcePushed =
    advanced &&
    workspace.upstream_head_sha !== null &&
    upstreamPack.byteLength > 0 &&
    !descendsFrom(upstreamPack, upstreamHead, workspace.upstream_head_sha);

  // 5. The patch branches.
  const patches = await livePatches(deps.store, workspace.slug);
  const refreshable = patches.filter(
    (patch) =>
      patch.branch_name !== workspace.integration_branch &&
      (patch.status === 'active' || patch.status === 'conflict' || patch.status === 'disabled'),
  );
  const synced: RefreshedPatch[] = [];
  const moved = new Set<string>();

  if (fromOrigin && fork !== undefined) {
    const policy = settings['patch_divergence_policy'] === 'report' ? 'report' : 'replace';
    for (const patch of refreshable) {
      try {
        const outcome = await refreshPatchBranch(
          { store: deps.store, repos: deps.repos, clock: deps.clock, repository, remote: fork },
          patch,
          { policy, onFork },
        );
        synced.push(outcome);
        if (
          outcome.action !== 'none' &&
          outcome.action !== 'missing_on_origin' &&
          (patch.status === 'active' || patch.status === 'conflict')
        ) {
          moved.add(patch.branch_name);
        }
      } catch (err) {
        // The sync ends here, with what it has already reached kept
        // and a rebuild owed for the branches that did move.
        // A branch that did move still owes a rebuild, and the
        // outcomes already recorded stay recorded.
        await maybeQueueRebuild(deps, workspace, moved.size > 0, at);
        throw new HttpError(
          500,
          err instanceof RefreshFailure ? err.message : 'failed to update patch branch',
        );
      }
    }
  } else {
    await clearOriginFields(deps.store, workspace.slug, at);
  }

  // 6. What the upstream has taken.
  const mergedBranches: string[] = [];
  if (advanced || moved.size > 0) {
    const detection = (settings['squash_merge_detection'] as Detection) ?? 'both';
    // Where this sync fetched nothing new, the upstream's history is
    // still the hub's to read: a patch may have landed before it.
    const read =
      upstreamPack.byteLength > 0
        ? upstreamPack
        : await deps.repos.servePack(repository, [upstreamHead]).catch(() => new Uint8Array(0));
    const history = historyOf(read, upstreamHead);
    const refs = new Map((await deps.repos.listRefs(repository)).map((ref) => [ref.name, ref.oid]));
    for (const patch of patches.filter(
      (one) => one.status === 'active' || one.status === 'conflict',
    )) {
      const tip = refs.get(`refs/heads/${patch.branch_name}`);
      const tree = tip === undefined ? undefined : await treeOf(deps, repository, tip);
      if (!landedUpstream(patch, tip, tree, history, detection)) {
        continue;
      }
      mergedBranches.push(patch.branch_name);
      await deps.store.run(
        `UPDATE patches SET status = 'merged_upstream', origin_sync_state = NULL,
           origin_sha = NULL, origin_synced_at = NULL, updated_at = ? WHERE id = ?`,
        [at, patch.id],
      );
    }
  }

  // 7. What the sync leaves on the workspace. sync_error is not used
  // on this path: the status says what happened.
  await updateWorkspace(
    deps.store,
    workspace.slug,
    {
      last_sync_at: at,
      ...(advanced ? { upstream_head_sha: upstreamHead } : {}),
    },
    at,
  );

  // 8. One rebuild, where anything the rebuild cares about changed.
  const worth = advanced || moved.size > 0 || mergedBranches.length > 0;
  const queued = await maybeQueueRebuild(deps, workspace, worth, at);

  return {
    patches_merged: mergedBranches,
    force_push_detected: forcePushed,
    origin_fetched: originFetched,
    ...queued,
    ...(fromOrigin
      ? {
          patches_synced: synced,
          patches_diverged: synced
            .filter((one) => one.state === 'diverged')
            .map((one) => one.branch_name),
        }
      : {}),
  };
}

/** The tree one commit holds, read through the store. */
async function treeOf(
  deps: CarryPatchDeps,
  repository: string,
  oid: string,
): Promise<string | undefined> {
  try {
    const object = await deps.repos.readObject(repository, oid);
    if (object === undefined) {
      return undefined;
    }
    const text: string = Buffer.from(object.data).toString('latin1');
    return /^tree ([0-9a-f]{40})$/m.exec(text)?.[1];
  } catch {
    return undefined;
  }
}

/**
 * Queues the one rebuild a sync may owe. Never a second: a rebuild
 * already queued or running will see whatever this sync did.
 */
async function maybeQueueRebuild(
  deps: CarryPatchDeps,
  workspace: WorkspaceRecord,
  worth: boolean,
  at: string,
): Promise<{ rebuild_triggered: boolean; rebuild_job_id?: string }> {
  const settings = effectiveSettings(workspace.settings);
  if (!worth || settings['auto_rebuild_after_sync'] !== true) {
    return { rebuild_triggered: false };
  }
  if ((await pendingRebuild(deps.store, workspace.slug)) !== undefined) {
    return { rebuild_triggered: false };
  }

  const id = newIdentifier();
  await insertRebuild(
    deps.store,
    {
      id,
      workspace_slug: workspace.slug,
      strategy: (settings['rebuild_strategy'] as 'rebase' | 'merge') ?? 'rebase',
      fail_mode: (settings['rebuild_fail_mode'] as 'fail_fast' | 'continue') ?? 'fail_fast',
      submitted_by: 'system:sync',
    },
    at,
  );
  if (deps.jobs !== undefined) {
    await queueRebuild(deps.jobs, id);
  }
  return { rebuild_triggered: true, rebuild_job_id: id };
}
