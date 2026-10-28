import type { RelationalStore } from '../platform/index.js';
import type { Row } from '../platform/store.js';

/**
 * One agent run against one workspace, and what it spent.
 *
 * A session is a cost record, not an audit trail: what the agent did
 * belongs elsewhere. What it holds is who opened it, with which
 * credential, how it ended and the tokens it consumed — enough to
 * answer "what did it cost", which cannot be answered after the fact
 * because the tokens are gone unless something counted them.
 */
export type SessionStatus = 'active' | 'completed' | 'failed' | 'timeout' | 'terminated';

export interface SessionRecord {
  id: string;
  workspace_slug: string;
  credential_id: string | null;
  credential_type: string;
  created_by: string | null;
  run_id: string | null;
  node_id: string | null;
  archetype: string | null;
  model: string | null;
  metadata: Record<string, unknown> | null;
  status: SessionStatus;
  error_message: string | null;
  duration_ms: number | null;
  cache_creation_input_tokens: number | null;
  started_at: string;
  ended_at: string | null;
}

export interface UsageRecord {
  id: string;
  session_id: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  recorded_at: string;
}

/** What a session spent, over however many reports it made. */
export interface TokenSummary {
  total_input_tokens: number;
  total_output_tokens: number;
  total_cache_read_tokens: number;
  models_used: string[];
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullable = (value: unknown): string | null => (typeof value === 'string' ? value : null);
const count = (value: unknown): number | null =>
  value === null || value === undefined ? null : Number(value);

export function toSession(row: Row): SessionRecord {
  const metadata = nullable(row['metadata']);
  return {
    id: text(row['id']),
    workspace_slug: text(row['workspace_slug']),
    credential_id: nullable(row['credential_id']),
    credential_type: text(row['credential_type']),
    created_by: nullable(row['created_by']),
    run_id: nullable(row['run_id']),
    node_id: nullable(row['node_id']),
    archetype: nullable(row['archetype']),
    model: nullable(row['model']),
    metadata: metadata === null ? null : (JSON.parse(metadata) as Record<string, unknown>),
    status: text(row['status']) as SessionStatus,
    error_message: nullable(row['error_message']),
    duration_ms: count(row['duration_ms']),
    cache_creation_input_tokens: count(row['cache_creation_input_tokens']),
    started_at: text(row['started_at']),
    ended_at: nullable(row['ended_at']),
  };
}

export function toUsage(row: Row): UsageRecord {
  return {
    id: text(row['id']),
    session_id: text(row['session_id']),
    model: text(row['model']),
    input_tokens: Number(row['input_tokens'] ?? 0),
    output_tokens: Number(row['output_tokens'] ?? 0),
    cache_read_tokens: Number(row['cache_read_tokens'] ?? 0),
    recorded_at: text(row['recorded_at']),
  };
}

export interface NewSession {
  id: string;
  workspace_slug: string;
  credential_id?: string;
  credential_type: string;
  created_by?: string;
  run_id?: string;
  node_id?: string;
  archetype?: string;
  model?: string;
  metadata?: Record<string, unknown>;
}

export async function insertSession(
  store: RelationalStore,
  session: NewSession,
  at: string,
): Promise<void> {
  await store.run(
    `INSERT INTO sessions
       (id, workspace_slug, credential_id, credential_type, created_by, run_id, node_id,
        archetype, model, metadata, status, started_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
    [
      session.id,
      session.workspace_slug,
      session.credential_id ?? null,
      session.credential_type,
      session.created_by ?? null,
      session.run_id ?? null,
      session.node_id ?? null,
      session.archetype ?? null,
      session.model ?? null,
      session.metadata === undefined ? null : JSON.stringify(session.metadata),
      at,
    ],
  );
}

export async function sessionById(
  store: RelationalStore,
  id: string,
): Promise<SessionRecord | undefined> {
  const rows = await store.all('SELECT * FROM sessions WHERE id = ?', [id]);
  return rows[0] === undefined ? undefined : toSession(rows[0]);
}

/** What one session spent, over every record it reported. */
export async function tokenSummary(
  store: RelationalStore,
  sessionId: string,
): Promise<TokenSummary> {
  const rows = await store.all('SELECT * FROM session_usage WHERE session_id = ?', [sessionId]);
  const records = rows.map((row) => toUsage(row));
  return {
    total_input_tokens: records.reduce((total, one) => total + one.input_tokens, 0),
    total_output_tokens: records.reduce((total, one) => total + one.output_tokens, 0),
    total_cache_read_tokens: records.reduce((total, one) => total + one.cache_read_tokens, 0),
    models_used: [...new Set(records.map((one) => one.model))].toSorted(),
  };
}

/** The session as every route that returns one returns it. */
export function serialiseSession(
  session: SessionRecord,
  extra: { token_summary?: TokenSummary } = {},
): Record<string, unknown> {
  return {
    id: session.id,
    workspace_slug: session.workspace_slug,
    credential_id: session.credential_id,
    credential_type: session.credential_type,
    run_id: session.run_id,
    node_id: session.node_id,
    archetype: session.archetype,
    model: session.model,
    metadata: session.metadata,
    status: session.status,
    error_message: session.error_message,
    duration_ms: session.duration_ms,
    cache_creation_input_tokens: session.cache_creation_input_tokens,
    started_at: session.started_at,
    ended_at: session.ended_at,
    ...(extra.token_summary === undefined ? {} : { token_summary: extra.token_summary }),
  };
}
