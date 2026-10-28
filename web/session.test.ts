import { describe, expect, it } from 'vitest';
import { SEED_NOW, freshStore } from '../src/testutil/identity.js';
import { movableClock } from '../src/platform/local/clock.js';
import {
  SESSION_COOKIE,
  SESSION_HOURS,
  endSession,
  openSession,
  sessionCookie,
  sessionFrom,
  signedInUserId,
} from './session.js';

/**
 * The browser's session. It is this package's own on purpose: the
 * API's sign-in revokes every API key the person holds, and a
 * developer opening the dashboard must not kill the key their agent
 * is running with.
 */
describe('a session', () => {
  it('identifies the person it was opened for', async () => {
    const seeded = await freshStore();
    try {
      const time = movableClock(SEED_NOW);
      const opened = await openSession(seeded.store, time.clock, 'u-1');

      expect(await signedInUserId(seeded.store, time.clock, opened.value)).toBe('u-1');
    } finally {
      seeded.dispose();
    }
  });

  it('is a secret the store does not hold in the clear', async () => {
    const seeded = await freshStore();
    try {
      const time = movableClock(SEED_NOW);
      const opened = await openSession(seeded.store, time.clock, 'u-1');

      const rows = await seeded.store.all<Record<string, unknown>>('SELECT * FROM web_sessions');

      expect(JSON.stringify(rows)).not.toContain(opened.value);
    } finally {
      seeded.dispose();
    }
  });

  it('stops being accepted once it has expired', async () => {
    const seeded = await freshStore();
    try {
      const time = movableClock(SEED_NOW);
      const opened = await openSession(seeded.store, time.clock, 'u-1');

      time.advance(SESSION_HOURS * 3_600_000 + 1000);

      expect(await signedInUserId(seeded.store, time.clock, opened.value)).toBeUndefined();
    } finally {
      seeded.dispose();
    }
  });

  it('stops being accepted once it has been ended', async () => {
    const seeded = await freshStore();
    try {
      const time = movableClock(SEED_NOW);
      const opened = await openSession(seeded.store, time.clock, 'u-1');

      await endSession(seeded.store, opened.value);

      expect(await signedInUserId(seeded.store, time.clock, opened.value)).toBeUndefined();
    } finally {
      seeded.dispose();
    }
  });

  it.each([
    ['a value nobody issued', 'not-a-session'],
    ['an empty value', ''],
  ])('refuses %s', async (_what, value) => {
    const seeded = await freshStore();
    try {
      const time = movableClock(SEED_NOW);

      expect(await signedInUserId(seeded.store, time.clock, value)).toBeUndefined();
    } finally {
      seeded.dispose();
    }
  });
});

describe('the cookie a session travels in', () => {
  it('cannot be read by a script, sent to another site, or scoped away from a page', () => {
    const header = sessionCookie('a-value', { secure: true });

    expect(header).toContain(`${SESSION_COOKIE}=a-value`);
    expect(header).toContain('HttpOnly');
    expect(header).toContain('SameSite=Lax');
    expect(header).toContain('Path=/');
    expect(header).toContain('Secure');
    expect(header).toMatch(/Max-Age=\d+/);
  });

  it('is cleared by a sign-out rather than left to expire', () => {
    const header = sessionCookie('', { secure: false, clear: true });

    expect(header).toContain('Max-Age=0');
    expect(header).not.toContain('Secure');
  });

  it('is read from a request that carries one among others', () => {
    const headers = new Headers({
      cookie: `other=1; ${SESSION_COOKIE}=the-value; another=2`,
    });

    expect(sessionFrom(headers)).toBe('the-value');
  });

  it('is absent from a request that carries none', () => {
    expect(sessionFrom(new Headers({ cookie: 'other=1' }))).toBeUndefined();
    expect(sessionFrom(new Headers())).toBeUndefined();
  });
});
