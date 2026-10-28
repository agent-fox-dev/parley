# 11. What the two forges really answer

## Status

Accepted.

## Context

Eighteen routes read and change a workspace's repository on GitHub or
GitLab. The API reference says what Parley must answer; it does not say
what GitHub and GitLab answer, and the two are not the same shape. The
spec's own note is explicit that nothing here was verified against
installed code, because there is no client library to verify: this
package speaks each forge's HTTP API directly.

So before the mappers were written, both forges' current documentation
was read and recorded. What follows is what each one really offers,
and where that differs from what the reference asks Parley to serve.

## What GitHub offers

| Parley's route          | GitHub                                                                                                           |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------- |
| issue list              | `GET /repos/{owner}/{repo}/issues` with `state`, `labels`, `assignee`, `sort`, `direction`, `per_page`           |
| issue create            | `POST /repos/{owner}/{repo}/issues` — `title`, `body`, `labels`, `assignees`                                     |
| issue update, close     | `PATCH /repos/{owner}/{repo}/issues/{n}` — `state` is `open` or `closed`                                         |
| comments                | `GET`/`POST /repos/{owner}/{repo}/issues/{n}/comments` — `body`                                                  |
| labels on an issue      | `POST .../issues/{n}/labels`, `DELETE .../issues/{n}/labels/{name}`                                              |
| the repository's labels | `GET`/`POST /repos/{owner}/{repo}/labels` — `name`, `color` without `#`, `description`                           |
| pull requests           | `POST`/`GET /repos/{owner}/{repo}/pulls[/{n}]` — `head`, `base`, `draft`; `head` takes `owner:branch` for a fork |
| changed files           | `GET /repos/{owner}/{repo}/pulls/{n}/files`                                                                      |
| reviews                 | `GET /repos/{owner}/{repo}/pulls/{n}/reviews`                                                                    |
| checks                  | `GET /repos/{owner}/{repo}/commits/{sha}/check-runs`, so the pull request's head has to be read first            |

Rate limiting: `x-ratelimit-limit`, `-remaining`, `-used`, `-reset`
(epoch seconds) on every answer, and `retry-after` (seconds) when a
secondary limit is hit. A rate-limited request is answered **403 or
429**, which is why Parley tells a rate-limited 403 from an ordinary one
by its headers rather than by its status.

## What GitLab offers

| Parley's route | GitLab                                                                                                 |
| -------------- | ------------------------------------------------------------------------------------------------------ |
| the project    | `/projects/{owner%2Fname}` — the namespace path, URL-encoded, as one segment                           |
| issue list     | `GET /projects/:id/issues` with `state`, `labels`, `assignee_username`, `order_by`, `sort`, `per_page` |
| issue create   | `POST /projects/:id/issues` — `title`, `description`                                                   |
| issue update   | `PUT /projects/:id/issues/:iid`                                                                        |
| comments       | `GET`/`POST /projects/:id/issues/:iid/notes` — `body`                                                  |
| labels         | `GET`/`POST /projects/:id/labels` — `color` **with** `#`                                               |
| merge requests | `POST`/`GET /projects/:id/merge_requests[/:iid]` — `source_branch`, `target_branch`, `description`     |
| changed files  | `GET /projects/:id/merge_requests/:iid/changes`                                                        |
| reviews        | approvals (`.../approvals`), there being no review object                                              |
| checks         | the merge request's pipelines (`.../pipelines`), there being no check run                              |

Rate limiting: `RateLimit-Limit`, `-Remaining`, `-Reset`, plus
`RateLimit-ResetTime` and `Retry-After` on a 429. `Retry-After` is the
time left in the current window rather than the length of the window.

## The divergences that matter

1. **The field names differ.** GitLab's issue body is `description` and
   its branches are `source_branch` and `target_branch`. The mapper
   translates; nothing above the mapper knows.
2. **The state words differ.** The caller says `open`; GitLab says
   `opened`, and closing is `state_event: close` rather than
   `state: closed`. A filter passed through unchanged would silently
   match nothing on GitLab.
3. **`sort` and `direction` are one parameter each on GitHub and
   `order_by` and `sort` on GitLab**, and GitHub's `sort` takes
   `comments` as well as the two values the reference allows. Parley
   validates the reference's values and translates.
4. **Neither reviews nor check runs exist on GitLab.** Approvals and
   pipelines are the nearest things, and that is what the mapper
   returns, in the reference's own shape.
5. **A rate-limited GitHub request may be a 403.** The failure mapping
   tells it from a plain `forge_forbidden` by the rate-limit headers.
6. **The body limits are not symmetrical in standing.** GitLab
   documents its 1,000,000-character cap on descriptions and comments.
   GitHub's 65,536 is in its API's error messages and in community
   records, not in its documentation, and some reports suggest the real
   cap is counted in bytes. Parley uses the reference's figures, which
   are the ones above, and refuses locally with
   `413 body_too_large`; a forge that refuses a body Parley allowed is
   answered like any other refusal, so the figure being slightly wrong
   costs a round trip and not a wrong answer.

## Decision

Implement one mapper per forge against the tables above, translating
names and state words at the mapper and nowhere else, and record the
divergences here rather than in the code, where each would otherwise
be an unexplained special case.

## Consequences

A forge that changes its API changes one mapper. The two shapes Parley
returns for GitLab's reviews and checks are the nearest equivalents
rather than the same thing, and this document is where that is said.
