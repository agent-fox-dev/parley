import { describe, expect, it } from 'vitest';
import { apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * What a workspace has cost, read from a running deployment. The
 * thing worth checking against real data is the agreement: a
 * breakdown that does not sum to its own totals is worse than none.
 */
const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
);

const WIDE = 'since=2020-01-01T00:00:00Z&until=2100-01-01T00:00:00Z';

/** The totals the cost route reports for one grouping. */
async function totalsOf(grouping: string): Promise<Record<string, number>> {
  const response = await call(
    `/api/v1/workspaces/${slug}/cost?${WIDE}&group_by=${grouping}`,
    bearer(apiKey),
  );
  return ((await response.json()) as { totals: Record<string, number> }).totals;
}

describe.skipIf(reason.length > 0)(`a workspace's cost${needed(reason)}`, () => {
  it.each(['day', 'session', 'model'])(
    'sums to its own totals, grouped by %s',
    async (grouping) => {
      const response = await call(
        `/api/v1/workspaces/${slug}/cost?${WIDE}&group_by=${grouping}`,
        bearer(apiKey),
      );
      expect(response.status).toBe(200);

      const body = (await response.json()) as {
        totals: Record<string, number>;
        breakdown: Array<Record<string, number>>;
      };
      for (const field of ['input_tokens', 'output_tokens', 'cache_read_tokens']) {
        expect(
          body.breakdown.reduce((sum, one) => sum + Number(one[field] ?? 0), 0),
          `${field} grouped by ${grouping}`,
        ).toBe(body.totals[field]);
      }
    },
    120_000,
  );

  it('reports the same totals however the period is grouped', async () => {
    const byDay = await totalsOf('day');
    expect(await totalsOf('model')).toEqual(byDay);
    expect(await totalsOf('session')).toEqual(byDay);
  }, 120_000);

  it('refuses a period that ends before it starts', async () => {
    const response = await call(
      `/api/v1/workspaces/${slug}/cost?since=2030-01-01T00:00:00Z&until=2020-01-01T00:00:00Z`,
      bearer(apiKey),
    );

    expect(response.status).toBe(400);
  }, 120_000);
});
