import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import { freshStore } from '../testutil/identity.js';
import { claimDelivery } from './dedupe.js';

/**
 * The claim that makes a delivery happen once. A store that cannot
 * take the row is the one case that must not look like a duplicate:
 * the forge will redeliver, and a redelivery treated as a duplicate
 * is a delivery lost for good.
 */
const delivery = (over: Record<string, unknown> = {}) => ({
  workspace_slug: 'mine',
  delivery_id: 'd-1',
  forge: 'github',
  event: 'issues',
  received_at: '2026-10-21T09:00:00.000Z',
  ...over,
});

describe('claiming a delivery', () => {
  it('is claimed once and a duplicate thereafter', async () => {
    const seeded = await freshStore();
    try {
      expect(await claimDelivery(seeded.store, delivery())).toBe('recorded');
      expect(await claimDelivery(seeded.store, delivery())).toBe('duplicate');
    } finally {
      seeded.dispose();
    }
  });

  it('is its own delivery for another workspace', async () => {
    const seeded = await freshStore();
    try {
      expect(await claimDelivery(seeded.store, delivery())).toBe('recorded');
      expect(await claimDelivery(seeded.store, delivery({ workspace_slug: 'theirs' }))).toBe(
        'recorded',
      );
    } finally {
      seeded.dispose();
    }
  });

  it('is claimed by exactly one of several copies arriving together', async () => {
    const seeded = await freshStore();
    try {
      const claims = await Promise.all(
        Array.from({ length: 5 }, async () => claimDelivery(seeded.store, delivery())),
      );

      expect(claims.filter((claim) => claim === 'recorded')).toHaveLength(1);
      expect(claims.filter((claim) => claim === 'duplicate')).toHaveLength(4);
    } finally {
      seeded.dispose();
    }
  });

  it('is refused rather than swallowed when the store cannot take it', async () => {
    const broken: RelationalStore = {
      ping: () => Promise.resolve(),
      all: () => Promise.resolve([]),
      run: () => Promise.reject(new Error('the store is down')),
      batch: () => Promise.reject(new Error('the store is down')),
    };

    await expect(claimDelivery(broken, delivery())).rejects.toBeInstanceOf(HttpError);
    await expect(claimDelivery(broken, delivery())).rejects.toMatchObject({ status: 500 });
  });
});
