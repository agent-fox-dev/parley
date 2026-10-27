import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { parseTimestamp, renderTimestamp } from './rfc3339.js';

const RFC3339_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;

describe('renderTimestamp', () => {
  it.each([
    ['the epoch', 0],
    ['a whole second', Date.UTC(2026, 9, 27, 14, 30, 5)],
    ['an instant with milliseconds', Date.UTC(2026, 9, 27, 14, 30, 5) + 123],
    ['an instant before 1970', Date.UTC(1969, 0, 1, 0, 0, 0)],
  ])('renders %s as RFC 3339 in UTC', (_name, epochMillis) => {
    const rendered = renderTimestamp(new Date(epochMillis));

    expect(rendered).toMatch(RFC3339_UTC);
    expect(rendered.endsWith('Z')).toBe(true);
  });
});

describe('parseTimestamp', () => {
  it.each([
    ['a UTC value', '2026-01-02T03:04:05Z'],
    ['a positive offset', '2026-01-02T05:04:05+02:00'],
    ['a negative offset', '2026-01-01T19:04:05-08:00'],
  ])('reads %s as the same instant', (_name, value) => {
    const parsed = parseTimestamp(value, 'since');

    expect(parsed.getTime()).toBe(Date.parse('2026-01-02T03:04:05Z'));
    expect(renderTimestamp(parsed)).toBe('2026-01-02T03:04:05Z');
  });

  it('keeps fractional seconds and still renders with a Z offset', () => {
    const parsed = parseTimestamp('2026-01-02T05:04:05.250+02:00', 'since');

    expect(parsed.getTime()).toBe(Date.parse('2026-01-02T03:04:05.250Z'));
    expect(renderTimestamp(parsed)).toMatch(RFC3339_UTC);
    expect(renderTimestamp(parsed).endsWith('Z')).toBe(true);
  });

  it.each([
    ['an impossible month', '2026-13-01T00:00:00Z'],
    ['prose', 'not-a-time'],
    ['the empty string', ''],
    ['a date with no time', '2026-01-02'],
    ['a value with no offset', '2026-01-02T03:04:05'],
  ])('refuses %s with 400 naming the field', (_name, value) => {
    let thrown: unknown;
    try {
      parseTimestamp(value, 'since');
    } catch (err) {
      thrown = err;
    }

    expect(thrown).toBeInstanceOf(HttpError);
    const err = thrown as HttpError;
    expect(err.status).toBe(400);
    expect(err.message).toContain('since');
    expect(err.errorType).toBeUndefined();
  });

  it('names whichever field was given', () => {
    expect(() => parseTimestamp('nope', 'completed_at')).toThrow(/completed_at/);
  });
});
