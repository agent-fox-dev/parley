import type { LogSink } from '../http/router.js';
import { ZERO_OID, lsRefs, pushPack } from '../git/transport.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { GitCredential } from './credentials.js';
import { authorizationFor } from './copy_in.js';

export interface PushOutRequest {
  repository: string;
  gitUrl: string;
  credential?: GitCredential;
  fetch?: typeof fetch;
  log?: LogSink;
}

/**
 * Pushes a workspace's branches out to origin so nothing exists only
 * here, and says whether the repository may now be let go.
 *
 * The push does not force. Pushing a workspace out is no licence to
 * rewrite history on somebody else's forge, so a branch origin refuses
 * keeps the whole repository instead — which loses nothing, because the
 * repository is still there to reactivate from.
 */
export async function pushOutOnArchive(
  repos: RepositoryStore,
  request: PushOutRequest,
): Promise<'released' | 'kept'> {
  if (request.credential === undefined) {
    request.log?.info?.('kept a repository because the workspace has no usable credential', {
      repository: request.repository,
    });
    return 'kept';
  }

  const branches = (await repos.listRefs(request.repository)).filter((ref) =>
    ref.name.startsWith('refs/heads/'),
  );
  if (branches.length === 0) {
    return 'released';
  }

  const authorization = authorizationFor(request.credential);
  const remote = {
    url: request.gitUrl.replace(/\/$/, ''),
    ...(authorization === undefined ? {} : { headers: { authorization } }),
    ...(request.fetch === undefined ? {} : { fetch: request.fetch }),
  };

  try {
    const onOrigin = new Map((await lsRefs(remote)).map((ref) => [ref.name, ref.oid]));
    const commands = branches
      .filter((branch) => onOrigin.get(branch.name) !== branch.oid)
      .map((branch) => ({
        name: branch.name,
        oldOid: onOrigin.get(branch.name) ?? ZERO_OID,
        newOid: branch.oid,
      }));

    if (commands.length > 0) {
      const pack = await repos.servePack(
        request.repository,
        commands.map((one) => one.newOid),
      );
      const report = await pushPack(remote, { commands, pack });
      const refused = report.refs.filter((ref) => !ref.ok);
      if (!report.unpacked || refused.length > 0) {
        request.log?.info?.('kept a repository because origin refused a branch', {
          repository: request.repository,
          refused: refused.map((ref) => ref.name),
        });
        return 'kept';
      }
    }
  } catch (err) {
    request.log?.info?.('kept a repository because the push out failed', {
      repository: request.repository,
      reason: (err as Error).message.slice(0, 200),
    });
    return 'kept';
  }

  await repos.remove(request.repository);
  return 'released';
}

/** Takes a workspace's repository away with the workspace itself. */
export async function removeRepository(repos: RepositoryStore, repository: string): Promise<void> {
  await repos.remove(repository);
}
