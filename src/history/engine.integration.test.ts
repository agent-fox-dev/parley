import { mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { artifactsRepositoryStore } from '../platform/cloudflare/artifacts.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { fakeArtifacts } from '../testutil/fakeartifacts.js';
import { buildCommit, readSnapshot } from '../testutil/history.js';
import { historyEngine } from './engine.js';

/** Where a git binary would be found, if the path held one. */
function whereIsGit(): string | undefined {
  const path = process.env['PATH'] ?? '';
  for (const entry of path.split(':').filter(Boolean)) {
    try {
      if (statSync(join(entry, 'git')).isFile()) {
        return join(entry, 'git');
      }
    } catch {
      // Not here.
    }
  }
  return undefined;
}

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface Candidate {
  name: string;
  make: () => RepositoryStore;
}

function tempRoot(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-engine-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

const candidates: Candidate[] = [
  { name: 'the local store', make: () => localRepositoryStore(tempRoot()) },
  {
    name: 'the Cloudflare store',
    make: () => {
      const fake = fakeArtifacts();
      cleanups.push(fake.dispose);
      return artifactsRepositoryStore(fake.binding, { fetch: fake.fetch });
    },
  },
];

const REPO = 'a-repository';

/** A base and two sides that touch different paths, so they merge. */
async function divergent(repos: RepositoryStore) {
  await repos.create(REPO);
  const base = await buildCommit(repos, REPO, { 'a.txt': 'base\n' }, [], 'base');
  const ours = await buildCommit(
    repos,
    REPO,
    { 'a.txt': 'base\n', 'b.txt': 'ours\n' },
    [base.oid],
    'ours',
  );
  const theirs = await buildCommit(
    repos,
    REPO,
    { 'a.txt': 'base\n', 'c.txt': 'theirs\n' },
    [base.oid],
    'theirs',
  );
  return { base, ours, theirs };
}

describe.each(candidates)('the engine over $name', ({ make }) => {
  it('merges two sides that touch different paths', async () => {
    const repos = make();
    const { ours, theirs } = await divergent(repos);

    const merged = await historyEngine(repos).threeWayMerge({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
    });

    expect(merged.conflicts).toBeUndefined();
    expect(merged.tree).toMatch(/^[0-9a-f]{40}$/);
  });

  it('reports the path two sides both changed, and no tree', async () => {
    const repos = make();
    await repos.create(REPO);
    const base = await buildCommit(repos, REPO, { 'a.txt': 'base\n' }, [], 'base');
    const ours = await buildCommit(repos, REPO, { 'a.txt': 'ours\n' }, [base.oid], 'ours');
    const theirs = await buildCommit(repos, REPO, { 'a.txt': 'theirs\n' }, [base.oid], 'theirs');

    const merged = await historyEngine(repos).threeWayMerge({
      repository: REPO,
      ours: ours.oid,
      theirs: theirs.oid,
    });

    expect(merged.conflicts).toEqual(['a.txt']);
    expect(merged.tree).toBeUndefined();
  });

  it('reads the history a suite built back through the store', async () => {
    const repos = make();
    const { ours } = await divergent(repos);

    expect(await readSnapshot(repos, REPO, ours.oid)).toEqual({
      'a.txt': 'base\n',
      'b.txt': 'ours\n',
    });
  });
});

describe('the engine and the two stores', () => {
  it('gives the same answer whichever store it is given', async () => {
    const answers: Array<string | undefined> = [];
    for (const candidate of candidates) {
      const repos = candidate.make();
      const { ours, theirs } = await divergent(repos);
      const merged = await historyEngine(repos).threeWayMerge({
        repository: REPO,
        ours: ours.oid,
        theirs: theirs.oid,
      });
      answers.push(merged.tree);
    }

    expect(answers[0]).toBe(answers[1]);
    expect(answers[0]).toMatch(/^[0-9a-f]{40}$/);
  });
});

describe('the engine and the environment it runs in', () => {
  it('works with no git executable anywhere on the path', async () => {
    const repos = localRepositoryStore(tempRoot());
    const { ours, theirs } = await divergent(repos);
    const path = process.env['PATH'];

    try {
      // Nothing can be found to execute, so an engine that shelled out
      // to git would fail here rather than quietly succeeding.
      process.env['PATH'] = '';
      expect(whereIsGit()).toBeUndefined();

      const merged = await historyEngine(repos).threeWayMerge({
        repository: REPO,
        ours: ours.oid,
        theirs: theirs.oid,
      });

      expect(merged.tree).toMatch(/^[0-9a-f]{40}$/);
    } finally {
      process.env['PATH'] = path;
    }
  });

  it('merges the same whether git is on the path or not', async () => {
    const withGit = localRepositoryStore(tempRoot());
    const withoutGit = localRepositoryStore(tempRoot());
    const first = await divergent(withGit);
    const second = await divergent(withoutGit);

    const before = await historyEngine(withGit).threeWayMerge({
      repository: REPO,
      ours: first.ours.oid,
      theirs: first.theirs.oid,
    });

    const path = process.env['PATH'];
    let after;
    try {
      process.env['PATH'] = '';
      after = await historyEngine(withoutGit).threeWayMerge({
        repository: REPO,
        ours: second.ours.oid,
        theirs: second.theirs.oid,
      });
    } finally {
      process.env['PATH'] = path;
    }

    expect(after.tree).toBe(before.tree);
  });
});

/** A stack of patches on top of a base, each adding one file. */
async function stack(repos: RepositoryStore, depth: number) {
  await repos.create(REPO);
  const root = await buildCommit(repos, REPO, { 'a.txt': 'root\n' }, [], 'root');
  const onto = await buildCommit(
    repos,
    REPO,
    { 'a.txt': 'root\n', 'base.txt': 'moved on\n' },
    [root.oid],
    'the base moved on',
  );

  let parent = root.oid;
  const carried: Record<string, string> = { 'a.txt': 'root\n' };
  for (let i = 0; i < depth; i += 1) {
    carried[`patch-${String(i)}.txt`] = `patch ${String(i)}\n`;
    const built = await buildCommit(repos, REPO, { ...carried }, [parent], `patch ${String(i)}`);
    parent = built.oid;
  }
  return { onto: onto.oid, branch: parent };
}

const COMMITTER = {
  name: 'Parley',
  email: 'parley@parley.test',
  timestamp: 1_700_000_100,
  timezoneOffset: 0,
};

describe.each(candidates)('an operation larger than one unit of work, over $name', ({ make }) => {
  it('is carried on until it finishes, with the result of one that fits', async () => {
    const repos = make();
    const { onto, branch } = await stack(repos, 9);
    const engine = historyEngine(repos);
    const request = { repository: REPO, branch, onto, committer: COMMITTER };

    const whole = await engine.rebase(request);

    // Three patches to a unit of work, so the same rebase has to be
    // taken up again twice before it is finished.
    let slice = await engine.rebase({ ...request, budget: { patches: 3 } });
    let continuations = 0;
    while (slice.continuation !== undefined) {
      continuations += 1;
      slice = await engine.carryOn(slice.continuation);
    }

    expect(continuations).toBeGreaterThanOrEqual(1);
    expect(slice.conflicts).toBeUndefined();
    expect(slice.commits).toEqual(whole.commits);
    expect(await readSnapshot(repos, REPO, slice.commits?.at(-1) ?? '')).toEqual(
      await readSnapshot(repos, REPO, whole.commits?.at(-1) ?? ''),
    );
  });

  it('leaves every ref where it was when it is abandoned part way', async () => {
    const repos = make();
    const { onto, branch } = await stack(repos, 9);
    const engine = historyEngine(repos);
    const before = await repos.listRefs(REPO);

    const started = await engine.rebase({
      repository: REPO,
      branch,
      onto,
      committer: COMMITTER,
      budget: { patches: 2 },
    });

    expect(started.continuation).toBeDefined();
    // Abandoned: nothing takes the continuation up, and the engine is
    // let go of.
    expect(await repos.listRefs(REPO)).toEqual(before);
  });

  it('leaves every ref where it was when it fails part way', async () => {
    const repos = make();
    const { onto, branch } = await stack(repos, 4);
    const before = await repos.listRefs(REPO);
    const failing: RepositoryStore = {
      ...repos,
      receivePack: () => Promise.reject(new Error('the store went away')),
      updateRefs: () => Promise.reject(new Error('the store went away')),
    };

    await expect(
      historyEngine(failing).rebase({
        repository: REPO,
        branch,
        onto,
        committer: COMMITTER,
      }),
    ).rejects.toThrow('the store went away');

    expect(await repos.listRefs(REPO)).toEqual(before);
  });

  it('refuses a stack past the patch limit before it writes anything', async () => {
    const repos = make();
    const { onto, branch } = await stack(repos, 4);
    const before = await repos.listRefs(REPO);

    await expect(
      historyEngine(repos).rebase({
        repository: REPO,
        branch,
        onto,
        committer: COMMITTER,
        limits: { patches: 3 },
      }),
    ).rejects.toThrow(/patches/);

    expect(await repos.listRefs(REPO)).toEqual(before);
  });
});
