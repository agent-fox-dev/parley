import type { FC } from 'hono/jsx';
import { Layout } from '../components/layout.js';

/**
 * One answer for every workspace a person cannot open: one that
 * belongs to somebody else, one they may not see, and an address
 * naming nothing at all. They are indistinguishable on purpose —
 * telling them apart would let anybody learn which slugs exist by
 * trying them.
 */
export const NotAvailable: FC<{ signedInAs?: string }> = ({ signedInAs }) => (
  <Layout title="Not available" {...(signedInAs === undefined ? {} : { signedInAs })}>
    <h1>Not available</h1>
    <p>
      There is no workspace here for you to read. It may not exist, or it may belong to somebody who
      has not shared it.
    </p>
    <p>
      <a class="button button-plain" href="/dashboard">
        Back to your workspaces
      </a>
    </p>
  </Layout>
);
