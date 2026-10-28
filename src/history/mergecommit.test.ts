import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import git from 'isomorphic-git';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { hydrate, storeBackedFs } from './adapter.js';
import { type Identity, historyEngine } from './engine.js';

const REPO = 'a-repository';
const AUTHOR: Identity = {
  name: 'Merger',
  email: 'merger@parley.test',
  timestamp: 1_800_000_000,
  timezoneOffset: 0,
};
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function repos(): RepositoryStore {
  const dir = mkdtempSync(join(tmpdir(), 'parley-mergecommit-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

async function commitOf(store: RepositoryStore, oid: string) {
  const backed = storeBackedFs();
  await git.init({ fs: backed.fs as never, gitdir: backed.gitdir, bare: true });
  await hydrate(store, REPO, backed);
  return (await git.readCommit({ fs: backed.fs as never, gitdir: backed.gitdir, oid })).commit;
}

async function twoSides(base: Snapshot, ours: Snapshot, theirs: Snapshot) {
  const store = repos();
  await store.create(REPO);
  const b = await buildCommit(store, REPO, base, [], 'base');
  const o = await buildCommit(store, REPO, ours, [b.oid], 'ours');
  const t = await buildCommit(store, REPO, theirs, [b.oid], 'theirs');
  return { store, base: b, ours: o, theirs: t };
}

describe('merging a branch into a base', () => {
  it('produces one commit whose parents are the base first and the branch second', async () => {
    const { store, ours, theirs } = await twoSides(
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'b.txt': 'ours\n' },
      { 'a.txt': 'base\n', 'c.txt': 'theirs\n' },
    );

    const result = await historyEngine(store).mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
      message: 'merge the branch',
    });

    expect(result.conflicts).toBeUndefined();
    const commit = await commitOf(store, result.commit ?? '');
    expect(commit.parent).toEqual([ours.oid, theirs.oid]);
    expect(commit.message.trim()).toBe('merge the branch');
    expect(commit.author.name).toBe(AUTHOR.name);
  });

  it('sets the commit’s tree to the three-way merge of the two sides', async () => {
    const { store, ours, theirs } = await twoSides(
      { 'a.txt': 'base\n' },
      { 'a.txt': 'base\n', 'b.txt': 'ours\n' },
      { 'a.txt': 'base\n', 'c.txt': 'theirs\n' },
    );
    const engine = historyEngine(store);

    const merged = await engine.threeWayMerge({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
    });
    const result = await engine.mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
    });

    const commit = await commitOf(store, result.commit ?? '');
    expect(commit.tree).toBe(merged.tree);
    expect(await readSnapshot(store, REPO, result.commit ?? '')).toEqual({
      'a.txt': 'base\n',
      'b.txt': 'ours\n',
      'c.txt': 'theirs\n',
    });
  });
});

describe('a merge that conflicts', () => {
  it('produces no commit, names the paths, and leaves the refs alone', async () => {
    const { store, ours, theirs } = await twoSides(
      { 'a.txt': 'base\n', 'quiet.txt': 'nobody\n' },
      { 'a.txt': 'ours\n', 'quiet.txt': 'nobody\n' },
      { 'a.txt': 'theirs\n', 'quiet.txt': 'nobody\n' },
    );
    const before = await store.listRefs(REPO);

    const result = await historyEngine(store).mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
    });

    expect(result.commit).toBeUndefined();
    expect(result.conflicts).toEqual(['a.txt']);
    expect(await store.listRefs(REPO)).toEqual(before);
  });

  it('names every conflicting path and no path that merged', async () => {
    const { store, ours, theirs } = await twoSides(
      { 'one.txt': 'base\n', 'two.txt': 'base\n', 'three.txt': 'base\n' },
      { 'one.txt': 'ours\n', 'two.txt': 'ours\n', 'three.txt': 'base\n' },
      { 'one.txt': 'theirs\n', 'two.txt': 'theirs\n', 'three.txt': 'base\n' },
    );

    const result = await historyEngine(store).mergeCommit({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
      author: AUTHOR,
    });

    expect(result.conflicts).toEqual(['one.txt', 'two.txt']);
  });
});
