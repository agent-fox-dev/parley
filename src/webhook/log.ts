import type { RelationalStore } from '../platform/index.js';

/**
 * What the hub records about a delivery.
 *
 * A row says what arrived and what the hub made of it, and holds
 * none of the payload: a delivery's body may carry anything at all,
 * and a log that kept it would be the one place a secret in an issue
 * title lived for seven days.
 *
 * The row is also how a delivery is processed once. It is written
 * before anything is relayed, keyed by the workspace and the forge's
 * own delivery id, so a redelivery finds it already there.
 */
export type DeliveryOutcome = 'relayed' | 'ignored' | 'repository_mismatch' | 'malformed';

export interface DeliveryRecord {
  workspace_slug: string;
  delivery_id: string;
  forge: string;
  /** The forge's event name, as it sent it. */
  event: string;
  action?: string;
  /** Which of the workspace's repositories it was about, where one matched. */
  target?: 'origin' | 'upstream' | null;
  outcome?: DeliveryOutcome;
  relayed?: string[];
  received_at: string;
}

/** How much of a forge's event name is kept. */
const EVENT_BYTES = 64;

function cut(value: string, bytes: number): string {
  const encoded = new TextEncoder().encode(value);
  if (encoded.length <= bytes) {
    return value;
  }
  return new TextDecoder().decode(encoded.slice(0, bytes)).replace(/�$/, '');
}

/**
 * Writes the row, or reports that this workspace already has one for
 * this delivery. The insert is what decides: two copies arriving
 * together race for the same primary key, and the one that loses is
 * the duplicate.
 */
export async function recordDelivery(
  store: RelationalStore,
  delivery: DeliveryRecord,
): Promise<'recorded' | 'duplicate'> {
  const held = await store.all<{ delivery_id: string }>(
    'SELECT delivery_id FROM forge_deliveries WHERE workspace_slug = ? AND delivery_id = ?',
    [delivery.workspace_slug, delivery.delivery_id],
  );
  if (held.length > 0) {
    return 'duplicate';
  }

  try {
    await store.run(
      `INSERT INTO forge_deliveries
         (workspace_slug, delivery_id, forge, event, action, target, outcome, relayed, received_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      [
        delivery.workspace_slug,
        delivery.delivery_id,
        delivery.forge,
        cut(delivery.event, EVENT_BYTES),
        delivery.action ?? '',
        delivery.target ?? null,
        delivery.outcome ?? 'ignored',
        JSON.stringify(delivery.relayed ?? []),
        delivery.received_at,
      ],
    );
  } catch (err) {
    // A primary key that refused it is another copy of this
    // delivery getting there first, which is the duplicate rule. A
    // store that failed is not, and the caller must hear about it:
    // answering 202 would lose a delivery for good.
    const now = await store
      .all<{ delivery_id: string }>(
        'SELECT delivery_id FROM forge_deliveries WHERE workspace_slug = ? AND delivery_id = ?',
        [delivery.workspace_slug, delivery.delivery_id],
      )
      .catch(() => []);
    if (now.length > 0) {
      return 'duplicate';
    }
    throw err;
  }
  return 'recorded';
}

/** Says what became of a delivery, once the relay has finished. */
export async function settleDelivery(
  store: RelationalStore,
  delivery: { workspace_slug: string; delivery_id: string },
  outcome: { outcome: DeliveryOutcome; target?: 'origin' | 'upstream' | null; relayed?: string[] },
): Promise<void> {
  await store.run(
    `UPDATE forge_deliveries SET outcome = ?, target = ?, relayed = ?
      WHERE workspace_slug = ? AND delivery_id = ?`,
    [
      outcome.outcome,
      outcome.target ?? null,
      JSON.stringify(outcome.relayed ?? []),
      delivery.workspace_slug,
      delivery.delivery_id,
    ],
  );
}

export interface DeliveryQuery {
  since?: string;
  limit: number;
}

/** One row as a client reads it. */
export interface ListedDelivery {
  delivery_id: string;
  forge: string;
  event: string;
  action: string;
  target: string | null;
  outcome: string;
  relayed: string[];
  received_at: string;
}

/**
 * The workspace's deliveries, newest first. Rows of one second are
 * ordered by when they were recorded, latest first, so a log read
 * twice reads the same way.
 */
export async function listDeliveries(
  store: RelationalStore,
  slug: string,
  query: DeliveryQuery,
): Promise<ListedDelivery[]> {
  const rows = await store.all<{
    delivery_id: string;
    forge: string;
    event: string;
    action: string;
    target: string | null;
    outcome: string;
    relayed: string;
    received_at: string;
    rowid: number;
  }>(
    `SELECT delivery_id, forge, event, action, target, outcome, relayed, received_at, rowid
       FROM forge_deliveries
      WHERE workspace_slug = ?${query.since === undefined ? '' : ' AND received_at >= ?'}
      ORDER BY received_at DESC, rowid DESC
      LIMIT ?`,
    query.since === undefined ? [slug, query.limit] : [slug, query.since, query.limit],
  );

  return rows.map((row) => ({
    delivery_id: row.delivery_id,
    forge: row.forge,
    event: row.event,
    action: row.action,
    target: row.target,
    outcome: row.outcome,
    relayed: JSON.parse(row.relayed) as string[],
    received_at: row.received_at,
  }));
}

/** When the workspace's latest delivery arrived, of any outcome. */
export async function lastDeliveryAt(
  store: RelationalStore,
  slug: string,
): Promise<string | undefined> {
  const rows = await store.all<{ received_at: string }>(
    'SELECT received_at FROM forge_deliveries WHERE workspace_slug = ? ORDER BY received_at DESC, rowid DESC LIMIT 1',
    [slug],
  );
  return rows[0]?.received_at;
}
