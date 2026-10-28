import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import type { RelationalStore } from '../platform/index.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import type { Conflict } from './conflict.js';
import { conflictIdentity } from './conflict.js';
import { type ResolutionStore, resolutionStore } from './resolutions.js';

const REPO = 'a-repository';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function world(): Promise<{ store: RelationalStore; resolutions: ResolutionStore }> {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  return {
    store: seeded.store,
    resolutions: resolutionStore(seeded.store, movableClock(SEED_NOW).clock),
  };
}

const conflict = (over: Partial<Conflict> = {}): Conflict => ({
  path: 'a.txt',
  base: 'base\n',
  ours: 'ours\n',
  theirs: 'theirs\n',
  ...over,
});

describe('recording how a conflict was resolved', () => {
  it('keeps it against the conflict’s identity, marked resolved', async () => {
    const { resolutions } = await world();
    const one = conflict();

    const identity = await resolutions.record(REPO, one, 'the resolved content\n');
    const listed = await resolutions.list(REPO);

    expect(identity).toBe(await conflictIdentity(one));
    expect(listed).toHaveLength(1);
    expect(listed[0]?.identity).toBe(identity);
    expect(listed[0]?.resolved).toBe(true);
    expect(listed[0]?.path).toBe('a.txt');
    expect(listed[0]?.recordedAt).toBe(SEED_NOW.toISOString());
  });

  it('is found again for the same conflict', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict(), 'the resolved content\n');

    expect(await resolutions.replay(REPO, conflict())).toBe('the resolved content\n');
  });

  it('is not found for a different conflict', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict(), 'the resolved content\n');

    expect(
      await resolutions.replay(REPO, conflict({ theirs: 'something else\n' })),
    ).toBeUndefined();
  });

  it('belongs to its own repository and no other', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict(), 'the resolved content\n');

    expect(await resolutions.replay('another-repository', conflict())).toBeUndefined();
  });

  it('replaces what was recorded for the same conflict before', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict(), 'the first answer\n');
    await resolutions.record(REPO, conflict(), 'a better answer\n');

    expect(await resolutions.replay(REPO, conflict())).toBe('a better answer\n');
    expect(await resolutions.list(REPO)).toHaveLength(1);
  });
});

describe('listing what has been recorded', () => {
  it('carries an identity, a path, a time and whether it is resolved', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict({ path: 'one.txt' }), 'one\n');
    await resolutions.record(REPO, conflict({ path: 'two.txt' }), 'two\n');

    const listed = await resolutions.list(REPO);

    expect(listed).toHaveLength(2);
    for (const entry of listed) {
      expect(entry.identity).toMatch(/^[0-9a-f]{64}$/);
      expect(entry.recordedAt).toBe(SEED_NOW.toISOString());
      expect(entry.resolved).toBe(true);
    }
    expect(listed.map((entry) => entry.path).toSorted()).toEqual(['one.txt', 'two.txt']);
  });

  it('is empty for a repository that has recorded nothing', async () => {
    const { resolutions } = await world();

    expect(await resolutions.list(REPO)).toEqual([]);
  });
});

describe('forgetting a resolution', () => {
  it('removes the one named by identity, and says it was there', async () => {
    const { resolutions } = await world();
    const identity = await resolutions.record(REPO, conflict(), 'the answer\n');

    expect(await resolutions.forget(REPO, identity)).toBe(true);
    expect(await resolutions.list(REPO)).toEqual([]);
    expect(await resolutions.replay(REPO, conflict())).toBeUndefined();
  });

  it('removes every one recorded at a path, and says how many', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict({ path: 'shared.txt' }), 'one\n');
    await resolutions.record(REPO, conflict({ path: 'shared.txt', ours: 'other\n' }), 'two\n');
    await resolutions.record(REPO, conflict({ path: 'elsewhere.txt' }), 'three\n');

    expect(await resolutions.forgetPath(REPO, 'shared.txt')).toBe(2);
    expect((await resolutions.list(REPO)).map((entry) => entry.path)).toEqual(['elsewhere.txt']);
  });

  it('removes nothing and says so when nothing was recorded there', async () => {
    const { resolutions } = await world();
    await resolutions.record(REPO, conflict(), 'the answer\n');

    expect(await resolutions.forgetPath(REPO, 'never-conflicted.txt')).toBe(0);
    expect(await resolutions.forget(REPO, 'an-identity-nobody-recorded')).toBe(false);
    expect(await resolutions.list(REPO)).toHaveLength(1);
  });
});
