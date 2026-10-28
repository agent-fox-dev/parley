import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { historyEngine } from './engine.js';

const REPO = 'a-repository';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function repos(): RepositoryStore {
  const dir = mkdtempSync(join(tmpdir(), 'parley-merge-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

/** Builds a base and two sides, and merges them. */
async function merged(base: Snapshot, ours: Snapshot, theirs: Snapshot) {
  const store = repos();
  await store.create(REPO);
  const b = await buildCommit(store, REPO, base, [], 'base');
  const o = await buildCommit(store, REPO, ours, [b.oid], 'ours');
  const t = await buildCommit(store, REPO, theirs, [b.oid], 'theirs');

  const result = await historyEngine(store).threeWayMerge({
    repository: REPO,
    ours: o.oid,
    theirs: t.oid,
  });
  return {
    result,
    store,
    snapshot: async () => (result.tree === undefined ? {} : readSnapshot(store, REPO, result.tree)),
  };
}

describe('a path only one side changed', () => {
  it('is taken as that side left it', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n', 'b.txt': 'untouched\n' },
      { 'a.txt': 'ours changed it\n', 'b.txt': 'untouched\n' },
      { 'a.txt': 'base\n', 'b.txt': 'untouched\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'ours changed it\n', 'b.txt': 'untouched\n' });
  });

  it('is added when one side added it', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'new.txt': 'added by us\n' },
      { 'a.txt': 'base\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'base\n', 'new.txt': 'added by us\n' });
  });

  it('is gone when one side deleted it', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'base\n' });
  });

  it('takes changes from both sides when they touch different paths', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n', 'b.txt': 'base\n' },
      { 'a.txt': 'ours\n', 'b.txt': 'base\n' },
      { 'a.txt': 'base\n', 'b.txt': 'theirs\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'ours\n', 'b.txt': 'theirs\n' });
  });
});

describe('a change both sides made identically', () => {
  it('merges with no conflict and keeps that content', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n' },
      { 'a.txt': 'the same new line\n' },
      { 'a.txt': 'the same new line\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'the same new line\n' });
  });

  it('merges an identical addition on both sides', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'new.txt': 'both added this\n' },
      { 'a.txt': 'base\n', 'new.txt': 'both added this\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'base\n', 'new.txt': 'both added this\n' });
  });

  it('merges an identical deletion on both sides', async () => {
    const { result, snapshot } = await merged(
      { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n' },
    );

    expect(result.conflicts).toBeUndefined();
    expect(await snapshot()).toEqual({ 'a.txt': 'base\n' });
  });
});

describe('a region both sides changed differently', () => {
  it('conflicts, names the path, and produces no tree', async () => {
    const { result } = await merged(
      { 'a.txt': 'base\n', 'quiet.txt': 'nobody touched this\n' },
      { 'a.txt': 'ours\n', 'quiet.txt': 'nobody touched this\n' },
      { 'a.txt': 'theirs\n', 'quiet.txt': 'nobody touched this\n' },
    );

    expect(result.conflicts).toEqual(['a.txt']);
    expect(result.tree).toBeUndefined();
  });

  it('names every path that conflicted and no path that did not', async () => {
    const { result } = await merged(
      { 'one.txt': 'base\n', 'two.txt': 'base\n', 'three.txt': 'base\n' },
      { 'one.txt': 'ours\n', 'two.txt': 'ours\n', 'three.txt': 'base\n' },
      { 'one.txt': 'theirs\n', 'two.txt': 'theirs\n', 'three.txt': 'base\n' },
    );

    expect(result.conflicts).toEqual(['one.txt', 'two.txt']);
  });

  it('conflicts when one side changed a path the other deleted', async () => {
    const { result } = await merged({ 'a.txt': 'base\n' }, { 'a.txt': 'ours changed it\n' }, {});

    expect(result.conflicts).toEqual(['a.txt']);
    expect(result.tree).toBeUndefined();
  });
});

describe('the order the two sides are given in', () => {
  const cases: Array<[string, Snapshot, Snapshot, Snapshot]> = [
    [
      'changes on different paths',
      { 'a.txt': 'base\n', 'b.txt': 'base\n' },
      { 'a.txt': 'ours\n', 'b.txt': 'base\n' },
      { 'a.txt': 'base\n', 'b.txt': 'theirs\n' },
    ],
    [
      'an addition on each side',
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'ours.txt': 'ours\n' },
      { 'a.txt': 'base\n', 'theirs.txt': 'theirs\n' },
    ],
    [
      'a deletion on one side and an addition on the other',
      { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'gone.txt': 'doomed\n', 'new.txt': 'added\n' },
    ],
    [
      'the same change on both sides',
      { 'a.txt': 'base\n' },
      { 'a.txt': 'same\n' },
      { 'a.txt': 'same\n' },
    ],
    [
      'many paths at once',
      Object.fromEntries(Array.from({ length: 8 }, (_, i) => [`f${String(i)}.txt`, 'base\n'])),
      Object.fromEntries(
        Array.from({ length: 8 }, (_, i) => [
          `f${String(i)}.txt`,
          i % 2 === 0 ? 'ours\n' : 'base\n',
        ]),
      ),
      Object.fromEntries(
        Array.from({ length: 8 }, (_, i) => [
          `f${String(i)}.txt`,
          i % 2 === 1 ? 'theirs\n' : 'base\n',
        ]),
      ),
    ],
  ];

  it.each(cases)('gives the same tree either way round: %s', async (_name, base, ours, theirs) => {
    const oneWay = await merged(base, ours, theirs);
    const otherWay = await merged(base, theirs, ours);

    expect(oneWay.result.conflicts).toBeUndefined();
    expect(otherWay.result.conflicts).toBeUndefined();
    expect(otherWay.result.tree).toBe(oneWay.result.tree);
  });

  it.each(cases)(
    'agrees on the content either way round: %s',
    async (_name, base, ours, theirs) => {
      const oneWay = await merged(base, ours, theirs);
      const otherWay = await merged(base, theirs, ours);

      expect(await otherWay.snapshot()).toEqual(await oneWay.snapshot());
    },
  );
});
