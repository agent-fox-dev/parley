/**
 * What a forge route answers with.
 *
 * These are the reference's own types, not either forge's. GitHub and
 * GitLab describe the same things in different words — a description
 * and a body, an iid and a number, opened and open — and the mappers
 * are where that ends: above them nothing knows which forge answered.
 */
export interface ForgeRepository {
  full_name: string;
  default_branch: string;
  private: boolean;
  archived: boolean;
  permissions: { push: boolean; pull: boolean; admin: boolean };
  allow_merge_commit?: boolean;
  allow_squash_merge?: boolean;
  allow_rebase_merge?: boolean;
}

export interface ForgeIssue {
  number: number;
  title: string;
  body: string;
  state: string;
  url?: string;
  html_url?: string;
  author: { login: string };
  labels?: string[];
  created_at: string;
  updated_at: string;
  closed_at?: string;
  is_pr: boolean;
}

export interface ForgeComment {
  id?: number;
  body: string;
  user: { login: string };
  created_at: string;
  updated_at?: string;
  url?: string;
  html_url?: string;
}

export interface ForgeIssueThread {
  issue: ForgeIssue;
  comments: ForgeComment[];
  /** Whether more comments exist than were read. */
  truncated: boolean;
  comments_err?: string;
}

export interface ForgePullRequest {
  number: number;
  title: string;
  body: string;
  state: string;
  url?: string;
  html_url?: string;
  draft: boolean;
  merged: boolean;
  head_sha: string;
  head_branch: string;
  base_branch: string;
  author: { login: string };
  created_at: string;
  updated_at: string;
  closed_at?: string;
  merged_at?: string;
}

export interface ForgePullState {
  number: number;
  state: string;
  merged: boolean;
  head_sha: string;
}

export interface ForgeChangedFile {
  filename: string;
  status: string;
  additions: number;
  deletions: number;
}

export interface ForgeCheckRun {
  name: string;
  status: string;
  conclusion: string;
  summary?: string;
  url?: string;
}

export interface ForgeReview {
  author: string;
  state: string;
  body: string;
  submitted_at: string;
}

export interface ForgeLabel {
  name: string;
  color?: string;
  description?: string;
}

/** What a caller may narrow an issue list by. */
export interface IssueQuery {
  state?: string;
  labels?: string;
  assignee?: string;
  sort?: string;
  direction?: string;
  limit: number;
}

/** What a caller may put in a new issue. */
export interface NewIssue {
  title: string;
  body?: string;
  labels?: string[];
  assignees?: string[];
}

/** What an update changes, holding only the fields the caller named. */
export interface IssueChanges {
  title?: string;
  body?: string;
  state?: string;
  labels?: string[];
  assignees?: string[];
}

export interface NewPullRequest {
  title: string;
  body?: string;
  head: string;
  base: string;
  draft: boolean;
}

/**
 * What a forge offers this service. One object per forge, built over
 * a client that already knows the repository and the credential.
 */
export interface ForgeApi {
  repository(): Promise<ForgeRepository>;
  issues(query: IssueQuery): Promise<ForgeIssue[]>;
  issue(n: number): Promise<ForgeIssue>;
  comments(n: number): Promise<{ comments: ForgeComment[]; truncated: boolean }>;
  pull(n: number): Promise<ForgePullRequest>;
  files(n: number): Promise<ForgeChangedFile[]>;
  checks(n: number): Promise<ForgeCheckRun[]>;
  reviews(n: number): Promise<ForgeReview[]>;

  createIssue(issue: NewIssue): Promise<ForgeIssue>;
  /** The fields an update really changed, for its event. */
  updateIssue(n: number, changes: IssueChanges): Promise<{ issue: ForgeIssue; fields: string[] }>;
  closeIssue(n: number): Promise<ForgeIssue>;
  commentOnIssue(n: number, body: string): Promise<{ url: string }>;
  commentOnPull(n: number, body: string): Promise<{ url: string }>;
  addLabels(n: number, names: string[]): Promise<void>;
  /** Removes a label, throwing forge_not_found where the forge says so. */
  removeLabel(n: number, name: string): Promise<void>;
  /** The label this repository holds, or none. */
  label(name: string): Promise<ForgeLabel | undefined>;
  createLabel(label: ForgeLabel): Promise<ForgeLabel>;
  createPull(pull: NewPullRequest): Promise<ForgePullRequest>;
}

/**
 * A label's name in a path. Only what a URL really needs encoding
 * is encoded, so `af:fix` travels as itself and `type/bug` as
 * `type%2Fbug`; `.` and `..` are encoded whole, being path segments
 * a server would otherwise resolve away.
 */
export function labelSegment(name: string): string {
  if (name === '.' || name === '..') {
    return name.replaceAll('.', '%2E');
  }
  return encodeURIComponent(name).replaceAll('%3A', ':');
}

/** Whether a refusal was the forge saying it has no such thing. */
export function isNotFound(err: unknown): boolean {
  return (
    typeof err === 'object' &&
    err !== null &&
    (err as { errorType?: string }).errorType === 'forge_not_found'
  );
}

/** A field of an answer, read without trusting the forge's shape. */
export function text(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

export function flag(value: unknown): boolean {
  return value === true;
}

export function number(value: unknown, fallback = 0): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : fallback;
}

/** The rows of a list answer, or none where the forge sent something else. */
export function rows(value: unknown): Array<Record<string, unknown>> {
  return Array.isArray(value) ? (value as Array<Record<string, unknown>>) : [];
}

export function object(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}
