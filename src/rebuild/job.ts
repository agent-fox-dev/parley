import type { LogSink } from '../http/router.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { newIdentifier } from '../auth/credentials.js';
import { type Identity, historyEngine } from '../history/engine.js';
import { resolutionStore } from '../history/resolutions.js';
import { ZERO_OID, lsRefs, pushPack } from '../git/transport.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { closePositions, livePatches } from '../patch/list.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import { effectiveSettings } from '../workspace/settings.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import {
  type PatchOutcome,
  type RebuildRecord,
  STALE_SNAPSHOT,
  insertRebuild,
  pendingRebuild,
  rebuildById,
  updateRebuild,
} from './record.js';

/**
 * Recreating the integration branch from the upstream head plus the
 * patches.
 *
 * Two things shape it. The lock is held only at the start and the end,
 * as the reference requires, so a sync or a push may run while patches
 * are applied — which is why a tip that moves mid-run is noticed and
 * answered with one follow-up rather than ignored or fought over. And
 * each patch's result is written as it finishes, so a client polling
 * the job sees movement rather than a spinner.
 */
export interface RebuildDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  locks: Locks;
  clock: Clock;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

/** Where a rebuild keeps the head it is building, so it stays reachable. */
const buildingRef = (id: string) => `refs/hub/rebuild/${id}`;

function who(clock: Clock): Identity {
  const now = clock.now();
  return {
    name: 'Parley',
    email: 'parley@agent-fox.dev',
    timestamp: Math.floor(now.getTime() / 1000),
    timezoneOffset: 0,
  };
}

/** The upstream a carry-patch workspace rebuilds onto. */
async function upstreamRemote(deps: RebuildDeps, workspace: WorkspaceRecord) {
  const credential =
    deps.secrets === undefined
      ? undefined
      : await resolveGitCredential(
          deps.secrets,
          workspaceScope(workspace.slug),
          'upstream',
          deps.log,
        );
  const authorization = authorizationFor(credential);
  return {
    url: (workspace.upstream_url ?? workspace.git_url).replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };
}

/** Which patches a rebuild leaves out, and why. */
function skipReason(
  patch: { status: string },
  tips: ReadonlyMap<string, string>,
  branch: string,
): string | undefined {
  if (patch.status === 'merged_upstream') {
    return 'merged_upstream';
  }
  if (patch.status === 'disabled') {
    return 'disabled';
  }
  if (patch.status === 'deleted') {
    return 'deleted';
  }
  return tips.has(`refs/heads/${branch}`) ? undefined : 'branch_not_found';
}

/**
 * Runs one rebuild to its end. It answers the record as it left it,
 * so a caller that wants the outcome need not read it back.
 */
export async function runRebuild(deps: RebuildDeps, id: string): Promise<RebuildRecord> {
  const job = await rebuildById(deps.store, id);
  if (job === undefined) {
    throw new Error(`no rebuild named ${id}`);
  }
  const workspace = await findWorkspace(deps.store, job.workspace_slug);
  if (workspace === undefined) {
    await updateRebuild(deps.store, id, {
      status: 'failed',
      error: 'the workspace was deleted while its rebuild was queued',
      finished_at: deps.clock.now().toISOString(),
    });
    return (await rebuildById(deps.store, id)) as RebuildRecord;
  }

  const repository = repositoryOf(workspace);
  const integration = workspace.integration_branch ?? 'integration';
  const settings = effectiveSettings(workspace.settings);
  const engine = historyEngine(deps.repos, {
    // A conflict met before and settled by hand is replayed rather
    // than met again.
    resolutions: resolutionStore(deps.store, deps.clock),
  });

  // The start: the lock is taken, the upstream head is read, and the
  // lock is let go before any patch is applied.
  const started = await withWorkspaceLock(deps.locks, workspace.slug, async () => {
    const refs = await deps.repos.listRefs(repository);
    const previous = refs.find((ref) => ref.name === `refs/heads/${integration}`)?.oid ?? null;

    let upstream: string | undefined;
    try {
      const remote = await upstreamRemote(deps, workspace);
      const branch = workspace.branch ?? 'main';
      const theirs = await lsRefs(remote);
      upstream =
        theirs.find((ref) => ref.name === `refs/heads/${branch}`)?.oid ??
        theirs.find((ref) => ref.name === 'refs/heads/main')?.oid;
    } catch {
      // A rebuild of what the hub already has is still a rebuild.
      upstream = undefined;
    }
    upstream ??= workspace.upstream_head_sha ?? previous ?? undefined;

    await updateRebuild(deps.store, id, {
      status: 'running',
      started_at: deps.clock.now().toISOString(),
      upstream_head: upstream ?? null,
      previous_integration_head: previous,
    });
    return { upstream, previous, tips: new Map(refs.map((ref) => [ref.name, ref.oid])) };
  });

  if (started.upstream === undefined) {
    await updateRebuild(deps.store, id, {
      status: 'failed',
      error: 'this workspace has no upstream head to rebuild onto',
      finished_at: deps.clock.now().toISOString(),
    });
    return (await rebuildById(deps.store, id)) as RebuildRecord;
  }

  // The middle: no lock, so anything else may run beside it.
  const patches = await livePatches(deps.store, workspace.slug);
  const results: PatchOutcome[] = [];
  let head = started.upstream;
  let failed: string | undefined;

  for (const patch of patches) {
    const branch = patch.branch_name;
    const skipped = skipReason(patch, started.tips, branch);
    const record = (outcome: PatchOutcome) => {
      results.push(outcome);
      return updateRebuild(deps.store, id, { results });
    };

    if (skipped !== undefined) {
      await record({
        patch_id: patch.id,
        branch_name: branch,
        position: patch.position,
        outcome: 'skipped',
        reason: skipped,
      });
      continue;
    }

    const tip = started.tips.get(`refs/heads/${branch}`) ?? '';
    const applied =
      job.strategy === 'merge'
        ? await engine
            .mergeCommit({
              repository,
              ours: head,
              theirs: tip,
              author: who(deps.clock),
              message: `merge ${branch}`,
            })
            .then((out) => ({ head: out.commit, conflicts: out.conflicts }))
        : await engine
            .rebase({ repository, branch: tip, onto: head, committer: who(deps.clock) })
            .then((out) => ({ head: out.commits?.at(-1) ?? head, conflicts: out.conflicts }));

    if (applied.conflicts !== undefined) {
      await record({
        patch_id: patch.id,
        branch_name: branch,
        position: patch.position,
        outcome: 'conflict',
        conflict_files: applied.conflicts,
      });
      await deps.store.run(
        'UPDATE patches SET status = ?, conflict_files = ?, updated_at = ? WHERE id = ?',
        ['conflict', JSON.stringify(applied.conflicts), deps.clock.now().toISOString(), patch.id],
      );
      if (job.fail_mode === 'fail_fast') {
        failed = `${branch} conflicted and this workspace fails fast`;
        break;
      }
      continue;
    }

    head = applied.head ?? head;
    // Kept under a ref of the job's own: the engine keeps its result
    // under one ref that the next operation overwrites.
    await deps.repos.updateRefs(repository, [
      { name: buildingRef(id), oldOid: null, newOid: head, force: true },
    ]);
    await record({
      patch_id: patch.id,
      branch_name: branch,
      position: patch.position,
      outcome: 'applied',
    });
  }

  if (failed !== undefined) {
    await updateRebuild(deps.store, id, {
      status: 'failed',
      error: failed,
      results,
      finished_at: deps.clock.now().toISOString(),
    });
    return (await rebuildById(deps.store, id)) as RebuildRecord;
  }

  // The end: the lock again, to set the branch and tidy the list.
  const pushed = await withWorkspaceLock(deps.locks, workspace.slug, async () => {
    const at = deps.clock.now().toISOString();
    await deps.repos.updateRefs(repository, [
      { name: `refs/heads/${integration}`, oldOid: null, newOid: head, force: true },
    ]);

    // A patch the upstream has taken is carried no longer.
    for (const patch of patches.filter((one) => one.status === 'merged_upstream')) {
      await deps.store.run(
        'UPDATE patches SET status = ?, deleted_at = ?, updated_at = ? WHERE id = ?',
        ['deleted', at, at, patch.id],
      );
    }
    await closePositions(deps.store, workspace.slug, at);

    if (settings['rebuild_push_integration_branch'] !== true) {
      return false;
    }
    return mirrorIntegration(deps, workspace, integration, head);
  });

  await updateRebuild(deps.store, id, {
    status: 'completed',
    results,
    new_integration_head: head,
    integration_branch_pushed: pushed,
    finished_at: deps.clock.now().toISOString(),
  });

  await queueFollowUp(deps, workspace, started.tips, id);
  return (await rebuildById(deps.store, id)) as RebuildRecord;
}

/**
 * Force-pushes the rebuilt branch to the fork. It is best effort: the
 * hub's copy is the one that counts, and a fork that will not take it
 * does not undo the rebuild.
 */
async function mirrorIntegration(
  deps: RebuildDeps,
  workspace: WorkspaceRecord,
  branch: string,
  head: string,
): Promise<boolean> {
  try {
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
    const pack = await deps.repos.servePack(repositoryOf(workspace), [head]);
    const report = await pushPack(remote, {
      commands: [{ name: `refs/heads/${branch}`, oldOid: ZERO_OID, newOid: head }],
      pack,
    });
    return report.unpacked && report.refs.every((ref) => ref.ok);
  } catch {
    deps.log?.info?.('the rebuilt integration branch was not pushed to the fork', {
      workspace: workspace.slug,
    });
    return false;
  }
}

/**
 * Queues one follow-up where a tip moved while the rebuild ran. The
 * finished job keeps the result it built from the tips it started
 * with; the follow-up is what takes the newer ones into account.
 */
async function queueFollowUp(
  deps: RebuildDeps,
  workspace: WorkspaceRecord,
  started: ReadonlyMap<string, string>,
  from: string,
): Promise<void> {
  const now = new Map(
    (await deps.repos.listRefs(repositoryOf(workspace))).map((ref) => [ref.name, ref.oid]),
  );
  const patches = await livePatches(deps.store, workspace.slug);
  const moved = patches.some((patch) => {
    const ref = `refs/heads/${patch.branch_name}`;
    return started.has(ref) && started.get(ref) !== now.get(ref);
  });
  if (!moved) {
    return;
  }
  // Never twice: one follow-up is what the reference asks for, and a
  // second would chase the first for ever.
  if ((await pendingRebuild(deps.store, workspace.slug)) !== undefined) {
    return;
  }

  const settings = effectiveSettings(workspace.settings);
  await insertRebuild(
    deps.store,
    {
      id: newIdentifier(),
      workspace_slug: workspace.slug,
      strategy: (settings['rebuild_strategy'] as 'rebase' | 'merge') ?? 'rebase',
      fail_mode: (settings['rebuild_fail_mode'] as 'fail_fast' | 'continue') ?? 'fail_fast',
      submitted_by: STALE_SNAPSHOT,
      follow_up_of: from,
    },
    deps.clock.now().toISOString(),
  );
}
