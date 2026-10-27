import { afterEach, describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { storeLocks } from '../platform/store_lock.js';
import { movableClock } from '../platform/local/clock.js';
import { freshStore } from '../testutil/identity.js';
import { SEED_NOW } from '../testutil/identity.js';
import type { RelationalStore } from '../platform/index.js';
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

describe('a workspace whose lock is held', () => {
  it('refuses the next taker at once, without waiting for the lock', async () => {
    const locks = storeLocks(await store(), movableClock(SEED_NOW).clock);
    let letGo!: () => void;
    const holding = new Promise<void>((resolve) => {
      letGo = resolve;
    });

    const first = withWorkspaceLock(locks, 'demo', async () => {
      await holding;
      return 'first';
    });

    // The second answers while the first is still inside its work, which
    // is what "without waiting" means: it cannot have waited for a lock
    // that is still held when it answers.
    const refused = await withWorkspaceLock(locks, 'demo', async () => 'second').catch(
      (err: unknown) => err,
    );

    expect(refused).toBeInstanceOf(HttpError);
    expect((refused as HttpError).status).toBe(409);
    expect((refused as HttpError).errorType).toBe('workspace_busy');

    letGo();
    expect(await first).toBe('first');

    expect(await withWorkspaceLock(locks, 'demo', async () => 'after')).toBe('after');
  });

  it('does not refuse a taker of another workspace', async () => {
    const locks = storeLocks(await store(), movableClock(SEED_NOW).clock);
    let letGo!: () => void;
    const holding = new Promise<void>((resolve) => {
      letGo = resolve;
    });
    const first = withWorkspaceLock(locks, 'one', async () => {
      await holding;
      return 'one';
    });

    expect(await withWorkspaceLock(locks, 'two', async () => 'two')).toBe('two');

    letGo();
    await first;
  });

  it('releases the lock even when the work it guarded failed', async () => {
    const locks = storeLocks(await store(), movableClock(SEED_NOW).clock);

    await expect(
      withWorkspaceLock(locks, 'demo', async () => {
        throw new Error('the work failed');
      }),
    ).rejects.toThrow('the work failed');

    expect(await withWorkspaceLock(locks, 'demo', async () => 'after')).toBe('after');
  });
});
