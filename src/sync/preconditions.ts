import { HttpError } from '../http/errors.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { effectiveSettings } from '../workspace/settings.js';

/**
 * Whether a sync may run at all.
 *
 * Both conditions are the workspace's own, and both are checked before
 * anything is fetched: a workspace that is still cloning, or whose
 * owner switched syncing off, should cost the remote nothing. Whether
 * another operation holds the workspace is not here — that is the
 * lock's answer, and a sync in progress is not a state the workspace
 * stores.
 */
export function syncRefusal(workspace: WorkspaceRecord): HttpError | undefined {
  if (workspace.status !== 'ready') {
    return new HttpError(
      400,
      `this workspace is ${workspace.status}, and only a ready workspace can be synced`,
    );
  }
  if (effectiveSettings(workspace.settings)['sync_enabled'] !== true) {
    return new HttpError(400, 'syncing is switched off for this workspace');
  }
  return undefined;
}
