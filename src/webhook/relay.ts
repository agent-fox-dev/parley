import type { LogSink } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { type EventRecorder, record } from '../audit/recorder.js';
import { repositoryFrom } from '../forge/target.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { type DeliveryOutcome, settleDelivery } from './log.js';
import type { MappedDelivery } from './mapping.js';

/**
 * What becomes of a delivery once the hub believes it.
 *
 * The forge is answered before any of this runs, so nothing here
 * may hold its connection; and the hub never contacts the forge,
 * because a route anyone can call must not be a way of making the
 * hub do things.
 */
export type DeliveryTarget = 'origin' | 'upstream' | null;

/** A remote URL as `owner/name`, folded for comparison. */
function pathOf(url: string): string | undefined {
  const found = repositoryFrom(url);
  return found === undefined ? undefined : `${found.owner}/${found.name}`.toLowerCase();
}

/**
 * Which of the workspace's repositories a delivery is about. The
 * comparison ignores case, because a forge may spell an owner as
 * the person typed it and the workspace as they cloned it.
 */
export function targetOf(workspace: WorkspaceRecord, repository: string): DeliveryTarget {
  const said = repository.toLowerCase();

  if (pathOf(workspace.git_url) === said) {
    return 'origin';
  }
  const upstream = workspace.upstream_url ?? '';
  if (
    workspace.workspace_mode === 'carry_patch' &&
    upstream.length > 0 &&
    pathOf(upstream) === said
  ) {
    return 'upstream';
  }
  return null;
}

export interface RelayDeps {
  store: RelationalStore;
  events?: EventRecorder;
  log?: LogSink;
}

/**
 * Relays a delivery's events and settles its log row. Called after
 * the forge has been answered, and therefore responsible for
 * finishing whatever the forge does next.
 */
export async function relayDelivery(
  deps: RelayDeps,
  workspace: WorkspaceRecord,
  delivery: { id: string; mapped: MappedDelivery; target: DeliveryTarget },
): Promise<{ outcome: DeliveryOutcome; relayed: string[] }> {
  const { mapped, target } = delivery;

  if (mapped.events.length === 0) {
    await settleDelivery(
      deps.store,
      { workspace_slug: workspace.slug, delivery_id: delivery.id },
      { outcome: 'ignored', target },
    );
    return { outcome: 'ignored', relayed: [] };
  }
  if (target === null) {
    // It had events to relay and was about neither of this
    // workspace's repositories, which is worth saying rather than
    // calling it ignored.
    await settleDelivery(
      deps.store,
      { workspace_slug: workspace.slug, delivery_id: delivery.id },
      { outcome: 'repository_mismatch', target: null },
    );
    return { outcome: 'repository_mismatch', relayed: [] };
  }

  const relayed: string[] = [];
  for (const event of mapped.events) {
    await record(
      deps.events,
      {
        type: event.type,
        workspace: workspace.slug,
        resourceId: event.resourceId,
        // The forge did this, not a caller of ours.
        actor: { id: '', type: 'forge' },
        metadata: { ...event.metadata, target },
      },
      deps.log,
    );
    relayed.push(event.type);
  }

  await settleDelivery(
    deps.store,
    { workspace_slug: workspace.slug, delivery_id: delivery.id },
    { outcome: 'relayed', target, relayed },
  );
  return { outcome: 'relayed', relayed };
}
