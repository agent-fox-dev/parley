import { describe, expect, it } from 'vitest';
import { noStore } from '../platform/store.js';
import { buildApp } from '../http/router.js';
import { SERVER_ROUTE_PATHS } from './server.js';

const VERSION_KEYS = ['build_time', 'commit', 'mount_point', 'runtime_version'];

interface VersionBody {
  build_time: string;
  commit: string;
  mount_point: string;
  runtime_version: string;
}

async function version(options: Parameters<typeof buildApp>[0] = {}): Promise<Response> {
  return buildApp(options).request('/version');
}

describe('the version route', () => {
  it('reports exactly the four documented keys', async () => {
    const body = (await (
      await version({ buildTime: '2026-10-27T00:00:00Z', commit: 'abc1234' })
    ).json()) as Record<string, unknown>;

    expect(Object.keys(body).toSorted()).toEqual(VERSION_KEYS.toSorted());
  });

  it('reports the build time and commit the build supplied', async () => {
    const body = (await (
      await version({ buildTime: '2026-10-27T00:00:00Z', commit: 'abc1234' })
    ).json()) as VersionBody;

    expect(body.build_time).toBe('2026-10-27T00:00:00Z');
    expect(body.commit).toBe('abc1234');
  });

  it('reports a non-empty runtime version and the configured mount point', async () => {
    const body = (await (await version({ mountPoint: '/api/v9' })).json()) as VersionBody;

    expect(body.runtime_version.length).toBeGreaterThan(0);
    expect(body.mount_point).toBe('/api/v9');
  });

  it('carries no go_version key', async () => {
    const body = (await (await version()).json()) as Record<string, unknown>;

    expect(Object.hasOwn(body, 'go_version')).toBe(false);
  });

  it.each([
    ['neither', {}, '', ''],
    ['no commit', { buildTime: '2026-10-27T00:00:00Z' }, '2026-10-27T00:00:00Z', ''],
    ['no build time', { commit: 'abc1234' }, '', 'abc1234'],
  ])(
    'answers successfully when the build supplied %s',
    async (_name, options, buildTime, commit) => {
      const response = await version(options);
      const body = (await response.json()) as VersionBody;

      expect(response.status).toBe(200);
      expect(body.build_time).toBe(buildTime);
      expect(body.commit).toBe(commit);
    },
  );

  it('permits public caching', async () => {
    const response = await version();

    expect(response.headers.get('cache-control')).toMatch(/\bpublic\b/);
  });
});

describe('the liveness probe', () => {
  it('answers ok without touching any store', async () => {
    const calls: string[] = [];
    const store = {
      ...noStore(),
      ping: async () => {
        calls.push('ping');
        throw new Error('the store must not be called');
      },
    };

    const response = await buildApp({ store }).request('/healthz');

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({ status: 'ok' });
    expect(calls).toEqual([]);
  });
});

describe('the server routes and credentials', () => {
  it.each(
    SERVER_ROUTE_PATHS.flatMap((path) =>
      [
        ['no Authorization header', undefined],
        ['a malformed Authorization header', 'Bearer nonsense'],
      ].map(([name, header]) => [path, name, header] as const),
    ),
  )('answers %s with %s without demanding a credential', async (path, _name, header) => {
    const app = buildApp({ store: noStore() });

    const response = await app.request(
      path,
      header === undefined ? {} : { headers: { authorization: header } },
    );

    expect(response.status).not.toBe(401);
  });
});
