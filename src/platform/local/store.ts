import { DatabaseSync } from 'node:sqlite';
import type { RelationalStore } from '../store.js';

/** The relational store off Cloudflare, over SQLite on the host's own disk. */
export function sqliteStore(path: string): RelationalStore {
  let db: DatabaseSync | undefined;

  return {
    async ping(): Promise<void> {
      db ??= new DatabaseSync(path);
      db.prepare('SELECT 1').get();
      await Promise.resolve();
    },
  };
}
