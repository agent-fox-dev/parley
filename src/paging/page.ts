import { HttpError } from '../http/errors.js';
import { type Position, decodeCursor, encodeCursor } from './cursor.js';

/**
 * Serving one page of a listing.
 *
 * Every paged route asks the same three questions — how many rows,
 * in which direction, and from where — and answers the same three
 * fields, so they are asked and answered once here. The rows
 * themselves are the route's business.
 */
export interface PageRequest {
  limit: number;
  order: 'asc' | 'desc';
  after?: Position;
}

export interface Paged<T> {
  rows: T[];
  next_cursor: string | null;
  has_more: boolean;
}

/** What the query string asks for, within what the route allows. */
export function readPageRequest(
  query: URLSearchParams,
  limits: { fallback: number; most: number },
): PageRequest {
  const asked = query.get('limit');
  let limit = limits.fallback;
  if (asked !== null) {
    const wanted = Number(asked);
    if (!Number.isInteger(wanted) || wanted < 1) {
      throw new HttpError(400, 'limit must be a whole number of at least 1');
    }
    if (wanted > limits.most) {
      throw new HttpError(400, `limit must be no more than ${String(limits.most)}`);
    }
    limit = wanted;
  }

  const order = query.get('order') ?? 'desc';
  if (order !== 'asc' && order !== 'desc') {
    throw new HttpError(400, 'order must be asc or desc');
  }

  const cursor = query.get('cursor');
  return {
    limit,
    order,
    ...(cursor === null || cursor.length === 0 ? {} : { after: decodeCursor(cursor) }),
  };
}

/**
 * Turns one row more than was asked for into a page. Asking for one
 * extra is how a listing knows whether there is another page without
 * counting the whole table.
 */
export function page<T>(
  rows: readonly T[],
  request: PageRequest,
  positionOf: (row: T) => Position,
): Paged<T> {
  const has_more = rows.length > request.limit;
  const kept = has_more ? rows.slice(0, request.limit) : [...rows];
  const last = kept.at(-1);
  return {
    rows: kept,
    next_cursor: has_more && last !== undefined ? encodeCursor(positionOf(last)) : null,
    has_more,
  };
}

/**
 * The SQL that continues a listing from where a cursor stopped, with
 * the id breaking a tie between rows sharing a sort key.
 */
export function afterClause(
  request: PageRequest,
  columns: { at: string; id: string },
): { sql: string; params: unknown[] } {
  if (request.after === undefined) {
    return { sql: '', params: [] };
  }
  const compare = request.order === 'asc' ? '>' : '<';
  return {
    sql: ` AND (${columns.at} ${compare} ? OR (${columns.at} = ? AND ${columns.id} ${compare} ?))`,
    params: [request.after.at, request.after.at, request.after.id],
  };
}
