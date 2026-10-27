import { DatabaseSync } from 'node:sqlite';
import type { RelationalStore, Row, Statement } from '../store.js';

const values = (params?: readonly unknown[]) => (params ?? []) as never[];

/**
 * The relational store off Cloudflare, over SQLite on the host's own disk.
 *
 * Every call opens and closes its own connection, so the store reports
 * whether it answers now rather than whether it answered when the process
 * started, and a batch holds its connection open across one transaction.
 */
export function sqliteStore(path: string): RelationalStore {
  function connected<T>(use: (db: DatabaseSync) => T): T {
    const db = new DatabaseSync(path);
    try {
      return use(db);
    } finally {
      db.close();
    }
  }

  return {
    async ping(): Promise<void> {
      connected((db) => db.prepare('SELECT 1').get());
      await Promise.resolve();
    },

    async all<T extends Row = Row>(sql: string, params?: readonly unknown[]): Promise<T[]> {
      const rows = connected((db) => db.prepare(sql).all(...values(params)));
      return Promise.resolve(rows as T[]);
    },

    async run(sql: string, params?: readonly unknown[]): Promise<void> {
      connected((db) => db.prepare(sql).run(...values(params)));
      await Promise.resolve();
    },

    async batch(statements: readonly Statement[]): Promise<void> {
      connected((db) => {
        db.exec('BEGIN');
        try {
          for (const statement of statements) {
            db.prepare(statement.sql).run(...values(statement.params));
          }
          db.exec('COMMIT');
        } catch (err) {
          db.exec('ROLLBACK');
          throw err;
        }
      });
      await Promise.resolve();
    },
  };
}
