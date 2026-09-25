package github

import (
	"context"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// DriverType is the provider type string used in configuration.
const DriverType = "github"

const (
	cloudHost   = "github.com"
	cloudAPIURL = "https://api.github.com"
	cloudWebURL = "https://github.com"

	// apiVersion pins the REST API version (supported by github.com and
	// GHES 3.9+; older GHES releases ignore the header).
	apiVersion = "2022-11-28"
	mediaType  = "application/vnd.github+json"

	// maxPerPage is the largest page size most GitHub list endpoints accept.
	maxPerPage = 100
	// maxNotificationsPerPage is the notifications API page size limit.
	maxNotificationsPerPage = 50
)

// Deployment kinds reported in Metadata.
const (
	deploymentCloud      = "cloud"
	deploymentEnterprise = "enterprise-server"
)

type driver struct{}

// NewDriver returns the GitHub / GitHub Enterprise Server driver.
func NewDriver() forge.Driver { return driver{} }

func (driver) Type() string        { return DriverType }
func (driver) DisplayName() string { return "GitHub" }
func (driver) Description() string {
	return "GitHub.com, GitHub Enterprise Cloud (ghe.com) and GitHub Enterprise Server"
}
func (driver) DefaultHost() string { return cloudHost }
func (driver) AuthMethods() []auth.Method {
	return []auth.Method{auth.MethodToken, auth.MethodOAuth, auth.MethodApp}
}

// Provider is a GitHub provider bound to one configured account.
type Provider struct {
	acct   forge.Account
	meta   forge.Metadata
	log    *slog.Logger
	api    *url.URL // API base, e.g. https://api.github.com or https://ghe/api/v3
	web    string   // web base without trailing slash
	clone  string   // HTTPS clone base without trailing slash
	ssh    string   // SSH host (optionally host:port)
	hc     *http.Client
	stream *http.Client // long timeout, for log downloads
	now    func() time.Time

	mu    sync.Mutex
	cred  *auth.Credential
	login string // cached login of the authenticated user

	appMu      sync.Mutex
	appKey     *rsa.PrivateKey
	appKeyPEM  string
	instToken  string
	instExpiry time.Time
}

// New implements forge.Driver. It never touches the network.
func (driver) New(acct forge.Account) (forge.Provider, error) {
	return newProvider(acct)
}

func newProvider(acct forge.Account) (*Provider, error) {
	host := normalizeHost(acct.Host)
	if host == "" {
		host = cloudHost
	}
	kind := hostKind(host)

	apiURL := strings.TrimRight(acct.APIURL, "/")
	webURL := strings.TrimRight(acct.WebURL, "/")
	switch kind {
	case deploymentCloud:
		if apiURL == "" {
			apiURL = cloudAPIURL
			if host != cloudHost {
				// GitHub Enterprise Cloud with data residency (SUBDOMAIN.ghe.com)
				// serves its API from api.SUBDOMAIN.ghe.com.
				apiURL = "https://api." + host
			}
		}
		if webURL == "" {
			webURL = cloudWebURL
			if host != cloudHost {
				webURL = "https://" + host
			}
		}
	default:
		// GitHub Enterprise Server serves REST under /api/v3 on the same host.
		if apiURL == "" {
			apiURL = "https://" + host + "/api/v3"
		}
		if webURL == "" {
			webURL = "https://" + host
		}
	}
	api, err := url.Parse(apiURL)
	if err != nil || api.Scheme == "" || api.Host == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
			Message: fmt.Sprintf("invalid api_url %q", acct.APIURL)}
	}
	if w, err := url.Parse(webURL); err != nil || w.Scheme == "" || w.Host == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
			Message: fmt.Sprintf("invalid web_url %q", acct.WebURL)}
	}
	cloneBase := strings.TrimRight(acct.CloneBaseURL, "/")
	if cloneBase == "" {
		cloneBase = webURL
	} else if c, err := url.Parse(cloneBase); err != nil || c.Scheme == "" || c.Host == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: acct.Name,
			Message: fmt.Sprintf("invalid clone_base_url %q", acct.CloneBaseURL)}
	}
	sshHost := acct.SSHHost
	if sshHost == "" {
		w, _ := url.Parse(webURL)
		sshHost = w.Hostname()
	}

	logger := acct.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	retry := httpx.RetryPolicy{RateLimited: rateLimited}
	hc := httpx.NewClient(httpx.Options{Base: acct.Transport, Logger: logger, Retry: retry})
	hc.CheckRedirect = checkRedirect
	stream := httpx.NewClient(httpx.Options{Base: acct.Transport, Logger: logger, Retry: retry, Timeout: 30 * time.Minute})
	stream.CheckRedirect = checkRedirect

	display := "GitHub Enterprise Server"
	if kind == deploymentCloud {
		display = "GitHub"
		if host != cloudHost {
			display = "GitHub Enterprise Cloud"
		}
	}
	p := &Provider{
		acct: acct, log: logger, api: api, web: webURL, clone: cloneBase, ssh: sshHost,
		hc: hc, stream: stream, now: time.Now,
	}
	p.meta = forge.Metadata{
		Name:        acct.Name,
		Type:        DriverType,
		DisplayName: display,
		Host:        host,
		APIURL:      api.String(),
		WebURL:      webURL,
		Deployment:  kind,
		AuthMethods: driver{}.AuthMethods(),
		Terms: forge.Terms{
			Repository:       "Repository",
			PullRequest:      "Pull Request",
			PullRequestShort: "PR",
			Pipeline:         "Workflow run",
			Snippet:          "Gist",
			Notification:     "Notification",
			Namespace:        "Organization",
		},
	}
	return p, nil
}

func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimRight(h, "/")
	switch h {
	case "www.github.com", "api.github.com":
		return cloudHost
	}
	return h
}

// hostKind distinguishes github.com (and ghe.com data-residency tenants,
// which share github.com's API surface) from GitHub Enterprise Server.
func hostKind(host string) string {
	if host == cloudHost || strings.HasSuffix(host, ".ghe.com") {
		return deploymentCloud
	}
	return deploymentEnterprise
}

// checkRedirect follows redirects but drops credentials whenever the target
// origin (scheme + host + port) differs from the original request. Go's own
// policy only compares host names, so it would forward the token to another
// port on the same host; GitHub redirects log and archive downloads to
// pre-signed blob storage URLs that must never receive the token.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		req.Header.Del("Authorization")
		req.Header.Del("X-GitHub-Api-Version")
	}
	return nil
}

// Metadata implements forge.Provider.
func (p *Provider) Metadata() forge.Metadata { return p.meta }

// isAppAccount reports whether the account is configured for GitHub App
// installation auth. Installation tokens cannot use user-scoped endpoints
// (/user, notifications, gists, keys), so those capabilities are withheld.
func (p *Provider) isAppAccount() bool { return p.acct.AuthMethod == auth.MethodApp }

// Capabilities implements forge.Provider.
func (p *Provider) Capabilities() forge.Capabilities {
	caps := []forge.Capability{
		forge.CapRepositories, forge.CapRepoCreate, forge.CapRepoDelete, forge.CapRepoFork,
		forge.CapRepoRename, forge.CapRepoArchive,
		forge.CapPullRequests, forge.CapPRCreate, forge.CapPRMerge, forge.CapPRClose,
		forge.CapIssues, forge.CapIssueCreate, forge.CapIssueState, forge.CapIssueLabels,
		forge.CapPipelines, forge.CapPipelineRun, forge.CapPipelineCancel, forge.CapPipelineRetry,
		forge.CapPipelineLogs,
		forge.CapReleases, forge.CapReleaseCreate, forge.CapReleaseDelete,
		forge.CapNamespaces, forge.CapSearchRepos, forge.CapSearchIssues, forge.CapSearchCode,
		forge.CapSummary,
	}
	if !p.isAppAccount() {
		caps = append(caps,
			forge.CapNotifications, forge.CapSnippets, forge.CapSnippetCreate,
			forge.CapSSHKeys, forge.CapGPGKeys, forge.CapSettings)
	}
	return forge.NewCapabilities(caps...)
}

// RepositoryURLs implements forge.RepositoryProvider without network access.
func (p *Provider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	full := ref.Namespace + "/" + ref.Name
	return domain.RepositoryURLs{
		Web:   p.web + "/" + full,
		HTTPS: p.clone + "/" + full + ".git",
		SSH:   p.sshURL(full),
		API:   strings.TrimRight(p.api.String(), "/") + "/repos/" + full,
	}
}

func (p *Provider) sshURL(full string) string {
	host := p.ssh
	if strings.HasPrefix(host, "ssh://") {
		return strings.TrimRight(host, "/") + "/" + full + ".git"
	}
	if strings.Contains(host, ":") {
		// A port (e.g. ssh.github.com:443) needs the URL form; the scp-like
		// form cannot carry one.
		return "ssh://git@" + host + "/" + full + ".git"
	}
	return "git@" + host + ":" + full + ".git"
}

// Compile-time interface checks.
var (
	_ forge.Provider             = (*Provider)(nil)
	_ forge.Authenticator        = (*Provider)(nil)
	_ forge.Refresher            = (*Provider)(nil)
	_ forge.GitAuthenticator     = (*Provider)(nil)
	_ forge.RepositoryProvider   = (*Provider)(nil)
	_ forge.RepositoryCreator    = (*Provider)(nil)
	_ forge.RepositoryDeleter    = (*Provider)(nil)
	_ forge.RepositoryForker     = (*Provider)(nil)
	_ forge.RepositoryRenamer    = (*Provider)(nil)
	_ forge.RepositoryArchiver   = (*Provider)(nil)
	_ forge.PullRequestProvider  = (*Provider)(nil)
	_ forge.PullRequestCreator   = (*Provider)(nil)
	_ forge.PullRequestMerger    = (*Provider)(nil)
	_ forge.PullRequestCloser    = (*Provider)(nil)
	_ forge.PullRequestHeadRefer = (*Provider)(nil)
	_ forge.IssueProvider        = (*Provider)(nil)
	_ forge.IssueCreator         = (*Provider)(nil)
	_ forge.IssueStateChanger    = (*Provider)(nil)
	_ forge.PipelineProvider     = (*Provider)(nil)
	_ forge.PipelineRunner       = (*Provider)(nil)
	_ forge.PipelineCanceler     = (*Provider)(nil)
	_ forge.PipelineRetrier      = (*Provider)(nil)
	_ forge.PipelineLogger       = (*Provider)(nil)
	_ forge.ReleaseProvider      = (*Provider)(nil)
	_ forge.ReleaseCreator       = (*Provider)(nil)
	_ forge.ReleaseDeleter       = (*Provider)(nil)
	_ forge.NamespaceProvider    = (*Provider)(nil)
	_ forge.Searcher             = (*Provider)(nil)
	_ forge.NotificationProvider = (*Provider)(nil)
	_ forge.SnippetProvider      = (*Provider)(nil)
	_ forge.SnippetCreator       = (*Provider)(nil)
	_ forge.SSHKeyProvider       = (*Provider)(nil)
	_ forge.GPGKeyProvider       = (*Provider)(nil)
	_ forge.SettingsProvider     = (*Provider)(nil)
	_ forge.SummaryProvider      = (*Provider)(nil)
)

// ctxErr returns a canceled error if ctx is done.
func (p *Provider) ctxErr(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return p.canceled(op, err)
	}
	return nil
}
