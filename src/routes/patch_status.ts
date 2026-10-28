import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import { jsonResponse } from '../http/json.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { resolutionStore } from '../history/resolutions.js';
import { type PatchRecord, livePatches } from '../patch/list.js';
import { repositoryOf } from '../repo/repository.js';
import { rebuildsOf } from '../rebuild/record.js';
import { workspaceAccess } from '../workspace/access.js';
import { effectiveSettings } from '../workspace/settings.js';

/**
 * One answer for "how is the stack doing".
 *
 * Everything in it can be read elsewhere; the point is that a person
 * wanting to know whether their fork is healthy should not have to
 * make five requests and do arithmetic. The counts are derived from
 * the same list the answer carries, so the two cannot disagree.
 */
export interface PatchStatusConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  repos?: RepositoryStore;
}

/** What the latest rebuild made of one patch. */
function resultFor(
  patch: PatchRecord,
  results: Array<{ patch_id: string; outcome: string; conflict_files?: string[] }>,
): { last_rebuild_result: string | null; conflict_files?: string[] } {
  const said = results.find((one) => one.patch_id === patch.id);
  if (said === undefined) {
    return { last_rebuild_result: null };
  }
  const outcome =
    said.outcome === 'applied' ? 'success' : said.outcome === 'skipped' ? 'skipped' : 'conflict';
  return {
    last_rebuild_result: outcome,
    ...(outcome === 'conflict' && said.conflict_files !== undefined
      ? { conflict_files: said.conflict_files }
      : {}),
  };
}

async function patchStatus(c: Context, config: PatchStatusConfig): Promise<Response> {
  const { workspace } = await workspaceAccess(c, config.store, 'read');
  if (workspace.workspace_mode !== 'carry_patch') {
    throw new HttpError(400, 'only a carry-patch workspace has a patch stack');
  }

  const patches = await livePatches(config.store, workspace.slug);
  const jobs = await rebuildsOf(config.store, workspace.slug);
  const latest = jobs.find((one) => one.status !== 'queued') ?? jobs[0];
  const fromOrigin = effectiveSettings(workspace.settings)['patch_branch_source'] === 'origin';

  // Resolutions are a nicety here: a store that cannot answer should
  // not keep a person from seeing the rest of the dashboard.
  let resolutions = 0;
  try {
    resolutions = (await resolutionStore(config.store, config.clock).list(repositoryOf(workspace)))
      .length;
  } catch {
    resolutions = 0;
  }

  const integrationHead =
    config.repos === undefined
      ? ''
      : ((await config.repos.listRefs(repositoryOf(workspace)).catch(() => [])).find(
          (ref) => ref.name === `refs/heads/${workspace.integration_branch ?? 'integration'}`,
        )?.oid ?? '');

  const listed = patches.map((patch) => ({
    id: patch.id,
    branch_name: patch.branch_name,
    position: patch.position,
    status: patch.status,
    ...resultFor(patch, latest?.results ?? []),
    ...(fromOrigin && patch.origin_sync_state !== null
      ? {
          origin_sync_state: patch.origin_sync_state,
          ...(patch.origin_sha === null ? {} : { origin_sha: patch.origin_sha }),
          ...(patch.origin_synced_at === null ? {} : { origin_synced_at: patch.origin_synced_at }),
        }
      : {}),
  }));

  const counted = (status: string) => patches.filter((patch) => patch.status === status).length;
  const withOriginState = (state: string) =>
    fromOrigin ? patches.filter((patch) => patch.origin_sync_state === state).length : 0;

  return jsonResponse({
    workspace_slug: workspace.slug,
    workspace_mode: workspace.workspace_mode,
    status: workspace.status,
    ...(workspace.clone_error === null ? {} : { clone_error: workspace.clone_error }),
    ...(workspace.sync_error === null ? {} : { sync_error: workspace.sync_error }),
    head_sha: workspace.head_sha ?? '',
    git_url: workspace.git_url,
    upstream_url: workspace.upstream_url,
    upstream_head_sha: workspace.upstream_head_sha ?? '',
    integration_branch: workspace.integration_branch,
    integration_head_sha: integrationHead,
    last_sync_at: workspace.last_sync_at,
    last_rebuild: latest === undefined ? null : { id: latest.id, status: latest.status },
    patches: listed,
    summary: {
      total_patches: listed.length,
      active: counted('active'),
      merged_upstream: counted('merged_upstream'),
      conflict: counted('conflict'),
      disabled: counted('disabled'),
      total_rerere_resolutions: resolutions,
      patches_diverged: withOriginState('diverged'),
      patches_missing_on_origin: withOriginState('missing_on_origin'),
    },
  });
}

export function patchStatusRoutes(config: PatchStatusConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/patch-status`,
      scope: 'workspaces:read',
      handler: async (c) => patchStatus(c, config),
    },
  ];
}
