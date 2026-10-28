import type { LogSink } from '../http/router.js';
import type { JobQueue } from '../platform/jobs.js';
import type { Locks } from '../platform/lock.js';
import { COPY_IN, type CopyInDeps, type CopyInPayload, runCopyIn } from './copy_in.js';
import { REBUILD, type RebuildPayload, runRebuildJob } from './rebuild.js';

/** How many jobs one drain takes on before leaving the rest for the next. */
const DEFAULT_BUDGET = 10;

/**
 * Takes work off the queue and runs it. A job that asks to continue is
 * put back with what it has done, so a copy too large for one unit
 * carries on in the next rather than starting again.
 *
 * Both targets call this: the Worker from its cron trigger, the host
 * process from an interval.
 */
export async function drainJobs(
  deps: CopyInDeps & { locks?: Locks },
  options: { budget?: number; log?: LogSink } = {},
): Promise<number> {
  const queue: JobQueue = deps.jobs;
  let ran = 0;

  for (let taken = 0; taken < (options.budget ?? DEFAULT_BUDGET); taken += 1) {
    const job = await queue.claim();
    if (job === undefined) {
      return ran;
    }
    ran += 1;

    try {
      if (job.kind === REBUILD) {
        // A rebuild needs the workspace lock at its start and its end,
        // so a deployment without one cannot run it.
        if (deps.locks === undefined) {
          await queue.fail(job.id, 'this deployment has no workspace lock to rebuild under');
          continue;
        }
        await runRebuildJob({ ...deps, locks: deps.locks }, job.payload as RebuildPayload);
        await queue.complete(job.id);
        continue;
      }
      if (job.kind !== COPY_IN) {
        await queue.fail(job.id, `no handler for ${job.kind}`);
        continue;
      }
      const payload = job.payload as CopyInPayload;
      const outcome = await runCopyIn(deps, payload);
      if (outcome === 'more') {
        await queue.release(job.id, payload);
      } else {
        await queue.complete(job.id);
      }
    } catch (err) {
      // The handler records what it can on the workspace; the queue
      // records that this job will not be taken up again.
      options.log?.error?.('a job failed', err);
      await queue.fail(job.id, (err as Error).message.slice(0, 500));
    }
  }

  return ran;
}
