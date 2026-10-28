import type { Hono } from 'hono';
import type { RelationalStore } from '../src/platform/index.js';

/**
 * How a page reads.
 *
 * Every page reads through the service's own API, as the signed-in
 * person, rather than through a path of its own into the store.
 * Two things follow from that and neither is incidental: the
 * interface shows exactly what that person's credential would be
 * given, and the access rules live in one place rather than two.
 *
 * Both targets serve one app, so "through the API" means
 * dispatching a request to that app rather than opening a socket to
 * ourselves.
 */
export interface ApiReader {
  /** A GET of an API path, answered as the person this reader is for. */
  read<T>(path: string): Promise<Read<T>>;
}

export type Read<T> = { ok: true; value: T } | { ok: false; status: number; what: string };

export interface ReaderDeps {
  app: Pick<Hono, 'request'>;
  mountPoint: string;
  /** The credential the page reads with, minted for the session's user. */
  credential: string;
}

/** The store is not a way a page may read; this says so in types. */
export type NoStore = RelationalStore extends never ? never : undefined;

export function apiReader(deps: ReaderDeps): ApiReader {
  return {
    async read<T>(path: string): Promise<Read<T>> {
      const response = await deps.app.request(`${deps.mountPoint}${path}`, {
        headers: { authorization: `Bearer ${deps.credential}` },
      });

      if (response.status >= 400) {
        return { ok: false, status: response.status, what: path };
      }
      return { ok: true, value: (await response.json()) as T };
    },
  };
}
