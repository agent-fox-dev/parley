import { describe, expect, it } from 'vitest';
import { checkRemoteCredential } from './copy_in.js';

/**
 * Whether a remote took the credential it was given. The distinction
 * matters at the moment a workspace is created: a refusal is the
 * caller's mistake and stops the create, while a forge having a bad
 * day is not a reason to refuse a workspace.
 */

/** A remote that answers one status to everything. */
function answering(status: number): typeof fetch {
  return (() => Promise.resolve(new Response('', { status }))) as unknown as typeof fetch;
}

describe('what a remote answered, and what it means for a create', () => {
  it.each([
    ['401, which is the credential being wrong', 401, 'refused'],
    ['403, which is the credential not being allowed', 403, 'refused'],
    ['500, which is the forge having a bad day', 500, 'unreachable'],
    ['404, which is the repository not being there', 404, 'unreachable'],
  ])('reads %s', async (_what, status, expected) => {
    expect(
      await checkRemoteCredential({
        gitUrl: 'https://example.test/r.git',
        fetch: answering(status),
      }),
    ).toBe(expected);
  });

  it('reads a remote nobody can reach as unreachable, not as a refusal', async () => {
    expect(
      await checkRemoteCredential({
        gitUrl: 'https://example.test/r.git',
        fetch: (() => Promise.reject(new TypeError('fetch failed'))) as unknown as typeof fetch,
      }),
    ).toBe('unreachable');
  });

  it('is not fooled by a 403 that is only part of an address', async () => {
    // The status is what decides, not the words of an error, so a
    // URL that happens to hold the number means nothing.
    expect(
      await checkRemoteCredential({
        gitUrl: 'https://example.test/pull/403/head.git',
        fetch: answering(500),
      }),
    ).toBe('unreachable');
  });
});
