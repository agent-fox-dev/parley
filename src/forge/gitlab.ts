import type { ForgeClient } from './client.js';
import type { Repository } from './target.js';
import {
  type ForgeApi,
  type ForgeLabel,
  type IssueChanges,
  type NewIssue,
  type NewPullRequest,
  isNotFound,
  labelSegment,
  type ForgeChangedFile,
  type ForgeCheckRun,
  type ForgeComment,
  type ForgeIssue,
  type ForgePullRequest,
  type ForgeRepository,
  type ForgeReview,
  type IssueQuery,
  flag,
  number,
  object,
  rows,
  text,
} from './types.js';

/**
 * GitLab, in the reference's own shapes. Six translations live here
 * and nowhere else (ADR 11): the project path as one encoded
 * segment, `description` for a body, `iid` for a number, `opened`
 * for open, `order_by`/`sort` for sort and direction, and the two
 * things GitLab does not have — reviews and check runs — answered
 * with its approvals and its pipelines.
 */
/** How many comments one read brings back before the thread is cut short. */
export const NOTE_PAGE = 100;

/** The project, as GitLab wants it in a path: one encoded segment. */
export function projectPath(repository: Repository): string {
  return `/projects/${encodeURIComponent(`${repository.owner}/${repository.name}`)}`;
}

/** GitLab says `opened`; the reference says `open`. */
function stateFrom(raw: unknown): string {
  const given = text(raw);
  return given === 'opened' ? 'open' : given;
}

/** And the other way round, for a filter the caller gave. */
export function stateFor(given: string): string {
  return given === 'open' ? 'opened' : given;
}

function issueFrom(raw: Record<string, unknown>): ForgeIssue {
  const labels = Array.isArray(raw['labels'])
    ? (raw['labels'] as unknown[]).map((label) => text(label))
    : [];
  return {
    number: number(raw['iid']),
    title: text(raw['title']),
    body: text(raw['description']),
    state: stateFrom(raw['state']),
    ...(typeof raw['web_url'] === 'string' ? { html_url: raw['web_url'] } : {}),
    author: { login: text(object(raw['author'])['username']) },
    ...(labels.length === 0 ? {} : { labels }),
    created_at: text(raw['created_at']),
    updated_at: text(raw['updated_at']),
    ...(typeof raw['closed_at'] === 'string' ? { closed_at: raw['closed_at'] } : {}),
    is_pr: false,
  };
}

function noteFrom(raw: Record<string, unknown>): ForgeComment {
  return {
    ...(raw['id'] === undefined ? {} : { id: number(raw['id']) }),
    body: text(raw['body']),
    user: { login: text(object(raw['author'])['username']) },
    created_at: text(raw['created_at']),
    ...(typeof raw['updated_at'] === 'string' ? { updated_at: raw['updated_at'] } : {}),
  };
}

function mergeRequestFrom(raw: Record<string, unknown>): ForgePullRequest {
  const state = stateFrom(raw['state']);
  return {
    number: number(raw['iid']),
    title: text(raw['title']),
    body: text(raw['description']),
    // GitLab's `merged` is a state rather than a flag of its own.
    state: state === 'merged' ? 'closed' : state,
    ...(typeof raw['web_url'] === 'string' ? { html_url: raw['web_url'] } : {}),
    draft: flag(raw['draft']) || flag(raw['work_in_progress']),
    merged: stateFrom(raw['state']) === 'merged',
    head_sha: text(raw['sha']),
    head_branch: text(raw['source_branch']),
    base_branch: text(raw['target_branch']),
    author: { login: text(object(raw['author'])['username']) },
    created_at: text(raw['created_at']),
    updated_at: text(raw['updated_at']),
    ...(typeof raw['closed_at'] === 'string' ? { closed_at: raw['closed_at'] } : {}),
    ...(typeof raw['merged_at'] === 'string' ? { merged_at: raw['merged_at'] } : {}),
  };
}

/** GitLab gives a diff rather than counts, so the counts are read from it. */
function changedLines(diff: string): { additions: number; deletions: number } {
  let additions = 0;
  let deletions = 0;
  for (const line of diff.split('\n')) {
    if (line.startsWith('+') && !line.startsWith('+++')) {
      additions += 1;
    }
    if (line.startsWith('-') && !line.startsWith('---')) {
      deletions += 1;
    }
  }
  return { additions, deletions };
}

export function gitlabApi(client: ForgeClient, repository: Repository): ForgeApi {
  const at = projectPath(repository);

  return {
    ...gitlabWrites(client, repository),
    async repository(): Promise<ForgeRepository> {
      const answer = await client.send({ method: 'GET', path: at });
      const raw = object(answer.body);
      // GitLab reports access as a level; the reference wants three
      // flags, and 30 (developer) and above may push.
      const level = number(object(object(raw['permissions'])['project_access'])['access_level']);
      return {
        full_name: text(raw['path_with_namespace']),
        default_branch: text(raw['default_branch']),
        private: text(raw['visibility']) !== 'public',
        archived: flag(raw['archived']),
        permissions: { push: level >= 30, pull: level >= 10, admin: level >= 50 },
      };
    },

    async issues(query: IssueQuery): Promise<ForgeIssue[]> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/issues`,
        query: {
          ...(query.state === undefined ? {} : { state: stateFor(query.state) }),
          ...(query.labels === undefined ? {} : { labels: query.labels }),
          ...(query.assignee === undefined ? {} : { assignee_username: query.assignee }),
          ...(query.sort === undefined ? {} : { order_by: `${query.sort}_at` }),
          ...(query.direction === undefined ? {} : { sort: query.direction }),
          per_page: String(query.limit),
        },
      });
      return rows(answer.body).map((raw) => issueFrom(raw));
    },

    async issue(n: number): Promise<ForgeIssue> {
      const answer = await client.send({ method: 'GET', path: `${at}/issues/${String(n)}` });
      return issueFrom(object(answer.body));
    },

    async comments(n: number): Promise<{ comments: ForgeComment[]; truncated: boolean }> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/issues/${String(n)}/notes`,
        query: { per_page: String(NOTE_PAGE) },
      });
      const read = rows(answer.body).map((raw) => noteFrom(raw));
      return {
        comments: read,
        truncated: read.length >= NOTE_PAGE || Number(answer.headers.get('x-next-page') ?? '') > 0,
      };
    },

    async pull(n: number): Promise<ForgePullRequest> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/merge_requests/${String(n)}`,
      });
      return mergeRequestFrom(object(answer.body));
    },

    async files(n: number): Promise<ForgeChangedFile[]> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/merge_requests/${String(n)}/changes`,
      });
      return rows(object(answer.body)['changes']).map((raw) => {
        const counted = changedLines(text(raw['diff']));
        return {
          filename: text(raw['new_path'], text(raw['old_path'])),
          status: flag(raw['new_file'])
            ? 'added'
            : flag(raw['deleted_file'])
              ? 'removed'
              : flag(raw['renamed_file'])
                ? 'renamed'
                : 'modified',
          ...counted,
        };
      });
    },

    async checks(n: number): Promise<ForgeCheckRun[]> {
      // GitLab has no check run; its pipelines are the nearest thing.
      const answer = await client.send({
        method: 'GET',
        path: `${at}/merge_requests/${String(n)}/pipelines`,
      });
      return rows(answer.body).map((raw) => {
        const status = text(raw['status']);
        return {
          name: `pipeline ${String(number(raw['id']))}`,
          status: status === 'running' || status === 'pending' ? 'in_progress' : 'completed',
          conclusion: status,
          ...(typeof raw['web_url'] === 'string' ? { url: raw['web_url'] } : {}),
        };
      });
    },

    async reviews(n: number): Promise<ForgeReview[]> {
      // Nor a review; an approval is what GitLab records.
      const answer = await client.send({
        method: 'GET',
        path: `${at}/merge_requests/${String(n)}/approvals`,
      });
      const raw = object(answer.body);
      return rows(raw['approved_by']).map((approval) => ({
        author: text(object(approval['user'])['username']),
        state: 'APPROVED',
        body: '',
        submitted_at: text(raw['updated_at']),
      }));
    },
  };
}

/** GitLab takes label sets as a comma-separated string. */
const joined = (names: string[]) => names.join(',');

/** The write half of GitLab's API, in the reference's own shapes. */
export function gitlabWrites(client: ForgeClient, repository: Repository) {
  const at = projectPath(repository);

  return {
    async createIssue(issue: NewIssue): Promise<ForgeIssue> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/issues`,
        body: {
          title: issue.title,
          ...(issue.body === undefined ? {} : { description: issue.body }),
          ...(issue.labels === undefined ? {} : { labels: joined(issue.labels) }),
          // GitLab takes user ids rather than names, so assignees are
          // accepted and not sent, as the reference says.
        },
      });
      return issueFrom(object(answer.body));
    },

    async updateIssue(
      n: number,
      changes: IssueChanges,
    ): Promise<{ issue: ForgeIssue; fields: string[] }> {
      const body: Record<string, unknown> = {};
      const fields: string[] = [];
      if (changes.title !== undefined) {
        body['title'] = changes.title;
        fields.push('title');
      }
      if (changes.body !== undefined) {
        body['description'] = changes.body;
        fields.push('body');
      }
      if (changes.state !== undefined) {
        body['state_event'] = changes.state === 'closed' ? 'close' : 'reopen';
        fields.push('state');
      }
      if (changes.labels !== undefined) {
        body['labels'] = joined(changes.labels);
        fields.push('labels');
      }

      const answer = await client.send({
        method: 'PUT',
        path: `${at}/issues/${String(n)}`,
        body,
      });
      return { issue: issueFrom(object(answer.body)), fields: fields.toSorted() };
    },

    async closeIssue(n: number): Promise<ForgeIssue> {
      const answer = await client.send({
        method: 'PUT',
        path: `${at}/issues/${String(n)}`,
        body: { state_event: 'close' },
      });
      return issueFrom(object(answer.body));
    },

    async commentOnIssue(n: number, body: string): Promise<{ url: string }> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/issues/${String(n)}/notes`,
        body: { body },
      });
      return { url: noteUrl(answer.body, `${at}/issues/${String(n)}`) };
    },

    async commentOnPull(n: number, body: string): Promise<{ url: string }> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/merge_requests/${String(n)}/notes`,
        body: { body },
      });
      return { url: noteUrl(answer.body, `${at}/merge_requests/${String(n)}`) };
    },

    async addLabels(n: number, names: string[]): Promise<void> {
      await client.send({
        method: 'PUT',
        path: `${at}/issues/${String(n)}`,
        body: { add_labels: joined(names) },
      });
    },

    async removeLabel(n: number, name: string): Promise<void> {
      await client.send({
        method: 'PUT',
        path: `${at}/issues/${String(n)}`,
        body: { remove_labels: name },
      });
    },

    async label(name: string): Promise<ForgeLabel | undefined> {
      try {
        const answer = await client.send({
          method: 'GET',
          path: `${at}/labels/${labelSegment(name)}`,
        });
        const raw = object(answer.body);
        return {
          name: text(raw['name']),
          ...(typeof raw['color'] === 'string' ? { color: raw['color'] } : {}),
          ...(typeof raw['description'] === 'string' ? { description: raw['description'] } : {}),
        };
      } catch (err) {
        if (isNotFound(err)) {
          return undefined;
        }
        throw err;
      }
    },

    async createLabel(label: ForgeLabel): Promise<ForgeLabel> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/labels`,
        body: {
          name: label.name,
          // GitLab takes the colour with its hash, GitHub without.
          color: `#${(label.color ?? 'ededed').replace(/^#/, '')}`,
          ...(label.description === undefined ? {} : { description: label.description }),
        },
      });
      const raw = object(answer.body);
      return {
        name: text(raw['name'], label.name),
        ...(typeof raw['color'] === 'string' ? { color: raw['color'] } : {}),
        ...(typeof raw['description'] === 'string' ? { description: raw['description'] } : {}),
      };
    },

    async createPull(pull: NewPullRequest): Promise<ForgePullRequest> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/merge_requests`,
        body: {
          title: pull.title,
          source_branch: pull.head,
          target_branch: pull.base,
          ...(pull.body === undefined ? {} : { description: pull.body }),
          // GitLab has no draft flag of its own on a create; the
          // title prefix is what marks one.
          ...(pull.draft ? { title: `Draft: ${pull.title}` } : {}),
        },
      });
      return mergeRequestFrom(object(answer.body));
    },
  };
}

/** A note has no URL of its own; its anchor on the thread is the nearest. */
function noteUrl(body: unknown, thread: string): string {
  const raw = object(body);
  const id = number(raw['id']);
  return text(raw['web_url'], id === 0 ? thread : `${thread}#note_${String(id)}`);
}
