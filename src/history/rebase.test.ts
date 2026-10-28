import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import git from 'isomorphic-git';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { hydrate, storeBackedFs } from './adapter.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { type Identity, historyEngine } from './engine.js';

const REPO = 'a-repository';
const COMMITTER: Identity = {
  name: 'Parley',
  email: 'parley@agent-fox.dev',
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
  const dir = mkdtempSync(join(tmpdir(), 'parley-rebase-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

/** Reads a commit back out of the store, for comparing with its original. */
async function commitOf(store: RepositoryStore, oid: string) {
  const backed = storeBackedFs();
  await git.init({ fs: backed.fs as never, gitdir: backed.gitdir, bare: true });
  await hydrate(store, REPO, backed);
  const { commit } = await git.readCommit({
    fs: backed.fs as never,
    gitdir: backed.gitdir,
    oid,
  });
  return commit;
}

/** A base of two commits and a branch of three, diverging from the first. */
async function diverged(store: RepositoryStore, branchFiles: Snapshot[]) {
  await store.create(REPO);
  const root = await buildCommit(store, REPO, { 'base.txt': 'one\n' }, [], 'root');
  const base = await buildCommit(
    store,
    REPO,
    { 'base.txt': 'one\n', 'base2.txt': 'two\n' },
    [root.oid],
    'second on base',
  );

  let parent = root.oid;
  const branch = [];
  for (const [index, files] of branchFiles.entries()) {
    const commit = await buildCommit(store, REPO, files, [parent], `branch ${String(index + 1)}`);
    branch.push(commit);
    parent = commit.oid;
  }
  return { root, base, branch, tip: parent };
}

const THREE: Snapshot[] = [
  { 'base.txt': 'one\n', 'x.txt': 'first\n' },
  { 'base.txt': 'one\n', 'x.txt': 'first\n', 'y.txt': 'second\n' },
  { 'base.txt': 'one\n', 'x.txt': 'first\n', 'y.txt': 'second\n', 'z.txt': 'third\n' },
];

describe('rebasing a branch onto a base', () => {
  it('copies every commit of the branch, in order, onto the base', async () => {
    const store = repos();
    const { base, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });

    expect(result.conflicts).toBeUndefined();
    expect(result.commits).toHaveLength(3);

    const messages = [];
    for (const oid of result.commits ?? []) {
      messages.push((await commitOf(store, oid)).message.trim());
    }
    expect(messages).toEqual(['branch 1', 'branch 2', 'branch 3']);
  });

  it('puts the base behind the copied commits', async () => {
    const store = repos();
    const { base, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });

    const first = await commitOf(store, (result.commits ?? [])[0] ?? '');
    expect(first.parent).toEqual([base.oid]);
  });

  it('carries the base’s own files into the result', async () => {
    const store = repos();
    const { base, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });

    const last = (result.commits ?? []).at(-1) ?? '';
    expect(await readSnapshot(store, REPO, last)).toEqual({
      'base.txt': 'one\n',
      'base2.txt': 'two\n',
      'x.txt': 'first\n',
      'y.txt': 'second\n',
      'z.txt': 'third\n',
    });
  });
});

describe('a copied commit', () => {
  it('keeps the author and the message of the one it copies', async () => {
    const store = repos();
    const { base, branch, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });

    for (const [index, oid] of (result.commits ?? []).entries()) {
      const copy = await commitOf(store, oid);
      const original = await commitOf(store, branch[index]?.oid ?? '');

      expect(copy.author).toEqual(original.author);
      expect(copy.message).toBe(original.message);
    }
  });

  it('takes the service as its committer, not the author', async () => {
    const store = repos();
    const { base, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });

    const copy = await commitOf(store, (result.commits ?? [])[0] ?? '');
    expect(copy.committer.name).toBe(COMMITTER.name);
    expect(copy.committer.email).toBe(COMMITTER.email);
    expect(copy.author.name).not.toBe(COMMITTER.name);
  });

  it('is a different commit from the one it copies, and follows the one before it', async () => {
    const store = repos();
    const { base, branch, tip } = await diverged(store, THREE);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: tip,
      onto: base.oid,
      committer: COMMITTER,
    });
    const copies = result.commits ?? [];

    expect(copies).not.toContain(branch[0]?.oid);
    expect((await commitOf(store, copies[1] ?? '')).parent).toEqual([copies[0]]);
    expect((await commitOf(store, copies[2] ?? '')).parent).toEqual([copies[1]]);
  });
});

describe('a rebase that conflicts', () => {
  it('stops at that commit, names the paths, and applies none after it', async () => {
    const store = repos();
    await store.create(REPO);
    const root = await buildCommit(store, REPO, { 'a.txt': 'base\n' }, [], 'root');
    // The base changes a.txt, and so does the branch's second commit.
    const base = await buildCommit(
      store,
      REPO,
      { 'a.txt': 'base changed it\n' },
      [root.oid],
      'base',
    );

    const first = await buildCommit(
      store,
      REPO,
      { 'a.txt': 'base\n', 'new.txt': 'fine\n' },
      [root.oid],
      'branch 1',
    );
    const second = await buildCommit(
      store,
      REPO,
      { 'a.txt': 'branch changed it\n', 'new.txt': 'fine\n' },
      [first.oid],
      'branch 2',
    );
    const third = await buildCommit(
      store,
      REPO,
      { 'a.txt': 'branch changed it\n', 'new.txt': 'fine\n', 'never.txt': 'unreached\n' },
      [second.oid],
      'branch 3',
    );
    const before = await store.listRefs(REPO);

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: third.oid,
      onto: base.oid,
      committer: COMMITTER,
    });

    expect(result.conflicts).toEqual(['a.txt']);
    expect(result.stoppedAt).toBe(second.oid);
    // The first commit was copied; nothing after the conflict was.
    expect(result.commits).toHaveLength(1);

    // And the repository is as it was: the rebase wrote no ref.
    expect(await store.listRefs(REPO)).toEqual(before);
  });

  it('leaves no ref pointing at anything the rebase built', async () => {
    const store = repos();
    await store.create(REPO);
    const root = await buildCommit(store, REPO, { 'a.txt': 'base\n' }, [], 'root');
    const base = await buildCommit(store, REPO, { 'a.txt': 'theirs\n' }, [root.oid], 'base');
    const branch = await buildCommit(store, REPO, { 'a.txt': 'ours\n' }, [root.oid], 'branch');

    const result = await historyEngine(store).rebase({
      repository: REPO,
      branch: branch.oid,
      onto: base.oid,
      committer: COMMITTER,
    });

    expect(result.conflicts).toEqual(['a.txt']);
    expect((await store.listRefs(REPO)).map((ref) => ref.name)).not.toContain(
      'refs/hub/engine/rebase',
    );
  });
});
