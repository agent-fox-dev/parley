import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { BODY_LIMITS, checkedBody } from './body_limits.js';

/**
 * What the forge will refuse is cheaper to refuse here — but only
 * the four fields the reference measures. Measuring a title would
 * reject what the forge accepts, which is worse than a round trip.
 */
describe('a body-like field', () => {
  it.each([
    ['github', 65_536],
    ['gitlab', 1_000_000],
  ] as const)('is measured against %s’s limit of %i code points', (forge, limit) => {
    expect(BODY_LIMITS[forge]).toBe(limit);

    expect(() => {
      checkedBody(forge, 'body', 'x'.repeat(limit));
    }).not.toThrow();
  });

  it('is refused when it is one code point too long, with the length and the limit', () => {
    let refusal: unknown;
    try {
      checkedBody('github', 'body', 'x'.repeat(65_537));
    } catch (err) {
      refusal = err;
    }

    const thrown = refusal as HttpError;
    expect(thrown.status).toBe(413);
    expect(thrown.errorType).toBe('body_too_large');
    expect(thrown.message).toContain('65537');
    expect(thrown.message).toContain('65536');
    expect(thrown.message).toContain('github');
  });

  it('counts a character and not a byte', () => {
    // Four bytes each, one code point each: the limit is in code
    // points, so sixty-five thousand of them fit where a byte count
    // would have refused them.
    const emoji = '😀'.repeat(65_536);

    expect(() => {
      checkedBody('github', 'body', emoji);
    }).not.toThrow();
    expect([...emoji]).toHaveLength(65_536);
  });

  it('is not measured when the caller gave none', () => {
    expect(() => {
      checkedBody('github', 'body', undefined);
    }).not.toThrow();
  });
});
