# Providers

This page describes how each built-in driver talks to its forge: hosts and
URLs, authentication, pagination, rate limits, terminology, which capabilities
are supported, and why some are not. `trove provider show <alias>` prints the
effective URLs and capabilities of a configured account.

- [GitHub](#github) (driver type `github`)
- [GitLab](#gitlab) (driver type `gitlab`)
- [Bitbucket Cloud](#bitbucket-cloud) (driver type `bitbucket`)
- [Bitbucket Server / Data Center](#bitbucket-server--data-center) (planned: `bitbucket-server`)
- [Custom forges](#custom-forges) (driver type `custom`)
- [Common behavior](#common-behavior)

Authentication details for every method are in
[authentication.md](authentication.md).

---

## GitHub

### Deployments, hosts and URLs

| Deployment | `host` | API base | Web base | Reported as |
| --- | --- | --- | --- | --- |
| github.com | `github.com` (default) | `https://api.github.com` | `https://github.com` | `cloud` |
| GitHub Enterprise Cloud with data residency | `<tenant>.ghe.com` | `https://api.<tenant>.ghe.com` | `https://<tenant>.ghe.com` | `cloud` |
| GitHub Enterprise Server | any other host | `https://<host>/api/v3` | `https://<host>` | `enterprise-server` |

`www.github.com` and `api.github.com` are normalized to `github.com`.
`api_base_url`, `web_base_url`, `clone_base_url` and `ssh_host` override the
derived values, including clone URLs returned by the API. HTTPS clone URLs are
`<clone_base_url or web>/<owner>/<repo>.git`; SSH URLs are
`git@<ssh_host>:<owner>/<repo>.git`, or `ssh://git@<host>:<port>/...` when
`ssh_host` carries a port (for example `ssh.github.com:443`).

Only the REST API is used, pinned to version `2022-11-28`
(`X-GitHub-Api-Version`), which github.com and GHES 3.9+ support; older GHES
releases ignore the header.

### Authentication

| Method | Credential | Notes |
| --- | --- | --- |
| `token` | Classic or fine-grained PAT | Classic token scopes are read from `X-OAuth-Scopes`; expiry from `GitHub-Authentication-Token-Expiration` |
| `oauth` | Device flow with your OAuth App or GitHub App client ID, or a pre-issued OAuth token | GitHub App user tokens expire and are refreshed automatically; OAuth App tokens do not expire |
| `app` | GitHub App private key (PEM) | Trove signs a short-lived RS256 JWT, exchanges it for an installation token (cached in memory, never stored) |

Git over HTTPS uses the username `x-access-token`. Server-side revocation is
not available: it needs the OAuth App's client secret, which Trove does not
hold.

### Pagination and rate limits

Pagination follows the `Link: rel="next"` header with `per_page=100`
(notifications: 50). GitHub signals rate limits with `429`, or with `403` plus
`Retry-After` (secondary limits) or `x-ratelimit-remaining: 0` (primary limit,
reset at `x-ratelimit-reset`). Short waits are retried automatically for
idempotent requests; longer ones surface as `Rate limited` (exit code 6)
with the reset time. The search API is limited to 1,000 results per query.

### Terminology

Repository · Pull Request (PR) · Workflow run · Gist · Notification · Organization

### Supported capabilities

Everything in Trove's capability set: repositories (create, delete, fork,
rename, archive), pull requests (create, merge with `merge`/`squash`/`rebase`,
close, checkout via `refs/pull/N/head`), issues (create, close/reopen, labels),
Actions workflow runs (run, cancel, retry, logs), releases (create with draft
and prerelease, delete), organizations, search (repositories, issues, code),
notifications, gists, SSH and GPG keys, profile settings and the dashboard
summary.

### Behavior worth knowing

- **Pull requests are issues** in GitHub's model. Issue listings filter pull
  requests out; `trove issue view <n>` on a pull request number reports "not
  found" with a hint.
- **Merged vs closed**: GitHub reports merged pull requests as `closed` with
  `merged_at` set. Trove splits them client-side, so `--state closed` excludes
  merged pull requests and `--state merged` shows only merged ones.
- **Delete branch on merge**: there is no merge-time option; Trove deletes the
  head branch afterwards, only when it lives in the base repository.
- **Workflow runs** have a lifecycle `status` and, once completed, a
  `conclusion`; both are normalized into Trove's pipeline status (for example
  `failure`, `timed_out` and `startup_failure` all become `failed`).
  `trove pipeline run` needs `--workflow <file or ID>` and a workflow that
  declares `on: workflow_dispatch`; `--var KEY=VALUE` become workflow inputs.
  `retry` re-runs every job of a completed run.
- **Job logs** are served through a redirect to short-lived pre-signed storage
  URLs; Trove follows it without forwarding the `Authorization` header.
- **Issue search** adds `is:issue` unless the query already contains an
  `is:issue`/`is:pr` qualifier. Code search covers default branches only.
- **GitHub App accounts** authenticate as the App's bot (`<slug>[bot]`), list
  repositories from `GET /installation/repositories`, and do **not** offer
  notifications, gists, SSH/GPG keys or settings, because installation tokens
  cannot call user endpoints.

### GHES differences

- Older releases may omit `visibility`; Trove falls back to `private`.
- Workflow dispatch answers `204` without the new run ID, so `pipeline run`
  cannot show the run it created.
- `internal` repositories exist on GHES and GitHub Enterprise Cloud
  organizations (`trove repo create --internal`), not on personal accounts.

---

## GitLab

### Deployments, hosts and URLs

| Deployment | `host` | API base | Web base | Reported as |
| --- | --- | --- | --- | --- |
| GitLab.com | `gitlab.com` (default) | `https://gitlab.com/api/v4` | `https://gitlab.com` | `cloud` |
| Self-Managed / Dedicated | your host | `https://<host>/api/v4` | `https://<host>` | `self-managed` |

Override `api_base_url` and `web_base_url` for instances served under a
relative URL root (for example `https://git.example.com/gitlab/api/v4`). Clone
URLs come from the project's `http_url_to_repo` / `ssh_url_to_repo` unless
`clone_base_url` or `ssh_host` override them. Only REST API v4 is used.

### Model

- Repositories are **projects** in a namespace: a user or a group, and groups
  nest (`group/subgroup/subsub`). Trove's namespace is the full path; the
  repository name is the project path (URL slug), not its display name.
  Projects are addressed by numeric ID when known, otherwise by URL-encoded
  full path.
- Merge requests and issues have a global ID and a project-scoped **IID**
  (`!12`, `#34`). Trove's numbers are always IIDs. Pipelines are addressed by
  pipeline ID, because that is what the pipeline endpoints accept.

### Authentication

| Method | Credential | Header |
| --- | --- | --- |
| `token` | Personal, project or group access token | `PRIVATE-TOKEN` |
| `oauth` | Device authorization grant (GitLab 17.2+, GA in 17.9) or a pre-issued OAuth token | `Authorization: Bearer` |

OAuth access tokens expire after two hours and are refreshed transparently
(and persisted) when expired or rejected. `trove auth logout --revoke` revokes
OAuth tokens with `POST /oauth/revoke` and access tokens with
`DELETE /personal_access_tokens/self` (GitLab 15.5+). Git over HTTPS uses the
username `oauth2` with either kind of token.

### Pagination and rate limits

Offset pagination with `per_page=100`: Trove follows `Link: rel="next"` and
falls back to `X-Next-Page` (some proxies strip `Link`). `X-Total` may be
missing for large collections, in which case dashboard counts show as
unavailable. Throttling is signalled with `429` plus `Retry-After` /
`RateLimit-Reset` and retried automatically; a `403` on GitLab is always a
permission error.

### Terminology

Project · Merge Request (MR) · Pipeline · Snippet · To-Do item · Group

### Supported capabilities

Projects (create, delete, fork, rename, archive), merge requests (create,
merge, close, checkout via `refs/merge-requests/N/head`), issues, CI/CD
pipelines (run, cancel, retry, logs), releases (create, delete), groups,
search, To-Do items as notifications, snippets, SSH and GPG keys, preferences
and status settings, dashboard summary.

### Limitations and differences (by design)

- **Merge methods**: `merge` and `squash` only. Whether a merge creates a merge
  commit, a semi-linear history or a fast-forward is a *project setting*, and
  rebasing is a separate operation (`PUT .../rebase`), so `--method rebase` is
  rejected with exit code 2.
- **Drafts** are created by prefixing the title with `Draft: `.
- **Merge requests from forks** are created on the fork (source) project with
  `target_project_id` pointing at the upstream project (`--head-repo`).
- **Releases** have no draft or prerelease flag (only "upcoming" releases with
  a future date), so `--draft`/`--prerelease` are rejected rather than
  emulated.
- **Pipelines** run the project's `.gitlab-ci.yml` for `--ref`; `--workflow`
  has no GitLab meaning and is ignored; `--var` become pipeline variables.
  `retry` retries the failed and canceled jobs. Logs are job traces, each
  prefixed with `==> <stage>/<job>` when all jobs are printed.
- **Notifications** are To-Do items: "unread" means pending, marking one read
  marks it done. GitLab has no endpoint for a single To-Do, so
  `notification read` scans the list.
- **Search**: repository search uses `GET /projects?search=`; issue and code
  search use the Search API at global, group or project scope. Code (blob)
  search at **global or group scope requires Advanced Search**
  (Elasticsearch/OpenSearch) or exact code search (Zoekt). Instances without
  it answer HTTP 400, which Trove reports as unsupported with a hint to scope
  the search to a project with `-R`.
- **Settings**: profile fields are only writable by administrators, so Trove
  exposes `/user/preferences` (`view_diffs_file_by_file`,
  `show_whitespace_in_diffs`, `pass_user_identities_to_ci_jwt`) and
  `/user/status` (`status.message`, `status.emoji`, `status.availability`).

### Self-managed differences

Feature availability depends on version and tier: token self-introspection and
self-revocation need 15.5+, `detailed_merge_status` 15.6+, the device grant
17.2+, and instance/group code search needs Advanced Search. Older instances
degrade gracefully (for example, `auth status` omits token scopes when
`/personal_access_tokens/self` is missing). Administrators can disable
snippets, CI or other features per instance or project; the resulting 403/404
responses surface as permission denied (exit 3) or not found (exit 4).

---

## Bitbucket Cloud

### Hosts and URLs

| | Value |
| --- | --- |
| `host` | `bitbucket.org` |
| API base | `https://api.bitbucket.org/2.0` |
| Web base | `https://bitbucket.org` |
| HTTPS clone | `https://bitbucket.org/<workspace>/<repo>.git` (or `clone_base_url`) |
| SSH clone | `git@ssh.bitbucket.org:<workspace>/<repo>.git` (or `ssh_host`) |

Git over SSH moves from `bitbucket.org` to `ssh.bitbucket.org` (the old host
stops accepting SSH in November 2026), so computed SSH URLs already use
`ssh.bitbucket.org`.

### Model

Workspace → (Project) → Repository. Trove's namespace is the **workspace
slug** and the repository name is the repository slug. The provider-specific
`workspace` setting is the account's default workspace:

```yaml
providers:
  bb:
    type: bitbucket
    host: bitbucket.org
    workspace: acme
```

It scopes code search and snippet/repository creation when no `--namespace`
is given, and it is **required** for access tokens.

The cross-workspace listing endpoints (`GET /2.0/repositories`,
`/2.0/workspaces`, `/2.0/user/permissions/workspaces`, `/2.0/snippets`) were
removed by Atlassian on 14 April 2026. Trove discovers workspaces with
`GET /2.0/user/workspaces` and lists per workspace.

### Authentication

| Method | Credential | Transport | Git username |
| --- | --- | --- | --- |
| `basic` (default) | Atlassian API token + account **email** | HTTP Basic | `x-bitbucket-api-token-auth` |
| `access-token` | Repository, project or workspace access token | Bearer | `x-token-auth` |
| `oauth` | OAuth consumer key + secret (client credentials grant) | Bearer | `x-token-auth` |

**App passwords are not supported**: Atlassian stopped issuing them on
9 September 2025 and disabled the remaining ones in 2026. OAuth consumer
tokens expire and are re-issued automatically (refresh token, or a new client
credentials grant with the stored consumer secret). There is no server-side
revoke; revoke tokens in Bitbucket's settings.

### Pagination and rate limits

Collections are JSON envelopes whose `next` member is an absolute URL; Trove
follows it only when it points at the configured API base. Page size is
`pagelen=100` (pull request collections: 50). Rate limiting is signalled only
with `429` and retried automatically.

### Terminology

Repository · Pull Request (PR) · Pipeline · Snippet · Workspace

### Supported capabilities

Repositories (create, delete, fork, rename), pull requests (create, merge with
`merge` → `merge_commit`, `squash`, `rebase` → `rebase_fast_forward`, or the
repository default; close = decline), Pipelines (list, view, run, stop,
logs), workspaces, repository search, code search, snippets (per workspace),
SSH and GPG keys, dashboard summary.

### Not supported, and why

| Feature | Reason |
| --- | --- |
| Issues, issue search | Atlassian removed the Bitbucket Cloud issue tracker and its API on 20 August 2026 |
| Releases | Bitbucket has no release objects (use tags) |
| Notifications | No notifications API |
| Repository archive | No archive flag |
| Pipeline retry | No re-run endpoint (`trove pipeline run` starts a new one) |
| Settings | No safely writable account settings API |
| Server-side revoke | Not offered for API tokens, access tokens or consumers |

### Behavior worth knowing

- **Pull request checkout**: there are no fetchable pull request refs, so
  Trove fetches the source branch, from the fork when the pull request comes
  from one.
- **Pipelines**: `trove pipeline run --ref <branch>` runs the branch pipeline;
  `--workflow <name>` selects a custom pipeline (selector type `custom`) from
  `bitbucket-pipelines.yml`. Pipeline IDs are UUIDs with braces. Status
  filtering is client-side because one normalized status covers several
  Bitbucket states. Step logs redirect to storage, followed without the
  `Authorization` header.
- **Code search** is scoped to a workspace (`--namespace`, `-R`, or the
  `workspace` setting); `-R` narrows it with the `repo:` qualifier. Atlassian
  has announced the deprecation of this API for 1 November 2026.
- **Access tokens** have no user; `auth status` validates them against the
  configured workspace, and user-scoped features (for example listing your SSH
  keys) need a user credential.

---

## Bitbucket Server / Data Center

Not implemented yet. Bitbucket Server / Data Center is a different product
with a different REST API (`/rest/api/1.0`): repositories live in projects
rather than workspaces, collections page with
`start`/`limit`/`isLastPage`/`nextPageStart`, and authentication uses HTTP
access tokens or personal access tokens with different permission semantics.

The architecture is ready for it: it will be a separate driver type,
`bitbucket-server`, in `providers/bitbucket/server`, sharing no API client
code with the Cloud driver. Remote detection already classifies self-hosted
hosts containing `bitbucket` or `stash` as `bitbucket-server`. Until the
driver exists, `trove provider add --type bitbucket-server` fails with
"provider type not supported" (exit code 5).

---

## Custom forges

Driver type `custom` speaks the **Trove Forge Protocol v1**, a small REST+JSON
contract whose payloads are Trove's own domain models. Capabilities,
authentication header and scheme, pagination strategy (`link`, `page`,
`cursor`, `none`) and clone URL templates are all declared in the account's
`custom:` block, and the capability set is exactly what you list. TFP v1
covers repositories (list, view, create, delete), pull requests (list, view,
create, merge, close), issues, pipelines and releases (read-only),
namespaces and the dashboard summary.

Full specification and configuration reference:
[custom-providers.md](custom-providers.md).

---

## Common behavior

These apply to every driver:

- **Construction is offline.** Creating a provider never touches the network;
  credentials load lazily on the first request, so `provider show`,
  `config validate` and `--help` work offline.
- **Retries.** Idempotent requests are retried on network errors, 429, 502,
  503 and 504 with exponential backoff and jitter (3 retries), honoring
  `Retry-After` up to 60 seconds. Writes are never retried implicitly.
- **Redirects.** Credentials are dropped whenever a redirect or pagination
  link leaves the configured origin.
- **Errors.** Only the provider's error message is surfaced, mapped to typed
  errors and exit codes; raw response bodies and credentials never appear.
- **Normalization.** States and statuses are normalized (`open`, `closed`,
  `merged`; `pending`, `running`, `success`, `failed`, `canceled`, `skipped`,
  `manual`), while JSON output keeps provider-native values where useful
  (`raw_status`) and human output uses provider terminology.
