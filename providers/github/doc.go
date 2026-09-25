// Package github implements the Trove forge driver for GitHub.com, GitHub
// Enterprise Cloud with data residency (*.ghe.com) and GitHub Enterprise
// Server (GHES). Only the REST API (version 2022-11-28) is used.
//
// # Deployments
//
//   - github.com: API https://api.github.com, web https://github.com.
//   - SUBDOMAIN.ghe.com: API https://api.SUBDOMAIN.ghe.com, same API surface
//     as github.com.
//   - Any other host is treated as GHES: API https://HOST/api/v3, web
//     https://HOST. GHES lags github.com: older releases may lack fields such
//     as "visibility" (the driver falls back to "private"), answer workflow
//     dispatches with 204 instead of returning the new run ID, and support
//     "internal" repositories that github.com personal accounts cannot use.
//
// Account overrides (api_url, web_url, clone_base_url, ssh_host) always take
// precedence over derived URLs, including clone URLs returned by the API.
//
// # Pull requests versus issues
//
// GitHub models every pull request as an issue. The issues endpoints return
// pull requests too; they are recognizable by a "pull_request" key and are
// filtered out of issue listings, and GetIssue reports a pull request number
// as not found with a hint. Pull requests have no "merged" state: GitHub
// reports them as "closed" with merged_at set, so the merged/closed split is
// done client-side (a "closed" filter excludes merged pull requests). Pull
// request heads can be fetched as refs/pull/N/head. GitHub has no
// "delete source branch on merge" option, so the branch ref is deleted after
// merging, only when it lives in the base repository.
//
// # GitHub Actions
//
// Pipelines are workflow runs. A run has a lifecycle "status" (queued,
// in_progress, completed, ...) and, once completed, a "conclusion"
// (success, failure, cancelled, ...); both are normalized into
// domain.PipelineStatus. Running a pipeline dispatches a workflow_dispatch
// event, which requires the workflow file name or ID and a workflow that
// declares the workflow_dispatch trigger. Job logs are served through a 302
// redirect to short-lived pre-signed storage URLs; the driver follows the
// redirect but never forwards the Authorization header to a different
// origin (Go's own redirect policy only compares host names, not ports).
//
// # Authentication
//
//   - token: classic or fine-grained personal access tokens. Classic tokens
//     report scopes in X-OAuth-Scopes; expiring tokens report their expiry in
//     GitHub-Authentication-Token-Expiration.
//   - oauth: the OAuth device flow against WEB/login/device/code with a
//     user-supplied OAuth App (or GitHub App) client ID, or a pre-issued
//     OAuth token. GitHub App user tokens expire and come with a refresh
//     token; they are refreshed automatically when expired and on demand via
//     RefreshCredential. OAuth App tokens have no refresh token.
//   - app: GitHub App installation auth. The stored secret is the App's PEM
//     private key; the driver signs a short-lived RS256 JWT and exchanges it
//     for an installation token, cached in memory until a minute before it
//     expires and never persisted. Installation tokens cannot call user
//     endpoints, so for App accounts the current user is the App's bot
//     (GET /app), repositories come from GET /installation/repositories and
//     notifications, gists, keys and profile settings are not offered.
//
// Server-side revocation is not implemented: it requires the OAuth App's
// client secret (DELETE /applications/{client_id}/token), which Trove does
// not hold.
//
// # Rate limits
//
// GitHub signals rate limits with 429, or 403 plus either Retry-After
// (secondary limits) or x-ratelimit-remaining: 0 (primary limit, reset at
// x-ratelimit-reset). Short waits are retried for idempotent requests; longer
// ones surface as errs.ErrRateLimited with RetryAfter set.
package github
