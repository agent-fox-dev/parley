import type { FC } from 'hono/jsx';
import { Layout } from '../components/layout.js';

/**
 * What a visitor sees before they sign in.
 *
 * It says what the service is and offers the one thing a visitor
 * can do, and it says nothing whatever about what this deployment
 * holds: a landing page that counted workspaces would be telling
 * anybody who asked how busy the place is.
 */
export const Landing: FC<{ providers: Array<{ name: string }> }> = ({ providers }) => (
  <Layout title="Parley">
    <h1>Parley</h1>
    <p>
      Parley gives an agent a workspace of its own: a clone of your repository it can read and
      change, a credential it never sees, and a record of everything it did. You keep the token; the
      agent keeps working.
    </p>
    <p>
      Each workspace follows a fork or an upstream, carries a stack of patches where you want one,
      and reports what it cost. Everything an agent does through Parley is attributable, because
      Parley is the one holding the key.
    </p>

    <h2>What you can see here</h2>
    <ul>
      <li>The workspaces you own or share, and how each one stands.</li>
      <li>A patch stack&apos;s health: what applies, what conflicts, what was rebuilt.</li>
      <li>What happened recently, and what it cost.</li>
    </ul>
    <p>
      Changing anything stays with the API and the <code>afc</code> command. This interface only
      reads.
    </p>

    <h2>Sign in</h2>
    {providers.length === 0 ? (
      <p class="notice">
        This deployment has no sign-in provider configured, so there is nothing to sign in with yet.
      </p>
    ) : (
      <p>
        {providers.map((provider) => (
          <a key={provider.name} class="button" href={`/signin/${provider.name}`}>
            {`Sign in with ${provider.name}`}
          </a>
        ))}
      </p>
    )}
    <p class="repo-meta">
      Signing in here gives this browser a session of its own. It does not touch the API keys your
      automation holds.
    </p>
  </Layout>
);
