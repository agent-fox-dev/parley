import { HttpError } from '../http/errors.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { historyEngine } from '../history/engine.js';
import { resolutionStore } from '../history/resolutions.js';
import { repositoryOf } from '../repo/repository.js';
import { livePatches } from '../patch/list.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * What a rebuild would do, without doing it.
 *
 * Every patch that is not disabled or merged upstream is tried, in
 * position order, against the upstream head and the patches before it
 * that would apply — so a prediction answers the question a person
 * actually has, which is "where will this stack break". A predicted
 * conflict does not stop the predictions after it.
 *
 * It writes nothing. The engine does the work in its own memory and
 * is never asked to hand it back, which is the difference between a
 * prediction and a rehearsal.
 */
export interface PreviewDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
}

export interface PreviewedPatch {
  patch_id: string;
  branch_name: string;
  position: number;
  outcome: 'would_succeed' | 'would_conflict';
  tree?: string;
  conflict_files?: string[];
  /** Why it was not tried, where it was not. */
  reason?: string;
}

export async function previewRebuild(
  deps: PreviewDeps,
  workspace: WorkspaceRecord,
): Promise<PreviewedPatch[]> {
  if (workspace.workspace_mode !== 'carry_patch') {
    throw new HttpError(
      400,
      'only a carry-patch workspace has a stack to predict',
      'workspace_mode_mismatch',
    );
  }

  const repository = repositoryOf(workspace);
  const refs = new Map((await deps.repos.listRefs(repository)).map((ref) => [ref.name, ref.oid]));
  const integration = workspace.integration_branch ?? 'integration';
  const onto =
    refs.get(`refs/heads/${workspace.branch ?? 'main'}`) ??
    workspace.upstream_head_sha ??
    refs.get(`refs/heads/${integration}`);
  if (onto === undefined) {
    throw new HttpError(400, 'this workspace has no upstream head to predict against');
  }

  const patches = (await livePatches(deps.store, workspace.slug)).filter(
    (patch) => patch.status !== 'disabled' && patch.status !== 'merged_upstream',
  );

  // A patch whose branch is not there would not stop a rebuild, so it
  // would not stop this either.
  const attempted = patches.filter((patch) => refs.has(`refs/heads/${patch.branch_name}`));
  const missing = patches.filter((patch) => !refs.has(`refs/heads/${patch.branch_name}`));

  const engine = historyEngine(deps.repos, {
    resolutions: resolutionStore(deps.store, deps.clock),
  });
  const now = deps.clock.now();
  const predicted = await engine.predict({
    repository,
    onto,
    branches: attempted.map((patch) => refs.get(`refs/heads/${patch.branch_name}`) ?? ''),
    author: {
      name: 'Parley',
      email: 'parley@agent-fox.dev',
      timestamp: Math.floor(now.getTime() / 1000),
      timezoneOffset: 0,
    },
  });

  const byPatch = new Map(attempted.map((patch, index) => [patch.id, predicted[index]]));
  return patches
    .map((patch): PreviewedPatch => {
      if (missing.includes(patch)) {
        return {
          patch_id: patch.id,
          branch_name: patch.branch_name,
          position: patch.position,
          outcome: 'would_succeed',
          reason: 'branch_not_found',
        };
      }
      const said = byPatch.get(patch.id);
      return said?.conflicts === undefined
        ? {
            patch_id: patch.id,
            branch_name: patch.branch_name,
            position: patch.position,
            outcome: 'would_succeed',
            ...(said?.tree === undefined ? {} : { tree: said.tree }),
          }
        : {
            patch_id: patch.id,
            branch_name: patch.branch_name,
            position: patch.position,
            outcome: 'would_conflict',
            conflict_files: said.conflicts,
          };
    })
    .toSorted((left, right) => left.position - right.position);
}
