import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { descendsFrom } from '../git/ancestry.js';
import { fetchPack, lsRefs } from '../git/transport.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { AFTER_RESET } from '../rebuild/record.js';
import { triggerRebuild } from '../rebuild/trigger.js';
import type { PatchRecord } from './list.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { effectiveSettings } from '../workspace/settings.js';

/**
 * Setting a patch branch back to the fork's.
 *
 * What happens is decided by the first rule that fits, and the rules
 * are ordered so that nothing is written where nothing needs to be:
 * a branch the hub does not have is created, one already at the
 * fork's tip is left alone, one the fork has moved ahead of is
 * fast-forwarded, and anything else is replaced — with the tip it had
 * kept, because that tip may be the only copy of work the hub is
 * about to drop.
 */
export type ResetAction = 'created' | 'none' | 'fast_forwarded' | 'replaced';

export interface ResetDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
  jobs?: JobQueue;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

export interface ResetOutcome {
  action: ResetAction;
  /** The tip the hub kept, where the move discarded one. */
  replaced_sha?: string;
  rebuild_triggered: boolean;
  rebuild_job_id?: string;
}

/** Whether this patch may be reset at all, in the documented order. */
export function resetRefusal(
  workspace: WorkspaceRecord,
  patch: PatchRecord | undefined,
): HttpError | undefined {
  if (workspace.status === 'archived' || workspace.workspace_mode !== 'carry_patch') {
    return new HttpError(400, 'only a carry-patch workspace that is not archived has patches');
  }
  if (workspace.status !== 'ready') {
    return new HttpError(409, 'this workspace is not ready');
  }
  if (patch === undefined) {
    return new HttpError(404, 'patch not found');
  }
  if (patch.branch_name === workspace.integration_branch) {
    return new HttpError(400, 'the integration branch is not a patch branch');
  }
  if (patch.status === 'merged_upstream' || patch.status === 'deleted') {
    return new HttpError(409, `a ${patch.status} patch is not reset to the fork`);
  }
  return undefined;
}

export async function resetToOrigin(
  deps: ResetDeps,
  workspace: WorkspaceRecord,
  patch: PatchRecord,
): Promise<ResetOutcome> {
  const repository = repositoryOf(workspace);
  const ref = `refs/heads/${patch.branch_name}`;
  const settings = effectiveSettings(workspace.settings);

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

  let theirs: string | undefined;
  try {
    theirs = (await lsRefs(remote)).find((one) => one.name === ref)?.oid;
  } catch {
    throw new HttpError(502, 'the fetch from the fork failed', 'origin_fetch_failed');
  }
  if (theirs === undefined) {
    throw new HttpError(409, 'the fork has no such branch', 'missing_on_origin');
  }

  const here = (await deps.repos.listRefs(repository)).find((one) => one.name === ref)?.oid;
  const at = deps.clock.now().toISOString();

  let action: ResetAction;
  let replaced: string | undefined;

  if (here === theirs) {
    action = 'none';
  } else {
    let pack: Uint8Array;
    try {
      pack = await fetchPack(remote, {
        wants: [theirs],
        ...(here === undefined ? {} : { haves: [here] }),
      });
    } catch {
      throw new HttpError(502, 'the fetch from the fork failed', 'origin_fetch_failed');
    }

    const forward = here !== undefined && descendsFrom(pack, theirs, here);
    action = here === undefined ? 'created' : forward ? 'fast_forwarded' : 'replaced';

    try {
      await deps.repos.receivePack(repository, pack, [
        { name: ref, oldOid: here ?? null, newOid: theirs, force: true },
        // The tip being dropped is kept, superseding an earlier one:
        // it may be the only copy of what the hub is discarding.
        ...(action === 'replaced' && here !== undefined
          ? [
              {
                name: `refs/hub/replaced/${patch.branch_name}`,
                oldOid: null,
                newOid: here,
                force: true,
              },
            ]
          : []),
      ]);
    } catch (err) {
      // Another operation may have moved the branch under us.
      throw new HttpError(
        409,
        `the branch changed while it was being reset: ${err instanceof Error ? err.message : 'retry'}`,
        'ref_changed',
      );
    }
    if (action === 'replaced') {
      replaced = here;
    }
  }

  // The patch's own record of how it stands against the fork.
  if (settings['patch_branch_source'] === 'origin') {
    await deps.store.run(
      'UPDATE patches SET origin_sync_state = ?, origin_sha = ?, origin_synced_at = ?, updated_at = ? WHERE id = ?',
      ['in_sync', theirs, at, at, patch.id],
    );
  }

  const moved = action !== 'none';
  const worthRebuilding = patch.status === 'active' || patch.status === 'conflict';
  const settled = {
    action,
    ...(replaced === undefined ? {} : { replaced_sha: replaced }),
  };
  if (!moved || !worthRebuilding || settings['auto_rebuild_after_sync'] !== true) {
    return { ...settled, rebuild_triggered: false };
  }

  // The same trigger the push and the sync use: a workspace owes at
  // most one rebuild at a time, whatever asked for it.
  return {
    ...settled,
    ...(await triggerRebuild(
      {
        store: deps.store,
        clock: deps.clock,
        ...(deps.jobs === undefined ? {} : { jobs: deps.jobs }),
        ...(deps.log === undefined ? {} : { log: deps.log }),
      },
      workspace,
      AFTER_RESET,
    )),
  };
}
