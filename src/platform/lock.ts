/** A lock that is held until it is released, or until its lease runs out. */
export interface LockHandle {
  release(): Promise<void>;
}

/**
 * Mutual exclusion over a named thing, shared by every instance of the
 * service rather than held inside one of them.
 */
export interface Locks {
  /**
   * Takes the lock, or answers undefined when somebody else holds it. It
   * never waits: a caller that cannot have the lock learns so at once.
   */
  take(key: string): Promise<LockHandle | undefined>;
}
