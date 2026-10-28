import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, it } from 'vitest';
import { afc, apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * Merging and batch rebasing against a running deployment, driven by
 * the commands a person runs. A merge is a queued job, so the suite
 * follows it to a settled state rather than assuming it ran.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const target = process.env['PARLEY_SMOKE_TARGET_BRANCH'] ?? 'main';
const source = process.env['PARLEY_SMOKE_SOURCE_BRANCH'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
  ['PARLEY_SMOKE_SOURCE_BRANCH', source],
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

/** Follows a merge job until it settles, and answers what it reached. */
async function settled(id: string): Promise<Record<string, unknown>> {
  const deadline = Date.now() + 180_000;
  let job: Record<string, unknown> = {};
  while (Date.now() < deadline) {
    const read = await call(`/api/v1/workspaces/${slug}/merges/${id}`, bearer(apiKey));
    job = (await read.json()) as Record<string, unknown>;
    if (job['status'] !== 'queued' && job['status'] !== 'running') {
      return job;
    }
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
  return job;
}

describe.skipIf(reason.length > 0)(`merging end to end${needed(reason)}`, () => {
  it('queues a merge, runs it and records what it did', async () => {
    const submitted = await call(
      `/api/v1/workspaces/${slug}/merges`,
      bearer(apiKey, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ target_branch: target, source_ref: source }),
      }),
    );
    expect([202, 409]).toContain(submitted.status);
    if (submitted.status !== 202) {
      return;
    }

    const job = await settled(((await submitted.json()) as { id: string }).id);
    expect(['completed', 'failed']).toContain(job['status']);

    if (job['status'] === 'completed') {
      expect(String(job['base_sha'])).toMatch(/^[0-9a-f]{40}$/);
      expect(String(job['merged_sha'])).toMatch(/^[0-9a-f]{40}$/);
      expect(job['conflict_files']).toEqual([]);
    } else {
      // A conflict carries the engine's own list of paths.
      expect(job['conflict_files']).toBeInstanceOf(Array);
    }
  }, 300_000);

  it('lists the workspace’s merges through afc', async () => {
    const listed = await afcRun('merge', 'list', slug);

    expect(listed.code, listed.stderr).toBe(0);
  }, 120_000);

  it('rebases several branches at once and answers for each', async () => {
    const response = await call(
      `/api/v1/workspaces/${slug}/rebase`,
      bearer(apiKey, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ target_ref: target, branches: [source] }),
      }),
    );
    expect([200, 409]).toContain(response.status);
    if (response.status !== 200) {
      return;
    }

    const results = ((await response.json()) as { results: Array<Record<string, unknown>> })
      .results;
    expect(results).toHaveLength(1);
    expect(['ok', 'conflict', 'error']).toContain(results[0]?.['status']);
  }, 300_000);
});
