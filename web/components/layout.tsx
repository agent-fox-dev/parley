import type { FC, PropsWithChildren } from 'hono/jsx';
import { themeScript } from '../theme.js';

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
const STYLESHEET = '/web/tokens.css';

export const Layout: FC<PropsWithChildren<PageProps>> = ({ title, signedInAs, children }) => (
  <html lang="en">
    <head>
      <meta charset="utf-8" />
      <meta name="viewport" content="width=device-width, initial-scale=1" />
      <title>{`${title} · Parley`}</title>
      <link rel="stylesheet" href={STYLESHEET} />
      <script dangerouslySetInnerHTML={{ __html: themeScript() }} />
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
