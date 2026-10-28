import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';

/**
 * The most one workspace's repository may hold. The store reports no
 * storage error of its own, so the service counts the bytes it writes
 * and refuses past this itself (see ADR 2, decision 11).
 */
export const REPOSITORY_BYTE_CAP = 1024 * 1024 * 1024;

/** What one repository is currently accounted as holding. */
export async function repositoryBytes(store: RelationalStore, repository: string): Promise<number> {
  const rows = await store.all<{ bytes: number }>(
    'SELECT bytes FROM repository_usage WHERE repository = ?',
    [repository],
  );
  return Number(rows[0]?.bytes ?? 0);
}

/**
 * Refuses a write that would take a repository past the cap, before any
 * of it is written. The workspace is left exactly as it was, so what it
 * already holds is still readable.
 */
export async function reserveRepositoryBytes(
  store: RelationalStore,
  repository: string,
  incoming: number,
  at: string,
  cap: number = REPOSITORY_BYTE_CAP,
): Promise<void> {
  const held = await repositoryBytes(store, repository);
  if (held + incoming > cap) {
    throw new HttpError(
      507,
      `this repository may hold at most ${String(Math.round(cap / (1024 * 1024)))} MB, and this write would take it past that`,
    );
  }

  await store.run(
    `INSERT INTO repository_usage (repository, bytes, updated_at) VALUES (?, ?, ?)
     ON CONFLICT (repository) DO UPDATE SET bytes = repository_usage.bytes + excluded.bytes,
                                            updated_at = excluded.updated_at`,
    [repository, incoming, at],
  );
}

/** Forgets what a repository was accounted as holding, when it is removed. */
export async function releaseRepositoryBytes(
  store: RelationalStore,
  repository: string,
): Promise<void> {
  await store.run('DELETE FROM repository_usage WHERE repository = ?', [repository]);
}
