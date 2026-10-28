import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import type {
  Clock,
  EventBus,
  MetricsSink,
  RelationalStore,
  Subscription,
} from '../platform/index.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { findWorkspace } from '../store/workspaces.js';
import { visibleWorkspaces } from '../audit/query.js';

/**
 * The live stream of events, as server-sent events.
 *
 * Three rules shape it. The set of workspaces a stream may see is
 * fixed when it opens, so a membership change takes effect on the
 * next connection and not in the middle of one. A client that stops
 * reading is let go rather than held for ever, and one that reads
 * slowly loses the oldest events rather than stalling the service.
 * And a refusal is an ordinary response: a client handed a stream
 * and an error frame would have to parse the stream to learn it was
 * refused.
 */
export interface EventStreamConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
  bus: EventBus;
  metrics?: MetricsSink;
  /** How many streams this deployment serves at once. */
  limit?: number;
  /** How often a heartbeat is sent, and how long silence is borne. */
  heartbeatMs?: number;
  idleMs?: number;
  /** How often the stream looks for what was published. */
  tickMs?: number;
  /** How the stream waits between looks; a suite gives it its own. */
  wait?: (ms: number) => Promise<void>;
}

const DEFAULT_STREAM_LIMIT = 100;
const HEARTBEAT_MS = 30_000;
const IDLE_MS = 60_000;
const TICK_MS = 250;

function frame(event: string, data: unknown): string {
  return `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

/** The workspaces this stream may see, settled before it opens. */
async function visibleTo(c: Context, config: EventStreamConfig) {
  const identity = identityOf(c);
  const asked = new URL(c.req.url).searchParams.get('workspace');

  if (asked !== null && asked.length > 0) {
    const workspace = await findWorkspace(config.store, asked);
    if (
      workspace === undefined ||
      (await accessLevel(config.store, identity, workspace)) === undefined
    ) {
      throw new HttpError(404, 'workspace not found');
    }
    return { workspaces: [workspace.slug] as readonly string[], workspace: workspace.slug };
  }

  return { workspaces: await visibleWorkspaces(config.store, identity) };
}

export function eventStreamRoutes(config: EventStreamConfig): RouteDefinition[] {
  const limit = config.limit ?? DEFAULT_STREAM_LIMIT;
  const heartbeatMs = config.heartbeatMs ?? HEARTBEAT_MS;
  const idleMs = config.idleMs ?? IDLE_MS;
  const tickMs = config.tickMs ?? TICK_MS;
  const wait =
    config.wait ?? ((ms: number) => new Promise<void>((settled) => setTimeout(settled, ms)));

  // One count per service, which is what the limit and the gauge are
  // both about: how many streams this component is serving now.
  let open = 0;
  const counted = () => config.metrics?.gauge('afhub_sse_connections').set({}, open);

  /**
   * The stream itself, once the connection has been admitted: a
   * heartbeat, then whatever the bus carries, until the client goes
   * away or stops reading.
   */
  function frames(subscription: Subscription, release: () => void): ReadableStream<Uint8Array> {
    const encoder = new TextEncoder();
    const begun = config.clock.now().getTime();
    let held: ReadableStreamDefaultController<Uint8Array> | undefined;
    let closed = false;
    let lastBeat = begun;
    let lastRead = begun;

    const finish = (): void => {
      if (closed) {
        return;
      }
      closed = true;
      subscription.close();
      release();
      try {
        held?.close();
      } catch {
        // The consumer closed it first, which is the ordinary way a
        // stream ends.
      }
    };

    const send = (text: string): void => {
      try {
        held?.enqueue(encoder.encode(text));
      } catch {
        finish();
      }
    };

    const beat = () => send(frame('heartbeat', { timestamp: config.clock.now().toISOString() }));

    const run = async (): Promise<void> => {
      for (;;) {
        await wait(tickMs);
        if (closed) {
          return;
        }
        const now = config.clock.now().getTime();

        for (const event of await subscription.take()) {
          send(
            frame('audit_event', {
              id: event.id,
              event_type: event.event_type,
              timestamp: event.timestamp,
              workspace: event.workspace,
              source: event.source,
              severity: event.severity,
            }),
          );
        }

        if (now - lastBeat >= heartbeatMs) {
          lastBeat = now;
          beat();
        }
        // A client that has read nothing for a minute is gone,
        // whatever its socket still says.
        if (now - lastRead >= idleMs) {
          finish();
          return;
        }
      }
    };

    return new ReadableStream<Uint8Array>({
      start: (controller) => {
        held = controller;
        // At once, so a client knows it is connected before anything
        // has happened.
        beat();
        void run();
      },
      // Reading is what keeps the connection alive, so a consumer
      // that stopped is noticed rather than served for ever.
      pull: () => {
        lastRead = config.clock.now().getTime();
      },
      cancel: () => {
        finish();
      },
    });
  }

  async function serve(c: Context): Promise<Response> {
    const category = new URL(c.req.url).searchParams.get('category');
    if (category !== null && category !== 'hub' && category !== 'agent') {
      throw new HttpError(400, 'category must be hub or agent');
    }

    // Everything that can refuse the connection happens before a byte
    // of the stream exists: a client handed a stream and an error
    // frame would have to parse the stream to learn it was refused.
    const seen = await visibleTo(c, config);
    if (open >= limit) {
      throw new HttpError(503, 'this deployment is serving as many event streams as it can');
    }

    const subscription = await config.bus.subscribe({
      workspaces: seen.workspaces,
      ...(seen.workspace === undefined ? {} : { workspace: seen.workspace }),
      ...(category === null ? {} : { category }),
    });

    open += 1;
    counted();

    return new Response(
      frames(subscription, () => {
        open -= 1;
        counted();
      }),
      {
        headers: {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
          connection: 'keep-alive',
        },
      },
    );
  }

  return [
    {
      path: `${config.mountPoint}/events`,
      scope: 'audit:read',
      handler: serve,
    },
  ];
}
