import git from 'isomorphic-git';
import { hydrate, storeBackedFs } from '../history/adapter.js';
import type { RepositoryStore } from '../platform/repostore.js';

/** What a commit holds: a path to its content. */
export type Snapshot = Record<string, string>;

export interface BuiltCommit {
  oid: string;
  tree: string;
}

const AUTHOR = {
  name: 'Fixture',
  email: 'fixture@parley.test',
  timestamp: 1_700_000_000,
  timezoneOffset: 0,
};

/**
 * Builds history in a repository the engine will work on, through the
 * same store the engine reads: a suite describes commits by their
 * content rather than by plumbing.
 */
export async function buildCommit(
  repos: RepositoryStore,
  repository: string,
  snapshot: Snapshot,
  parents: string[] = [],
  message = 'a commit',
): Promise<BuiltCommit> {
  const backed = storeBackedFs();
  const { fs, gitdir } = backed;
  await git.init({ fs: fs as never, gitdir, bare: true, defaultBranch: 'main' });
  await hydrate(repos, repository, backed);

  const entries = [];
  for (const [path, content] of Object.entries(snapshot)) {
    const oid = await git.writeBlob({
      fs: fs as never,
      gitdir,
      blob: new TextEncoder().encode(content),
    });
    entries.push({ mode: '100644', path, oid, type: 'blob' as const });
  }
  const tree = await git.writeTree({ fs: fs as never, gitdir, tree: entries });
  const oid = await git.writeCommit({
    fs: fs as never,
    gitdir,
    commit: { tree, parent: parents, author: AUTHOR, committer: AUTHOR, message },
  });

  const oids = [...backed.written().keys()]
    .map((path) => /\/objects\/([0-9a-f]{2})\/([0-9a-f]{38})$/.exec(path))
    .filter((match): match is RegExpExecArray => match !== null)
    .map((match) => `${match[1] ?? ''}${match[2] ?? ''}`);
  const { packfile } = await git.packObjects({ fs: fs as never, gitdir, oids });
  // A commit nothing points at is unreachable, and an unreachable object
  // is one a real store may drop. Every fixture commit gets a ref.
  await repos.receivePack(repository, packfile ?? new Uint8Array(), [
    { name: `refs/hub/fixtures/${oid}`, oldOid: null, newOid: oid, force: true },
  ]);

  return { oid, tree };
}

/** The content of a tree, as a suite compares it. */
export async function readSnapshot(
  repos: RepositoryStore,
  repository: string,
  treeOrCommit: string,
): Promise<Snapshot> {
  const backed = storeBackedFs();
  const { fs, gitdir } = backed;
  await git.init({ fs: fs as never, gitdir, bare: true, defaultBranch: 'main' });
  await hydrate(repos, repository, backed);

  let treeOid = treeOrCommit;
  try {
    treeOid = (await git.readCommit({ fs: fs as never, gitdir, oid: treeOrCommit })).commit.tree;
  } catch {
    // It was a tree to begin with.
  }

  const { tree } = await git.readTree({ fs: fs as never, gitdir, oid: treeOid });
  const snapshot: Snapshot = {};
  for (const entry of tree) {
    if (entry.type === 'blob') {
      const { blob } = await git.readBlob({ fs: fs as never, gitdir, oid: entry.oid });
      snapshot[entry.path] = new TextDecoder().decode(blob);
    }
  }
  return snapshot;
}
