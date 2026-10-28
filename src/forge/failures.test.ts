import { describe, expect, it } from 'vitest';
import { scrubbed } from './failures.js';

/**
 * Nothing of a credential leaves this service in a message. The
 * forge's own words are worth carrying because they usually say what
 * was wrong, and they are exactly the words most likely to quote the
 * credential back.
 */
describe('a message from the forge', () => {
  it.each([
    [
      'the token, wherever it appears',
      'bad token ghp_secret here',
      'ghp_secret',
      'bad token [redacted] here',
    ],
    [
      'the userinfo of an https URL',
      'at https://user:pass@github.com/acme/widgets',
      undefined,
      'at https://github.com/acme/widgets',
    ],
    [
      'the userinfo of an ssh URL',
      'at ssh://git@github.com/acme/widgets',
      undefined,
      'at ssh://github.com/acme/widgets',
    ],
    ['nothing where there is nothing to take out', 'plain words', undefined, 'plain words'],
  ])('loses %s', (_what, message, token, expected) => {
    expect(scrubbed(message, token)).toBe(expected);
  });

  it('is cut to 1024 bytes, counted as bytes and not characters', () => {
    const long = '😀'.repeat(1000);

    const cut = scrubbed(long);

    expect(new TextEncoder().encode(cut).length).toBeLessThanOrEqual(1024);
  });
});
