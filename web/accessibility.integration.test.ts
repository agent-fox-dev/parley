import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { renderDashboard, renderLanding, renderNotAvailable, renderWorkspace } from './render.js';

/**
 * Whether a page can be used without a mouse, and whether a screen
 * reader is told what it is reading.
 *
 * Every control the interface has is a real link or a real button,
 * which is what makes it reachable and operable from the keyboard
 * without a line of script; these cases check that it stayed that
 * way, that every control is named, and that each page's structure
 * is one a reader can navigate. The focus order a browser really
 * computes is a browser's answer, and the smoke tests ask for it.
 */
const tokens = readFileSync('web/tokens.ts', 'utf8');

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
      workspace: { slug: 'widgets', status: 'ready', workspace_mode: 'carry_patch' },
      patchStatus: {
        patches: [
          { position: 1, branch_name: 'first', status: 'active', last_rebuild_result: 'success' },
        ],
        last_rebuild: { id: 'r-1', status: 'completed' },
      },
      jobs: [{ kind: 'merge', status: 'completed', at: '2026-10-21T10:00:00.000Z' }],
      events: [{ event_type: 'hub.workspace.sync', timestamp: '2026-10-21T11:00:00.000Z' }],
    }),
  },
  { name: 'the not-available page', html: renderNotAvailable('alice') },
];

/** The text a control would be announced by. */
function accessibleName(element: string): string {
  const label = /aria-label="([^"]*)"/.exec(element)?.[1] ?? '';
  const text = element
    .replace(/<[^>]*>/g, ' ')
    .replace(/&[a-z]+;/g, "'")
    .trim();
  return `${label} ${text}`.trim();
}

describe('every page', () => {
  it('has one main landmark', () => {
    for (const { name, html } of pages()) {
      expect([...html.matchAll(/<main\b/g)], name).toHaveLength(1);
    }
  });

  it('has a heading structure that starts at one and skips no level', () => {
    for (const { name, html } of pages()) {
      const levels = [...html.matchAll(/<h([1-6])\b/g)].map((match) => Number(match[1]));
      expect(levels[0], `${name} starts at h1`).toBe(1);
      expect([...html.matchAll(/<h1\b/g)], `${name} has one h1`).toHaveLength(1);

      for (const [index, level] of levels.entries()) {
        const previous = levels[index - 1] ?? level;
        expect(level - previous, `${name} skips no level`).toBeLessThanOrEqual(1);
      }
    }
  });

  it('declares its language', () => {
    for (const { name, html } of pages()) {
      expect(html, name).toContain('<html lang="en">');
    }
  });

  it('gives every link and button something to be announced by', () => {
    for (const { name, html } of pages()) {
      const controls = [
        ...html.matchAll(/<a\b[^>]*>.*?<\/a>/gs),
        ...html.matchAll(/<button\b[^>]*>.*?<\/button>/gs),
      ].map((match) => match[0]);

      expect(controls.length, `${name} has controls`).toBeGreaterThan(0);
      for (const control of controls) {
        expect(accessibleName(control).length, `${name}: ${control.slice(0, 60)}`).toBeGreaterThan(
          0,
        );
      }
    }
  });

  it('builds every control from a real link or button, so a keyboard reaches it', () => {
    for (const { name, html } of pages()) {
      // Nothing is made clickable by script on an element that is
      // not focusable to begin with.
      expect(html, name).not.toMatch(/<(?:div|span)[^>]*\sonclick/i);
      expect(html, name).not.toMatch(/role="button"/i);
      expect(html, name).not.toMatch(/tabindex="-?[1-9]/i);
    }
  });

  it('names every list a reader may land in', () => {
    const workspace = pages()[2]?.html ?? '';

    for (const list of [...workspace.matchAll(/<ul\b[^>]*>/g)].map((match) => match[0])) {
      expect(list, list).toContain('aria-label=');
    }
  });

  it('shows where the keyboard is', () => {
    expect(tokens).toContain('a:focus-visible');
    expect(tokens).toContain('button:focus-visible');
    expect(tokens.slice(tokens.indexOf('a:focus-visible'))).toContain('outline:');
  });
});
