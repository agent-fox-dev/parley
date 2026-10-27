import { describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import { MAX_VALUE_BYTES, checkedEntries, checkedValue, foldKey, isValidKey } from './validate.js';

function refusal(body: Record<string, unknown>): HttpError {
  try {
    checkedEntries(body);
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the entries were accepted when they should have been refused');
}

const entry = (key: string, value = 'a value') => ({ entries: [{ key, value }] });

describe('the shape of a key', () => {
  it.each([
    ['one letter', 'A'],
    ['letters and an underscore', 'A_1'],
    ['lower case and a digit', 'a1'],
    ['a leading underscore', '_A'],
    ['two hundred and fifty-five characters', 'K'.repeat(255)],
  ])('accepts %s', (_name, key) => {
    expect(isValidKey(key)).toBe(true);
    expect(checkedEntries(entry(key))[0]?.key).toBe(key);
  });

  it.each([
    ['an empty key', ''],
    ['two hundred and fifty-six characters', 'K'.repeat(256)],
    ['a leading digit', '1A'],
    ['a hyphen', 'A-B'],
    ['a dot', 'A.B'],
    ['a space', 'A B'],
    ['a dollar sign', 'A$B'],
  ])('refuses %s', (_name, key) => {
    expect(isValidKey(key)).toBe(false);
    expect(refusal(entry(key)).status).toBe(400);
  });
});

describe('matching a key', () => {
  it.each([
    ['my_key', 'MY_KEY'],
    ['MyKey', 'mykey'],
    ['Mixed_Case_1', 'mixed_case_1'],
  ])('folds %s and %s to the same thing', (left, right) => {
    expect(foldKey(left)).toBe(foldKey(right));
  });

  it('keeps keys that differ by more than case apart', () => {
    expect(foldKey('MY_KEY')).not.toBe(foldKey('MY_KEY_2'));
  });
});

describe('the size of a value', () => {
  it.each([
    ['an empty value', 0],
    ['one byte', 1],
    ['exactly the limit', MAX_VALUE_BYTES],
  ])('accepts %s', (_name, size) => {
    expect(checkedValue('v'.repeat(size))).toHaveLength(size);
  });

  it('refuses one byte over the limit', () => {
    let err: HttpError | undefined;
    try {
      checkedValue('v'.repeat(MAX_VALUE_BYTES + 1));
    } catch (caught) {
      err = caught as HttpError;
    }

    expect(err?.status).toBe(400);
  });

  it('measures the value in bytes, not in characters', () => {
    // Each of these is three bytes of UTF-8, so a third of the limit in
    // characters is already the whole limit in bytes.
    const atLimit = '中'.repeat(MAX_VALUE_BYTES / 3);

    expect(checkedValue(atLimit)).toBe(atLimit);
    expect(() => checkedValue(`${atLimit}中`)).toThrow();
  });

  it.each([
    ['a missing value', undefined],
    ['a null value', null],
    ['a value that is not a string', 42],
  ])('refuses %s', (_name, value) => {
    expect(() => checkedValue(value)).toThrow();
  });
});

describe('a create body', () => {
  it('reads every entry it names', () => {
    const read = checkedEntries({
      entries: [
        { key: 'ONE', value: 'first' },
        { key: 'TWO', value: 'second' },
      ],
    });

    expect(read).toEqual([
      { key: 'ONE', value: 'first' },
      { key: 'TWO', value: 'second' },
    ]);
  });

  it.each([
    ['no entries key', {}],
    ['an empty list', { entries: [] }],
    ['a list that is not an array', { entries: 'ONE' }],
    ['an entry that is not an object', { entries: ['ONE'] }],
    ['an entry with no key', { entries: [{ value: 'v' }] }],
    ['an entry with no value', { entries: [{ key: 'ONE' }] }],
    ['a second entry that is malformed', { entries: [{ key: 'ONE', value: 'v' }, { key: '1' }] }],
  ])('is refused when it has %s', (_name, body) => {
    expect(refusal(body as Record<string, unknown>).status).toBe(400);
  });

  it('refuses a body naming one key twice, whatever the casing', () => {
    expect(
      refusal({
        entries: [
          { key: 'MY_KEY', value: 'first' },
          { key: 'my_key', value: 'second' },
        ],
      }).status,
    ).toBe(400);
  });
});
