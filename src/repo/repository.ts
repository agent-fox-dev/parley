import type { Locks } from '../platform/lock.js';
import type { RepositoryStore } from '../platform/repostore.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { withWorkspaceLock } from '../workspace/lock.js';
import { repositoryName } from './naming.js';

/** The repository a workspace's record points at. */
export function repositoryOf(workspace: Pick<WorkspaceRecord, 'slug' | 'owner_id'>): string {
  return repositoryName(workspace.slug, workspace.owner_id);
}

/**
 * Brings a workspace's repository into being. Creating a workspace calls
 * it, and so does anything that finds the repository missing: the store
 * treats a second create as the state the caller wanted.
 */
export async function ensureRepository(
  repos: RepositoryStore,
  workspace: Pick<WorkspaceRecord, 'slug' | 'owner_id'>,
): Promise<string> {
  const name = repositoryOf(workspace);
  await repos.create(name);
  return name;
}

/**
 * Runs work that writes a workspace's repository, with the workspace lock
 * held. Two such operations never run at once for one workspace, and a
 * caller that finds the lock held is refused at once rather than waiting.
 */
export async function writingRepository<T>(
  locks: Locks,
  workspace: Pick<WorkspaceRecord, 'slug'>,
  work: () => Promise<T>,
): Promise<T> {
  return withWorkspaceLock(locks, workspace.slug, work);
}
