import { DatabaseSync } from 'node:sqlite';
import type { RelationalStore } from '../store.js';

/**
 * The relational store off Cloudflare, over SQLite on the host's own disk.
 *
 * A ping opens and closes its own connection, so that it reports whether the
 * store answers now rather than whether it answered when the process started.
 */
export function sqliteStore(path: string): RelationalStore {
  return {
    async ping(): Promise<void> {
      const db = new DatabaseSync(path);
      try {
        db.prepare('SELECT 1').get();
      } finally {
        db.close();
      }
      await Promise.resolve();
    },
  };
}
