import type { WorkspaceRecord } from '../store/workspaces.js';
import { effectiveSettings } from './settings.js';

export type AccessLevel = 'admin' | 'owner' | 'member';

export interface SerialiseContext {
  access: AccessLevel;
  /** The organization's slug, or null where the workspace has none. */
  orgSlug: string | null;
  /** The deployment's external URL, or undefined where it is unset. */
  externalUrl: string | undefined;
}

/**
 * The clone URL on this service's git server. It needs both the
 * deployment's external URL and an organization to name, so it is null
 * whenever either is missing.
 */
export function hubUrl(
  workspace: WorkspaceRecord,
  orgSlug: string | null,
  externalUrl: string | undefined,
): string | null {
  if (externalUrl === undefined || externalUrl.length === 0 || orgSlug === null) {
    return null;
  }
  return `${externalUrl.replace(/\/$/, '')}/git/${orgSlug}/${workspace.slug}.git`;
}

/** The workspace as every route that returns one returns it. */
export function serialiseWorkspace(workspace: WorkspaceRecord, context: SerialiseContext) {
  return {
    slug: workspace.slug,
    git_url: workspace.git_url,
    hub_url: hubUrl(workspace, context.orgSlug, context.externalUrl),
    branch: workspace.branch,
    owner_id: workspace.owner_id,
    org_id: workspace.org_id,
    org_slug: context.orgSlug,
    access: context.access,
    status: workspace.status,
    display_name: workspace.display_name.length > 0 ? workspace.display_name : workspace.slug,
    description: workspace.description,
    head_sha: workspace.head_sha,
    clone_error: workspace.clone_error,
    upstream_head_sha: workspace.upstream_head_sha,
    last_sync_at: workspace.last_sync_at,
    sync_error: workspace.sync_error,
    workspace_mode: workspace.workspace_mode,
    upstream_url: workspace.upstream_url,
    integration_branch: workspace.integration_branch,
    settings: effectiveSettings(workspace.settings),
    created_at: workspace.created_at,
    updated_at: workspace.updated_at,
  };
}
