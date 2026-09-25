// Command trove is a unified CLI for GitHub, GitLab, Bitbucket and other Git
// forges.
package main

import (
	"os"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/cli"
	providers "github.com/SurajMazar/trove-cli/providers/all"
	secretproviders "github.com/SurajMazar/trove-cli/secrets/all"
)

func main() {
	os.Exit(cli.Main(app.Deps{
		RegisterDrivers: providers.Register,
		RegisterSecrets: secretproviders.Register,
	}))
}
