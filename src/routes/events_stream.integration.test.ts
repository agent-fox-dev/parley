import { afterEach, describe, expect, it } from 'vitest';
import { buildApp } from '../http/router.js';
import { movableClock } from '../platform/local/clock.js';
import { inMemoryMetrics } from '../platform/local/metrics.js';
import { storeBus } from '../platform/store_bus.js';
import { eventRecorder } from '../audit/recorder.js';
import { removeMember } from '../store/identity.js';
import {
  SEED_NOW,
  freshStore,
  seedApiKey,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { eventStreamRoutes } from './events_stream.js';

/**
 * The stream against a clock the suite moves and a bus two services
 * share. Both are the point: the heartbeat and the idle timeout are
 * about time passing, and the delivery is about an event recorded
 * somewhere other than where the stream is served.
 */
const MOUNT = '/api/v1';
const ADMIN = 'af_admin_stream_test';

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

/** A service serving the stream alone, with its timing in the suite's hands. */
async function world(options: { idleMs?: number; heartbeatMs?: number } = {}) {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const moving = movableClock(SEED_NOW);
  const metrics = inMemoryMetrics();
  const bus = storeBus(seeded.store, moving.clock);

  const owner = await seedUser(seeded.store);
  const org = await seedOrg(seeded.store, owner);
  await seedWorkspace(seeded.store, {
    slug: 'mine',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  const key = (await seedApiKey(seeded.store, owner)).presented;

  const app = buildApp({
    routes: eventStreamRoutes({
      mountPoint: MOUNT,
      store: seeded.store,
      clock: moving.clock,
      bus,
      metrics,
      heartbeatMs: options.heartbeatMs ?? 30_000,
      idleMs: options.idleMs ?? 60_000,
      tickMs: 1,
      // Real time only to let the loop run; what it makes of the
      // passing of time comes from the clock the suite moves.
      wait: () => new Promise((settled) => setTimeout(settled, 1)),
    }),
    store: seeded.store,
    clock: moving.clock,
    auth: { store: seeded.store, clock: moving.clock, adminToken: ADMIN, mountPoint: MOUNT },
  });

  return { store: seeded.store, app, bus, moving, metrics, key, owner, org };
}

type World = Awaited<ReturnType<typeof world>>;

/** Opens the stream and reads frames as they arrive. */
async function open(held: World, query = '') {
  const response = await held.app.request(`${MOUNT}/events${query}`, {
    headers: { authorization: `Bearer ${held.key}` },
  });
  expect(response.status).toBe(200);
  const reader = (response.body as ReadableStream<Uint8Array>).getReader();
  const decoder = new TextDecoder();
  cleanups.push(() => {
    void reader.cancel();
  });

  // A read outlives the call that started it and is cleared only
  // once its value has been taken: a read the suite stopped waiting
  // for would otherwise resolve with a frame nobody ever reads.
  let pending: Promise<ReadableStreamReadResult<Uint8Array>> | null = null;

  return {
    /** Reads what has arrived, until nothing more does. */
    frames: async (): Promise<string[]> => {
      const read: string[] = [];
      for (;;) {
        pending ??= reader.read();
        const next = await Promise.race([
          pending,
          new Promise<undefined>((settled) => {
            setTimeout(() => {
              settled(undefined);
            }, 40);
          }),
        ]);
        if (next === undefined) {
          return read;
        }
        pending = null;
        if (next.done) {
          read.push('<closed>');
          return read;
        }
        read.push(decoder.decode(next.value));
      }
    },
    reader,
  };
}

/** Lets the stream's loop run while the clock stands where it was put. */
async function letItRun(times = 10): Promise<void> {
  for (let turn = 0; turn < times; turn += 1) {
    await new Promise((settled) => setTimeout(settled, 2));
  }
}

async function record(held: World, id: string, workspace: string | null = 'mine'): Promise<void> {
  await held.bus.publish({
    id,
    event_type: 'hub.workspace.update',
    source: 'hub',
    timestamp: held.moving.clock.now().toISOString(),
    workspace,
    severity: 'info',
  });
}

describe('a stream that is being read', () => {
  it('beats at once, beats again every interval, and carries what is recorded', async () => {
    const held = await world();
    const watching = await open(held);

    expect((await watching.frames()).join('')).toContain('event: heartbeat');

    await record(held, 'e-1');
    const carried = (await watching.frames()).join('');
    expect(carried).toContain('event: audit_event');
    expect(carried).toContain('"id":"e-1"');

    // No heartbeat before the interval, one after it.
    held.moving.advance(29_000);
    await letItRun();
    expect((await watching.frames()).join('')).not.toContain('heartbeat');

    held.moving.advance(2000);
    await letItRun();
    expect((await watching.frames()).join('')).toContain('event: heartbeat');
  });

  it('counts the streams it is serving, and stops counting a closed one', async () => {
    const held = await world();
    const open1 = await open(held);
    await letItRun(2);

    const whileOpen = held.metrics
      .snapshot()
      .find((series) => series.name === 'afhub_sse_connections')?.value;
    await open1.reader.cancel();
    await letItRun(2);
    const afterwards = held.metrics
      .snapshot()
      .find((series) => series.name === 'afhub_sse_connections')?.value;

    expect([whileOpen, afterwards]).toEqual([1, 0]);
  });
});

describe('a stream nobody is reading', () => {
  it('is closed once the idle time has passed', async () => {
    const held = await world({ idleMs: 60_000 });
    const watching = await open(held);
    await letItRun(2);

    held.moving.advance(61_000);
    await letItRun();

    expect((await watching.frames()).at(-1)).toBe('<closed>');
  });

  it('keeps the newest events for a client that falls behind, and drops the oldest', async () => {
    const held = await world();
    const watching = await open(held);
    await letItRun(2);

    for (let index = 0; index < 300; index += 1) {
      held.moving.advance(1);
      await record(held, `e-${String(index).padStart(3, '0')}`);
    }
    await letItRun(20);

    const carried = (await watching.frames()).join('');
    const delivered = [...carried.matchAll(/"id":"(e-\d{3})"/g)].map((match) => match[1]);
    expect(delivered).toHaveLength(256);
    expect(delivered[0]).toBe('e-044');
    expect(delivered.at(-1)).toBe('e-299');
  });
});

describe('what a stream may see', () => {
  it('is fixed when it opens, so a membership change waits for the next one', async () => {
    const held = await world();
    const other = await seedUser(held.store);
    await seedWorkspace(held.store, {
      slug: 'joined-later',
      owner_id: other.id,
      org_id: held.org.id,
      status: 'ready',
    });

    // The stream opens while the caller shares the organization.
    const watching = await open(held);
    await letItRun(2);

    // They leave it, and the shared workspace records something.
    await removeMember(held.store, held.org.id, held.owner.id);
    await record(held, 'e-shared', 'joined-later');
    await letItRun();

    // The open stream sees what it could see when it opened.
    expect((await watching.frames()).join('')).toContain('"id":"e-shared"');

    // The next connection sees what the membership says now.
    const again = await open(held);
    await letItRun(2);
    await record(held, 'e-after', 'joined-later');
    await record(held, 'e-own', 'mine');
    await letItRun();

    const carried = (await again.frames()).join('');
    expect(carried).toContain('"id":"e-own"');
    expect(carried).not.toContain('"id":"e-after"');
  });
});

describe('an event recorded by another component', () => {
  it('reaches a stream served by the first, and the other way round', async () => {
    const held = await world();
    // A second component over the same store: its own recorder, its
    // own bus, nothing shared but the database.
    const elsewhere = eventRecorder({
      store: held.store,
      clock: held.moving.clock,
      bus: storeBus(held.store, held.moving.clock),
    });

    const watching = await open(held);
    await letItRun(2);

    await elsewhere.record({
      type: 'hub.workspace.archive',
      workspace: 'mine',
      resourceId: 'mine',
      metadata: { head_sha: 'a'.repeat(40) },
    });
    await letItRun();

    const carried = (await watching.frames()).join('');
    expect(carried).toContain('event: audit_event');
    expect(carried).toContain('hub.workspace.archive');
  });
});
