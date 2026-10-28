import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * A run's records, ingested as an agent ingests them: one at a time
 * and in batches, with ids the agent chose so that a retry of a
 * request it never saw answered cannot double-count the run.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

const path = (route: string) => `${MOUNT}/workspaces/mine/runs/${RUN}/${route}`;

/** The five kinds, with a body and the table each lands in. */
const KINDS = [
  ['events', { id: 'r-1', event_type: 'session.start' }, 'run_events'],
  [
    'sessions/outcomes',
    { id: 'r-2', session_id: 's-1', status: 'completed', duration_ms: 12 },
    'run_session_outcomes',
  ],
  ['tools/calls', { id: 'r-3', tool_name: 'bash', input: { cmd: 'ls' } }, 'run_tool_calls'],
  [
    'tools/errors',
    { id: 'r-4', tool_name: 'bash', error_code: 'E1', error_msg: 'it failed' },
    'run_tool_errors',
  ],
  ['traces', { id: 'r-5', event_type: 'assistant.message', sequence: 1 }, 'run_traces'],
] as const;

async function count(held: Awaited<ReturnType<typeof world>>, table: string): Promise<number> {
  const rows = await held.store.all<{ n: number }>(`SELECT COUNT(*) AS n FROM ${table}`);
  return Number(rows[0]?.n ?? 0);
}

describe('each kind of record an agent reports', () => {
  it.each(KINDS)('is stored once however often %s is sent', async (route, body, table) => {
    const held = await world();

    const first = await held.app.request(path(route), withJson('POST', body, held.key));
    const again = await held.app.request(path(route), withJson('POST', body, held.key));

    expect(first.status).toBe(201);
    expect(again.status).toBe(200);
    expect(await count(held, table)).toBe(1);
    // The repeat answered with what is stored, not with a second record.
    expect(((await again.json()) as { id: string }).id).toBe(body.id);
  });

  it('gives a record with no id of its own one that is unique', async () => {
    const held = await world();

    const first = (await (
      await held.app.request(path('events'), withJson('POST', { event_type: 'a.type' }, held.key))
    ).json()) as { id: string };
    const second = (await (
      await held.app.request(path('events'), withJson('POST', { event_type: 'a.type' }, held.key))
    ).json()) as { id: string };

    expect(first.id).not.toBe(second.id);
    expect(await count(held, 'run_events')).toBe(2);
  });
});

describe('a batch of a hundred items', () => {
  it('counts the accepted, the duplicates and the failures separately', async () => {
    const held = await world();
    // Three the run already holds.
    for (const id of ['old-1', 'old-2', 'old-3']) {
      await held.app.request(
        path('events'),
        withJson('POST', { id, event_type: 'session.start' }, held.key),
      );
    }

    const items: Array<Record<string, unknown>> = [];
    for (let index = 0; index < 100; index += 1) {
      if (index < 3) {
        items.push({ id: `old-${String(index + 1)}`, event_type: 'session.start' });
      } else if (index === 7 || index === 42) {
        items.push({ id: `bad-${String(index)}` });
      } else {
        items.push({ id: `new-${String(index)}`, event_type: 'session.start' });
      }
    }

    const response = await held.app.request(
      path('events/batch'),
      withJson('POST', items, held.key),
    );
    const report = (await response.json()) as {
      accepted: number;
      duplicates: number;
      errors: Array<{ index: number; id?: string; message: string }>;
    };

    expect(response.status).toBe(200);
    expect(report.accepted).toBe(95);
    expect(report.duplicates).toBe(3);
    expect(report.errors.map((one) => one.index)).toEqual([7, 42]);
    expect(report.errors[0]?.message).toMatch(/event_type/);
    // The failures stopped nothing: the valid items are all there.
    expect(await count(held, 'run_events')).toBe(98);
  });

  it('stores a batch of traces the same way', async () => {
    const held = await world();

    const response = await held.app.request(
      path('traces/batch'),
      withJson(
        'POST',
        [
          { id: 't-1', event_type: 'session.init' },
          { id: 't-1', event_type: 'session.init' },
          { event_type: 'not.a.trace.type' },
        ],
        held.key,
      ),
    );
    const report = (await response.json()) as {
      accepted: number;
      duplicates: number;
      errors: unknown[];
    };

    expect(response.status).toBe(200);
    expect([report.accepted, report.duplicates, report.errors.length]).toEqual([1, 1, 1]);
    expect(await count(held, 'run_traces')).toBe(1);
  });
});
