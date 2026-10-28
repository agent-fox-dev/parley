import type { EventRecorder } from '../audit/recorder.js';
import type { Clock } from '../platform/clock.js';
import type { JobQueue } from '../platform/jobs.js';
import type { RelationalStore } from '../platform/index.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import type { LogSink } from '../http/router.js';
import { findWorkspace, updateWorkspace } from '../store/workspaces.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { copyIn } from '../repo/copy_in.js';
import { reserveRepositoryBytes } from '../repo/limits.js';
import { repositoryOf } from '../repo/repository.js';

export const COPY_IN = 'copy-in';

/**
 * What to record about a failure. A credential cannot reach a workspace
 * field a client reads, so the reason is the failure's own words with
 * anything credential-shaped taken out.
 */
function reasonOf(err: unknown): string {
  const message = err instanceof Error ? err.message : String(err);
  return message.replace(/\b(?:Bearer|Basic)\s+\S+/g, '[redacted]').slice(0, 500);
}

export interface CopyInPayload {
  slug: string;
  /** The refs earlier units of work already brought across. */
  brought?: string[];
  [key: string]: unknown;
}

export interface CopyInDeps {
  store: RelationalStore;
  repos: RepositoryStore;
  secrets: SecretStore;
  jobs: JobQueue;
  clock: Clock;
  fetch?: typeof fetch;
  log?: LogSink;
  /** Carried through to the jobs this deployment drains. */
  events?: EventRecorder;
}

/** Queues the copy of a workspace's repository, to run outside the request. */
export async function queueCopyIn(jobs: JobQueue, slug: string): Promise<string> {
  return jobs.enqueue(COPY_IN, { slug } satisfies CopyInPayload);
}

/**
 * One unit of a copy. It brings a batch of refs across and either asks
 * to be run again with what it has done or reports that it is finished.
 */
export async function runCopyIn(
  deps: CopyInDeps,
  payload: CopyInPayload,
): Promise<'done' | 'more'> {
  const workspace = await findWorkspace(deps.store, payload.slug);
  if (workspace === undefined) {
    // The workspace was deleted while its copy was queued.
    return 'done';
  }

  const at = deps.clock.now().toISOString();
  if (workspace.status === 'pending') {
    await updateWorkspace(deps.store, workspace.slug, { status: 'cloning' }, at);
  }

  const credential = await resolveGitCredential(
    deps.secrets,
    workspaceScope(workspace.slug),
    'origin',
    deps.log,
  );

  let progress;
  try {
    progress = await copyIn(deps.repos, {
      repository: repositoryOf(workspace),
      gitUrl: workspace.git_url,
      ...(credential === undefined ? {} : { credential }),
      ...(payload.brought === undefined ? {} : { brought: payload.brought }),
      ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
      ...(deps.log === undefined ? {} : { log: deps.log }),
      account: async (bytes: number) => {
        await reserveRepositoryBytes(
          deps.store,
          repositoryOf(workspace),
          bytes,
          deps.clock.now().toISOString(),
        );
      },
    });
  } catch (err) {
    // A copy that fails after the workspace exists does not take the
    // workspace with it: it stays readable, and says why.
    await updateWorkspace(
      deps.store,
      workspace.slug,
      { status: 'failed', clone_error: reasonOf(err) },
      deps.clock.now().toISOString(),
    );
    return 'done';
  }

  if (!progress.done) {
    payload.brought = progress.brought;
    return 'more';
  }

  await updateWorkspace(
    deps.store,
    workspace.slug,
    {
      status: 'ready',
      ...(progress.headOid === undefined ? {} : { head_sha: progress.headOid }),
      clone_error: null,
    },
    deps.clock.now().toISOString(),
  );
  return 'done';
}

/** How long a copy may stay in cloning before it is given up on. */
export const CLONING_DEADLINE_MS = 30 * 60 * 1000;

/**
 * Gives up on copies that stopped without saying so. A workspace left in
 * cloning is one nobody can archive or reclone, so after the deadline it
 * is moved to failed with the reason, and becomes usable again.
 */
export async function expireStalledClones(
  store: RelationalStore,
  clock: Clock,
  deadlineMs: number = CLONING_DEADLINE_MS,
): Promise<number> {
  const cutoff = new Date(clock.now().getTime() - deadlineMs).toISOString();
  const stalled = await store.all<{ slug: string }>(
    "SELECT slug FROM workspaces WHERE status = 'cloning' AND updated_at <= ?",
    [cutoff],
  );

  for (const row of stalled) {
    await updateWorkspace(
      store,
      row.slug,
      {
        status: 'failed',
        clone_error: `the copy timed out after ${String(Math.round(deadlineMs / 60000))} minutes`,
      },
      clock.now().toISOString(),
    );
  }
  return stalled.length;
}
