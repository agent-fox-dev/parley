import { afterEach, describe, expect, it } from 'vitest';
import { gitClone, servedService, whatIsMissing, withCredential } from '../testutil/gitserver.js';

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
