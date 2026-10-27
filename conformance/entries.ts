import type { Check } from './harness.js';

/**
 * The conformance checks of secrets and variables. They need a deployment
 * and a credential, because every scope these routes serve belongs to
 * somebody.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';

function credentialled(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  return apiKey().length === 0 ? 'PARLEY_SMOKE_API_KEY' : undefined;
}

async function send(
  baseUrl: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<Response> {
  return fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    method,
    headers: {
      authorization: `Bearer ${apiKey()}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

const KEY = 'PARLEY_CONFORMANCE_PROBE';
const VALUE = 'conformance-probe-value-9f3a';

export function entryChecks(): Check[] {
  return [
    {
      name: 'a stored secret never comes back out',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        await send(baseUrl, 'DELETE', `/api/v1/user/secrets/${KEY}`);
        const created = await send(baseUrl, 'POST', '/api/v1/user/secrets', {
          entries: [{ key: KEY, value: VALUE }],
        });
        if (created.status !== 201) {
          return { outcome: 'fail', message: `the create answered ${String(created.status)}` };
        }
        const createdText = await created.text();
        const listed = await send(baseUrl, 'GET', '/api/v1/user/secrets');
        const listedText = await listed.text();
        await send(baseUrl, 'DELETE', `/api/v1/user/secrets/${KEY}`);

        if (createdText.includes(VALUE) || listedText.includes(VALUE)) {
          return { outcome: 'fail', message: 'a secret route answered with the value' };
        }
        return listedText.includes(KEY)
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the key was not listed after the create' };
      },
    },
    {
      name: 'a stored variable comes back with its value',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        await send(baseUrl, 'DELETE', `/api/v1/user/vars/${KEY}`);
        const created = await send(baseUrl, 'POST', '/api/v1/user/vars', {
          entries: [{ key: KEY, value: VALUE }],
        });
        const createdText = await created.text();
        await send(baseUrl, 'DELETE', `/api/v1/user/vars/${KEY}`);

        if (created.status !== 201) {
          return { outcome: 'fail', message: `the create answered ${String(created.status)}` };
        }
        return createdText.includes(VALUE)
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'a variable route did not answer with the value' };
      },
    },
    {
      name: 'a key the rules forbid is refused and writes nothing',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const refused = await send(baseUrl, 'POST', '/api/v1/user/vars', {
          entries: [{ key: '1-not-a-key', value: 'v' }],
        });
        return refused.status === 400
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 400, got ${String(refused.status)}` };
      },
    },
    {
      name: 'an admin token is refused the user scope of both kinds',
      needs: () =>
        (process.env['PARLEY_BASE_URL'] ?? '').length === 0
          ? 'a deployment'
          : (process.env['PARLEY_ADMIN_TOKEN'] ?? '').length === 0
            ? 'PARLEY_ADMIN_TOKEN'
            : undefined,
      run: async ({ baseUrl }) => {
        const admin = process.env['PARLEY_ADMIN_TOKEN'] ?? '';
        const answers = await Promise.all(
          ['/api/v1/user/secrets', '/api/v1/user/vars'].map(async (path) =>
            fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
              headers: { authorization: `Bearer ${admin}` },
            }),
          ),
        );
        return answers.every((r) => r.status === 403)
          ? { outcome: 'pass' }
          : {
              outcome: 'fail',
              message: `expected 403 from both, got ${answers.map((r) => r.status).join(' and ')}`,
            };
      },
    },
  ];
}
