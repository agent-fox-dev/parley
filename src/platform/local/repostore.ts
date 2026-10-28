import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { HttpError } from '../../http/errors.js';
import { type StoreCapabilities, requireCapability } from '../../repo/capabilities.js';
import type { RefChange, RepositoryStore, StoredObject, StoredRef } from '../repostore.js';
import { type ObjectType, type PackedObject, readPack, writePack } from './pack.js';

/**
 * Everything this store will do with refs. It keeps its own repository,
 * so it is bound by nothing but its own code.
 */
export const LOCAL_CAPABILITIES: StoreCapabilities = {
  arbitraryRefNamespaces: true,
  nonFastForward: true,
  refDeletion: true,
};

interface StoredRefs {
  [name: string]: string;
}

/**
 * A bare repository on the host's own disk: refs in one file, objects
 * loose, one directory per repository. It speaks the same refs-and-packs
 * vocabulary as the Cloudflare store so that one contract suite holds
 * them both.
 */
const refsPath = (repo: string) => join(repo, 'refs.json');
const objectsPath = (repo: string) => join(repo, 'objects');

function refsOf(repo: string): StoredRefs {
  try {
    return JSON.parse(readFileSync(refsPath(repo), 'utf8')) as StoredRefs;
  } catch {
    return {};
  }
}

function writeRefs(repo: string, refs: StoredRefs): void {
  writeFileSync(refsPath(repo), JSON.stringify(refs, null, 2));
}

function writeObject(repo: string, object: PackedObject): void {
  writeFileSync(
    join(objectsPath(repo), object.oid),
    Buffer.concat([Buffer.from(`${object.type}\u0000`, 'latin1'), Buffer.from(object.data)]),
  );
}

/**
 * Every object reachable from a set of starting points: the commits
 * behind them, the trees those commits hold and the blobs in the trees.
 * An object the repository does not hold ends that line of the walk,
 * because a client may well name a commit this store never saw.
 */
function reachableFrom(
  roots: readonly string[],
  held: (oid: string) => PackedObject | undefined,
): Set<string> {
  const found = new Set<string>();
  const pending = [...roots];

  while (pending.length > 0) {
    const oid = pending.pop() ?? '';
    if (found.has(oid)) {
      continue;
    }
    const object = held(oid);
    if (object === undefined) {
      continue;
    }
    found.add(oid);
    pending.push(...referencedBy(object));
  }
  return found;
}

/** The object ids one object names: a commit's, a tree's or a tag's. */
function referencedBy(object: PackedObject): string[] {
  if (object.type === 'blob') {
    return [];
  }
  if (object.type === 'tree') {
    return treeEntries(object.data);
  }

  // A commit's and a tag's headers end at the first blank line.
  const text: string = Buffer.from(object.data).toString('latin1');
  const blank = text.indexOf('\n\n');
  const header: string = blank === -1 ? text : text.slice(0, blank);
  return header
    .split('\n')
    .map((line) => /^(?:tree|parent|object) ([0-9a-f]{40})$/.exec(line)?.[1])
    .filter((oid): oid is string => oid !== undefined);
}

/** The object ids a tree holds, read from its binary entries. */
function treeEntries(data: Uint8Array): string[] {
  const oids: string[] = [];
  let at = 0;
  while (at < data.byteLength) {
    const nul = data.indexOf(0, at);
    if (nul === -1 || nul + 21 > data.byteLength) {
      break;
    }
    oids.push(Buffer.from(data.subarray(nul + 1, nul + 21)).toString('hex'));
    at = nul + 21;
  }
  return oids;
}

function readStored(repo: string, oid: string): PackedObject | undefined {
  try {
    const raw = readFileSync(join(objectsPath(repo), oid));
    const split = raw.indexOf(0);
    return {
      oid,
      type: raw.subarray(0, split).toString('latin1') as ObjectType,
      data: new Uint8Array(raw.subarray(split + 1)),
    };
  } catch {
    return undefined;
  }
}

/**
 * Whether the store will make this change. A ref that did not exist or
 * still holds what the caller expected moves freely; anything else is a
 * rewind, which needs force and the capability behind it.
 */
function checkChange(capabilities: StoreCapabilities, refs: StoredRefs, change: RefChange): void {
  if (!change.name.startsWith('refs/heads/') && !change.name.startsWith('refs/tags/')) {
    requireCapability(capabilities, 'arbitraryRefNamespaces');
  }
  if (change.newOid === null) {
    requireCapability(capabilities, 'refDeletion');
  }

  const held = refs[change.name];
  if (change.oldOid !== null && held !== change.oldOid) {
    if (change.force !== true) {
      throw new HttpError(409, `${change.name} does not hold ${change.oldOid}`);
    }
    requireCapability(capabilities, 'nonFastForward');
  }
  if (change.oldOid === null && held !== undefined && change.force !== true) {
    throw new HttpError(409, `${change.name} already exists`);
  }
}

export function localRepositoryStore(
  root: string,
  capabilities: StoreCapabilities = LOCAL_CAPABILITIES,
): RepositoryStore {
  const repoPath = (name: string) => join(root, name);

  function present(name: string): void {
    if (!existsSync(repoPath(name))) {
      throw new HttpError(404, `no repository named ${name}`);
    }
  }

  return {
    capabilities,

    async create(name: string): Promise<void> {
      mkdirSync(objectsPath(repoPath(name)), { recursive: true });
      if (!existsSync(refsPath(repoPath(name)))) {
        writeRefs(repoPath(name), {});
      }
      await Promise.resolve();
    },

    async exists(name: string): Promise<boolean> {
      return Promise.resolve(existsSync(repoPath(name)));
    },

    async remove(name: string): Promise<void> {
      rmSync(repoPath(name), { recursive: true, force: true });
      await Promise.resolve();
    },

    async listRefs(name: string): Promise<StoredRef[]> {
      present(name);
      const refs = refsOf(repoPath(name));
      return Promise.resolve(
        Object.keys(refs)
          .toSorted()
          .map((ref) => ({ name: ref, oid: refs[ref] ?? '' })),
      );
    },

    async updateRefs(name: string, changes: readonly RefChange[]): Promise<void> {
      present(name);
      const refs = refsOf(repoPath(name));
      // Checked first, every one of them, so a change the store refuses
      // leaves the refs exactly as they were.
      for (const change of changes) {
        checkChange(capabilities, refs, change);
      }
      for (const change of changes) {
        if (change.newOid === null) {
          delete refs[change.name];
        } else {
          refs[change.name] = change.newOid;
        }
      }
      writeRefs(repoPath(name), refs);
      await Promise.resolve();
    },

    async receivePack(
      name: string,
      pack: Uint8Array,
      changes: readonly RefChange[],
    ): Promise<void> {
      present(name);
      const refs = refsOf(repoPath(name));
      for (const change of changes) {
        checkChange(capabilities, refs, change);
      }
      for (const object of readPack(pack)) {
        writeObject(repoPath(name), object);
      }
      await this.updateRefs(name, changes);
    },

    async servePack(
      name: string,
      wants: readonly string[],
      haves: readonly string[] = [],
    ): Promise<Uint8Array> {
      present(name);
      const held = (oid: string) => readStored(repoPath(name), oid);

      // With nothing asked for, everything: that is how a repository is
      // read in whole, and what hydration wants.
      if (wants.length === 0) {
        const objects = readdirSync(objectsPath(repoPath(name)))
          .map((oid) => held(oid))
          .filter((object): object is PackedObject => object !== undefined);
        return Promise.resolve(writePack(objects));
      }

      // What the client already has is not sent again, which is the
      // whole point of the negotiation it just made.
      const known = reachableFrom(haves, held);
      const sending = [...reachableFrom(wants, held)].filter((oid) => !known.has(oid));
      const objects = sending
        .map((oid) => held(oid))
        .filter((object): object is PackedObject => object !== undefined);
      return Promise.resolve(writePack(objects));
    },

    async readObject(name: string, oid: string): Promise<StoredObject | undefined> {
      present(name);
      const object = readStored(repoPath(name), oid);
      return Promise.resolve(
        object === undefined ? undefined : { type: object.type, data: object.data },
      );
    },
  };
}
