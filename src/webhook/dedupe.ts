import { HttpError } from '../http/errors.js';
import type { RelationalStore } from '../platform/index.js';
import { type DeliveryRecord, recordDelivery } from './log.js';

/**
 * Processing a delivery once.
 *
 * The row is the claim: it is written before anything is relayed,
 * keyed by the workspace and the forge's own delivery id, so two
 * copies arriving together compete for one primary key and the one
 * that loses is the duplicate. Nothing in memory could do that —
 * the copies may reach different components.
 *
 * A store that fails is not a duplicate. The delivery is refused
 * with 500 so the forge redelivers it and the redelivery is
 * processed in full; answering 202 would lose it for good.
 */
export type Claim = 'recorded' | 'duplicate';

export async function claimDelivery(
  store: RelationalStore,
  delivery: DeliveryRecord,
): Promise<Claim> {
  try {
    return await recordDelivery(store, delivery);
  } catch {
    throw new HttpError(500, 'the delivery could not be recorded');
  }
}
