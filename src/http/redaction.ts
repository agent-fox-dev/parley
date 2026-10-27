import type { Context } from 'hono';

type RedactingContext = Context<{ Variables: { protectedValues: string[] } }>;

const KEY = 'protectedValues';
export const REDACTED = '[redacted]';

/**
 * Marks a value this request carried as one that may never be written to a
 * log. A route that handles a secret calls it as soon as it has read the
 * value, so a failure anywhere afterwards cannot carry it out.
 */
export function protectValue(c: Context, value: string): void {
  if (value.length === 0) {
    return;
  }
  const held = (c as RedactingContext).get(KEY) ?? [];
  (c as RedactingContext).set(KEY, [...held, value]);
}

/** The text with every value this request protected taken out of it. */
export function redactProtected(c: Context | undefined, text: string): string {
  if (c === undefined) {
    return text;
  }
  let redacted = text;
  for (const value of (c as RedactingContext).get(KEY) ?? []) {
    redacted = redacted.split(value).join(REDACTED);
  }
  return redacted;
}
