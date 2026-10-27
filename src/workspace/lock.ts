import { HttpError } from '../http/errors.js';
import type { Locks } from '../platform/lock.js';

/** The name a workspace's lock is held under. */
export function workspaceLockKey(slug: string): string {
  return `workspace:${slug}`;
}

/**
 * Runs the work with the workspace's lock held, or refuses at once when
 * another operation has it. The lock is released however the work ends,
 * so a failure does not leave the workspace busy.
 */
export async function withWorkspaceLock<T>(
  locks: Locks,
  slug: string,
  work: () => Promise<T>,
): Promise<T> {
  const held = await locks.take(workspaceLockKey(slug));
  if (held === undefined) {
    throw new HttpError(409, 'another operation is working on this workspace', 'workspace_busy');
  }
  try {
    return await work();
  } finally {
    await held.release();
  }
}
