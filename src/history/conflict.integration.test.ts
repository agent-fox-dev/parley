import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit } from '../testutil/history.js';
import { hasRealGit, realGitMerge } from '../testutil/realgit.js';
import { type Identity, historyEngine } from './engine.js';
import { conflictIdentity } from './conflict.js';

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

function repos(): RepositoryStore {
  const dir = mkdtempSync(join(tmpdir(), 'parley-conflict-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

async function engineConflicts(base: Snapshot, ours: Snapshot, theirs: Snapshot) {
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
  return { store, result, commits: { base: b, ours: o, theirs: t } };
}

/** Shapes that conflict, and shapes that do not, mixed together. */
const CASES: Array<[string, Snapshot, Snapshot, Snapshot]> = [
  [
    'one path conflicting among three',
    { 'one.txt': 'base\n', 'two.txt': 'base\n', 'three.txt': 'base\n' },
    { 'one.txt': 'ours\n', 'two.txt': 'base\n', 'three.txt': 'ours only\n' },
    { 'one.txt': 'theirs\n', 'two.txt': 'theirs only\n', 'three.txt': 'base\n' },
  ],
  [
    'two paths conflicting',
    { 'a.txt': 'base\n', 'b.txt': 'base\n' },
    { 'a.txt': 'ours\n', 'b.txt': 'ours\n' },
    { 'a.txt': 'theirs\n', 'b.txt': 'theirs\n' },
  ],
  [
    'a change against a deletion',
    { 'a.txt': 'base\n', 'keep.txt': 'kept\n' },
    { 'a.txt': 'ours changed it\n', 'keep.txt': 'kept\n' },
    { 'keep.txt': 'kept\n' },
  ],
  [
    'nothing conflicting at all',
    { 'a.txt': 'base\n' },
    { 'a.txt': 'base\n', 'b.txt': 'ours\n' },
    { 'a.txt': 'base\n', 'c.txt': 'theirs\n' },
  ],
];

describe.skipIf(!hasRealGit())(
  `the paths that conflict, against the real git binary${hasRealGit() ? '' : ' (needs git)'}`,
  () => {
    it.each(CASES)('names what git names for %s', async (_name, base, ours, theirs) => {
      const { result } = await engineConflicts(base, ours, theirs);
      const real = realGitMerge(base, ours, theirs);

      expect(result.conflicts ?? []).toEqual(real.conflicts ?? []);
    });

    it('names no path that merged cleanly', async () => {
      const { result } = await engineConflicts(
        { 'fights.txt': 'base\n', 'agrees.txt': 'base\n' },
        { 'fights.txt': 'ours\n', 'agrees.txt': 'both the same\n' },
        { 'fights.txt': 'theirs\n', 'agrees.txt': 'both the same\n' },
      );

      expect(result.conflicts).toEqual(['fights.txt']);
    });
  },
);

describe('a conflicting operation', () => {
  it('leaves every ref exactly where it was', async () => {
    const store = repos();
    await store.create(REPO);
    const base = await buildCommit(store, REPO, { 'a.txt': 'base\n' }, [], 'base');
    const ours = await buildCommit(store, REPO, { 'a.txt': 'ours\n' }, [base.oid], 'ours');
    const theirs = await buildCommit(store, REPO, { 'a.txt': 'theirs\n' }, [base.oid], 'theirs');
    const before = await store.listRefs(REPO);

    const engine = historyEngine(store);
    const merged = await engine.threeWayMerge({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
    });
    const committed = await engine.mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
    });
    const rebased = await engine.rebase({
      repository: REPO,
      branch: ours.oid,
      onto: theirs.oid,
      committer: AUTHOR,
    });

    expect(merged.conflicts).toEqual(['a.txt']);
    expect(committed.conflicts).toEqual(['a.txt']);
    expect(rebased.conflicts).toEqual(['a.txt']);
    expect(await store.listRefs(REPO)).toEqual(before);
  });

  it('leaves nothing it attempted reachable from any ref', async () => {
    const store = repos();
    await store.create(REPO);
    const base = await buildCommit(store, REPO, { 'a.txt': 'base\n' }, [], 'base');
    const ours = await buildCommit(store, REPO, { 'a.txt': 'ours\n' }, [base.oid], 'ours');
    const theirs = await buildCommit(store, REPO, { 'a.txt': 'theirs\n' }, [base.oid], 'theirs');

    await historyEngine(store).mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
    });

    const refs = (await store.listRefs(REPO)).map((ref) => ref.name);
    expect(refs).not.toContain('refs/hub/engine/merge');
    expect(refs).not.toContain('refs/hub/engine/rebase');
  });
});

describe('the identity of a conflict a merge reported', () => {
  it('is the same for the same conflict met twice in different repositories', async () => {
    const shape = {
      path: 'a.txt',
      base: 'one\ntwo\nthree\n',
      ours: 'one\nOURS\nthree\n',
      theirs: 'one\nTHEIRS\nthree\n',
    };

    const first = await engineConflicts(
      { 'a.txt': shape.base },
      { 'a.txt': shape.ours },
      { 'a.txt': shape.theirs },
    );
    const second = await engineConflicts(
      { 'a.txt': shape.base },
      { 'a.txt': shape.ours },
      { 'a.txt': shape.theirs },
    );

    expect(first.result.conflicts).toEqual(['a.txt']);
    expect(second.result.conflicts).toEqual(['a.txt']);
    expect(await conflictIdentity(shape)).toBe(await conflictIdentity(shape));
  });

  it('differs when one side of the conflict differs', async () => {
    const left = { path: 'a.txt', base: 'base\n', ours: 'ours\n', theirs: 'theirs\n' };
    const right = { ...left, theirs: 'theirs, but different\n' };

    expect(await conflictIdentity(left)).not.toBe(await conflictIdentity(right));
  });
});
