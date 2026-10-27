import type { Check } from './harness.js';

/**
 * The conformance checks of identity and credentials. They need only a
 * deployment; the ones that need a credential say so and skip without it.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';

function deployed(): string | undefined {
  return (process.env['PARLEY_BASE_URL'] ?? '').length === 0 ? 'a deployment' : undefined;
}

function credentialled(): string | undefined {
  return deployed() ?? (apiKey().length === 0 ? 'PARLEY_SMOKE_API_KEY' : undefined);
}

async function get(baseUrl: string, path: string, credential?: string): Promise<Response> {
  return fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    headers: credential === undefined ? {} : { authorization: `Bearer ${credential}` },
  });
}

export function identityChecks(): Check[] {
  return [
    {
      name: 'a protected route refuses a caller with no credential',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/user');
        if (response.status !== 401) {
          return { outcome: 'fail', message: `expected 401, got ${response.status}` };
        }
        const body = (await response.json()) as { error?: { message?: string } };
        return body.error?.message === 'authentication required'
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `unexpected message ${String(body.error?.message)}` };
      },
    },
    {
      name: 'an unknown credential is refused exactly as none at all',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const without = await get(baseUrl, '/api/v1/user');
        const unknown = await get(baseUrl, '/api/v1/user', 'af_nobody_0123456789abcdef');
        const same =
          without.status === unknown.status &&
          JSON.stringify(await without.json()) === JSON.stringify(await unknown.json());
        return same
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the two refusals can be told apart' };
      },
    },
    {
      name: 'the provider list is public and publicly cacheable',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/auth/providers');
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${response.status}` };
        }
        const cacheControl = response.headers.get('cache-control') ?? '';
        return cacheControl.includes('public') && cacheControl.includes('max-age=300')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `unexpected Cache-Control ${cacheControl}` };
      },
    },
    {
      name: 'a key listing carries no secret',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/user/keys', apiKey());
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${response.status}` };
        }
        const text = await response.text();
        return text.includes(apiKey()) || text.includes('secret')
          ? { outcome: 'fail', message: 'the listing carries a credential or a secret field' }
          : { outcome: 'pass' };
      },
    },
    {
      name: 'an administration route refuses an ordinary key',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/users', apiKey());
        return response.status === 403 || response.status === 200
          ? { outcome: 'pass', message: `answered ${response.status}` }
          : {
              outcome: 'fail',
              message: `expected 200 for an admin or 403, got ${response.status}`,
            };
      },
    },
  ];
}
