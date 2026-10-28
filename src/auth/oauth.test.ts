import { afterEach, describe, expect, it, vi } from 'vitest';
import { configuredProviders, githubProvider } from './oauth.js';
import { processConfig } from '../platform/local/config.js';

const settings = {
  clientId: 'client-abc',
  clientSecret: 'shhh',
  redirectAllowlist: [],
  authorizeBase: 'https://provider.test/authorize',
  tokenUrl: 'https://provider.test/token',
  profileUrl: 'https://provider.test/profile',
};

interface Answer {
  ok: boolean;
  body: unknown;
}

function answering(...answers: Answer[]) {
  const calls: Array<{ url: string; init?: RequestInit }> = [];
  const fetching = vi.fn(async (url: string | URL, init?: RequestInit) => {
    calls.push({ url: String(url), ...(init === undefined ? {} : { init }) });
    const answer = answers[calls.length - 1] ?? { ok: false, body: {} };
    return Promise.resolve({
      ok: answer.ok,
      json: async () => Promise.resolve(answer.body),
    } as Response);
  });
  vi.stubGlobal('fetch', fetching);
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the GitHub provider', () => {
  it('exchanges a code for a token and reads the profile behind it', async () => {
    const calls = answering(
      { ok: true, body: { access_token: 'granted-token' } },
      { ok: true, body: { id: 4242, login: 'alice', email: 'alice@example.com', name: 'Alice' } },
    );

    const identity = await githubProvider(settings).exchange('a-code', 'https://app.test/back');

    expect(identity).toEqual({
      provider: 'github',
      provider_id: '4242',
      username: 'alice',
      email: 'alice@example.com',
      full_name: 'Alice',
    });
    expect(calls[0]?.url).toBe(settings.tokenUrl);
    expect(String(calls[0]?.init?.body)).toContain('a-code');
    expect(String(calls[0]?.init?.body)).toContain('https://app.test/back');
    const profileHeaders = (calls[1]?.init?.headers ?? {}) as Record<string, string>;
    expect(profileHeaders['authorization']).toBe('Bearer granted-token');
  });

  it('introduces itself on every call, which GitHub refuses a request without', async () => {
    const calls = answering(
      { ok: true, body: { access_token: 'granted-token' } },
      { ok: true, body: { id: 1, login: 'alice' } },
    );

    await githubProvider(settings).exchange('a-code');

    // api.github.com answers 403 to a request carrying no
    // User-Agent, and neither Node nor a Worker sets one.
    for (const call of calls) {
      const headers = (call.init?.headers ?? {}) as Record<string, string>;
      expect(headers['user-agent'], `${call.url} introduced itself`).toBe('af-hub/parley');
    }
  });

  it('falls back to the login when the provider gives no display name', async () => {
    answering(
      { ok: true, body: { access_token: 'granted-token' } },
      { ok: true, body: { id: 7, login: 'bob' } },
    );

    const identity = await githubProvider(settings).exchange('a-code');

    expect(identity.full_name).toBe('bob');
    expect(identity.email).toBe('');
  });

  it.each([
    ['the token endpoint refuses the code', [{ ok: false, body: {} }]],
    ['the token endpoint returns no access token', [{ ok: true, body: { error: 'bad_code' } }]],
  ])('refuses the sign-in with 401 when %s', async (_name, answers) => {
    answering(...answers);

    await expect(githubProvider(settings).exchange('a-code')).rejects.toMatchObject({
      status: 401,
      message: 'the authorization code is invalid or expired',
    });
  });

  it('says the provider failed when the code was taken but the profile was not', async () => {
    // The two are not the same thing, and an operator told that a
    // code GitHub had just accepted was invalid looks in the wrong
    // place for as long as the message says so.
    answering({ ok: true, body: { access_token: 't' } }, { ok: false, body: {} });

    await expect(githubProvider(settings).exchange('a-code')).rejects.toMatchObject({
      status: 502,
      errorType: 'provider_unavailable',
    });
  });

  it('carries the client id into the authorize URL it advertises', () => {
    expect(githubProvider(settings).authorizeUrl).toBe(
      'https://provider.test/authorize?client_id=client-abc',
    );
  });
});

describe('the providers a deployment configured', () => {
  it('is empty when no client id or secret is set', () => {
    expect(configuredProviders(processConfig({})).list()).toEqual([]);
    expect(
      configuredProviders(processConfig({ PARLEY_OAUTH_GITHUB_CLIENT_ID: 'only-the-id' })).list(),
    ).toEqual([]);
  });

  it('reads the endpoints and the redirect allowlist from the environment', () => {
    const providers = configuredProviders(
      processConfig({
        PARLEY_OAUTH_GITHUB_CLIENT_ID: 'client-abc',
        PARLEY_OAUTH_GITHUB_CLIENT_SECRET: 'shhh',
        PARLEY_OAUTH_GITHUB_REDIRECT_URIS: 'https://a.test/back, https://b.test/back',
        PARLEY_OAUTH_GITHUB_AUTHORIZE_URL: 'https://provider.test/authorize',
      }),
    );

    const github = providers.find('github');
    expect(github?.authorizeUrl).toBe('https://provider.test/authorize?client_id=client-abc');
    expect(github?.redirectAllowlist).toEqual(['https://a.test/back', 'https://b.test/back']);
    expect(providers.find('gitlab')).toBeUndefined();
  });
});
