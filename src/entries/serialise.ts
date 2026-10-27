import type { EntryRecord } from './store.js';

/**
 * What a secret route may answer with. There is no value field at all, so
 * a route cannot leak one by forgetting to remove it.
 */
export interface SecretView {
  key: string;
  created_at: string;
  updated_at: string;
}

export interface VariableView extends SecretView {
  value: string;
}

export function asSecret(entry: EntryRecord): SecretView {
  return { key: entry.key, created_at: entry.created_at, updated_at: entry.updated_at };
}

export function asVariable(entry: EntryRecord): VariableView {
  return { ...asSecret(entry), value: entry.value };
}
