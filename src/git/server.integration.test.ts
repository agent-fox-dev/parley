import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { buildCommit } from '../testutil/history.js';
import {
  GIT_ENV,
  gitClone,
  gitRefs,
  gitRun,
  servedService,
  whatIsMissing,
  withCredential,
} from '../testutil/gitserver.js';

/**
 * A real git client against the real routes. It needs a listening
 * socket, which some hosts refuse, and a git binary; it says which is
 * missing rather than passing quietly.
 */
const missing = await whatIsMissing();

const cleanups: Array<() => void | Promise<void>> = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) {
    await cleanup();
  }
});

describe.skipIf(missing.length > 0)(
  `a real git client against the git routes${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('clones the repository from the organization and workspace path', async () => {
      const world = await servedService({ cleanups });
      const url = world.url;

      const cloned = await gitClone(withCredential(url, world.credential), cleanups);

      const stored = (await world.repos.listRefs(world.repository))
        .map((ref) => ref.name)
        .filter((name) => name.startsWith('refs/heads/') || name.startsWith('refs/tags/'));
      expect(cloned.refs.toSorted()).toEqual(stored.toSorted());
    });

    it('clones the hub_url a workspace reports, which is the address served', async () => {
      const world = await servedService({ cleanups });
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });
      const body = (await read.json()) as { hub_url: string | null };

      expect(body.hub_url).toBe(
        `${world.base}/git/${world.org?.slug ?? ''}/${world.workspace.slug}.git`,
      );
      expect(
        (await gitClone(withCredential(body.hub_url ?? '', world.credential), cleanups)).refs,
      ).toEqual(['refs/heads/main']);
    });

    it('reports no hub_url for a workspace with no organization, and serves it nowhere', async () => {
      const world = await servedService({ cleanups, withOrg: false });
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });

      expect(((await read.json()) as { hub_url: string | null }).hub_url).toBeNull();
      await expect(
        gitClone(`${world.base}/git//${world.workspace.slug}.git`, cleanups),
      ).rejects.toThrow();
    });

    it('reports no hub_url where the deployment has no external URL', async () => {
      const world = await servedService({ cleanups, externalUrl: '' });
      const read = await world.service.app.request(`/api/v1/workspaces/${world.workspace.slug}`, {
        headers: { authorization: `Bearer ${world.credential}` },
      });

      expect(((await read.json()) as { hub_url: string | null }).hub_url).toBeNull();
    });
  },
);

describe.skipIf(missing.length > 0)(
  `fetching from the git routes${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it.each(['git-upload-pack', 'git-receive-pack'])(
      'advertises refs for %s, in a body a real git client parses',
      async (service) => {
        const world = await servedService({ cleanups });
        const response = await fetch(`${world.url}/info/refs?service=${service}`, {
          headers: {
            authorization: `Basic ${Buffer.from(`parley:${world.credential}`).toString('base64')}`,
          },
        });

        expect(response.status).toBe(200);
        expect(response.headers.get('content-type')).toBe(`application/x-${service}-advertisement`);

        // What git itself makes of it: ls-remote parses the same
        // advertisement a clone or a push would.
        const listed = await gitRun(
          'git',
          ['ls-remote', withCredential(world.url, world.credential)],
          { env: GIT_ENV, timeout: 30_000 },
        );
        expect(listed.stdout).toContain('refs/heads/main');
      },
    );

    it('transfers only what the client lacks when it fetches again', async () => {
      const world = await servedService({ cleanups });
      // Enough history that a full transfer and an incremental one are
      // not the same size by accident.
      let parent = (await world.repos.listRefs(world.repository)).find(
        (ref) => ref.name === 'refs/heads/main',
      )?.oid;
      for (let i = 0; i < 40; i += 1) {
        const built = await buildCommit(
          world.repos,
          world.repository,
          Object.fromEntries(
            Array.from({ length: 20 }, (_, j) => [
              `file-${String(j)}.txt`,
              `commit ${String(i)} line ${String(j)} ${'padding '.repeat(60)}\n`,
            ]),
          ),
          parent === undefined ? [] : [parent],
          `commit ${String(i)}`,
        );
        parent = built.oid;
      }
      await world.repos.updateRefs(world.repository, [
        { name: 'refs/heads/main', oldOid: null, newOid: parent ?? '', force: true },
      ]);

      const url = withCredential(world.url, world.credential);
      const beforeClone = world.bytesServed();
      const cloned = await gitClone(url, cleanups);
      const cloneBytes = world.bytesServed() - beforeClone;

      const extra = await buildCommit(
        world.repos,
        world.repository,
        { 'one-more.txt': 'the only new content\n' },
        [parent ?? ''],
        'one more',
      );
      await world.repos.updateRefs(world.repository, [
        { name: 'refs/heads/main', oldOid: parent ?? null, newOid: extra.oid, force: true },
      ]);

      const beforeFetch = world.bytesServed();
      await gitRun('git', ['-C', cloned.dir, 'fetch', '--quiet', 'origin'], {
        env: GIT_ENV,
        timeout: 30_000,
      });
      const fetchBytes = world.bytesServed() - beforeFetch;

      expect(cloneBytes).toBeGreaterThan(10_000);
      expect(fetchBytes).toBeLessThan(cloneBytes / 10);
      const listed = await gitRun('git', ['ls-remote', url], { env: GIT_ENV, timeout: 30_000 });
      expect(listed.stdout).toContain(extra.oid);
    });

    it.each([
      ['version 2', '2'],
      ['version 0', '0'],
    ])('clones for a client asking for protocol %s', async (_what, version) => {
      const world = await servedService({ cleanups });

      const dir = mkdtempSync(join(tmpdir(), 'parley-version-'));
      cleanups.push(() => {
        rmSync(dir, { recursive: true, force: true });
      });
      await gitRun(
        'git',
        [
          '-c',
          `protocol.version=${version}`,
          'clone',
          '--quiet',
          withCredential(world.url, world.credential),
          dir,
        ],
        { env: GIT_ENV, timeout: 30_000 },
      );

      expect(await gitRefs(dir)).toEqual(['refs/heads/main']);
    });
  },
);
