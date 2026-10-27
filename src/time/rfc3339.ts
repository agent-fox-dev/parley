import { HttpError } from '../http/errors.js';

/**
 * RFC 3339 with a mandatory offset. JavaScript's own Date parser accepts a
 * good deal more than RFC 3339 — a bare date, a value with no offset, an
 * out-of-range month rolled into the next one — so the shape is checked
 * before the value is read.
 */
const RFC3339 =
  /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;

/** Renders an instant as RFC 3339 in UTC, always ending in Z. */
export function renderTimestamp(instant: Date): string {
  return instant.toISOString().replace(/\.000Z$/, 'Z');
}

/**
 * Reads a client-supplied RFC 3339 timestamp of any offset and returns the
 * instant it names. The field's name goes in the refusal so a caller can
 * tell which of several timestamps was wrong.
 */
export function parseTimestamp(value: string, field: string): Date {
  const match = RFC3339.exec(value);
  if (match === null) {
    throw new HttpError(
      400,
      `${field} must be an RFC 3339 timestamp, for example 2026-10-21T09:00:00Z`,
    );
  }

  const millis = Date.parse(value);
  if (Number.isNaN(millis)) {
    throw new HttpError(
      400,
      `${field} must be an RFC 3339 timestamp, for example 2026-10-21T09:00:00Z`,
    );
  }

  // Date.parse rolls an out-of-range component into the next unit, so a value
  // such as 2026-13-01 would be read as 2027-01-01. Compare the parts back.
  const instant = new Date(millis);
  const [, year, month, day] = match;
  if (value.endsWith('Z') || value.endsWith('z')) {
    const sameYear = instant.getUTCFullYear() === Number(year);
    const sameMonth = instant.getUTCMonth() + 1 === Number(month);
    const sameDay = instant.getUTCDate() === Number(day);
    if (!sameYear || !sameMonth || !sameDay) {
      throw new HttpError(
        400,
        `${field} must be an RFC 3339 timestamp, for example 2026-10-21T09:00:00Z`,
      );
    }
  }

  return instant;
}
