import { describe, expect, it } from 'vitest';
import type { EntryRecord } from './store.js';
import { asSecret, asVariable } from './serialise.js';

const entry: EntryRecord = {
  scope: 'user:someone',
  kind: 'secret',
  key: 'THE_KEY',
  value: 'SENTINEL-9f3a',
  created_at: '2026-03-01T12:00:00.000Z',
  updated_at: '2026-03-02T12:00:00.000Z',
};

describe('how an entry is answered with', () => {
  it('leaves a secret with nowhere to put its value', () => {
    const answered = asSecret(entry);

    expect(JSON.stringify(answered)).not.toContain('SENTINEL-9f3a');
    expect(Object.keys(answered).toSorted()).toEqual(['created_at', 'key', 'updated_at']);
  });

  it('gives a variable its value, so the two kinds differ and not the route', () => {
    const answered = asVariable({ ...entry, kind: 'variable' });

    expect(answered.value).toBe('SENTINEL-9f3a');
    expect(Object.keys(answered).toSorted()).toEqual(['created_at', 'key', 'updated_at', 'value']);
  });
});
