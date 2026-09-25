// Package bitwarden implements Trove's first-class secret provider on top of
// Bitwarden, using Bitwarden's official command line tools. Trove never talks
// to the Bitwarden API directly and never stores Bitwarden credentials.
//
// References look like
//
//	bitwarden://trove/github/personal/token
//
// where everything after the scheme is the secret key.
//
// # Backends
//
// Two backends are available, selected with Options.Backend:
//
// "cli" (default) uses the Bitwarden Password Manager CLI, bw
// (https://bitwarden.com/help/cli/). Every key is stored as one Login item
// whose name is the key and whose login password is the value; the item notes
// are set to "Managed by trove". Items are placed in a folder (default
// "trove", created on first write) and, optionally, in an organization
// collection. The vault session is taken from the BW_SESSION environment
// variable, which bw reads itself; Trove only inherits it and never persists
// it. Unlock the vault before running Trove:
//
//	bw login                                  # once
//	export BW_SESSION="$(bw unlock --raw)"    # per shell session
//
// Values never appear on the bw command line: items are sent to
// `bw create item` / `bw edit item` as base64-encoded JSON on standard input
// (the same encoding `bw encode` produces). `bw delete item` moves items to the
// Bitwarden trash; permanent deletion (emptying the trash) is intentionally
// left to the user. All bw invocations pass --nointeraction so bw never
// prompts, and they are serialized because bw does not tolerate concurrent
// processes writing its local data file.
//
// "secrets-manager" uses the Bitwarden Secrets Manager CLI, bws
// (https://bitwarden.com/help/secrets-manager-cli/), authenticated with a
// machine account access token from the BWS_ACCESS_TOKEN environment variable
// (inherited, never stored). A key maps to a secret with the same key; new
// secrets are created in Options.ProjectID. Limitation: bws only accepts a
// secret value as a command-line argument (`bws secret create <KEY> <VALUE>
// <PROJECT_ID>` and `bws secret edit <ID> --value <VALUE>`), so while Trove
// writes a secret the value is briefly visible to other users of the machine
// through the process list. Reads do not have this problem. Prefer the "cli"
// backend on shared machines.
//
// # Configuration
//
//	secrets:
//	  provider: bitwarden        # default provider used when trove stores new credentials
//	  bitwarden:
//	    backend: cli             # or secrets-manager
//	    folder: trove
//	    organization_id: ""
//	    collection_id: ""
//	    project_id: ""           # secrets-manager only
//
// # Usage
//
//	p, err := bitwarden.New(bitwarden.Options{Folder: "trove"})
//	if err != nil { ... }
//	if err := p.Check(ctx); err != nil { ... } // e.g. errs.ErrSecretProviderLocked with an unlock hint
//	err = p.Set(ctx, "trove/github/personal/token", token)
//	token, err = p.Get(ctx, "trove/github/personal/token")
//
// # Errors
//
// Missing keys yield errs.ErrSecretNotFound from Get (Delete of a missing key
// succeeds). A missing CLI yields errs.ErrSecretUnavailable; a logged-out or
// locked vault, or a missing/rejected access token, yields
// errs.ErrSecretProviderLocked with the exact command to fix it in the hint.
// Errors never contain secret values and never contain what was written to
// the CLI's standard input.
package bitwarden
