import { afterEach, describe, expect, it } from 'vitest';
import { storeLocks } from '../platform/store_lock.js';
import { movableClock } from '../platform/local/clock.js';
import type { RelationalStore } from '../platform/index.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import { withWorkspaceLock } from './lock.js';

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

async function store(): Promise<RelationalStore> {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  return seeded.store;
}

interface Interval {
  slug: string;
  from: number;
  to: number;
}

/** Takes the lock, retrying while another holder has it, then records its turn. */
async function takeTurn(
  locks: ReturnType<typeof storeLocks>,
  slug: string,
  intervals: Interval[],
  counter: { now: number },
): Promise<void> {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try {
      return await withWorkspaceLock(locks, slug, async () => {
        const from = (counter.now += 1);
        await Promise.resolve();
        await Promise.resolve();
        const to = (counter.now += 1);
        intervals.push({ slug, from, to });
      });
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
  }
  throw new Error(`never took the lock for ${slug}`);
}

const overlap = (a: Interval, b: Interval) => a.from < b.to && b.from < a.to;

describe('two operations on one workspace', () => {
  it('take their turns, and never hold the workspace at the same time', async () => {
    const locks = storeLocks(await store(), movableClock(SEED_NOW).clock);
    const intervals: Interval[] = [];
    const counter = { now: 0 };

    await Promise.all([
      takeTurn(locks, 'shared', intervals, counter),
      takeTurn(locks, 'shared', intervals, counter),
    ]);

    expect(intervals).toHaveLength(2);
    expect(overlap(intervals[0] as Interval, intervals[1] as Interval)).toBe(false);
  });

  it('may hold two different workspaces at the same time', async () => {
    const locks = storeLocks(await store(), movableClock(SEED_NOW).clock);
    let letGo!: () => void;
    const holding = new Promise<void>((resolve) => {
      letGo = resolve;
    });

    const first = withWorkspaceLock(locks, 'one', async () => {
      await holding;
      return 'one';
    });
    const second = await withWorkspaceLock(locks, 'two', async () => 'two');

    expect(second).toBe('two');
    letGo();
    expect(await first).toBe('one');
  });
});

describe('a lock held by an instance that stopped', () => {
  it('frees itself once its lease runs out, with nobody intervening', async () => {
    const relational = await store();
    const moving = movableClock(SEED_NOW);
    const leaseMs = 30_000;
    const stopped = storeLocks(relational, moving.clock, { leaseMs });

    // Taken and never released: the instance holding it went away.
    expect(await stopped.take('workspace:abandoned')).toBeDefined();

    const survivor = storeLocks(relational, moving.clock, { leaseMs });
    expect(await survivor.take('workspace:abandoned')).toBeUndefined();

    moving.advance(leaseMs + 1);

    expect(await survivor.take('workspace:abandoned')).toBeDefined();
  });
});

describe('two instances of the service sharing one store', () => {
  it.each([
    ['the first holding', 0],
    ['the second holding', 1],
  ])('serialise the lock between them: %s', async (_name, holderIndex) => {
    const relational = await store();
    const clock = movableClock(SEED_NOW).clock;
    const instances = [storeLocks(relational, clock), storeLocks(relational, clock)];
    const holder = instances[holderIndex];
    const other = instances[1 - holderIndex];

    const held = await holder?.take('workspace:shared');
    expect(held).toBeDefined();
    expect(await other?.take('workspace:shared')).toBeUndefined();

    await held?.release();

    expect(await other?.take('workspace:shared')).toBeDefined();
  });
});
