import { describe, expect, it } from 'vitest';
import { renderDashboard, renderLanding, renderNotAvailable, renderWorkspace } from './render.js';

/**
 * Whose interface this is.
 *
 * It follows GitHub's visual language because that is what this
 * audience reads fluently, and it says Parley everywhere, because
 * it is not GitHub and must not look like a thing GitHub made.
 */
const pages = (): Array<{ name: string; html: string }> => [
  { name: 'the landing page', html: renderLanding([{ name: 'github' }]) },
  {
    name: 'the dashboard',
    html: renderDashboard('alice', [
      { slug: 'widgets', status: 'ready', access: 'owner', last_sync_at: null },
    ]),
  },
  {
    name: 'a workspace',
    html: renderWorkspace('alice', {
      workspace: { slug: 'widgets', status: 'ready', workspace_mode: 'standard' },
      jobs: [],
      events: [],
    }),
  },
  { name: 'the not-available page', html: renderNotAvailable('alice') },
];

describe('every page of the interface', () => {
  it('carries Parley’s name in its title and its masthead', () => {
    for (const { name, html } of pages()) {
      expect(html, `${name} is titled`).toMatch(/<title>[^<]*Parley<\/title>/);
      expect(html, `${name} has the wordmark`).toContain('class="wordmark"');
      expect(html, `${name} names the service`).toMatch(/>\s*Parley\s*</);
    }
  });

  it('never presents itself as GitHub', () => {
    for (const { name, html } of pages()) {
      expect(html, `${name}'s title`).not.toMatch(/<title>[^<]*GitHub/i);
      // No wordmark, logo or favicon of theirs.
      expect(html, `${name}'s marks`).not.toMatch(/githubassets|github\.com\/images|octicon/i);
      expect(html, `${name}'s wordmark`).not.toMatch(/class="wordmark"[^>]*>\s*GitHub/i);
    }
  });

  it('claims no association with GitHub', () => {
    for (const { name, html } of pages()) {
      for (const claim of [
        /a GitHub (product|company|service)/i,
        /in partnership with GitHub/i,
        /powered by GitHub/i,
        /official(ly)? (GitHub|affiliated)/i,
      ]) {
        expect(html, `${name}: ${String(claim)}`).not.toMatch(claim);
      }
    }
  });

  it('names GitHub only where GitHub is the thing being named', () => {
    // The provider's name on a sign-in control is a fact about the
    // provider, not a claim about the site.
    const landing = pages()[0]?.html ?? '';
    const mentions = [...landing.matchAll(/github/gi)].map((match) => match.index ?? 0);

    for (const at of mentions) {
      const around = landing.slice(Math.max(0, at - 60), at + 60);
      expect(around, around).toMatch(/signin|Sign in with/i);
    }
  });
});
