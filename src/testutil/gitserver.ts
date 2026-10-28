import { execFile } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { serve } from '@hono/node-server';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import type { OrgRecord, UserRecord } from '../store/identity.js';
import { type Service, identityService } from './app.js';
import { buildCommit } from './history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from './identity.js';
import type { Decider } from '../git/push.js';
import { hasRealGit } from './realgit.js';

/**
 * The service on a real socket, with a real repository, for suites that
 * drive the real git binary against the real routes. A protocol detail
 * the hub gets wrong is a clone that hangs rather than a field that is
 * missing, which no in-process request would show.
 */

/**
 * git, run without blocking. The service it talks to is in this very
 * process, so a synchronous child would hold the event loop and the
 * server could never answer.
 */
export const gitRun = promisify(execFile);

/** git, with nothing that could stop and wait for a person. */
export const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_TERMINAL_PROMPT: '0',
  GIT_ASKPASS: '/usr/bin/true',
  GIT_CONFIG_NOSYSTEM: '1',
  HOME: '/nonexistent',
};

/** A port nothing is listening on, or nothing where the host refuses. */
export async function freePort(): Promise<number | undefined> {
  return new Promise((resolve) => {
    const probe = createServer();
    probe.on('error', () => {
      resolve(undefined);
    });
    probe.listen(0, '127.0.0.1', () => {
      const address = probe.address();
      const found = typeof address === 'object' && address !== null ? address.port : undefined;
      probe.close(() => {
        resolve(found);
      });
    });
  });
}

/** What a suite driving a real git client needs, and may not have. */
export async function whatIsMissing(): Promise<string[]> {
  return [
    ...(hasRealGit() ? [] : ['a git binary on the path']),
    ...((await freePort()) === undefined ? ['a host that allows binding a local port'] : []),
  ];
}

export interface GitWorld {
  service: Service;
  repos: RepositoryStore;
  repository: string;
  workspace: WorkspaceRecord;
  org?: OrgRecord;
  owner: UserRecord;
  credential: string;
  /** The address the service is served at, which is its external URL. */
  base: string;
  url: string;
  /** How many bytes of response body the service has sent so far. */
  bytesServed: () => number;
  stop: () => Promise<void>;
}

export interface GitWorldOptions {
  withOrg?: boolean;
  externalUrl?: string;
  /** What the service makes of each ref of a git push. */
  decideRef?: Decider;
  /** Reaches a fork of the suite's own instead of the network. */
  fetch?: typeof fetch;
  /** Wraps the store the service is given, to narrow what it will do. */
  narrow?: (store: RepositoryStore) => RepositoryStore;
  /** Set on the workspace before it is served. */
  workspace?: Partial<WorkspaceRecord>;
  cleanups: Array<() => void | Promise<void>>;
}

/** A listening service with one ready workspace holding one commit. */
export async function servedService(options: GitWorldOptions): Promise<GitWorld> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-served-'));
  options.cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const onDisk = localRepositoryStore(dir);
  const repos = options.narrow === undefined ? onDisk : options.narrow(onDisk);

  // The address has to be settled before the service is built, because
  // hub_url is built from it.
  const port = (await freePort()) ?? 0;
  const externalUrl = options.externalUrl ?? `http://127.0.0.1:${String(port)}`;
  const service = await identityService([], {
    repos,
    externalUrl,
    ...(options.decideRef === undefined ? {} : { decideRef: options.decideRef }),
    ...(options.fetch === undefined ? {} : { fetch: options.fetch }),
  });
  options.cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = options.withOrg === false ? undefined : await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    ...(org === undefined ? {} : { org_id: org.id }),
    status: 'ready',
    ...options.workspace,
  });
  const key = await seedApiKey(service.store, owner);

  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid },
  ]);

  // Counted on the way out, so a suite can say what a transfer cost
  // rather than trusting a client's own report of it.
  let bytesServed = 0;
  const counting = async (request: Request, ...rest: unknown[]): Promise<Response> => {
    const response = await (service.app.fetch as (...args: unknown[]) => Promise<Response>)(
      request,
      ...rest,
    );
    bytesServed += (await response.clone().arrayBuffer()).byteLength;
    return response;
  };

  // Awaited, because a clone that starts before the socket is listening
  // fails to connect rather than waiting.
  const listening = await new Promise<ReturnType<typeof serve>>((resolve) => {
    const server = serve(
      { fetch: counting as unknown as typeof service.app.fetch, port, hostname: '127.0.0.1' },
      () => {
        resolve(server);
      },
    );
  });
  const stop = () =>
    new Promise<void>((resolve) => {
      listening.close(() => {
        resolve();
      });
    });
  options.cleanups.push(stop);

  const base = `http://127.0.0.1:${String(port)}`;
  return {
    service,
    repos,
    repository,
    workspace,
    ...(org === undefined ? {} : { org }),
    owner,
    credential: key.presented,
    base,
    url: `${base}/git/${org?.slug ?? ''}/${workspace.slug}.git`,
    bytesServed: () => bytesServed,
    stop,
  };
}

/** The URL a git client is given, with the credential in it. */
export function withCredential(url: string, credential: string): string {
  return url.replace('://', `://parley:${encodeURIComponent(credential)}@`);
}

/** Clones a URL and answers where it landed and what refs came with it. */
export async function gitClone(
  url: string,
  cleanups: Array<() => void | Promise<void>>,
): Promise<{ dir: string; refs: string[] }> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-clone-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  await gitRun('git', ['clone', '--quiet', url, dir], { env: GIT_ENV, timeout: 30_000 });
  return { dir, refs: await gitRefs(dir) };
}

/** The branches and tags a clone holds. */
export async function gitRefs(dir: string): Promise<string[]> {
  const shown = await gitRun('git', ['-C', dir, 'show-ref'], { env: GIT_ENV }).catch(() => ({
    stdout: '',
  }));
  return shown.stdout
    .split('\n')
    .filter(Boolean)
    .map((line) => line.split(' ')[1] ?? '')
    .filter((name) => name.startsWith('refs/heads/') || name.startsWith('refs/tags/'))
    .toSorted();
}
