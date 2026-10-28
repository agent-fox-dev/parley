import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import { FLUSH, concatBytes, encodePkt, encodeSpecial } from './pktline.js';

/**
 * Serving a push.
 *
 * The store offers no pre-receive hook, so the service decides each ref
 * before any of the push reaches the store, relays what it accepted and
 * merges the outcome with its own refusals. The client receives one
 * report covering every ref it sent, which is how it learns what
 * happened: a push that reports success for a ref it did not apply is a
 * lost commit.
 */
export interface RefOutcome {
  name: string;
  ok: boolean;
  /** Why it was refused, as the client will print it. */
  reason?: string;
}

export type RefDecision = { accept: true } | { accept: false; reason: string };

/** What the service makes of one ref of a push, before it is relayed. */
export type Decider = (command: RefChange) => Promise<RefDecision>;

/** git's per-ref report, in the framing a client parses. */
export function reportOf(outcomes: readonly RefOutcome[]): Uint8Array {
  return concatBytes([
    // The objects were taken whatever became of the refs: a ref the
    // service refused is a decision, not a broken pack.
    encodePkt('unpack ok\n'),
    ...outcomes.map((outcome) =>
      encodePkt(
        outcome.ok ? `ok ${outcome.name}\n` : `ng ${outcome.name} ${outcome.reason ?? 'refused'}\n`,
      ),
    ),
    encodeSpecial(FLUSH),
  ]);
}

export interface PushRequest {
  repos: RepositoryStore;
  repository: string;
  commands: readonly RefChange[];
  pack: Uint8Array;
  /** How each ref is decided; everything is accepted without one. */
  decide?: Decider;
  /** Run once the store has taken what it was given, whatever it was. */
  afterward?: (outcomes: readonly RefOutcome[]) => Promise<void>;
}

/**
 * Decides each ref, relays the ones it accepted, and answers with the
 * report. A store that will not take what it was sent refuses every ref
 * it was sent, because the store applies all of them or none.
 */
export async function servePush(request: PushRequest): Promise<Uint8Array> {
  const outcomes: RefOutcome[] = [];
  const accepted: RefChange[] = [];

  for (const command of request.commands) {
    const decision: RefDecision =
      request.decide === undefined ? { accept: true } : await request.decide(command);
    if (decision.accept) {
      accepted.push(command);
      outcomes.push({ name: command.name, ok: true });
      continue;
    }
    outcomes.push({ name: command.name, ok: false, reason: decision.reason });
  }

  if (accepted.length === 0) {
    await request.afterward?.(outcomes);
    return reportOf(outcomes);
  }

  try {
    // A push that deletes refs carries no pack at all, and there is
    // nothing for the store to unpack; the refs still have to move.
    if (request.pack.byteLength === 0) {
      await request.repos.updateRefs(request.repository, accepted);
    } else {
      await request.repos.receivePack(request.repository, request.pack, accepted);
    }
  } catch (err) {
    const reason = err instanceof Error ? err.message : 'the store refused this push';
    const refused = outcomes.map((outcome) =>
      outcome.ok ? { name: outcome.name, ok: false, reason } : outcome,
    );
    await request.afterward?.(refused);
    return reportOf(refused);
  }

  await request.afterward?.(outcomes);
  return reportOf(outcomes);
}
