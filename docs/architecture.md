# Architecture

Trove is a layered Go program. Each layer depends only on the layers below it,
and provider-specific behavior lives in exactly one place: the driver package
for that provider.

```
                 ┌──────────────────────────────────────────────────────┐
  user  ───────▶ │ cmd/trove        main: wires built-in drivers/secrets │
                 ├──────────────────────────────────────────────────────┤
                 │ internal/cli     Cobra commands (thin)               │
                 │ internal/tui     Bubble Tea pickers, dashboard, ...  │
                 │ internal/output  human tables, JSON, quiet, errors   │
                 ├──────────────────────────────────────────────────────┤
                 │ internal/app       app core: config, account         │
                 │                    resolution, credential stores     │
                 │ internal/services  bulk clone, doctor                │
                 ├──────────────────────────┬───────────────────────────┤
                 │ internal/forge           │ internal/secrets          │
                 │ forge contract:          │ secret contract:          │
                 │ Driver, Provider,        │ SecretProvider, Resolver, │
                 │ capability interfaces,   │ SecretReference           │
                 │ Registry, As, Collect    │                           │
                 ├──────────────────────────┼───────────────────────────┤
                 │ providers/github         │ secrets/bitwarden         │
                 │ providers/gitlab         │ secrets/keychain          │
                 │ providers/bitbucket/cloud│ secrets/secretservice     │
                 │ providers/custom         │ (env is built in)         │
                 └──────────────────────────┴───────────────────────────┘
   shared plumbing: internal/httpx · internal/redact · internal/logging ·
   internal/git · internal/auth · internal/domain · internal/errs ·
   internal/config · internal/cache · internal/terminal · internal/version
```

## Layers

**CLI (`internal/cli`).** Commands parse flags, ask the app core for a
provider, request a capability, call it and hand the result to `output`.
They contain no HTTP and no provider knowledge. Interactive pieces (pickers,
prompts, spinners, the dashboard) come from `internal/tui` and are only used
when the session is interactive.

**App core (`internal/app`).** `app.Build` loads configuration, applies
environment overrides (`TROVE_CONFIG`, `TROVE_PROVIDER`, `TROVE_OUTPUT`,
`TROVE_LOG_LEVEL`), builds the driver registry and the secret resolver, and
creates the output printer, logger, cache and git wrapper. At run time it
answers three questions for every command:

1. *Which account?* `ProviderName`: explicit flag or repository prefix, then
   `TROVE_PROVIDER`, then `default_provider`, then the only configured account.
2. *Which repository?* `ResolveRepo`: parse `[alias:]namespace/name`, or detect
   it from the current directory's git remotes (`origin` first).
3. *Which credential?* `credentialStore`: `TROVE_TOKEN_<ALIAS>`, then
   `TROVE_TOKEN` for the selected account, then the account's `secret_ref`.

It then builds a `forge.Account` and asks the account's driver for a
`forge.Provider`. The app core contains no provider-specific API behavior.

**Contracts (`internal/forge`, `internal/secrets`).** Interfaces and a few
provider-neutral helpers. Forge providers and secret providers are completely
independent: any forge account can use any secret provider.

**Drivers and secret providers.** One package per implementation. Drivers own
their API client: base URLs, authentication headers, pagination, error
mapping, terminology and quirks.

## Package map

| Package | Responsibility |
| --- | --- |
| `cmd/trove` | `main`: registers `providers/all` and `secrets/all`, runs `cli.Main` |
| `internal/cli` | Cobra command tree, flag parsing, rendering calls |
| `internal/tui` | List/multi-select picker, text/secret input, spinner, clone progress, dashboard |
| `internal/output` | Human output (tables, key/values), JSON, quiet mode, error rendering |
| `internal/terminal` | TTY detection, width, color policy (`NO_COLOR`, `--color`), theme, confirmations |
| `internal/app` | Composition root, account/repository/credential resolution |
| `internal/services` | Multi-step workflows: bulk clone worker pool, `doctor` checks |
| `internal/forge` | Forge contract, capability system, registry, `As`, `Collect`, `CloneURL` |
| `internal/forge/forgetest` | Contract test suite every driver runs |
| `internal/domain` | Provider-neutral models (`Repository`, `PullRequest`, `Pipeline`, ...) |
| `internal/auth` | Auth methods, `Credential` (self-redacting), stored format, RFC 8628 device flow, OAuth refresh |
| `internal/secrets` | Secret contract, `Resolver`, references, built-in `env` provider |
| `internal/secrets/secrettest` | Contract test suite every secret provider runs |
| `internal/config` | YAML schema, defaults, validation, migrations, dotted get/set, atomic 0600 save |
| `internal/errs` | Typed error kinds, structured `*errs.Error`, exit codes |
| `internal/httpx` | Retry transport, redacting debug transport, Link header parsing, JSON helpers |
| `internal/redact` | Redaction of headers, URLs and free text |
| `internal/logging` | `log/slog` setup with mandatory redaction |
| `internal/git` | System `git` wrapper (clone, fetch, commit, push, ...), SSH key selection, remote URL parsing and detection, credential helper protocol |
| `internal/cache` | Small on-disk TTL cache for non-sensitive listings |
| `internal/version` | Build metadata set through `-ldflags` |
| `providers/github` | GitHub.com, GHE Cloud (`*.ghe.com`), GitHub Enterprise Server |
| `providers/gitlab` | GitLab.com and Self-Managed/Dedicated |
| `providers/bitbucket` | Parent package; `cloud` is Bitbucket Cloud, `server` is reserved for Server/Data Center |
| `providers/custom` | Trove Forge Protocol v1 driver (declarative) |
| `providers/all`, `secrets/all` | Registration of the built-in implementations |
| `secrets/bitwarden`, `secrets/keychain`, `secrets/secretservice` | Secret providers |

## The forge contract and capabilities

A `forge.Driver` has a type (`github`, `gitlab`, `bitbucket`, `custom`), a
display name, a default host, the auth methods it supports and a constructor
`New(forge.Account) (forge.Provider, error)`. Constructors never touch the
network; credentials are loaded lazily on the first request.

A `forge.Provider` is deliberately tiny:

```go
type Provider interface {
    Metadata() Metadata          // alias, type, host, URLs, deployment, terminology
    Capabilities() Capabilities  // the features this account supports
    CurrentUser(ctx) (*domain.User, error)
}
```

Everything else is an optional, narrow interface: `RepositoryProvider`,
`RepositoryCreator`, `PullRequestMerger`, `PipelineLogger`, `SnippetCreator`,
`SettingsProvider` and so on, each paired with a `Capability` constant such as
`repositories.create` or `pipelines.logs`. Auth-related interfaces
(`Authenticator`, `Refresher`, `Revoker`, `GitAuthenticator`) are discovered by
type assertion.

A provider implements **only what its API genuinely supports** and declares it
in `Capabilities()`. Capabilities can depend on the account: a GitHub App
account withholds notifications, gists, keys and settings; a custom provider
declares exactly what its configuration lists.

Commands never type-assert providers directly. They call `forge.As`:

```go
merger, err := forge.As[forge.PullRequestMerger](p, forge.CapPRMerge)
if err != nil {
    return err // errs.ErrUnsupportedCapability: provider "bb" does not support ...
}
```

`As` checks both that the capability is declared and that the interface is
implemented. A missing capability becomes a typed `ErrUnsupportedCapability`
(exit code 5) with the canonical message `provider "x" does not support
<feature>`; it never panics and never emulates missing functionality.

`forge.ValidateCapabilities` (used by the contract suite and `trove doctor`)
verifies that every declared capability has its interface and that
sub-capabilities have their parent (`pull_requests.merge` requires
`pull_requests`).

Provider-specific terminology (`Terms`: "Merge Request"/"MR", "Workflow run",
"To-Do item", "Workspace", ...) travels in `Metadata`, so output uses each
forge's own words without the CLI knowing about them.

## Pagination

Forges paginate differently: GitHub uses `Link` headers, GitLab `Link` plus
`X-Next-Page`, Bitbucket an envelope with an absolute `next` URL, custom
providers one of `link`, `page`, `cursor` or `none`. `forge.Collect`
normalizes all of them:

```go
func Collect[T any](ctx context.Context, limit int,
    fetch func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error)
```

Each driver supplies `fetch`, which retrieves one page with its own mechanism
and returns the items plus an opaque cursor for the next page (`""` when
done). `Collect` stops at `limit` (`<= 0` means all), honors cancellation and
stops on repeated cursors, so a misbehaving server cannot cause an infinite
loop. `forge.PageSize(limit, max)` picks the per-page size so small limits do
not over-fetch. Drivers only follow pagination URLs that point at their
configured API origin, so credentials are never sent elsewhere.

## Error model

All errors that cross a package boundary are, or wrap, a sentinel kind from
`internal/errs` (`ErrNotAuthenticated`, `ErrRepositoryNotFound`,
`ErrRateLimited`, `ErrUnsupportedCapability`, ...). The structured
`*errs.Error` adds the provider alias, operation, message, actionable hint,
HTTP status and `RetryAfter`. `Unwrap` exposes both kind and cause, so
`errors.Is` works against either.

- `errs.ExitCode` maps kinds to exit codes (0, 1, 2, 3, 4, 5, 6, 130).
- `output.RenderError` prints a title, detail, provider and hint in human mode,
  or one JSON object on stderr in JSON mode (`code`, `exit_code`, `message`,
  `provider`, `hint`).
- Drivers map HTTP statuses themselves (for example, 403 is a rate limit on
  GitHub only when the rate-limit headers say so, and always a permission
  error on GitLab) and surface only the provider's error message, never raw
  bodies.
- Error text must never contain secrets; every message is also passed through
  `redact.String` before display.

## HTTP plumbing

`httpx.NewClient` builds each driver's client from two transports:

1. **RetryTransport** retries idempotent requests (GET, HEAD, OPTIONS, or
   requests explicitly marked with `WithRetrySafe`) on network errors, HTTP 429,
   502, 503 and 504, and on provider-specific rate-limit signals supplied by the
   driver (`RetryPolicy.RateLimited`, used for GitHub's `403` +
   `x-ratelimit-remaining: 0`). It uses exponential backoff with jitter
   (default 3 retries, 500 ms base, 20 s cap) and honors `Retry-After`. Waits
   longer than `MaxWait` (60 s) are not slept; the driver reports
   `ErrRateLimited` with `RetryAfter` instead. POST/PATCH/DELETE are never
   retried implicitly.
2. **LoggingTransport** (only when logging is enabled) logs method, redacted
   URL, redacted headers, status and duration at debug level.

Credentials are attached below the logging layer by each driver, so they never
reach the log even under `--debug`. Drivers install a redirect policy that
drops `Authorization` whenever a redirect leaves the original origin (scheme,
host and port); GitHub and Bitbucket log downloads redirect to pre-signed
storage URLs that must not receive the token. Every request carries
`User-Agent: trove/<version>`.

## Secret resolution

The secret layer is independent of forges:

```go
type SecretProvider interface {
    Name() string
    Get(ctx, key) (string, error)
    Set(ctx, key, value string) error
    Delete(ctx, key) error          // idempotent
    Exists(ctx, key) (bool, error)
}
```

A `SecretReference` is `scheme://key`; the `Resolver` dispatches on the scheme.
Providers are registered as **lazy factories**, so a command that never needs a
credential never starts `bw` or opens D-Bus. The `env` provider is built in and
read-only.

For an account, the app core builds an `auth.Store` (Load/Save/Delete):

- **Environment credentials** (`TROVE_TOKEN_<ALIAS>`, `TROVE_TOKEN`) become a
  read-only in-memory store: they are used as-is and never persisted, even
  after an OAuth refresh.
- Otherwise a **secret store** resolves `auth.secret_ref` (or the default
  `trove/<type>/<alias>/token` key in `secrets.provider`).

Drivers receive only the `auth.Store`. They never know where a credential is
stored, and they persist refreshed OAuth tokens through the same interface.
Credentials are encoded by `auth.Credential.Encode`: plain tokens are stored
verbatim (so users can paste a token into their secret manager by hand) and
richer credentials as a small versioned JSON document. See
[secrets.md](secrets.md).

## Git layer and credential helper

`internal/git` wraps the system `git` binary; Trove never reimplements the Git
protocol. It provides clone, fetch, checkout, status/changes, add, commit,
push, remotes, branch and upstream queries, plus
remote URL parsing and **detection**: configured accounts (matched on host,
SSH host and the hosts of API/web/clone URLs) win over well-known SaaS hosts,
which win over hostname heuristics.

SSH authentication uses the account's `ssh_key` (chosen during `trove provider
add`, or `--ssh-key`/`--choose-key`). For remote operations Trove sets, for
that invocation only, `GIT_SSH_COMMAND="ssh -i '<key>' -o IdentitiesOnly=yes"`,
which overrides `core.sshCommand` and stops ssh from trying other agent keys.
With no key configured, git's own SSH configuration applies unchanged.
`trove push` identifies the account from the remote's fetch URL and picks
SSH-key or credential-helper authentication from its push URL.

HTTPS authentication uses git's **credential helper protocol**. For every
clone, fetch or push it runs, Trove adds, for that invocation only:

```
git -c credential.helper= \
    -c "credential.helper=!'/path/to/trove' '--config' '<file>' '--provider' '<alias>' 'auth' 'git-credential'" \
    clone <https-url> <dir>
```

The empty helper first clears inherited helpers; then git calls `trove auth
git-credential get`, which reads the request from stdin, checks that the host
belongs to the account, and answers with the driver's
`GitAuthenticator.GitCredentials()` (`x-access-token` for GitHub, `oauth2` for
GitLab, `x-bitbucket-api-token-auth` or `x-token-auth` for Bitbucket). Tokens
therefore never appear in URLs, `argv`, `.git/config` or the process list.
`store` and `erase` are no-ops because Trove owns credential storage. Git runs
with `GIT_TERMINAL_PROMPT=0` and `GCM_INTERACTIVE=never`, so it never prompts,
and its stderr is redacted before it is shown.

## TUI components

`internal/tui` holds reusable Bubble Tea components:

- **List picker**: single or multi select, `/` search with `field:value`
  terms matched against item facets (`author:`, `label:`, `branch:`), facet
  cycling keys (namespace, provider, author), paging, HTTPS/SSH toggle,
  cursor restore (`InitialID`) and context-aware help.
- **Input**: text and masked secret prompts with validation.
- **Choose**: small option menus used by `provider add`.
- **Spinner**: wraps long calls (`tui.SpinValue`).
- **Clone progress**: live per-repository status; aggregates when there are
  many jobs.
- **Reader**: full-screen scrollable view (bubbles viewport, alternate
  screen) for an issue or pull request: header fields plus a lightly
  formatted, word-wrapped description.
- **Pause**: "enter/esc back · q quit" prompt after a dashboard view.
- **Dashboard**: account summary and a menu; hides entries the provider
  lacks, and loops back after each view.

Navigation is uniform: `esc` goes back one screen (pickers opened from another
screen set `EscBack`), while `q`/`Ctrl+C` quit Trove; a quit is reported as an
`errs.ErrAborted` error wrapping `tui.ErrQuit` so callers can tell the two
apart.

Components render to stderr so stdout stays clean for pipes, never call
providers themselves (callers pass data or loader functions), and are only
used when the session is interactive: stdin and stdout are terminals, `CI` is
unset, and neither `--non-interactive`, `--json` nor `--quiet` was given.

## Caching

`internal/cache` is a small on-disk TTL cache in `$XDG_CACHE_HOME/trove` (or
the OS cache directory). It stores only non-sensitive listings (repository
lists and namespaces) keyed by a hash of account alias, host and parameters.
The TTL is `cache.ttl` (default 2 minutes); `cache.enabled: false` or
`--no-cache` disables it, and every mutating command invalidates it. It must
never hold credentials.

## Concurrency

Fan-out is always bounded:

- **Bulk clone** (`services.Cloner`): a worker pool of `clone.concurrency`
  workers (default 4, maximum 32). A failure never stops unrelated jobs; each
  job reports queued/cloning/cloned/skipped/failed events to the progress view;
  cancellation stops the feed, kills running `git` processes and removes
  partial directories.
- **Multi-account operations** (`repo list --all-providers`, `auth status`,
  `doctor`) query accounts in parallel with a semaphore of 4; one failing
  account is reported without hiding the others.
- Providers are safe for concurrent use: credential loading, OAuth refresh and
  GitHub App installation tokens are guarded by mutexes. The Bitwarden `bw`
  backend serializes its CLI invocations because `bw` does not tolerate
  concurrent writers.

## Adding a provider

Adding a forge (Gitea, Forgejo, Azure DevOps, Bitbucket Server, ...) touches
exactly two places:

1. A new package, for example `providers/gitea`, implementing `forge.Driver`
   and the capability interfaces its API supports, with a fake-server test that
   runs `forgetest.RunForgeProviderContractTests`.
2. One line in `providers/all/all.go`:

   ```go
   r.MustRegister(gitea.NewDriver())
   ```

The CLI, domain model, configuration schema, secret management, TUI, output
and git layers do not change: commands discover the new provider's features
through its capabilities, configuration validation learns its type and auth
methods from the registry, and provider-specific settings are read by the
driver through `Account.Decode`. The step-by-step guide, including a driver
skeleton, is in [custom-providers.md](custom-providers.md#b-adding-a-compiled-in-forge-driver).
Forges you cannot or do not want to compile in can use the declarative
`custom` driver instead ([custom-providers.md](custom-providers.md)).
