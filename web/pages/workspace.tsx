import type { FC } from 'hono/jsx';
import { Layout } from '../components/layout.js';
import { CouldNotRead, Empty, StatusBadge, Timeline } from '../components/patterns.js';
import { badgeFor, saidWhen } from '../present.js';

/**
 * One workspace, as the person signed in may read it.
 *
 * Every section is rendered on its own and takes its own data, so a
 * read that failed costs its section and nothing else: a person
 * looking at a workspace because something is wrong should not be
 * shown an error page because one part of it is.
 */
export interface WorkspaceFields {
  slug: string;
  display_name?: string;
  description?: string;
  status: string;
  workspace_mode: string;
  head_sha?: string | null;
  upstream_head_sha?: string | null;
  last_sync_at?: string | null;
  clone_error?: string | null;
  sync_error?: string | null;
  settings?: Record<string, unknown>;
}

export interface PatchRow {
  position: number;
  branch_name: string;
  status: string;
  last_rebuild_result?: string | null;
}

export interface PatchStatus {
  patches: PatchRow[];
  last_rebuild: { id: string; status: string } | null;
}

export interface JobRow {
  kind: string;
  status: string;
  at: string;
}

export interface EventRow {
  event_type: string;
  timestamp: string;
}

export interface WorkspaceSections {
  workspace: WorkspaceFields;
  /** Absent for a standard workspace; undefined where the read failed. */
  patchStatus?: PatchStatus | undefined;
  jobs?: JobRow[] | undefined;
  events?: EventRow[] | undefined;
}

/** The workspace's own fields, as a definition list. */
const Fields: FC<{ workspace: WorkspaceFields }> = ({ workspace }) => (
  <dl class="fields">
    <dt>Status</dt>
    <dd>
      <StatusBadge {...badgeFor(workspace.status)} />
    </dd>
    <dt>Mode</dt>
    <dd>{workspace.workspace_mode}</dd>
    {workspace.description === undefined || workspace.description.length === 0 ? null : (
      <>
        <dt>Description</dt>
        <dd>{workspace.description}</dd>
      </>
    )}
    <dt>Head</dt>
    <dd class="mono">{workspace.head_sha ?? 'none yet'}</dd>
    {workspace.upstream_head_sha === null || workspace.upstream_head_sha === undefined ? null : (
      <>
        <dt>Upstream head</dt>
        <dd class="mono">{workspace.upstream_head_sha}</dd>
      </>
    )}
    <dt>Last sync</dt>
    <dd>{saidWhen(workspace.last_sync_at)}</dd>
    {workspace.clone_error === null || workspace.clone_error === undefined ? null : (
      <>
        <dt>Clone error</dt>
        <dd>{workspace.clone_error}</dd>
      </>
    )}
    {workspace.sync_error === null || workspace.sync_error === undefined ? null : (
      <>
        <dt>Sync error</dt>
        <dd>{workspace.sync_error}</dd>
      </>
    )}
  </dl>
);

/** The settings, which a person reads to know why something happened. */
const Settings: FC<{ settings: Record<string, unknown> }> = ({ settings }) => (
  <dl class="fields">
    {Object.entries(settings).map(([name, value]) => (
      <>
        <dt key={name}>{name}</dt>
        <dd>{typeof value === 'string' ? value : JSON.stringify(value)}</dd>
      </>
    ))}
  </dl>
);

/** The patch stack, in the order it is applied. */
const Patches: FC<{ status: PatchStatus }> = ({ status }) => (
  <>
    <h2>Patches</h2>
    {status.patches.length === 0 ? (
      <Empty>This workspace carries no patches yet.</Empty>
    ) : (
      <ul class="repo-list" aria-label="The patches this workspace carries">
        {status.patches
          .toSorted((left, right) => left.position - right.position)
          .map((patch) => (
            <li key={patch.branch_name}>
              <div class="repo-name">
                {`${String(patch.position)}. ${patch.branch_name}`}{' '}
                <StatusBadge {...badgeFor(patch.status)} />
              </div>
              <div class="repo-meta">
                <span>
                  {patch.last_rebuild_result === null || patch.last_rebuild_result === undefined
                    ? 'not in the latest rebuild'
                    : `latest rebuild: ${patch.last_rebuild_result}`}
                </span>
              </div>
            </li>
          ))}
      </ul>
    )}
    <h3>Latest rebuild</h3>
    {status.last_rebuild === null ? (
      <p>No rebuild has run for this workspace yet.</p>
    ) : (
      <p>
        <span class="mono">{status.last_rebuild.id}</span>{' '}
        <StatusBadge {...badgeFor(status.last_rebuild.status)} />
      </p>
    )}
  </>
);

export const Workspace: FC<{ signedInAs: string; sections: WorkspaceSections }> = ({
  signedInAs,
  sections,
}) => {
  const { workspace, patchStatus, jobs, events } = sections;
  return (
    <Layout title={workspace.slug} signedInAs={signedInAs}>
      <h1>{workspace.slug}</h1>
      {workspace.display_name === undefined || workspace.display_name === workspace.slug ? null : (
        <p>{workspace.display_name}</p>
      )}

      <Fields workspace={workspace} />

      {workspace.workspace_mode === 'carry_patch' ? (
        patchStatus === undefined ? (
          <>
            <h2>Patches</h2>
            <CouldNotRead what="The patches" />
          </>
        ) : (
          <Patches status={patchStatus} />
        )
      ) : null}

      <h2>Settings</h2>
      {workspace.settings === undefined || Object.keys(workspace.settings).length === 0 ? (
        <p>This workspace uses the deployment&apos;s defaults.</p>
      ) : (
        <Settings settings={workspace.settings} />
      )}

      <h2>Recent jobs</h2>
      {jobs === undefined ? (
        <CouldNotRead what="The jobs" />
      ) : jobs.length === 0 ? (
        <p>No jobs have run for this workspace.</p>
      ) : (
        <Timeline
          label="The jobs that have run"
          entries={jobs.map((job) => ({
            what: `${job.kind} — ${job.status}`,
            when: saidWhen(job.at),
          }))}
        />
      )}

      <h2>Recent events</h2>
      {events === undefined ? (
        <CouldNotRead what="The events" />
      ) : events.length === 0 ? (
        <p>No events have been recorded for this workspace.</p>
      ) : (
        <Timeline
          label="What happened recently"
          entries={events.map((event) => ({
            what: event.event_type,
            when: saidWhen(event.timestamp),
          }))}
        />
      )}
    </Layout>
  );
};
