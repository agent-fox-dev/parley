import { refusal } from './checks.js';
import type { LogSink } from '../http/router.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { ZERO_OID, lsRefs, pushPack } from '../git/transport.js';
import { descendsFrom } from '../git/ancestry.js';
import { nonFastForward, pushFailure, refusedRef } from './failures.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * Sending one branch of the hub's copy to origin.
 *
 * It sends exactly `refs/heads/<name>` to `refs/heads/<name>`: this
 * route moves one branch and never guesses at another.
 */
export type PushOutcome = 'pushed' | 'up_to_date' | 'forced';

export interface BranchPushDeps {
  repos: RepositoryStore;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
  /** How long the push may take; two minutes by default. */
  deadlineMs?: number;
}

export const PUSH_DEADLINE_MS = 2 * 60 * 1000;

export interface BranchPushResult {
  branch: string;
  sha: string;
  remote: 'origin';
  remote_url: string;
  result: PushOutcome;
}

/** A remote's address with any username and password taken out of it. */
export function withoutUserinfo(gitUrl: string): string {
  try {
    const parsed = new URL(gitUrl);
    if (parsed.username === '' && parsed.password === '') {
      return gitUrl;
    }
    parsed.username = '';
    parsed.password = '';
    return parsed.toString();
  } catch {
    // An scp-style address is not a URL and comes back as it was.
    return gitUrl;
  }
}

export async function pushBranch(
  deps: BranchPushDeps,
  workspace: WorkspaceRecord,
  request: { name: string; force: boolean },
): Promise<BranchPushResult> {
  const repository = repositoryOf(workspace);
  const ref = `refs/heads/${request.name}`;
  const answered = {
    branch: request.name,
    remote: 'origin' as const,
    remote_url: withoutUserinfo(workspace.git_url),
  };

  const here = (await deps.repos.listRefs(repository)).find((one) => one.name === ref)?.oid;
  if (here === undefined) {
    throw refusal(404, 'branch_not_found', `branch "${request.name}" does not exist`);
  }

  const credential =
    deps.secrets === undefined
      ? undefined
      : await resolveGitCredential(
          deps.secrets,
          workspaceScope(workspace.slug),
          'origin',
          deps.log,
        );
  const context = {
    branch: request.name,
    gitUrl: workspace.git_url,
    ...(credential === undefined ? {} : { credential }),
  };
  const authorization = authorizationFor(credential);
  const remote = {
    url: workspace.git_url.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };

  return within(
    (async (): Promise<BranchPushResult> => {
      let theirs: string | undefined;
      let listed = true;
      try {
        theirs = (await lsRefs(remote)).find((one) => one.name === ref)?.oid;
      } catch (err) {
        // A remote that will not say what it holds may still take the
        // push; only an answer about this workspace's standing is lost.
        if (!(err instanceof Error) || err.name === 'RemoteError') {
          throw pushFailure(err, context);
        }
        listed = false;
      }

      if (theirs === here) {
        return { ...answered, sha: here, result: 'up_to_date' };
      }

      const pack = await deps.repos.servePack(
        repository,
        [here],
        theirs === undefined ? [] : [theirs],
      );

      // Whether this is a fast-forward is answerable from the pack,
      // before origin is asked to take it.
      if (theirs !== undefined && !descendsFrom(pack, here, theirs)) {
        if (!request.force) {
          throw nonFastForward(context, {
            force: false,
            ...(listed ? { origin: theirs } : {}),
            own: workspace.branch === request.name,
          });
        }
      }

      let report;
      try {
        report = await pushPack(remote, {
          commands: [{ name: ref, oldOid: theirs ?? ZERO_OID, newOid: here }],
          pack,
        });
      } catch (err) {
        throw pushFailure(err, context);
      }

      const refused = report.refs.find((one) => !one.ok);
      if (!report.unpacked) {
        throw refusal(502, 'forge_error', 'origin could not unpack what it was sent');
      }
      if (refused !== undefined) {
        throw refusedRef(refused.reason ?? 'origin gave no reason', context, {
          force: request.force,
          ...(theirs === undefined ? {} : { theirs }),
        });
      }

      return {
        ...answered,
        sha: here,
        result: request.force && theirs !== undefined ? 'forced' : 'pushed',
      };
    })(),
    deps.deadlineMs ?? PUSH_DEADLINE_MS,
    context,
  );
}

/** Gives up when the push takes longer than it may. */
async function within<T>(
  work: Promise<T>,
  deadlineMs: number,
  context: { branch: string; gitUrl: string },
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      work,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => {
          reject(
            refusal(504, 'forge_timeout', `the push of ${context.branch} did not finish in time`),
          );
        }, deadlineMs);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}
