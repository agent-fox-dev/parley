import { describe, expect, it } from 'vitest';

/**
 * 01-PATH-1: an operator brings the service up and probes it. Run against a
 * real deployment named by PARLEY_BASE_URL.
 */
const baseUrl = process.env['PARLEY_BASE_URL'];

function at(path: string): string {
  return `${(baseUrl ?? '').replace(/\/$/, '')}${path}`;
}

describe.skipIf(baseUrl === undefined || baseUrl.length === 0)(
  'an operator probing a running deployment',
  () => {
    it('is answered ok by the liveness probe', async () => {
      const response = await fetch(at('/healthz'));

      expect(response.status).toBe(200);
      await expect(response.json()).resolves.toEqual({ status: 'ok' });
    });

    it('is answered by the readiness probe from the real store', async () => {
      const response = await fetch(at('/readyz'));

      expect([200, 503]).toContain(response.status);
      if (response.status === 200) {
        await expect(response.json()).resolves.toEqual({ status: 'ready' });
      } else {
        const body = (await response.json()) as { error: { code: number } };
        expect(body.error.code).toBe(503);
      }
    });

    it('reads the build information of the running deployment', async () => {
      const response = await fetch(at('/version'));
      const body = (await response.json()) as Record<string, string>;

      expect(response.status).toBe(200);
      expect(Object.keys(body).toSorted()).toEqual([
        'build_time',
        'commit',
        'mount_point',
        'runtime_version',
      ]);
      expect(body.runtime_version?.length).toBeGreaterThan(0);
    });
  },
);
