import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { type Identity, historyEngine } from './engine.js';
import { resolutionStore } from './resolutions.js';

const REPO = 'a-repository';
const AUTHOR: Identity = {
  name: 'Fixture',
  email: 'fixture@parley.test',
  timestamp: 1_700_000_000,
  timezoneOffset: 0,
};
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

/** The content of each side of the one conflict these cases meet. */
const BASE = 'one\ntwo\nthree\n';
const OURS = 'one\nOURS\nthree\n';
const THEIRS = 'one\nTHEIRS\nthree\n';
const RESOLVED = 'one\nBOTH, SETTLED BY HAND\nthree\n';

async function world(): Promise<{
  relational: RelationalStore;
  repos: RepositoryStore;
  /** Where the repository lives, so another store can be built over it. */
  root: string;
  sides: { ours: string; theirs: string };
}> {
  const seeded = await freshStore();
  cleanups.push(seeded.dispose);
  const dir = mkdtempSync(join(tmpdir(), 'parley-replay-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  const repos = localRepositoryStore(dir);
  await repos.create(REPO);
  const base = await buildCommit(repos, REPO, { 'a.txt': BASE }, [], 'base');
  const ours = await buildCommit(repos, REPO, { 'a.txt': OURS }, [base.oid], 'ours');
  const theirs = await buildCommit(repos, REPO, { 'a.txt': THEIRS }, [base.oid], 'theirs');

  return {
    relational: seeded.store,
    repos,
    root: dir,
    sides: { ours: ours.oid, theirs: theirs.oid },
  };
}

const conflict = { path: 'a.txt', base: BASE, ours: OURS, theirs: THEIRS };

describe('a conflict met a second time', () => {
  it('is settled by the resolution recorded the first time', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);

    // First time: it conflicts.
    const first = await historyEngine(repos, { resolutions }).threeWayMerge({
      repository: REPO,
      ...sides,
    });
    expect(first.conflicts).toEqual(['a.txt']);

    await resolutions.record(REPO, conflict, RESOLVED);

    // Second time: the operation goes through.
    const second = await historyEngine(repos, { resolutions }).threeWayMerge({
      repository: REPO,
      ...sides,
    });

    expect(second.conflicts).toBeUndefined();
    expect(await readSnapshot(repos, REPO, second.tree ?? '')).toEqual({ 'a.txt': RESOLVED });
  });

  it('gives the same result as resolving it by hand would', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);
    await resolutions.record(REPO, conflict, RESOLVED);

    const replayed = await historyEngine(repos, { resolutions }).threeWayMerge({
      repository: REPO,
      ...sides,
    });

    // What a person would have committed, built the same way.
    const byHand: Snapshot = { 'a.txt': RESOLVED };
    const handCommit = await buildCommit(repos, REPO, byHand, [sides.ours], 'by hand');

    expect(await readSnapshot(repos, REPO, replayed.tree ?? '')).toEqual(
      await readSnapshot(repos, REPO, handCommit.oid),
    );
  });

  it('lets a merge commit through, with the resolved content in its tree', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);
    await resolutions.record(REPO, conflict, RESOLVED);

    const result = await historyEngine(repos, { resolutions }).mergeCommit({
      repository: REPO,
      ...sides,
      author: AUTHOR,
    });

    expect(result.conflicts).toBeUndefined();
    expect(await readSnapshot(repos, REPO, result.commit ?? '')).toEqual({ 'a.txt': RESOLVED });
  });

  it('still conflicts where nothing was recorded for it', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);
    // A resolution for a conflict with different content than this one.
    await resolutions.record(REPO, { ...conflict, theirs: 'something else\n' }, RESOLVED);

    const result = await historyEngine(repos, { resolutions }).threeWayMerge({
      repository: REPO,
      ...sides,
    });

    expect(result.conflicts).toEqual(['a.txt']);
  });
});

describe('a resolution and the process that recorded it', () => {
  it('outlives it: every component but the store is built again', async () => {
    const { relational, root, sides } = await world();

    // One process records it.
    await resolutionStore(relational, movableClock(SEED_NOW).clock).record(
      REPO,
      conflict,
      RESOLVED,
    );

    // Another, built from nothing but the same store, finds it.
    const afterRestart = resolutionStore(relational, movableClock(SEED_NOW).clock);
    // Built from nothing but the same two stores, as a restart would.
    const engine = historyEngine(localRepositoryStore(root), { resolutions: afterRestart });

    const result = await engine.threeWayMerge({ repository: REPO, ...sides });

    expect(result.conflicts).toBeUndefined();
    expect(await afterRestart.list(REPO)).toHaveLength(1);
  });
});

describe('a resolution that was forgotten', () => {
  it('stops being applied, and the conflict is reported again', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);
    const identity = await resolutions.record(REPO, conflict, RESOLVED);

    expect(
      (await historyEngine(repos, { resolutions }).threeWayMerge({ repository: REPO, ...sides }))
        .conflicts,
    ).toBeUndefined();

    await resolutions.forget(REPO, identity);

    expect(
      (await historyEngine(repos, { resolutions }).threeWayMerge({ repository: REPO, ...sides }))
        .conflicts,
    ).toEqual(['a.txt']);
  });

  it('stops being applied when it was forgotten by its path', async () => {
    const { relational, repos, sides } = await world();
    const resolutions = resolutionStore(relational, movableClock(SEED_NOW).clock);
    await resolutions.record(REPO, conflict, RESOLVED);

    expect(await resolutions.forgetPath(REPO, 'a.txt')).toBe(1);

    expect(
      (await historyEngine(repos, { resolutions }).threeWayMerge({ repository: REPO, ...sides }))
        .conflicts,
    ).toEqual(['a.txt']);
  });
});
