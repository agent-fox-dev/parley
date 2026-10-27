import type { Clock } from './clock.js';
import type { LockHandle, Locks } from './lock.js';
import type { RelationalStore } from './store.js';

/**
 * How long a holder keeps the lock without saying anything further. An
 * instance that stops without releasing one holds it no longer than this,
 * so a workspace cannot be left busy for ever.
 */
const DEFAULT_LEASE_MS = 60_000;

/**
 * The lock in the relational store, so every instance of the service sees
 * the same one. Taking it is a single statement that writes only when the
 * lock is free or its lease has run out, which is what makes two instances
 * racing for it safe.
 */
export function storeLocks(
  store: RelationalStore,
  clock: Clock,
  options: { leaseMs?: number } = {},
): Locks {
  const leaseMs = options.leaseMs ?? DEFAULT_LEASE_MS;

  return {
    async take(key: string): Promise<LockHandle | undefined> {
      const holder = crypto.randomUUID();
      const now = clock.now().toISOString();
      const until = new Date(clock.now().getTime() + leaseMs).toISOString();

      await store.run(
        `INSERT INTO workspace_locks (key, holder, taken_at, expires_at)
         VALUES (?, ?, ?, ?)
         ON CONFLICT (key) DO UPDATE SET holder = excluded.holder,
                                         taken_at = excluded.taken_at,
                                         expires_at = excluded.expires_at
         WHERE workspace_locks.expires_at <= excluded.taken_at`,
        [key, holder, now, until],
      );

      const rows = await store.all<{ holder: string }>(
        'SELECT holder FROM workspace_locks WHERE key = ?',
        [key],
      );
      if (rows[0]?.holder !== holder) {
        return undefined;
      }

      return {
        release: async () => {
          // Only the holder releases it, so a lease that already ran out
          // and was taken by somebody else is not torn away from them.
          await store.run('DELETE FROM workspace_locks WHERE key = ? AND holder = ?', [
            key,
            holder,
          ]);
        },
      };
    },
  };
}
