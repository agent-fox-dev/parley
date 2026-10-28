import { prepareAfc } from './afc.js';
import { identityChecks } from './identity.js';
import { entryChecks } from './entries.js';
import { repositoryChecks } from './repository.js';
import { gitServerChecks } from './git_server.js';
import { syncPushChecks } from './sync_push.js';
import { carryPatchChecks } from './carry_patch.js';
import { mergeChecks } from './merges.js';
import { sessionChecks } from './sessions.js';
import { workspaceChecks } from './workspaces.js';
import { type Check, formatReport, runHarness } from './harness.js';

/**
 * The conformance checks of the service foundation. Later packages add
 * their own to this list.
 */
function reachable(baseUrl: string): boolean {
  return baseUrl.length > 0;
}

function route(name: string, path: string, expect: (r: Response) => string | undefined): Check {
  return {
    name,
    needs: () => (process.env['PARLEY_BASE_URL'] === undefined ? 'a deployment' : undefined),
    run: async ({ baseUrl }) => {
      if (!reachable(baseUrl)) {
        return { outcome: 'skip', message: 'skipped: a deployment is not available' };
      }
      const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`);
      const problem = expect(response);
      return problem === undefined ? { outcome: 'pass' } : { outcome: 'fail', message: problem };
    },
  };
}

export function foundationChecks(): Check[] {
  return [
    route('healthz answers ok', '/healthz', (r) =>
      r.status === 200 ? undefined : `expected 200, got ${r.status}`,
    ),
    route('readyz answers', '/readyz', (r) =>
      r.status === 200 || r.status === 503 ? undefined : `expected 200 or 503, got ${r.status}`,
    ),
    route('version reports the build', '/version', (r) =>
      r.status === 200 ? undefined : `expected 200, got ${r.status}`,
    ),
    route('metrics are exposed', '/metrics', (r) =>
      r.headers.get('content-type') === 'text/plain; version=0.0.4'
        ? undefined
        : `expected the Prometheus content type, got ${r.headers.get('content-type')}`,
    ),
    {
      name: 'afc is available for the CLI checks',
      run: async () => {
        const prepared = await prepareAfc();
        return prepared.skipped === undefined
          ? { outcome: 'pass', message: `built from ${prepared.commit ?? 'unknown'}` }
          : { outcome: 'skip', message: prepared.skipped };
      },
    },
  ];
}

export async function main(): Promise<number> {
  const result = await runHarness({
    baseUrl: process.env['PARLEY_BASE_URL'] ?? '',
    checks: [
      ...foundationChecks(),
      ...identityChecks(),
      ...workspaceChecks(),
      ...entryChecks(),
      ...repositoryChecks(),
      ...gitServerChecks(),
      ...syncPushChecks(),
      ...carryPatchChecks(),
      ...mergeChecks(),
      ...sessionChecks(),
    ],
  });
  console.log(formatReport(result));
  return result.exitCode;
}
