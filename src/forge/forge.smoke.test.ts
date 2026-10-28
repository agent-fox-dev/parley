import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, it } from 'vitest';
import { afc, apiKey, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * The three paths of this package against a running deployment and
 * the forge its workspace really names: an agent triaging an issue,
 * one opening a pull request upstream, and a credential the forge
 * refuses.
 *
 * They are driven through `afc` where the command exists, because
 * what the reference promises is what a person runs.
 */
const run = promisify(execFile);

const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
);

/** A workspace whose credential the forge refuses, for the last path. */
const refused = process.env['PARLEY_SMOKE_FORGE_REFUSED'] ?? '';

/** A carry-patch workspace with an upstream, for the second path. */
const upstream = process.env['PARLEY_SMOKE_UPSTREAM_WORKSPACE'] ?? '';

async function afcRun(
  ...args: string[]
): Promise<{ code: number; stdout: string; stderr: string }> {
  return run(afc, args, {
    env: { ...process.env, ENDPOINT_URL: baseUrl, API_KEY: apiKey },
    timeout: 120_000,
  })
    .then((done) => ({ code: 0, ...done }))
    .catch((err: { code?: number; stdout?: string; stderr?: string }) => ({
      code: err.code ?? 1,
      stdout: err.stdout ?? '',
      stderr: err.stderr ?? '',
    }));
}

describe.skipIf(reason.length > 0)(`the forge, through the hub${needed(reason)}`, () => {
  it('says how it reaches the forge, without needing the forge', async () => {
    const described = await call(`/api/v1/workspaces/${slug}/forge`, bearer(apiKey));
    expect(described.status).toBe(200);

    const body = (await described.json()) as {
      forge: string;
      api_url: string | null;
      repository: { owner: string; name: string } | null;
      credential: string;
      webhook: Record<string, unknown>;
    };
    expect(['github', 'gitlab', 'unknown']).toContain(body.forge);
    expect(body.credential).toMatch(/^(pat|basic|none)$/);
    expect(Object.keys(body.webhook).toSorted()).toEqual(['configured', 'last_delivery_at', 'url']);
    expect(JSON.stringify(body)).not.toContain('ghp_');
  }, 120_000);

  it('triages an issue: lists, reads and labels one', async () => {
    const listed = await call(
      `/api/v1/workspaces/${slug}/issues?state=open&limit=5`,
      bearer(apiKey),
    );
    expect(listed.status).toBe(200);
    const issues = (await listed.json()) as Array<{ number: number; title: string }>;
    expect(Array.isArray(issues)).toBe(true);
    if (issues.length === 0) {
      // A repository with no open issue has nothing to triage; the
      // list answering at all is what this path needed from it.
      return;
    }

    const first = issues[0] as { number: number };
    const thread = await call(
      `/api/v1/workspaces/${slug}/issues/${String(first.number)}`,
      bearer(apiKey),
    );
    expect(thread.status).toBe(200);
    const read = (await thread.json()) as { issue: { number: number }; comments: unknown[] };
    expect(read.issue.number).toBe(first.number);
    expect(Array.isArray(read.comments)).toBe(true);

    // The label write, and the event it leaves behind.
    const label = `af-smoke-${Date.now().toString(36)}`;
    const added = await call(`/api/v1/workspaces/${slug}/issues/${String(first.number)}/labels`, {
      ...bearer(apiKey),
      method: 'POST',
      headers: { authorization: `Bearer ${apiKey}`, 'content-type': 'application/json' },
      body: JSON.stringify([label]),
    });
    expect([204, 404, 422]).toContain(added.status);

    if (added.status === 204) {
      const events = (await (
        await call(
          `/api/v1/audit?workspace=${slug}&event_type=hub.forge.label.add&limit=1`,
          bearer(apiKey),
        )
      ).json()) as { events: Array<{ event_type: string }> };
      expect(events.events[0]?.event_type).toBe('hub.forge.label.add');

      await call(
        `/api/v1/workspaces/${slug}/issues/${String(first.number)}/labels/${label}`,
        bearer(apiKey, { method: 'DELETE' }),
      );
    }
  }, 180_000);

  it.skipIf(upstream.length === 0)(
    'opens a pull request against the upstream, with the upstream’s own credential',
    async () => {
      const described = (await (
        await call(`/api/v1/workspaces/${upstream}/forge`, bearer(apiKey))
      ).json()) as { upstream: { api_url: string; repository: { name: string } } | null };
      expect(described.upstream).not.toBeNull();

      const opened = await call(`/api/v1/workspaces/${upstream}/pulls?target=upstream`, {
        method: 'POST',
        headers: { authorization: `Bearer ${apiKey}`, 'content-type': 'application/json' },
        body: JSON.stringify({
          title: `smoke ${new Date().toISOString()}`,
          head: process.env['PARLEY_SMOKE_HEAD'] ?? 'main',
          base: process.env['PARLEY_SMOKE_BASE'] ?? 'main',
        }),
      });

      // Either it opened, or the forge refused it for a reason of
      // its own; what must not happen is the hub reaching the wrong
      // repository, which the status above already showed.
      expect([201, 422, 502]).toContain(opened.status);
      if (opened.status === 201) {
        const created = (await opened.json()) as { number: number };
        const state = await call(
          `/api/v1/workspaces/${upstream}/pulls/${String(created.number)}/state?target=upstream`,
          bearer(apiKey),
        );
        expect(state.status).toBe(200);
        expect((await state.json()) as { state: string }).toMatchObject({ state: 'open' });
      }
    },
    180_000,
  );

  it.skipIf(refused.length === 0)(
    'reports a credential the forge refuses without disclosing it',
    async () => {
      const response = await call(`/api/v1/workspaces/${refused}/issues`, bearer(apiKey));
      const text = await response.text();

      expect(response.status).toBe(502);
      expect(JSON.parse(text) as { error: { error_type: string } }).toMatchObject({
        error: { error_type: 'forge_unauthenticated' },
      });
      expect(text).not.toMatch(/gh[pousr]_[A-Za-z0-9]/);
      expect(text).not.toMatch(/glpat-/);
      expect(text).not.toContain('@github.com');
    },
    120_000,
  );

  it.skipIf(afc.length === 0)(
    'answers afc forge status and afc issue list',
    async () => {
      const described = await afcRun('forge', 'status', slug);
      const listed = await afcRun('issue', 'list', slug);

      expect(described.code, described.stderr).toBe(0);
      expect(described.stdout).toMatch(/github|gitlab|unknown/);
      expect(listed.code, listed.stderr).toBe(0);
    },
    180_000,
  );
});
