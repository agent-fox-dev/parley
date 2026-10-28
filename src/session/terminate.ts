import type { Clock, RelationalStore } from '../platform/index.js';

/**
 * Ending the sessions of a workspace that is being retired.
 *
 * An agent whose workspace is archived under it is not going to
 * report that it finished, so the hub says what happened instead:
 * `terminated` is the one status a client cannot set, because it
 * means "the hub ended this", not "the run ended".
 */
export async function terminateSessions(
  store: RelationalStore,
  clock: Clock,
  slug: string,
): Promise<number> {
  const at = clock.now().toISOString();
  const active = await store.all<{ id: string }>(
    "SELECT id FROM sessions WHERE workspace_slug = ? AND status = 'active'",
    [slug],
  );
  if (active.length === 0) {
    return 0;
  }

  await store.run(
    `UPDATE sessions SET status = 'terminated', ended_at = ?
     WHERE workspace_slug = ? AND status = 'active'`,
    [at, slug],
  );
  return active.length;
}
