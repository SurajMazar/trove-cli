// Package cloud implements the Bitbucket Cloud forge driver (driver type
// "bitbucket") on top of the Bitbucket Cloud REST API 2.0.
//
// Bitbucket's model is Workspace -> (Project) -> Repository. A Trove
// repository's Namespace is the workspace slug and its Name the repository
// slug (not the display name).
//
// Notable platform facts this driver is built around (verified against the
// published OpenAPI description and the Bitbucket Cloud changelog, 2026):
//
//   - The cross-workspace listing endpoints (GET /2.0/repositories,
//     GET /2.0/workspaces, GET /2.0/user/permissions/workspaces,
//     GET /2.0/snippets) were removed on 14 April 2026 (CHANGE-2770).
//     Workspaces are discovered with GET /2.0/user/workspaces and every
//     listing is then made per workspace.
//   - The native issue tracker and its API were removed on 20 August 2026,
//     so this driver declares no issue capabilities.
//   - Git over SSH moves from bitbucket.org to ssh.bitbucket.org (the old
//     host stops accepting SSH in November 2026), so computed SSH clone URLs
//     use ssh.bitbucket.org.
//   - Bitbucket has no repository archive flag, no releases, no
//     notifications API, no pipeline re-run endpoint and no fetchable pull
//     request refs; the corresponding capabilities are not declared.
//
// Provider-specific account configuration (read through Account.Decode):
//
//	workspace: my-workspace   # default workspace; required for access tokens
package cloud

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// DriverType is the registry key of this driver.
const DriverType = "bitbucket"

const (
	defaultHost    = "bitbucket.org"
	defaultAPIURL  = "https://api.bitbucket.org/2.0"
	defaultWebURL  = "https://bitbucket.org"
	defaultSSHHost = "ssh.bitbucket.org"
	// tokenPath is the OAuth 2.0 token endpoint, relative to the web URL.
	tokenPath = "/site/oauth2/access_token"
)

// accountConfig holds Bitbucket-specific keys of an account's configuration.
type accountConfig struct {
	// Workspace is the default workspace slug. It scopes code search and
	// repository/snippet creation when no namespace is given, and it is the
	// only way to validate and use workspace/project/repository access
	// tokens, which are not tied to a user.
	Workspace string `yaml:"workspace"`
}

type driver struct{}

// NewDriver returns the Bitbucket Cloud driver.
func NewDriver() forge.Driver { return driver{} }

func (driver) Type() string        { return DriverType }
func (driver) DisplayName() string { return "Bitbucket Cloud" }
func (driver) Description() string {
	return "Bitbucket Cloud (bitbucket.org): workspaces, repositories, pull requests, Pipelines and snippets"
}
func (driver) DefaultHost() string        { return defaultHost }
func (driver) AuthMethods() []auth.Method { return authMethods() }

func authMethods() []auth.Method {
	return []auth.Method{auth.MethodBasic, auth.MethodAccessToken, auth.MethodOAuth}
}

// New builds a provider bound to acct. It does not touch the network or load
// credentials.
func (driver) New(acct forge.Account) (forge.Provider, error) {
	var cfg accountConfig
	if acct.Decode != nil {
		if err := acct.Decode(&cfg); err != nil {
			return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
				Message: "invalid Bitbucket account configuration", Cause: err}
		}
	}
	apiURL := strings.TrimRight(firstNonEmpty(acct.APIURL, defaultAPIURL), "/")
	webURL := strings.TrimRight(firstNonEmpty(acct.WebURL, defaultWebURL), "/")
	api, err := url.Parse(apiURL)
	if err != nil || api.Scheme == "" || api.Host == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
			Message: fmt.Sprintf("invalid Bitbucket API URL %q", apiURL)}
	}
	if _, err := url.Parse(webURL); err != nil {
		return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
			Message: fmt.Sprintf("invalid Bitbucket web URL %q", webURL)}
	}
	logger := acct.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := &Provider{
		acct:      acct,
		api:       api,
		apiURL:    apiURL,
		webURL:    webURL,
		host:      firstNonEmpty(acct.Host, defaultHost),
		tokenURL:  webURL + tokenPath,
		workspace: strings.TrimSpace(cfg.Workspace),
		logger:    logger,
	}
	// Bitbucket Cloud signals rate limiting only with HTTP 429, which the
	// shared retry transport already understands, so no extra detector is
	// needed.
	p.hc = httpx.NewClient(httpx.Options{Base: acct.Transport, Logger: acct.Logger, Retry: httpx.RetryPolicy{}})
	p.hc.CheckRedirect = p.checkRedirect
	return p, nil
}

// Provider is a Bitbucket Cloud provider bound to one account.
type Provider struct {
	acct      forge.Account
	api       *url.URL
	apiURL    string
	webURL    string
	host      string
	tokenURL  string
	workspace string
	logger    *slog.Logger
	hc        *http.Client

	mu       sync.Mutex
	cred     *auth.Credential
	userUUID string
}

var (
	_ forge.Provider            = (*Provider)(nil)
	_ forge.Authenticator       = (*Provider)(nil)
	_ forge.Refresher           = (*Provider)(nil)
	_ forge.GitAuthenticator    = (*Provider)(nil)
	_ forge.RepositoryProvider  = (*Provider)(nil)
	_ forge.RepositoryCreator   = (*Provider)(nil)
	_ forge.RepositoryDeleter   = (*Provider)(nil)
	_ forge.RepositoryForker    = (*Provider)(nil)
	_ forge.RepositoryRenamer   = (*Provider)(nil)
	_ forge.PullRequestProvider = (*Provider)(nil)
	_ forge.PullRequestCreator  = (*Provider)(nil)
	_ forge.PullRequestMerger   = (*Provider)(nil)
	_ forge.PullRequestCloser   = (*Provider)(nil)
	_ forge.PipelineProvider    = (*Provider)(nil)
	_ forge.PipelineRunner      = (*Provider)(nil)
	_ forge.PipelineCanceler    = (*Provider)(nil)
	_ forge.PipelineLogger      = (*Provider)(nil)
	_ forge.NamespaceProvider   = (*Provider)(nil)
	_ forge.Searcher            = (*Provider)(nil)
	_ forge.SnippetProvider     = (*Provider)(nil)
	_ forge.SnippetCreator      = (*Provider)(nil)
	_ forge.SSHKeyProvider      = (*Provider)(nil)
	_ forge.GPGKeyProvider      = (*Provider)(nil)
	_ forge.SummaryProvider     = (*Provider)(nil)
)

// Metadata implements forge.Provider.
func (p *Provider) Metadata() forge.Metadata {
	return forge.Metadata{
		Name:        p.acct.Name,
		Type:        DriverType,
		DisplayName: "Bitbucket Cloud",
		Host:        p.host,
		APIURL:      p.apiURL,
		WebURL:      p.webURL,
		Deployment:  "cloud",
		AuthMethods: authMethods(),
		Terms: forge.Terms{
			Repository:       "Repository",
			PullRequest:      "Pull Request",
			PullRequestShort: "PR",
			Pipeline:         "Pipeline",
			Snippet:          "Snippet",
			Notification:     "Notification",
			Namespace:        "Workspace",
		},
	}
}

// Capabilities implements forge.Provider. Only what the Bitbucket Cloud API
// genuinely supports is declared (see the package documentation).
func (p *Provider) Capabilities() forge.Capabilities {
	return forge.NewCapabilities(
		forge.CapRepositories, forge.CapRepoCreate, forge.CapRepoDelete, forge.CapRepoFork, forge.CapRepoRename,
		forge.CapPullRequests, forge.CapPRCreate, forge.CapPRMerge, forge.CapPRClose,
		forge.CapPipelines, forge.CapPipelineRun, forge.CapPipelineCancel, forge.CapPipelineLogs,
		forge.CapNamespaces, forge.CapSearchRepos, forge.CapSearchCode,
		forge.CapSnippets, forge.CapSnippetCreate,
		forge.CapSSHKeys, forge.CapGPGKeys,
		forge.CapSummary,
	)
}

// RepositoryURLs computes URLs without an API call, honoring the account's
// CloneBaseURL, SSHHost and WebURL overrides.
func (p *Provider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	full := ref.Namespace + "/" + ref.Name
	return domain.RepositoryURLs{
		Web:   p.webURL + "/" + full,
		HTTPS: p.httpsCloneBase() + "/" + full + ".git",
		SSH:   p.sshPrefix() + full + ".git",
		API:   p.apiURL + "/repositories/" + pathEscape(ref.Namespace) + "/" + pathEscape(ref.Name),
	}
}

func (p *Provider) httpsCloneBase() string {
	return strings.TrimRight(firstNonEmpty(p.acct.CloneBaseURL, p.webURL), "/")
}

func (p *Provider) sshPrefix() string {
	h := firstNonEmpty(p.acct.SSHHost, defaultSSHHost)
	if strings.Contains(h, "@") {
		return h + ":"
	}
	return "git@" + h + ":"
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
