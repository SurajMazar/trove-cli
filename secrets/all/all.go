// Package all registers Trove's built-in secret providers with a resolver.
// Providers are constructed lazily on first use, so commands that never
// touch credentials never start the Bitwarden CLI or open D-Bus.
package all

import (
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/secrets/bitwarden"
	"github.com/SurajMazar/trove-cli/secrets/keychain"
	"github.com/SurajMazar/trove-cli/secrets/secretservice"
)

// Register adds bitwarden, keychain and secretservice to r.
func Register(r *secrets.Resolver, cfg config.SecretsConfig) {
	r.RegisterFactory("bitwarden", func() (secrets.SecretProvider, error) {
		bw := cfg.Bitwarden
		return bitwarden.New(bitwarden.Options{
			Backend:        bw.Backend,
			CLIPath:        bw.CLIPath,
			Folder:         bw.Folder,
			NoFolder:       bw.NoFolder,
			OrganizationID: bw.OrganizationID,
			CollectionID:   bw.CollectionID,
			ProjectID:      bw.ProjectID,
			SyncOnStart:    bw.SyncOnStart,
		})
	})
	r.RegisterFactory("keychain", func() (secrets.SecretProvider, error) {
		return keychain.New(keychain.Options{Service: cfg.Keychain.Service})
	})
	r.RegisterFactory("secretservice", func() (secrets.SecretProvider, error) {
		return secretservice.New(secretservice.Options{Service: cfg.SecretService.Service})
	})
}
