import git from 'isomorphic-git';
import type { RepositoryStore } from '../platform/repostore.js';

/**
 * The filesystem isomorphic-git asks for. Only the calls it makes are
 * implemented, and each is promise-shaped as its promise client expects.
 */
export interface FsLike {
  promises: {
    readFile(path: string, options?: unknown): Promise<Uint8Array | string>;
    writeFile(path: string, data: Uint8Array | string, options?: unknown): Promise<void>;
    unlink(path: string): Promise<void>;
    readdir(path: string): Promise<string[]>;
    mkdir(path: string): Promise<void>;
    rmdir(path: string): Promise<void>;
    stat(path: string): Promise<FileStat>;
    lstat(path: string): Promise<FileStat>;
    readlink(path: string): Promise<string>;
    symlink(target: string, path: string): Promise<void>;
  };
}

export interface FileStat {
  type: 'file' | 'dir';
  mode: number;
  size: number;
  ino: number;
  mtimeMs: number;
  ctimeMs: number;
  uid: number;
  gid: number;
  dev: number;
  isFile(): boolean;
  isDirectory(): boolean;
  isSymbolicLink(): boolean;
}

function statOf(type: 'file' | 'dir', size: number): FileStat {
  return {
    type,
    mode: type === 'dir' ? 0o040_000 : 0o100_644,
    size,
    ino: 0,
    mtimeMs: 0,
    ctimeMs: 0,
    uid: 0,
    gid: 0,
    dev: 0,
    isFile: () => type === 'file',
    isDirectory: () => type === 'dir',
    isSymbolicLink: () => false,
  };
}

/** An error shaped as node's fs errors are, which the library checks for. */
function notFound(path: string): Error & { code: string } {
  return Object.assign(new Error(`ENOENT: no such file or directory, '${path}'`), {
    code: 'ENOENT',
  });
}

const encoder = new TextEncoder();

/** Deflates with what both Node and a Worker have, rather than a bundled zlib. */
async function deflate(bytes: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([bytes as unknown as ArrayBuffer])
    .stream()
    .pipeThrough(new CompressionStream('deflate'));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

/** The bytes a loose object file holds: the header, the content, deflated. */
export async function looseObject(type: string, data: Uint8Array): Promise<Uint8Array> {
  const header = encoder.encode(`${type} ${String(data.length)}\u0000`);
  const whole = new Uint8Array(header.length + data.length);
  whole.set(header, 0);
  whole.set(data, header.length);
  return deflate(whole);
}

/**
 * Brings a repository's objects into a filesystem as one pack, which is
 * how git itself reads them. A store that cannot hand back an object's
 * raw bytes one at a time can still serve a pack.
 *
 * It answers how many bytes that pack held, which is what an operation
 * measures a repository's size as.
 */
export async function hydrate(
  repos: RepositoryStore,
  repository: string,
  backed: StoreBackedFs,
): Promise<number> {
  const refs = await repos.listRefs(repository);
  if (refs.length === 0) {
    return 0;
  }
  const pack = await repos.servePack(
    repository,
    refs.map((ref) => ref.oid),
  );
  const filepath = 'objects/pack/pack-parley.pack';
  await backed.fs.promises.mkdir(`${backed.gitdir}/objects/pack`);
  await backed.fs.promises.writeFile(`${backed.gitdir}/${filepath}`, pack);
  await git.indexPack({
    fs: backed.fs as never,
    dir: backed.gitdir,
    gitdir: backed.gitdir,
    filepath,
  });
  return pack.byteLength;
}

export interface StoreBackedFs {
  fs: FsLike;
  gitdir: string;
  /** Every path written since the adapter was opened. */
  written: () => Map<string, Uint8Array>;
}

const GITDIR = '/repo.git';

/**
 * The filesystem an operation works in. It holds everything in memory:
 * the repository's objects arrive as one pack through `hydrate`, and
 * what the operation writes is collected on the way out.
 */
export function storeBackedFs(): StoreBackedFs {
  const files = new Map<string, Uint8Array>();
  const directories = new Set<string>([GITDIR, `${GITDIR}/objects`, `${GITDIR}/refs`]);

  async function load(path: string): Promise<Uint8Array | undefined> {
    return Promise.resolve(files.get(path));
  }

  const promises: FsLike['promises'] = {
    readFile: async (path: string, options?: unknown) => {
      const bytes = await load(path);
      if (bytes === undefined) {
        throw notFound(path);
      }
      const encoding =
        typeof options === 'string'
          ? options
          : (options as { encoding?: string } | undefined)?.encoding;
      return encoding === 'utf8' ? new TextDecoder().decode(bytes) : bytes;
    },

    writeFile: async (path: string, data: Uint8Array | string) => {
      files.set(path, typeof data === 'string' ? encoder.encode(data) : data);
      await Promise.resolve();
    },

    unlink: async (path: string) => {
      files.delete(path);
      await Promise.resolve();
    },

    readdir: async (path: string) => {
      const prefix = `${path}/`;
      const names = new Set<string>();
      for (const held of [...files.keys(), ...directories]) {
        if (held.startsWith(prefix)) {
          names.add(held.slice(prefix.length).split('/')[0] ?? '');
        }
      }
      if (names.size === 0 && !directories.has(path)) {
        throw notFound(path);
      }
      return Promise.resolve([...names].filter((name) => name.length > 0).toSorted());
    },

    mkdir: async (path: string) => {
      directories.add(path);
      await Promise.resolve();
    },

    rmdir: async (path: string) => {
      directories.delete(path);
      await Promise.resolve();
    },

    stat: async (path: string) => {
      const bytes = await load(path);
      if (bytes !== undefined) {
        return statOf('file', bytes.length);
      }
      if (directories.has(path) || [...files.keys()].some((held) => held.startsWith(`${path}/`))) {
        return statOf('dir', 0);
      }
      throw notFound(path);
    },

    lstat: async (path: string) => promises.stat(path),

    readlink: async (path: string) => {
      await Promise.resolve();
      throw notFound(path);
    },

    symlink: async () => {
      await Promise.resolve();
      throw new Error('this filesystem holds no symbolic links');
    },
  };

  return { fs: { promises }, gitdir: GITDIR, written: () => files };
}
