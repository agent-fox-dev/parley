import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { repositoryOf } from '../repo/repository.js';
import { ZERO_OID, lsRefs, pushPack } from '../git/transport.js';
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

  const here = (await deps.repos.listRefs(repository)).find((one) => one.name === ref)?.oid;
  if (here === undefined) {
    throw new HttpError(
      404,
      `branch_not_found: branch "${request.name}" does not exist`,
      'branch_not_found',
    );
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
  const authorization = authorizationFor(credential);
  const remote = {
    url: workspace.git_url.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };

  const theirs = (await lsRefs(remote)).find((one) => one.name === ref)?.oid;
  if (theirs === here) {
    return {
      branch: request.name,
      sha: here,
      remote: 'origin',
      remote_url: withoutUserinfo(workspace.git_url),
      result: 'up_to_date',
    };
  }

  const pack = await deps.repos.servePack(repository, [here], theirs === undefined ? [] : [theirs]);
  const report = await pushPack(remote, {
    commands: [{ name: ref, oldOid: theirs ?? ZERO_OID, newOid: here }],
    pack,
  });

  const refused = report.refs.find((one) => !one.ok);
  if (!report.unpacked || refused !== undefined) {
    throw new HttpError(
      422,
      `forge_rejected: origin refused ${request.name}: ${refused?.reason ?? 'the remote could not unpack what it was sent'}`,
      'forge_rejected',
    );
  }

  return {
    branch: request.name,
    sha: here,
    remote: 'origin',
    remote_url: withoutUserinfo(workspace.git_url),
    result: request.force && theirs !== undefined ? 'forced' : 'pushed',
  };
}
