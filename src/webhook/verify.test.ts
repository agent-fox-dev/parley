import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import {
  checkGithubSignature,
  checkGitlabToken,
  identityOf,
  sameSecret,
  schemeOf,
} from './verify.js';

/**
 * The checks that stand between anyone on the internet and a
 * delivery the hub believes. Each one of them answers the same thing
 * however it fails, so a caller cannot learn which part of a forgery
 * was nearly right.
 */
const SECRET = 'a-secret-the-forge-and-the-hub-share';

const headers = (values: Record<string, string>) => new Headers(values);

/** The signature GitHub would send for these bytes and this secret. */
async function signature(body: Uint8Array, secret = SECRET): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const signed = new Uint8Array(
    await crypto.subtle.sign('HMAC', key, body as unknown as BufferSource),
  );
  return `sha256=${[...signed].map((byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

describe('which forge a delivery says it is from', () => {
  it.each([
    ["GitHub's signature header", { 'x-hub-signature-256': 'sha256=x' }, 'github'],
    ["GitLab's token header", { 'x-gitlab-token': 'x' }, 'gitlab'],
    [
      'both, which GitHub wins',
      { 'x-hub-signature-256': 'sha256=x', 'x-gitlab-token': 'y' },
      'github',
    ],
  ])('reads %s', (_what, given, expected) => {
    expect(schemeOf(headers(given))).toBe(expected);
  });

  it('refuses a delivery that names neither', () => {
    expect(() => schemeOf(headers({ 'content-type': 'application/json' }))).toThrow(HttpError);
  });
});

describe('a secret compared with what was sent', () => {
  it.each([
    ['the same secret', SECRET, true],
    ['one differing in the first byte', `X${SECRET.slice(1)}`, false],
    ['one differing in the last byte', `${SECRET.slice(0, -1)}X`, false],
    ['a prefix of it', SECRET.slice(0, 10), false],
    ['one longer than it', `${SECRET}x`, false],
    ['an empty string', '', false],
  ])('answers %s correctly', (_what, given, expected) => {
    expect(sameSecret(given, SECRET)).toBe(expected);
  });
});

describe("GitLab's token", () => {
  it('is accepted when it is the secret', () => {
    expect(() => {
      checkGitlabToken(headers({ 'x-gitlab-token': SECRET }), SECRET);
    }).not.toThrow();
  });

  it.each([
    ['a missing token', {}],
    ['an empty token', { 'x-gitlab-token': '' }],
    ['a wrong token', { 'x-gitlab-token': 'not-the-secret' }],
    ['a token that is a prefix', { 'x-gitlab-token': SECRET.slice(0, 8) }],
  ])('refuses %s with the one refusal', (_what, given) => {
    let refusal: unknown;
    try {
      checkGitlabToken(headers(given), SECRET);
    } catch (err) {
      refusal = err;
    }

    expect((refusal as HttpError).status).toBe(401);
    expect((refusal as HttpError).message).toBe('delivery signature missing or invalid');
  });
});

describe("GitHub's signature", () => {
  // Spacing a forge may well send, and which a re-serialisation
  // would not reproduce.
  const body = new TextEncoder().encode('{\n  "zebra": 1,\n  "apple": 2\n}');

  it('is accepted when it is the HMAC of the bytes sent', async () => {
    await expect(
      checkGithubSignature(headers({ 'x-hub-signature-256': await signature(body) }), body, SECRET),
    ).resolves.toBeUndefined();
  });

  it('is refused when it is the HMAC of the same JSON written out again', async () => {
    // The same document, re-serialised: different bytes, so a
    // different signature, and the hub signs what was sent.
    const rewritten = new TextEncoder().encode(
      JSON.stringify(JSON.parse(new TextDecoder().decode(body)) as unknown),
    );
    expect(rewritten).not.toEqual(body);

    await expect(
      checkGithubSignature(
        headers({ 'x-hub-signature-256': await signature(rewritten) }),
        body,
        SECRET,
      ),
    ).rejects.toMatchObject({ status: 401 });
  });

  it.each([
    ['no signature at all', undefined],
    ['one with no prefix', 'a'.repeat(64)],
    ['one with the SHA-1 prefix', `sha1=${'a'.repeat(40)}`],
    ['one with uppercase hex', `sha256=${'A'.repeat(64)}`],
    ['one too short', `sha256=${'a'.repeat(63)}`],
    ['one that is simply wrong', `sha256=${'a'.repeat(64)}`],
  ])('refuses %s with the one refusal', async (_what, given) => {
    const sent: Record<string, string> =
      given === undefined ? {} : { 'x-hub-signature-256': given };

    await expect(checkGithubSignature(headers(sent), body, SECRET)).rejects.toMatchObject({
      status: 401,
      message: 'delivery signature missing or invalid',
    });
  });

  it('refuses a signature made with another secret', async () => {
    await expect(
      checkGithubSignature(
        headers({ 'x-hub-signature-256': await signature(body, 'another-secret-entirely') }),
        body,
        SECRET,
      ),
    ).rejects.toMatchObject({ status: 401 });
  });
});

describe("a delivery's identity", () => {
  it.each([
    [
      'GitHub',
      'github' as const,
      { 'x-github-delivery': 'd-1', 'x-github-event': 'issues' },
      'd-1',
      'issues',
    ],
    [
      'GitLab, by idempotency key',
      'gitlab' as const,
      { 'idempotency-key': 'd-2', 'x-gitlab-event': 'Issue Hook' },
      'd-2',
      'Issue Hook',
    ],
    [
      'GitLab, by event UUID where there is no key',
      'gitlab' as const,
      { 'x-gitlab-event-uuid': 'd-3', 'x-gitlab-event': 'Push Hook' },
      'd-3',
      'Push Hook',
    ],
  ])('is read from %s', (_what, forge, given, id, event) => {
    expect(identityOf(headers(given), forge)).toEqual({ forge, id, event });
  });

  it.each([
    ['no id', { 'x-github-event': 'issues' }],
    ['no event name', { 'x-github-delivery': 'd-1' }],
    [
      'an id of 256 characters',
      { 'x-github-delivery': 'd'.repeat(256), 'x-github-event': 'issues' },
    ],
    [
      'an id with a character outside printable ASCII',
      { 'x-github-delivery': 'd-\u0001', 'x-github-event': 'issues' },
    ],
  ])('is refused for %s', (_what, given) => {
    let refusal: unknown;
    try {
      identityOf(headers(given), 'github');
    } catch (err) {
      refusal = err;
    }

    expect((refusal as HttpError).status).toBe(400);
  });
});
