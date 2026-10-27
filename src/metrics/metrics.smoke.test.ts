import { describe, expect, it } from 'vitest';

/**
 * 01-PATH-3: a missed route is answered in the envelope and counted under
 * the bounded route-template label, read back from the deployment's own
 * metrics endpoint.
 */
const baseUrl = process.env['PARLEY_BASE_URL'];

function at(path: string): string {
  return `${(baseUrl ?? '').replace(/\/$/, '')}${path}`;
}

describe.skipIf(baseUrl === undefined || baseUrl.length === 0)(
  'a missed route against a running deployment',
  () => {
    it('is answered 404 in the envelope and counted under one path label', async () => {
      const missed = `/api/v1/no-such-thing-${Date.now()}`;

      const answer = await fetch(at(missed));
      const body = (await answer.json()) as { error: { code: number; message: string } };
      expect(answer.status).toBe(404);
      expect(body.error.code).toBe(404);
      expect(body.error.message.length).toBeGreaterThan(0);

      const exposition = await (await fetch(at('/metrics'))).text();
      const counted = exposition
        .split('\n')
        .filter((line) => line.startsWith('afhub_http_requests_total{'))
        .filter((line) => line.includes('status="404"'));

      expect(counted.length).toBeGreaterThan(0);
      expect(counted.join('\n')).not.toContain(missed);
    });
  },
);
