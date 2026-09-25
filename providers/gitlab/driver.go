package gitlab

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// DriverType is the configuration "type" of GitLab accounts.
const DriverType = "gitlab"

const (
	defaultHost = "gitlab.com"
	// maxPerPage is GitLab's upper bound for per_page on offset pagination.
	maxPerPage = 100
)

type driver struct{}

// NewDriver returns the GitLab forge driver (GitLab.com and Self-Managed).
func NewDriver() forge.Driver { return driver{} }

func (driver) Type() string        { return DriverType }
func (driver) DisplayName() string { return "GitLab" }
func (driver) Description() string {
	return "GitLab.com and GitLab Self-Managed (REST API v4): projects, merge requests, issues, CI/CD pipelines, releases, groups, To-Do items and snippets"
}
func (driver) DefaultHost() string { return defaultHost }
func (driver) AuthMethods() []auth.Method {
	return []auth.Method{auth.MethodToken, auth.MethodOAuth}
}

// New binds a provider to acct. It performs no network I/O and does not load
// credentials; those are loaded lazily on the first request.
func (driver) New(acct forge.Account) (forge.Provider, error) {
	p := &provider{acct: acct, projectPaths: map[int64]string{}}

	host := strings.TrimSpace(acct.Host)
	if host == "" && acct.APIURL != "" {
		u, err := url.Parse(acct.APIURL)
		if err != nil || u.Host == "" {
			return nil, invalidConfig(acct.Name, "api_url %q is not a valid URL", acct.APIURL)
		}
		host = u.Host
	}
	if host == "" {
		host = defaultHost
	}
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	p.host = host

	p.webURL = strings.TrimRight(acct.WebURL, "/")
	if p.webURL == "" {
		p.webURL = "https://" + host
	}
	p.apiURL = strings.TrimRight(acct.APIURL, "/")
	if p.apiURL == "" {
		p.apiURL = "https://" + host + "/api/v4"
	}
	for field, raw := range map[string]string{"api_url": p.apiURL, "web_url": p.webURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, invalidConfig(acct.Name, "%s %q is not a valid http(s) URL", field, raw)
		}
	}
	if acct.CloneBaseURL != "" {
		if u, err := url.Parse(acct.CloneBaseURL); err != nil || u.Host == "" {
			return nil, invalidConfig(acct.Name, "clone_base_url %q is not a valid URL", acct.CloneBaseURL)
		}
	}

	p.log = acct.Logger
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	// GitLab signals throttling exclusively with HTTP 429 (plus Retry-After /
	// RateLimit-Reset), which RetryTransport already understands. A 403 on
	// GitLab is always an authorization failure, never a rate limit, so no
	// provider-specific RateLimited detector is needed.
	p.hc = httpx.NewClient(httpx.Options{Base: acct.Transport, Logger: acct.Logger})
	return p, nil
}

func invalidConfig(name, format string, args ...any) error {
	e := errs.New(errs.ErrInvalidConfiguration, format, args...)
	e.Provider = name
	return e
}

// provider is a GitLab provider bound to one account.
type provider struct {
	acct   forge.Account
	host   string
	apiURL string // e.g. https://gitlab.com/api/v4
	webURL string // e.g. https://gitlab.com
	hc     *http.Client
	log    *slog.Logger

	credMu sync.Mutex
	cred   *auth.Credential

	// projectPaths caches project ID -> path_with_namespace, used to name
	// fork sources of merge requests and code-search hits, which GitLab only
	// reports by numeric project ID.
	pathMu       sync.Mutex
	projectPaths map[int64]string
}

// Compile-time interface checks for everything this driver declares.
var (
	_ forge.Provider             = (*provider)(nil)
	_ forge.Authenticator        = (*provider)(nil)
	_ forge.Refresher            = (*provider)(nil)
	_ forge.Revoker              = (*provider)(nil)
	_ forge.GitAuthenticator     = (*provider)(nil)
	_ forge.RepositoryProvider   = (*provider)(nil)
	_ forge.RepositoryCreator    = (*provider)(nil)
	_ forge.RepositoryDeleter    = (*provider)(nil)
	_ forge.RepositoryForker     = (*provider)(nil)
	_ forge.RepositoryRenamer    = (*provider)(nil)
	_ forge.RepositoryArchiver   = (*provider)(nil)
	_ forge.PullRequestProvider  = (*provider)(nil)
	_ forge.PullRequestCreator   = (*provider)(nil)
	_ forge.PullRequestMerger    = (*provider)(nil)
	_ forge.PullRequestCloser    = (*provider)(nil)
	_ forge.PullRequestHeadRefer = (*provider)(nil)
	_ forge.IssueProvider        = (*provider)(nil)
	_ forge.IssueCreator         = (*provider)(nil)
	_ forge.IssueStateChanger    = (*provider)(nil)
	_ forge.PipelineProvider     = (*provider)(nil)
	_ forge.PipelineRunner       = (*provider)(nil)
	_ forge.PipelineCanceler     = (*provider)(nil)
	_ forge.PipelineRetrier      = (*provider)(nil)
	_ forge.PipelineLogger       = (*provider)(nil)
	_ forge.ReleaseProvider      = (*provider)(nil)
	_ forge.ReleaseCreator       = (*provider)(nil)
	_ forge.ReleaseDeleter       = (*provider)(nil)
	_ forge.NamespaceProvider    = (*provider)(nil)
	_ forge.Searcher             = (*provider)(nil)
	_ forge.NotificationProvider = (*provider)(nil)
	_ forge.SnippetProvider      = (*provider)(nil)
	_ forge.SnippetCreator       = (*provider)(nil)
	_ forge.SSHKeyProvider       = (*provider)(nil)
	_ forge.GPGKeyProvider       = (*provider)(nil)
	_ forge.SettingsProvider     = (*provider)(nil)
	_ forge.SummaryProvider      = (*provider)(nil)
)

// isCloud reports whether the account targets GitLab.com.
func (p *provider) isCloud() bool {
	h := strings.ToLower(p.host)
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == defaultHost
}

func (p *provider) Metadata() forge.Metadata {
	m := forge.Metadata{
		Name:        p.acct.Name,
		Type:        DriverType,
		DisplayName: "GitLab",
		Host:        p.host,
		APIURL:      p.apiURL,
		WebURL:      p.webURL,
		Deployment:  "cloud",
		AuthMethods: driver{}.AuthMethods(),
		Terms: forge.Terms{
			Repository:       "Project",
			PullRequest:      "Merge Request",
			PullRequestShort: "MR",
			Pipeline:         "Pipeline",
			Snippet:          "Snippet",
			Notification:     "To-Do item",
			Namespace:        "Group",
		},
	}
	if !p.isCloud() {
		m.DisplayName = "GitLab Self-Managed"
		m.Deployment = "self-managed"
	}
	return m
}

func (p *provider) Capabilities() forge.Capabilities {
	return forge.NewCapabilities(
		forge.CapRepositories, forge.CapRepoCreate, forge.CapRepoDelete, forge.CapRepoFork,
		forge.CapRepoRename, forge.CapRepoArchive,
		forge.CapPullRequests, forge.CapPRCreate, forge.CapPRMerge, forge.CapPRClose,
		forge.CapIssues, forge.CapIssueCreate, forge.CapIssueState, forge.CapIssueLabels,
		forge.CapPipelines, forge.CapPipelineRun, forge.CapPipelineCancel, forge.CapPipelineRetry,
		forge.CapPipelineLogs,
		forge.CapReleases, forge.CapReleaseCreate, forge.CapReleaseDelete,
		forge.CapNamespaces, forge.CapSearchRepos, forge.CapSearchIssues, forge.CapSearchCode,
		forge.CapNotifications, forge.CapSnippets, forge.CapSnippetCreate,
		forge.CapSSHKeys, forge.CapGPGKeys, forge.CapSettings, forge.CapSummary,
	)
}
