import type { Context } from 'hono';
import { newIdentifier } from '../auth/credentials.js';
import { type Identity, identityOf } from '../auth/middleware.js';
import type { LogSink } from '../http/router.js';
import type { Clock, EventBus, MetricsSink, RelationalStore } from '../platform/index.js';
import { shapeOf } from './events_table.js';

/**
 * Where the service writes down what it did.
 *
 * Two rules shape it. The event's shape comes from the declaration,
 * not from the call site: a caller says what happened and hands over
 * values, and the recorder keeps the keys the reference lists and no
 * others. And recording never changes the answer of the request that
 * caused it — every failure here is swallowed and logged, because an
 * archive that worked is not a failed archive because the writing
 * down failed.
 */
export interface Actor {
  id: string;
  type: string;
}

export interface RecordedEvent {
  /** A type the declaration names. */
  type: string;
  /** The workspace the event belongs to, or null where it has none. */
  workspace?: string | null;
  resourceId?: string;
  /** Values for the declared metadata keys; anything else is dropped. */
  metadata?: Readonly<Record<string, unknown>>;
  /** Overrides the caller as the actor, where the reference names one. */
  actor?: Actor;
  severity?: 'info' | 'warning' | 'error' | 'critical';
}

export interface BoundRefusal {
  tokenId: string;
  boundWorkspace: string;
  requestedWorkspace?: string;
  method: string;
  route: string;
  status: number;
  surface: 'api' | 'git';
}

export interface EventRecorder {
  /** Writes one event down. Never throws and never rejects. */
  record(event: RecordedEvent): Promise<void>;
  /**
   * A bound token's refusal, which is recorded at most once per token
   * per minute with the refusals held back since counted into it.
   */
  refusal(refusal: BoundRefusal): Promise<void>;
}

export interface RecorderDeps {
  store: RelationalStore;
  clock: Clock;
  metrics?: MetricsSink;
  log?: LogSink;
  /** Where a recorded event is published for the streams watching. */
  bus?: EventBus;
}

/** How long one bound token's refusal event stands for the rest. */
const REFUSAL_WINDOW_MS = 60_000;

/** The actor an event carries when the caller is the one who acted. */
export function actorOf(identity: Identity | undefined): Actor {
  if (identity === undefined) {
    return { id: '', type: '' };
  }
  return { id: identity.userId ?? '', type: identity.kind };
}

/** The same, read from the request the route is answering. */
export function actorIn(c: Context): Actor {
  return actorOf(identityOf(c));
}

/**
 * Only the keys the declaration lists, in its order, and only those the
 * caller gave a value for. A key the reference marks conditional is
 * simply absent when the caller passes nothing for it.
 */
function metadataFor(
  keys: readonly string[],
  given: Readonly<Record<string, unknown>> | undefined,
): Record<string, unknown> {
  const kept: Record<string, unknown> = {};
  for (const key of keys) {
    const value = given?.[key];
    if (value !== undefined) {
      kept[key] = value;
    }
  }
  return kept;
}

export function eventRecorder(deps: RecorderDeps): EventRecorder {
  const counted = (type: string) =>
    deps.metrics
      ?.counter('afhub_audit_events_total')
      .increment({ source: 'hub', event_type: type });

  async function insert(event: RecordedEvent, at: string, id: string): Promise<void> {
    const shape = shapeOf(event.type);
    if (shape === undefined) {
      // A type nobody declared would be an event no consumer can
      // filter for, so it is refused here rather than stored.
      throw new Error(`no declared shape for the event type ${event.type}`);
    }
    await deps.store.run(
      `INSERT INTO hub_events
         (id, event_type, timestamp, severity, workspace,
          actor_id, actor_type, resource_type, resource_id, action, metadata)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      [
        id,
        event.type,
        at,
        event.severity ?? 'info',
        event.workspace ?? null,
        event.actor?.id ?? '',
        event.actor?.type ?? shape.actor ?? '',
        shape.resource,
        event.resourceId ?? '',
        shape.action,
        JSON.stringify(metadataFor(shape.metadata, event.metadata)),
      ],
    );
    counted(event.type);

    // Published after it is stored, so a stream never carries an
    // event a reader of the log could not then find.
    await deps.bus?.publish({
      id,
      event_type: event.type,
      source: 'hub',
      timestamp: at,
      workspace: event.workspace ?? null,
      severity: event.severity ?? 'info',
    });
  }

  return {
    async record(event) {
      await insert(event, deps.clock.now().toISOString(), newIdentifier());
    },

    async refusal(refusal) {
      {
        const now = deps.clock.now();
        const held = (
          await deps.store.all<{
            event_id: string;
            window_started_at: string;
            suppressed: number;
          }>(
            'SELECT event_id, window_started_at, suppressed FROM bound_token_refusals WHERE token_id = ?',
            [refusal.tokenId],
          )
        )[0];

        const within =
          held !== undefined &&
          now.getTime() - Date.parse(held.window_started_at) < REFUSAL_WINDOW_MS;

        if (within) {
          // The event already recorded stands for this refusal too, so
          // the count it carries is raised rather than a second written.
          const suppressed = held.suppressed + 1;
          await deps.store.run(
            'UPDATE bound_token_refusals SET suppressed = ? WHERE token_id = ?',
            [suppressed, refusal.tokenId],
          );
          const stored = (
            await deps.store.all<{ metadata: string }>(
              'SELECT metadata FROM hub_events WHERE id = ?',
              [held.event_id],
            )
          )[0];
          if (stored !== undefined) {
            const metadata = JSON.parse(stored.metadata) as Record<string, unknown>;
            metadata['suppressed'] = suppressed;
            await deps.store.run('UPDATE hub_events SET metadata = ? WHERE id = ?', [
              JSON.stringify(metadata),
              held.event_id,
            ]);
          }
          return;
        }

        const id = newIdentifier();
        await insert(
          {
            type: 'hub.token.bound_refused',
            workspace: refusal.boundWorkspace,
            resourceId: refusal.tokenId,
            actor: { id: refusal.tokenId, type: 'pat' },
            severity: 'warning',
            metadata: {
              token_id: refusal.tokenId,
              bound_workspace: refusal.boundWorkspace,
              ...(refusal.requestedWorkspace === undefined
                ? {}
                : { requested_workspace: refusal.requestedWorkspace }),
              method: refusal.method,
              route: refusal.route,
              status: refusal.status,
              surface: refusal.surface,
              suppressed: 0,
            },
          },
          now.toISOString(),
          id,
        );
        await deps.store.run(
          `INSERT INTO bound_token_refusals (token_id, event_id, window_started_at, suppressed)
             VALUES (?, ?, ?, 0)
           ON CONFLICT (token_id) DO UPDATE
             SET event_id = excluded.event_id,
                 window_started_at = excluded.window_started_at,
                 suppressed = 0`,
          [refusal.tokenId, id, now.toISOString()],
        );
      }
    },
  };
}

/**
 * Writing an event down, where failing to is not a failure.
 *
 * Every call site goes through these rather than through the recorder
 * itself, so the rule that recording never changes a request's answer
 * holds whatever recorder the deployment was given — including none.
 */
export async function record(
  events: EventRecorder | undefined,
  event: RecordedEvent,
  log?: LogSink,
): Promise<void> {
  try {
    await events?.record(event);
  } catch (err) {
    log?.error('an event could not be recorded', err);
  }
}

export async function recordRefusal(
  events: EventRecorder | undefined,
  refusal: BoundRefusal,
  log?: LogSink,
): Promise<void> {
  try {
    await events?.refusal(refusal);
  } catch (err) {
    log?.error('a bound token refusal could not be recorded', err);
  }
}

/**
 * Which events a caller may be shown. A user sees the events of the
 * workspaces they can read; an event belonging to no workspace is the
 * deployment's own business and goes to admin tokens alone.
 */
export function visibleEventClause(
  identity: Identity | undefined,
  readable: readonly string[],
): { clause: string; params: string[] } {
  if (identity?.kind === 'admin_token') {
    return { clause: '1 = 1', params: [] };
  }
  if (readable.length === 0) {
    return { clause: '1 = 0', params: [] };
  }
  const places = readable.map(() => '?').join(', ');
  return { clause: `workspace IN (${places})`, params: [...readable] };
}
