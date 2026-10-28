import { HttpError } from '../http/errors.js';

/**
 * The opaque cursor a paged route hands back.
 *
 * It is opaque to a client and legible to the service: what it holds
 * is where the last page stopped, so the next page can start exactly
 * there. It is not a row offset, because rows inserted between two
 * requests would shift an offset and a client would see a row twice
 * or not at all.
 *
 * It is signed by nothing. A cursor names a position in a listing the
 * caller is already allowed to read, so forging one buys nothing that
 * asking plainly would not; what it must do is fail loudly when it is
 * not one of ours, rather than returning the wrong rows.
 */
export interface Position {
  /** The sort key of the last row of the page just served. */
  at: string;
  /** Its id, which breaks a tie between rows sharing a sort key. */
  id: string;
}

const PREFIX = 'p1.';

/** Where a page stopped, as a client carries it. */
export function encodeCursor(position: Position): string {
  return `${PREFIX}${Buffer.from(JSON.stringify(position), 'utf8').toString('base64url')}`;
}

/**
 * Where a client says the last page stopped. Anything that is not a
 * cursor this service issued is refused: returning the first page
 * instead would quietly give a client rows it had already seen.
 */
export function decodeCursor(cursor: string): Position {
  const refused = new HttpError(400, 'the cursor is not one this service issued');
  if (!cursor.startsWith(PREFIX)) {
    throw refused;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(Buffer.from(cursor.slice(PREFIX.length), 'base64url').toString('utf8'));
  } catch {
    throw refused;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    throw refused;
  }

  const { at, id } = parsed as Record<string, unknown>;
  if (typeof at !== 'string' || typeof id !== 'string') {
    throw refused;
  }
  return { at, id };
}
