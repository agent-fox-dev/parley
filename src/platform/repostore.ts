import type { StoreCapabilities } from '../repo/capabilities.js';

export interface StoredRef {
  name: string;
  oid: string;
}

export interface RefChange {
  name: string;
  /** What the ref is expected to hold now; null creates it. */
  oldOid: string | null;
  /** What it should hold; null deletes it. */
  newOid: string | null;
  /** Allows an update that is not a fast-forward. */
  force?: boolean;
}

export interface StoredObject {
  type: 'commit' | 'tree' | 'blob' | 'tag';
  data: Uint8Array;
}

/**
 * Where a workspace's repository lives. The vocabulary is refs and packs
 * rather than the shape of any one store's SDK, because the store Parley
 * runs on offers no ref listing and no write API: everything but object
 * reads goes over the git protocol (see ADR 2 and ADR 6).
 */
export interface RepositoryStore {
  /** What this store will and will not do with refs. */
  readonly capabilities: StoreCapabilities;

  /** Brings a repository into being; doing it twice is not an error. */
  create(name: string): Promise<void>;
  exists(name: string): Promise<boolean>;
  remove(name: string): Promise<void>;

  listRefs(name: string): Promise<StoredRef[]>;
  /**
   * Applies every change or none. A change the store cannot make — a
   * namespace it refuses, a rewind it will not take, a deletion it does
   * not support — fails naming the capability and leaves the refs alone.
   */
  updateRefs(name: string, changes: readonly RefChange[]): Promise<void>;

  /** Takes a pack into the repository, with the refs it makes reachable. */
  receivePack(name: string, pack: Uint8Array, changes: readonly RefChange[]): Promise<void>;
  /** A pack holding what the caller wants and does not have. */
  servePack(name: string, wants: readonly string[], haves?: readonly string[]): Promise<Uint8Array>;

  readObject(name: string, oid: string): Promise<StoredObject | undefined>;
}
