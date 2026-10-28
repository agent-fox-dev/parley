import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import git from 'isomorphic-git';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit } from '../testutil/history.js';
import { hasRealGit, realGitMergeTree } from '../testutil/realgit.js';
import { hydrate, storeBackedFs } from './adapter.js';
import { type Identity, historyEngine } from './engine.js';

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
  const dir = mkdtempSync(join(tmpdir(), 'parley-mc-diff-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

/** The engine's merge, built over the same history the git harness builds. */
async function engineMergeTree(
  root: Snapshot,
  oursCommits: Snapshot[],
  theirsCommits: Snapshot[],
  crissCross = false,
) {
  const store = repos();
  await store.create(REPO);
  const engine = historyEngine(store);
  const rootCommit = await buildCommit(store, REPO, root, [], 'root');

  let ours = rootCommit.oid;
  for (const [i, files] of oursCommits.entries()) {
    ours = (await buildCommit(store, REPO, files, [ours], `ours ${String(i + 1)}`)).oid;
  }
  let theirs = rootCommit.oid;
  const theirsHistory = [rootCommit.oid];
  for (const [i, files] of theirsCommits.entries()) {
    theirs = (await buildCommit(store, REPO, files, [theirs], `theirs ${String(i + 1)}`)).oid;
    theirsHistory.push(theirs);
  }

  if (crissCross) {
    const intoTopic = await engine.mergeCommit({
      repository: REPO,
      ours: theirs,
      theirs: ours,
      author: AUTHOR,
      message: 'cross into topic',
    });
    theirs = intoTopic.commit ?? theirs;
    const earlier = theirsHistory.at(-2) ?? rootCommit.oid;
    const intoMain = await engine.mergeCommit({
      repository: REPO,
      ours,
      theirs: earlier,
      author: AUTHOR,
      message: 'cross into main',
    });
    ours = intoMain.commit ?? ours;
  }

  const result = await engine.mergeCommit({
    repository: REPO,
    ours,
    theirs,
    author: AUTHOR,
    message: 'merge',
  });
  if (result.conflicts !== undefined) {
    return { conflicts: result.conflicts };
  }

  const backed = storeBackedFs();
  await git.init({ fs: backed.fs as never, gitdir: backed.gitdir, bare: true });
  await hydrate(store, REPO, backed);
  const { commit } = await git.readCommit({
    fs: backed.fs as never,
    gitdir: backed.gitdir,
    oid: result.commit ?? '',
  });
  return { tree: commit.tree };
}

describe.skipIf(!hasRealGit())(
  `the merge commit's tree against the real git binary${hasRealGit() ? '' : ' (needs git)'}`,
  () => {
    it('equals git’s for a simple two-sided merge', async () => {
      const root: Snapshot = { 'a.txt': 'one\n' };
      const ours: Snapshot[] = [{ 'a.txt': 'one\n', 'b.txt': 'ours\n' }];
      const theirs: Snapshot[] = [{ 'a.txt': 'one\n', 'c.txt': 'theirs\n' }];

      const mine = await engineMergeTree(root, ours, theirs);
      const real = realGitMergeTree(root, ours, theirs);

      expect(mine.conflicts).toBeUndefined();
      expect(mine.tree).toBe(real.tree);
    });

    it('equals git’s where each side edits its own region of one file', async () => {
      const root: Snapshot = { 'a.txt': 'one\ntwo\nthree\nfour\nfive\nsix\n' };
      const ours: Snapshot[] = [{ 'a.txt': 'ONE\ntwo\nthree\nfour\nfive\nsix\n' }];
      const theirs: Snapshot[] = [{ 'a.txt': 'one\ntwo\nthree\nfour\nfive\nSIX\n' }];

      const mine = await engineMergeTree(root, ours, theirs);
      const real = realGitMergeTree(root, ours, theirs);

      expect(mine.tree).toBe(real.tree);
    });

    it('equals git’s where the two sides share more than one merge base', async () => {
      const root: Snapshot = { 'a.txt': 'one\n' };
      const ours: Snapshot[] = [
        { 'a.txt': 'one\n', 'b.txt': 'ours one\n' },
        { 'a.txt': 'one\n', 'b.txt': 'ours one\n', 'b2.txt': 'ours two\n' },
      ];
      const theirs: Snapshot[] = [
        { 'a.txt': 'one\n', 'c.txt': 'theirs one\n' },
        { 'a.txt': 'one\n', 'c.txt': 'theirs one\n', 'c2.txt': 'theirs two\n' },
      ];

      const mine = await engineMergeTree(root, ours, theirs, true);
      const real = realGitMergeTree(root, ours, theirs, true);

      expect(mine.conflicts).toBeUndefined();
      expect(real.conflicts).toBeUndefined();
      expect(mine.tree).toBe(real.tree);
    });
  },
);
