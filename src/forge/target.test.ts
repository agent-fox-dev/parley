import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { forgeFor, repositoryFrom } from './target.js';

/**
 * Where a forge route is about to send a workspace's credential. It is
 * worked out from the remote URL and one setting, and getting it wrong
 * sends the credential to the wrong host, so every shape of URL the
 * reference names is here.
 */
describe('the repository a remote URL names', () => {
  it.each([
    ['https://github.com/acme/widgets.git', 'github.com', 'acme', 'widgets'],
    ['https://github.com/acme/widgets', 'github.com', 'acme', 'widgets'],
    ['ssh://git@github.com/acme/widgets.git', 'github.com', 'acme', 'widgets'],
    ['git@github.com:acme/widgets.git', 'github.com', 'acme', 'widgets'],
    ['https://token@github.com/acme/widgets.git', 'github.com', 'acme', 'widgets'],
    ['https://user:pass@github.com/acme/widgets.git', 'github.com', 'acme', 'widgets'],
    [
      'https://gitlab.example.com/group/subgroup/widgets.git',
      'gitlab.example.com',
      'group/subgroup',
      'widgets',
    ],
    [
      'git@gitlab.example.com:group/subgroup/widgets.git',
      'gitlab.example.com',
      'group/subgroup',
      'widgets',
    ],
    ['https://github.com/acme/widgets/', 'github.com', 'acme', 'widgets'],
  ])('reads %s as the same repository', (url, host, owner, name) => {
    expect(repositoryFrom(url)).toEqual({ host, owner, name });
  });

  it.each([
    ['an empty URL', ''],
    ['a host with no path', 'https://github.com'],
    ['a host and an owner alone', 'https://github.com/acme'],
    ['something that is no URL at all', 'not a url'],
  ])('reads none from %s', (_what, url) => {
    expect(repositoryFrom(url)).toBeUndefined();
  });
});

describe('the forge a host and a setting name', () => {
  it.each([
    [
      'github.com with no setting',
      'https://github.com/a/b',
      '',
      'github',
      'https://api.github.com',
    ],
    [
      'a self-hosted GitHub with no setting',
      'https://github.acme.test/a/b',
      '',
      'github',
      'https://github.acme.test/api/v3',
    ],
    [
      'a GitLab host with no setting',
      'https://gitlab.acme.test/a/b',
      '',
      'gitlab',
      'https://gitlab.acme.test/api/v4',
    ],
    [
      'a setting naming GitHub for a host that does not',
      'https://code.acme.test/a/b',
      'https://github.acme.test/api/v3',
      'github',
      'https://github.acme.test/api/v3',
    ],
    [
      'a GitLab setting without the version suffix',
      'https://code.acme.test/a/b',
      'https://gitlab.acme.test',
      'gitlab',
      'https://gitlab.acme.test/api/v4',
    ],
    [
      'a setting with whitespace and a trailing slash',
      'https://code.acme.test/a/b',
      '  https://gitlab.acme.test/api/v4/  ',
      'gitlab',
      'https://gitlab.acme.test/api/v4',
    ],
    [
      'a setting that overrides a host naming the other forge',
      'https://github.acme.test/a/b',
      'https://gitlab.acme.test/api/v4',
      'gitlab',
      'https://gitlab.acme.test/api/v4',
    ],
  ])('takes %s', (_what, url, setting, forge, apiUrl) => {
    const found = forgeFor({ remoteUrl: url, setting });

    expect(found.forge).toBe(forge);
    expect(found.apiUrl).toBe(apiUrl);
  });

  it.each([
    ['a host naming neither forge and no setting', 'https://code.acme.test/a/b', ''],
    ['a setting naming neither forge', 'https://code.acme.test/a/b', 'https://code.acme.test/api'],
    ['a URL naming no repository', 'https://github.com', ''],
  ])('refuses %s with forge_unknown', (_what, url, setting) => {
    let refusal: unknown;
    try {
      forgeFor({ remoteUrl: url, setting });
    } catch (err) {
      refusal = err;
    }

    expect(refusal).toBeInstanceOf(HttpError);
    expect((refusal as HttpError).status).toBe(422);
    expect((refusal as HttpError).errorType).toBe('forge_unknown');
  });

  it.each([
    ['a URL that is not absolute', 'api.github.com'],
    ['a URL that is not https', 'http://gitlab.acme.test/api/v4'],
    ['a URL carrying userinfo', 'https://token@gitlab.acme.test/api/v4'],
    ['a URL carrying a query', 'https://gitlab.acme.test/api/v4?token=1'],
    ['a URL carrying a fragment', 'https://gitlab.acme.test/api/v4#here'],
    ['a URL with no host', 'https://'],
  ])('refuses %s as a setting, naming the setting and not the value', (_what, setting) => {
    let refusal: unknown;
    try {
      forgeFor({ remoteUrl: 'https://github.com/a/b', setting, settingName: 'forge_api_url' });
    } catch (err) {
      refusal = err;
    }

    const thrown = refusal as HttpError;
    expect(thrown.status).toBe(422);
    expect(thrown.errorType).toBe('forge_api_url_invalid');
    expect(thrown.message).toContain('forge_api_url');
    expect(thrown.message).not.toContain(setting);
  });
});
