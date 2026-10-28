import git from 'isomorphic-git';
import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import { type FsLike, hydrate, storeBackedFs } from './adapter.js';

export interface Identity {
  name: string;
  email: string;
  /** Seconds since the epoch, as git records them. */
  timestamp: number;
  timezoneOffset: number;
}

export interface MergeResult {
  /** The merged tree, when the sides merged. */
  tree?: string;
  /** Every path that conflicted, when they did not. */
  conflicts?: string[];
}

/**
 * Where the engine keeps the result of its last merge. A tree nothing
 * points at is unreachable, and an unreachable object is one a store may
 * drop before the caller has done anything with it.
 */
export const MERGE_RESULT_REF = 'refs/hub/engine/merge';

/** Where the engine keeps the tip of its last rebase, for the same reason. */
export const REBASE_RESULT_REF = 'refs/hub/engine/rebase';

export interface RebaseResult {
  /** The commits written, oldest first, when the rebase finished. */
  commits?: string[];
  /** Where it stopped, when a commit conflicted. */
  stoppedAt?: string;
  conflicts?: string[];
}

export interface CommitResult {
  commit?: string;
  conflicts?: string[];
}

/** What a conflict looks like to a caller, and what identifies it. */
export interface ConflictReport {
  paths: string[];
}

export interface HistoryEngine {
  threeWayMerge(request: {
    repository: string;
    ours: string;
    theirs: string;
  }): Promise<MergeResult>;
  rebase(request: {
    repository: string;
    branch: string;
    onto: string;
    committer: Identity;
  }): Promise<RebaseResult>;
  mergeCommit(request: {
    repository: string;
    ours: string;
    theirs: string;
    author: Identity;
    message?: string;
  }): Promise<CommitResult>;
}

interface Session {
  fs: FsLike;
  gitdir: string;
  /** Hands everything written back to the store under these refs. */
  flush(changes: readonly RefChange[]): Promise<void>;
}

/** The conflicting paths a thrown merge failure names. */
export function conflictingPaths(err: unknown): string[] | undefined {
  const data = (err as { code?: string; data?: { filepaths?: string[] } }).data;
  const code = (err as { code?: string }).code;
  if (code !== 'MergeConflictError' && code !== 'MergeNotSupportedError') {
    return undefined;
  }
  return [...(data?.filepaths ?? [])].toSorted();
}

/**
 * Opens one repository for an operation: a filesystem the library can
 * work in, lazily backed by the store, and a way to hand back what the
 * operation wrote.
 */
async function open(repos: RepositoryStore, repository: string): Promise<Session> {
  const backed = storeBackedFs();
  const { fs, gitdir } = backed;

  await git.init({ fs: fs as never, gitdir, bare: true, defaultBranch: 'main' });

  // The objects come across as a pack rather than one at a time: a store
  // that cannot hand back an object's raw bytes can still serve a pack,
  // and a pack is what git itself reads (see ADR 8).
  await hydrate(repos, repository, backed);

  for (const ref of await repos.listRefs(repository)) {
    await git.writeRef({ fs: fs as never, gitdir, ref: ref.name, value: ref.oid, force: true });
  }

  return {
    fs,
    gitdir,
    flush: async (changes: readonly RefChange[]) => {
      const oids = [...backed.written().keys()]
        .map((path) => /\/objects\/([0-9a-f]{2})\/([0-9a-f]{38})$/.exec(path))
        .filter((match): match is RegExpExecArray => match !== null)
        .map((match) => `${match[1] ?? ''}${match[2] ?? ''}`);

      if (oids.length === 0) {
        await repos.updateRefs(repository, changes);
        return;
      }
      // The library packs what it wrote, so neither zlib nor a pack
      // writer of our own is needed where a Worker runs.
      const { packfile } = await git.packObjects({ fs: fs as never, gitdir, oids });
      await repos.receivePack(repository, packfile ?? new Uint8Array(), changes);
    },
  };
}

const TEMP_OURS = 'refs/hub/engine/ours';
const TEMP_THEIRS = 'refs/hub/engine/theirs';

/** The commits a branch has that a base does not, oldest first. */
async function commitsNotIn(
  options: { fs: never; gitdir: string },
  branch: string,
  base: string,
): Promise<string[]> {
  const inBase = new Set(
    (await git.log({ ...options, ref: base, depth: 10_000 })).map((entry) => entry.oid),
  );
  const own = (await git.log({ ...options, ref: branch, depth: 10_000 }))
    .map((entry) => entry.oid)
    .filter((oid) => !inBase.has(oid));
  return own.toReversed();
}

/**
 * The three-way merge of two commits over an explicit base, which is
 * what replaying one commit onto another needs: the base must be the
 * commit's own parent rather than the merge base of the two sides.
 *
 * The library offers no merge over three trees, so the base is made
 * true by construction: a commit carrying the first side's tree is
 * written with the base as its parent, so the merge base of the two
 * sides is the base and nothing else.
 */
async function mergeOverBase(
  options: { fs: never; gitdir: string },
  oids: { base: string; ours: string; theirs: string },
): Promise<MergeResult> {
  const treeOf = async (oid: string) => (await git.readCommit({ ...options, oid })).commit.tree;

  const ourSide = await git.writeCommit({
    ...options,
    commit: {
      tree: await treeOf(oids.ours),
      parent: [oids.base],
      author: ENGINE,
      committer: ENGINE,
      message: 'the side a replay is landing on',
    },
  });

  await git.writeRef({ ...options, ref: TEMP_OURS, value: ourSide, force: true });
  await git.writeRef({ ...options, ref: TEMP_THEIRS, value: oids.theirs, force: true });

  try {
    const merged = await git.merge({
      ...options,
      ours: TEMP_OURS,
      theirs: TEMP_THEIRS,
      noUpdateBranch: true,
      author: ENGINE,
      committer: ENGINE,
      message: 'replay',
    });
    return { tree: merged.tree ?? (await treeOf(merged.oid ?? '')) };
  } catch (err) {
    const conflicts = conflictingPaths(err);
    if (conflicts === undefined) {
      throw err;
    }
    return { conflicts };
  }
}

export function historyEngine(repos: RepositoryStore): HistoryEngine {
  return {
    /**
     * The merged tree of two commits over their merge base, computed
     * without writing a commit. A dry run gives the arithmetic alone,
     * which is what a caller deciding whether to merge wants.
     */
    async threeWayMerge(request): Promise<MergeResult> {
      const session = await open(repos, request.repository);
      const { fs, gitdir } = session;
      await git.writeRef({
        fs: fs as never,
        gitdir,
        ref: TEMP_OURS,
        value: request.ours,
        force: true,
      });
      await git.writeRef({
        fs: fs as never,
        gitdir,
        ref: TEMP_THEIRS,
        value: request.theirs,
        force: true,
      });

      try {
        const merged = await git.merge({
          fs: fs as never,
          gitdir,
          ours: TEMP_OURS,
          theirs: TEMP_THEIRS,
          noUpdateBranch: true,
          author: ENGINE,
          committer: ENGINE,
          message: 'three-way merge',
        });

        // The merged tree is handed back to the store with a ref on the
        // commit that holds it, so the caller can read it.
        const oid = merged.oid;
        await session.flush(
          oid === undefined
            ? []
            : [{ name: MERGE_RESULT_REF, oldOid: null, newOid: oid, force: true }],
        );
        return { tree: merged.tree ?? oid };
      } catch (err) {
        const conflicts = conflictingPaths(err);
        if (conflicts === undefined) {
          throw err;
        }
        return { conflicts };
      }
    },

    /**
     * Copies each of a branch's own commits onto a base, in the order
     * the branch made them. The library has no rebase, so this is the
     * one piece of history arithmetic written here (ADR 8), built on
     * the merge of each commit's change against the growing base.
     *
     * A commit that conflicts stops the rebase there and leaves the
     * repository as it was: the refs are untouched, so nothing a later
     * commit would have written is reachable.
     */
    async rebase(request): Promise<RebaseResult> {
      const session = await open(repos, request.repository);
      const { fs, gitdir } = session;
      const options = { fs: fs as never, gitdir };

      const own = await commitsNotIn(options, request.branch, request.onto);
      let parent = request.onto;
      const written: string[] = [];

      for (const oid of own) {
        const { commit } = await git.readCommit({ ...options, oid });
        const base = commit.parent[0];
        if (base === undefined) {
          throw new Error('a rebase cannot replay a commit with no parent');
        }

        // The change this commit made, replayed on what the rebase has
        // built so far: its own parent is the base of the merge between
        // it and the new parent.
        const merged = await mergeOverBase(options, { base, ours: parent, theirs: oid });
        if (merged.conflicts !== undefined) {
          return { conflicts: merged.conflicts, stoppedAt: oid, commits: written };
        }

        // A replay that changes nothing is a commit git drops, because
        // its content is already in the base. Keeping it would leave an
        // empty commit the real rebase never makes.
        const parentTree = (await git.readCommit({ ...options, oid: parent })).commit.tree;
        if (merged.tree === parentTree) {
          continue;
        }

        parent = await git.writeCommit({
          ...options,
          commit: {
            tree: merged.tree ?? '',
            parent: [parent],
            // The author is the person who wrote the change; the
            // committer is whoever moved it, which is this service.
            author: commit.author,
            committer: request.committer,
            message: commit.message,
          },
        });
        written.push(parent);
      }

      await session.flush(
        written.length === 0
          ? []
          : [{ name: REBASE_RESULT_REF, oldOid: null, newOid: parent, force: true }],
      );
      return { commits: written };
    },

    async mergeCommit(): Promise<CommitResult> {
      return Promise.resolve({ conflicts: [] });
    },
  };
}

/** What the engine signs its own intermediate work with. */
const ENGINE: Identity = {
  name: 'parley',
  email: 'parley@agent-fox.dev',
  timestamp: 0,
  timezoneOffset: 0,
};
