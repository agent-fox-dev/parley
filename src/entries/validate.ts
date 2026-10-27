import { HttpError } from '../http/errors.js';

export interface EntryInput {
  key: string;
  value: string;
}

export const MAX_KEY_LENGTH = 255;
export const MAX_VALUE_BYTES = 262_144;
export const MAX_ENTRIES_PER_SCOPE = 100;

const KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Letters, digits and underscores, never starting with a digit. */
export function isValidKey(key: string): boolean {
  return key.length > 0 && key.length <= MAX_KEY_LENGTH && KEY.test(key);
}

/**
 * The form a key is matched by. Keys are stored as they were given and
 * compared folded, so MY_KEY and my_key are one entry.
 */
export function foldKey(key: string): string {
  return key.toLowerCase();
}

export function checkedKey(key: unknown): string {
  if (typeof key !== 'string' || !isValidKey(key)) {
    throw new HttpError(
      400,
      `a key must be letters, digits and underscores, not start with a digit, and be at most ${String(MAX_KEY_LENGTH)} characters`,
    );
  }
  return key;
}

export function checkedValue(value: unknown): string {
  if (typeof value !== 'string') {
    throw new HttpError(400, 'value is required and must be a string');
  }
  // The limit is on the bytes stored, not on the characters typed.
  if (new TextEncoder().encode(value).length > MAX_VALUE_BYTES) {
    throw new HttpError(400, `a value may be at most ${String(MAX_VALUE_BYTES)} bytes`);
  }
  return value;
}

/** The entries a create names, each validated, with no key named twice. */
export function checkedEntries(body: Record<string, unknown>): EntryInput[] {
  const given = body['entries'];
  if (!Array.isArray(given) || given.length === 0) {
    throw new HttpError(400, 'entries must be a non-empty array');
  }

  const entries: EntryInput[] = [];
  const seen = new Set<string>();
  for (const one of given as unknown[]) {
    if (typeof one !== 'object' || one === null || Array.isArray(one)) {
      throw new HttpError(400, 'each entry must be an object with a key and a value');
    }
    const entry = one as Record<string, unknown>;
    const key = checkedKey(entry['key']);
    if (seen.has(foldKey(key))) {
      throw new HttpError(400, `the body names ${key} more than once`);
    }
    seen.add(foldKey(key));
    entries.push({ key, value: checkedValue(entry['value']) });
  }
  return entries;
}
