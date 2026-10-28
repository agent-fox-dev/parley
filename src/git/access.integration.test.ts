import { afterEach, describe, expect, it } from 'vitest';
import { addMember } from '../store/identity.js';
import { ADMIN_TOKEN } from '../testutil/app.js';
import {
  GIT_ENV,
  gitClone,
  gitRun,
  servedService,
  whatIsMissing,
  withCredential,
} from '../testutil/gitserver.js';
import { SEED_NOW, seedApiKey, seedUser } from '../testutil/identity.js';

/**
 * The access table, exercised by the client it exists for. A refusal
 * that a git client cannot act on is a refusal that does not work,
 * however well it reads in a unit test.
 */
const missing = await whatIsMissing();

const cleanups: Array<() => void | Promise<void>> = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) {
    await cleanup();
  }
});

/** Commits a file in a clone and pushes it as a new branch. */
async function pushBranch(dir: string, branch: string): Promise<string> {
  await gitRun('git', ['-C', dir, 'checkout', '-q', '-b', branch], { env: GIT_ENV });
  await gitRun('git', ['-C', dir, 'commit', '-q', '--allow-empty', '-m', `from ${branch}`], {
    env: {
      ...GIT_ENV,
      GIT_AUTHOR_NAME: 'A',
      GIT_AUTHOR_EMAIL: 'a@b.test',
      GIT_COMMITTER_NAME: 'A',
      GIT_COMMITTER_EMAIL: 'a@b.test',
    },
  });
  const pushed = await gitRun('git', ['-C', dir, 'push', '--quiet', 'origin', branch], {
    env: GIT_ENV,
    timeout: 30_000,
  });
  return pushed.stderr;
}

describe.skipIf(missing.length > 0)(
  `the access table against a real git client${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('serves the owner, an admin token and a member the same fetch, and lets the first two push', async () => {
      const world = await servedService({ cleanups });
      const member = await seedUser(world.service.store);
      await addMember(world.service.store, {
        org_id: world.org?.id ?? '',
        user_id: member.id,
        role: 'member',
        created_at: SEED_NOW.toISOString(),
      });
      const theirs = await seedApiKey(world.service.store, member);

      const asOwner = await gitClone(withCredential(world.url, world.credential), cleanups);
      const asMember = await gitClone(withCredential(world.url, theirs.presented), cleanups);
      const asAdmin = await gitClone(withCredential(world.url, ADMIN_TOKEN), cleanups);

      expect(asMember.refs).toEqual(asOwner.refs);
      expect(asAdmin.refs).toEqual(asOwner.refs);

      await pushBranch(asOwner.dir, 'from-the-owner');
      await pushBranch(asAdmin.dir, 'from-the-admin');

      const stored = (await world.repos.listRefs(world.repository)).map((ref) => ref.name);
      expect(stored).toContain('refs/heads/from-the-owner');
      expect(stored).toContain('refs/heads/from-the-admin');
    });

    it('refuses a member the push, and the repository keeps every ref it had', async () => {
      const world = await servedService({ cleanups });
      const member = await seedUser(world.service.store);
      await addMember(world.service.store, {
        org_id: world.org?.id ?? '',
        user_id: member.id,
        role: 'member',
        created_at: SEED_NOW.toISOString(),
      });
      const theirs = await seedApiKey(world.service.store, member);
      const before = await world.repos.listRefs(world.repository);

      const cloned = await gitClone(withCredential(world.url, theirs.presented), cleanups);

      await expect(pushBranch(cloned.dir, 'from-a-member')).rejects.toThrow(
        /org members have read-only access/,
      );
      expect(await world.repos.listRefs(world.repository)).toEqual(before);
    });

    it('tells a stranger the repository is not found', async () => {
      const world = await servedService({ cleanups });
      const stranger = await seedUser(world.service.store);
      const theirs = await seedApiKey(world.service.store, stranger);

      await expect(gitClone(withCredential(world.url, theirs.presented), cleanups)).rejects.toThrow(
        /repository not found/,
      );
    });

    it('serves a push from a clone taken with a token holding git:write', async () => {
      const world = await servedService({ cleanups });
      const cloned = await gitClone(withCredential(world.url, world.credential), cleanups);

      await pushBranch(cloned.dir, 'a-new-branch');

      // The branch is in the store, and another client sees it listed.
      const listed = await gitRun(
        'git',
        ['ls-remote', withCredential(world.url, world.credential)],
        { env: GIT_ENV, timeout: 30_000 },
      );
      expect(listed.stdout).toContain('refs/heads/a-new-branch');
    });
  },
);
