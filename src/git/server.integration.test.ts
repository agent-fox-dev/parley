import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createServer } from 'node:http';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { serve } from '@hono/node-server';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { repositoryOf } from '../repo/repository.js';
import { identityService } from '../testutil/app.js';
import { buildCommit } from '../testutil/history.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';
import { hasRealGit } from '../testutil/realgit.js';

/**
 * A real git client against the real routes. It needs a listening
 * socket, which some hosts refuse, and a git binary; it says which is
 * missing rather than passing quietly.
 */
async function freePort(): Promise<number | undefined> {
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

const missing = [
  ...(hasRealGit() ? [] : ['a git binary on the path']),
  ...((await freePort()) === undefined ? ['a host that allows binding a local port'] : []),
];

const cleanups: Array<() => void | Promise<void>> = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) {
    await cleanup();
  }
});

function temporary(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

/** The service, listening, with one workspace whose repository holds a commit. */
async function served(options: { withOrg?: boolean; externalUrl?: string } = {}) {
  const repos = localRepositoryStore(temporary('parley-served-'));
  // The address has to be known before the service is built, because
  // hub_url is built from it.
  const port = (await freePort()) ?? 0;
  const externalUrl = options.externalUrl ?? `http://127.0.0.1:${String(port)}`;

  const service = await identityService([], { repos, externalUrl });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = options.withOrg === false ? undefined : await seedOrg(service.store, owner);
  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    ...(org === undefined ? {} : { org_id: org.id }),
    status: 'ready',
  });
  const key = await seedApiKey(service.store, owner);
  const repository = repositoryOf(workspace);
  await repos.create(repository);
  const first = await buildCommit(repos, repository, { 'a.txt': 'hello\n' }, [], 'first');
  await repos.updateRefs(repository, [
    { name: 'refs/heads/main', oldOid: null, newOid: first.oid },
  ]);

  // Awaited, because a clone that starts before the socket is listening
  // fails to connect rather than waiting.
  const listening = await new Promise<ReturnType<typeof serve>>((resolve) => {
    const server = serve({ fetch: service.app.fetch, port, hostname: '127.0.0.1' }, () => {
      resolve(server);
    });
  });
  cleanups.push(
    () =>
      new Promise<void>((resolve) => {
        listening.close(() => {
          resolve();
        });
      }),
  );

  return {
    service,
    repos,
    repository,
    workspace,
    org,
    credential: key.presented,
    base: `http://127.0.0.1:${String(port)}`,
  };
}

/** The URL a git client is given, with the credential in it. */
function withCredential(url: string, credential: string): string {
  return url.replace('://', `://parley:${encodeURIComponent(credential)}@`);
}

/** git, with nothing that could stop and wait for a person. */
const GIT_ENV = {
  ...process.env,
  GIT_TERMINAL_PROMPT: '0',
  GIT_ASKPASS: '/usr/bin/true',
  GIT_CONFIG_NOSYSTEM: '1',
  HOME: '/nonexistent',
};

/**
 * git, run without blocking: the service it talks to is in this very
 * process, so a synchronous child would hold the event loop and the
 * server could never answer.
 */
const run = promisify(execFile);

async function clone(url: string): Promise<{ dir: string; refs: string[] }> {
  const dir = temporary('parley-clone-');
  await run('git', ['clone', '--quiet', url, dir], { env: GIT_ENV, timeout: 30_000 });
  const shown = await run('git', ['-C', dir, 'show-ref'], { env: GIT_ENV });
  const refs = shown.stdout
    .split('\n')
    .filter(Boolean)
    .map((line) => line.split(' ')[1] ?? '')
    .filter((name) => name.startsWith('refs/heads/') || name.startsWith('refs/tags/'));
  return { dir, refs };
}

describe.skipIf(missing.length > 0)(
  `a real git client against the git routes${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('clones the repository from the organization and workspace path', async () => {
      const world = await served();
      const url = `${world.base}/git/${world.org?.slug ?? ''}/${world.workspace.slug}.git`;

      const cloned = await clone(withCredential(url, world.credential));

      const stored = (await world.repos.listRefs(world.repository))
        .map((ref) => ref.name)
        .filter((name) => name.startsWith('refs/heads/') || name.startsWith('refs/tags/'));
      expect(cloned.refs.toSorted()).toEqual(stored.toSorted());
    });

    it('clones the hub_url a workspace reports, which is the address served', async () => {
      const world = await served();
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });
      const body = (await read.json()) as { hub_url: string | null };

      expect(body.hub_url).toBe(
        `${world.base}/git/${world.org?.slug ?? ''}/${world.workspace.slug}.git`,
      );
      expect((await clone(withCredential(body.hub_url ?? '', world.credential))).refs).toEqual([
        'refs/heads/main',
      ]);
    });

    it('reports no hub_url for a workspace with no organization, and serves it nowhere', async () => {
      const world = await served({ withOrg: false });
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });

      expect(((await read.json()) as { hub_url: string | null }).hub_url).toBeNull();
      await expect(clone(`${world.base}/git//${world.workspace.slug}.git`)).rejects.toThrow();
    });

    it('reports no hub_url where the deployment has no external URL', async () => {
      const world = await served({ externalUrl: '' });
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });

      expect(((await read.json()) as { hub_url: string | null }).hub_url).toBeNull();
    });
  },
);
