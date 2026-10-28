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
