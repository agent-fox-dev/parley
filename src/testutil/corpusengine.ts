import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import git from 'isomorphic-git';
import type { CorpusCase } from '../history/corpus.js';
import { hydrate, storeBackedFs } from '../history/adapter.js';
import { type Identity, historyEngine } from '../history/engine.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { buildCommit } from './history.js';
import type { CaseOutcome } from './corpusgit.js';

const REPO = 'corpus';
const WHO: Identity = {
  name: 'Parley',
  email: 'parley@agent-fox.dev',
  timestamp: 1_800_000_000,
  timezoneOffset: 0,
};

/** Runs one corpus case through the engine, answering as the git harness does. */
export async function engineCase(one: CorpusCase): Promise<CaseOutcome> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-corpus-engine-'));
  try {
    const repos = localRepositoryStore(dir);
    await repos.create(REPO);
    const engine = historyEngine(repos);

    const root = await buildCommit(repos, REPO, one.root, [], 'root');
    let base = root.oid;
    for (const [index, files] of one.base.entries()) {
      base = (await buildCommit(repos, REPO, files, [base], `base ${String(index + 1)}`)).oid;
    }
    let branch = root.oid;
    const branchHistory = [root.oid];
    for (const [index, files] of one.branch.entries()) {
      branch = (await buildCommit(repos, REPO, files, [branch], `branch ${String(index + 1)}`)).oid;
      branchHistory.push(branch);
    }

    if (one.crossOver === true) {
      const intoTopic = await engine.mergeCommit({
        repository: REPO,
        ours: branch,
        theirs: base,
        author: WHO,
        message: 'cross into topic',
      });
      if (intoTopic.conflicts !== undefined) {
        return { conflicts: intoTopic.conflicts };
      }
      branch = intoTopic.commit ?? branch;
      const intoMain = await engine.mergeCommit({
        repository: REPO,
        ours: base,
        theirs: branchHistory.at(-2) ?? root.oid,
        author: WHO,
        message: 'cross into main',
      });
      if (intoMain.conflicts !== undefined) {
        return { conflicts: intoMain.conflicts };
      }
      base = intoMain.commit ?? base;
    }

    if (one.operation === 'merge') {
      const merged = await engine.mergeCommit({
        repository: REPO,
        ours: base,
        theirs: branch,
        author: WHO,
        message: 'merge',
      });
      if (merged.conflicts !== undefined) {
        return { conflicts: merged.conflicts };
      }
      const commit = await readCommit(repos, merged.commit ?? '');
      return { tree: commit.tree, messages: ['merge'], parentCounts: [commit.parent.length] };
    }

    const rebased = await engine.rebase({
      repository: REPO,
      branch,
      onto: base,
      committer: WHO,
    });
    if (rebased.conflicts !== undefined) {
      return { conflicts: rebased.conflicts };
    }
    const produced = rebased.commits ?? [];
    const messages: string[] = [];
    let parentCounts: number[] = [];
    for (const oid of produced) {
      const commit = await readCommit(repos, oid);
      messages.push(commit.message.trim());
      parentCounts.push(commit.parent.length);
    }
    const tip = produced.at(-1) ?? base;
    const tipCommit = await readCommit(repos, tip);
    parentCounts = produced.length === 0 ? [] : parentCounts;
    return { tree: tipCommit.tree, messages, parentCounts };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

async function readCommit(repos: ReturnType<typeof localRepositoryStore>, oid: string) {
  const backed = storeBackedFs();
  await git.init({ fs: backed.fs as never, gitdir: backed.gitdir, bare: true });
  await hydrate(repos, REPO, backed);
  return (await git.readCommit({ fs: backed.fs as never, gitdir: backed.gitdir, oid })).commit;
}
