import type { Clock, RelationalStore } from '../platform/index.js';

/**
 * Jobs that stopped making progress.
 *
 * A merge job runs somewhere that can be restarted, evicted or
 * simply killed. The queue redelivers what it holds, so a job whose
 * runner vanished is run again; what the queue cannot help with is a
 * job that was claimed, began, and never said anything more. That one
 * is dead-lettered, which is a state a person can requeue from —
 * better than a job that reads "running" for ever with nothing
 * running it.
 */
export const STALL_AFTER_MS = 15 * 60 * 1000;

/**
 * Dead-letters every merge that has been running without progress
 * for longer than it should, and answers how many it found.
 */
export async function sweepStalledMerges(
  store: RelationalStore,
  clock: Clock,
  options: { stallMs?: number } = {},
): Promise<number> {
  const now = clock.now();
  const cutoff = new Date(now.getTime() - (options.stallMs ?? STALL_AFTER_MS)).toISOString();

  const stalled = await store.all<{ id: string; retry_count: number }>(
    "SELECT id, retry_count FROM merges WHERE status = 'running' AND updated_at < ?",
    [cutoff],
  );
  if (stalled.length === 0) {
    return 0;
  }

  await store.batch(
    stalled.map((one) => ({
      sql: `UPDATE merges SET status = 'dead_letter', retry_count = ?, error = ?, updated_at = ?
            WHERE id = ? AND status = 'running'`,
      params: [
        Number(one.retry_count) + 1,
        'the merge stopped making progress and was given up on',
        now.toISOString(),
        one.id,
      ],
    })),
  );
  return stalled.length;
}
