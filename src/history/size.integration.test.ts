import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { randomFillSync } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { hasRealGit } from '../testutil/realgit.js';
import { historyEngine } from './engine.js';

/**
 * The size the engine states it works at: a stack of 50 patches onto a
 * repository of 500 MB of packed objects. Building the fixture costs
 * minutes and half a gigabyte of disk, so the suite runs when it is
 * asked for and says why it did not otherwise.
 */
const asked = process.env['PARLEY_BIG_FIXTURE'] ?? '';
const ballastMegabytes = Number(asked === '1' ? '480' : asked);

const missing = [
  ...(hasRealGit() ? [] : ['a git binary on the path']),
  ...(asked === '' ? ['PARLEY_BIG_FIXTURE, which this fixture costs minutes to build'] : []),
];

const PATCHES = 50;
const REPO = 'a-large-repository';
const cleanups: Array<() => void> = [];

afterAll(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function temporary(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

/**
 * A repository of incompressible ballast with a 50-patch branch beside
 * a base that moved on, built with the real git binary because nothing
 * else builds half a gigabyte in reasonable time.
 */
function buildFixture(): { dir: string; pack: Buffer; base: string; branch: string } {
  const dir = temporary('parley-big-');
  const run = (...args: string[]) =>
    execFileSync('git', args, {
      cwd: dir,
      encoding: 'utf8',
      maxBuffer: 1024 * 1024 * 64,
      env: {
        ...process.env,
        GIT_AUTHOR_NAME: 'Fixture',
        GIT_AUTHOR_EMAIL: 'fixture@parley.test',
        GIT_COMMITTER_NAME: 'Fixture',
        GIT_COMMITTER_EMAIL: 'fixture@parley.test',
        GIT_AUTHOR_DATE: '2023-11-14T22:13:20Z',
        GIT_COMMITTER_DATE: '2023-11-14T22:13:20Z',
      },
    });

  run('init', '-q', '-b', 'main');
  run('config', 'gc.auto', '0');

  // Random bytes, because packed size is what the limit is stated in
  // and anything compressible would weigh a fraction of what it looks.
  const chunk = Buffer.allocUnsafe(1024 * 1024);
  for (let i = 0; i < ballastMegabytes; i += 1) {
    randomFillSync(chunk);
    writeFileSync(join(dir, `ballast-${String(i)}.bin`), chunk);
  }
  run('add', '-A');
  run('commit', '-qm', 'ballast');
  const root = run('rev-parse', 'HEAD').trim();

  writeFileSync(join(dir, 'base.txt'), 'the base moved on\n');
  run('add', '-A');
  run('commit', '-qm', 'the base moved on');
  const base = run('rev-parse', 'HEAD').trim();

  run('checkout', '-q', '-b', 'work', root);
  for (let i = 0; i < PATCHES; i += 1) {
    writeFileSync(join(dir, `patch-${String(i)}.txt`), `patch ${String(i)}\n`);
    run('add', '-A');
    run('commit', '-qm', `patch ${String(i)}`);
  }
  const branch = run('rev-parse', 'HEAD').trim();

  const packPath = join(temporary('parley-bigpack-'), 'all.pack');
  execFileSync('/bin/sh', [
    '-c',
    `git -C '${dir}' rev-list --objects --all | git -C '${dir}' pack-objects --stdout > '${packPath}'`,
  ]);
  return { dir, pack: readFileSync(packPath), base, branch };
}

describe.skipIf(missing.length > 0)(
  `the engine at the size it states${missing.length > 0 ? ` (needs ${missing.join(' and ')})` : ''}`,
  () => {
    it(
      'applies a fifty-patch stack onto a repository of the stated size, as git does',
      async () => {
        const fixture = buildFixture();
        const packedMegabytes = Math.round(fixture.pack.byteLength / (1024 * 1024));
        // A fixture that did not reach the size it claims proves nothing.
        expect(packedMegabytes, 'the packed fixture').toBeGreaterThan(ballastMegabytes * 0.9);

        const repos = localRepositoryStore(temporary('parley-bigstore-'));
        await repos.create(REPO);
        await repos.receivePack(REPO, new Uint8Array(fixture.pack), [
          { name: 'refs/heads/main', oldOid: null, newOid: fixture.base },
          { name: 'refs/heads/work', oldOid: null, newOid: fixture.branch },
        ]);

        const started = Date.now();
        const rebased = await historyEngine(repos).rebase({
          repository: REPO,
          branch: fixture.branch,
          onto: fixture.base,
          committer: {
            name: 'Parley',
            email: 'parley@agent-fox.dev',
            timestamp: 1_700_000_100,
            timezoneOffset: 0,
          },
        });
        const took = Date.now() - started;

        expect(rebased.conflicts).toBeUndefined();
        expect(rebased.commits).toHaveLength(PATCHES);

        // The same rebase, by the binary the engine must agree with.
        const run = (...args: string[]) =>
          execFileSync('git', args, { cwd: fixture.dir, encoding: 'utf8' }).trim();
        run('rebase', '--onto', fixture.base, 'main', 'work');

        const tip = await repos.readObject(REPO, rebased.commits?.at(-1) ?? '');
        const mine = /^tree ([0-9a-f]{40})/.exec(
          new TextDecoder().decode(tip?.data ?? new Uint8Array()),
        );

        expect(mine?.[1], "the engine's tree").toBe(run('rev-parse', 'HEAD^{tree}'));
        console.log(
          `rebased ${String(PATCHES)} patches over ${String(packedMegabytes)} MB of packed objects in ${String(took)} ms`,
        );
      },
      30 * 60 * 1000,
    );
  },
);
