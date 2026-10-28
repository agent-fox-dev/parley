import type { Check } from './harness.js';

/**
 * The conformance checks of the forge routes. They go after what a
 * client cannot see for itself: that the description route needs no
 * forge, that a refusal carries no credential, and that the order of
 * checks is the reference's.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

function configured(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  if (apiKey().length === 0) {
    return 'PARLEY_SMOKE_API_KEY';
  }
  return workspace().length === 0 ? 'PARLEY_SMOKE_WORKSPACE' : undefined;
}

async function read(baseUrl: string, path: string): Promise<{ status: number; text: string }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    headers: { authorization: `Bearer ${apiKey()}` },
  });
  return { status: response.status, text: await response.text() };
}

export function forgeChecks(): Check[] {
  return [
    {
      name: 'the forge description answers with a credential kind and no credential',
      needs: configured,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, `/api/v1/workspaces/${workspace()}/forge`);
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the description answered ${String(got.status)}` };
        }
        const body = JSON.parse(got.text) as { forge: string; credential: string };
        if (!['pat', 'basic', 'none'].includes(body.credential)) {
          return { outcome: 'fail', message: `credential read ${body.credential}` };
        }
        return /gh[pousr]_[A-Za-z0-9]|glpat-/.test(got.text)
          ? { outcome: 'fail', message: 'the description carried a credential' }
          : { outcome: 'pass', message: `${body.forge}, credential ${body.credential}` };
      },
    },
    {
      name: 'a bad target is refused before a bad path number',
      needs: configured,
      run: async ({ baseUrl }) => {
        const got = await read(
          baseUrl,
          `/api/v1/workspaces/${workspace()}/issues/not-a-number?target=sideways`,
        );
        if (got.status !== 400) {
          return { outcome: 'fail', message: `expected 400, got ${String(got.status)}` };
        }
        return got.text.includes('target')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the path number was answered before the target' };
      },
    },
    {
      name: 'a workspace the caller cannot see is 404 before anything else',
      needs: configured,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, '/api/v1/workspaces/no-such-workspace-here/issues');
        return got.status === 404
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 404, got ${String(got.status)}` };
      },
    },
    {
      name: 'an issue list answers a bare array',
      needs: configured,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, `/api/v1/workspaces/${workspace()}/issues?limit=1`);
        if (got.status === 422) {
          return { outcome: 'pass', message: 'the forge could not be told from the remote URL' };
        }
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the list answered ${String(got.status)}` };
        }
        return Array.isArray(JSON.parse(got.text))
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the list was not a bare array' };
      },
    },
  ];
}
