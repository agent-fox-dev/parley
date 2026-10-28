/**
 * How a recorded event reaches the streams watching for it.
 *
 * It is a platform interface and not a list in memory because the
 * component that records an event is rarely the one serving the
 * stream: on Cloudflare they are different isolates, and on a host
 * they may be different processes against one database. A bus that
 * only carried events within one process would work in every test
 * and almost never in a deployment.
 */
export interface BusEvent {
  id: string;
  event_type: string;
  source: 'hub' | 'agent';
  timestamp: string;
  /** The workspace the event belongs to, or null where it has none. */
  workspace: string | null;
  severity: string;
}

export interface BusFilter {
  /** The workspaces this subscriber may see, fixed when it opened. */
  workspaces: readonly string[] | 'every';
  category?: 'hub' | 'agent';
  /** One workspace, where the subscriber asked for one alone. */
  workspace?: string;
}

export interface Subscription {
  /** The events published since the last look, oldest first. */
  take(): Promise<BusEvent[]>;
  close(): void;
}

export interface EventBus {
  publish(event: BusEvent): Promise<void>;
  /** A subscription that sees what is published from now on. */
  subscribe(filter: BusFilter): Promise<Subscription>;
}
