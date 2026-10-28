import type { Clock } from './clock.js';
import type { BusEvent, BusFilter, EventBus, Subscription } from './eventbus.js';
import type { RelationalStore } from './store.js';

/**
 * The bus in the relational store, so every component of a
 * deployment publishes to one place and every stream reads from it —
 * the same reasoning as the workspace lock and the job queue (ADR 5),
 * and the only shape that works where the components share nothing
 * but the database.
 *
 * A row here is a delivery, not a record: the audit log is the
 * hub_events and run_events tables, and these rows are swept once
 * they are older than any live stream could still want them.
 */
export interface StoreBusOptions {
  /** How many events one subscriber may fall behind by. */
  bufferSize?: number;
  /** How long a delivery is kept for subscribers that are behind. */
  keepMs?: number;
}

/** What a client may fall behind by before it loses the oldest events. */
const BUFFER = 256;

/** How long a delivery is kept. Longer than any stream's idle timeout. */
const KEEP_MS = 5 * 60 * 1000;

interface BusRow {
  [column: string]: unknown;
  id: string;
  event_id: string;
  event_type: string;
  source: string;
  timestamp: string;
  workspace: string | null;
  severity: string;
}

export function storeBus(
  store: RelationalStore,
  clock: Clock,
  options: StoreBusOptions = {},
): EventBus {
  const bufferSize = options.bufferSize ?? BUFFER;
  const keepMs = options.keepMs ?? KEEP_MS;
  let published = 0;

  return {
    async publish(event: BusEvent): Promise<void> {
      await store.run(
        `INSERT INTO event_bus (id, event_id, event_type, source, timestamp, workspace, severity, published_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        [
          crypto.randomUUID(),
          event.id,
          event.event_type,
          event.source,
          event.timestamp,
          event.workspace,
          event.severity,
          clock.now().toISOString(),
        ],
      );

      // Swept now and then rather than on every publish: a delivery
      // nobody can still be waiting for is only taking up room.
      published += 1;
      if (published % 50 === 0) {
        await store.run('DELETE FROM event_bus WHERE published_at < ?', [
          new Date(clock.now().getTime() - keepMs).toISOString(),
        ]);
      }
    },

    async subscribe(filter: BusFilter): Promise<Subscription> {
      // Where the subscriber starts: what is already published is
      // not theirs, so the mark is taken before the first look.
      const opened = (
        await store.all<{ id: string }>(
          'SELECT id FROM event_bus ORDER BY published_at DESC, id DESC LIMIT 1',
        )
      )[0];
      let since =
        opened === undefined
          ? ''
          : ((
              await store.all<{ published_at: string }>(
                'SELECT published_at FROM event_bus WHERE id = ?',
                [opened.id],
              )
            )[0]?.published_at ?? '');
      let seen = new Set<string>(opened === undefined ? [] : [opened.id]);
      let closed = false;

      const where = ['published_at >= ?'];
      const params: unknown[] = [since];
      if (filter.category !== undefined) {
        where.push('source = ?');
        params.push(filter.category);
      }
      if (filter.workspace !== undefined) {
        where.push('workspace = ?');
        params.push(filter.workspace);
      } else if (filter.workspaces !== 'every') {
        const slugs = [...filter.workspaces];
        where.push(
          slugs.length === 0 ? '1 = 0' : `workspace IN (${slugs.map(() => '?').join(', ')})`,
        );
        params.push(...slugs);
      }

      return {
        async take(): Promise<BusEvent[]> {
          if (closed) {
            return [];
          }
          const rows = await store.all<BusRow>(
            `SELECT id, event_id, event_type, source, timestamp, workspace, severity, published_at
               FROM event_bus
              WHERE ${where.join(' AND ')}
              ORDER BY published_at, id`,
            [since, ...params.slice(1)],
          );

          const fresh = rows.filter((row) => !seen.has(row.id));
          if (fresh.length === 0) {
            return [];
          }
          // A client that has fallen behind loses the oldest events
          // rather than the stream stalling or the service growing.
          const kept = fresh.slice(-bufferSize);
          const last = fresh.at(-1) as BusRow;
          since = String(last['published_at']);
          seen = new Set(
            rows.filter((row) => String(row['published_at']) === since).map((row) => row.id),
          );

          return kept.map((row) => ({
            id: row.event_id,
            event_type: row.event_type,
            source: row.source === 'agent' ? 'agent' : 'hub',
            timestamp: row.timestamp,
            workspace: row.workspace,
            severity: row.severity,
          }));
        },

        close(): void {
          closed = true;
        },
      };
    },
  };
}
