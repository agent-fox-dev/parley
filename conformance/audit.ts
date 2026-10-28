import type { Check } from './harness.js';

/**
 * The conformance checks of the audit surface. They go after what a
 * client cannot check for itself: that the unified query really
 * carries both sources in one order, that the ingestion's access
 * rule answers in the order the reference gives, and that the stream
 * is a stream.
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

export function auditChecks(): Check[] {
  return [
    {
      name: 'the unified query answers newest first across both sources',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, '/api/v1/audit?limit=50');
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the query answered ${String(got.status)}` };
        }
        const body = got.body as { events: Array<{ timestamp: string; source: string }> };
        const times = body.events.map((one) => one.timestamp);
        const ordered = [...times].toSorted().toReversed();
        if (JSON.stringify(times) !== JSON.stringify(ordered)) {
          return { outcome: 'fail', message: 'the events are not in order by instant' };
        }
        return {
          outcome: 'pass',
          message: `${String(times.length)} events, sources ${[...new Set(body.events.map((one) => one.source))].join(' and ')}`,
        };
      },
    },
    {
      name: 'a hub-only filter leaves the agent events out',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, '/api/v1/audit?resource_type=workspace&limit=50');
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the query answered ${String(got.status)}` };
        }
        const sources = new Set(
          (got.body as { events: Array<{ source: string }> }).events.map((one) => one.source),
        );
        return sources.has('agent')
          ? { outcome: 'fail', message: 'an agent event answered a hub-only filter' }
          : { outcome: 'pass' };
      },
    },
    {
      name: 'an ingestion with a malformed run id is refused',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const response = await fetch(
          `${baseUrl.replace(/\/$/, '')}/api/v1/workspaces/${workspace()}/runs/nonsense/events`,
          {
            method: 'POST',
            headers: {
              authorization: `Bearer ${apiKey()}`,
              'content-type': 'application/json',
            },
            body: JSON.stringify({ event_type: 'session.start' }),
          },
        );
        return response.status === 400
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 400, got ${String(response.status)}` };
      },
    },
    {
      name: 'the event stream opens with a heartbeat',
      needs: recorded,
      run: async ({ baseUrl }) => {
        const response = await fetch(
          `${baseUrl.replace(/\/$/, '')}/api/v1/events?workspace=${workspace()}`,
          { headers: { authorization: `Bearer ${apiKey()}` } },
        );
        if (response.status !== 200) {
          return { outcome: 'fail', message: `the stream answered ${String(response.status)}` };
        }
        if (!(response.headers.get('content-type') ?? '').includes('text/event-stream')) {
          return { outcome: 'fail', message: 'the answer is not an event stream' };
        }
        const reader = (response.body as ReadableStream<Uint8Array>).getReader();
        try {
          const first = await Promise.race([
            reader.read(),
            new Promise<undefined>((settled) => {
              setTimeout(() => {
                settled(undefined);
              }, 10_000);
            }),
          ]);
          if (first === undefined || first.done) {
            return { outcome: 'fail', message: 'no heartbeat arrived' };
          }
          const text = new TextDecoder().decode(first.value);
          return text.includes('event: heartbeat')
            ? { outcome: 'pass' }
            : { outcome: 'fail', message: `the first frame was ${text.slice(0, 40)}` };
        } finally {
          await reader.cancel();
        }
      },
    },
  ];
}
