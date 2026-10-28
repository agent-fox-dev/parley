import type { LogSink } from '../http/router.js';
import { type Remote, fetchPack, lsRefs } from '../git/transport.js';
import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import type { GitCredential } from './credentials.js';

export interface CopyRequest {
  /** The repository in the store the copy writes into. */
  repository: string;
  /** Where to copy from. */
  gitUrl: string;
  credential?: GitCredential;
  /** The refs earlier units of work already brought across. */
  brought?: readonly string[];
  /** How many refs one unit of work takes on. */
  batchSize?: number;
  fetch?: typeof fetch;
  log?: LogSink;
}

export interface CopyProgress {
  done: boolean;
  /** Every ref brought across so far, for the next unit of work. */
  brought: string[];
  /** Where HEAD points on the remote, once it is known. */
  head?: string;
}

/** How many refs one unit of platform work takes on by default. */
const BATCH = 50;

/** The header git itself would send for a credential of each kind. */
export function authorizationFor(credential: GitCredential | undefined): string | undefined {
  if (credential === undefined) {
    return undefined;
  }
  if (credential.kind === 'token') {
    return `Bearer ${credential.token}`;
  }
  const pair = `${credential.username}:${credential.password}`;
  return `Basic ${btoa(pair)}`;
}

function remoteFor(request: {
  gitUrl: string;
  credential?: GitCredential;
  fetch?: typeof fetch;
}): Remote {
  const authorization = authorizationFor(request.credential);
  return {
    url: request.gitUrl.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(request.fetch === undefined ? {} : { fetch: request.fetch }),
  };
}

/**
 * Whether the remote takes the credential it was given. A refusal is the
 * caller's mistake and stops a create; anything else is the remote having
 * a bad day, which is not a reason to refuse a workspace.
 */
export async function checkRemoteCredential(request: {
  gitUrl: string;
  credential?: GitCredential;
  fetch?: typeof fetch;
}): Promise<'accepted' | 'refused' | 'unreachable'> {
  try {
    await lsRefs(remoteFor(request));
    return 'accepted';
  } catch (err) {
    const message = (err as Error).message;
    return /\b40[13]\b/.test(message) ? 'refused' : 'unreachable';
  }
}

/**
 * Brings one batch of the remote's refs into the workspace's repository.
 *
 * Git cannot resume a pack, so the grain of recovery is the ref: each
 * unit of work asks for the refs the earlier ones did not bring, and the
 * caller carries the list between units. The copy is finished when every
 * ref the remote holds has been brought.
 */
export async function copyIn(repos: RepositoryStore, request: CopyRequest): Promise<CopyProgress> {
  const remote = remoteFor(request);
  const advertised = await lsRefs(remote);

  const head = advertised.find((ref) => ref.name === 'HEAD');
  const refs = advertised.filter((ref) => ref.name !== 'HEAD');
  const brought = new Set(request.brought ?? []);
  const remaining = refs.filter((ref) => !brought.has(ref.name));

  if (remaining.length === 0) {
    return {
      done: true,
      brought: [...brought],
      ...(head?.target === undefined ? {} : { head: head.target }),
    };
  }

  // The default branch first: a workspace with only that is already
  // useful, and a copy that never finishes has still brought the one ref
  // most callers want.
  const defaultFirst = (name: string) => (name === head?.target ? 0 : 1);
  const ordered = remaining.toSorted(
    (left, right) =>
      defaultFirst(left.name) - defaultFirst(right.name) || left.name.localeCompare(right.name),
  );
  const batch = ordered.slice(0, request.batchSize ?? BATCH);

  const pack = await fetchPack(remote, { wants: batch.map((ref) => ref.oid) });
  const changes: RefChange[] = batch.map((ref) => ({
    name: ref.name,
    oldOid: null,
    newOid: ref.oid,
    force: true,
  }));
  await repos.receivePack(request.repository, pack, changes);

  for (const ref of batch) {
    brought.add(ref.name);
  }
  request.log?.info?.('brought a batch of refs across', {
    repository: request.repository,
    refs: batch.length,
    remaining: remaining.length - batch.length,
  });

  return {
    done: remaining.length === batch.length,
    brought: [...brought],
    ...(head?.target === undefined ? {} : { head: head.target }),
  };
}
