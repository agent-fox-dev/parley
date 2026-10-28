import type { Clock } from './clock.js';
import type { JobQueue, QueuedJob } from './jobs.js';
import type { RelationalStore } from './store.js';

/** How long a claimed job may run before another instance may take it. */
const DEFAULT_LEASE_MS = 5 * 60 * 1000;

/**
 * The queue in the relational store, so every instance of the service
 * sees the same work and a job survives the instance that took it. A
 * claim is one conditional statement, which is what makes two instances
 * racing for a job safe — the same shape the workspace lock uses (ADR 5).
 */
export function storeJobs(
  store: RelationalStore,
  clock: Clock,
  options: { leaseMs?: number } = {},
): JobQueue {
  const leaseMs = options.leaseMs ?? DEFAULT_LEASE_MS;

  return {
    async enqueue(kind: string, payload: Record<string, unknown>): Promise<string> {
      const id = crypto.randomUUID();
      const at = clock.now().toISOString();
      await store.run(
        `INSERT INTO jobs (id, kind, payload, state, attempts, reason, created_at, updated_at, claimed_until)
         VALUES (?, ?, ?, 'queued', 0, NULL, ?, ?, NULL)`,
        [id, kind, JSON.stringify(payload), at, at],
      );
      return id;
    },

    async claim(): Promise<QueuedJob | undefined> {
      const now = clock.now();
      const holder = crypto.randomUUID();
      const until = new Date(now.getTime() + leaseMs).toISOString();

      // One statement, so two instances cannot take the same job: only a
      // job nobody holds, or one whose holder's lease ran out, is taken.
      await store.run(
        `UPDATE jobs SET state = 'running', attempts = attempts + 1, claimed_until = ?,
                         reason = ?, updated_at = ?
         WHERE id = (
           SELECT id FROM jobs
           WHERE state = 'queued' OR (state = 'running' AND claimed_until <= ?)
           ORDER BY created_at LIMIT 1
         )`,
        [until, holder, now.toISOString(), now.toISOString()],
      );

      const rows = await store.all<{
        id: string;
        kind: string;
        payload: string;
        attempts: number;
      }>('SELECT id, kind, payload, attempts FROM jobs WHERE reason = ? AND state = ?', [
        holder,
        'running',
      ]);
      const row = rows[0];
      if (row === undefined) {
        return undefined;
      }
      await store.run('UPDATE jobs SET reason = NULL WHERE id = ?', [row.id]);

      return {
        id: row.id,
        kind: row.kind,
        payload: JSON.parse(row.payload) as Record<string, unknown>,
        attempts: Number(row.attempts),
      };
    },

    async release(id: string, payload: Record<string, unknown>): Promise<void> {
      await store.run(
        `UPDATE jobs SET state = 'queued', payload = ?, claimed_until = NULL, updated_at = ?
         WHERE id = ?`,
        [JSON.stringify(payload), clock.now().toISOString(), id],
      );
    },

    async complete(id: string): Promise<void> {
      await store.run(
        `UPDATE jobs SET state = 'done', claimed_until = NULL, updated_at = ? WHERE id = ?`,
        [clock.now().toISOString(), id],
      );
    },

    async fail(id: string, reason: string): Promise<void> {
      await store.run(
        `UPDATE jobs SET state = 'failed', reason = ?, claimed_until = NULL, updated_at = ?
         WHERE id = ?`,
        [reason, clock.now().toISOString(), id],
      );
    },

    async read(id: string): Promise<{ state: string; reason: string | null } | undefined> {
      const rows = await store.all<{ state: string; reason: string | null }>(
        'SELECT state, reason FROM jobs WHERE id = ?',
        [id],
      );
      return rows[0];
    },
  };
}
