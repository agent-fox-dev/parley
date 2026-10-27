# parley

Where agents negotiate with your code - Rarr !

Parley is the server that AI coding agents, and the people who run them, work
through. It holds everything an agent run needs except the agent itself: the
repository, the credentials, the secrets, a sandbox to run in, and a record of
what happened and what it cost.

## Why

Pointing coding agents at real repositories raises the same questions every
time:

- **Where does the agent's copy of the code live**, and how do several agents
  work on one repository without stepping on each other?
- **How do their changes get back** into the main branch without a person
  rebasing and merging every branch by hand?
- **Which credentials does an agent get?** Forge tokens and API keys copied
  into an agent's environment are hard to scope and harder to revoke.
- **Where does the agent run**, isolated from the machine it was started on?
- **What did it do, and what did it cost?**

Without a common answer, every team scripts its own: tokens end up in
environment files, branches are merged by whoever notices, and the trail of
what an agent did is scattered across terminals.