import type { FC } from 'hono/jsx';
import { Layout } from '../components/layout.js';
import { StatusBadge } from '../components/patterns.js';
import { badgeFor } from '../present.js';

/**
 * One workspace, as the person signed in may read it. Everything
 * shown here came from the API answering that person, so a field
 * they may not see is a field that never reached this page.
 */
export interface WorkspaceView {
  slug: string;
  status: string;
  display_name?: string;
}

export const Workspace: FC<{ signedInAs: string; workspace: WorkspaceView }> = ({
  signedInAs,
  workspace,
}) => (
  <Layout title={workspace.slug} signedInAs={signedInAs}>
    <h1>
      {workspace.slug} <StatusBadge {...badgeFor(workspace.status)} />
    </h1>
    {workspace.display_name === undefined || workspace.display_name === workspace.slug ? null : (
      <p>{workspace.display_name}</p>
    )}
  </Layout>
);
