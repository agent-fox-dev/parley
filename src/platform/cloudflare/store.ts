import type { RelationalStore } from '../store.js';

/** The relational store on Cloudflare, over a D1 binding. */
export function d1Store(db: D1Database): RelationalStore {
  return {
    async ping(): Promise<void> {
      await db.prepare('SELECT 1').first();
    },
  };
}
