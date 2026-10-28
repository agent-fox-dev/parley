import { mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { movableClock } from '../platform/local/clock.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { corpus } from './corpus.js';
import { compareCorpus } from '../testutil/corpusdiff.js';
import { buildCommit, readSnapshot } from '../testutil/history.js';
import { SEED_NOW, freshStore } from '../testutil/identity.js';
import { hasRealGit } from '../testutil/realgit.js';
import { conflictIdentity } from './conflict.js';
import { type Identity, historyEngine } from './engine.js';
import { resolutionStore } from './resolutions.js';

/**
 * The engine's three paths, each run end to end with the real parts:
 * a real repository store holding a real repository, the real
 * resolution store, and the real git binary to be compared against.
 * Nothing here is stubbed.
 */
const REPO = 'a-workspace-repository';
const COMMITTER: Identity = {
  name: 'Parley',
  email: 'parley@agent-fox.dev',
  timestamp: 1_700_000_100,
  timezoneOffset: 0,
};

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function temporary(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-smoke-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

/** Whether any directory on the path holds a git binary. */
function gitOnThePath(): boolean {
  return (process.env['PATH'] ?? '')
    .split(':')
    .filter(Boolean)
    .some((entry) => {
      try {
        return statSync(join(entry, 'git')).isFile();
      } catch {
        return false;
      }
    });
}

/** Runs something with nothing on the path, so no binary can be reached. */
async function withNoBinaries<T>(what: () => Promise<T>): Promise<T> {
  const path = process.env['PATH'];
  process.env['PATH'] = '';
  try {
    return await what();
  } finally {
    if (path === undefined) {
      delete process.env['PATH'];
    } else {
      process.env['PATH'] = path;
    }
  }
}

describe('a branch rebased in a real repository', () => {
  it('reads back with its commits in order and the base behind them', async () => {
    const repos: RepositoryStore = localRepositoryStore(temporary());
    await repos.create(REPO);

    const root = await buildCommit(repos, REPO, { 'a.txt': 'root\n' }, [], 'root');
    const onto = await buildCommit(
      repos,
      REPO,
      { 'a.txt': 'root\n', 'base.txt': 'the base moved on\n' },
      [root.oid],
      'the base moved on',
    );
    const first = await buildCommit(
      repos,
      REPO,
      { 'a.txt': 'root\n', 'one.txt': 'first\n' },
      [root.oid],
      'the first patch',
    );
    const second = await buildCommit(
      repos,
      REPO,
      { 'a.txt': 'root\n', 'one.txt': 'first\n', 'two.txt': 'second\n' },
      [first.oid],
      'the second patch',
    );

    let reachable = true;
    const rebased = await withNoBinaries(async () => {
      reachable = gitOnThePath();
      return historyEngine(repos).rebase({
        repository: REPO,
        branch: second.oid,
        onto: onto.oid,
        committer: COMMITTER,
      });
    });

    expect(reachable, 'no git binary was reachable while it ran').toBe(false);
    expect(rebased.conflicts).toBeUndefined();
    expect(rebased.commits).toHaveLength(2);

    // Read back through the store, which is the only way the engine
    // reached the objects in the first place.
    const head = rebased.commits?.at(-1) ?? '';
    expect(await readSnapshot(repos, REPO, head)).toEqual({
      'a.txt': 'root\n',
      'base.txt': 'the base moved on\n',
      'one.txt': 'first\n',
      'two.txt': 'second\n',
    });

    const written = new Map(
      (await repos.listRefs(REPO)).map((ref) => [ref.name, ref.oid] as const),
    );
    expect(written.get('refs/hub/engine/rebase'), 'the new head, in the store').toBe(head);
  });
});

describe('a resolution recorded before a restart', () => {
  it('settles the same conflict after everything but the store is new', async () => {
    const seeded = await freshStore();
    cleanups.push(seeded.dispose);
    const clock = movableClock(SEED_NOW).clock;
    const root = temporary();

    const base = 'one\ntwo\nthree\n';
    const ours = 'one\nOURS\nthree\n';
    const theirs = 'one\nTHEIRS\nthree\n';
    const settled = 'one\nSETTLED BY HAND\nthree\n';

    const repos = localRepositoryStore(root);
    await repos.create(REPO);
    const first = await buildCommit(repos, REPO, { 'a.txt': base }, [], 'base');
    const ourSide = await buildCommit(repos, REPO, { 'a.txt': ours }, [first.oid], 'ours');
    const theirSide = await buildCommit(repos, REPO, { 'a.txt': theirs }, [first.oid], 'theirs');

    // It conflicts the first time, and the conflict is settled by hand.
    const met = await historyEngine(repos).threeWayMerge({
      repository: REPO,
      ours: ourSide.oid,
      theirs: theirSide.oid,
    });
    expect(met.conflicts).toEqual(['a.txt']);

    const conflict = { path: 'a.txt', base, ours, theirs };
    await resolutionStore(seeded.store, clock).record(REPO, conflict, settled);

    // Everything but the store is new: a new engine, a new resolution
    // store, a new store over the same repository on disk.
    const restarted = historyEngine(localRepositoryStore(root), {
      resolutions: resolutionStore(seeded.store, clock),
    });
    const again = await restarted.threeWayMerge({
      repository: REPO,
      ours: ourSide.oid,
      theirs: theirSide.oid,
    });

    expect(again.conflicts, 'the conflict was settled, not reported').toBeUndefined();
    expect(again.tree).toMatch(/^[0-9a-f]{40}$/);

    // The identity is what connected the two: content, not a commit id.
    const [remembered] = await resolutionStore(seeded.store, clock).list(REPO);
    expect(remembered?.identity).toBe(await conflictIdentity(conflict));
  });
});

describe.skipIf(!hasRealGit())(
  `the whole corpus against the real git binary${hasRealGit() ? '' : ' (needs git on the path)'}`,
  () => {
    it(
      'agrees on every case it compared',
      async () => {
        const report = await compareCorpus(corpus());

        expect(report.compared, 'the cases compared').toBeGreaterThanOrEqual(200);
        expect(report.differences, 'what differed').toEqual([]);
        expect(report.mismatches).toBe(0);
      },
      15 * 60 * 1000,
    );
  },
);
