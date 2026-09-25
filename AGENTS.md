# AGENTS.md

Guidance for AI coding agents (and humans) working on **Trove**, a Go CLI
(`trove`) that manages repositories, pull/merge requests, issues, pipelines
and more across GitHub, GitLab, Bitbucket Cloud and custom forges.
Module: `github.com/SurajMazar/trove-cli` · Go 1.25 · binary: `trove`.

## Commands

```sh
make build            # ./bin/trove with version metadata
make install          # go install into GOBIN / ~/go/bin
make test             # go test ./...
make test-race        # go test -race ./...
make fmt-check vet    # what CI enforces besides tests
go run ./cmd/trove --help
```

Before finishing any change: `gofmt -l .` is empty, `go vet ./...` passes,
`go test -race ./...` passes, and `go build ./...` succeeds. Run
`go mod tidy` only when imports changed. If the local toolchain is older than
a dependency requires, set `GOTOOLCHAIN=local` and pin compatible versions
rather than bumping the `go` directive.

## Layout

| Path | Role |
|------|------|
| `cmd/trove` | entry point; wires `providers/all` and `secrets/all` into `internal/cli` |
| `internal/cli` | Cobra commands; thin: resolve account → check capability → call → render |
| `internal/app` | application core: config + registry + secrets + account/credential resolution |
| `internal/forge` | forge provider contract, capability system, registry, `forge.Collect` pagination; `forgetest` contract suite |
| `internal/secrets` | secret provider contract, `scheme://key` references, resolver; `secrettest` contract suite |
| `internal/domain` | provider-neutral models (Repository, PullRequest, Namespace, Pipeline, ...) |
| `internal/auth` | Credential (self-redacting), Store, RFC 8628 device flow, refresh |
| `internal/git` | system-git wrapper (clone/fetch/commit/push/...), SSH key selection, remote detection, credential-helper protocol |
| `internal/tui` | Bubble Tea components (pickers, inputs, spinner, progress, dashboard, pause) |
| `internal/output`, `internal/terminal` | human/JSON/quiet output, tables, error rendering, theme, prompts |
| `internal/httpx`, `internal/redact`, `internal/logging` | retry/rate-limit transport, redaction, slog setup |
| `internal/services` | bulk clone worker pool, `doctor` checks |
| `internal/config` | YAML config: load/migrate/validate/save, dotted get/set |
| `providers/<name>` | one forge driver per directory; `providers/all` registers them |
| `secrets/<name>` | bitwarden, keychain, secretservice; `secrets/all` registers them |
| `docs/` | architecture, providers, authentication, secrets, custom providers |

## Architectural rules (do not break)

1. **Forge providers and secret providers are independent systems.**
   Bitwarden/Keychain/Secret Service are secret providers, never forge
   providers. Neither package tree imports the other.
2. **Each forge is its own driver.** Never assume GitHub behavior applies to
   GitLab or Bitbucket (auth, pagination, hierarchy, PR/MR semantics, CI,
   SaaS vs self-hosted). No provider-specific API code in `internal/forge`,
   `internal/app` or `internal/cli`.
3. **Capabilities are declared, not emulated.** A driver declares exactly
   what it implements (`Capabilities()`); commands obtain implementations via
   `forge.As[T](p, cap)`, which returns `errs.ErrUnsupportedCapability`
   ("Provider "x" does not support ..."). Never fake data or return empty
   results for unsupported features. `forge.ValidateCapabilities` must report
   no problems.
4. **Adding a forge = new `providers/<name>` package + one line in
   `providers/all/all.go`.** No changes to CLI, domain, config, secrets, TUI or
   git layers should be needed.
5. **No placeholders.** No `TODO`s, no stub implementations that pretend to
   work. Verify endpoints and fields against official API docs; never invent
   them.

## Security rules

- Secrets never go into the config file, logs, errors, JSON output, URLs or
  command-line arguments. Config stores `auth.secret_ref` only; `Save` and
  `config set` refuse secret-looking keys.
- `auth.Credential` redacts itself in `fmt`, JSON and slog; use `Encode()` only
  when writing to a secret provider.
- HTTP debug logs go through `internal/redact` (Authorization, cookies, tokens
  in query strings). Map API errors to `errs` kinds using provider `message`
  fields, never raw bodies that could echo credentials.
- Git authentication: HTTPS via `trove auth git-credential` (credential-helper
  protocol, host-checked); SSH via the account's `ssh_key` passed as
  `GIT_SSH_COMMAND` with `IdentitiesOnly=yes`. Don't embed tokens in remotes.
- Destructive operations (delete, force-push, merge, key removal) ask for
  confirmation via `confirm(a, ...)` and require `--yes` in scripts.
- Environment credentials (`TROVE_TOKEN`, `TROVE_TOKEN_<ALIAS>`) are never
  persisted.

## Conventions

- **Errors**: return `*errs.Error` with a sentinel `Kind`, provider alias,
  safe `Message` and an actionable `Hint` (e.g. `trove auth login <alias>`).
  Exit codes come from `errs.ExitCode`.
- **Context**: every network or git operation takes `ctx`; cancellation must
  surface as `context.Canceled`.
- **Pagination**: implement the provider's own mechanism inside a fetch func
  and normalize with `forge.Collect(ctx, limit, fetch)`; `Limit <= 0` = all.
- **Concurrency**: bounded worker pools only (see `services.Cloner`, 4-slot
  semaphores in multi-provider listing); never unbounded goroutines.
- **Output**: commands render through `a.Out.Result(v, quiet, human)`. JSON
  mode writes exactly one JSON document to stdout; progress, spinners,
  success lines and errors go to stderr.
- **Interactivity**: prompts and TUIs only when `a.IO.Interactive` (TTY, not
  `--non-interactive`, not JSON). Non-interactive paths fail with
  `errs.ErrInteractionRequired` and a hint showing the flag to use.
- **TUI keys**: `esc` goes back one screen (pickers opened from another screen
  set `EscBack`), `q`/`Ctrl+C` quit Trove (`tui.ErrQuit`); help lines must
  describe what keys do in the current state. TUIs render to stderr and must
  fit 80 columns; meaning never relies on color alone (NO_COLOR respected).
- **Terminology**: show the provider's own terms via `Metadata().Terms`
  (Merge Request, Gist, To-Do item, Workspace, Workflow run).
- Match the surrounding style; comment non-obvious provider behavior.

## Adding things

- **A forge provider**: implement `forge.Driver` + `forge.Provider` and only
  the capability interfaces the API supports; implement `forge.Authenticator`
  and `forge.GitAuthenticator`. Tests must run
  `forgetest.RunForgeProviderContractTests` against an `httptest.Server` fake
  (serve more than one page of repositories), plus focused tests for request
  shapes, error mapping (401/403/404/422/429), auth flows and cancellation.
  See `docs/custom-providers.md`.
- **A secret provider**: implement `secrets.SecretProvider` (+ `Checker`,
  `Describer`), register it in `secrets/all`, and run
  `secrettest.RunSecretProviderContractTests` with an injected fake backend.
- **A command**: add it under `internal/cli`, register it in `NewRoot` in the
  right group, support `--json`/`--quiet` where it returns data, and cover it
  in `internal/cli/*_test.go` using the mock driver harness (`newHarness`).
  Update `README.md` (and `docs/` when behavior or architecture changes).

## Testing rules

- Never call real forge APIs or real secret stores in tests; use
  `httptest.Server`, the mock driver in `internal/cli/cli_test.go`, and
  in-memory secret providers. Real Keychain/Secret Service suites are opt-in
  (`TROVE_KEYCHAIN_INTEGRATION=1`, `TROVE_SECRETSERVICE_INTEGRATION=1`).
- Keep tests hermetic: temp dirs for config/cache (`app.Deps.CacheDir`),
  `t.Setenv("HOME", ...)` for anything under `~`, `GIT_CONFIG_GLOBAL=/dev/null`
  for git, local bare repositories instead of network remotes.
- Assert that secrets never appear in outputs or error strings.

## Releases

Tag `vX.Y.Z` and push the tag; `.github/workflows/release.yml` runs
GoReleaser (darwin/linux × amd64/arm64, Homebrew cask in
`SurajMazar/homebrew-tap`, needs the `HOMEBREW_TAP_GITHUB_TOKEN` secret).
