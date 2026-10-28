import { describe, expect, it } from 'vitest';
import { renderLanding } from '../render.js';

/**
 * What a visitor is shown before they sign in: what the service is,
 * and the one thing they can do about it.
 */
describe('the landing page', () => {
  it('says what the service is and what this interface shows', () => {
    const html = renderLanding([{ name: 'github' }]);

    expect(html).toContain('<!doctype html>');
    expect(html).toContain('Parley');
    // A description, not a slogan: the page says what it does.
    expect(html).toMatch(/workspace/i);
    expect(html).toMatch(/patch stack/i);
    expect(html).toMatch(/only reads|This interface only reads/i);
  });

  it('offers a sign-in control leading to the provider', () => {
    const html = renderLanding([{ name: 'github' }]);

    expect(html).toContain('href="/signin/github"');
    expect(html).toMatch(/Sign in with github/);
  });

  it('says so plainly where the deployment has no provider', () => {
    const html = renderLanding([]);

    expect(html).toMatch(/no sign-in provider/i);
    expect(html).not.toContain('href="/signin/');
  });

  it('carries the service’s own name and no other', () => {
    const html = renderLanding([{ name: 'github' }]);

    // The provider is named as a provider; the site is Parley.
    expect(html).toContain('<title>Parley · Parley</title>');
    expect(html).toContain('class="wordmark"');
    expect(html).not.toMatch(/<title>[^<]*GitHub/i);
  });

  it('is one main landmark with a heading structure that skips nothing', () => {
    const html = renderLanding([{ name: 'github' }]);

    expect([...html.matchAll(/<main\b/g)]).toHaveLength(1);
    const levels = [...html.matchAll(/<h([1-6])\b/g)].map((match) => Number(match[1]));
    expect(levels[0]).toBe(1);
    for (const [index, level] of levels.entries()) {
      const previous = levels[index - 1] ?? level;
      expect(level - previous).toBeLessThanOrEqual(1);
    }
  });
});
