import type { FC } from 'hono/jsx';
import { Layout } from '../components/layout.js';
import { CouldNotRead, Empty, RepoList, type RepoRow } from '../components/patterns.js';
import { badgeFor, saidWhen } from '../present.js';

/**
 * What a person sees when they have signed in: the workspaces they
 * own or share, and nothing of anybody else's — which is not a rule
 * this page applies, but one it inherits by asking the API as them.
 */
export interface DashboardWorkspace {
  slug: string;
  status: string;
  access: string;
  last_sync_at: string | null;
}

export const Dashboard: FC<{
  signedInAs: string;
  workspaces?: DashboardWorkspace[];
}> = ({ signedInAs, workspaces }) => (
  <Layout title="Your workspaces" signedInAs={signedInAs}>
    <h1>Your workspaces</h1>
    {workspaces === undefined ? (
      <CouldNotRead what="Your workspaces" />
    ) : workspaces.length === 0 ? (
      <Empty>
        <p>You have no workspaces yet.</p>
        <p>
          Create one with <code>afc workspace create</code>, and it will appear here.
        </p>
      </Empty>
    ) : (
      <RepoList
        label="Your workspaces"
        rows={workspaces.map((workspace): RepoRow => ({
          href: `/workspaces/${workspace.slug}`,
          name: workspace.slug,
          badge: badgeFor(workspace.status),
          meta: [workspace.access, `synced ${saidWhen(workspace.last_sync_at)}`],
        }))}
      />
    )}
  </Layout>
);
