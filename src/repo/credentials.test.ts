import { describe, expect, it } from 'vitest';
import type { LogSink } from '../http/router.js';
import type { SecretStore } from '../platform/secrets.js';
import { resolveGitCredential } from './credentials.js';

const SCOPE = 'workspace:a-workspace';

/** A secret store holding exactly what a case names, and nothing else. */
function holding(entries: Record<string, string>): SecretStore {
  return {
    put: async () => Promise.resolve(),
    delete: async () => Promise.resolve(),
    names: async () => Promise.resolve(Object.keys(entries)),
    get: async (scope: string, key: string) =>
      Promise.resolve(scope === SCOPE ? entries[key] : undefined),
  };
}

function recordingLog(): { log: LogSink; records: string[] } {
  const records: string[] = [];
  const write = (message: string, detail?: unknown) => {
    records.push(`${message} ${detail === undefined ? '' : JSON.stringify(detail)}`);
  };
  return { log: { error: write, info: write }, records };
}

describe('the credential for origin', () => {
  it.each([
    [
      'the token, when there is one',
      { GIT_PAT: 'ghp_token' },
      { kind: 'token', token: 'ghp_token' },
    ],
    [
      'the token, even beside a complete basic pair',
      { GIT_PAT: 'ghp_token', GIT_USERNAME: 'user', GIT_PASSWORD: 'pw' },
      { kind: 'token', token: 'ghp_token' },
    ],
    [
      'the basic pair, when there is no token',
      { GIT_USERNAME: 'user', GIT_PASSWORD: 'pw' },
      { kind: 'basic', username: 'user', password: 'pw' },
    ],
  ])('is %s', async (_name, entries, expected) => {
    expect(await resolveGitCredential(holding(entries), SCOPE, 'origin')).toEqual(expected);
  });

  it('is nothing when the workspace holds neither', async () => {
    expect(await resolveGitCredential(holding({}), SCOPE, 'origin')).toBeUndefined();
  });
});

describe('the credential for upstream', () => {
  it.each([
    [
      'its own token, when there is one',
      { UPSTREAM_GIT_PAT: 'up_token', GIT_PAT: 'origin_token' },
      { kind: 'token', token: 'up_token' },
    ],
    [
      'its own basic pair, when there is no upstream token',
      { UPSTREAM_GIT_USERNAME: 'up_user', UPSTREAM_GIT_PASSWORD: 'up_pw', GIT_PAT: 'origin' },
      { kind: 'basic', username: 'up_user', password: 'up_pw' },
    ],
    [
      "origin's, when the workspace holds no upstream secret at all",
      { GIT_PAT: 'origin_token' },
      { kind: 'token', token: 'origin_token' },
    ],
    [
      "origin's basic pair, when that is all there is",
      { GIT_USERNAME: 'user', GIT_PASSWORD: 'pw' },
      { kind: 'basic', username: 'user', password: 'pw' },
    ],
  ])('is %s', async (_name, entries, expected) => {
    expect(await resolveGitCredential(holding(entries), SCOPE, 'upstream')).toEqual(expected);
  });

  it('is nothing when the workspace holds neither upstream nor origin', async () => {
    expect(await resolveGitCredential(holding({}), SCOPE, 'upstream')).toBeUndefined();
  });
});

describe('a basic pair with only one half', () => {
  it.each([
    ['only a username', { GIT_USERNAME: 'user' }],
    ['only a password', { GIT_PASSWORD: 'SENTINEL-pw-9f3a' }],
  ])('falls through as though neither half were there: %s', async (_name, entries) => {
    expect(await resolveGitCredential(holding(entries), SCOPE, 'origin')).toBeUndefined();
  });

  it('falls through to the token when there is one', async () => {
    const resolved = await resolveGitCredential(
      holding({ GIT_PAT: 'ghp_token', GIT_USERNAME: 'user' }),
      SCOPE,
      'origin',
    );

    expect(resolved).toEqual({ kind: 'token', token: 'ghp_token' });
  });

  it('says in the log that the half pair was ignored, without saying what it held', async () => {
    const { log, records } = recordingLog();

    await resolveGitCredential(holding({ GIT_PASSWORD: 'SENTINEL-pw-9f3a' }), SCOPE, 'origin', log);

    expect(records.join('\n')).toContain('GIT_PASSWORD');
    expect(records.join('\n')).toContain('ignored');
    expect(records.join('\n')).not.toContain('SENTINEL-pw-9f3a');
  });

  it('says nothing when the pair is complete', async () => {
    const { log, records } = recordingLog();

    await resolveGitCredential(
      holding({ GIT_USERNAME: 'user', GIT_PASSWORD: 'pw' }),
      SCOPE,
      'origin',
      log,
    );

    expect(records.join('\n')).not.toContain('ignored');
  });
});
