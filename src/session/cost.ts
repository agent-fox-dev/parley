import { HttpError } from '../http/errors.js';
import type { Clock, RelationalStore } from '../platform/index.js';

/**
 * What a workspace's agents have spent over a period.
 *
 * The totals and the breakdown are computed from the same rows, so
 * the two cannot disagree — which matters because the breakdown is
 * what a person reads and the totals are what they quote.
 *
 * The routes report tokens and not money: the reference defines no
 * currency, and a service that invented one would be guessing at
 * somebody's contract.
 */
export type Grouping = 'day' | 'session' | 'model';

export interface CostPeriod {
  since: string;
  until: string;
}

export interface CostTotals {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  sessions: number;
}

export interface CostRow extends CostTotals {
  date?: string;
  session_id?: string;
  model?: string;
}

export interface CostReport {
  workspace: string;
  period: CostPeriod;
  totals: CostTotals;
  breakdown: CostRow[];
}

const GROUPINGS: readonly Grouping[] = ['day', 'session', 'model'];

/** The period and grouping a request asks for, within what is allowed. */
export function readCostQuery(
  query: URLSearchParams,
  clock: Clock,
): { group_by: Grouping; period: CostPeriod } {
  const grouping = query.get('group_by') ?? 'day';
  if (!GROUPINGS.includes(grouping as Grouping)) {
    throw new HttpError(400, `group_by must be one of ${GROUPINGS.join(', ')}`);
  }

  const now = clock.now();
  const until = query.get('until') ?? now.toISOString();
  const since = query.get('since') ?? new Date(now.getTime() - 24 * 60 * 60 * 1000).toISOString();

  for (const [name, value] of [
    ['since', since],
    ['until', until],
  ] as const) {
    if (Number.isNaN(Date.parse(value))) {
      throw new HttpError(400, `${name} must be a time`);
    }
  }
  if (Date.parse(since) >= Date.parse(until)) {
    throw new HttpError(400, 'since must be before until');
  }

  return { group_by: grouping as Grouping, period: { since, until } };
}

/**
 * Aggregates a workspace's usage over the period. The grouping
 * decides what a row is; the arithmetic is the same either way, so
 * the rows and the totals are summed from one list of records.
 */
export async function costOf(
  store: RelationalStore,
  slug: string,
  asked: { group_by: Grouping; period: CostPeriod },
): Promise<CostReport> {
  const rows = await store.all<{
    session_id: string;
    model: string;
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    recorded_at: string;
  }>(
    `SELECT u.session_id, u.model, u.input_tokens, u.output_tokens, u.cache_read_tokens,
            u.recorded_at
       FROM session_usage u
       JOIN sessions s ON s.id = u.session_id
      WHERE s.workspace_slug = ? AND u.recorded_at >= ? AND u.recorded_at < ?`,
    [slug, asked.period.since, asked.period.until],
  );

  const keyOf = (row: (typeof rows)[number]) =>
    asked.group_by === 'session'
      ? row.session_id
      : asked.group_by === 'model'
        ? row.model
        : row.recorded_at.slice(0, 10);

  const grouped = new Map<string, { totals: CostTotals; sessions: Set<string> }>();
  for (const row of rows) {
    const key = keyOf(row);
    const held = grouped.get(key) ?? {
      totals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, sessions: 0 },
      sessions: new Set<string>(),
    };
    held.totals.input_tokens += Number(row.input_tokens);
    held.totals.output_tokens += Number(row.output_tokens);
    held.totals.cache_read_tokens += Number(row.cache_read_tokens);
    held.sessions.add(row.session_id);
    grouped.set(key, held);
  }

  const named = (key: string): Partial<CostRow> =>
    asked.group_by === 'session'
      ? { session_id: key }
      : asked.group_by === 'model'
        ? { model: key }
        : { date: key };

  const breakdown = [...grouped]
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([key, held]) => ({
      ...named(key),
      ...held.totals,
      sessions: held.sessions.size,
    })) as CostRow[];

  return {
    workspace: slug,
    period: asked.period,
    totals: {
      input_tokens: breakdown.reduce((sum, one) => sum + one.input_tokens, 0),
      output_tokens: breakdown.reduce((sum, one) => sum + one.output_tokens, 0),
      cache_read_tokens: breakdown.reduce((sum, one) => sum + one.cache_read_tokens, 0),
      // Counted over the period rather than summed across rows: one
      // session that spans two days is one session, not two.
      sessions: new Set(rows.map((row) => row.session_id)).size,
    },
    breakdown,
  };
}
