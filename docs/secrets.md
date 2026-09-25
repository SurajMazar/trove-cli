# Secrets

Trove never stores credentials in its configuration file. Each account points
at its credential with a **secret reference**, and a **secret provider**
(Bitwarden, macOS Keychain, Linux Secret Service or environment variables)
holds the value. Forge providers and secret providers are independent: any
account can use any secret provider.

- [Secret references](#secret-references)
- [Choosing a provider](#choosing-a-provider)
- [Bitwarden](#bitwarden)
- [macOS Keychain](#macos-keychain)
- [Linux Secret Service](#linux-secret-service)
- [Environment variables](#environment-variables)
- [Stored credential format](#stored-credential-format)
- [Security guarantees](#security-guarantees)
- [Moving credentials](#moving-credentials)

---

## Secret references

A reference is `scheme://key`:

```
bitwarden://trove/github/github-personal/token
keychain://trove/gitlab/gitlab-work/token
secretservice://trove/bitbucket/bb/token
env://TROVE_TOKEN_CI
```

The scheme selects the secret provider; everything after `://` is the key
(leading and trailing `/` are trimmed, percent-escapes are decoded). Scheme
names are lower-case letters, digits, `-` and `_`.

It is stored per account as `auth.secret_ref`:

```yaml
providers:
  github-personal:
    type: github
    host: github.com
    auth:
      type: token
      secret_ref: keychain://trove/github/github-personal/token
```

### Default key layout

When an account has no explicit `secret_ref`, Trove uses

```
<secrets.provider>://trove/<type>/<alias>/token
```

for example `keychain://trove/gitlab/gitlab-work/token`. For the `env`
provider the default key is the variable `TROVE_TOKEN_<ALIAS>` (alias
upper-cased, non-alphanumerics replaced by `_`). `trove provider add` writes
the resolved reference into the configuration, so changing
`secrets.provider` later does not move existing accounts.

`trove provider list` (in a wide terminal) and `trove provider show` display
each account's reference. The reference is not secret; the value is.

---

## Choosing a provider

| Provider | Scheme | Platforms | Default on | Writable |
| --- | --- | --- | --- | --- |
| macOS Keychain | `keychain` | macOS | macOS | yes |
| Secret Service | `secretservice` | Linux | Linux | yes |
| Bitwarden | `bitwarden` | any (needs `bw` or `bws`) | other platforms | yes |
| Environment | `env` | any | never | no (read-only) |

```sh
trove config set secrets.provider bitwarden                     # default for new accounts
trove provider add work --type gitlab --host gitlab.company.com --secret-provider bitwarden
trove provider add ci --type github --secret-ref env://GH_TOKEN # explicit reference
trove doctor                                                    # checks every provider in use
```

Secret providers are constructed lazily: a command that never needs a
credential never starts `bw`/`bws`, never touches the Keychain and never opens
D-Bus.

---

## Bitwarden

Trove uses Bitwarden's official CLIs; it never talks to the Bitwarden API
directly and never stores Bitwarden credentials.

```yaml
secrets:
  provider: bitwarden
  bitwarden:
    backend: cli              # cli (Password Manager, bw) | secrets-manager (bws)
    cli_path: ""              # default: bw or bws on PATH
    folder: trove             # cli backend: folder for Trove's items (default "trove")
    no_folder: false          # cli backend: true = keep items outside any folder
    organization_id: ""       # cli backend: store items in an organization...
    collection_id: ""         # ...collection (both or neither)
    project_id: ""            # secrets-manager backend: project for new secrets
    sync_on_start: false      # cli backend: run `bw sync` before the first operation
```

### Password Manager (`backend: cli`, default)

Install the [Bitwarden CLI](https://bitwarden.com/help/cli/) (`bw`), log in once
and unlock per shell session:

```sh
bw login
export BW_SESSION="$(bw unlock --raw)"
trove auth login github-personal
```

- **Session.** `bw` reads `BW_SESSION` from the environment it inherits from
  Trove. Trove never persists the session key and never passes it on the
  command line. A locked or logged-out vault is reported as
  `secret provider locked` (exit code 3) with the exact command to fix it,
  for example `Bitwarden vault is locked (BW_SESSION is not set)` →
  `export BW_SESSION="$(bw unlock --raw)"`.
- **Items.** Each key is one **Login item** whose *name* is the key (for
  example `trove/github/github-personal/token`) and whose *password* is the
  value; the notes say `Managed by trove`. You can create or edit these items
  in any Bitwarden client; pasting a plain token into the password field works.
- **Folders.** Items live in the folder `folder` (default `trove`), which is
  created on first write. Lookups only match items in that folder. Set
  `no_folder: true` to store and look up items outside any folder instead. With
  `organization_id` and `collection_id`, new items are created in that
  organization collection (Bitwarden requires organization items to belong to
  a collection, so both must be set together).
- **Duplicates.** If several Login items with the same name exist in the scope,
  Trove refuses to guess and reports a conflict listing their IDs.
- **No values on the command line.** Items are sent to `bw create item` /
  `bw edit item` as base64-encoded JSON on stdin, the same encoding `bw encode`
  produces.
- **Deletion.** `bw delete item` moves the item to the Bitwarden trash. Trove
  never empties the trash; permanent deletion is left to you.
- **Behavior.** Every `bw` call uses `--nointeraction`, so `bw` never prompts.
  Calls are serialized, because `bw` does not tolerate concurrent processes
  writing its local data file. `sync_on_start: true` runs `bw sync` once per
  Trove process before the first operation, useful when another device
  changed the vault.

### Secrets Manager (`backend: secrets-manager`)

For machines and CI, use the
[Secrets Manager CLI](https://bitwarden.com/help/secrets-manager-cli/) (`bws`)
with a machine account access token:

```yaml
secrets:
  provider: bitwarden
  bitwarden:
    backend: secrets-manager
    project_id: 0f9e...        # required to create secrets; also scopes lookups
```

```sh
export BWS_ACCESS_TOKEN=...    # inherited by bws, never stored by Trove
trove auth login ci-account
```

A key maps to a secret with the same key. Lookups are scoped to `project_id`
when it is set; duplicate keys are reported as a conflict.

> **Caveat: values in `argv` during writes.** `bws` only accepts a secret
> value as a command-line argument (`bws secret create <KEY> <VALUE>
> <PROJECT_ID>`, `bws secret edit <ID> --value <VALUE>`). While Trove *writes*
> a secret (login, OAuth refresh), the value is briefly visible to other users
> of the machine through the process list. Reads are not affected. On shared
> machines prefer the `cli` backend, or populate secrets with the Bitwarden web
> app and let Trove only read them.

---

## macOS Keychain

```yaml
secrets:
  provider: keychain
  keychain:
    service: trove            # item "service" (default "trove")
```

- Each key is a **generic password** in your default (login) keychain with
  service `trove` and account = the key. Inspect items with Keychain Access or:

  ```sh
  security find-generic-password -s trove -a trove/github/github-personal/token
  ```

- Trove drives `/usr/bin/security` (through
  [go-keyring](https://github.com/zalando/go-keyring)). Writes send the command,
  value included, on the tool's **stdin** (`security -i`), so values never
  appear in the process list.
- Values are stored base64-encoded with a `go-keyring-base64:` prefix, so
  multi-line values (PEM keys) and JSON round-trip exactly. Items written by
  other tools are read as-is, with surrounding whitespace trimmed.
- **Size limit.** `security` limits one command to 4096 bytes, so values
  larger than roughly **3000 bytes** (service, account and encoded value
  combined) are rejected with an `invalid argument` error. Tokens, OAuth
  documents and GitHub App keys (about 1.7 KB) fit; for anything larger use
  Bitwarden.
- **Locked keychain.** macOS may show an access prompt the first time Trove
  reads an item. Over SSH, where no prompt can be shown, a locked keychain is
  reported as `secret provider locked` with the hint
  `security unlock-keychain`.
- Keychain calls cannot be interrupted. Trove stops waiting when you press
  `Ctrl-C` (for example while an access prompt is open), but a call that has
  already started may complete in the background.
- On non-macOS systems the provider reports `secret provider unavailable`.

---

## Linux Secret Service

```yaml
secrets:
  provider: secretservice
  secretservice:
    service: trove            # "service" attribute (default "trove")
```

Uses the freedesktop.org Secret Service API (`org.freedesktop.secrets` on the
D-Bus session bus), provided by GNOME Keyring, KDE Wallet (5.97+) and
KeePassXC with Secret Service integration.

- Each key is an item in the **default collection** (the `login` keyring when
  present) with attributes `service=trove` and `username=<key>`, the layout
  go-keyring uses. Inspect items with:

  ```sh
  secret-tool search service trove username trove/gitlab/gitlab-work/token
  ```

- Values travel over D-Bus, never through `argv` or temporary files, and
  round-trip byte for byte (PEM keys and JSON are fine).
- **Requirements**: a session bus (`DBUS_SESSION_BUS_ADDRESS` or
  `/run/user/<uid>/bus`) and a running Secret Service implementation. Trove does
  not autolaunch a bus. Typical failures map to actionable errors:
  - no session bus, or nothing provides `org.freedesktop.secrets`:
    `secret provider unavailable` ("install and start gnome-keyring or KeePassXC
    with Secret Service integration");
  - a locked collection whose unlock prompt was dismissed or could not be
    shown: `secret provider locked`.
- Over SSH or in containers there is often no session bus. Start one (for
  example with `dbus-run-session` and `gnome-keyring-daemon --unlock`), or use
  Bitwarden or environment credentials instead.
- `trove doctor` only checks that the service is reachable; it never triggers
  an unlock prompt, so a locked keyring shows up on the first real operation.
- D-Bus calls (in particular an unlock prompt) cannot be canceled; Trove stops
  waiting on `Ctrl-C`, but the abandoned call may complete in the background.
- On non-Linux systems the provider reports `secret provider unavailable`.

---

## Environment variables

The built-in `env` provider reads a variable named by the key:

```yaml
auth:
  secret_ref: env://GITLAB_TOKEN
```

It is **read-only**: `trove auth login` refuses to write it (with a hint to
`export` the variable) and `trove auth logout` does not touch it.

Independently of `secret_ref`, `TROVE_TOKEN_<ALIAS>` and `TROVE_TOKEN` (for the
account a command operates on) override any secret provider and are never
persisted. See [authentication.md](authentication.md#environment-credentials-for-ci).

---

## Stored credential format

`auth.Credential.Encode` chooses the simplest representation:

- **A plain token** (personal access tokens, project/group tokens, Bitbucket
  access tokens) is stored **verbatim**. You can therefore populate a secret
  store by hand: paste the token into a Bitwarden item's password, or
  `security add-generic-password -s trove -a trove/github/github-personal/token -w`.
- **Anything richer** is stored as a small versioned JSON document:

  ```json
  {
    "v": 1,
    "kind": "oauth",
    "token": "<access token>",
    "refresh_token": "<refresh token>",
    "token_type": "bearer",
    "username": "<client id or account email>",
    "client_secret": "<only when a refresh needs it>",
    "expiry": "2026-09-25T12:00:00Z",
    "scopes": ["api", "read_user"]
  }
  ```

  `kind` is `token`, `oauth`, `basic` (Bitbucket API token + email) or `app`
  (GitHub App private key in `token`). Only non-empty fields are written.

When reading, a value that is not a Trove JSON document (`v >= 1`) is treated
as a plain token, so hand-entered values always work. GitHub App installation
tokens are minted on demand and kept in memory only; they are never stored.

---

## Security guarantees

- **Never in the configuration.** The config file holds only references.
  `trove config set` and `provider add --set` refuse keys that look like
  secrets (`token`, `password`, `secret`, `private_key`, `client_secret`,
  `api_key`, `passphrase`, `access_token`, `refresh_token`, ...), and
  `config.Save` refuses to write a file containing them. The file is written
  atomically with mode `0600` in a `0700` directory.
- **Never printed or logged.** `auth.Credential` redacts itself when formatted,
  logged (`slog.LogValuer`) or marshalled to JSON: it always renders as
  `[REDACTED]`. JSON output never contains credential values.
- **Redaction everywhere.** Debug logs, error messages and git's stderr pass
  through `internal/redact`, which masks sensitive headers (`Authorization`,
  `PRIVATE-TOKEN`, cookies, any header containing `token`, `secret` or
  `password`), credential query parameters, URL user info and token-shaped
  strings. Credentials are attached to requests below the logging layer.
- **Never in URLs or argv.** HTTPS git operations use the credential helper
  protocol; `bw` receives values on stdin; the Keychain receives commands on
  stdin; Secret Service receives values over D-Bus. The only exception is the
  documented `bws` write caveat above.
- **Never sent to the wrong host.** Credentials are dropped on cross-origin
  redirects, pagination links to other hosts are not followed, and the git
  credential helper only answers for hosts that belong to the account.
- **Never persisted from the environment.** `TROVE_TOKEN*` credentials live in
  memory only, even when refreshed.
- **Caches hold no secrets.** The on-disk cache stores only repository and
  namespace listings.

---

## Moving credentials

To move an account's credential to another provider, point it at the new
location and log in again (or copy the value by hand):

```sh
trove config set providers.github-personal.auth.secret_ref bitwarden://trove/github/github-personal/token
trove auth login github-personal
```

The old item is left where it was; delete it with your secret manager's own
tools, or run `trove auth logout` *before* changing the reference.
