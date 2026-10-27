/** A statement and the values bound to its placeholders, in order. */
export interface Statement {
  sql: string;
  params?: readonly unknown[];
}

/** One row of a result, keyed by column name. */
export type Row = Record<string, unknown>;

/**
 * The relational store. Both platforms answer the same four calls, so the
 * service never learns which one it is running on.
 */
export interface RelationalStore {
  /** Resolves when the store answers, and rejects when it does not. */
  ping(): Promise<void>;
  /** The rows a statement selects, empty when it selects none. */
  all<T extends Row = Row>(sql: string, params?: readonly unknown[]): Promise<T[]>;
  /** Runs a statement that returns no rows. */
  run(sql: string, params?: readonly unknown[]): Promise<void>;
  /** Runs statements so that either all of them take effect or none does. */
  batch(statements: readonly Statement[]): Promise<void>;
}

/**
 * The store of a deployment that was configured with none. It answers the
 * readiness probe, because the service itself is up, and refuses every read
 * and write rather than pretending to hold data.
 */
const absent = () => Promise.reject(new Error('no relational store is configured'));

export function noStore(): RelationalStore {
  return {
    ping: () => Promise.resolve(),
    all: absent,
    run: absent,
    batch: absent,
  };
}
