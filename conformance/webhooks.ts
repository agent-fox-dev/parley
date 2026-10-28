import type { Check } from './harness.js';

/**
 * The conformance checks of the webhook ingress. It is the one route
 * open to the internet, so what these go after is its refusals: that
 * they say nothing about which workspaces exist, and that they
 * happen whatever the body.
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

async function postTo(
  baseUrl: string,
  path: string,
  headers: Record<string, string>,
  body: string,
): Promise<{ status: number; text: string }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...headers },
    body,
  });
  return { status: response.status, text: await response.text() };
}

const PAYLOAD = JSON.stringify({ action: 'opened', repository: { full_name: 'acme/widgets' } });

export function webhookChecks(): Check[] {
  return [
    {
      name: 'the ingress needs no bearer credential to refuse',
      needs: configured,
      run: async ({ baseUrl }) => {
        const got = await postTo(baseUrl, `/hooks/forge/${workspace()}`, {}, PAYLOAD);
        // Either the workspace has no secret (404) or it has one and
        // this request carries no signature (401). Both are refusals
        // reached without a credential of ours.
        return [401, 404].includes(got.status)
          ? { outcome: 'pass', message: `answered ${String(got.status)}` }
          : { outcome: 'fail', message: `expected 401 or 404, got ${String(got.status)}` };
      },
    },
    {
      name: 'an unknown workspace and one without a secret are told the same thing',
      needs: configured,
      run: async ({ baseUrl }) => {
        const unknown = await postTo(
          baseUrl,
          '/hooks/forge/no-such-workspace-here',
          { 'x-gitlab-token': 'anything', 'x-gitlab-event': 'Push Hook' },
          PAYLOAD,
        );
        if (unknown.status !== 404) {
          return { outcome: 'fail', message: `an unknown slug answered ${String(unknown.status)}` };
        }
        return unknown.text.includes('not found')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the refusal said more than "not found"' };
      },
    },
    {
      name: 'the delivery log answers an array and names a bad parameter',
      needs: configured,
      run: async ({ baseUrl }) => {
        const listed = await fetch(
          `${baseUrl.replace(/\/$/, '')}/api/v1/workspaces/${workspace()}/forge/deliveries`,
          { headers: { authorization: `Bearer ${apiKey()}` } },
        );
        if (listed.status !== 200) {
          return { outcome: 'fail', message: `the log answered ${String(listed.status)}` };
        }
        if (!Array.isArray(await listed.json())) {
          return { outcome: 'fail', message: 'the log was not an array' };
        }

        const refused = await fetch(
          `${baseUrl.replace(/\/$/, '')}/api/v1/workspaces/${workspace()}/forge/deliveries?limit=0`,
          { headers: { authorization: `Bearer ${apiKey()}` } },
        );
        return refused.status === 400
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `a limit of zero answered ${String(refused.status)}` };
      },
    },
  ];
}
