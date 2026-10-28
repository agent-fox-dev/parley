import type { Clock, RelationalStore } from '../platform/index.js';
import { type Conflict, conflictIdentity } from './conflict.js';

export interface RecordedResolution {
  identity: string;
  /** Where the conflict was met, when that is known. */
  path?: string;
  resolved: boolean;
  recordedAt: string;
}

/**
 * Where resolutions live. Git keeps them in a directory beside the
 * repository; this service has no directory, so they are rows kept
 * beyond the process that recorded them.
 */
export interface ResolutionStore {
  /** Remembers how a conflict was resolved, against its identity. */
  record(repository: string, conflict: Conflict, resolution: string): Promise<string>;
  /** The resolution for a conflict, when one was recorded. */
  replay(repository: string, conflict: Conflict): Promise<string | undefined>;
  list(repository: string): Promise<RecordedResolution[]>;
  /** Forgets one by its identity; answers whether it was there. */
  forget(repository: string, identity: string): Promise<boolean>;
  /** Forgets every resolution recorded at a path; answers how many. */
  forgetPath(repository: string, path: string): Promise<number>;
}

export function resolutionStore(store: RelationalStore, clock: Clock): ResolutionStore {
  return {
    async record(repository, conflict, resolution): Promise<string> {
      const identity = await conflictIdentity(conflict);
      await store.run(
        `INSERT INTO resolutions (repository, identity, path, resolved, content, recorded_at)
         VALUES (?, ?, ?, 1, ?, ?)
         ON CONFLICT (repository, identity) DO UPDATE SET path = excluded.path,
                                                          resolved = 1,
                                                          content = excluded.content,
                                                          recorded_at = excluded.recorded_at`,
        [repository, identity, conflict.path, resolution, clock.now().toISOString()],
      );
      return identity;
    },

    async replay(repository, conflict): Promise<string | undefined> {
      const identity = await conflictIdentity(conflict);
      const rows = await store.all<{ content: string | null; resolved: number }>(
        'SELECT content, resolved FROM resolutions WHERE repository = ? AND identity = ?',
        [repository, identity],
      );
      const row = rows[0];
      return row === undefined || Number(row.resolved) !== 1 || row.content === null
        ? undefined
        : row.content;
    },

    async list(repository): Promise<RecordedResolution[]> {
      const rows = await store.all<{
        identity: string;
        path: string | null;
        resolved: number;
        recorded_at: string;
      }>(
        `SELECT identity, path, resolved, recorded_at FROM resolutions
         WHERE repository = ? ORDER BY recorded_at, identity`,
        [repository],
      );
      return rows.map((row) => ({
        identity: row.identity,
        ...(row.path === null ? {} : { path: row.path }),
        resolved: Number(row.resolved) === 1,
        recordedAt: row.recorded_at,
      }));
    },

    async forget(repository, identity): Promise<boolean> {
      const before = await store.all(
        'SELECT identity FROM resolutions WHERE repository = ? AND identity = ?',
        [repository, identity],
      );
      await store.run('DELETE FROM resolutions WHERE repository = ? AND identity = ?', [
        repository,
        identity,
      ]);
      return before.length > 0;
    },

    async forgetPath(repository, path): Promise<number> {
      const before = await store.all(
        'SELECT identity FROM resolutions WHERE repository = ? AND path = ?',
        [repository, path],
      );
      await store.run('DELETE FROM resolutions WHERE repository = ? AND path = ?', [
        repository,
        path,
      ]);
      return before.length;
    },
  };
}
