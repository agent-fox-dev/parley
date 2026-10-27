import type { RelationalStore, Row, Statement } from '../store.js';

/** The relational store on Cloudflare, over a D1 binding. */
export function d1Store(db: D1Database): RelationalStore {
  const bound = (statement: Statement) =>
    db.prepare(statement.sql).bind(...((statement.params ?? []) as unknown[]));

  return {
    async ping(): Promise<void> {
      await db.prepare('SELECT 1').first();
    },

    async all<T extends Row = Row>(sql: string, params?: readonly unknown[]): Promise<T[]> {
      const answer = await bound({ sql, ...(params === undefined ? {} : { params }) }).all<T>();
      return answer.results;
    },

    async run(sql: string, params?: readonly unknown[]): Promise<void> {
      await bound({ sql, ...(params === undefined ? {} : { params }) }).run();
    },

    async batch(statements: readonly Statement[]): Promise<void> {
      // D1 applies a batch in one transaction, so a failure part way leaves
      // nothing behind.
      await db.batch(statements.map(bound));
    },
  };
}
