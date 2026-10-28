import { HttpError } from '../http/errors.js';
import { newIdentifier } from '../auth/credentials.js';
import type { RelationalStore } from '../platform/index.js';

/**
 * The records an agent reports about one run: audit events, session
 * outcomes, tool calls, tool errors and conversation traces.
 *
 * The five are the same shape of thing — a row with an id the agent
 * may choose, a run, a node, a session and a time — so they are one
 * declaration and one pair of read-and-store functions rather than
 * five of each. What differs between them is the fields they require
 * and the identity they answer with, and that is all that is written
 * out per kind.
 */
export type RecordKind = 'events' | 'outcomes' | 'calls' | 'errors' | 'traces';

export interface RecordShape {
  table: string;
  /** The fields a body must carry, each a non-empty string. */
  required: readonly string[];
  /** The columns the row holds beyond the ones every kind has. */
  columns: readonly string[];
  /** The fields stored as JSON rather than as text. */
  json: readonly string[];
  /** The column the client's timestamp is stored in. */
  time: string;
  /** What the route answers with, beyond the id and the run. */
  identity: readonly string[];
}

export const RECORD_SHAPES: Readonly<Record<RecordKind, RecordShape>> = {
  events: {
    table: 'run_events',
    required: ['event_type'],
    columns: ['event_type', 'severity', 'payload'],
    json: ['payload'],
    time: 'timestamp',
    identity: ['event_type', 'severity', 'created_at'],
  },
  outcomes: {
    table: 'run_session_outcomes',
    required: ['session_id', 'status'],
    columns: ['status', 'duration_ms', 'token_usage'],
    json: ['token_usage'],
    time: 'timestamp',
    identity: ['node_id', 'status', 'created_at'],
  },
  calls: {
    table: 'run_tool_calls',
    required: ['tool_name'],
    columns: ['tool_name', 'duration_ms', 'input', 'output'],
    json: ['input', 'output'],
    time: 'called_at',
    identity: ['tool_name', 'called_at'],
  },
  errors: {
    table: 'run_tool_errors',
    required: ['tool_name', 'error_msg'],
    columns: ['tool_name', 'error_code', 'error_msg'],
    json: [],
    time: 'failed_at',
    identity: ['tool_name', 'failed_at'],
  },
  traces: {
    table: 'run_traces',
    required: ['event_type'],
    columns: ['event_type', 'sequence', 'data'],
    json: ['data'],
    time: 'timestamp',
    identity: ['event_type', 'timestamp'],
  },
};

/** The trace event types the reference names, and no others. */
export const TRACE_TYPES: readonly string[] = [
  'session.init',
  'assistant.message',
  'tool.use',
  'tool.error',
  'session.result',
];

const SEVERITIES: readonly string[] = ['info', 'warning', 'error', 'critical'];

/** The severities the reference gives particular event types. */
const SEVERITY_BY_TYPE: Readonly<Record<string, string>> = {
  'session.fail': 'error',
  'run.limit_reached': 'warning',
  'git.conflict': 'warning',
  'harvest.empty': 'warning',
  'review.parse_failure': 'warning',
};

/** A date, a time and six lowercase hex characters. */
const RUN_ID = /^\d{8}_\d{6}_[0-9a-f]{6}$/;

export function isRunId(value: string): boolean {
  return RUN_ID.test(value);
}

/** The run a route names, refused where it is not one. */
export function checkedRunId(value: string): string {
  if (!isRunId(value)) {
    throw new HttpError(400, 'run_id must be a date, a time and six lowercase hex characters');
  }
  return value;
}

/** One record as it is stored, with every column already settled. */
export interface RunRecord {
  id: string;
  workspace_slug: string;
  run_id: string;
  node_id: string | null;
  session_id: string | null;
  at: string;
  created_at: string;
  values: Record<string, unknown>;
}

function text(body: Record<string, unknown>, name: string): string | undefined {
  const value = body[name];
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== 'string') {
    throw new HttpError(400, `${name} must be a string`);
  }
  return value;
}

function time(body: Record<string, unknown>, name: string, fallback: string): string {
  const given = text(body, name);
  if (given === undefined || given.length === 0) {
    return fallback;
  }
  if (Number.isNaN(Date.parse(given))) {
    throw new HttpError(400, `${name} must be an RFC 3339 time`);
  }
  return new Date(given).toISOString();
}

/**
 * One record read from a request body: every field checked, every
 * default settled. It throws the refusal the reference gives rather
 * than returning a verdict, so a batch's item and a single request
 * are refused for the same reasons in the same words.
 */
export function readRecord(
  kind: RecordKind,
  raw: unknown,
  context: { slug: string; runId: string; now: string },
): RunRecord {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'a record must be a JSON object');
  }
  const body = raw as Record<string, unknown>;
  const shape = RECORD_SHAPES[kind];

  for (const field of shape.required) {
    const value = body[field];
    if (typeof value !== 'string' || value.length === 0) {
      throw new HttpError(400, `${field} is required`);
    }
  }

  // A body may name its run, and then it must be the run of the URL:
  // an agent reporting one run's work under another's would be worse
  // than a refusal.
  const named = text(body, 'run_id');
  if (named !== undefined && named !== context.runId) {
    throw new HttpError(400, 'run_id in the body differs from the one in the path');
  }

  const values: Record<string, unknown> = {};
  for (const column of shape.columns) {
    const value = body[column];
    values[column] = shape.json.includes(column)
      ? value === undefined
        ? null
        : JSON.stringify(value)
      : (value ?? null);
  }

  if (kind === 'events') {
    const severity = text(body, 'severity');
    if (severity !== undefined && !SEVERITIES.includes(severity)) {
      throw new HttpError(400, `severity must be one of ${SEVERITIES.join(', ')}`);
    }
    values['severity'] = severity ?? SEVERITY_BY_TYPE[String(body['event_type'] ?? '')] ?? 'info';
  }
  if (kind === 'traces' && !TRACE_TYPES.includes(String(body['event_type']))) {
    throw new HttpError(400, `event_type must be one of ${TRACE_TYPES.join(', ')}`);
  }
  if (kind === 'outcomes' || kind === 'calls') {
    const duration = body['duration_ms'];
    if (duration !== undefined && duration !== null) {
      if (typeof duration !== 'number' || !Number.isInteger(duration) || duration < 0) {
        throw new HttpError(
          400,
          'duration_ms must be a whole number of milliseconds, not negative',
        );
      }
    }
  }

  return {
    id: text(body, 'id') ?? newIdentifier(),
    workspace_slug: context.slug,
    run_id: context.runId,
    node_id: text(body, 'node_id') ?? null,
    session_id: text(body, 'session_id') ?? null,
    // Every kind takes the client's time in `timestamp`; which column
    // it is stored in is the shape's business, not the client's.
    at: time(body, 'timestamp', context.now),
    created_at: context.now,
    values,
  };
}

/**
 * Stores one record, or reports that the run already holds it. The id
 * is the whole test: an agent that retries a request it never saw
 * answered sends the same id, and sending it twice must not count the
 * work twice.
 */
export async function storeRecord(
  store: RelationalStore,
  kind: RecordKind,
  record: RunRecord,
): Promise<'stored' | 'duplicate'> {
  const shape = RECORD_SHAPES[kind];
  const held = await store.all<{ id: string }>(`SELECT id FROM ${shape.table} WHERE id = ?`, [
    record.id,
  ]);
  if (held.length > 0) {
    return 'duplicate';
  }

  const columns = [
    'id',
    'workspace_slug',
    'run_id',
    'node_id',
    'session_id',
    shape.time,
    'created_at',
    ...shape.columns,
  ];
  const values = [
    record.id,
    record.workspace_slug,
    record.run_id,
    record.node_id,
    record.session_id,
    record.at,
    record.created_at,
    ...shape.columns.map((column) => record.values[column] ?? null),
  ];

  await store.run(
    `INSERT INTO ${shape.table} (${columns.join(', ')})
     VALUES (${columns.map(() => '?').join(', ')})`,
    values,
  );
  return 'stored';
}

/** What a route answers with once a record is stored or found. */
export async function identityOfRecord(
  store: RelationalStore,
  kind: RecordKind,
  id: string,
): Promise<Record<string, unknown>> {
  const shape = RECORD_SHAPES[kind];
  const wanted = ['id', 'run_id', ...shape.identity];
  const rows = await store.all<Record<string, unknown>>(
    `SELECT ${wanted.join(', ')} FROM ${shape.table} WHERE id = ?`,
    [id],
  );
  return rows[0] ?? { id };
}
