import type { JobQueue } from '../platform/jobs.js';
import type { Locks } from '../platform/lock.js';
import { type RebuildDeps, runRebuild } from '../rebuild/job.js';

/** The kind of queued work a rebuild is. */
export const REBUILD = 'rebuild';

export interface RebuildPayload {
  /** The job record the work reports into. */
  id: string;
  [key: string]: unknown;
}

/** Queues a rebuild to run outside the request that asked for it. */
export async function queueRebuild(jobs: JobQueue, id: string): Promise<string> {
  return jobs.enqueue(REBUILD, { id } satisfies RebuildPayload);
}

export interface RebuildJobDeps extends Omit<RebuildDeps, 'locks'> {
  locks: Locks;
}

/**
 * One rebuild, run to its end. A rebuild is not resumable: a run that
 * is interrupted leaves a job that says what it had reached, and the
 * next rebuild starts again from the upstream head.
 */
export async function runRebuildJob(
  deps: RebuildJobDeps,
  payload: RebuildPayload,
): Promise<'done'> {
  await runRebuild(deps, payload.id);
  return 'done';
}
