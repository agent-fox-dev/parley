import { describe, expect, it } from 'vitest';
import { repositoryName } from './naming.js';

describe("the name a workspace's repository is kept under", () => {
  it('is the same every time it is computed', () => {
    expect(repositoryName('my-project', 'id-1')).toBe(repositoryName('my-project', 'id-1'));
  });

  it('carries the slug, so a person reading the store knows what it is', () => {
    expect(repositoryName('my-project', 'id-1')).toContain('my-project');
  });

  it('holds only what the store accepts in a name', () => {
    expect(repositoryName('my-project', 'id-1')).toMatch(/^[A-Za-z0-9._-]+$/);
  });

  it.each([
    [
      'two workspaces of the same slug in different records',
      'same-slug',
      'id-1',
      'same-slug',
      'id-2',
    ],
    ['two workspaces of different slugs', 'one', 'id-1', 'two', 'id-1'],
    ['a slug that is a prefix of another', 'proj', 'id-1', 'project', 'id-1'],
  ])('tells %s apart', (_name, leftSlug, leftId, rightSlug, rightId) => {
    expect(repositoryName(leftSlug, leftId)).not.toBe(repositoryName(rightSlug, rightId));
  });

  it('does not change when a workspace is renamed, because it follows the id', () => {
    // The slug is immutable, but the display name and the organization are
    // not; neither takes part in the name.
    const before = repositoryName('my-project', 'id-1');

    expect(repositoryName('my-project', 'id-1')).toBe(before);
  });

  it('stays within what a store will take for a long slug', () => {
    const name = repositoryName('a'.repeat(64), 'id-1');

    expect(name.length).toBeLessThanOrEqual(100);
    expect(name).toMatch(/^[A-Za-z0-9._-]+$/);
  });

  it('is not simply the slug, so a slug reused after a delete gets its own repository', () => {
    expect(repositoryName('my-project', 'id-1')).not.toBe('my-project');
  });
});
