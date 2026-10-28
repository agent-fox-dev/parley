import type { Check } from './harness.js';

/**
 * The conformance checks of the patch stack. They ask a running
 * deployment about a carry-patch workspace and check that the answers
 * agree with one another: a dashboard whose counts disagree with its
 * own list is worse than no dashboard.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_PATCH_WORKSPACE'] ?? '';

function carrying(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  if (apiKey().length === 0) {
    return 'PARLEY_SMOKE_API_KEY';
  }
  return workspace().length === 0 ? 'PARLEY_SMOKE_PATCH_WORKSPACE' : undefined;
}

async function read(baseUrl: string, path: string): Promise<{ status: number; body: unknown }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    headers: { authorization: `Bearer ${apiKey()}` },
  });
  return { status: response.status, body: await response.json().catch(() => undefined) };
}

/** Where the integration branch stands, as the dashboard reports it. */
function integrationHead(what: { body: unknown }): string | undefined {
  return (what.body as { integration_head_sha?: string }).integration_head_sha;
}

export function carryPatchChecks(): Check[] {
  return [
    {
      name: "the dashboard's counts agree with the patches it lists",
      needs: carrying,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, `/api/v1/workspaces/${workspace()}/patch-status`);
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the dashboard answered ${String(got.status)}` };
        }
        const body = got.body as { patches: unknown[]; summary: Record<string, number> };
        const summed =
          (body.summary['active'] ?? 0) +
          (body.summary['merged_upstream'] ?? 0) +
          (body.summary['conflict'] ?? 0) +
          (body.summary['disabled'] ?? 0);
        if (body.summary['total_patches'] !== body.patches.length) {
          return {
            outcome: 'fail',
            message: `it counts ${String(body.summary['total_patches'])} patches and lists ${String(body.patches.length)}`,
          };
        }
        return summed === body.summary['total_patches']
          ? { outcome: 'pass', message: `${String(body.patches.length)} patches` }
          : { outcome: 'fail', message: 'the per-status counts do not add up to the total' };
      },
    },
    {
      name: 'the patch list is contiguous from 1',
      needs: carrying,
      run: async ({ baseUrl }) => {
        const got = await read(baseUrl, `/api/v1/workspaces/${workspace()}/patches`);
        if (got.status !== 200) {
          return { outcome: 'fail', message: `the list answered ${String(got.status)}` };
        }
        const positions = (got.body as Array<{ position: number }>).map((one) => one.position);
        const wanted = positions.map((_unused, index) => index + 1);
        return JSON.stringify(positions) === JSON.stringify(wanted)
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `the positions read ${positions.join(', ')}` };
      },
    },
    {
      name: 'the rebuild preview predicts without writing',
      needs: carrying,
      run: async ({ baseUrl }) => {
        const before = await read(baseUrl, `/api/v1/workspaces/${workspace()}/patch-status`);
        const previewed = await fetch(
          `${baseUrl.replace(/\/$/, '')}/api/v1/workspaces/${workspace()}/rebuild/preview`,
          { method: 'POST', headers: { authorization: `Bearer ${apiKey()}` } },
        );
        if (previewed.status !== 200) {
          return { outcome: 'fail', message: `the preview answered ${String(previewed.status)}` };
        }
        const after = await read(baseUrl, `/api/v1/workspaces/${workspace()}/patch-status`);
        return integrationHead(before) === integrationHead(after)
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the preview moved the integration branch' };
      },
    },
  ];
}
