import git from 'isomorphic-git';

export interface FlatEntry {
  oid: string;
  mode: string;
}

/** Every blob a tree holds, by its full path. */
export async function flatten(
  options: { fs: never; gitdir: string },
  treeOid: string,
): Promise<Map<string, FlatEntry>> {
  const entries = new Map<string, FlatEntry>();

  await git.walk({
    ...options,
    trees: [git.TREE({ ref: treeOid })],
    map: async (filepath, [entry]) => {
      if (filepath === '.' || entry === null || entry === undefined) {
        return undefined;
      }
      if ((await entry.type()) !== 'blob') {
        return undefined;
      }
      entries.set(filepath, {
        oid: (await entry.oid()) ?? '',
        mode: (await entry.mode()).toString(8),
      });
      return undefined;
    },
  });

  return entries;
}

/**
 * Writes a tree from a flat map of paths, building each directory on
 * the way: the library writes one level at a time.
 */
export async function buildTree(
  options: { fs: never; gitdir: string },
  entries: ReadonlyMap<string, FlatEntry>,
): Promise<string> {
  const here = new Map<string, FlatEntry>();
  const below = new Map<string, Map<string, FlatEntry>>();

  for (const [path, entry] of entries) {
    const slash = path.indexOf('/');
    if (slash === -1) {
      here.set(path, entry);
      continue;
    }
    const directory = path.slice(0, slash);
    const rest = path.slice(slash + 1);
    const held = below.get(directory) ?? new Map<string, FlatEntry>();
    held.set(rest, entry);
    below.set(directory, held);
  }

  const tree = [
    ...[...here].map(([path, entry]) => ({
      mode: entry.mode,
      path,
      oid: entry.oid,
      type: 'blob' as const,
    })),
    ...(await Promise.all(
      [...below].map(async ([path, held]) => ({
        mode: '040000',
        path,
        oid: await buildTree(options, held),
        type: 'tree' as const,
      })),
    )),
  ].toSorted((left, right) => (left.path < right.path ? -1 : 1));

  return git.writeTree({ ...options, tree });
}

/**
 * How alike two blobs are, between 0 and 1: the bytes they hold in
 * common, as a share of the larger of the two. Content is counted a
 * line at a time, which is the granularity git's own rename detection
 * works at; a file with no newlines is one line and so matches only
 * itself.
 */
function similarity(left: Uint8Array, right: Uint8Array): number {
  const largest = Math.max(left.byteLength, right.byteLength);
  if (largest === 0) {
    return 1;
  }

  const counts = new Map<string, number>();
  for (const line of lines(left)) {
    counts.set(line, (counts.get(line) ?? 0) + 1);
  }

  let common = 0;
  for (const line of lines(right)) {
    const held = counts.get(line) ?? 0;
    if (held > 0) {
      counts.set(line, held - 1);
      common += line.length + 1;
    }
  }
  return Math.min(common, largest) / largest;
}

/** A blob's lines, as strings that compare byte for byte. */
function lines(content: Uint8Array): string[] {
  return Buffer.from(content).toString('latin1').split('\n');
}

/** How alike two files must be to count as one moved. git's own default. */
const RENAME_THRESHOLD = 0.5;

/**
 * The most pairs the similarity pass will score. Past it the work grows
 * faster than the answer is worth, and git gives up on rename detection
 * at a limit of its own for the same reason.
 */
const RENAME_LIMIT = 1000;

/**
 * The paths one side moved a file to. A path the side dropped whose
 * blob turns up at a path it added is a move, and so is one whose
 * content is mostly there: git pairs by similarity, not by identity, so
 * a file moved and edited in the same commit is still a move, and the
 * other side's edit has to land on it.
 *
 * Identical content is matched first, because that pairing is certain
 * and it keeps the scoring pass small.
 */
export async function renamesBetween(
  options: { fs: never; gitdir: string },
  base: ReadonlyMap<string, FlatEntry>,
  side: ReadonlyMap<string, FlatEntry>,
): Promise<Map<string, string>> {
  const dropped = [...base].filter(([path]) => !side.has(path));
  const added = [...side].filter(([path]) => !base.has(path));
  const renames = new Map<string, string>();
  const taken = new Set<string>();

  for (const [from, entry] of dropped) {
    const match = added.find(
      ([path, candidate]) => !taken.has(path) && candidate.oid === entry.oid,
    );
    if (match !== undefined) {
      renames.set(from, match[0]);
      taken.add(match[0]);
    }
  }

  const unmatched = dropped.filter(([path]) => !renames.has(path));
  const free = added.filter(([path]) => !taken.has(path));
  if (unmatched.length * free.length > RENAME_LIMIT) {
    return renames;
  }

  const content = new Map<string, Uint8Array>();
  const blobOf = async (oid: string) => {
    const held = content.get(oid);
    if (held !== undefined) {
      return held;
    }
    const { blob } = await git.readBlob({ ...options, oid });
    content.set(oid, blob);
    return blob;
  };

  // Every pair that could be a move, best first, so the strongest
  // pairing wins a path that two of them want.
  const scored: Array<{ from: string; to: string; score: number }> = [];
  for (const [from, entry] of unmatched) {
    for (const [to, candidate] of free) {
      const score = similarity(await blobOf(entry.oid), await blobOf(candidate.oid));
      if (score >= RENAME_THRESHOLD) {
        scored.push({ from, to, score });
      }
    }
  }

  for (const pair of scored.toSorted((left, right) => right.score - left.score)) {
    if (renames.has(pair.from) || taken.has(pair.to)) {
      continue;
    }
    renames.set(pair.from, pair.to);
    taken.add(pair.to);
  }
  return renames;
}

/** The same entries with one path moved to another. */
export function applyRename(
  entries: ReadonlyMap<string, FlatEntry>,
  from: string,
  to: string,
): Map<string, FlatEntry> {
  const moved = new Map(entries);
  const entry = moved.get(from);
  if (entry === undefined) {
    return moved;
  }
  moved.delete(from);
  moved.set(to, entry);
  return moved;
}
