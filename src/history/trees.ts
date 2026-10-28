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
 * The paths one side moved a file to, found by content: a path the side
 * dropped whose blob turns up at a path it added. That is git's exact
 * rename pass, and it is what lets an edit on one side land on the file
 * the other side renamed.
 */
export function renamesBetween(
  base: ReadonlyMap<string, FlatEntry>,
  side: ReadonlyMap<string, FlatEntry>,
): Map<string, string> {
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
