import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { descendsFrom } from '../git/ancestry.js';
import { remoteReason } from '../push/failures.js';
import { type Remote, fetchPack, lsRefs } from '../git/transport.js';
import { type WorkspaceRecord, updateWorkspace } from '../store/workspaces.js';

/**
 * Bringing a standard workspace up to date with origin.
 *
 * The outcome is not success or failure but one of six states, and what
 * tells them apart is as much what the workspace records as what the
 * caller is answered: a diverged origin is a recovery instruction, not
 * a fault, and a failed fetch leaves a sync_error the next success
 * clears.
 *
 * A carry_patch workspace fetches upstream and works on its patch list
 * instead. That is the carry-patch package's, and `syncStandard` is
 * where it takes over: the route asks the mode and calls this for a
 * standard workspace alone.
 */
export type SyncResult = 'up_to_date' | 'fast_forward' | 'reset_to_upstream';

export interface SyncDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  clock: Clock;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
  /** How long the whole sync may take; two minutes by default. */
  deadlineMs?: number;
}

export const SYNC_DEADLINE_MS = 2 * 60 * 1000;

const DIVERGED =
  "origin's history has diverged from this workspace's; sync with reset_to_upstream to move the branch to origin's head";

/** What a sync writes about itself when it fails. */
async function recordFailure(
  deps: SyncDeps,
  workspace: WorkspaceRecord,
  why: string,
): Promise<void> {
  await updateWorkspace(
    deps.store,
    workspace.slug,
    { sync_error: why },
    deps.clock.now().toISOString(),
  );
}

/** Which branch this workspace tracks on origin. */
export function trackedBranch(
  workspace: Pick<WorkspaceRecord, 'branch'>,
  refs: ReadonlyArray<{ name: string; target?: string }>,
): string {
  if (workspace.branch !== null && workspace.branch.length > 0) {
    return workspace.branch;
  }
  const head = refs.find((ref) => ref.name === 'HEAD')?.target;
  if (head !== undefined && head.startsWith('refs/heads/')) {
    return head.slice('refs/heads/'.length);
  }
  const branches = refs
    .filter((ref) => ref.name.startsWith('refs/heads/'))
    .map((ref) => ref.name.slice('refs/heads/'.length));
  return branches.find((name) => name === 'main') ?? branches[0] ?? 'main';
}

/** Origin, as something to fetch from, or the refusal to try. */
async function origin(deps: SyncDeps, workspace: WorkspaceRecord): Promise<Remote> {
  if (deps.secrets === undefined) {
    return { url: workspace.git_url.replace(/\/$/, '') };
  }
  const credential = await resolveGitCredential(
    deps.secrets,
    workspaceScope(workspace.slug),
    'origin',
    deps.log,
  );
  const authorization = authorizationFor(credential);
  return {
    url: workspace.git_url.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };
}

/** Gives up on the work when it takes longer than the sync may have. */
async function within<T>(work: Promise<T>, deadlineMs: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      work,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => {
          reject(new HttpError(504, 'the sync was cancelled before it finished'));
        }, deadlineMs);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Syncs a standard workspace, answering which of the three successful
 * outcomes it was. A refusal is an HttpError, and the workspace records
 * why before it is thrown.
 */
export async function syncStandard(
  deps: SyncDeps,
  workspace: WorkspaceRecord,
  options: { resetToUpstream?: boolean } = {},
): Promise<SyncResult> {
  const repository = repositoryOf(workspace);
  const at = deps.clock.now().toISOString();

  return within(
    (async (): Promise<SyncResult> => {
      const remote = await origin(deps, workspace);

      let refs;
      try {
        refs = await lsRefs(remote);
      } catch (err) {
        throw await fetchFailure(deps, workspace, err);
      }

      const branch = trackedBranch(workspace, refs);
      const theirs = refs.find((ref) => ref.name === `refs/heads/${branch}`)?.oid;
      if (theirs === undefined) {
        const why = `origin has no branch named ${branch}`;
        await recordFailure(deps, workspace, why);
        throw new HttpError(502, why);
      }

      const here = (await deps.repos.listRefs(repository)).find(
        (ref) => ref.name === `refs/heads/${branch}`,
      )?.oid;

      if (here === theirs) {
        await settle(deps, workspace, { head: theirs, at });
        return 'up_to_date';
      }

      let pack: Uint8Array;
      try {
        pack = await fetchPack(remote, {
          wants: [theirs],
          ...(here === undefined ? {} : { haves: [here] }),
        });
      } catch (err) {
        throw await fetchFailure(deps, workspace, err);
      }

      const forward = here === undefined || descendsFrom(pack, theirs, here);
      if (!forward && options.resetToUpstream !== true) {
        await recordFailure(deps, workspace, DIVERGED);
        throw new HttpError(409, DIVERGED);
      }

      await deps.repos.receivePack(repository, pack, [
        { name: `refs/heads/${branch}`, oldOid: here ?? null, newOid: theirs, force: true },
      ]);
      await settle(deps, workspace, { head: theirs, at });
      return forward ? 'fast_forward' : 'reset_to_upstream';
    })(),
    deps.deadlineMs ?? SYNC_DEADLINE_MS,
  ).catch(async (err: unknown) => {
    if (err instanceof HttpError && err.status === 504) {
      await recordFailure(deps, workspace, 'the sync was cancelled before it finished');
    }
    throw err;
  });
}

/** The workspace as a sync leaves it when it worked. */
async function settle(
  deps: SyncDeps,
  workspace: WorkspaceRecord,
  outcome: { head: string; at: string },
): Promise<void> {
  await updateWorkspace(
    deps.store,
    workspace.slug,
    {
      head_sha: outcome.head,
      upstream_head_sha: outcome.head,
      last_sync_at: outcome.at,
      sync_error: null,
    },
    outcome.at,
  );
}

/**
 * A fetch that did not happen. The reason is recorded and answered,
 * with nothing of the credential in it: what the remote said about
 * authentication is the remote's business, and the value is never
 * anybody's.
 */
async function fetchFailure(
  deps: SyncDeps,
  workspace: WorkspaceRecord,
  err: unknown,
): Promise<HttpError> {
  // Said in the same vocabulary the branch push uses, which carries
  // nothing of the credential, the address or the transport's words.
  const why = `the fetch from origin failed: ${remoteReason(err)}`;
  await recordFailure(deps, workspace, why);
  return new HttpError(502, why);
}
