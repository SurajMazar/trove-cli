// Package all registers Trove's built-in forge drivers. Adding a new forge
// (Gitea, Forgejo, Azure DevOps, SourceHut, ...) means implementing a
// forge.Driver in providers/<name> and adding one line here; the CLI,
// domain model, secret management, configuration, TUI and git layers do not
// change.
package all

import (
	"github.com/SurajMazar/trove-cli/internal/forge"
	bitbucketcloud "github.com/SurajMazar/trove-cli/providers/bitbucket/cloud"
	"github.com/SurajMazar/trove-cli/providers/custom"
	"github.com/SurajMazar/trove-cli/providers/github"
	"github.com/SurajMazar/trove-cli/providers/gitlab"
)

// Register adds every built-in driver to r.
func Register(r *forge.Registry) {
	r.MustRegister(github.NewDriver())
	r.MustRegister(gitlab.NewDriver())
	r.MustRegister(bitbucketcloud.NewDriver())
	r.MustRegister(custom.NewDriver())
}
