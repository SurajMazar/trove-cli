# Custom forge providers

Trove talks to forges through **drivers**. GitHub, GitLab and Bitbucket each have a
compiled-in driver. There are two ways to connect a forge Trove does not ship with:

| Path | When to use it | What you build |
| --- | --- | --- |
| **A. Trove Forge Protocol (driver type `custom`)** | An in-house or niche forge you control, or any forge you can put a small adapter service in front of. | An HTTP service speaking the **Trove Forge Protocol v1 (TFP)**, plus a declarative block in Trove's config. No Trove code changes. |
| **B. Compiled-in driver** | A widely used forge with its own public API (Gitea, Forgejo, Azure DevOps, SourceHut, ...) that should work out of the box for every Trove user. | A Go package `providers/<name>` implementing `forge.Driver`, registered in the driver registry. |

The `custom` driver does **not** emulate the GitHub, GitLab or Bitbucket APIs. TFP is a
small REST+JSON contract, and its payloads are Trove's own domain models (the JSON form
of `internal/domain`). An adapter only has to translate the forge's data into those
models. It never has to copy another forge's API quirks.

---

## A. Using the `custom` driver

### Configuration

```yaml
providers:
  custom-company:
    type: custom
    host: git.example.com
    api_base_url: https://git.example.com/api      # required
    clone_base_url: https://git.example.com        # required for the template clone strategy
    web_base_url: https://git.example.com          # optional (defaults to https://<host>)
    ssh_host: git.example.com                      # optional (defaults to host without port)
    auth:
      type: token
      secret_ref: keychain://trove/custom/company/token
    custom:
      display_name: Example Forge
      capabilities: [repositories, repositories.create, repositories.delete,
                     pull_requests, pull_requests.create, issues, issues.create,
                     issues.state, pipelines, releases, namespaces]
      git_username: trove            # optional, username handed to git over HTTPS
      auth:
        header: Authorization        # header that carries the credential
        scheme: Bearer               # value prefix; "" = raw token; "basic" = HTTP Basic
        username: alice              # only for scheme: basic
      pagination:
        strategy: link               # link | page | cursor | none
        page_param: page             # page strategy
        per_page_param: per_page     # page, link and cursor strategies
        cursor_param: cursor         # cursor strategy
        max_page_size: 100
      clone:
        strategy: template           # template | api
        https: "{clone_base_url}/{namespace}/{name}.git"
        ssh: "git@{ssh_host}:{namespace}/{name}.git"
        web: "{web_base_url}/{namespace}/{name}"
      terms:                         # optional terminology overrides
        pull_request: Change Request
        pull_request_short: CR
```

| Key | Default | Notes |
| --- | --- | --- |
| `api_base_url` | *(required)* | Absolute `http`/`https` URL with no embedded credentials. TFP paths such as `/v1/user` are appended to it, so a base path like `/api` is kept. |
| `host` | host of `api_base_url` | Shown in `trove provider list` and used for `{host}`. |
| `clone_base_url` | none | Required when a template uses `{clone_base_url}` and `clone.strategy` is `template`. |
| `web_base_url` | `https://<host>` | Used for `{web_base_url}`. |
| `ssh_host` | `host` without the port | Used for `{ssh_host}`. |
| `custom.display_name` | `Custom Forge` | Used in messages ("Example Forge authentication failed: ..."). |
| `custom.capabilities` | `[repositories]` | See [Capabilities](#capabilities). |
| `custom.git_username` | see [Git over HTTPS](#git-over-https) | |
| `custom.auth.header` | `Authorization` | Any valid HTTP header name, for example `X-Forge-Key`. |
| `custom.auth.scheme` | `Bearer` | `Bearer` sends `Bearer <token>`. `""` sends the raw token. `basic` sends HTTP Basic. Any other single word `W` sends `W <token>`. |
| `custom.auth.username` | account `username` | The Basic-auth username. It is only used with `scheme: basic`. |
| `custom.pagination.*` | `link`, `page`, `per_page`, `cursor`, `100` | See [Pagination](#pagination). |
| `custom.clone.*` | `template` and the templates shown above | See [Clone URLs](#clone-urls). |
| `custom.terms.*` | `Repository`, `Pull Request`, `PR`, `Pipeline`, `Snippet`, `Notification`, `Namespace` | The keys are `repository`, `pull_request`, `pull_request_short`, `pipeline`, `snippet`, `notification` and `namespace`. |

Trove validates the whole block when it loads the provider, without making a network
request. Every problem is reported at once as an `invalid configuration` error (exit
code 2) that names the offending entry. For example:

```
invalid custom provider configuration: capabilities: "notifications" is not supported by
Trove Forge Protocol v1; clone.https template "{clone_base_url}/{owner}/{name}.git":
unknown placeholder {owner} (...)
```

### Capabilities

`custom.capabilities` is the **exact** set of features Trove offers for this account.
If a capability is not listed, Trove refuses it with `provider "custom-company" does not
support <feature>` (exit code 5) and sends no request. List only the capabilities your
adapter implements.

TFP v1 defines endpoints for these capabilities:

| Capability | Requires | Endpoints |
| --- | --- | --- |
| *(always)* | | `GET /v1/user` |
| `repositories` | | `GET /v1/repositories`, `GET /v1/repositories/{repo}` |
| `repositories.create` | `repositories` | `POST /v1/repositories` |
| `repositories.delete` | `repositories` | `DELETE /v1/repositories/{repo}` |
| `pull_requests` | | `GET .../pull-requests`, `GET .../pull-requests/{number}` |
| `pull_requests.create` | `pull_requests` | `POST .../pull-requests` |
| `pull_requests.merge` | `pull_requests` | `POST .../pull-requests/{number}/merge` |
| `pull_requests.close` | `pull_requests` | `POST .../pull-requests/{number}/close` |
| `issues` | | `GET .../issues`, `GET .../issues/{number}` |
| `issues.create` | `issues` | `POST .../issues` |
| `issues.state` | `issues` | `PATCH .../issues/{number}` |
| `issues.labels` | `issues` | the `labels` filter on `GET .../issues` and `labels` in `POST .../issues` |
| `pipelines` | | `GET .../pipelines`, `GET .../pipelines/{id}` |
| `releases` | | `GET .../releases`, `GET .../releases/{tag}` |
| `namespaces` | | `GET /v1/namespaces`, `GET /v1/namespaces/{path}` |
| `summary` | | `GET /v1/summary` |

Configuration is rejected in these cases:

- The name is not a Trove capability, for example `teleport`.
- The name is a Trove capability that TFP v1 does not define, for example
  `notifications`, `repositories.fork`, `pipelines.run` or `snippets`. These need a
  compiled-in driver (path B) or a future TFP version.
- A sub-capability is listed without its parent, for example `repositories.create`
  without `repositories`.

### Authentication

Credentials are stored in Trove's secret store like those of any other provider:

```sh
trove auth login custom-company          # asks for the token
trove auth status custom-company
```

`trove auth login` checks the token by calling `GET /v1/user`, and Trove stores the token
only if that call succeeds. With `scheme: basic`, the username comes from the login
request, then `custom.auth.username`, then the account's `username`. It is stored with
the credential. The auth methods are `token`, plus `basic` when `scheme: basic` is set.

The credential header is attached only to requests whose origin (scheme, host and port)
matches `api_base_url`. Redirects and pagination links that point anywhere else never
receive it. The header is added below Trove's debug-logging layer, so it cannot appear in
`--debug` output even when it has a non-standard name.

### Git over HTTPS

Git operations (clone, fetch, push) use the API token through Trove's git
credential helper. The token never goes into the URL. The username that goes with it is
chosen in this order:

1. `custom.git_username`, if set.
2. The Basic-auth username, when `scheme: basic` is used.
3. `trove`. Most token-authenticating Git servers ignore the username.

### Clone URLs

Two placeholders describe the repository and five come from configuration:

| Placeholder | Value |
| --- | --- |
| `{namespace}` | Full namespace path, which may contain `/` (e.g. `acme/platform/tools`) |
| `{name}` | Repository name |
| `{full_name}` | `{namespace}/{name}` |
| `{clone_base_url}`, `{web_base_url}`, `{ssh_host}`, `{host}` | From the configuration above |

Values are inserted as they are, and `/` inside the namespace is not escaped. Unknown
placeholders and unbalanced braces are configuration errors.

- **`strategy: template`** (default): the templates are authoritative. Trove builds the
  HTTPS, SSH and web URLs itself and ignores the `urls` the server sends. This suits
  forges with a predictable layout, or sites that route Git through a different host
  than the API.
- **`strategy: api`**: Trove uses the `urls.https`, `urls.ssh` and `urls.web` values from
  TFP payloads. When a value is missing or is not an http(s) URL, Trove falls back to the
  template, but only if the template can be rendered. `clone_base_url` is optional here.
  Without it, a repository that has no `urls.https` in its payload has no HTTPS clone
  URL.

`urls.api` is always computed by Trove as `<api_base_url>/v1/repositories/{repo}`.

---

## Trove Forge Protocol v1

The key words MUST, SHOULD and MAY are used as in RFC 2119.

### Conventions

- **Base URL.** Every path below is relative to `api_base_url`. With
  `api_base_url: https://git.example.com/api`, the user endpoint is
  `https://git.example.com/api/v1/user`.
- **Encoding.** Request and response bodies are UTF-8 JSON
  (`Content-Type: application/json`). Trove sends `Accept: application/json` and a
  `User-Agent: trove/<version>` header.
- **Models.** Payloads use the field names of Trove's domain models, listed
  [below](#models). Servers MAY omit optional fields, and MUST NOT rely on Trove sending
  fields it does not document. Trove ignores unknown response fields. New optional fields
  can appear within v1.
- **Identifiers.** Every `id` is a JSON **string**, even when the forge uses numbers
  (`"id": "42"`). PR and issue `number` values are JSON integers.
- **Timestamps.** RFC 3339 strings (`"2026-09-01T10:00:00Z"`). Omit a field when it has
  no value.
- **Repository identifier `{repo}`.** A repository is addressed by its full name
  (`namespace/name`), escaped as **one** path segment the way Go's `url.PathEscape` does
  it. `acme/platform/tools/deployer` becomes
  `/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer`. The same rule applies to
  `{path}` in the namespace endpoints, `{tag}` in releases and `{id}` in pipelines.
  Servers MUST decode the segment. Many frameworks and proxies decode `%2F` early or
  reject it: route on the raw path, and set options such as nginx's
  `proxy_pass` without a URI part, or Apache's `AllowEncodedSlashes NoDecode`.
- **Trust.** Trove always overwrites `provider` and `provider_type` on repositories with
  its own values. It rebuilds `full_name` from `namespace` and `name`, or derives both
  from `full_name` when they are missing. A missing or unknown `visibility` is treated
  as `private`.
- **Success codes.** Any `2xx` status is success. Endpoints documented as `204` MAY
  return `200` with a body, which Trove ignores.

### Authentication

Each request carries the configured header (default `Authorization: Bearer <token>`).
Servers MUST answer `401` with code `unauthorized` when the credential is missing,
invalid or expired, and `403` with code `forbidden` when it is valid but not allowed.
Servers SHOULD support HTTPS only.

### Errors

Every non-2xx response SHOULD carry the error envelope:

```json
{"error": {"code": "not_found", "message": "repository acme/nope not found"}}
```

| `code` | Status | Trove error (exit code) |
| --- | --- | --- |
| `unauthorized` | 401 | authentication failed (3), with the hint `trove auth login <alias>` |
| `forbidden` | 403 | permission denied (3) |
| `not_found` | 404 | not found (4). On repository endpoints: repository not found (4) |
| `conflict` | 409 | conflict (1), e.g. the repository already exists |
| `invalid` | 400 or 422 | invalid argument (2), e.g. validation failures or an unsupported merge method |
| `rate_limited` | 429 | rate limited (6) |
| anything else | 5xx | provider API error (1) |

- The **code is authoritative**. Trove falls back to the HTTP status when the code is
  missing or unknown, and a body without the envelope is mapped from the status alone.
- `message` is shown to the user. After Trove scrubs it, it is limited to 300
  characters. It MUST NOT contain secrets. Trove also removes the request's own token if
  the message echoes it.
- **Rate limits.** Send `429` with `code: rate_limited` and a `Retry-After` header in
  delta-seconds or as an HTTP date. Trove retries idempotent `GET` requests on `429`, and
  on `502`, `503` and `504`, when the wait is 60 seconds or less. Longer waits are
  reported to the user with the retry time. A gateway that can only return `403` MAY
  include `Retry-After`, and Trove then treats the response as a rate limit.

### Pagination

Every list endpoint (`GET /v1/repositories`, `.../pull-requests`, `.../issues`,
`.../pipelines`, `.../releases` and `/v1/namespaces`) uses the configured strategy.
Trove asks for `min(limit, max_page_size)` items per page. Configure `max_page_size` no
higher than the server's real maximum.

**`link`**: Trove sends `per_page_param` and reads the RFC 8288 `Link` header:

```http
GET /api/v1/repositories?per_page=100
200 OK
Link: <https://git.example.com/api/v1/repositories?per_page=100&page=2>; rel="next"
[ {...}, {...} ]
```

The `next` target may be relative, and Trove resolves it against the request URL. Trove
refuses to follow a `next` link whose origin differs from `api_base_url` and reports a
protocol error, so the credential is never sent to another host. No `rel="next"` means
the last page.

**`page`**: Trove sends `page_param` (starting at 1) and `per_page_param`:

```http
GET /api/v1/repositories?page=2&per_page=100
200 OK
X-Next-Page: 3          (optional)
[ {...}, {...} ]
```

If the response has an `X-Next-Page` header, its value is authoritative: a positive
integer names the next page, and an empty value marks the last page. Without the header,
a page with fewer than `per_page` items is the last page.

**`cursor`**: Trove sends `per_page_param` and, after the first page, `cursor_param`
holding the previous `next_cursor`. The response is an envelope rather than a bare array:

```http
GET /api/v1/repositories?per_page=100&cursor=eyJvIjoxMDB9
200 OK
{"items": [ {...}, {...} ], "next_cursor": "eyJvIjoyMDB9"}
```

An empty or missing `next_cursor` marks the last page. Cursors are opaque to Trove,
which sends them back URL-encoded.

**`none`**: one request that returns every item as a JSON array. Trove applies `limit`
on its side.

With `link`, `page` and `none`, list responses MUST be bare JSON arrays. With `cursor`,
they MUST be the `items` envelope. Anything else is a protocol error.

**Filters.** List filters are passed as query parameters. Servers SHOULD apply them.
Trove re-applies each documented filter to the results as well, so an adapter that
ignores a filter still returns correct results, only less efficiently. `owned` is the
exception: Trove cannot check it on its side.

### Models

Only the fields listed here are part of TFP v1. Fields marked * are required in responses.

**User**

```json
{"id": "u1", "username": "alice", "name": "Alice Doe", "email": "alice@example.com",
 "web_url": "https://git.example.com/alice", "avatar_url": "https://..."}
```

At least one of `id` and `username` is required. Trove shows `username`.

**Repository**

```json
{
  "id": "1042", "name": "deployer", "namespace": "acme/platform/tools",
  "full_name": "acme/platform/tools/deployer",
  "description": "Deploy tooling", "visibility": "internal", "default_branch": "main",
  "archived": false, "fork": false, "empty": false, "language": "Go",
  "stars": 12, "forks": 3, "open_issues": 4, "topics": ["deploy"],
  "created_at": "2024-01-02T03:04:05Z", "updated_at": "2026-09-01T10:00:00Z",
  "pushed_at": "2026-09-01T09:00:00Z",
  "urls": {"web": "https://git.example.com/acme/platform/tools/deployer",
           "https": "https://git.example.com/acme/platform/tools/deployer.git",
           "ssh": "git@git.example.com:acme/platform/tools/deployer.git"}
}
```

`name`* and `namespace`* are required, or `full_name` alone from which Trove derives
them. `visibility` is one of `public`, `private` or `internal`. `urls` is used only with
`clone.strategy: api`. `provider` and `provider_type` are ignored.

**PullRequest**

```json
{"id": "p77", "number": 7, "title": "Add rollout", "body": "...", "state": "open",
 "draft": false, "author": "alice", "source_branch": "rollout", "target_branch": "main",
 "source_repo": "alice/deployer", "head_sha": "9f1c...", "mergeable": true,
 "labels": ["deploy"], "reviewers": ["bob"],
 "web_url": "https://git.example.com/acme/platform/tools/deployer/pulls/7",
 "created_at": "...", "updated_at": "...", "merged_at": "...", "closed_at": "..."}
```

`number`*, `title`*, `state`* (`open`, `closed` or `merged`), `source_branch`* and
`target_branch`* are required. `source_repo` is set only for PRs from forks. `mergeable`
is omitted when unknown.

**Issue**

```json
{"id": "i12", "number": 12, "title": "Crash on start", "body": "...", "state": "open",
 "raw_state": "triaged", "author": "bob", "assignees": ["alice"], "labels": ["bug"],
 "comments": 3, "web_url": "https://...", "created_at": "...", "updated_at": "...",
 "closed_at": "..."}
```

`number`*, `title`* and `state`* (`open` or `closed`) are required. `raw_state` MAY
carry a richer native state.

**Pipeline** and **PipelineJob**

```json
{"id": "5001", "number": 88, "name": "ci", "status": "failed", "raw_status": "failed",
 "ref": "main", "sha": "9f1c...", "event": "push", "actor": "alice",
 "web_url": "https://...", "created_at": "...", "started_at": "...", "finished_at": "...",
 "duration": 90000000000,
 "jobs": [{"id": "j1", "name": "test", "stage": "test", "status": "failed",
           "raw_status": "failed", "web_url": "https://...",
           "started_at": "...", "finished_at": "..."}]}
```

`id`* and `status`* are required. `status` is one of `pending`, `running`, `success`,
`failed`, `canceled`, `skipped`, `manual` or `unknown`. Other values are shown as
`unknown`, and the original value is kept in `raw_status`. `duration` is an integer
number of **nanoseconds** (Go's `time.Duration`). If it is omitted, Trove computes it from
`started_at` and `finished_at`. `jobs` is expected only from `GET .../pipelines/{id}`.

**Release** and **Asset**

```json
{"id": "r3", "tag": "v1.2.0", "name": "1.2.0", "body": "Notes", "draft": false,
 "prerelease": false, "author": "alice", "web_url": "https://...",
 "created_at": "...", "published_at": "...",
 "assets": [{"name": "deployer_linux_amd64.tar.gz", "url": "https://...", "size": 1234567}]}
```

`tag`* is required.

**Namespace**

```json
{"id": "n2", "name": "platform", "full_path": "acme/platform", "type": "group",
 "parent_path": "acme", "description": "...", "web_url": "https://...",
 "avatar_url": "https://...", "label": "Team"}
```

`full_path`* and `type`* are required. `type` is one of `organization`, `group`,
`subgroup`, `workspace`, `project` or `user`. If `label` is omitted, Trove uses
`terms.namespace`.

**AccountSummary**

```json
{"repositories": 261, "open_pull_requests": 3, "open_issues": 17,
 "running_pipelines": 1, "unread_notifications": 0}
```

Every field is optional. **Omit a count you cannot compute cheaply**: Trove shows a
missing field as "not available", never as zero.

### Endpoints

In the path column, `{repo}` stands for `/v1/repositories/{repo}`.

#### `GET /v1/user`: current user (always required)

`200` returns a User. `401` means the credential is invalid. Trove calls this endpoint
for `trove auth login`, `trove auth status` and connectivity checks.

#### Repositories

| Method and path | Capability | Request | Success |
| --- | --- | --- | --- |
| `GET /v1/repositories` | `repositories` | query `namespace`, `visibility`, `include_archived`, `owned` | `200` paginated `[Repository]` |
| `GET /v1/repositories/{repo}` | `repositories` | | `200` Repository, `404` |
| `POST /v1/repositories` | `repositories.create` | body below | `201` Repository, `409`, `422` |
| `DELETE /v1/repositories/{repo}` | `repositories.delete` | | `204`, `404` |

List query parameters. Each one is sent only when set:

- `namespace=acme/platform`: repositories in this namespace **or any namespace nested
  below it**.
- `visibility=public|private|internal`.
- `include_archived=true`: include archived repositories. When the parameter is absent,
  archived repositories MUST be excluded.
- `owned=true`: only repositories owned by the authenticated user.

The list endpoint returns every repository the user can access that matches the filters.

Create body:

```json
{"name": "svc", "namespace": "acme/platform", "description": "d",
 "visibility": "internal", "default_branch": "trunk", "auto_init": true}
```

`namespace` is omitted for the user's personal namespace. `visibility`,
`description` and `default_branch` are omitted when the user did not set them, and the
server chooses defaults. `auto_init: true` asks for an initial commit.

#### Pull requests

| Method and path | Capability | Request | Success |
| --- | --- | --- | --- |
| `GET {repo}/pull-requests` | `pull_requests` | query `state`, `author`, `target_branch` | `200` paginated `[PullRequest]` |
| `GET {repo}/pull-requests/{number}` | `pull_requests` | | `200` PullRequest, `404` |
| `POST {repo}/pull-requests` | `pull_requests.create` | body below | `201` PullRequest, `409`, `422` |
| `POST {repo}/pull-requests/{number}/merge` | `pull_requests.merge` | body below | `204`, `409` (not mergeable), `422` (bad method) |
| `POST {repo}/pull-requests/{number}/close` | `pull_requests.close` | | `204` |

`state` is `open`, `closed`, `merged` or `all`. When it is absent, the server returns
open PRs.

```json
{"title": "Add rollout", "body": "...", "source_branch": "rollout",
 "target_branch": "main", "draft": false, "source_repo": "alice/deployer"}
```

```json
{"method": "squash", "commit_title": "Add rollout (#7)", "commit_message": "...",
 "delete_source_branch": true}
```

`method` is `merge`, `squash` or `rebase`. When it is omitted, the server uses its
default. Trove offers all three methods. A server that does not support the requested
method MUST reply `422` with `code: invalid` and a message saying which methods it
supports.

#### Issues

| Method and path | Capability | Request | Success |
| --- | --- | --- | --- |
| `GET {repo}/issues` | `issues` | query `state`, `labels`, `assignee`, `author` | `200` paginated `[Issue]` |
| `GET {repo}/issues/{number}` | `issues` | | `200` Issue, `404` |
| `POST {repo}/issues` | `issues.create` | `{"title", "body", "labels", "assignees"}` | `201` Issue, `422` |
| `PATCH {repo}/issues/{number}` | `issues.state` | `{"state": "closed"}` or `{"state": "open"}` | `200` Issue |

`state` is `open`, `closed` or `all`. When it is absent, the server returns open issues.
`labels=a,b` is a comma-separated list, and an issue matches only if it has **all** of
the listed labels. The `labels` filter and the `labels` field of the create body are
used only when `issues.labels` is declared.

#### Pipelines

| Method and path | Capability | Request | Success |
| --- | --- | --- | --- |
| `GET {repo}/pipelines` | `pipelines` | query `ref`, `status` | `200` paginated `[Pipeline]`, newest first |
| `GET {repo}/pipelines/{id}` | `pipelines` | | `200` Pipeline **including `jobs`**, `404` |

#### Releases

| Method and path | Capability | Success |
| --- | --- | --- |
| `GET {repo}/releases` | `releases` | `200` paginated `[Release]`, newest first |
| `GET {repo}/releases/{tag}` | `releases` | `200` Release, `404`. `{tag}` is path-escaped, so `v1.0/rc` becomes `v1.0%2Frc` |

#### Namespaces

| Method and path | Capability | Success |
| --- | --- | --- |
| `GET /v1/namespaces` | `namespaces` | `200` paginated `[Namespace]` that the user can see or create repositories in |
| `GET /v1/namespaces/{path}` | `namespaces` | `200` Namespace, `404`. `{path}` is escaped, so `acme/platform` becomes `acme%2Fplatform` |

#### `GET /v1/summary`: dashboard counts (`summary`)

`200` returns an AccountSummary. Keep this endpoint cheap: the dashboard calls it for every
account.

### Versioning

The major version is part of the path (`/v1/...`). Within v1, new optional request
parameters and response fields can be added. Adapters MUST ignore request fields they do
not know, and Trove ignores response fields it does not know. Breaking changes will use
`/v2`, and a v2 driver will keep speaking v1 for configured v1 adapters.

### Security checklist for adapters

- Serve over HTTPS and authenticate every endpoint, including `/v1/user`.
- Do not echo credentials in error messages, redirects or `Link` headers.
- Keep pagination links on the same origin as `api_base_url`. Trove refuses other
  origins.
- Enforce permissions on your side. Trove's capability list is a UI and safety contract,
  not an authorization boundary.

### Testing an adapter

1. Configure the provider, then run `trove auth login custom-company` and
   `trove auth status custom-company`.
2. List all repositories of the account and compare the count with your forge. A
   mismatch usually means a pagination setting is wrong, most often `max_page_size`
   being larger than the server's real maximum with `strategy: page`.
3. Open a repository in a nested namespace (`custom-company:group/sub/repo`) to check
   escaping of `{repo}`.
4. Turn on debug logging to see every request. The credential is always redacted.

You can also run Trove's provider contract suite against a staging adapter from a Go
test in a checkout of Trove:

```go
package adapter_test

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/forge/forgetest"
	"github.com/SurajMazar/trove-cli/providers/custom"
)

const cfg = `
type: custom
api_base_url: https://staging-forge.example.com/api
clone_base_url: https://staging-forge.example.com
custom:
  capabilities: [repositories, pull_requests, issues]
  pagination: {strategy: cursor}
`

func provider(t *testing.T, token string) forge.Provider {
	p, err := custom.NewDriver().New(forge.Account{
		Name: "staging", Type: "custom",
		Credentials: auth.TokenStore(token),
		Decode:      func(v any) error { return yaml.Unmarshal([]byte(cfg), v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdapterContract(t *testing.T) {
	token := os.Getenv("STAGING_FORGE_TOKEN")
	if token == "" {
		t.Skip("STAGING_FORGE_TOKEN not set")
	}
	forgetest.RunForgeProviderContractTests(t, provider(t, token), forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "acme/platform", Name: "deployer"},
		MissingRepo:     domain.RepositoryRef{Namespace: "acme", Name: "does-not-exist"},
		MinRepositories: 150, // more than one page
		Secret:          token,
		Unauthenticated: provider(t, "invalid-token"),
	})
}
```

`providers/custom/fake_test.go` in the Trove repository is a complete in-memory TFP
server covering every endpoint and all four pagination strategies. It is a useful
reference when you write an adapter.

---

## B. Adding a compiled-in forge driver

Choose a compiled-in driver when the forge has a public API that many Trove users will
want: Gitea, Forgejo, Azure DevOps, SourceHut and similar. Such a driver speaks the
forge's native API directly, with no adapter service.

1. Create `providers/<name>/` (for example `providers/gitea`) exporting
   `func NewDriver() forge.Driver`.
2. Implement `forge.Provider` plus **only** the capability interfaces the API really
   supports: `forge.RepositoryProvider`, `forge.PullRequestProvider`,
   `forge.PipelineLogger` and so on (see `internal/forge/forge.go`). Declare exactly
   those capabilities in `Capabilities()`. Do not emulate missing features. Trove turns
   an undeclared capability into a clear "does not support" error.
3. Implement `forge.Authenticator`. Add `forge.GitAuthenticator` for Git over HTTPS,
   `forge.Refresher` only if the forge has a real refresh flow, and `forge.Revoker` only
   if it has a revoke endpoint. `internal/auth.DeviceFlow` implements RFC 8628 for
   forges that support device authorization.
4. Map HTTP errors to the `internal/errs` kinds: 401 to `ErrAuthenticationFailed`, 403
   to `ErrPermissionDenied` (or `ErrRateLimited`), 404 to `ErrNotFound` or
   `ErrRepositoryNotFound`, 409 to `ErrConflict`, 400 and 422 to `ErrInvalidArgument`,
   and 429 to `ErrRateLimited` with `RetryAfter`. Never put tokens or raw response
   bodies into errors.
5. Build the HTTP client with `httpx.NewClient`, which gives retries, `Retry-After`
   handling and redacted debug logging. Normalize pagination through `forge.Collect`.
6. Register the driver where the built-in drivers are registered, for example
   `registry.MustRegister(gitea.NewDriver())`. Users then configure `type: gitea`.
7. Test against an `httptest.Server` fake and run
   `forgetest.RunForgeProviderContractTests`.

### Skeleton

```go
// Package gitea implements the Gitea forge driver.
package gitea

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

const Type = "gitea"

type driver struct{}

func NewDriver() forge.Driver { return driver{} }

func (driver) Type() string               { return Type }
func (driver) DisplayName() string        { return "Gitea" }
func (driver) Description() string        { return "Gitea and Forgejo (API v1)" }
func (driver) DefaultHost() string        { return "gitea.com" }
func (driver) AuthMethods() []auth.Method { return []auth.Method{auth.MethodToken} }

// New must not touch the network or load credentials.
func (driver) New(acct forge.Account) (forge.Provider, error) {
	if acct.Host == "" {
		acct.Host = "gitea.com"
	}
	api := acct.APIURL
	if api == "" {
		api = "https://" + acct.Host + "/api/v1"
	}
	logger := acct.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Provider{
		acct: acct, api: api, logger: logger,
		http: httpx.NewClient(httpx.Options{Base: acct.Transport, Logger: acct.Logger}),
	}, nil
}

type Provider struct {
	acct   forge.Account
	api    string
	logger *slog.Logger
	http   *http.Client

	mu    sync.Mutex
	token string // loaded lazily from acct.Credentials
}

func (p *Provider) Metadata() forge.Metadata {
	return forge.Metadata{
		Name: p.acct.Name, Type: Type, DisplayName: "Gitea", Host: p.acct.Host,
		APIURL: p.api, WebURL: "https://" + p.acct.Host, Deployment: "self-managed",
		AuthMethods: []auth.Method{auth.MethodToken},
		Terms: forge.Terms{Repository: "Repository", PullRequest: "Pull Request",
			PullRequestShort: "PR", Pipeline: "Action run", Snippet: "Snippet",
			Notification: "Notification", Namespace: "Organization"},
	}
}

// Declare exactly what is implemented below.
func (p *Provider) Capabilities() forge.Capabilities {
	return forge.NewCapabilities(forge.CapRepositories)
}

func (p *Provider) CurrentUser(ctx context.Context) (*domain.User, error) {
	// GET {api}/user with "Authorization: token <token>", mapped to domain.User.
	...
}

func (p *Provider) ListRepositories(ctx context.Context, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	return forge.Collect(ctx, opts.Limit, func(ctx context.Context, cursor string) ([]domain.Repository, string, error) {
		// Fetch one page with Gitea's own pagination (page/limit + Link
		// header). Return the items and the next page ("" when done).
		...
	})
}

func (p *Provider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	// 404 -> &errs.Error{Kind: errs.ErrRepositoryNotFound, Cause: errs.ErrNotFound, ...}
	...
}

func (p *Provider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	// Computed without network, honoring acct.CloneBaseURL / SSHHost / WebURL.
	...
}

func (p *Provider) Login(ctx context.Context, req auth.Request) (*auth.Result, error)  { ... }
func (p *Provider) AuthStatus(ctx context.Context) (*auth.Status, error)              { ... }

var (
	_ forge.Provider           = (*Provider)(nil)
	_ forge.Authenticator      = (*Provider)(nil)
	_ forge.RepositoryProvider = (*Provider)(nil)
)
```

The `...` bodies are where the forge-specific API calls go. Every method must be fully
implemented before it is declared.

### Contract test

```go
func TestContract(t *testing.T) {
	fake := newFakeGitea(t) // httptest.Server serving >1 page of repositories
	good := newProvider(t, fake.URL, "good-token")
	bad := newProvider(t, fake.URL, "bad-token") // fake answers 401

	forgetest.RunForgeProviderContractTests(t, good, forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "org", Name: "repo"},
		MissingRepo:     domain.RepositoryRef{Namespace: "org", Name: "missing"},
		MinRepositories: 120,
		Secret:          "good-token",
		Unauthenticated: bad,
	})
}

func newProvider(t *testing.T, apiURL, token string) forge.Provider {
	p, err := gitea.NewDriver().New(forge.Account{
		Name: "gitea-test", Type: "gitea", Host: "gitea.test",
		APIURL: apiURL, Credentials: auth.TokenStore(token),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
```

The suite checks the following:

- Metadata is complete.
- The declared capabilities are implemented, and sub-capabilities have their parents.
- Undeclared capabilities are refused.
- `CurrentUser` works and honors context cancellation.
- A bad credential yields `ErrAuthenticationFailed`, and no error leaks the token.
- Repository listing paginates without duplicates and honors `Limit`.
- A missing repository yields `ErrNotFound`.
- Repository models and URLs are well formed.

Add focused tests on top for request shapes, error mapping and authentication flows.
