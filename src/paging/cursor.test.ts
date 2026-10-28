import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { decodeCursor, encodeCursor } from './cursor.js';
import { type Paged, afterClause, page, readPageRequest } from './page.js';

/**
 * The pagination every paged route uses. What it must guarantee is
 * simple to say and easy to get wrong: reading to the end gives every
 * row exactly once, in the order asked for, however the pages fall.
 */
interface Row {
  id: string;
  at: string;
}

const positionOf = (row: Row) => ({ at: row.at, id: row.id });

/** A set of rows, as a store would hold them. */
function rows(count: number): Row[] {
  return Array.from({ length: count }, (_unused, index) => ({
    id: `r${String(index).padStart(3, '0')}`,
    at: new Date(Date.UTC(2026, 0, 1, 0, 0, index)).toISOString(),
  }));
}

/**
 * One page out of a set, the way a store would serve it: ordered,
 * continued from the cursor, and one row longer than asked for.
 */
function serve(
  all: readonly Row[],
  query: string,
  limits = { fallback: 50, most: 500 },
): Paged<Row> {
  const request = readPageRequest(new URLSearchParams(query), limits);
  const ordered = [...all].toSorted((left, right) =>
    request.order === 'asc' ? left.id.localeCompare(right.id) : right.id.localeCompare(left.id),
  );
  const after = request.after;
  const following =
    after === undefined
      ? ordered
      : ordered.filter((row) =>
          request.order === 'asc'
            ? row.at > after.at || (row.at === after.at && row.id > after.id)
            : row.at < after.at || (row.at === after.at && row.id < after.id),
        );
  return page(following.slice(0, request.limit + 1), request, positionOf);
}

describe('a page of a listing', () => {
  it('says there is more until there is not, and continues where it stopped', async () => {
    const all = rows(7);

    const first = serve(all, 'limit=3&order=asc');
    expect(first.rows).toHaveLength(3);
    expect(first.has_more).toBe(true);
    expect(first.next_cursor).not.toBeNull();

    const second = serve(all, `limit=3&order=asc&cursor=${first.next_cursor ?? ''}`);
    expect(second.has_more).toBe(true);
    expect(second.rows[0]?.id).toBe('r003');

    const third = serve(all, `limit=3&order=asc&cursor=${second.next_cursor ?? ''}`);
    expect(third.rows).toHaveLength(1);
    expect(third.has_more).toBe(false);
    expect(third.next_cursor).toBeNull();
  });
});

describe('reading a listing to its end', () => {
  it.each([
    [7, 3, 'asc'],
    [7, 3, 'desc'],
    [1, 1, 'asc'],
    [10, 10, 'asc'],
    [10, 1, 'desc'],
    [0, 5, 'asc'],
    [23, 7, 'desc'],
    [100, 50, 'asc'],
  ])('gives every row once, in order: %i rows, %i a page, %s', (count, size, order) => {
    const all = rows(count);
    const seen: Row[] = [];
    let cursor: string | null = null;

    for (let round = 0; round < count + 2; round += 1) {
      const served: Paged<Row> = serve(
        all,
        `limit=${String(size)}&order=${order}${cursor === null ? '' : `&cursor=${cursor}`}`,
      );
      seen.push(...served.rows);
      cursor = served.next_cursor;
      if (cursor === null) {
        break;
      }
    }

    const wanted = [...all]
      .toSorted((left, right) =>
        order === 'asc' ? left.id.localeCompare(right.id) : right.id.localeCompare(left.id),
      )
      .map((row) => row.id);
    expect(seen.map((row) => row.id)).toEqual(wanted);
    expect(new Set(seen.map((row) => row.id)).size).toBe(seen.length);
  });
});

describe('a cursor this service did not issue', () => {
  const real = encodeCursor({ at: '2026-01-01T00:00:00.000Z', id: 'r001' });

  it.each([
    ['one cut short', real.slice(0, real.length - 4)],
    ['one with a character changed', `${real.slice(0, -1)}${real.endsWith('A') ? 'B' : 'A'}`],
    ['something from another encoding', 'eyJhdCI6IjIwMjYifQ=='],
    ['an empty string with the prefix', 'p1.'],
    ['one somebody encoded by hand', Buffer.from('{"at":"x","id":"y"}').toString('base64url')],
    ['a plain number', '42'],
  ])('is refused: %s', (_what, cursor) => {
    expect(() => decodeCursor(cursor)).toThrow(HttpError);
    expect(() =>
      readPageRequest(new URLSearchParams(`cursor=${cursor}`), {
        fallback: 50,
        most: 500,
      }),
    ).toThrow(/cursor/);
  });

  it('leaves the real one working afterwards', () => {
    expect(decodeCursor(real)).toEqual({ at: '2026-01-01T00:00:00.000Z', id: 'r001' });
  });
});

describe('what a paged route accepts', () => {
  it.each([
    ['a limit that is not a number', 'limit=many'],
    ['a limit below one', 'limit=0'],
    ['a limit past what the route allows', 'limit=501'],
    ['an order nobody has', 'order=sideways'],
  ])('refuses %s', (_what, query) => {
    expect(() => readPageRequest(new URLSearchParams(query), { fallback: 50, most: 500 })).toThrow(
      HttpError,
    );
  });

  it('falls back to the route’s own defaults', () => {
    const request = readPageRequest(new URLSearchParams(''), { fallback: 25, most: 500 });

    expect(request).toEqual({ limit: 25, order: 'desc' });
  });
});

describe('continuing a listing in SQL', () => {
  it('asks for the rows after the cursor, with the id breaking a tie', () => {
    const request = readPageRequest(
      new URLSearchParams(`order=asc&cursor=${encodeCursor({ at: 'T', id: 'i' })}`),
      { fallback: 50, most: 500 },
    );

    const clause = afterClause(request, { at: 'started_at', id: 'id' });

    expect(clause.sql).toContain('started_at > ?');
    expect(clause.sql).toContain('started_at = ?');
    expect(clause.params).toEqual(['T', 'T', 'i']);
  });

  it('asks for nothing extra on a first page', () => {
    const request = readPageRequest(new URLSearchParams(''), { fallback: 50, most: 500 });

    expect(afterClause(request, { at: 'started_at', id: 'id' })).toEqual({ sql: '', params: [] });
  });
});
