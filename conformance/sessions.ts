import type { Check } from './harness.js';

/**
 * The conformance checks of sessions and cost. They go after the
 * answers agreeing with one another, which is what a client cannot
 * check for itself: a session's summary against its own usage
 * records, and a cost breakdown against its own totals.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

function recorded(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  if (apiKey().length === 0) {
    return 'PARLEY_SMOKE_API_KEY';
  }
  return workspace().length === 0 ? 'PARLEY_SMOKE_WORKSPACE' : undefined;
}

async function read(baseUrl: string, path: string): Promise<{ status: number; body: unknown }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    headers: { authorization: `Bearer ${apiKey()}` },
  });
  return { status: response.status, body: await response.json().catch(() => undefined) };
}

export function sessionChecks(): Check[] {
  return [
    {
      name: 'a cost breakdown sums to its own totals',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const got = await read(
          baseUrl,
          `/api/v1/workspaces/${workspace()}/cost?since=2020-01-01T00:00:00Z&until=2100-01-01T00:00:00Z`,
        );
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the cost route answered ${String(got.status)}` };
        }
        const body = got.body as {
          totals: Record<string, number>;
          breakdown: Array<Record<string, number>>;
        };
        const summed = body.breakdown.reduce(
          (sum, one) => sum + Number(one['input_tokens'] ?? 0),
          0,
        );
        return summed === body.totals['input_tokens']
          ? { outcome: 'pass', message: `${String(body.totals['input_tokens'])} input tokens` }
          : {
              outcome: 'fail',
              message: `the breakdown sums to ${String(summed)} and the totals say ${String(body.totals['input_tokens'])}`,
            };
      },
    },
    {
      name: 'a session list pages with a cursor it issued',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const first = await read(baseUrl, `/api/v1/sessions?limit=1&order=asc`);
        if (first.status !== 200) {
          return { outcome: 'fail', message: `the list answered ${String(first.status)}` };
        }
        const cursor = (first.body as { next_cursor: string | null }).next_cursor;
        if (cursor === null) {
          return { outcome: 'pass', message: 'one page of sessions' };
        }
        const second = await read(baseUrl, `/api/v1/sessions?limit=1&order=asc&cursor=${cursor}`);
        return second.status === 200
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `the second page answered ${String(second.status)}` };
      },
    },
    {
      name: 'a cursor the service did not issue is refused',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const refused = await read(baseUrl, '/api/v1/sessions?cursor=not-one-of-ours');
        return refused.status === 400
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 400, got ${String(refused.status)}` };
      },
    },
  ];
}
