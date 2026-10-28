import type { FC, PropsWithChildren } from 'hono/jsx';

/**
 * The three patterns every page is built from.
 *
 * They exist so that a workspace reads the same way wherever it
 * appears: a page that laid out its own list would drift from the
 * others the first time either changed.
 */
export interface RepoRow {
  /** Where the row leads. */
  href: string;
  name: string;
  /** The small facts under the name, each already in words. */
  meta: string[];
  badge?: { text: string; tone: BadgeTone };
}

export type BadgeTone = 'neutral' | 'success' | 'attention' | 'danger';

/** A status, in the colour its state earns. */
export const StatusBadge: FC<{ text: string; tone?: BadgeTone }> = ({ text, tone = 'neutral' }) => (
  <span class={tone === 'neutral' ? 'badge' : `badge badge-${tone}`}>{text}</span>
);

/** The list of workspaces, as GitHub lists repositories. */
export const RepoList: FC<{ rows: RepoRow[]; label: string }> = ({ rows, label }) => (
  <ul class="repo-list" aria-label={label}>
    {rows.map((row) => (
      <li key={row.href}>
        <div class="repo-name">
          <a href={row.href}>{row.name}</a>{' '}
          {row.badge === undefined ? null : (
            <StatusBadge text={row.badge.text} tone={row.badge.tone} />
          )}
        </div>
        <div class="repo-meta">
          {row.meta.map((said) => (
            <span key={said}>{said}</span>
          ))}
        </div>
      </li>
    ))}
  </ul>
);

export interface TimelineEntry {
  /** What happened, in the words the service uses for it. */
  what: string;
  when: string;
  detail?: string;
}

/** What happened, newest first. */
export const Timeline: FC<{ entries: TimelineEntry[]; label: string }> = ({ entries, label }) => (
  <ul class="timeline" aria-label={label}>
    {entries.map((entry) => (
      <li key={`${entry.when}-${entry.what}`}>
        <div class="mono">{entry.what}</div>
        {entry.detail === undefined ? null : <div>{entry.detail}</div>}
        <div class="when">{entry.when}</div>
      </li>
    ))}
  </ul>
);

/** A section that could not be read, said where the section would be. */
export const CouldNotRead: FC<{ what: string }> = ({ what }) => (
  <p class="notice notice-danger">{`${what} could not be read.`}</p>
);

/** A page, or a part of one, with nothing in it yet. */
export const Empty: FC<PropsWithChildren> = ({ children }) => <div class="empty">{children}</div>;
