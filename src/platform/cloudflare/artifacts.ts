import { ARTIFACTS_CAPABILITIES, requireCapability } from '../../repo/capabilities.js';
import { HttpError } from '../../http/errors.js';
import {
  type RefCommand,
  type Remote,
  ZERO_OID,
  fetchPack,
  lsRefs,
  pushPack,
} from '../../git/transport.js';
import { writePack } from '../local/pack.js';
import type { RefChange, RepositoryStore, StoredObject, StoredRef } from '../repostore.js';

/**
 * What the Artifacts binding offers. Narrower than the platform's own
 * type, so the adapter says plainly what it uses and a test can stand in
 * for it.
 */
export interface ArtifactsBinding {
  create(name: string, opts?: { description?: string }): Promise<{ remote: string; token: string }>;
  get(name: string): Promise<ArtifactsRepoHandle>;
  delete(name: string): Promise<unknown>;
  list(): Promise<{ repos: Array<{ name: string }> }>;
}

export interface ArtifactsRepoHandle {
  info(): Promise<{ remote: string }>;
  createToken(scope?: 'write' | 'read', ttl?: number): Promise<{ plaintext: string }>;
  readBlob(hash: string): Promise<Blob | null>;
  readCommit(hash: string): Promise<unknown>;
  readTree(hash: string): Promise<unknown>;
}

/**
 * The repository store on Cloudflare. The binding gives the repository
 * lifecycle, a repo-scoped token and object reads; it has no ref listing
 * and no write API, so refs and packs go over the git remote with that
 * token (ADR 6).
 */
/**
 * A remote for one repository. The token is minted per call and lives
 * only inside this call: it reaches no response and no log.
 */
async function remoteOf(
  binding: ArtifactsBinding,
  name: string,
  fetching?: typeof fetch,
): Promise<Remote> {
  const repo = await binding.get(name);
  const [info, token] = await Promise.all([repo.info(), repo.createToken('write')]);
  return {
    url: info.remote.replace(/\/$/, ''),
    headers: { authorization: `Bearer ${token.plaintext}` },
    ...(fetching === undefined ? {} : { fetch: fetching }),
  };
}

/** Sends the changes and the pack, and refuses loudly if the store did. */
async function push(
  remote: Remote,
  changes: readonly RefChange[],
  pack: Uint8Array,
): Promise<void> {
  const report = await pushPack(remote, { commands: commandsOf(changes), pack });
  const refused = report.refs.filter((ref) => !ref.ok);
  if (!report.unpacked || refused.length > 0) {
    throw new HttpError(
      409,
      refused.length > 0
        ? `the store refused ${refused.map((r) => `${r.name} (${r.reason ?? 'no reason'})`).join(', ')}`
        : 'the store could not unpack what it was sent',
    );
  }
}

/** The ref commands a set of changes becomes, each checked against what the store can do. */
function commandsOf(changes: readonly RefChange[]): RefCommand[] {
  for (const change of changes) {
    if (!change.name.startsWith('refs/heads/') && !change.name.startsWith('refs/tags/')) {
      requireCapability(ARTIFACTS_CAPABILITIES, 'arbitraryRefNamespaces');
    }
    if (change.newOid === null) {
      requireCapability(ARTIFACTS_CAPABILITIES, 'refDeletion');
    }
    if (change.force === true) {
      requireCapability(ARTIFACTS_CAPABILITIES, 'nonFastForward');
    }
  }
  return changes.map((change) => ({
    name: change.name,
    oldOid: change.oldOid ?? ZERO_OID,
    newOid: change.newOid ?? ZERO_OID,
  }));
}

export function artifactsRepositoryStore(
  binding: ArtifactsBinding,
  options: { fetch?: typeof fetch } = {},
): RepositoryStore {
  return {
    capabilities: ARTIFACTS_CAPABILITIES,

    async create(name: string): Promise<void> {
      try {
        await binding.create(name);
      } catch (err) {
        // Already there is the state the caller wanted.
        if ((err as { code?: string }).code !== 'ALREADY_EXISTS') {
          throw err;
        }
      }
    },

    async exists(name: string): Promise<boolean> {
      try {
        await binding.get(name);
        return true;
      } catch {
        return false;
      }
    },

    async remove(name: string): Promise<void> {
      try {
        await binding.delete(name);
      } catch (err) {
        if ((err as { code?: string }).code !== 'NOT_FOUND') {
          throw err;
        }
      }
    },

    async listRefs(name: string): Promise<StoredRef[]> {
      const refs = await lsRefs(await remoteOf(binding, name, options.fetch));
      return refs
        .filter((ref) => ref.name !== 'HEAD')
        .map((ref) => ({ name: ref.name, oid: ref.oid }))
        .toSorted((left, right) => left.name.localeCompare(right.name));
    },

    async updateRefs(name: string, changes: readonly RefChange[]): Promise<void> {
      // A ref change with nothing new to carry still needs a pack, and an
      // empty one is what git sends.
      await push(await remoteOf(binding, name, options.fetch), changes, writePack([]));
    },

    async receivePack(
      name: string,
      pack: Uint8Array,
      changes: readonly RefChange[],
    ): Promise<void> {
      await push(await remoteOf(binding, name, options.fetch), changes, pack);
    },

    async servePack(
      name: string,
      wants: readonly string[],
      haves?: readonly string[],
    ): Promise<Uint8Array> {
      return fetchPack(await remoteOf(binding, name, options.fetch), {
        wants,
        ...(haves === undefined ? {} : { haves }),
      });
    },

    async readObject(name: string, oid: string): Promise<StoredObject | undefined> {
      const repo = await binding.get(name);
      const blob = await repo.readBlob(oid);
      if (blob === null) {
        return undefined;
      }
      return { type: 'blob', data: new Uint8Array(await blob.arrayBuffer()) };
    },
  };
}
