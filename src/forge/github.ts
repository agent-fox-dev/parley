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
 * GitHub, in the reference's own shapes. Every translation GitHub
 * needs is here and nowhere else: the issue's `user`, the labels as
 * objects, and the check runs that hang off the head commit rather
 * than the pull request (ADR 11).
 */
const PER_PAGE = 100;

/** How many comments one read brings back before the thread is cut short. */
export const COMMENT_PAGE = 100;

function issueFrom(raw: Record<string, unknown>): ForgeIssue {
  const author = object(raw['user']);
  const labels = rows(raw['labels']).map((label) => text(label['name']));
  return {
    number: number(raw['number']),
    title: text(raw['title']),
    body: text(raw['body']),
    state: text(raw['state']),
    ...(raw['url'] === undefined ? {} : { url: text(raw['url']) }),
    ...(raw['html_url'] === undefined ? {} : { html_url: text(raw['html_url']) }),
    author: { login: text(author['login']) },
    ...(labels.length === 0 ? {} : { labels }),
    created_at: text(raw['created_at']),
    updated_at: text(raw['updated_at']),
    ...(typeof raw['closed_at'] === 'string' ? { closed_at: raw['closed_at'] } : {}),
    // GitHub answers a pull request from the issue routes too, and
    // says so by carrying a pull_request block.
    is_pr: raw['pull_request'] !== undefined,
  };
}

function commentFrom(raw: Record<string, unknown>): ForgeComment {
  const author = object(raw['user']);
  return {
    ...(raw['id'] === undefined ? {} : { id: number(raw['id']) }),
    body: text(raw['body']),
    user: { login: text(author['login']) },
    created_at: text(raw['created_at']),
    ...(typeof raw['updated_at'] === 'string' ? { updated_at: raw['updated_at'] } : {}),
    ...(typeof raw['url'] === 'string' ? { url: raw['url'] } : {}),
    ...(typeof raw['html_url'] === 'string' ? { html_url: raw['html_url'] } : {}),
  };
}

function pullFrom(raw: Record<string, unknown>): ForgePullRequest {
  const head = object(raw['head']);
  const base = object(raw['base']);
  const author = object(raw['user']);
  return {
    number: number(raw['number']),
    title: text(raw['title']),
    body: text(raw['body']),
    state: text(raw['state']),
    ...(typeof raw['url'] === 'string' ? { url: raw['url'] } : {}),
    ...(typeof raw['html_url'] === 'string' ? { html_url: raw['html_url'] } : {}),
    draft: flag(raw['draft']),
    merged: flag(raw['merged']),
    head_sha: text(head['sha']),
    head_branch: text(head['ref']),
    base_branch: text(base['ref']),
    author: { login: text(author['login']) },
    created_at: text(raw['created_at']),
    updated_at: text(raw['updated_at']),
    ...(typeof raw['closed_at'] === 'string' ? { closed_at: raw['closed_at'] } : {}),
    ...(typeof raw['merged_at'] === 'string' ? { merged_at: raw['merged_at'] } : {}),
  };
}

/** The repository's path, as GitHub spells it in a URL. */
export function repoPath(repository: Repository): string {
  return `/repos/${repository.owner}/${repository.name}`;
}

export function githubApi(client: ForgeClient, repository: Repository): ForgeApi {
  const at = repoPath(repository);

  return {
    ...githubWrites(client, repository),
    async repository(): Promise<ForgeRepository> {
      const answer = await client.send({ method: 'GET', path: at });
      const raw = object(answer.body);
      const permissions = object(raw['permissions']);
      return {
        full_name: text(raw['full_name']),
        default_branch: text(raw['default_branch']),
        private: flag(raw['private']),
        archived: flag(raw['archived']),
        permissions: {
          push: flag(permissions['push']),
          pull: flag(permissions['pull']),
          admin: flag(permissions['admin']),
        },
        ...(raw['allow_merge_commit'] === undefined
          ? {}
          : { allow_merge_commit: flag(raw['allow_merge_commit']) }),
        ...(raw['allow_squash_merge'] === undefined
          ? {}
          : { allow_squash_merge: flag(raw['allow_squash_merge']) }),
        ...(raw['allow_rebase_merge'] === undefined
          ? {}
          : { allow_rebase_merge: flag(raw['allow_rebase_merge']) }),
      };
    },

    async issues(query: IssueQuery): Promise<ForgeIssue[]> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/issues`,
        query: {
          ...(query.state === undefined ? {} : { state: query.state }),
          ...(query.labels === undefined ? {} : { labels: query.labels }),
          ...(query.assignee === undefined ? {} : { assignee: query.assignee }),
          ...(query.sort === undefined ? {} : { sort: query.sort }),
          ...(query.direction === undefined ? {} : { direction: query.direction }),
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
        path: `${at}/issues/${String(n)}/comments`,
        query: { per_page: String(COMMENT_PAGE) },
      });
      const read = rows(answer.body).map((raw) => commentFrom(raw));
      return {
        comments: read,
        // A next page is the forge saying there are more; a full page
        // says so too, for a forge that sends no link header.
        truncated:
          read.length >= COMMENT_PAGE || (answer.headers.get('link') ?? '').includes('rel="next"'),
      };
    },

    async pull(n: number): Promise<ForgePullRequest> {
      const answer = await client.send({ method: 'GET', path: `${at}/pulls/${String(n)}` });
      return pullFrom(object(answer.body));
    },

    async files(n: number): Promise<ForgeChangedFile[]> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/pulls/${String(n)}/files`,
        query: { per_page: String(PER_PAGE) },
      });
      return rows(answer.body).map((raw) => ({
        filename: text(raw['filename']),
        status: text(raw['status']),
        additions: number(raw['additions']),
        deletions: number(raw['deletions']),
      }));
    },

    async checks(n: number): Promise<ForgeCheckRun[]> {
      // GitHub's check runs hang off the commit, so the pull request
      // is read first for its head.
      const pull = await this.pull(n);
      const answer = await client.send({
        method: 'GET',
        path: `${at}/commits/${pull.head_sha}/check-runs`,
        query: { per_page: String(PER_PAGE) },
      });
      return rows(object(answer.body)['check_runs']).map((raw) => ({
        name: text(raw['name']),
        status: text(raw['status']),
        conclusion: text(raw['conclusion']),
        ...(typeof object(raw['output'])['summary'] === 'string'
          ? { summary: text(object(raw['output'])['summary']) }
          : {}),
        ...(typeof raw['html_url'] === 'string' ? { url: raw['html_url'] } : {}),
      }));
    },

    async reviews(n: number): Promise<ForgeReview[]> {
      const answer = await client.send({
        method: 'GET',
        path: `${at}/pulls/${String(n)}/reviews`,
        query: { per_page: String(PER_PAGE) },
      });
      return rows(answer.body).map((raw) => ({
        author: text(object(raw['user'])['login']),
        state: text(raw['state']),
        body: text(raw['body']),
        submitted_at: text(raw['submitted_at']),
      }));
    },
  };
}

/** The write half of GitHub's API, in the reference's own shapes. */
export function githubWrites(client: ForgeClient, repository: Repository) {
  const at = repoPath(repository);

  return {
    async createIssue(issue: NewIssue): Promise<ForgeIssue> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/issues`,
        body: {
          title: issue.title,
          ...(issue.body === undefined ? {} : { body: issue.body }),
          ...(issue.labels === undefined ? {} : { labels: issue.labels }),
          ...(issue.assignees === undefined ? {} : { assignees: issue.assignees }),
        },
      });
      return issueFrom(object(answer.body));
    },

    async updateIssue(
      n: number,
      changes: IssueChanges,
    ): Promise<{ issue: ForgeIssue; fields: string[] }> {
      const body: Record<string, unknown> = { ...changes };
      const answer = await client.send({
        method: 'PATCH',
        path: `${at}/issues/${String(n)}`,
        body,
      });
      return { issue: issueFrom(object(answer.body)), fields: Object.keys(body).toSorted() };
    },

    async closeIssue(n: number): Promise<ForgeIssue> {
      const answer = await client.send({
        method: 'PATCH',
        path: `${at}/issues/${String(n)}`,
        body: { state: 'closed' },
      });
      return issueFrom(object(answer.body));
    },

    async commentOnIssue(n: number, body: string): Promise<{ url: string }> {
      const answer = await client.send({
        method: 'POST',
        path: `${at}/issues/${String(n)}/comments`,
        body: { body },
      });
      const raw = object(answer.body);
      return { url: text(raw['html_url'], text(raw['url'])) };
    },

    async commentOnPull(n: number, body: string): Promise<{ url: string }> {
      // A pull request's conversation is the issue's, on GitHub.
      return this.commentOnIssue(n, body);
    },

    async addLabels(n: number, names: string[]): Promise<void> {
      await client.send({
        method: 'POST',
        path: `${at}/issues/${String(n)}/labels`,
        body: { labels: names },
      });
    },

    async removeLabel(n: number, name: string): Promise<void> {
      await client.send({
        method: 'DELETE',
        path: `${at}/issues/${String(n)}/labels/${labelSegment(name)}`,
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
          // GitHub takes the colour without its hash.
          color: (label.color ?? 'ededed').replace(/^#/, ''),
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
        path: `${at}/pulls`,
        body: {
          title: pull.title,
          // Sent exactly as given, so owner:branch opens from a fork.
          head: pull.head,
          base: pull.base,
          ...(pull.body === undefined ? {} : { body: pull.body }),
          draft: pull.draft,
        },
      });
      return pullFrom(object(answer.body));
    },
  };
}
