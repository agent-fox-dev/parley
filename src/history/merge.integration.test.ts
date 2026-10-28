import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { type Snapshot, buildCommit, readSnapshot } from '../testutil/history.js';
import { hasRealGit, realGitMerge } from '../testutil/realgit.js';
import { historyEngine } from './engine.js';

const REPO = 'a-repository';
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function repos(): RepositoryStore {
  const dir = mkdtempSync(join(tmpdir(), 'parley-merge-diff-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return localRepositoryStore(dir);
}

/** The engine's answer, in the shape the git harness answers in. */
async function engineMerge(
  base: Snapshot,
  ours: Snapshot,
  theirs: Snapshot,
): Promise<{ conflicts?: string[]; files?: Snapshot }> {
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
  if (result.conflicts !== undefined) {
    return { conflicts: result.conflicts };
  }
  return { files: await readSnapshot(store, REPO, result.tree ?? '') };
}

/** The shapes a merge has to get right, each with a base and two sides. */
const SHAPES: Array<[string, Snapshot, Snapshot, Snapshot]> = [
  [
    'a rename on one side',
    { 'old.txt': 'the content\n', 'other.txt': 'quiet\n' },
    { 'new.txt': 'the content\n', 'other.txt': 'quiet\n' },
    { 'old.txt': 'the content\n', 'other.txt': 'quiet\n' },
  ],
  [
    'a deletion on one side',
    { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
    { 'a.txt': 'base\n' },
    { 'a.txt': 'base\n', 'gone.txt': 'doomed\n' },
  ],
  [
    'an addition on each side',
    { 'a.txt': 'base\n' },
    { 'a.txt': 'base\n', 'ours.txt': 'ours\n' },
    { 'a.txt': 'base\n', 'theirs.txt': 'theirs\n' },
  ],
  [
    'changes to different regions of one file',
    { 'a.txt': 'one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n' },
    { 'a.txt': 'ONE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n' },
    { 'a.txt': 'one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nTEN\n' },
  ],
  [
    'a binary file changed on one side',
    { 'blob.bin': '\u0000\u0001\u0002base\u0000\n' },
    { 'blob.bin': '\u0000\u0001\u0002ours\u0000\n' },
    { 'blob.bin': '\u0000\u0001\u0002base\u0000\n' },
  ],
  [
    'a binary file changed on both sides',
    { 'blob.bin': '\u0000\u0001\u0002base\u0000\n' },
    { 'blob.bin': '\u0000\u0001\u0002ours\u0000\n' },
    { 'blob.bin': '\u0000\u0001\u0002theirs\u0000\n' },
  ],
  [
    'one side changed what the other deleted',
    { 'a.txt': 'base\n' },
    { 'a.txt': 'ours changed it\n' },
    {},
  ],
];

describe.skipIf(!hasRealGit())(
  `each merge shape against the real git binary${hasRealGit() ? '' : ' (needs git on the path)'}`,
  () => {
    it.each(SHAPES)('agrees with git on %s', async (_name, base, ours, theirs) => {
      const mine = await engineMerge(base, ours, theirs);
      const real = realGitMerge(base, ours, theirs);

      if (real.conflicts !== undefined) {
        expect(mine.conflicts, 'git conflicted and the engine did not').toEqual(real.conflicts);
        return;
      }

      expect(mine.conflicts, 'the engine conflicted and git did not').toBeUndefined();
      expect(mine.files).toEqual(real.files);
    });

    it('does not line-merge a binary file, as git does not', async () => {
      const base = { 'blob.bin': '\u0000a\u0000b\u0000c\n' };
      const ours = { 'blob.bin': '\u0000a\u0000OURS\u0000c\n' };
      const theirs = { 'blob.bin': '\u0000a\u0000THEIRS\u0000c\n' };

      const mine = await engineMerge(base, ours, theirs);
      const real = realGitMerge(base, ours, theirs);

      // Both refuse it rather than producing a half-merged binary.
      expect(real.conflicts).toEqual(['blob.bin']);
      expect(mine.conflicts).toEqual(['blob.bin']);
    });
  },
);
