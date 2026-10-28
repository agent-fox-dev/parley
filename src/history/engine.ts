import git from 'isomorphic-git';
import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import { type FsLike, hydrate, storeBackedFs } from './adapter.js';
import { applyRename, buildTree, flatten, renamesBetween } from './trees.js';
import { ENGINE_LIMITS, type EngineLimits, withinLimits } from './limits.js';
import type { ResolutionStore } from './resolutions.js';

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

/**
 * What one unit of platform work may spend on an operation. A Worker
 * invocation is bounded, so an operation larger than one unit stops at
 * the boundary and hands back the means to take it up again.
 */
export interface Budget {
  /** The most patches one unit of work replays. */
  patches?: number;
  /** When this unit of work must be given up, in epoch milliseconds. */
  deadline?: number;
  now?: () => number;
}

/**
 * An operation that ran out of its unit of work. It holds everything
 * written so far, which reaches the store only when the operation
 * finishes: a continuation nobody takes up leaves the repository as it
 * was.
 */
export interface Continuation {
  /** How many units of work have carried this operation so far. */
  slices: number;
  /** Carries it on in another unit. */
  resume(): Promise<RebaseResult>;
}

export interface RebaseResult {
  /** The commits written, oldest first, when the rebase finished. */
  commits?: string[];
  /** Where it stopped, when a commit conflicted. */
  stoppedAt?: string;
  conflicts?: string[];
  /** Set when the unit of work ran out before the rebase did. */
  continuation?: Continuation;
  /** How many units of work carried it. */
  slices?: number;
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
    /** The sizes this operation may work at; the engine's by default. */
    limits?: Partial<EngineLimits>;
  }): Promise<MergeResult>;
  rebase(request: {
    repository: string;
    branch: string;
    onto: string;
    committer: Identity;
    /** What one unit of platform work may spend; one unit by default. */
    budget?: Budget;
    /** The sizes this operation may work at; the engine's by default. */
    limits?: Partial<EngineLimits>;
  }): Promise<RebaseResult>;
  /** Takes up an operation that ran out of its unit of work. */
  carryOn(continuation: Continuation): Promise<RebaseResult>;
  mergeCommit(request: {
    repository: string;
    ours: string;
    theirs: string;
    author: Identity;
    message?: string;
    /** The sizes this operation may work at; the engine's by default. */
    limits?: Partial<EngineLimits>;
  }): Promise<CommitResult>;
}

interface Session {
  fs: FsLike;
  gitdir: string;
  /** What the repository's objects weighed, packed, when it was read. */
  packedBytes: number;
  /** Hands everything written back to the store under these refs. */
  flush(changes: readonly RefChange[]): Promise<void>;
}

/** The conflicting paths a thrown merge failure names. */
export function conflictingPaths(err: unknown): string[] | undefined {
  const data = (err as { code?: string; data?: { filepaths?: string[] } }).data;
  if ((err as { code?: string }).code !== 'MergeConflictError') {
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
  const packedBytes = await hydrate(repos, repository, backed);

  for (const ref of await repos.listRefs(repository)) {
    await git.writeRef({ fs: fs as never, gitdir, ref: ref.name, value: ref.oid, force: true });
  }

  return {
    fs,
    gitdir,
    packedBytes,
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

/**
 * The base a three-way merge should use. Two commits usually share one
 * merge base, and that is it; where they share several — a history that
 * crossed over — git merges the bases into a virtual one and merges
 * against that. The library does neither, so this does.
 */
async function mergeBaseOf(
  options: { fs: never; gitdir: string },
  ours: string,
  theirs: string,
): Promise<string> {
  const bases = (await git.findMergeBase({ ...options, oids: [ours, theirs] })) as string[];
  const first = bases[0];
  if (first === undefined) {
    throw new Error('these two commits share no history');
  }
  if (bases.length === 1) {
    return first;
  }

  let base = first;
  for (const next of bases.slice(1)) {
    // Each pair of bases is merged as any other pair would be, which is
    // where the recursion is: their own base may itself be several.
    const merged = await mergeOverBase(options, {
      base: await mergeBaseOf(options, base, next),
      ours: base,
      theirs: next,
    });
    base = await git.writeCommit({
      ...options,
      commit: {
        // A region the bases disagree on is left as the conflict wrote
        // it, which is what git's own virtual base does.
        tree: merged.tree ?? (await git.readCommit({ ...options, oid: base })).commit.tree,
        parent: [base, next],
        author: ENGINE,
        committer: ENGINE,
        message: 'a virtual base, merged from two',
      },
    });
  }
  return base;
}

/**
 * How large the change one commit made is: the paths whose content it
 * added, removed or altered, and what those paths weigh on the new
 * side. It is the measure the size limits are stated in, and it is
 * taken before the patch is replayed rather than during it.
 */
async function patchSize(
  options: { fs: never; gitdir: string },
  base: string,
  oid: string,
): Promise<{ changedFiles: number; diffBytes: number }> {
  const treeOf = async (commit: string) =>
    flatten(options, (await git.readCommit({ ...options, oid: commit })).commit.tree);
  const before = await treeOf(base);
  const after = await treeOf(oid);

  const changed = [...new Set([...before.keys(), ...after.keys()])].filter(
    (path) => before.get(path)?.oid !== after.get(path)?.oid,
  );

  let diffBytes = 0;
  for (const path of changed) {
    const entry = after.get(path);
    if (entry === undefined) {
      continue;
    }
    const { blob } = await git.readBlob({ ...options, oid: entry.oid });
    diffBytes += blob.byteLength;
  }
  return { changedFiles: changed.length, diffBytes };
}

/**
 * The base of a merge, refused before any of it is done when the
 * repository or either side is larger than an operation may work at. A
 * merge is two patches over that base, so each side is measured as one.
 */
async function sizedMerge(
  options: { fs: never; gitdir: string },
  session: { packedBytes: number },
  request: { ours: string; theirs: string; limits?: Partial<EngineLimits> },
): Promise<string> {
  const limits = { ...ENGINE_LIMITS, ...request.limits };
  withinLimits({ repositoryBytes: session.packedBytes }, limits);

  const base = await mergeBaseOf(options, request.ours, request.theirs);
  withinLimits(await patchSize(options, base, request.ours), limits);
  withinLimits(await patchSize(options, base, request.theirs), limits);
  return base;
}

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
  replay?: MergeDriver,
): Promise<MergeResult> {
  const treeOf = async (oid: string) => (await git.readCommit({ ...options, oid })).commit.tree;

  const trees = await alignRenames(options, {
    base: await treeOf(oids.base),
    ours: await treeOf(oids.ours),
    theirs: await treeOf(oids.theirs),
  });

  // The base the two sides are merged over. Where a rename was aligned
  // it carries the adjusted tree, so the merge sees a modification at
  // the path the file moved to rather than an edit to a file that went.
  const baseCommit =
    trees.base === (await treeOf(oids.base))
      ? oids.base
      : await git.writeCommit({
          ...options,
          commit: {
            tree: trees.base,
            parent: [],
            author: ENGINE,
            committer: ENGINE,
            message: 'the base of a merge, with renames aligned',
          },
        });

  const sideOn = async (tree: string, what: string) =>
    git.writeCommit({
      ...options,
      commit: {
        tree,
        parent: [baseCommit],
        author: ENGINE,
        committer: ENGINE,
        message: what,
      },
    });

  const ourSide = await sideOn(trees.ours, 'one side of a merge over a stated base');
  const theirSide = await sideOn(trees.theirs, 'the other side of a merge over a stated base');

  await git.writeRef({ ...options, ref: TEMP_OURS, value: ourSide, force: true });
  await git.writeRef({ ...options, ref: TEMP_THEIRS, value: theirSide, force: true });

  try {
    const merged = await git.merge({
      ...options,
      ours: TEMP_OURS,
      theirs: TEMP_THEIRS,
      noUpdateBranch: true,
      author: ENGINE,
      committer: ENGINE,
      message: 'replay',
      ...(replay === undefined ? {} : { mergeDriver: replay }),
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

/**
 * Moves a file the other side left alone to the path one side renamed
 * it to, in the base and in that other side. The library detects no
 * renames, so a side that edits a file the other side moved would
 * otherwise read as an edit to a file that was deleted; git follows the
 * rename and merges the edit, and so does this.
 *
 * A path both sides moved is left where it is: that is a conflict, and
 * the merge reports it as one.
 */
async function alignRenames(
  options: { fs: never; gitdir: string },
  trees: { base: string; ours: string; theirs: string },
): Promise<{ base: string; ours: string; theirs: string }> {
  let base = await flatten(options, trees.base);
  let ours = await flatten(options, trees.ours);
  let theirs = await flatten(options, trees.theirs);

  const ourRenames = renamesBetween(base, ours);
  const theirRenames = renamesBetween(base, theirs);
  let moved = false;

  for (const [from, to] of ourRenames) {
    if (theirRenames.has(from) || !theirs.has(from)) {
      continue;
    }
    base = applyRename(base, from, to);
    theirs = applyRename(theirs, from, to);
    moved = true;
  }
  for (const [from, to] of theirRenames) {
    if (ourRenames.has(from) || !ours.has(from)) {
      continue;
    }
    base = applyRename(base, from, to);
    ours = applyRename(ours, from, to);
    moved = true;
  }

  if (!moved) {
    return trees;
  }
  return {
    base: await buildTree(options, base),
    ours: await buildTree(options, ours),
    theirs: await buildTree(options, theirs),
  };
}

/**
 * What the library asks when a path needs merging. The three contents
 * arrive in the order base, ours, theirs.
 */
type MergeDriver = (args: {
  branches: string[];
  contents: string[];
  path: string;
}) => Promise<{ cleanMerge: boolean; mergedText: string }>;

export interface EngineDeps {
  /** Where resolutions are remembered, when the deployment keeps them. */
  resolutions?: ResolutionStore;
}

export function historyEngine(repos: RepositoryStore, deps: EngineDeps = {}): HistoryEngine {
  /**
   * Replays what was recorded for a conflict, so an operation that met
   * it before does not stop at it again. A path with nothing recorded
   * is merged as it would have been.
   */
  function replayFor(repository: string): MergeDriver | undefined {
    const resolutions = deps.resolutions;
    if (resolutions === undefined) {
      return undefined;
    }
    return async ({ contents, path }) => {
      const [base, ours, theirs] = contents;
      const recorded = await resolutions.replay(repository, {
        path,
        base: base ?? '',
        ours: ours ?? '',
        theirs: theirs ?? '',
      });
      if (recorded !== undefined) {
        return { cleanMerge: true, mergedText: recorded };
      }
      // Nothing recorded: let the library merge it as it always would,
      // which it does by taking the driver's refusal.
      return { cleanMerge: false, mergedText: ours ?? '' };
    };
  }

  return {
    /**
     * The merged tree of two commits over their merge base, computed
     * without writing a commit. A dry run gives the arithmetic alone,
     * which is what a caller deciding whether to merge wants.
     */
    async threeWayMerge(request): Promise<MergeResult> {
      const session = await open(repos, request.repository);
      const options = { fs: session.fs as never, gitdir: session.gitdir };
      const base = await sizedMerge(options, session, request);

      const merged = await mergeOverBase(
        options,
        {
          base,
          ours: request.ours,
          theirs: request.theirs,
        },
        replayFor(request.repository),
      );
      if (merged.conflicts !== undefined) {
        return { conflicts: merged.conflicts };
      }

      // The merged tree goes back to the store under a commit, so the
      // caller can read it: an object nothing points at is one the
      // store may drop.
      const holder = await git.writeCommit({
        ...options,
        commit: {
          tree: merged.tree ?? '',
          parent: [request.ours, request.theirs],
          author: ENGINE,
          committer: ENGINE,
          message: 'three-way merge',
        },
      });
      await session.flush([{ name: MERGE_RESULT_REF, oldOid: null, newOid: holder, force: true }]);
      return { tree: merged.tree };
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
      const limits = { ...ENGINE_LIMITS, ...request.limits };
      const session = await open(repos, request.repository);
      const { fs, gitdir } = session;
      const options = { fs: fs as never, gitdir };
      withinLimits({ repositoryBytes: session.packedBytes }, limits);

      const own = await commitsNotIn(options, request.branch, request.onto);
      withinLimits({ patches: own.length }, limits);

      /**
       * One unit of work: it replays patches until the budget is spent
       * or there are none left. Nothing reaches the store until the
       * last patch is in, so an operation abandoned between units
       * leaves the repository exactly as it was.
       */
      const slice = async (
        remaining: string[],
        from: string,
        written: string[],
        slices: number,
      ): Promise<RebaseResult> => {
        const budget = request.budget ?? {};
        const now = budget.now ?? Date.now;
        let parent = from;
        let spent = 0;

        while (remaining.length > 0) {
          if (
            (budget.patches !== undefined && spent >= budget.patches) ||
            (budget.deadline !== undefined && now() >= budget.deadline)
          ) {
            const left = [...remaining];
            const soFar = [...written];
            const at = parent;
            return {
              slices,
              continuation: {
                slices,
                resume: () => slice(left, at, soFar, slices + 1),
              },
            };
          }

          const oid = remaining.shift() ?? '';
          spent += 1;
          const { commit } = await git.readCommit({ ...options, oid });
          const base = commit.parent[0];
          if (base === undefined) {
            throw new Error('a rebase cannot replay a commit with no parent');
          }
          withinLimits(await patchSize(options, base, oid), limits);

          // The change this commit made, replayed on what the rebase has
          // built so far: its own parent is the base of the merge between
          // it and the new parent.
          const merged = await mergeOverBase(
            options,
            { base, ours: parent, theirs: oid },
            replayFor(request.repository),
          );
          if (merged.conflicts !== undefined) {
            return { conflicts: merged.conflicts, stoppedAt: oid, commits: written, slices };
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
        return { commits: written, slices };
      };

      return slice([...own], request.onto, [], 1);
    },

    carryOn(continuation): Promise<RebaseResult> {
      return continuation.resume();
    },

    /**
     * One merge commit, with the base first and the branch second, and
     * the three-way merge of the two as its tree. A merge that
     * conflicts writes nothing: no commit, and no ref pointing at one.
     */
    async mergeCommit(request): Promise<CommitResult> {
      const session = await open(repos, request.repository);
      const { fs, gitdir } = session;
      const options = { fs: fs as never, gitdir };
      const base = await sizedMerge(options, session, request);

      const merged = await mergeOverBase(
        options,
        {
          base,
          ours: request.ours,
          theirs: request.theirs,
        },
        replayFor(request.repository),
      );
      if (merged.conflicts !== undefined) {
        return { conflicts: merged.conflicts };
      }
      const tree = merged.tree ?? '';

      // Written here rather than taken from the library's own merge, so
      // the parents are in the order the reference fixes and the author
      // is the caller's rather than the engine's.
      const commit = await git.writeCommit({
        ...options,
        commit: {
          tree,
          parent: [request.ours, request.theirs],
          author: request.author,
          committer: request.author,
          message: request.message ?? 'merge',
        },
      });

      await session.flush([{ name: MERGE_RESULT_REF, oldOid: null, newOid: commit, force: true }]);
      return { commit };
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
