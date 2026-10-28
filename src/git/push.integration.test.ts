import { afterEach, describe, expect, it } from 'vitest';
import {
  GIT_ENV,
  gitClone,
  gitRun,
  servedService,
  whatIsMissing,
  withCredential,
} from '../testutil/gitserver.js';

/**
 * Pushing, by the client that does it. The per-ref report is how a
 * person learns what happened, so it is read from git's own output
 * rather than from the bytes the service wrote.
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

/** What git reports for each ref it pushed, as `<flag>\t<ref>` lines. */
async function push(dir: string, args: string[]): Promise<string> {
  const pushed = await gitRun('git', ['-C', dir, 'push', '--porcelain', 'origin', ...args], {
    env: { ...GIT_ENV, ...AUTHORED },
    timeout: 30_000,
  }).catch((err: { stdout?: string; stderr?: string }) => ({
    stdout: err.stdout ?? '',
    stderr: err.stderr ?? '',
  }));
  return pushed.stdout;
}

describe.skipIf(missing.length > 0)(
  `pushing to the git routes${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('applies a new branch, an update and a tag, and reports each one', async () => {
      const world = await servedService({ cleanups });
      const url = withCredential(world.url, world.credential);
      const cloned = await gitClone(url, cleanups);

      await gitRun('git', ['-C', cloned.dir, 'commit', '-q', '--allow-empty', '-m', 'on main'], {
        env: { ...GIT_ENV, ...AUTHORED },
      });
      await gitRun('git', ['-C', cloned.dir, 'checkout', '-q', '-b', 'a-new-branch'], {
        env: GIT_ENV,
      });
      await gitRun(
        'git',
        ['-C', cloned.dir, 'commit', '-q', '--allow-empty', '-m', 'on the branch'],
        {
          env: { ...GIT_ENV, ...AUTHORED },
        },
      );
      await gitRun('git', ['-C', cloned.dir, 'tag', 'a-tag'], { env: GIT_ENV });

      const reported = await push(cloned.dir, ['main', 'a-new-branch', 'a-tag']);

      // Every ref git names is one it was told was taken.
      expect(reported).toMatch(/\*?\t?refs\/heads\/main/);
      expect(reported).not.toMatch(/^!/m);

      const stored = new Map(
        (await world.repos.listRefs(world.repository)).map((ref) => [ref.name, ref.oid]),
      );
      const local = async (ref: string) =>
        (await gitRun('git', ['-C', cloned.dir, 'rev-parse', ref], { env: GIT_ENV })).stdout.trim();

      expect(stored.get('refs/heads/main')).toBe(await local('refs/heads/main'));
      expect(stored.get('refs/heads/a-new-branch')).toBe(await local('refs/heads/a-new-branch'));
      expect(stored.get('refs/tags/a-tag')).toBe(await local('refs/tags/a-tag'));
    });

    it('changes exactly the refs it reported as taken', async () => {
      // One ref the service refuses and one it takes, pushed together.
      const refused = 'refs/heads/no-thank-you';
      const world = await servedService({
        cleanups,
        decideRef: (command) =>
          Promise.resolve(
            command.name === refused
              ? { accept: false as const, reason: 'this branch is not wanted here' }
              : { accept: true as const },
          ),
      });
      const url = withCredential(world.url, world.credential);
      const cloned = await gitClone(url, cleanups);

      const before = new Map(
        (await world.repos.listRefs(world.repository)).map((ref) => [ref.name, ref.oid]),
      );

      for (const branch of ['welcome', 'no-thank-you']) {
        await gitRun('git', ['-C', cloned.dir, 'checkout', '-q', '-b', branch, 'main'], {
          env: GIT_ENV,
        });
        await gitRun('git', ['-C', cloned.dir, 'commit', '-q', '--allow-empty', '-m', branch], {
          env: { ...GIT_ENV, ...AUTHORED },
        });
      }

      const reported = await push(cloned.dir, ['welcome', 'no-thank-you']);

      // git's own report: one taken, one refused with the reason.
      expect(reported).toMatch(/refs\/heads\/welcome/);
      expect(reported).toMatch(/this branch is not wanted here/);

      const after = new Map(
        (await world.repos.listRefs(world.repository)).map((ref) => [ref.name, ref.oid]),
      );
      const changed = [...after]
        .filter(([name, oid]) => before.get(name) !== oid)
        .map(([name]) => name);

      expect(changed).toEqual(['refs/heads/welcome']);
      expect(after.has(refused)).toBe(false);
    });
  },
);
