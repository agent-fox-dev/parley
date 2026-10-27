import { describe, expect, it, vi } from 'vitest';
import { SERVER_ROUTE_PATHS } from '../routes/server.js';
import { buildApp } from './router.js';

const MOUNT_POINTS = ['/api/v1', '/api/v2', '/parley/api'];
const OTHER_METHODS = ['POST', 'PUT', 'PATCH', 'DELETE'];

function cross<A, B>(as: readonly A[], bs: readonly B[]): Array<[A, B]> {
  return as.flatMap((a) => bs.map((b): [A, B] => [a, b]));
}

describe('the server routes and the mount point', () => {
  it.each(cross(MOUNT_POINTS, SERVER_ROUTE_PATHS))(
    'serves %s root route %s whatever the mount point',
    async (mountPoint, path) => {
      const response = await buildApp({ mountPoint }).request(path);

      expect(response.status).not.toBe(404);
    },
  );

  it.each([
    ['the default', undefined, '/api/v1'],
    ['a configured prefix', '/api/v2', '/api/v2'],
  ])('reports %s mount point in the version document', async (_name, configured, expected) => {
    const app = buildApp(configured === undefined ? {} : { mountPoint: configured });

    const body = (await (await app.request('/version')).json()) as { mount_point: string };

    expect(body.mount_point).toBe(expected);
  });
});

describe('a path that matches no route', () => {
  it.each(['/api/v1/nope', '/nope', '/api/v1/workspaces/demo/patches/deep/and/deeper', '/health'])(
    'answers 404 in the error envelope for %s',
    async (path) => {
      const response = await buildApp().request(path);
      const body = (await response.json()) as { error: Record<string, unknown> };

      expect(response.status).toBe(404);
      expect(body.error.code).toBe(404);
      expect(typeof body.error.message).toBe('string');
      expect((body.error.message as string).length).toBeGreaterThan(0);
      expect(Object.hasOwn(body.error, 'error_type')).toBe(false);
    },
  );
});

describe('a server route requested with another method', () => {
  it.each(cross(OTHER_METHODS, SERVER_ROUTE_PATHS))(
    'answers %s %s with 405 without invoking the handler',
    async (method, path) => {
      const handler = vi.fn(() => new Response('should not run'));
      const app = buildApp({ routes: SERVER_ROUTE_PATHS.map((p) => ({ path: p, handler })) });

      const response = await app.request(path, { method });
      const body = (await response.json()) as { error: { code: number; message: string } };

      expect(response.status).toBe(405);
      expect(body.error.code).toBe(405);
      expect(body.error.message.length).toBeGreaterThan(0);
      expect(handler).not.toHaveBeenCalled();
    },
  );

  it('still serves GET on those routes', async () => {
    const handler = vi.fn(() => new Response('served'));
    const app = buildApp({ routes: [{ path: '/healthz', handler }] });

    const response = await app.request('/healthz');

    expect(response.status).toBe(200);
    expect(handler).toHaveBeenCalledTimes(1);
  });
});

describe('an unexpected failure while answering', () => {
  const sentinel = 'SENTINEL-9f3a-leaky-detail';

  function appThatThrows(records: string[]) {
    return buildApp({
      log: { error: (message, detail) => records.push(`${message} ${String(detail)}`) },
      routes: [
        {
          path: '/boom',
          handler: () => {
            throw new Error(sentinel);
          },
        },
      ],
    });
  }

  it('answers 500 with a fixed message and leaks nothing into the body', async () => {
    const response = await appThatThrows([]).request('/boom');
    const text = await response.text();

    expect(response.status).toBe(500);
    expect(JSON.parse(text)).toEqual({ error: { code: 500, message: 'internal server error' } });
    expect(text).not.toContain(sentinel);
  });

  it('records the underlying failure in the log', async () => {
    const records: string[] = [];

    await appThatThrows(records).request('/boom');

    expect(records.some((record) => record.includes(sentinel))).toBe(true);
  });
});

describe('the request body limit', () => {
  const DEFAULT_LIMIT = 1_048_576;

  it.each([
    ['at the configured limit', 1024, 1024, true],
    ['one byte over the configured limit', 1024, 1025, false],
    ['far over the configured limit', 1024, 64_000, false],
    ['empty', 1024, 0, true],
    ['at the default limit', undefined, DEFAULT_LIMIT, true],
    ['one byte over the default limit', undefined, DEFAULT_LIMIT + 1, false],
  ])('answers a body %s', async (_name, maxBodyBytes, size, accepted) => {
    const handler = vi.fn(() => new Response('served'));
    const app = buildApp({
      ...(maxBodyBytes === undefined ? {} : { maxBodyBytes }),
      routes: [{ path: '/t', handler, methods: ['POST'] }],
    });

    const response = await app.request('/t', { method: 'POST', body: 'x'.repeat(size) });

    if (accepted) {
      expect(response.status).not.toBe(413);
      expect(handler).toHaveBeenCalledTimes(1);
    } else {
      const body = (await response.json()) as { error: { code: number; message: string } };
      expect(response.status).toBe(413);
      expect(body.error.code).toBe(413);
      expect(body.error.message.length).toBeGreaterThan(0);
      expect(handler).not.toHaveBeenCalled();
    }
  });

  it('refuses an over-sized body sent without a content length', async () => {
    const handler = vi.fn(() => new Response('served'));
    const app = buildApp({
      maxBodyBytes: 64,
      routes: [{ path: '/t', handler, methods: ['POST'] }],
    });

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('x'.repeat(256)));
        controller.close();
      },
    });
    const response = await app.request('/t', {
      method: 'POST',
      body: stream,
      duplex: 'half',
    } as RequestInit);

    expect(response.status).toBe(413);
    expect(handler).not.toHaveBeenCalled();
  });
});
