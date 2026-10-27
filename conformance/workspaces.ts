import type { Check } from './harness.js';

/**
 * The conformance checks of the workspace package. They need a deployment
 * and, for the ones that create anything, a credential.
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

const DOCUMENTED_SETTINGS = [
  'rebuild_strategy',
  'rebuild_fail_mode',
  'auto_rebuild_after_sync',
  'auto_rebuild_after_push',
  'patch_branch_source',
  'patch_divergence_policy',
  'push_patches_to_origin',
  'rebuild_push_integration_branch',
  'squash_merge_detection',
  'forge_api_url',
  'upstream_forge_api_url',
  'sync_enabled',
];

export function workspaceChecks(): Check[] {
  return [
    {
      name: 'an unknown workspace is not found, and says no more than that',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/workspaces/no-such-workspace-xyz', apiKey());
        if (response.status !== 404) {
          return { outcome: 'fail', message: `expected 404, got ${response.status}` };
        }
        const body = (await response.json()) as { error?: { message?: string } };
        return body.error?.message === 'workspace not found'
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `unexpected message ${String(body.error?.message)}` };
      },
    },
    {
      name: 'every listed workspace carries all twelve settings',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/workspaces', apiKey());
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${response.status}` };
        }
        const listed = (await response.json()) as Array<{ settings?: Record<string, unknown> }>;
        if (listed.length === 0) {
          return { outcome: 'skip', message: 'skipped: the deployment has no workspace to read' };
        }
        const missing = DOCUMENTED_SETTINGS.filter((key) => !(key in (listed[0]?.settings ?? {})));
        return missing.length === 0
          ? { outcome: 'pass', message: `${String(listed.length)} listed` }
          : { outcome: 'fail', message: `settings missing: ${missing.join(', ')}` };
      },
    },
    {
      name: 'a sandbox route says sandboxes are not enabled',
      needs: credentialled,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/sandbox-profiles', apiKey());
        if (response.status !== 404) {
          return { outcome: 'fail', message: `expected 404, got ${response.status}` };
        }
        const body = (await response.json()) as { error?: { error_type?: string } };
        return body.error?.error_type === 'sandboxes_disabled'
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the answer carries no sandboxes_disabled' };
      },
    },
    {
      name: 'a sandbox route refuses a caller with no credential for the credential',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/sandbox-profiles');
        return response.status === 401
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 401, got ${response.status}` };
      },
    },
    {
      name: 'the workspace list is refused without a credential',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const response = await get(baseUrl, '/api/v1/workspaces');
        return response.status === 401
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `expected 401, got ${response.status}` };
      },
    },
  ];
}
