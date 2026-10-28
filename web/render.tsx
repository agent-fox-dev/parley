import { Landing } from './pages/landing.js';
import { Dashboard, type DashboardWorkspace } from './pages/dashboard.js';
import { NotAvailable } from './pages/not_available.js';
import { Workspace } from './pages/workspace.js';

/**
 * Where JSX ends and the service begins.
 *
 * The route modules hand these plain values and take back a string
 * of HTML, so nothing outside `web/` has to be compiled as JSX.
 */
export function renderLanding(providers: Array<{ name: string }>): string {
  return `<!doctype html>${String(<Landing providers={providers} />)}`;
}

/** The one answer for a workspace a person cannot open. */
export function renderNotAvailable(signedInAs?: string): string {
  return `<!doctype html>${String(<NotAvailable {...(signedInAs === undefined ? {} : { signedInAs })} />)}`;
}

/** The dashboard, or the dashboard saying it could not be read. */
export function renderDashboard(
  signedInAs: string,
  workspaces: Array<Record<string, unknown>> | undefined,
): string {
  const rows =
    workspaces === undefined
      ? undefined
      : workspaces.map((one): DashboardWorkspace => ({
          slug: String(one['slug'] ?? ''),
          status: String(one['status'] ?? ''),
          access: String(one['access'] ?? ''),
          last_sync_at: typeof one['last_sync_at'] === 'string' ? one['last_sync_at'] : null,
        }));
  return `<!doctype html>${String(<Dashboard signedInAs={signedInAs} {...(rows === undefined ? {} : { workspaces: rows })} />)}`;
}

/** One workspace, as the person signed in may read it. */
export function renderWorkspace(signedInAs: string, workspace: Record<string, unknown>): string {
  const display = workspace['display_name'];
  return `<!doctype html>${String(
    <Workspace
      signedInAs={signedInAs}
      workspace={{
        slug: String(workspace['slug'] ?? ''),
        status: String(workspace['status'] ?? ''),
        ...(typeof display === 'string' ? { display_name: display } : {}),
      }}
    />,
  )}`;
}
