import { execFile } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it } from 'vitest';
import { afc, apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * A branch that reaches the forge, end to end: pushed to Parley's own
 * git server by a real git client, then sent on to the real remote by
 * the branch push route, driven by the command a person runs.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
  ['PARLEY_SMOKE_AFC', afc],
);

const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_TERMINAL_PROMPT: '0',
  GIT_ASKPASS: '/usr/bin/true',
  GIT_AUTHOR_NAME: 'Parley Smoke',
  GIT_AUTHOR_EMAIL: 'smoke@parley.test',
  GIT_COMMITTER_NAME: 'Parley Smoke',
  GIT_COMMITTER_EMAIL: 'smoke@parley.test',
};

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function afcRun(
  ...args: string[]
): Promise<{ code: number; stdout: string; stderr: string }> {
  return run(afc, args, {
    env: { ...process.env, ENDPOINT_URL: baseUrl, API_KEY: apiKey },
    timeout: 300_000,
  })
    .then((done) => ({ code: 0, ...done }))
    .catch((err: { code?: number; stdout?: string; stderr?: string }) => ({
      code: err.code ?? 1,
      stdout: err.stdout ?? '',
      stderr: err.stderr ?? '',
    }));
}

async function git(...args: string[]): Promise<{ stdout: string; stderr: string }> {
  return run('git', args, { env: GIT_ENV, timeout: 300_000 }).catch(
    (err: { stdout?: string; stderr?: string }) => ({
      stdout: err.stdout ?? '',
      stderr: err.stderr ?? '',
    }),
  );
}

describe.skipIf(reason.length > 0)(`a branch reaching the forge${needed(reason)}`, () => {
  it('is pushed to Parley, then sent on to the remote', async () => {
    const read = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey));
    const workspace = (await read.json()) as { hub_url: string | null };
    expect(workspace.hub_url, 'the workspace reports a clone URL').not.toBeNull();

    const dir = mkdtempSync(join(tmpdir(), 'parley-smoke-push-'));
    cleanups.push(() => {
      rmSync(dir, { recursive: true, force: true });
    });
    const url = (workspace.hub_url ?? '').replace(
      '://',
      `://parley:${encodeURIComponent(apiKey)}@`,
    );
    await git('clone', '--quiet', url, dir);

    // A name with a slash in it, which the route takes as one escaped
    // segment.
    const branch = `smoke/push-${Date.now().toString(36)}`;
    await git('-C', dir, 'checkout', '-q', '-b', branch);
    await git('-C', dir, 'commit', '-q', '--allow-empty', '-m', 'for the forge');
    await git('-C', dir, 'push', '--quiet', 'origin', branch);
    const tip = (await git('-C', dir, 'rev-parse', 'HEAD')).stdout.trim();

    const pushed = await afcRun('workspace', 'push', slug, branch);

    expect(pushed.code, pushed.stderr).toBe(0);
    expect(pushed.stdout).toContain('pushed');
    expect(pushed.stdout).toContain(tip.slice(0, 7));
    // Nothing in what it printed carries a credential.
    expect(pushed.stdout).not.toContain(apiKey);

    await git('-C', dir, 'push', '--quiet', 'origin', '--delete', branch);
  }, 300_000);
});
