import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import git from 'isomorphic-git';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { hasRealGit, realGitRebase } from '../testutil/realgit.js';
import { hydrate, storeBackedFs } from './adapter.js';
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
  const dir = mkdtempSync(join(tmpdir(), 'parley-rebase-diff-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

/** The engine's rebase, in the shape the git harness answers in. */
async function engineRebase(root: Snapshot, baseCommits: Snapshot[], branchCommits: Snapshot[]) {
  const store = repos();
  await store.create(REPO);
  const rootCommit = await buildCommit(store, REPO, root, [], 'root');

  let base = rootCommit.oid;
  for (const [index, files] of baseCommits.entries()) {
    base = (await buildCommit(store, REPO, files, [base], `base ${String(index + 1)}`)).oid;
  }
  let branch = rootCommit.oid;
  for (const [index, files] of branchCommits.entries()) {
    branch = (await buildCommit(store, REPO, files, [branch], `branch ${String(index + 1)}`)).oid;
  }

  const result = await historyEngine(store).rebase({
    repository: REPO,
    branch,
    onto: base,
    committer: COMMITTER,
  });
  if (result.conflicts !== undefined) {
    return { conflicts: result.conflicts };
  }

  const backed = storeBackedFs();
  await git.init({ fs: backed.fs as never, gitdir: backed.gitdir, bare: true });
  await hydrate(store, REPO, backed);
  const messages = [];
  for (const oid of result.commits ?? []) {
    messages.push(
      (
        await git.readCommit({ fs: backed.fs as never, gitdir: backed.gitdir, oid })
      ).commit.message.trim(),
    );
  }
  const tip = (result.commits ?? []).at(-1) ?? base;
  return { messages, files: await readSnapshot(store, REPO, tip) };
}

describe.skipIf(!hasRealGit())(
  `a rebase against the real git binary${hasRealGit() ? '' : ' (needs git on the path)'}`,
  () => {
    it('drops a commit whose changes the base already has, as git does', async () => {
      const root: Snapshot = { 'a.txt': 'one\n' };
      // The base and the branch both add the same file with the same
      // content, so the branch's first commit is already upstream.
      const baseCommits: Snapshot[] = [{ 'a.txt': 'one\n', 'shared.txt': 'the same\n' }];
      const branchCommits: Snapshot[] = [
        { 'a.txt': 'one\n', 'shared.txt': 'the same\n' },
        { 'a.txt': 'one\n', 'shared.txt': 'the same\n', 'own.txt': 'only the branch\n' },
      ];

      const mine = await engineRebase(root, baseCommits, branchCommits);
      const real = realGitRebase(root, baseCommits, branchCommits);

      expect(mine.conflicts).toBeUndefined();
      expect(real.conflicts).toBeUndefined();
      expect(mine.messages).toEqual(real.messages);
      expect(mine.files).toEqual(real.files);
    });

    it.each([
      [
        'a branch of three commits onto a base of two',
        { 'a.txt': 'one\n' },
        [
          { 'a.txt': 'one\n', 'b.txt': 'base one\n' },
          { 'a.txt': 'one\n', 'b.txt': 'base one\n', 'c.txt': 'base two\n' },
        ],
        [
          { 'a.txt': 'one\n', 'x.txt': 'first\n' },
          { 'a.txt': 'one\n', 'x.txt': 'first\n', 'y.txt': 'second\n' },
          { 'a.txt': 'one\n', 'x.txt': 'first\n', 'y.txt': 'second\n', 'z.txt': 'third\n' },
        ],
      ],
      [
        'a branch that edits a file the base left alone',
        { 'a.txt': 'one\ntwo\nthree\n' },
        [{ 'a.txt': 'one\ntwo\nthree\n', 'b.txt': 'base\n' }],
        [{ 'a.txt': 'ONE\ntwo\nthree\n' }],
      ],
      [
        'a branch that deletes what the base left alone',
        { 'a.txt': 'one\n', 'doomed.txt': 'bye\n' },
        [{ 'a.txt': 'one\n', 'doomed.txt': 'bye\n', 'b.txt': 'base\n' }],
        [{ 'a.txt': 'one\n' }],
      ],
    ] as Array<[string, Snapshot, Snapshot[], Snapshot[]]>)(
      'produces the history git produces for %s',
      async (_name, root, baseCommits, branchCommits) => {
        const mine = await engineRebase(root, baseCommits, branchCommits);
        const real = realGitRebase(root, baseCommits, branchCommits);

        expect(mine.conflicts).toEqual(real.conflicts);
        expect(mine.messages).toEqual(real.messages);
        expect(mine.files).toEqual(real.files);
      },
    );

    it('conflicts on the same paths git conflicts on', async () => {
      const root: Snapshot = { 'a.txt': 'one\n' };
      const baseCommits: Snapshot[] = [{ 'a.txt': 'the base changed it\n' }];
      const branchCommits: Snapshot[] = [{ 'a.txt': 'the branch changed it\n' }];

      const mine = await engineRebase(root, baseCommits, branchCommits);
      const real = realGitRebase(root, baseCommits, branchCommits);

      expect(real.conflicts).toEqual(['a.txt']);
      expect(mine.conflicts).toEqual(real.conflicts);
    });
  },
);
