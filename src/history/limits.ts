import { HttpError } from '../http/errors.js';

/**
 * The sizes one history operation may work at. Worker CPU and memory
 * bound the work, and an operation that stalls half way through or is
 * killed by the platform is worse than one that refuses before it
 * starts: the refusal says which ceiling was reached, and the caller
 * can split the work.
 *
 * The figures are the ones the source PRD states, confirmed by the
 * measurement ADR 9 records.
 */
export interface EngineLimits {
  /** The most commits one rebase or merge may replay. */
  patches: number;
  /** The most packed bytes an operation may read a repository as. */
  repositoryBytes: number;
  /** The most paths one patch may change. */
  changedFiles: number;
  /** The most bytes of content one patch may carry. */
  diffBytes: number;
}

export const ENGINE_LIMITS: EngineLimits = {
  patches: 50,
  repositoryBytes: 500 * 1024 * 1024,
  changedFiles: 1000,
  diffBytes: 10 * 1024 * 1024,
};

/** What an operation was measured at, in whole or in part. */
export type Measured = Partial<Record<keyof EngineLimits, number>>;

/** How many megabytes a byte count is, as a refusal should say it. */
function megabytes(bytes: number): string {
  return `${String(Math.round((bytes / (1024 * 1024)) * 10) / 10)} MB`;
}

/**
 * How each ceiling reads when it is reached. The message says what was
 * asked for and what is allowed, because a caller that is told only
 * "too large" cannot tell how much to split off.
 */
const REFUSALS: Record<keyof EngineLimits, (measured: number, allowed: number) => string> = {
  patches: (measured, allowed) =>
    `this operation replays ${String(measured)} patches, and at most ${String(allowed)} patches may be replayed at once`,
  repositoryBytes: (measured, allowed) =>
    `this repository holds ${megabytes(measured)} of packed objects, and an operation may work on at most ${megabytes(allowed)}`,
  changedFiles: (measured, allowed) =>
    `this patch changes ${String(measured)} files, and a patch may change at most ${String(allowed)} files`,
  diffBytes: (measured, allowed) =>
    `this patch carries ${megabytes(measured)} of diff, and a patch may carry at most ${megabytes(allowed)}`,
};

/**
 * Refuses an operation measured past any of the ceilings, naming the
 * one it reached. It is called before the work rather than during it,
 * so a refused operation has written nothing.
 */
export function withinLimits(measured: Measured, limits: EngineLimits = ENGINE_LIMITS): void {
  for (const [name, allowed] of Object.entries(limits) as Array<[keyof EngineLimits, number]>) {
    const reached = measured[name];
    if (reached !== undefined && reached > allowed) {
      throw new HttpError(413, REFUSALS[name](reached, allowed));
    }
  }
}
