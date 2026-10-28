import type { Clock, RelationalStore } from '../platform/index.js';

/**
 * The hourly sweep of the delivery log. A delivery is a record of
 * something a forge said, useful while somebody is working out why a
 * webhook did not do what they expected and of no use a week later.
 */
export const KEEP_DAYS = 7;

export async function pruneDeliveries(store: RelationalStore, clock: Clock): Promise<void> {
  const oldest = new Date(clock.now().getTime() - KEEP_DAYS * 86_400_000).toISOString();
  await store.run('DELETE FROM forge_deliveries WHERE received_at < ?', [oldest]);
  // A workspace that is gone keeps nothing, whatever the age.
  await store.run(
    'DELETE FROM forge_deliveries WHERE workspace_slug NOT IN (SELECT slug FROM workspaces)',
  );
}
