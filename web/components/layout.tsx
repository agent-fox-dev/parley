import type { FC, PropsWithChildren } from 'hono/jsx';

/**
 * The shell every page is rendered in.
 *
 * It carries the service's own name and marks, the one main
 * landmark each page needs, and the theme control. Nothing else in
 * the interface writes a `<head>` or a masthead, so a page cannot
 * forget the viewport, the stylesheet or the heading structure.
 */
export interface PageProps {
  title: string;
  /** Who is signed in, where anybody is. */
  signedInAs?: string;
}

/** The one stylesheet, served by the web routes. */
export const STYLESHEET = '/web/tokens.css';

/**
 * Remembers the viewer's choice of theme and applies it before the
 * first paint. It is the only script the interface ships: without
 * it a chosen theme would flash the system's on every page.
 */
export const THEME_SCRIPT = `(() => {
  try {
    const chosen = localStorage.getItem('parley-theme');
    if (chosen === 'dark' || chosen === 'light') {
      document.documentElement.dataset.theme = chosen;
    }
  } catch {}
  document.addEventListener('click', (event) => {
    const control = event.target instanceof Element ? event.target.closest('[data-theme-toggle]') : null;
    if (control === null) return;
    const root = document.documentElement;
    const dark = root.dataset.theme
      ? root.dataset.theme === 'dark'
      : window.matchMedia('(prefers-color-scheme: dark)').matches;
    root.dataset.theme = dark ? 'light' : 'dark';
    try {
      localStorage.setItem('parley-theme', root.dataset.theme);
    } catch {}
  });
})();`;

export const Layout: FC<PropsWithChildren<PageProps>> = ({ title, signedInAs, children }) => (
  <html lang="en">
    <head>
      <meta charset="utf-8" />
      <meta name="viewport" content="width=device-width, initial-scale=1" />
      <title>{`${title} · Parley`}</title>
      <link rel="stylesheet" href={STYLESHEET} />
      <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
    </head>
    <body>
      <header class="masthead">
        <div class="page">
          <a class="wordmark" href="/">
            Parley
          </a>
          <span style="flex: 1"></span>
          <button
            type="button"
            class="button button-plain"
            data-theme-toggle
            aria-label="Switch between the dark and light themes"
          >
            Theme
          </button>
          {signedInAs === undefined ? (
            <a class="button" href="/signin">
              Sign in
            </a>
          ) : (
            <>
              <span class="repo-meta">{signedInAs}</span>
              <a class="button button-plain" href="/signout">
                Sign out
              </a>
            </>
          )}
        </div>
      </header>
      <main class="page">{children}</main>
      <footer class="footer">
        <div class="page">Parley — where agents negotiate with your code.</div>
      </footer>
    </body>
  </html>
);
