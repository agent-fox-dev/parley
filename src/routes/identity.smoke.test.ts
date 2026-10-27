import { describe, expect, it } from 'vitest';

/**
 * Sign-in, scoping and blocking against a deployment that is really
 * running, with its own store and its own OAuth provider. Each suite says
 * what it needs and skips naming it rather than passing on nothing.
 */
const baseUrl = (process.env['PARLEY_BASE_URL'] ?? '').replace(/\/$/, '');
const adminToken = process.env['PARLEY_ADMIN_TOKEN'] ?? '';
const signInCode = process.env['PARLEY_SMOKE_OAUTH_CODE'] ?? '';
const provider = process.env['PARLEY_SMOKE_OAUTH_PROVIDER'] ?? 'github';

const missing = (...needs: Array<[string, string]>): string =>
  needs
    .filter(([, value]) => value.length === 0)
    .map(([name]) => name)
    .join(', ');

const noDeployment = missing(['PARLEY_BASE_URL', baseUrl]);
const noSignIn = missing(['PARLEY_BASE_URL', baseUrl], ['PARLEY_SMOKE_OAUTH_CODE', signInCode]);
const noAdmin = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_OAUTH_CODE', signInCode],
  ['PARLEY_ADMIN_TOKEN', adminToken],
);

async function call(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(`${baseUrl}${path}`, init);
}

const bearer = (credential: string, init: RequestInit = {}): RequestInit => ({
  ...init,
  headers: {
    ...((init.headers ?? {}) as Record<string, string>),
    authorization: `Bearer ${credential}`,
  },
});

const asJson = (method: string, body: unknown, credential: string): RequestInit =>
  bearer(credential, {
    method,
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });

interface SignIn {
  user: { id: string };
  api_key: { key: string; key_id: string };
}

async function signIn(): Promise<SignIn> {
  const response = await call('/api/v1/auth/callback', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ provider, code: signInCode }),
  });
  expect(response.status).toBe(200);
  return (await response.json()) as SignIn;
}

describe.skipIf(noSignIn.length > 0)(
  `signing in end to end${noSignIn.length > 0 ? ` (needs ${noSignIn})` : ''}`,
  () => {
    it('issues a key that works and retires the one the user already held', async () => {
      const providers = await call('/api/v1/auth/providers');
      expect(providers.status).toBe(200);
      expect(((await providers.json()) as Array<{ name: string }>).map((p) => p.name)).toContain(
        provider,
      );

      const first = await signIn();
      expect((await call('/api/v1/user', bearer(first.api_key.key))).status).toBe(200);

      const second = await signIn();
      expect((await call('/api/v1/user', bearer(second.api_key.key))).status).toBe(200);
      expect((await call('/api/v1/user', bearer(first.api_key.key))).status).toBe(401);

      const orgs = await call('/api/v1/user/orgs', bearer(second.api_key.key));
      expect(((await orgs.json()) as unknown[]).length).toBeGreaterThan(0);
    });
  },
);

describe.skipIf(noSignIn.length > 0)(
  `a token minted against a running deployment${noSignIn.length > 0 ? ` (needs ${noSignIn})` : ''}`,
  () => {
    it('is admitted on its own scope and refused outside it', async () => {
      const { api_key } = await signIn();

      const created = await call(
        '/api/v1/user/tokens',
        asJson('POST', { name: 'smoke token', permissions: ['workspaces:read'] }, api_key.key),
      );
      expect(created.status).toBe(201);
      const token = ((await created.json()) as { token: string }).token;

      const outside = await call('/api/v1/user/tokens', bearer(token));
      expect(outside.status).toBe(403);
      expect(Object.keys(((await outside.json()) as { error: object }).error)).not.toContain(
        'error_type',
      );

      const within = await call('/api/v1/user/orgs', bearer(token));
      expect(within.status).not.toBe(403);
    });
  },
);

describe.skipIf(noAdmin.length > 0)(
  `blocking a user against a running deployment${noAdmin.length > 0 ? ` (needs ${noAdmin})` : ''}`,
  () => {
    it('stops their live key without revoking it, and unblocking restores it', async () => {
      const { user, api_key } = await signIn();

      expect(
        (await call(`/api/v1/users/${user.id}/block`, bearer(adminToken, { method: 'POST' })))
          .status,
      ).toBe(200);
      expect((await call('/api/v1/user', bearer(api_key.key))).status).toBe(401);

      const listed = await call(`/api/v1/users/${user.id}/keys`, bearer(adminToken));
      const entry = (
        (await listed.json()) as Array<{ key_id: string; revoked_at: string | null }>
      ).find((key) => key.key_id === api_key.key_id);
      expect(entry?.revoked_at).toBeNull();

      expect(
        (await call(`/api/v1/users/${user.id}/unblock`, bearer(adminToken, { method: 'POST' })))
          .status,
      ).toBe(200);
      expect((await call('/api/v1/user', bearer(api_key.key))).status).toBe(200);
    });
  },
);

describe.skipIf(noDeployment.length > 0)(
  `a deployment answering without a credential${noDeployment.length > 0 ? ` (needs ${noDeployment})` : ''}`,
  () => {
    it('refuses a protected route with the documented message', async () => {
      const response = await call('/api/v1/user');

      expect(response.status).toBe(401);
      expect(((await response.json()) as { error: { message: string } }).error.message).toBe(
        'authentication required',
      );
    });
  },
);
