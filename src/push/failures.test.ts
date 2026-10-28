import { describe, expect, it } from 'vitest';
import { RemoteError } from '../git/transport.js';
import { nonFastForward, pushFailure, refusedRef, scrub } from './failures.js';

const CONTEXT = {
  branch: 'feature',
  gitUrl: 'https://someone:a-very-secret-token@example.test/r.git',
};

/** Every error type this route may answer with, and how to produce it. */
const OWN_TYPES: Array<[string, () => { errorType?: string; message: string }]> = [
  ['non_fast_forward', () => nonFastForward(CONTEXT, { force: false, origin: 'a'.repeat(40) })],
  ['forge_rejected', () => refusedRef('the branch is protected', CONTEXT, { force: false })],
  ['forge_not_found', () => pushFailure(new RemoteError(404, 'not found'), CONTEXT)],
  ['forge_unauthenticated', () => pushFailure(new RemoteError(401, 'nope'), CONTEXT)],
  ['forge_forbidden', () => pushFailure(new RemoteError(403, 'nope'), CONTEXT)],
  ['forge_unreachable', () => pushFailure(new TypeError('fetch failed'), CONTEXT)],
  ['forge_error', () => pushFailure(new RemoteError(500, 'boom'), CONTEXT)],
  [
    'forge_timeout',
    () => pushFailure(Object.assign(new Error('too slow'), { name: 'TimeoutError' }), CONTEXT),
  ],
];

describe("this route's own refusals", () => {
  it.each(OWN_TYPES)('begin the message with the type: %s', (type, make) => {
    const answer = make();

    expect(answer.errorType).toBe(type);
    expect(answer.message.startsWith(`${type}: `)).toBe(true);
  });
});

describe('a push origin is ahead of', () => {
  it("names origin's commit and says to force, for an ordinary branch", () => {
    const answer = nonFastForward(CONTEXT, { force: false, origin: 'b'.repeat(40) });

    expect(answer.status).toBe(409);
    expect(answer.message).toContain('b'.repeat(40));
    expect(answer.message.endsWith('push with force to overwrite it')).toBe(true);
  });

  it("says to sync, for the workspace's own branch", () => {
    const answer = nonFastForward(CONTEXT, { force: false, origin: 'c'.repeat(40), own: true });

    expect(answer.message.endsWith('sync the workspace to bring it up to date')).toBe(true);
  });

  it('says origin refuses even a forced update when it did', () => {
    const answer = refusedRef('non-fast-forward', CONTEXT, { force: true, theirs: 'd'.repeat(40) });

    expect(answer.errorType).toBe('non_fast_forward');
    expect(answer.message).toContain('also with force');
  });

  it("omits origin's commit when its refs could not be listed", () => {
    const answer = nonFastForward(CONTEXT, { force: false });

    expect(answer.message).toContain('has commits this workspace');
    expect(answer.message).not.toMatch(/[0-9a-f]{40}/);
  });
});

describe('what a refusal may say about the credential', () => {
  it('says there is none when the workspace stored none', () => {
    const answer = pushFailure(new RemoteError(401, 'nope'), CONTEXT);

    expect(answer.message).toContain('no stored credential');
  });

  it.each([
    ['a token', { kind: 'token' as const, token: 'a-very-secret-token' }, 'GIT_PAT'],
    [
      'a username and password',
      { kind: 'basic' as const, username: 'someone', password: 'a-very-secret-token' },
      'GIT_USERNAME',
    ],
  ])('names the kind origin rejected, never the value: %s', (_what, credential, named) => {
    const answer = pushFailure(new RemoteError(401, 'nope'), { ...CONTEXT, credential });

    expect(answer.message).toContain(named);
    expect(answer.message).not.toContain('a-very-secret-token');
  });
});

describe('anything a remote said', () => {
  it.each([
    ['a bearer token', 'failed with Authorization: Bearer abc.def-123', 'abc.def-123'],
    ['a URL carrying a credential', 'could not reach https://u:p@example.test/r.git', 'u:p@'],
    ['the workspace’s own userinfo', 'rejected for a-very-secret-token', 'a-very-secret-token'],
  ])('comes back with no secret in it: %s', (_what, said, secret) => {
    expect(scrub(said, CONTEXT.gitUrl)).not.toContain(secret);
  });

  it('keeps what is useful about the refusal', () => {
    expect(scrub('the branch is protected on this forge', CONTEXT.gitUrl)).toBe(
      'the branch is protected on this forge',
    );
  });
});
