import { describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import { checkedCreate, isGitUrl, isValidSlug } from './validate.js';

const valid = {
  slug: 'my-project',
  git_url: 'https://github.com/user/repo.git',
};

function refusal(body: Record<string, unknown>): HttpError {
  try {
    checkedCreate(body);
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the body was accepted when it should have been refused');
}

describe('the slug rule', () => {
  it.each([
    ['three characters, the minimum', 'abc'],
    ['sixty-four characters, the maximum', `a${'b'.repeat(62)}c`],
    ['an ordinary slug', 'my-project'],
    ['digits inside and at the end', 'repo-2000'],
  ])('accepts %s', (_name, slug) => {
    expect(isValidSlug(slug)).toBe(true);
    expect(checkedCreate({ ...valid, slug }).slug).toBe(slug);
  });

  it.each([
    ['two characters', 'ab'],
    ['sixty-five characters', `a${'b'.repeat(63)}c`],
    ['a leading digit', '1abc'],
    ['a leading hyphen', '-abc'],
    ['a trailing hyphen', 'abc-'],
    ['an uppercase letter', 'myProject'],
    ['an underscore', 'my_project'],
    ['two consecutive hyphens', 'my--project'],
    ['an empty slug', ''],
    ['a slash', 'my/project'],
  ])('refuses %s', (_name, slug) => {
    expect(isValidSlug(slug)).toBe(false);
    expect(refusal({ ...valid, slug }).status).toBe(400);
  });
});

describe('the git URL rule', () => {
  it.each([
    ['an https URL', 'https://github.com/user/repo.git'],
    ['an scp-style ssh URL', 'git@github.com:user/repo.git'],
    ['an ssh:// URL', 'ssh://git@github.com/user/repo.git'],
  ])('accepts %s', (_name, url) => {
    expect(isGitUrl(url)).toBe(true);
  });

  it.each([
    ['an http URL', 'http://github.com/user/repo.git'],
    ['a file path', '/srv/git/repo.git'],
    ['a bare host', 'github.com/user/repo'],
    ['an empty string', ''],
  ])('refuses %s', (_name, url) => {
    expect(isGitUrl(url)).toBe(false);
    expect(refusal({ ...valid, git_url: url }).status).toBe(400);
  });
});

describe('a create body the validator refuses', () => {
  it.each([
    ['no slug', { git_url: valid.git_url }],
    ['no git_url', { slug: valid.slug }],
    ['a display_name of 129 characters', { ...valid, display_name: 'x'.repeat(129) }],
    ['a description of 1025 characters', { ...valid, description: 'y'.repeat(1025) }],
    ['an unknown workspace_mode', { ...valid, workspace_mode: 'mirror' }],
    ['carry_patch with no upstream_url', { ...valid, workspace_mode: 'carry_patch' }],
    ['standard with an upstream_url', { ...valid, upstream_url: 'https://up.test/r.git' }],
    ['standard with an integration_branch', { ...valid, integration_branch: 'deploy' }],
    [
      'carry_patch with a malformed upstream_url',
      { ...valid, workspace_mode: 'carry_patch', upstream_url: 'not-a-url' },
    ],
  ])('refuses %s with 400', (_name, body) => {
    expect(refusal(body).status).toBe(400);
  });

  it.each([
    ['slug', { git_url: valid.git_url }, 'slug'],
    ['git_url', { slug: valid.slug }, 'git_url'],
    ['display_name', { ...valid, display_name: 'x'.repeat(129) }, 'display_name'],
    ['description', { ...valid, description: 'y'.repeat(1025) }, 'description'],
    ['upstream_url', { ...valid, upstream_url: 'https://up.test/r.git' }, 'upstream_url'],
  ])('names the offending field for %s', (_name, body, field) => {
    expect(refusal(body).message).toContain(field);
  });

  it('accepts a carry_patch body carrying an upstream_url', () => {
    const request = checkedCreate({
      ...valid,
      workspace_mode: 'carry_patch',
      upstream_url: 'https://github.com/upstream/repo.git',
    });

    expect(request.workspace_mode).toBe('carry_patch');
    expect(request.integration_branch).toBe('deploy');
  });

  it('defaults the display name to the slug and the description to the empty string', () => {
    const request = checkedCreate(valid);

    expect(request.display_name).toBe(valid.slug);
    expect(request.description).toBe('');
    expect(request.branch).toBeNull();
    expect(request.integration_branch).toBeNull();
  });
});

describe('the credentials a create may carry', () => {
  it.each([
    ['a token alone', { git_pat: 'ghp_secret' }, { GIT_PAT: 'ghp_secret' }],
    [
      'the full basic pair',
      { git_username: 'user', git_password: 'pw' },
      { GIT_USERNAME: 'user', GIT_PASSWORD: 'pw' },
    ],
    ['none at all', {}, {}],
  ])('accepts %s', (_name, given, expected) => {
    expect(checkedCreate({ ...valid, ...given }).credentials).toEqual(expected);
  });

  it.each([
    ['a token together with a username', { git_pat: 'p', git_username: 'u' }],
    ['a token together with a password', { git_pat: 'p', git_password: 'w' }],
    ['a username with no password', { git_username: 'u' }],
    ['a password with no username', { git_password: 'w' }],
    ['an empty token', { git_pat: '' }],
    ['an empty password', { git_username: 'u', git_password: '' }],
  ])('refuses %s with 400', (_name, given) => {
    expect(refusal({ ...valid, ...given }).status).toBe(400);
  });

  it.each([
    ['a token', { git_pat: 'ghp_secret' }],
    ['the basic pair', { git_username: 'user', git_password: 'pw' }],
  ])('refuses %s with a git_url that is not https', (_name, given) => {
    expect(refusal({ ...valid, git_url: 'git@github.com:user/repo.git', ...given }).status).toBe(
      400,
    );
  });
});
