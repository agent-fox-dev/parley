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
export function localRepositoryStore(
  root: string,
  capabilities: StoreCapabilities = LOCAL_CAPABILITIES,
): RepositoryStore {
  const repoPath = (name: string) => join(root, name);
  const refsPath = (name: string) => join(repoPath(name), 'refs.json');
  const objectsPath = (name: string) => join(repoPath(name), 'objects');

  function present(name: string): void {
    if (!existsSync(repoPath(name))) {
      throw new HttpError(404, `no repository named ${name}`);
    }
  }

  function refsOf(name: string): StoredRefs {
    try {
      return JSON.parse(readFileSync(refsPath(name), 'utf8')) as StoredRefs;
    } catch {
      return {};
    }
  }

  function writeRefs(name: string, refs: StoredRefs): void {
    writeFileSync(refsPath(name), JSON.stringify(refs, null, 2));
  }

  function writeObject(name: string, object: PackedObject): void {
    writeFileSync(
      join(objectsPath(name), object.oid),
      Buffer.concat([Buffer.from(`${object.type}\u0000`, 'latin1'), Buffer.from(object.data)]),
    );
  }

  function readStored(name: string, oid: string): PackedObject | undefined {
    try {
      const raw = readFileSync(join(objectsPath(name), oid));
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
   * Whether a change is a fast-forward, which here means the ref did not
   * exist or still holds what the caller expected. The store cannot walk
   * history without reading every commit, so the expected-value check is
   * what it offers; a caller that means to rewind says force.
   */
  function checkChange(name: string, refs: StoredRefs, change: RefChange): void {
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
    void name;
  }

  return {
    capabilities,

    async create(name: string): Promise<void> {
      mkdirSync(objectsPath(name), { recursive: true });
      if (!existsSync(refsPath(name))) {
        writeRefs(name, {});
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
      const refs = refsOf(name);
      return Promise.resolve(
        Object.keys(refs)
          .toSorted()
          .map((ref) => ({ name: ref, oid: refs[ref] ?? '' })),
      );
    },

    async updateRefs(name: string, changes: readonly RefChange[]): Promise<void> {
      present(name);
      const refs = refsOf(name);
      // Checked first, every one of them, so a change the store refuses
      // leaves the refs exactly as they were.
      for (const change of changes) {
        checkChange(name, refs, change);
      }
      for (const change of changes) {
        if (change.newOid === null) {
          delete refs[change.name];
        } else {
          refs[change.name] = change.newOid;
        }
      }
      writeRefs(name, refs);
      await Promise.resolve();
    },

    async receivePack(
      name: string,
      pack: Uint8Array,
      changes: readonly RefChange[],
    ): Promise<void> {
      present(name);
      const refs = refsOf(name);
      for (const change of changes) {
        checkChange(name, refs, change);
      }
      for (const object of readPack(pack)) {
        writeObject(name, object);
      }
      await this.updateRefs(name, changes);
    },

    async servePack(name: string, wants: readonly string[]): Promise<Uint8Array> {
      present(name);
      // Every object the repository holds. A pack may carry more than was
      // asked for; narrowing it to what is reachable from the wants is
      // object-level work, and belongs to the history engine.
      void wants;
      const objects = readdirSync(objectsPath(name))
        .map((oid) => readStored(name, oid))
        .filter((object): object is PackedObject => object !== undefined);
      return Promise.resolve(writePack(objects));
    },

    async readObject(name: string, oid: string): Promise<StoredObject | undefined> {
      present(name);
      const object = readStored(name, oid);
      return Promise.resolve(
        object === undefined ? undefined : { type: object.type, data: object.data },
      );
    },
  };
}
