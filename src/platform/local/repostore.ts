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

    async servePack(name: string, wants: readonly string[]): Promise<Uint8Array> {
      present(name);
      // Every object the repository holds. A pack may carry more than was
      // asked for; narrowing it to what is reachable from the wants is
      // object-level work, and belongs to the history engine.
      void wants;
      const objects = readdirSync(objectsPath(repoPath(name)))
        .map((oid) => readStored(repoPath(name), oid))
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
