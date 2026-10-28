import { afterEach, describe, expect, it } from 'vitest';
import { LOCAL_CAPABILITIES } from '../platform/local/repostore.js';
import {
  GIT_ENV,
  type GitWorld,
  gitClone,
  gitRun,
  servedService,
  whatIsMissing,
  withCredential,
} from '../testutil/gitserver.js';
import { PROTECTED_MESSAGE, readableNamespace } from './protected_refs.js';

/**
 * The namespace the hub keeps, held against the client that would
 * overwrite it. Reading those refs is the store's business and
 * writing them is not, so the two are measured separately.
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

const HELD = 'refs/hub/replaced/a-branch';

/** Puts a ref under the protected namespace at the repository's head. */
async function withHeldRef(world: GitWorld): Promise<string> {
  const head =
    (await world.repos.listRefs(world.repository)).find((ref) => ref.name === 'refs/heads/main')
      ?.oid ?? '';
  await world.repos.updateRefs(world.repository, [{ name: HELD, oldOid: null, newOid: head }]);
  return head;
}

async function pushRefs(dir: string, args: string[]): Promise<string> {
  const pushed = await gitRun('git', ['-C', dir, 'push', '--porcelain', 'origin', ...args], {
    env: { ...GIT_ENV, ...AUTHORED },
    timeout: 30_000,
  }).catch((err: { stdout?: string; stderr?: string }) => ({
    stdout: `${err.stdout ?? ''}${err.stderr ?? ''}`,
  }));
  return pushed.stdout;
}

describe.skipIf(missing.length > 0)(
  `the protected namespace${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it('refuses a create, an update and a delete while the rest of the push lands', async () => {
      const world = await servedService({ cleanups });
      const head = await withHeldRef(world);
      const url = withCredential(world.url, world.credential);
      const cloned = await gitClone(url, cleanups);
      const before = new Map(
        (await world.repos.listRefs(world.repository)).map((ref) => [ref.name, ref.oid]),
      );

      await gitRun('git', ['-C', cloned.dir, 'checkout', '-q', '-b', 'ordinary'], {
        env: GIT_ENV,
      });
      await gitRun('git', ['-C', cloned.dir, 'commit', '-q', '--allow-empty', '-m', 'ordinary'], {
        env: { ...GIT_ENV, ...AUTHORED },
      });
      const made = (
        await gitRun('git', ['-C', cloned.dir, 'rev-parse', 'HEAD'], { env: GIT_ENV })
      ).stdout.trim();

      const reported = await pushRefs(cloned.dir, [
        'ordinary',
        // A create, an update and a delete, all in the one push.
        `${made}:refs/hub/replaced/created`,
        `${made}:${HELD}`,
        `:refs/hub/replaced/gone`,
      ]);

      expect(reported).toContain(PROTECTED_MESSAGE);

      const after = new Map(
        (await world.repos.listRefs(world.repository)).map((ref) => [ref.name, ref.oid]),
      );
      // The ordinary branch landed; the namespace is exactly as it was.
      expect(after.get('refs/heads/ordinary')).toBe(made);
      expect(after.get(HELD)).toBe(head);
      expect(after.has('refs/hub/replaced/created')).toBe(false);
      expect([...after].filter(([name]) => name.startsWith('refs/hub/replaced/'))).toEqual(
        [...before].filter(([name]) => name.startsWith('refs/hub/replaced/')),
      );
    });

    it('serves those refs to a reader where the store accepts the namespace', async () => {
      const world = await servedService({ cleanups });
      const head = await withHeldRef(world);
      const url = withCredential(world.url, world.credential);

      expect(readableNamespace(world.repos.capabilities)).toBe(true);
      const listed = await gitRun('git', ['ls-remote', url], { env: GIT_ENV, timeout: 30_000 });
      expect(listed.stdout).toContain(HELD);

      // And it fetches, which is the read that matters.
      const cloned = await gitClone(url, cleanups);
      await gitRun(
        'git',
        ['-C', cloned.dir, 'fetch', '--quiet', 'origin', `${HELD}:refs/heads/fetched`],
        { env: GIT_ENV, timeout: 30_000 },
      );
      const fetched = (
        await gitRun('git', ['-C', cloned.dir, 'rev-parse', 'refs/heads/fetched'], {
          env: GIT_ENV,
        })
      ).stdout.trim();
      expect(fetched).toBe(head);
    });

    it('keeps the push refusal where the store refuses the namespace outright', async () => {
      // A store that will not hold the namespace at all. What the
      // service does about reading follows its capability report
      // rather than a guess, and what it does about writing does not
      // depend on the store at all.
      const world = await servedService({
        cleanups,
        narrow: (store) => ({
          ...store,
          capabilities: { ...LOCAL_CAPABILITIES, arbitraryRefNamespaces: false },
          listRefs: async (name) =>
            (await store.listRefs(name)).filter(
              (ref) => ref.name.startsWith('refs/heads/') || ref.name.startsWith('refs/tags/'),
            ),
        }),
      });
      expect(readableNamespace(world.repos.capabilities)).toBe(false);

      const url = withCredential(world.url, world.credential);
      const cloned = await gitClone(url, cleanups);
      const made = (
        await gitRun('git', ['-C', cloned.dir, 'rev-parse', 'HEAD'], { env: GIT_ENV })
      ).stdout.trim();

      // The read is unavailable: the namespace is listed nowhere.
      const listed = await gitRun('git', ['ls-remote', url], { env: GIT_ENV, timeout: 30_000 });
      expect(listed.stdout).not.toContain('refs/hub/replaced/');

      // The refusal of the push holds all the same.
      expect(await pushRefs(cloned.dir, [`${made}:${HELD}`])).toContain(PROTECTED_MESSAGE);
      expect((await world.repos.listRefs(world.repository)).some((ref) => ref.name === HELD)).toBe(
        false,
      );
    });
  },
);
