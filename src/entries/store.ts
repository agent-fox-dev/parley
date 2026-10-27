import { HttpError } from '../http/errors.js';
import type { RelationalStore, Row } from '../platform/index.js';
import { MAX_ENTRIES_PER_SCOPE, type EntryInput, foldKey } from './validate.js';

export type EntryKind = 'secret' | 'variable';

export interface EntryRecord {
  scope: string;
  kind: EntryKind;
  key: string;
  value: string;
  created_at: string;
  updated_at: string;
}

/** How a kind's value is written and read. A variable's passes through. */
export interface Cipher {
  seal(value: string): Promise<string>;
  open(value: string): Promise<string>;
}

export const plainCipher: Cipher = {
  seal: async (value) => Promise.resolve(value),
  open: async (value) => Promise.resolve(value),
};

export const userScope = (userId: string) => `user:${userId}`;
export const orgScope = (orgId: string) => `org:${orgId}`;

const text = (value: unknown): string => (typeof value === 'string' ? value : '');

async function toEntry(row: Row, kind: EntryKind, cipher: Cipher): Promise<EntryRecord> {
  return {
    scope: text(row['scope']),
    kind,
    key: text(row['key']),
    value: await cipher.open(text(row['value'])),
    created_at: text(row['created_at']),
    updated_at: text(row['updated_at']),
  };
}

/** A scope's entries of one kind, ordered by key ignoring case. */
export async function listEntries(
  store: RelationalStore,
  scope: string,
  kind: EntryKind,
  cipher: Cipher,
): Promise<EntryRecord[]> {
  const rows = await store.all(
    'SELECT * FROM entries WHERE scope = ? AND kind = ? ORDER BY key_folded',
    [scope, kind],
  );
  return Promise.all(rows.map(async (row) => toEntry(row, kind, cipher)));
}

async function entryRow(
  store: RelationalStore,
  scope: string,
  kind: EntryKind,
  key: string,
): Promise<Row | undefined> {
  const rows = await store.all(
    'SELECT * FROM entries WHERE scope = ? AND kind = ? AND key_folded = ?',
    [scope, kind, foldKey(key)],
  );
  return rows[0];
}

async function countOf(store: RelationalStore, scope: string, kind: EntryKind): Promise<number> {
  const rows = await store.all<{ total: number }>(
    'SELECT COUNT(*) AS total FROM entries WHERE scope = ? AND kind = ?',
    [scope, kind],
  );
  return Number(rows[0]?.total ?? 0);
}

/**
 * Creates every entry or none of them. The duplicate and the count are
 * checked before anything is written, and the writes go in one batch, so a
 * create that cannot finish leaves the scope as it was.
 */
export async function createEntries(
  store: RelationalStore,
  scope: string,
  kind: EntryKind,
  inputs: readonly EntryInput[],
  at: string,
  cipher: Cipher,
): Promise<EntryRecord[]> {
  for (const input of inputs) {
    if ((await entryRow(store, scope, kind, input.key)) !== undefined) {
      throw new HttpError(409, `${input.key} already exists in this scope`);
    }
  }
  if ((await countOf(store, scope, kind)) + inputs.length > MAX_ENTRIES_PER_SCOPE) {
    throw new HttpError(
      400,
      `a scope may hold at most ${String(MAX_ENTRIES_PER_SCOPE)} entries of one kind`,
    );
  }

  const sealed = await Promise.all(inputs.map(async (input) => cipher.seal(input.value)));
  await store.batch(
    inputs.map((input, index) => ({
      sql: `INSERT INTO entries (scope, kind, key, key_folded, value, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)`,
      params: [scope, kind, input.key, foldKey(input.key), sealed[index], at, at],
    })),
  );

  return inputs.map((input) => ({
    scope,
    kind,
    key: input.key,
    value: input.value,
    created_at: at,
    updated_at: at,
  }));
}

export async function replaceEntry(
  store: RelationalStore,
  scope: string,
  kind: EntryKind,
  key: string,
  value: string,
  at: string,
  cipher: Cipher,
): Promise<EntryRecord> {
  const existing = await entryRow(store, scope, kind, key);
  if (existing === undefined) {
    throw new HttpError(404, 'no entry with that key');
  }

  await store.run(
    'UPDATE entries SET value = ?, updated_at = ? WHERE scope = ? AND kind = ? AND key_folded = ?',
    [await cipher.seal(value), at, scope, kind, foldKey(key)],
  );

  return {
    scope,
    kind,
    key: text(existing['key']),
    value,
    created_at: text(existing['created_at']),
    updated_at: at,
  };
}

export async function deleteEntry(
  store: RelationalStore,
  scope: string,
  kind: EntryKind,
  key: string,
): Promise<void> {
  if ((await entryRow(store, scope, kind, key)) === undefined) {
    throw new HttpError(404, 'no entry with that key');
  }
  await store.run('DELETE FROM entries WHERE scope = ? AND kind = ? AND key_folded = ?', [
    scope,
    kind,
    foldKey(key),
  ]);
}
