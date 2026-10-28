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
