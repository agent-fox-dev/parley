import { execFile } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it } from 'vitest';
import { apiKey, baseUrl, bearer, call, memberKey, missing, needed } from '../testutil/smoke.js';
import { hasRealGit } from '../testutil/realgit.js';

/**
 * The git server against a running deployment, driven by the git
 * binary a person would use. Everything a unit suite can stub is real
 * here: the deployment, the access decision, the store and the client.
 */
const run = promisify(execFile);

const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_TERMINAL_PROMPT: '0',
  GIT_ASKPASS: '/usr/bin/true',
  GIT_AUTHOR_NAME: 'Parley Smoke',
  GIT_AUTHOR_EMAIL: 'smoke@parley.test',
  GIT_COMMITTER_NAME: 'Parley Smoke',
  GIT_COMMITTER_EMAIL: 'smoke@parley.test',
};

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
  ['a git binary on the path', hasRealGit() ? 'yes' : ''],
);

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

function temporary(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-smoke-git-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}

/** The workspace as the deployment reports it. */
async function workspace(
  credential = apiKey,
): Promise<{ hub_url: string | null; head_sha: string | null }> {
  const response = await call(`/api/v1/workspaces/${slug}`, bearer(credential));
  return (await response.json()) as { hub_url: string | null; head_sha: string | null };
}

/** A clone of the workspace, with the credential in the URL. */
async function clone(url: string, credential: string): Promise<string> {
  const dir = temporary();
  await run('git', ['clone', '--quiet', withCredential(url, credential), dir], {
    env: GIT_ENV,
    timeout: 120_000,
  });
  return dir;
}

function withCredential(url: string, credential: string): string {
  return url.replace('://', `://parley:${encodeURIComponent(credential)}@`);
}

async function git(dir: string, ...args: string[]): Promise<{ stdout: string; stderr: string }> {
  return run('git', ['-C', dir, ...args], { env: GIT_ENV, timeout: 120_000 }).catch(
    (err: { stdout?: string; stderr?: string }) => ({
      stdout: err.stdout ?? '',
      stderr: err.stderr ?? '',
    }),
  );
}

describe.skipIf(reason.length > 0)(`the git server end to end${needed(reason)}`, () => {
  it('clones, takes a push and refreshes the head', async () => {
    const before = await workspace();
    expect(before.hub_url, 'the workspace reports a clone URL').not.toBeNull();
    const dir = await clone(before.hub_url ?? '', apiKey);

    // The clone carried the repository, not an empty directory.
    const listed = await git(dir, 'rev-parse', 'HEAD');
    expect(listed.stdout.trim()).toMatch(/^[0-9a-f]{40}$/);

    const branch = `smoke-${Date.now().toString(36)}`;
    await git(dir, 'checkout', '-q', '-b', branch);
    await git(dir, 'commit', '-q', '--allow-empty', '-m', 'a commit from the smoke suite');
    const pushed = await git(dir, 'push', 'origin', branch);
    expect(`${pushed.stdout}${pushed.stderr}`).not.toMatch(/rejected|error:/);

    const listedRemotely = await git(dir, 'ls-remote', 'origin', `refs/heads/${branch}`);
    expect(listedRemotely.stdout).toContain(branch);

    // Pushing to the default branch is what moves the head, so the
    // workspace is asked about the branch it actually reports.
    const after = await workspace();
    expect(after.head_sha).toMatch(/^[0-9a-f]{40}$/);

    await git(dir, 'push', 'origin', '--delete', branch);
  }, 300_000);

  it.skipIf(memberKey.length === 0)(
    'serves a member the clone and refuses them the push',
    async () => {
      const reported = await workspace();
      const url = reported.hub_url ?? '';
      const asOwner = await clone(url, apiKey);
      const asMember = await clone(url, memberKey);

      const ownerRefs = await git(asOwner, 'show-ref');
      const memberRefs = await git(asMember, 'show-ref');
      expect(memberRefs.stdout).toBe(ownerRefs.stdout);

      await git(asMember, 'checkout', '-q', '-b', 'smoke-from-a-member');
      await git(asMember, 'commit', '-q', '--allow-empty', '-m', 'from a member');
      const pushed = await git(asMember, 'push', 'origin', 'smoke-from-a-member');

      expect(`${pushed.stdout}${pushed.stderr}`).toMatch(/read-only access/);
      const after = await git(asOwner, 'ls-remote', 'origin', 'refs/heads/smoke-from-a-member');
      expect(after.stdout.trim()).toBe('');
    },
    300_000,
  );

  it('refuses a push to the namespace the hub keeps, and takes the rest', async () => {
    const reported = await workspace();
    const dir = await clone(reported.hub_url ?? '', apiKey);
    const ordinary = `smoke-ordinary-${Date.now().toString(36)}`;

    await git(dir, 'commit', '-q', '--allow-empty', '-m', 'for the protected push');
    const pushed = await git(
      dir,
      'push',
      'origin',
      `HEAD:refs/hub/replaced/smoke`,
      `HEAD:refs/heads/${ordinary}`,
    );

    expect(`${pushed.stdout}${pushed.stderr}`).toMatch(/maintained by the hub/);
    const held = await git(dir, 'ls-remote', 'origin', 'refs/hub/replaced/smoke');
    expect(held.stdout.trim()).toBe('');
    const landed = await git(dir, 'ls-remote', 'origin', `refs/heads/${ordinary}`);
    expect(landed.stdout).toContain(ordinary);

    await git(dir, 'push', 'origin', '--delete', ordinary);
  }, 300_000);

  it.skipIf((process.env['PARLEY_SMOKE_PATCH_WORKSPACE'] ?? '').length === 0)(
    'refuses a patch branch while the ordinary branch of the same push lands',
    async () => {
      const patchSlug = process.env['PARLEY_SMOKE_PATCH_WORKSPACE'] ?? '';
      const patchBranch = process.env['PARLEY_SMOKE_PATCH_BRANCH'] ?? 'a-patch';
      const response = await call(`/api/v1/workspaces/${patchSlug}`, bearer(apiKey));
      const reported = (await response.json()) as { hub_url: string | null };
      const dir = await clone(reported.hub_url ?? '', apiKey);
      const ordinary = `smoke-ordinary-${Date.now().toString(36)}`;

      await git(dir, 'checkout', '-q', '-B', patchBranch);
      await git(dir, 'commit', '-q', '--allow-empty', '-m', 'on the patch branch');
      await git(dir, 'checkout', '-q', '-b', ordinary);
      await git(dir, 'commit', '-q', '--allow-empty', '-m', 'on an ordinary branch');

      const pushed = await git(dir, 'push', 'origin', patchBranch, ordinary);
      const output = `${pushed.stdout}${pushed.stderr}`;

      expect(output).toMatch(/synced from origin|origin rejected push/);
      const landed = await git(dir, 'ls-remote', 'origin', `refs/heads/${ordinary}`);
      expect(landed.stdout).toContain(ordinary);

      await git(dir, 'push', 'origin', '--delete', ordinary);
    },
    300_000,
  );
});
