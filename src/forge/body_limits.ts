import { HttpError } from '../http/errors.js';
import type { ForgeName } from './target.js';

/**
 * The four fields each forge measures, and what it allows.
 *
 * A body the forge will refuse is cheaper to refuse here, but only
 * these four: measuring a title or a label name would reject what
 * the forge accepts, and a round trip is the lesser cost. The limits
 * are in Unicode code points, so a four-byte emoji counts as one
 * character (ADR 11 records where each figure comes from).
 */
export const BODY_LIMITS: Readonly<Record<ForgeName, number>> = {
  github: 65_536,
  gitlab: 1_000_000,
};

/**
 * The field as the caller gave it, or the refusal the reference
 * gives. It is applied after the forge is known, because the limit
 * depends on which forge it is.
 */
export function checkedBody(
  forge: ForgeName,
  field: string,
  given: string | undefined,
): string | undefined {
  if (given === undefined) {
    return undefined;
  }
  const limit = BODY_LIMITS[forge];
  const length = [...given].length;
  if (length > limit) {
    throw new HttpError(
      413,
      `${field} is ${String(length)} characters; ${forge} accepts at most ${String(limit)}`,
      'body_too_large',
    );
  }
  return given;
}
