# Authentication

Every Trove account has an auth method (`auth.type`) and a credential that
lives in a secret provider (see [secrets.md](secrets.md)) or, in CI, in an
environment variable. This page covers every method per provider, how to
create the credential, and how refresh, revocation and git authentication
work.

- [Overview](#overview)
- [GitHub](#github): PAT, OAuth device flow, GitHub App
- [GitLab](#gitlab): PAT, project and group tokens, OAuth device flow
- [Bitbucket Cloud](#bitbucket-cloud): API token, access token, OAuth consumer
- [Custom forges](#custom-forges)
- [Environment credentials for CI](#environment-credentials-for-ci)
- [Status, refresh and logout](#status-refresh-and-logout)
- [Git credential helper](#git-credential-helper)

---

## Overview

| Provider | `auth.type` values | Default |
| --- | --- | --- |
| GitHub (`github`) | `token`, `oauth`, `app` | `token` |
| GitLab (`gitlab`) | `token`, `oauth` | `token` |
| Bitbucket Cloud (`bitbucket`) | `basic`, `access-token`, `oauth` | `basic` |
| Custom (`custom`) | `token`, plus `basic` when `custom.auth.scheme: basic` | `token` |

Set the method when adding the account (`--auth-type`), or override it for one
login with `--method`. `trove auth login`:

1. collects the credential (masked prompt, stdin with `--with-token`, a PEM
   file, or a device flow),
2. validates it against the provider API,
3. stores it in the account's secret provider (never the config file), and
4. records non-secret details (`auth.type`, `username`, `client_id`,
   `secret_ref`) in the configuration.

Nothing is stored if validation fails.

```sh
trove auth login github-personal                          # interactive prompt
echo "$TOKEN" | trove auth login gitlab-work --with-token # scripted
trove auth login gh --method oauth                        # device flow
trove auth login gh-app --method app --private-key-file app.pem
```

`--with-token` reads up to 1 MiB from stdin: the token, the GitHub App private
key, or the Bitbucket OAuth consumer secret, depending on the method.

---

## GitHub

### Personal access token (`token`)

Create one under **Settings → Developer settings → Personal access tokens**
(on GHES: `https://<host>/settings/tokens`).

**Classic tokens.** Recommended scopes:

| Scope | Needed for |
| --- | --- |
| `repo` | Private repositories, pull requests, issues, releases, Actions runs, search |
| `read:org` | Organizations (`trove namespace`), organization repository listings |
| `workflow` | Dispatching and managing workflows |
| `gist` | Snippets (gists) |
| `notifications` | `trove notification` |
| `admin:public_key` | `trove key ssh add/remove` (`read:public_key` for listing only) |
| `read:gpg_key` | `trove key gpg list` |
| `user` | `trove settings set` (profile fields) |
| `delete_repo` | `trove repo delete` |

Trove reads the granted scopes from `X-OAuth-Scopes`; `trove auth status`
shows them, together with the token's expiry when it has one.

**Fine-grained tokens** work too. Grant the repositories you need and, for
example: Metadata (read), Contents (read/write), Pull requests (read/write),
Issues (read/write), Actions (read/write), Administration (read/write, for
create/rename/archive/delete), plus account permissions Gists, Git SSH keys,
GPG keys and Profile as needed. GitHub's notifications API does not accept
fine-grained tokens; use a classic token or OAuth for `trove notification`.

```sh
trove provider add github-personal --type github
trove auth login github-personal
```

### OAuth device flow (`oauth`)

The device flow needs a client ID of an app **you** register; Trove ships none.

1. Register an **OAuth App** (Settings → Developer settings → OAuth Apps → New)
   or a **GitHub App**. Any homepage and callback URL will do.
2. Tick **Enable Device Flow** in the app's settings.
3. Configure the account with the client ID:

```sh
trove provider add gh --type github --auth-type oauth --client-id Ov23liAbCdEf0123
trove auth login gh
```

```
! One-time code: ABCD-1234
  Open https://github.com/login/device and enter the code (expires in 15m)
  Waiting for authorization...
```

Trove requests `repo read:org gist notifications workflow admin:public_key`
unless `auth.scopes` or `--scopes` say otherwise (GitHub App user tokens use
the App's permissions instead of scopes). The code and URL are printed on
stderr, and Trove polls until you approve the request in a browser (on any
device).

- **OAuth App tokens** do not expire.
- **GitHub App user tokens** expire (8 hours by default) and come with a refresh
  token. Trove refreshes them automatically when they expire, and on demand
  with `trove auth refresh`; this needs the client ID, which Trove keeps with
  the credential and in `auth.client_id`.

Without a client ID, `--method oauth` accepts a **pre-issued OAuth token**
instead (pasted or piped); such tokens cannot be refreshed. With a client ID,
`--with-token` also accepts a pre-issued token instead of running the flow.

The device flow waits for someone to approve it in a browser, so Trove refuses
to start it under `--non-interactive`; pipe a token with `--with-token`
instead.

Errors map to actionable hints: an unknown client ID, "Device Flow is not
enabled for this OAuth App", an expired code, or access denied in the browser.

### GitHub App installation (`app`)

Use this for automation that should act as a bot, scoped to the repositories an
App is installed on.

1. Create a GitHub App (Settings → Developer settings → GitHub Apps → New, or
   in an organization's settings). Grant the repository permissions you need
   (Metadata, Contents, Pull requests, Issues, Actions, Administration, ...).
2. Note the **App ID** on the App's settings page.
3. **Install** the App on the account or organization and select repositories.
   The **installation ID** is the number at the end of the installation's
   settings URL (`.../settings/installations/<installation-id>`).
4. **Generate a private key** (a `.pem` file) on the App's settings page.

```sh
trove provider add gh-app --type github --auth-type app --app-id 123456 --installation-id 7890123
trove auth login gh-app --private-key-file ./my-app.private-key.pem
# or: cat key.pem | trove auth login gh-app --with-token
```

Login mints an installation token to prove the App ID, key and installation
belong together, then stores the PEM key. At run time Trove signs a
short-lived RS256 JWT, exchanges it for an installation token, and caches that
token in memory until a minute before it expires (one hour); installation
tokens are never persisted.

App accounts act as `<app-slug>[bot]`. Installation tokens cannot call user
endpoints, so **notifications, gists, SSH/GPG keys and profile settings are
unavailable**, and repositories come from the installation. The PEM key is
about 1.7 KB, which fits in every secret provider (see the Keychain size limit
in [secrets.md](secrets.md#macos-keychain)).

### Revocation

`trove auth logout --revoke` is not supported for GitHub: revoking an OAuth
token server-side requires the OAuth App's client secret. Revoke PATs and
authorizations in GitHub's settings, then run `trove auth logout`.

---

## GitLab

### Personal access token (`token`)

Create one at **User settings → Access tokens**
(`https://<host>/-/user_settings/personal_access_tokens`). Scope **`api`**
covers every Trove feature, including Git over HTTPS. `read_api` is enough for
read-only use (listing, viewing, search, logs), but not for clones over HTTPS
(add `read_repository`) or any write.

```sh
trove provider add gitlab-work --type gitlab --host gitlab.company.com
trove auth login gitlab-work
```

On GitLab 15.5+ `trove auth status` shows the token's scopes and expiry
(`GET /personal_access_tokens/self`).

### Project and group access tokens (`token`)

Created under a project's or group's **Settings → Access tokens** with a role
(Developer or Maintainer) and scope `api`. They are used exactly like a PAT
(`auth.type: token`) and authenticate as a bot user that only sees that project
or group, which makes them a good fit for CI.

### OAuth device authorization grant (`oauth`)

Requires **GitLab 17.2+** (generally available in 17.9) and an OAuth
application registered on the instance:

1. Open **User settings → Applications**
   (`https://<host>/-/user_settings/applications`), or the admin area for an
   instance-wide application.
2. Create an application: **untick "Confidential"**, select scopes **`api`**
   and **`read_user`**, and enable the **device authorization grant**. The
   redirect URI is not used by the device flow; any valid URL works.
3. Copy the **Application ID**:

```sh
trove provider add gl --type gitlab --host gitlab.company.com --auth-type oauth --client-id 3f1e5c...
trove auth login gl
```

Trove requests `api read_user` unless `auth.scopes`/`--scopes` override it.
Access tokens expire after two hours; Trove refreshes them automatically when
they expire or are rejected, saves the new pair, and `trove auth refresh` forces
a refresh. Without a client ID, `--method oauth` accepts a pre-issued OAuth
token, which cannot be refreshed.

### Revocation

`trove auth logout --revoke` revokes on the server, then deletes the local
credential:

- OAuth tokens: `POST /oauth/revoke` (the refresh token is revoked with it).
- Access tokens: `DELETE /personal_access_tokens/self` (GitLab 15.5+).

---

## Bitbucket Cloud

Bitbucket **app passwords are not supported**: Atlassian stopped issuing them
on 9 September 2025 and disabled the remaining ones in 2026. Use one of the
methods below.

### Atlassian API token (`basic`, default)

HTTP Basic authentication with your **Atlassian account email** as the
username and an API token as the password.

1. Open <https://id.atlassian.com/manage-profile/security/api-tokens> and
   choose **Create API token with scopes**, app **Bitbucket**.
2. Select the scopes for what you will use, for example:
   `read:user:bitbucket`, `read:workspace:bitbucket`,
   `read:repository:bitbucket`, `write:repository:bitbucket`,
   `admin:repository:bitbucket` (create), `delete:repository:bitbucket`,
   `read:pullrequest:bitbucket`, `write:pullrequest:bitbucket`,
   `read:pipeline:bitbucket`, `write:pipeline:bitbucket`,
   `read:snippet:bitbucket`, `write:snippet:bitbucket`,
   `read:ssh-key:bitbucket`, `write:ssh-key:bitbucket`,
   `read:gpg-key:bitbucket`.

```sh
trove provider add bb --type bitbucket --auth-type basic --username me@example.com --set workspace=acme
trove auth login bb
```

API tokens offer no scope introspection, so `auth status` shows the account
email instead of scopes. Git over HTTPS uses the username
`x-bitbucket-api-token-auth`.

### Access token (`access-token`)

Repository, project or workspace **access tokens** (Repository/Project/Workspace
settings → Security → Access tokens) are sent as Bearer tokens. They do not
belong to a user, so the account **must** set `workspace`, which Trove uses to
validate the token; user-scoped features (your SSH keys, for example) are not
available with them.

```sh
trove provider add bb-ci --type bitbucket --auth-type access-token --set workspace=acme
echo "$BB_ACCESS_TOKEN" | trove auth login bb-ci --with-token
```

Git over HTTPS uses the username `x-token-auth`.

### OAuth consumer (`oauth`)

Trove uses the **client credentials grant** of an OAuth consumer:

1. **Workspace settings → OAuth consumers → Add consumer.** Tick **This is a
   private consumer** (required for the client credentials grant) and enter a
   callback URL (any valid URL).
2. Grant the permissions you need (Account read, Workspace membership read,
   Repositories, Pull requests, Pipelines, Snippets, ...).
3. Use the **Key** as the client ID and the **Secret** at login:

```sh
trove provider add bb-oauth --type bitbucket --auth-type oauth --client-id AbCdEf123 --set workspace=acme
trove auth login bb-oauth                               # prompts for the consumer secret
echo "$BB_CONSUMER_SECRET" | trove auth login bb-oauth --with-token
```

Without a consumer key (leave the prompt empty, or omit `auth.client_id` and
use `--with-token`), Trove accepts a **pre-issued OAuth access token** instead;
such tokens cannot be refreshed.

The consumer secret is stored with the token (in the secret provider) so Trove
can re-issue tokens: expired tokens are refreshed with the refresh token, or
re-issued with a new client credentials grant. `trove auth refresh` forces it.

There is no server-side revocation for Bitbucket (`--revoke` reports
unsupported); revoke tokens and consumers in Bitbucket's settings.

---

## Custom forges

Custom providers authenticate with a token sent in a configurable header and
scheme (`Authorization: Bearer <token>` by default, a raw token, any
`<Word> <token>`, or HTTP Basic with `scheme: basic`). `trove auth login`
validates the token with `GET /v1/user`. The credential header is only sent to
the configured API origin. See
[custom-providers.md](custom-providers.md#authentication).

---

## Environment credentials for CI

Environment variables take precedence over secret providers and are **never
persisted**:

| Variable | Applies to |
| --- | --- |
| `TROVE_TOKEN_<ALIAS>` | The account `<alias>`: upper-cased, non-alphanumerics replaced by `_` (`gitlab-work` → `TROVE_TOKEN_GITLAB_WORK`) |
| `TROVE_TOKEN` | The account the command operates on: from `--provider`, a repository prefix, `TROVE_PROVIDER`, `default_provider`, or the only configured account |

How the value is interpreted depends on the account's `auth.type`:

- `token`, `access-token`: the token.
- `basic` (Bitbucket API token): the token; the email comes from
  `auth.username`.
- `oauth`: an OAuth access token (not refreshable).
- `app` (GitHub): the App's PEM private key.

```sh
export TROVE_PROVIDER=gitlab-work
export TROVE_TOKEN="$GITLAB_TOKEN"
trove --non-interactive pipeline list --ref main --json
```

Alternatively point an account at a specific variable with
`secret_ref: env://MY_VAR` (read-only; `trove auth login` refuses to write it).
`trove auth status` reports the source as `env:<VARIABLE>`.

In non-interactive sessions (`--non-interactive`, `--json`, `--quiet`, `CI`
set, or no terminal), login never prompts; it fails with exit code 2 and a
hint such as `echo "$TOKEN" | trove auth login gitlab-work --with-token`.

---

## Status, refresh and logout

```sh
trove auth status [alias]      # all accounts (in parallel), or one
trove auth refresh [alias]     # force an OAuth refresh
trove auth logout [alias]      # delete the stored credential
trove auth logout [alias] --revoke   # revoke on the server first (GitLab)
```

`auth status` shows, per account: the user, method, credential source (secret
provider or `env:<VAR>`), scopes where the provider reports them, expiry
(highlighted within seven days) and details such as the GitHub App
installation. With an alias it exits with code 3 if the account is not
authenticated, which makes it a convenient CI guard.

| | Refresh | Server-side revoke |
| --- | --- | --- |
| GitHub | GitHub App user tokens (OAuth with refresh token) | not supported |
| GitLab | OAuth tokens | OAuth tokens and access tokens |
| Bitbucket Cloud | OAuth consumer tokens | not supported |
| Custom | not supported | not supported |

Refreshed credentials are saved back to the account's secret provider.
Environment credentials are refreshed in memory only. `trove auth logout` warns
when `TROVE_TOKEN`/`TROVE_TOKEN_<ALIAS>` is set, since that credential keeps
working until you unset it. `trove provider remove <alias>` also deletes the
stored credential unless `--keep-credential` is given.

---

## Git credential helper

`trove auth git-credential` implements git's
[credential helper protocol](https://git-scm.com/docs/gitcredentials). For the
clones and fetches Trove runs itself (`repo clone`, `repo create --clone`,
`repo fork --clone`, `pr checkout`), it installs itself as the only helper for
that one git invocation, so HTTPS remotes authenticate **without the token
ever appearing in a URL, in command-line arguments, in `.git/config` or in the
process list**.

To use the same credentials for your own `git pull`/`push`, register the helper
per host in your global git config:

```sh
git config --global credential.https://github.com.helper \
  '!trove auth git-credential --provider github-personal'
git config --global credential.https://gitlab.company.com.helper \
  '!trove auth git-credential --provider gitlab-work'
git config --global credential.https://bitbucket.org.helper \
  '!trove auth git-credential --provider bb'
```

Resulting `~/.gitconfig`:

```ini
[credential "https://github.com"]
	helper = "!trove auth git-credential --provider github-personal"
[credential "https://gitlab.company.com"]
	helper = "!trove auth git-credential --provider gitlab-work"
```

Notes:

- `trove` must be on the `PATH` git sees; otherwise use the absolute path. Add
  `--config /path/to/config.yaml` if you use a non-default configuration.
- Without `--provider`, the helper picks the account whose host matches the
  request (the default provider wins when several match).
- The helper only answers for HTTPS requests to hosts that belong to the
  account; for anything else it prints nothing, so git falls through to your
  other helpers. `store` and `erase` are ignored because Trove owns credential
  storage.
- Usernames follow each provider's convention: `x-access-token` (GitHub),
  `oauth2` (GitLab), `x-bitbucket-api-token-auth` (Bitbucket API tokens),
  `x-token-auth` (Bitbucket access and OAuth tokens), `custom.git_username` or
  `trove` (custom).
- Expired OAuth tokens are refreshed before they are handed to git.
