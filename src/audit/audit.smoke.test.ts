import { describe, expect, it } from 'vitest';
import { apiKey, asJson, baseUrl, bearer, call, missing, needed } from '../testutil/smoke.js';

/**
 * The three paths of this package end to end against a running
 * deployment: an operation remembered and found again, a run
 * reported and read back, and a watcher that sees an event arrive.
 */
const slug = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', slug],
);

const digits = (value: number, width: number) => String(value).padStart(width, '0');

/** A run id of this run alone, in the shape the routes demand. */
function runId(): string {
  const now = new Date();
  const stamp = `${String(now.getUTCFullYear())}${digits(now.getUTCMonth() + 1, 2)}${digits(now.getUTCDate(), 2)}_${digits(now.getUTCHours(), 2)}${digits(now.getUTCMinutes(), 2)}${digits(now.getUTCSeconds(), 2)}`;
  const tail = Math.floor(Math.random() * 0xff_ff_ff)
    .toString(16)
    .padStart(6, '0');
  return `${stamp}_${tail}`;
}

describe.skipIf(reason.length > 0)(`the audit trail${needed(reason)}`, () => {
  it('remembers an operation and finds it again through the query', async () => {
    // A workspace of this run's own, so the archive is ours to make.
    const workspace = `smoke-audit-${Date.now().toString(36)}`;
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
      const archived = await call(
        `/api/v1/workspaces/${workspace}/archive`,
        bearer(apiKey, { method: 'POST' }),
      );
      expect(archived.status).toBe(200);

      const read = await call(
        `/api/v1/audit?workspace=${workspace}&event_type=hub.workspace.archive`,
        bearer(apiKey),
      );
      expect(read.status).toBe(200);

      const body = (await read.json()) as {
        events: Array<{
          event_type: string;
          source: string;
          workspace: string;
          actor_type: string;
          action: string;
        }>;
      };
      expect(body.events).toHaveLength(1);
      expect(body.events[0]).toMatchObject({
        event_type: 'hub.workspace.archive',
        source: 'hub',
        workspace,
        action: 'archive',
      });
      expect(body.events[0]?.actor_type).not.toBe('');
    } finally {
      await call(`/api/v1/workspaces/${workspace}/archive`, bearer(apiKey, { method: 'POST' }));
      await call(`/api/v1/workspaces/${workspace}`, bearer(apiKey, { method: 'DELETE' }));
    }
  }, 180_000);

  it('ingests a run, counts the repeats, and reads it back as a conversation', async () => {
    const run = runId();
    const node = 'node-1';
    const traces = [
      { id: `${run}-1`, event_type: 'session.init', node_id: node, data: { content: 'start' } },
      {
        id: `${run}-2`,
        event_type: 'assistant.message',
        node_id: node,
        data: { content: 'thinking' },
      },
      {
        id: `${run}-3`,
        event_type: 'tool.use',
        node_id: node,
        data: { tool_name: 'bash', content: 'ls' },
      },
    ];

    const first = await call(
      `/api/v1/workspaces/${slug}/runs/${run}/traces/batch`,
      asJson('POST', traces, apiKey),
    );
    expect(first.status).toBe(200);
    expect((await first.json()) as { accepted: number }).toMatchObject({ accepted: 3 });

    // The same batch again, plus one that cannot be read.
    const again = await call(
      `/api/v1/workspaces/${slug}/runs/${run}/traces/batch`,
      asJson('POST', [...traces, { event_type: 'not.a.trace' }], apiKey),
    );
    const report = (await again.json()) as {
      accepted: number;
      duplicates: number;
      errors: Array<{ index: number }>;
    };
    expect([report.accepted, report.duplicates, report.errors.length]).toEqual([0, 3, 1]);

    const postmortem = await call(
      `/api/v1/workspaces/${slug}/runs/${run}/postmortem`,
      asJson(
        'POST',
        {
          run_status: 'stalled',
          started_at: new Date(Date.now() - 60_000).toISOString(),
          completed_at: new Date().toISOString(),
          task_summary: { total: 1 },
          cost_summary: { input_tokens: 10 },
        },
        apiKey,
      ),
    );
    expect(postmortem.status).toBe(201);
    const second = await call(
      `/api/v1/workspaces/${slug}/runs/${run}/postmortem`,
      asJson(
        'POST',
        {
          run_status: 'cost_limit',
          started_at: new Date().toISOString(),
          completed_at: new Date().toISOString(),
          task_summary: { total: 9 },
          cost_summary: {},
        },
        apiKey,
      ),
    );
    expect(second.status).toBe(200);

    const listed = (await (
      await call(`/api/v1/workspaces/${slug}/runs/${run}/traces`, bearer(apiKey))
    ).json()) as { traces: Array<{ id: string }> };
    expect(listed.traces).toHaveLength(3);

    const read = (await (
      await call(`/api/v1/workspaces/${slug}/runs/${run}/postmortem`, bearer(apiKey))
    ).json()) as { run_status: string };
    // The first account of the run, not the second.
    expect(read.run_status).toBe('stalled');

    const transcript = (await (
      await call(
        `/api/v1/workspaces/${slug}/runs/${run}/transcript?node_id=${node}`,
        bearer(apiKey),
      )
    ).json()) as { messages: Array<{ role: string; tool_name: string | null }> };
    expect(transcript.messages.map((one) => one.role)).toEqual(['system', 'assistant', 'tool_use']);
    expect(transcript.messages.at(-1)?.tool_name).toBe('bash');
  }, 180_000);

  it('delivers an event to a watcher that was already listening', async () => {
    const opened = await call(`/api/v1/events?workspace=${slug}`, bearer(apiKey));
    expect(opened.status).toBe(200);
    expect(opened.headers.get('content-type')).toContain('text/event-stream');

    const reader = (opened.body as ReadableStream<Uint8Array>).getReader();
    const decoder = new TextDecoder();
    const read = async (within: number): Promise<string> => {
      const next = await Promise.race([
        reader.read(),
        new Promise<undefined>((settled) => {
          setTimeout(() => {
            settled(undefined);
          }, within);
        }),
      ]);
      return next === undefined || next.done ? '' : decoder.decode(next.value);
    };

    try {
      // The heartbeat comes first, before anything has happened.
      expect(await read(10_000)).toContain('event: heartbeat');

      // Then an operation records an event — on whichever component
      // of the deployment answers this request.
      const stored = await call(
        `/api/v1/workspaces/${slug}/vars`,
        asJson(
          'POST',
          { entries: [{ key: `SMOKE_${Date.now().toString(36)}`, value: 'v' }] },
          apiKey,
        ),
      );
      expect(stored.status).toBe(201);

      let carried = '';
      for (let look = 0; look < 30 && !carried.includes('audit_event'); look += 1) {
        carried += await read(2000);
      }
      expect(carried).toContain('event: audit_event');
      expect(carried).toContain('hub.variable.create');
    } finally {
      await reader.cancel();
    }
  }, 180_000);
});
