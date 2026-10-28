import { describe, expect, it } from 'vitest';
import {
  apiKey,
  asJson,
  baseUrl,
  bearer,
  call,
  missing,
  needed,
  uniqueSlug,
} from '../testutil/smoke.js';

/**
 * One agent run recorded end to end against a running deployment:
 * open, report, complete, read back. The question the package exists
 * to answer is "what did it cost", so the suite asks it.
 */
const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
);

describe.skipIf(reason.length > 0)(`an agent run, recorded${needed(reason)}`, () => {
  it('opens, reports twice, completes, and reads back what it spent', async () => {
    const run = uniqueSlug('run');

    const opened = await call(
      '/api/v1/sessions',
      asJson('POST', { workspace_slug: slug, run_id: run, model: 'a-model' }, apiKey),
    );
    expect(opened.status).toBe(201);
    const session = (await opened.json()) as { id: string; status: string };
    expect(session.status).toBe('active');

    for (const tokens of [
      { model: 'a-model', input_tokens: 300, output_tokens: 120 },
      { model: 'a-model', input_tokens: 200, output_tokens: 80, cache_read_tokens: 30 },
    ]) {
      const reported = await call(
        `/api/v1/sessions/${session.id}/usage`,
        asJson('POST', tokens, apiKey),
      );
      expect(reported.status).toBe(201);
    }

    const completed = await call(
      `/api/v1/sessions/${session.id}/complete`,
      asJson('POST', { status: 'completed', duration_ms: 1234 }, apiKey),
    );
    expect(completed.status).toBe(200);
    expect(((await completed.json()) as { status: string }).status).toBe('completed');

    const read = (await (await call(`/api/v1/sessions/${session.id}`, bearer(apiKey))).json()) as {
      token_summary: Record<string, number>;
    };
    expect(read.token_summary['total_input_tokens']).toBe(500);
    expect(read.token_summary['total_output_tokens']).toBe(200);

    const usage = (await (
      await call(`/api/v1/sessions/${session.id}/usage`, bearer(apiKey))
    ).json()) as { records: unknown[]; totals: Record<string, number> };
    expect(usage.records.length).toBeGreaterThanOrEqual(2);
    expect(usage.totals['total_input_tokens']).toBe(500);

    // And the run is findable among the workspace's sessions.
    const listed = (await (
      await call(`/api/v1/sessions?workspace_slug=${slug}&run_id=${run}`, bearer(apiKey))
    ).json()) as { sessions: Array<{ id: string }> };
    expect(listed.sessions.map((one) => one.id)).toContain(session.id);
  }, 120_000);

  it('ends a workspace’s running session when the workspace is retired', async () => {
    const workspace = uniqueSlug('session-archive');
    const registered = await call(
      '/api/v1/workspaces',
      asJson(
        'POST',
        {
          slug: workspace,
          git_url: 'https://github.com/agent-fox-dev/parley.git',
          git_pat: 'ghp_SMOKE_SENTINEL',
        },
        apiKey,
      ),
    );
    expect(registered.status).toBe(201);

    try {
      const opened = await call(
        '/api/v1/sessions',
        asJson('POST', { workspace_slug: workspace, model: 'a-model' }, apiKey),
      );
      expect(opened.status).toBe(201);
      const session = (await opened.json()) as { id: string };

      const archived = await call(
        `/api/v1/workspaces/${workspace}/archive`,
        bearer(apiKey, { method: 'POST' }),
      );
      expect(archived.status).toBe(200);

      // The session is out of reach while the workspace is archived,
      // and the refusal says why rather than hiding the workspace.
      const refused = await call(`/api/v1/sessions/${session.id}`, bearer(apiKey));
      expect(refused.status).toBe(403);
      expect(JSON.stringify(await refused.json())).toContain('workspace access denied');

      // Reactivating is how the suite reads the end state: it puts the
      // workspace back in reach without putting a session back in it.
      await call(`/api/v1/workspaces/${workspace}/reactivate`, bearer(apiKey, { method: 'POST' }));
      const read = (await (
        await call(`/api/v1/sessions/${session.id}`, bearer(apiKey))
      ).json()) as { status: string };
      expect(read.status).toBe('terminated');

      const left = (await (
        await call(`/api/v1/sessions?workspace_slug=${workspace}&status=active`, bearer(apiKey))
      ).json()) as { sessions: unknown[] };
      expect(left.sessions).toHaveLength(0);
    } finally {
      await call(`/api/v1/workspaces/${workspace}/archive`, bearer(apiKey, { method: 'POST' }));
      await call(`/api/v1/workspaces/${workspace}`, bearer(apiKey, { method: 'DELETE' }));
    }
  }, 180_000);
});
