/**
 * The relational store. This package needs only to know whether it answers;
 * later packages add the reads and writes they need.
 */
export interface RelationalStore {
  /** Resolves when the store answers, and rejects when it does not. */
  ping(): Promise<void>;
}
