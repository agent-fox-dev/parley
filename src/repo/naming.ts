import { createHash } from 'node:crypto';

/** How much of the workspace's slug the repository name carries. */
const SLUG_BUDGET = 64;
const HASH_LENGTH = 12;

/**
 * The name a workspace's repository is kept under: its slug, so a person
 * reading the store knows what they are looking at, and a short hash of
 * its id, so two workspaces never collide and a slug reused after a
 * delete gets a repository of its own.
 *
 * It is derived rather than stored, so the mapping cannot drift from the
 * workspace record and a repository orphaned by a failed delete can still
 * be found.
 */
export function repositoryName(slug: string, workspaceId: string): string {
  const digest = createHash('sha256').update(`${slug}\u0000${workspaceId}`).digest('hex');
  return `${slug.slice(0, SLUG_BUDGET)}-${digest.slice(0, HASH_LENGTH)}`;
}
