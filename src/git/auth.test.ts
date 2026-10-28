import { describe, expect, it } from 'vitest';
import { decodePktLines, textOf } from './pktline.js';
import { basicPassword } from './auth.js';

/** What a client puts in an Authorization header for a given password. */
function basic(username: string, password: string): string {
  return `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`;
}

describe('the credential a git client presents', () => {
  it.each([
    ['a username', basic('parley', 'af_key_secret'), 'af_key_secret'],
    ['no username', basic('', 'af_key_secret'), 'af_key_secret'],
    ['a username that is an email', basic('a@b.test', 'af_key_secret'), 'af_key_secret'],
    ['a password holding a colon', basic('x', 'af_key:with:colons'), 'af_key:with:colons'],
  ])('is the password, whatever the username: %s', (_what, header, expected) => {
    expect(basicPassword(header)).toBe(expected);
  });

  it.each([
    ['nothing at all', undefined],
    ['a bearer token', 'Bearer af_key_secret'],
    ['a scheme that is not Basic', 'Digest nonsense'],
    ['base64 holding no colon', `Basic ${Buffer.from('nocolonhere').toString('base64')}`],
    ['base64 that is not base64', 'Basic !!!!'],
  ])('is absent for %s', (_what, header) => {
    expect(basicPassword(header)).toBeUndefined();
  });
});

describe('a refusal a git client receives', () => {
  it('is pkt-line the client can parse, not a JSON envelope', async () => {
    const { pktError } = await import('./pktline.js');
    const response = pktError(401, 'authentication required');
    const body = new Uint8Array(await response.arrayBuffer());

    expect(response.status).toBe(401);
    expect(new TextDecoder().decode(body).startsWith('{')).toBe(false);

    const lines = decodePktLines(body);
    expect(lines).toHaveLength(1);
    expect(lines.map((line) => textOf(line))).toEqual(['ERR authentication required\n']);
  });
});
