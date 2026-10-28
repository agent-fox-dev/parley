import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { checkedRunId } from '../audit/run_records.js';
import { type PostmortemRouteConfig, readableRun } from './postmortem.js';

/**
 * One node's conversation, rebuilt from its trace events.
 *
 * A run's traces are several conversations interleaved, so the
 * transcript names a node and a request without one is refused
 * rather than answered with a conversation nobody had. Four trace
 * types are messages and the rest are not, which is why the mapping
 * is a table: a type nobody mapped produces no message instead of a
 * message with an invented role.
 */
export type TranscriptConfig = PostmortemRouteConfig;

/** The trace types that are messages, and the role each becomes. */
const ROLES: Readonly<Record<string, string>> = {
  'session.init': 'system',
  'assistant.message': 'assistant',
  'tool.use': 'tool_use',
  'tool.error': 'tool_error',
};

interface StoredTrace {
  [column: string]: unknown;
  event_type: string;
  timestamp: string;
  data: string | null;
}

/** What the agent put in the trace's data, where it put anything. */
function contentsOf(trace: StoredTrace): { content: string; tool_name: string | null } {
  const data = (
    trace.data === null ? {} : (JSON.parse(trace.data) as Record<string, unknown>)
  ) as Record<string, unknown>;
  const content = data['content'] ?? data['message'] ?? data['text'];
  const tool = data['tool_name'] ?? data['tool'];
  return {
    content: typeof content === 'string' ? content : '',
    tool_name: typeof tool === 'string' ? tool : null,
  };
}

async function transcript(c: Context, config: TranscriptConfig): Promise<Response> {
  const slug = await readableRun(c, config);
  const runId = checkedRunId(c.req.param('run_id') ?? '');

  const node = new URL(c.req.url).searchParams.get('node_id') ?? '';
  if (node.length === 0) {
    throw new HttpError(400, 'node_id is required');
  }

  const traces = await config.store.all<StoredTrace>(
    `SELECT event_type, timestamp, data FROM run_traces
      WHERE workspace_slug = ? AND run_id = ? AND node_id = ?
      ORDER BY timestamp, sequence, id`,
    [slug, runId, node],
  );

  return jsonResponse({
    run_id: runId,
    node_id: node,
    messages: traces
      .filter((trace) => ROLES[trace.event_type] !== undefined)
      .map((trace) => ({
        role: ROLES[trace.event_type],
        ...contentsOf(trace),
        timestamp: trace.timestamp,
      })),
  });
}

export function transcriptRoutes(config: TranscriptConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/runs/:run_id/transcript`,
      scope: 'audit:read',
      handler: async (c) => transcript(c, config),
    },
  ];
}
