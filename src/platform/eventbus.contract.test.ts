import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { d1Store } from './cloudflare/store.js';
import { sqliteStore } from './local/store.js';
import { movableClock } from './local/clock.js';
import { storeBus } from './store_bus.js';
import { migrate } from '../store/migrations/index.js';
import type { RelationalStore } from './index.js';

/**
 * The event bus, over every store a deployment can be wired with. It
 * exists for one reason — an event recorded by one component must
 * reach a stream served by another — so every case here opens the
 * subscription on one bus and publishes on a second.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function tempPath(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-bus-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return join(dir, 'parley.db');
}

/** A D1 binding over a real database, as the store contract uses. */
function fakeD1(path: string) {
  const db = new DatabaseSync(path);
  cleanups.push(() => {
    db.close();
  });
  return {
    prepare: (sql: string) => ({
      bind: (...params: unknown[]) => ({
        all: () => ({ results: db.prepare(sql).all(...(params as never[])) }),
        run: () => db.prepare(sql).run(...(params as never[])),
        first: () => db.prepare(sql).get(...(params as never[])) ?? null,
      }),
      all: () => ({ results: db.prepare(sql).all() }),
      run: () => db.prepare(sql).run(),
      first: () => db.prepare(sql).get() ?? null,
    }),
    batch: async (statements: Array<{ run: () => unknown }>) =>
      statements.map((statement) => statement.run()),
  };
}

const EVENT = {
  id: 'e-1',
  event_type: 'hub.workspace.archive',
  source: 'hub' as const,
  timestamp: '2026-03-01T12:00:00.000Z',
  workspace: 'mine',
  severity: 'info',
};

describe.each([
  { name: 'the Cloudflare store over D1', make: () => d1Store(fakeD1(tempPath()) as never) },
  { name: 'the local store over SQLite', make: () => sqliteStore(tempPath()) },
])('the event bus contract: over $name', ({ make }) => {
  async function bussed(bufferSize = 256) {
    const store: RelationalStore = await make();
    await migrate(store);
    const moving = movableClock(new Date('2026-03-01T12:00:00Z'));
    return { store, moving, bufferSize };
  }

  it('carries an event published by another instance to a subscriber', async () => {
    const { store, moving } = await bussed();
    const serving = storeBus(store, moving.clock);
    const recording = storeBus(store, moving.clock);

    const watching = await serving.subscribe({ workspaces: 'every' });
    moving.advance(1000);
    await recording.publish({ ...EVENT, timestamp: moving.clock.now().toISOString() });

    expect((await watching.take()).map((one) => one.id)).toEqual(['e-1']);
    watching.close();
  });

  it('carries an event once, however often the subscriber looks', async () => {
    const { store, moving } = await bussed();
    const bus = storeBus(store, moving.clock);
    const watching = await bus.subscribe({ workspaces: 'every' });

    moving.advance(1000);
    await bus.publish({ ...EVENT, timestamp: moving.clock.now().toISOString() });

    expect(await watching.take()).toHaveLength(1);
    expect(await watching.take()).toHaveLength(0);
    watching.close();
  });

  it('carries nothing published before the subscription opened', async () => {
    const { store, moving } = await bussed();
    const bus = storeBus(store, moving.clock);
    await bus.publish({ ...EVENT, timestamp: moving.clock.now().toISOString() });

    moving.advance(1000);
    const watching = await bus.subscribe({ workspaces: 'every' });

    expect(await watching.take()).toHaveLength(0);
    watching.close();
  });

  it('carries only the workspaces the subscription may see', async () => {
    const { store, moving } = await bussed();
    const bus = storeBus(store, moving.clock);
    const watching = await bus.subscribe({ workspaces: ['mine'] });

    moving.advance(1000);
    const at = moving.clock.now().toISOString();
    await bus.publish({ ...EVENT, id: 'e-mine', timestamp: at });
    await bus.publish({ ...EVENT, id: 'e-theirs', workspace: 'theirs', timestamp: at });
    await bus.publish({ ...EVENT, id: 'e-nowhere', workspace: null, timestamp: at });

    expect((await watching.take()).map((one) => one.id)).toEqual(['e-mine']);
    watching.close();
  });

  it('carries only the category the subscription asked for', async () => {
    const { store, moving } = await bussed();
    const bus = storeBus(store, moving.clock);
    const watching = await bus.subscribe({ workspaces: 'every', category: 'agent' });

    moving.advance(1000);
    const at = moving.clock.now().toISOString();
    await bus.publish({ ...EVENT, id: 'e-hub', timestamp: at });
    await bus.publish({ ...EVENT, id: 'e-agent', source: 'agent', timestamp: at });

    expect((await watching.take()).map((one) => one.id)).toEqual(['e-agent']);
    watching.close();
  });

  it('keeps the newest events and drops the oldest when a slow client falls behind', async () => {
    const { store, moving } = await bussed();
    const bus = storeBus(store, moving.clock, { bufferSize: 256 });
    const watching = await bus.subscribe({ workspaces: 'every' });

    for (let index = 0; index < 300; index += 1) {
      moving.advance(1);
      await bus.publish({
        ...EVENT,
        id: `e-${String(index).padStart(3, '0')}`,
        timestamp: moving.clock.now().toISOString(),
      });
    }

    const carried = await watching.take();
    expect(carried).toHaveLength(256);
    expect(carried[0]?.id).toBe('e-044');
    expect(carried.at(-1)?.id).toBe('e-299');
    watching.close();
  });
});
