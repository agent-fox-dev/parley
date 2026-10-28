import type { JobQueue } from '../platform/jobs.js';
import { type MergeRunDeps, runMerge } from '../merge/run.js';

/** The kind of queued work a merge is. */
export const MERGE = 'merge';

export interface MergePayload {
  id: string;
  [key: string]: unknown;
}

/** Queues a merge to run outside the request that asked for it. */
export async function queueMerge(jobs: JobQueue, id: string): Promise<string> {
  return jobs.enqueue(MERGE, { id } satisfies MergePayload);
}

/**
 * One merge. A job that could not have the workspace within its
 * patience asks to be run again rather than failing: the workspace is
 * busy, which is not the merge's fault.
 */
export async function runMergeJob(
  deps: MergeRunDeps,
  payload: MergePayload,
): Promise<'done' | 'more'> {
  return (await runMerge(deps, payload.id)) === 'again' ? 'more' : 'done';
}
