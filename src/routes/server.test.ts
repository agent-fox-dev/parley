import { describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';

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
