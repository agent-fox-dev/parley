import { type EventRecorder, record } from '../audit/recorder.js';
import type { LogSink } from '../http/router.js';
import type { Clock, Locks, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Identity, historyEngine } from '../history/engine.js';
import { resolutionStore } from '../history/resolutions.js';
import { repositoryOf } from '../repo/repository.js';
import { findWorkspace } from '../store/workspaces.js';
import { workspaceLockKey } from '../workspace/lock.js';
import { type MergeRecord, mergeById, updateMerge } from './job.js';

/**
 * Running one merge job.
 *
 * A merge is a rebase of the source onto the target followed by a
 * fast-forward of the target, so the history it leaves is the
 * source's commits on top of the target's with no merge commit —
 * the same operation the history engine proves against the real git
 * binary.
 *
 * It waits its turn for the workspace rather than being refused. That
 * is what a queue is for, and it is the one way a merge differs from
 * every other operation that touches a repository.
 */
export interface MergeRunDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  locks: Locks;
  clock: Clock;
  log?: LogSink;
  /** Where the job writes down how it ended. */
  events?: EventRecorder;
  /** How long to wait for the workspace before giving up its turn. */
  waitMs?: number;
}

/** How long a job waits for the workspace before trying again later. */
export const WAIT_FOR_LOCK_MS = 30_000;

/** How often it looks while it waits. */
const LOOK_EVERY_MS = 50;

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
 * Waits for the workspace rather than refusing it. A job that cannot
 * have it within its patience says so, and the queue runs it again.
 */
async function waitForWorkspace(
  deps: MergeRunDeps,
  slug: string,
): Promise<{ release: () => Promise<void> } | undefined> {
  // Counted in looks rather than read from the clock: the clock a
  // deployment injects may be a fixed one, and a deadline computed
  // from a clock that does not move is a loop that never ends.
  const looks = Math.max(1, Math.ceil((deps.waitMs ?? WAIT_FOR_LOCK_MS) / LOOK_EVERY_MS));
  for (let look = 0; look < looks; look += 1) {
    const held = await deps.locks.take(workspaceLockKey(slug));
    if (held !== undefined) {
      return held;
    }
    await new Promise((resolve) => setTimeout(resolve, LOOK_EVERY_MS));
  }
  return undefined;
}

/**
 * Runs the job to its end and answers the record it left. A job that
 * could not have the workspace in time is left queued for the next
 * turn rather than failed.
 */
export async function runMerge(deps: MergeRunDeps, id: string): Promise<'done' | 'again'> {
  const job = await mergeById(deps.store, id);
  if (job === undefined || job.status === 'cancelled') {
    // A job cancelled while it waited never runs.
    return 'done';
  }
  if (job.status === 'completed' || job.status === 'failed') {
    // Already answered for: a redelivery of finished work changes
    // nothing.
    return 'done';
  }
  // A job that reads `running` is one whose runner went away before
  // it could say otherwise. Running it again is safe, because a
  // merge is a rebase of what the branches hold now.

  const workspace = await findWorkspace(deps.store, job.workspace_slug);
  if (workspace === undefined) {
    await finish(deps, job, {
      status: 'failed',
      error: 'the workspace was deleted while its merge was queued',
    });
    return 'done';
  }

  const held = await waitForWorkspace(deps, job.workspace_slug);
  if (held === undefined) {
    return 'again';
  }

  try {
    // Checked once the lock is held: a cancel may have landed while
    // the job waited its turn.
    const now = await mergeById(deps.store, id);
    if (now?.status === 'cancelled') {
      return 'done';
    }
    await updateMerge(
      deps.store,
      id,
      { status: 'running', started_at: deps.clock.now().toISOString() },
      deps.clock.now().toISOString(),
    );

    const repository = repositoryOf(workspace);
    const refs = new Map((await deps.repos.listRefs(repository)).map((ref) => [ref.name, ref.oid]));
    const base = refs.get(`refs/heads/${job.target_branch}`);
    const source = refs.get(`refs/heads/${job.source_ref}`);
    if (base === undefined || source === undefined) {
      await finish(deps, job, {
        status: 'failed',
        error: `${base === undefined ? job.target_branch : job.source_ref} does not exist in this workspace`,
      });
      return 'done';
    }

    const engine = historyEngine(deps.repos, {
      resolutions: resolutionStore(deps.store, deps.clock),
    });
    const rebased = await engine.rebase({
      repository,
      branch: source,
      onto: base,
      committer: who(deps.clock),
    });

    if (rebased.conflicts !== undefined) {
      // The target is where it was: a conflict changes nothing.
      await finish(deps, job, {
        status: 'failed',
        conflict_files: rebased.conflicts,
        error: `${job.source_ref} conflicts with ${job.target_branch}`,
      });
      return 'done';
    }

    // The fast-forward is one ref write, so the target holds either
    // its previous head or the merged result and never anything
    // between the two.
    const merged = rebased.commits?.at(-1) ?? base;
    await deps.repos.updateRefs(repository, [
      {
        name: `refs/heads/${job.target_branch}`,
        oldOid: base,
        newOid: merged,
        force: false,
      },
    ]);

    await finish(deps, job, { status: 'completed', base_sha: base, merged_sha: merged });
    return 'done';
  } catch (err) {
    await finish(deps, job, {
      status: 'failed',
      error: err instanceof Error ? err.message : 'the merge could not be made',
    });
    return 'done';
  } finally {
    await held.release();
  }
}

/** Writes what became of a job, once. */
async function finish(
  deps: MergeRunDeps,
  job: MergeRecord,
  outcome: {
    status: 'completed' | 'failed';
    base_sha?: string;
    merged_sha?: string;
    conflict_files?: string[];
    error?: string;
  },
): Promise<void> {
  await updateMerge(
    deps.store,
    job.id,
    {
      status: outcome.status,
      ...(outcome.base_sha === undefined ? {} : { base_sha: outcome.base_sha }),
      ...(outcome.merged_sha === undefined ? {} : { merged_sha: outcome.merged_sha }),
      ...(outcome.conflict_files === undefined ? {} : { conflict_files: outcome.conflict_files }),
      ...(outcome.error === undefined ? {} : { error: outcome.error }),
    },
    deps.clock.now().toISOString(),
  );

  // The job has no caller to answer to, so the record of how it went
  // is the only account of it. The system is the actor for the same
  // reason: nobody was waiting on this.
  await record(
    deps.events,
    outcome.status === 'completed'
      ? {
          type: 'hub.merge.complete',
          workspace: job.workspace_slug,
          resourceId: job.id,
          actor: { id: '', type: 'system' },
          metadata: { base_sha: outcome.base_sha, merged_sha: outcome.merged_sha },
        }
      : {
          type: 'hub.merge.fail',
          workspace: job.workspace_slug,
          resourceId: job.id,
          actor: { id: '', type: 'system' },
          severity: 'error',
          metadata: { reason: outcome.error ?? 'the merge failed' },
        },
    deps.log,
  );
}
