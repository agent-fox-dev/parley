/**
 * Which theme a page is read in.
 *
 * The default is the viewer's own system setting, which the
 * stylesheet answers with a media query and no script at all. A
 * viewer who wants the other one says so, and that choice has to be
 * applied before the first paint — otherwise every page flashes the
 * system's theme first — which is why the few lines below are
 * inline rather than a file the browser fetches.
 *
 * They are the whole of the interface's client-side code, and they
 * are generated from this one constant so that what the browser
 * stores and what the tests look for cannot be two different keys.
 */
export type Theme = 'dark' | 'light';

/** Where the choice is kept, in the viewer's own browser. */
export const THEME_KEY = 'parley-theme';

/** The script every page carries: apply the choice, and offer the toggle. */
export function themeScript(): string {
  return `(() => {
  const KEY = ${JSON.stringify(THEME_KEY)};
  const root = document.documentElement;
  try {
    const chosen = localStorage.getItem(KEY);
    if (chosen === 'dark' || chosen === 'light') root.dataset.theme = chosen;
  } catch {}
  document.addEventListener('click', (event) => {
    const control = event.target instanceof Element ? event.target.closest('[data-theme-toggle]') : null;
    if (control === null) return;
    const dark = root.dataset.theme
      ? root.dataset.theme === 'dark'
      : window.matchMedia('(prefers-color-scheme: dark)').matches;
    root.dataset.theme = dark ? 'light' : 'dark';
    try {
      localStorage.setItem(KEY, root.dataset.theme);
    } catch {}
  });
})();`;
}
