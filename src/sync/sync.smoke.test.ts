import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it } from 'vitest';
import { afc, apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * The sync against a running deployment, driven by the command a
 * person runs. What the CLI prints and what the deployment records
 * are both read, because the route's whole job is to leave the
 * workspace saying something true about its remote.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
  ['PARLEY_SMOKE_AFC', afc],
);

const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

/** afc, told where the deployment is and who is calling. */
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

async function workspace(): Promise<Record<string, unknown>> {
  const response = await call(`/api/v1/workspaces/${slug}`, bearer(apiKey));
  return (await response.json()) as Record<string, unknown>;
}

describe.skipIf(reason.length > 0)(`the sync end to end${needed(reason)}`, () => {
  it('brings the remote’s work in and leaves the workspace saying so', async () => {
    const before = await workspace();

    const synced = await afcRun('workspace', 'sync', slug);

    expect(synced.code, synced.stderr).toBe(0);
    const after = await workspace();
    expect(after['sync_error']).toBeNull();
    expect(after['last_sync_at']).not.toBe(before['last_sync_at']);
    expect(String(after['head_sha'])).toMatch(/^[0-9a-f]{40}$/);
    expect(after['upstream_head_sha']).toBe(after['head_sha']);
  }, 300_000);

  it.skipIf((process.env['PARLEY_SMOKE_REWRITTEN_WORKSPACE'] ?? '').length === 0)(
    'refuses a rewritten remote and recovers when told to reset',
    async () => {
      const rewritten = process.env['PARLEY_SMOKE_REWRITTEN_WORKSPACE'] ?? '';

      const refused = await afcRun('workspace', 'sync', rewritten);
      expect(refused.code).not.toBe(0);

      const read = await call(`/api/v1/workspaces/${rewritten}`, bearer(apiKey));
      const recorded = (await read.json()) as { sync_error: string | null };
      expect(recorded.sync_error).toContain('reset_to_upstream');

      const recovered = await afcRun('workspace', 'sync', rewritten, '--reset-to-upstream');
      expect(recovered.code, recovered.stderr).toBe(0);

      const settled = await call(`/api/v1/workspaces/${rewritten}`, bearer(apiKey));
      expect(((await settled.json()) as { sync_error: string | null }).sync_error).toBeNull();
    },
    300_000,
  );

  it.skipIf((process.env['PARLEY_SMOKE_RECLONE_WORKSPACE'] ?? '').length === 0)(
    'recovers a broken repository with a reclone',
    async () => {
      const broken = process.env['PARLEY_SMOKE_RECLONE_WORKSPACE'] ?? '';

      const recloned = await afcRun('workspace', 'reclone', broken, '--confirm');
      expect(recloned.code, recloned.stderr).toBe(0);
      expect(recloned.stdout).toContain('pending');

      // It follows the new copy to a settled state.
      const deadline = Date.now() + 240_000;
      let status = 'pending';
      while (Date.now() < deadline && (status === 'pending' || status === 'cloning')) {
        await new Promise((resolve) => setTimeout(resolve, 3000));
        const read = await call(`/api/v1/workspaces/${broken}`, bearer(apiKey));
        status = ((await read.json()) as { status: string }).status;
      }
      expect(status).toBe('ready');
    },
    300_000,
  );
});
