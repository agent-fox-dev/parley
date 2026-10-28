import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import { newIdentifier } from '../auth/credentials.js';
import { queueRebuild } from '../jobs/rebuild.js';
import { patchBranchOf, registeredPatches } from '../patch/list.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { effectiveSettings } from '../workspace/settings.js';
import { AFTER_PUSH, insertRebuild, pendingRebuild } from './record.js';

/**
 * Queueing the one rebuild a workspace may owe.
 *
 * Three things ask for a rebuild — a push to a patch branch, a sync
 * and a reset — and a workspace never has more than one queued at a
 * time, whichever asked. A rebuild that has not started yet will see
 * whatever happened since it was queued, so a second would only
 * repeat the first.
 */
export interface TriggerDeps {
  store: RelationalStore;
  clock: Clock;
  jobs?: JobQueue;
  log?: LogSink;
}

export interface Triggered {
  rebuild_triggered: boolean;
  rebuild_job_id?: string;
}

/**
 * Queues a rebuild unless one is already owed. The caller says why it
 * is asking, which is what a person reading the job list sees.
 */
export async function triggerRebuild(
  deps: TriggerDeps,
  workspace: WorkspaceRecord,
  because: string,
): Promise<Triggered> {
  if ((await pendingRebuild(deps.store, workspace.slug)) !== undefined) {
    return { rebuild_triggered: false };
  }

  const settings = effectiveSettings(workspace.settings);
  const id = newIdentifier();
  await insertRebuild(
    deps.store,
    {
      id,
      workspace_slug: workspace.slug,
      strategy: (settings['rebuild_strategy'] as 'rebase' | 'merge') ?? 'rebase',
      fail_mode: (settings['rebuild_fail_mode'] as 'fail_fast' | 'continue') ?? 'fail_fast',
      submitted_by: because,
    },
    deps.clock.now().toISOString(),
  );
  if (deps.jobs !== undefined) {
    await queueRebuild(deps.jobs, id);
  }
  return { rebuild_triggered: true, rebuild_job_id: id };
}

/**
 * What an accepted push owes. A push that moved a registered patch
 * branch means the stack is out of date, so the workspace rebuilds —
 * unless its owner said not to.
 */
export async function afterPatchBranchPush(
  deps: TriggerDeps,
  workspace: WorkspaceRecord,
  accepted: readonly string[],
): Promise<Triggered> {
  if (workspace.workspace_mode !== 'carry_patch') {
    return { rebuild_triggered: false };
  }
  if (effectiveSettings(workspace.settings)['auto_rebuild_after_push'] !== true) {
    return { rebuild_triggered: false };
  }

  const patches = await registeredPatches(deps.store, workspace.slug);
  const touched = accepted.some((ref) => patchBranchOf(workspace, ref, patches) !== undefined);
  return touched ? triggerRebuild(deps, workspace, AFTER_PUSH) : { rebuild_triggered: false };
}
