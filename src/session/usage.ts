import { HttpError } from '../http/errors.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { newIdentifier } from '../auth/credentials.js';
import { type SessionRecord, type UsageRecord, toUsage } from './store.js';

/**
 * What a session spent, reported as it goes.
 *
 * A report is only accepted while the session is active: tokens
 * attributed to a run that has finished would move a total somebody
 * has already read.
 */
export interface UsageReport {
  id?: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

const COUNTS = ['input_tokens', 'output_tokens', 'cache_read_tokens'] as const;

export function readUsageBody(raw: unknown): UsageReport {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const model = body['model'];
  if (typeof model !== 'string' || model.length === 0) {
    throw new HttpError(400, 'model is required');
  }

  const report: UsageReport = {
    model,
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    ...(typeof body['id'] === 'string' && body['id'].length > 0 ? { id: body['id'] } : {}),
  };

  for (const name of COUNTS) {
    const value = body[name];
    if (value === undefined) {
      continue;
    }
    if (typeof value !== 'number' || !Number.isInteger(value)) {
      throw new HttpError(400, `${name} must be a whole number`);
    }
    if (value < 0) {
      // Not a malformed request but an impossible one: a count of
      // tokens cannot be less than none.
      throw new HttpError(422, `${name} cannot be negative`);
    }
    report[name] = value;
  }
  return report;
}

export async function recordUsage(
  store: RelationalStore,
  clock: Clock,
  session: SessionRecord,
  report: UsageReport,
): Promise<UsageRecord> {
  if (session.status !== 'active') {
    throw new HttpError(409, 'this session is not active');
  }

  const id = report.id ?? newIdentifier();
  const at = clock.now().toISOString();
  await store.run(
    `INSERT INTO session_usage
       (id, session_id, model, input_tokens, output_tokens, cache_read_tokens, recorded_at)
     VALUES (?, ?, ?, ?, ?, ?, ?)`,
    [
      id,
      session.id,
      report.model,
      report.input_tokens,
      report.output_tokens,
      report.cache_read_tokens,
      at,
    ],
  );

  const rows = await store.all('SELECT * FROM session_usage WHERE id = ?', [id]);
  if (rows[0] === undefined) {
    throw new HttpError(500, 'the usage record could not be read back');
  }
  return toUsage(rows[0]);
}
