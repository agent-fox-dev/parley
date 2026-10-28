import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';

import { seedPatch } from '../testutil/identity.js';
import { recordedEvents } from '../testutil/app.js';
import { type FakeRemote, fakeRemote } from '../testutil/fakeremote.js';
import {
  GIT_ENV,
  type GitWorld,
  gitClone,
  gitRun,
  servedService,
  whatIsMissing,
  withCredential,
} from '../testutil/gitserver.js';

/**
 * The four outcomes of the pre-receive table, produced in front of a
 * store that has no pre-receive hook, and read from what a real git
 * client prints.
 */
const missing = await whatIsMissing();

const cleanups: Array<() => void | Promise<void>> = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) {
    await cleanup();
  }
});

const AUTHORED = {
  GIT_AUTHOR_NAME: 'A Person',
  GIT_AUTHOR_EMAIL: 'person@parley.test',
  GIT_COMMITTER_NAME: 'A Person',
  GIT_COMMITTER_EMAIL: 'person@parley.test',
};

const PATCH = 'a-patch';

interface Carried {
  world: GitWorld;
  remote: FakeRemote;
  dir: string;
}

/** A carry-patch workspace with one registered patch branch and a fork. */
async function carried(
  settings: Record<string, unknown>,
  options: { credential?: boolean; remote?: FakeRemote } = {},
): Promise<Carried> {
  const remote = options.remote ?? fakeRemote();
  cleanups.push(remote.dispose);

  const world = await servedService({
    cleanups,
    fetch: remote.fetch,
    workspace: {
      workspace_mode: 'carry_patch',
      integration_branch: 'integration',
      git_url: `${remote.url}/fork.git`,
      settings,
    },
  });
  await seedPatch(world.service.store, world.workspace.slug, PATCH);
  if (options.credential !== false) {
    await world.service.secrets.put(
      workspaceScope(world.workspace.slug),
      'GIT_PAT',
      'a-token-for-the-fork',
    );
  }

  const cloned = await gitClone(withCredential(world.url, world.credential), cleanups);
  return { world, remote, dir: cloned.dir };
}

/** Makes a branch with one commit on it and pushes it, reporting git's output. */
async function pushBranch(dir: string, branch: string, from = 'main'): Promise<string> {
  await gitRun('git', ['-C', dir, 'checkout', '-q', '-B', branch, from], { env: GIT_ENV });
  await gitRun('git', ['-C', dir, 'commit', '-q', '--allow-empty', '-m', `on ${branch}`], {
    env: { ...GIT_ENV, ...AUTHORED },
  });
  const pushed = await gitRun('git', ['-C', dir, 'push', '--porcelain', 'origin', branch], {
    env: { ...GIT_ENV, ...AUTHORED },
    timeout: 30_000,
  }).catch((err: { stdout?: string; stderr?: string }) => ({
    stdout: `${err.stdout ?? ''}${err.stderr ?? ''}`,
  }));
  return pushed.stdout;
}

/** Whether the store holds a branch at all. */
async function holds(world: GitWorld, branch: string): Promise<boolean> {
  const refs = await world.repos.listRefs(world.repository);
  return refs.some((ref) => ref.name === `refs/heads/${branch}`);
}

describe.skipIf(missing.length > 0)(
  `push control${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('refuses a patch branch that is synced from the fork, and says where to push', async () => {
      const place = await carried({
        patch_branch_source: 'origin',
        push_patches_to_origin: false,
      });

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).toContain('branch is synced from origin; push to');
      // The address carries no credential, however the workspace holds one.
      expect(reported).not.toContain('@');
      expect(await holds(place.world, PATCH)).toBe(false);
      expect(place.remote.requests()).toBe(0);
    });

    it('refuses a deletion of such a branch by telling the client where to delete it', async () => {
      const place = await carried({
        patch_branch_source: 'origin',
        push_patches_to_origin: false,
      });

      const reported = await gitRun(
        'git',
        ['-C', place.dir, 'push', '--porcelain', 'origin', `:refs/heads/${PATCH}`],
        { env: GIT_ENV, timeout: 30_000 },
      ).catch((err: { stdout?: string; stderr?: string }) => ({
        stdout: `${err.stdout ?? ''}${err.stderr ?? ''}`,
      }));

      expect(reported.stdout).toContain('delete it on the fork instead');
    });

    it('forwards to the fork first and takes the branch only if the fork took it', async () => {
      const place = await carried({
        patch_branch_source: 'origin',
        push_patches_to_origin: true,
      });

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).not.toContain('synced from origin');
      expect(await holds(place.world, PATCH)).toBe(true);
      // Forwarded as the client sent it, which is not a forced push.
      const sent = place.remote.pushed();
      expect(sent).toHaveLength(1);
      expect(sent[0]?.name).toBe(`refs/heads/${PATCH}`);
      expect(sent[0]?.oldOid).toBe('0'.repeat(40));
    });

    it('refuses the branch when the fork refuses it, naming what the fork said', async () => {
      const refusing = fakeRemote({ refusesPush: 'the fork says no' });
      const place = await carried(
        { patch_branch_source: 'origin', push_patches_to_origin: true },
        { remote: refusing },
      );

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).toContain('origin rejected push:');
      expect(await holds(place.world, PATCH)).toBe(false);
    });

    it('refuses the branch when the workspace has no usable fork credential', async () => {
      const place = await carried(
        { patch_branch_source: 'origin', push_patches_to_origin: true },
        { credential: false },
      );

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).toContain('could not resolve the credentials for origin');
      expect(await holds(place.world, PATCH)).toBe(false);
      expect(place.remote.requests()).toBe(0);
    });

    it('takes a hub-owned patch branch and then mirrors it to the fork', async () => {
      const place = await carried({
        patch_branch_source: 'hub',
        push_patches_to_origin: true,
      });

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).not.toContain('synced from origin');
      expect(await holds(place.world, PATCH)).toBe(true);
      // Mirrored by force: the hub's copy is the one that counts.
      const sent = place.remote.pushed();
      expect(sent.map((one) => one.name)).toEqual([`refs/heads/${PATCH}`]);
      expect(sent[0]?.oldOid).toBe('0'.repeat(40));
    });

    it('keeps a hub-owned push successful when the mirror fails', async () => {
      const refusing = fakeRemote({ refusesPush: 'the fork says no' });
      const place = await carried(
        { patch_branch_source: 'hub', push_patches_to_origin: true },
        { remote: refusing },
      );

      const reported = await pushBranch(place.dir, PATCH);

      expect(reported).not.toContain('rejected');
      expect(await holds(place.world, PATCH)).toBe(true);

      // The hub's copy is the one that counts, so the mirror that
      // failed is written down rather than answered with.
      const [recorded] = await recordedEvents(place.world.service.store, 'hub.patch.mirror_failed');
      expect(recorded?.metadata).toMatchObject({ branch_name: PATCH });
    });

    it.each([
      [
        'a hub-owned branch nobody mirrors',
        { patch_branch_source: 'hub', push_patches_to_origin: false },
        PATCH,
      ],
      [
        'a branch that is not a patch',
        { patch_branch_source: 'origin', push_patches_to_origin: false },
        'ordinary',
      ],
      [
        'the integration branch',
        { patch_branch_source: 'origin', push_patches_to_origin: false },
        'integration',
      ],
    ])('applies no push control to %s', async (_what, settings, branch) => {
      const place = await carried(settings);

      const reported = await pushBranch(place.dir, branch);

      expect(reported).not.toContain('synced from origin');
      expect(await holds(place.world, branch)).toBe(true);
      expect(place.remote.requests()).toBe(0);
    });

    it('applies no push control to a standard workspace', async () => {
      const remote = fakeRemote();
      cleanups.push(remote.dispose);
      const world = await servedService({ cleanups, fetch: remote.fetch });
      await seedPatch(world.service.store, world.workspace.slug, PATCH);
      const cloned = await gitClone(withCredential(world.url, world.credential), cleanups);

      const reported = await pushBranch(cloned.dir, PATCH);

      expect(reported).not.toContain('synced from origin');
      expect(await holds(world, PATCH)).toBe(true);
      expect(remote.requests()).toBe(0);
    });

    it('decides each ref of one push on its own', async () => {
      const place = await carried({
        patch_branch_source: 'origin',
        push_patches_to_origin: false,
      });

      for (const branch of [PATCH, 'ordinary']) {
        await gitRun('git', ['-C', place.dir, 'checkout', '-q', '-B', branch, 'main'], {
          env: GIT_ENV,
        });
        await gitRun('git', ['-C', place.dir, 'commit', '-q', '--allow-empty', '-m', branch], {
          env: { ...GIT_ENV, ...AUTHORED },
        });
      }
      await gitRun('git', ['-C', place.dir, 'tag', 'a-tag'], { env: GIT_ENV });

      const reported = await gitRun(
        'git',
        ['-C', place.dir, 'push', '--porcelain', 'origin', PATCH, 'ordinary', 'a-tag'],
        { env: { ...GIT_ENV, ...AUTHORED }, timeout: 30_000 },
      ).catch((err: { stdout?: string; stderr?: string }) => ({
        stdout: `${err.stdout ?? ''}${err.stderr ?? ''}`,
      }));

      expect(reported.stdout).toContain('synced from origin');
      expect(await holds(place.world, 'ordinary')).toBe(true);
      expect(await holds(place.world, PATCH)).toBe(false);
      const refs = (await place.world.repos.listRefs(place.world.repository)).map((r) => r.name);
      expect(refs).toContain('refs/tags/a-tag');
    });
  },
);
