import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, it } from 'vitest';
import { afc, apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * The patch stack end to end against a running deployment: a real
 * upstream, a real fork, the real history engine and the command a
 * person runs. Nothing here stands in for anything.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_PATCH_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_PATCH_WORKSPACE', slug],
  ['PARLEY_SMOKE_AFC', afc],
);

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

/** Polls a rebuild to a settled state and answers what it reached. */
async function settled(id: string): Promise<Record<string, unknown>> {
  const deadline = Date.now() + 240_000;
  let job: Record<string, unknown> = {};
  while (Date.now() < deadline) {
    const read = await call(`/api/v1/workspaces/${slug}/rebuilds/${id}`, bearer(apiKey));
    job = (await read.json()) as Record<string, unknown>;
    if (job['status'] !== 'queued' && job['status'] !== 'running') {
      return job;
    }
    await new Promise((resolve) => setTimeout(resolve, 3000));
  }
  return job;
}

describe.skipIf(reason.length > 0)(`the patch stack end to end${needed(reason)}`, () => {
  it('registers a patch and rebuilds the stack', async () => {
    const branch = process.env['PARLEY_SMOKE_PATCH_BRANCH'] ?? 'a-patch';

    const added = await afcRun('patch', 'add', slug, branch, '--if-not-exists');
    expect(added.code, added.stderr).toBe(0);

    const submitted = await call(
      `/api/v1/workspaces/${slug}/rebuild`,
      bearer(apiKey, { method: 'POST' }),
    );
    expect([202, 400, 409]).toContain(submitted.status);
    if (submitted.status !== 202) {
      return;
    }

    const job = await settled(((await submitted.json()) as { id: string }).id);
    expect(job['status'], JSON.stringify(job['error'])).toBe('completed');
    expect(job['patch_results']).toBeDefined();
    expect(String(job['integration_head_sha'])).toMatch(/^[0-9a-f]{40}$/);
  }, 300_000);

  it('absorbs an upstream advance through a sync and its rebuild', async () => {
    const synced = await call(
      `/api/v1/workspaces/${slug}/sync`,
      bearer(apiKey, { method: 'POST' }),
    );
    expect(synced.status).toBe(200);
    const body = (await synced.json()) as Record<string, unknown>;

    expect(body['force_push_detected']).toBeDefined();
    expect(body['patches_merged']).toBeDefined();
    expect(body['sync_error']).toBeNull();

    if (body['rebuild_triggered'] === true) {
      const job = await settled(String(body['rebuild_job_id']));
      expect(['completed', 'failed']).toContain(job['status']);
    }
  }, 300_000);

  it('shows the stack on the dashboard, with counts that agree', async () => {
    const read = await call(`/api/v1/workspaces/${slug}/patch-status`, bearer(apiKey));
    expect(read.status).toBe(200);
    const body = (await read.json()) as {
      patches: unknown[];
      summary: Record<string, number>;
    };

    expect(body.summary['total_patches']).toBe(body.patches.length);
    expect(
      (body.summary['active'] ?? 0) +
        (body.summary['merged_upstream'] ?? 0) +
        (body.summary['conflict'] ?? 0) +
        (body.summary['disabled'] ?? 0),
    ).toBe(body.summary['total_patches']);
  }, 120_000);

  it('lists what it has been told to remember about conflicts', async () => {
    const read = await call(`/api/v1/workspaces/${slug}/rerere`, bearer(apiKey));

    expect(read.status).toBe(200);
    expect(((await read.json()) as { resolutions: unknown[] }).resolutions).toBeInstanceOf(Array);
  }, 120_000);
});
