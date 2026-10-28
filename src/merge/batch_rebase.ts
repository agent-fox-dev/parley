import { HttpError } from '../http/errors.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Identity, historyEngine } from '../history/engine.js';
import { resolutionStore } from '../history/resolutions.js';
import { branchNameRefusal } from '../push/checks.js';
import { repositoryOf } from '../repo/repository.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * Rebasing several branches onto a target, one after another, while
 * the caller waits.
 *
 * It is the merge job's arithmetic offered synchronously, for the
 * question "which of these still apply". A branch that conflicts is
 * reported and the rest are still tried: the answer is a survey, and
 * a survey that stops at the first problem is not one.
 */
export const MOST_BRANCHES = 20;

export interface BatchRebaseDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
}

export interface BatchRequest {
  target_ref: string;
  branches: string[];
}

export interface BranchOutcome {
  branch: string;
  status: 'ok' | 'conflict' | 'error';
  new_head?: string;
  conflict_files?: string[];
  error?: string;
}

/** What the body asks for, checked for syntax alone. */
export function readBatchBody(raw: unknown): BatchRequest {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const target = body['target_ref'];
  if (typeof target !== 'string' || target.length === 0) {
    throw new HttpError(400, 'target_ref is required');
  }
  const invalidTarget = branchNameRefusal(target);
  if (invalidTarget !== undefined) {
    throw new HttpError(400, `invalid target_ref: ${invalidTarget.message}`);
  }

  const branches = body['branches'];
  if (!Array.isArray(branches) || branches.length === 0) {
    throw new HttpError(400, 'branches must be a non-empty array');
  }
  if (branches.length > MOST_BRANCHES) {
    throw new HttpError(400, `at most ${String(MOST_BRANCHES)} branches may be rebased at once`);
  }
  for (const [index, branch] of branches.entries()) {
    if (typeof branch !== 'string' || branch.length === 0) {
      throw new HttpError(400, `invalid branches[${String(index)}]: a branch name is required`);
    }
    const invalid = branchNameRefusal(branch);
    if (invalid !== undefined) {
      throw new HttpError(400, `invalid branches[${String(index)}]: ${invalid.message}`);
    }
  }

  return { target_ref: target, branches: branches as string[] };
}

function who(clock: Clock): Identity {
  const now = clock.now();
  return {
    name: 'Parley',
    email: 'parley@agent-fox.dev',
    timestamp: Math.floor(now.getTime() / 1000),
    timezoneOffset: 0,
  };
}

/**
 * Rebases each branch onto the target in the order given. Every
 * branch ends either at its new head or at the head it had: a branch
 * that conflicted or failed is not left part way.
 */
export async function rebaseBranches(
  deps: BatchRebaseDeps,
  workspace: WorkspaceRecord,
  request: BatchRequest,
): Promise<BranchOutcome[]> {
  const repository = repositoryOf(workspace);
  const engine = historyEngine(deps.repos, {
    resolutions: resolutionStore(deps.store, deps.clock),
  });

  const results: BranchOutcome[] = [];
  for (const branch of request.branches) {
    const refs = new Map((await deps.repos.listRefs(repository)).map((ref) => [ref.name, ref.oid]));
    const onto = refs.get(`refs/heads/${request.target_ref}`);
    const tip = refs.get(`refs/heads/${branch}`);

    if (onto === undefined) {
      throw new HttpError(
        400,
        `target_ref "${request.target_ref}" does not exist in this workspace`,
      );
    }
    if (tip === undefined) {
      results.push({ branch, status: 'error', error: 'the branch does not exist' });
      continue;
    }

    try {
      const rebased = await engine.rebase({
        repository,
        branch: tip,
        onto,
        committer: who(deps.clock),
      });
      if (rebased.conflicts !== undefined) {
        // Reported, and the branch is where it was.
        results.push({ branch, status: 'conflict', conflict_files: rebased.conflicts });
        continue;
      }
      const head = rebased.commits?.at(-1) ?? onto;
      await deps.repos.updateRefs(repository, [
        { name: `refs/heads/${branch}`, oldOid: tip, newOid: head, force: true },
      ]);
      results.push({ branch, status: 'ok', new_head: head });
    } catch (err) {
      // One branch that could not be rebased does not end the survey.
      results.push({
        branch,
        status: 'error',
        error: err instanceof Error ? err.message : 'the rebase could not be made',
      });
    }
  }
  return results;
}
