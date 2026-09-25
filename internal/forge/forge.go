// Package forge defines the Forge Provider contract.
//
// A forge provider (GitHub, GitLab, Bitbucket, a custom forge, ...) is a
// Driver that produces Provider instances bound to one configured account.
// The contract is intentionally split into small capability interfaces: a
// provider implements only what its API genuinely supports and declares it in
// Capabilities(). The application core never assumes two providers behave the
// same way; it asks for a capability and receives either an implementation or
// a typed ErrUnsupportedCapability.
//
// This package must not contain provider-specific API behavior.
package forge

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
)

// Capability names a feature a provider may support.
type Capability string

const (
	CapRepositories   Capability = "repositories"
	CapRepoCreate     Capability = "repositories.create"
	CapRepoDelete     Capability = "repositories.delete"
	CapRepoFork       Capability = "repositories.fork"
	CapRepoRename     Capability = "repositories.rename"
	CapRepoArchive    Capability = "repositories.archive"
	CapPullRequests   Capability = "pull_requests"
	CapPRCreate       Capability = "pull_requests.create"
	CapPRMerge        Capability = "pull_requests.merge"
	CapPRClose        Capability = "pull_requests.close"
	CapIssues         Capability = "issues"
	CapIssueCreate    Capability = "issues.create"
	CapIssueState     Capability = "issues.state"
	CapIssueLabels    Capability = "issues.labels"
	CapPipelines      Capability = "pipelines"
	CapPipelineRun    Capability = "pipelines.run"
	CapPipelineCancel Capability = "pipelines.cancel"
	CapPipelineRetry  Capability = "pipelines.retry"
	CapPipelineLogs   Capability = "pipelines.logs"
	CapReleases       Capability = "releases"
	CapReleaseCreate  Capability = "releases.create"
	CapReleaseDelete  Capability = "releases.delete"
	CapNamespaces     Capability = "namespaces"
	CapSearchRepos    Capability = "search.repositories"
	CapSearchIssues   Capability = "search.issues"
	CapSearchCode     Capability = "search.code"
	CapNotifications  Capability = "notifications"
	CapSnippets       Capability = "snippets"
	CapSnippetCreate  Capability = "snippets.create"
	CapSSHKeys        Capability = "keys.ssh"
	CapGPGKeys        Capability = "keys.gpg"
	CapSettings       Capability = "settings"
	CapSummary        Capability = "summary"
)

// AllCapabilities lists every known capability in display order.
var AllCapabilities = []Capability{
	CapRepositories, CapRepoCreate, CapRepoDelete, CapRepoFork, CapRepoRename, CapRepoArchive,
	CapPullRequests, CapPRCreate, CapPRMerge, CapPRClose,
	CapIssues, CapIssueCreate, CapIssueState, CapIssueLabels,
	CapPipelines, CapPipelineRun, CapPipelineCancel, CapPipelineRetry, CapPipelineLogs,
	CapReleases, CapReleaseCreate, CapReleaseDelete,
	CapNamespaces, CapSearchRepos, CapSearchIssues, CapSearchCode,
	CapNotifications, CapSnippets, CapSnippetCreate, CapSSHKeys, CapGPGKeys,
	CapSettings, CapSummary,
}

// IsKnown reports whether c is a capability Trove understands.
func (c Capability) IsKnown() bool {
	for _, k := range AllCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// Capabilities is the set of features a provider supports.
type Capabilities map[Capability]bool

// NewCapabilities builds a set from a list.
func NewCapabilities(cs ...Capability) Capabilities {
	out := make(Capabilities, len(cs))
	for _, c := range cs {
		out[c] = true
	}
	return out
}

// Has reports whether c is supported.
func (c Capabilities) Has(cap Capability) bool { return c[cap] }

// List returns supported capabilities in canonical order.
func (c Capabilities) List() []Capability {
	out := make([]Capability, 0, len(c))
	for _, k := range AllCapabilities {
		if c[k] {
			out = append(out, k)
		}
	}
	// Unknown (future) capabilities last, sorted.
	var extra []Capability
	for k, v := range c {
		if v && !k.IsKnown() {
			extra = append(extra, k)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append(out, extra...)
}

// Terms holds provider-specific terminology shown to users.
type Terms struct {
	Repository       string // "Repository" / "Project"
	PullRequest      string // "Pull Request" / "Merge Request"
	PullRequestShort string // "PR" / "MR"
	Pipeline         string // "Workflow run" / "Pipeline"
	Snippet          string // "Gist" / "Snippet"
	Notification     string // "Notification" / "To-Do item"
	Namespace        string // "Organization" / "Group" / "Workspace"
}

// Metadata describes a provider instance.
type Metadata struct {
	Name        string        `json:"name"`         // local account alias
	Type        string        `json:"type"`         // driver type
	DisplayName string        `json:"display_name"` // e.g. "GitHub Enterprise Server"
	Host        string        `json:"host"`
	APIURL      string        `json:"api_url"`
	WebURL      string        `json:"web_url"`
	Deployment  string        `json:"deployment"` // "cloud", "self-managed", "enterprise-server", "custom"
	AuthMethods []auth.Method `json:"auth_methods"`
	Terms       Terms         `json:"-"`
}

// Account is everything a driver needs to construct a Provider for one
// configured account. It never contains secret values directly: credentials
// are loaded lazily through Credentials.
type Account struct {
	Name           string
	Type           string
	Host           string
	APIURL         string // optional override
	WebURL         string // optional override
	CloneBaseURL   string // optional override for HTTPS clone URLs
	SSHHost        string // optional override for SSH clone host
	AuthMethod     auth.Method
	Username       string // e.g. Bitbucket account email for API tokens
	ClientID       string // OAuth client ID (public)
	AppID          string // GitHub App ID
	InstallationID string
	Scopes         []string

	Credentials auth.Store
	Logger      *slog.Logger
	// Transport is the base HTTP transport (tests inject httptest transports).
	Transport http.RoundTripper
	// Decode decodes this account's full configuration block into v, letting
	// drivers read their own provider-specific settings.
	Decode func(v any) error
}

// Driver constructs providers of one type.
type Driver interface {
	Type() string
	DisplayName() string
	Description() string
	DefaultHost() string
	AuthMethods() []auth.Method
	New(acct Account) (Provider, error)
}

// Provider is the minimal contract every forge provider implements.
type Provider interface {
	Metadata() Metadata
	Capabilities() Capabilities
	CurrentUser(ctx context.Context) (*domain.User, error)
}

// Authenticator runs provider-specific login flows and validates
// credentials. Every built-in provider implements it; the flows themselves
// differ per provider.
type Authenticator interface {
	// Login obtains a credential. It must not persist it; the caller does.
	Login(ctx context.Context, req auth.Request) (*auth.Result, error)
	// AuthStatus validates the stored credential against the API.
	AuthStatus(ctx context.Context) (*auth.Status, error)
}

// Refresher can refresh expiring credentials (e.g. OAuth refresh tokens) and
// persists the result through Account.Credentials.
type Refresher interface {
	RefreshCredential(ctx context.Context) (*auth.Status, error)
}

// GitAuthenticator supplies credentials for Git-over-HTTPS operations. The
// username convention is provider specific (e.g. GitHub "x-access-token",
// GitLab "oauth2", Bitbucket "x-bitbucket-api-token-auth"/"x-token-auth").
// Trove hands these to git through its credential-helper protocol, never via
// URLs or command-line arguments.
type GitAuthenticator interface {
	GitCredentials(ctx context.Context) (username, password string, err error)
}

// Revoker can revoke the current credential server-side.
type Revoker interface {
	RevokeCredential(ctx context.Context) error
}

// --- Repositories -----------------------------------------------------------

// ListOptions are common list options. Limit <= 0 means "all".
type ListOptions struct {
	Limit int
}

// ListRepositoryOptions filter repository listings.
type ListRepositoryOptions struct {
	ListOptions
	Namespace       string
	Visibility      domain.Visibility
	IncludeArchived bool
	// OwnedOnly restricts to repositories owned by the user.
	OwnedOnly bool
}

// CreateRepositoryRequest creates a repository.
type CreateRepositoryRequest struct {
	Name          string
	Namespace     string // empty = personal namespace
	Description   string
	Visibility    domain.Visibility
	DefaultBranch string
	AutoInit      bool
}

// ForkRequest forks a repository.
type ForkRequest struct {
	Namespace string // target namespace; empty = personal
	Name      string // optional new name
}

type RepositoryProvider interface {
	ListRepositories(ctx context.Context, opts ListRepositoryOptions) ([]domain.Repository, error)
	GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error)
	// RepositoryURLs computes URLs without an API call.
	RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs
}

type RepositoryCreator interface {
	CreateRepository(ctx context.Context, req CreateRepositoryRequest) (*domain.Repository, error)
}

type RepositoryDeleter interface {
	DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error
}

type RepositoryForker interface {
	ForkRepository(ctx context.Context, ref domain.RepositoryRef, req ForkRequest) (*domain.Repository, error)
}

type RepositoryRenamer interface {
	RenameRepository(ctx context.Context, ref domain.RepositoryRef, newName string) (*domain.Repository, error)
}

type RepositoryArchiver interface {
	SetRepositoryArchived(ctx context.Context, ref domain.RepositoryRef, archived bool) (*domain.Repository, error)
}

// --- Pull / merge requests --------------------------------------------------

type PullRequestListOptions struct {
	ListOptions
	State        domain.PullRequestState
	Author       string
	TargetBranch string
}

type CreatePullRequestRequest struct {
	Title        string
	Body         string
	SourceBranch string
	TargetBranch string
	Draft        bool
	// SourceRepo is set when the source branch lives in a fork
	// ("owner/repo" or the provider's equivalent).
	SourceRepo string
}

type MergePullRequestRequest struct {
	Method             domain.MergeMethod
	CommitTitle        string
	CommitMessage      string
	DeleteSourceBranch bool
}

type PullRequestProvider interface {
	ListPullRequests(ctx context.Context, ref domain.RepositoryRef, opts PullRequestListOptions) ([]domain.PullRequest, error)
	GetPullRequest(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.PullRequest, error)
}

type PullRequestCreator interface {
	CreatePullRequest(ctx context.Context, ref domain.RepositoryRef, req CreatePullRequestRequest) (*domain.PullRequest, error)
}

type PullRequestMerger interface {
	MergePullRequest(ctx context.Context, ref domain.RepositoryRef, number int, req MergePullRequestRequest) error
	// MergeMethods lists methods the provider supports.
	MergeMethods() []domain.MergeMethod
}

// PullRequestHeadRefer is implemented by providers that expose pull request
// heads as fetchable refs on the base repository (e.g. GitHub
// "refs/pull/N/head", GitLab "refs/merge-requests/N/head"). Providers without
// such refs are checked out by fetching the source branch instead.
type PullRequestHeadRefer interface {
	PullRequestHeadRef(number int) string
}

type PullRequestCloser interface {
	ClosePullRequest(ctx context.Context, ref domain.RepositoryRef, number int) error
}

// --- Issues -------------------------------------------------------------------

type IssueListOptions struct {
	ListOptions
	State    domain.IssueState
	Labels   []string
	Assignee string
	Author   string
}

type CreateIssueRequest struct {
	Title     string
	Body      string
	Labels    []string
	Assignees []string
}

type IssueProvider interface {
	ListIssues(ctx context.Context, ref domain.RepositoryRef, opts IssueListOptions) ([]domain.Issue, error)
	GetIssue(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.Issue, error)
}

type IssueCreator interface {
	CreateIssue(ctx context.Context, ref domain.RepositoryRef, req CreateIssueRequest) (*domain.Issue, error)
}

type IssueStateChanger interface {
	SetIssueState(ctx context.Context, ref domain.RepositoryRef, number int, state domain.IssueState) (*domain.Issue, error)
}

// --- Pipelines ----------------------------------------------------------------

type PipelineListOptions struct {
	ListOptions
	Ref    string
	Status domain.PipelineStatus
}

type RunPipelineRequest struct {
	Ref string
	// Workflow identifies what to run where the provider needs it (GitHub
	// workflow file name or ID; Bitbucket custom pipeline name).
	Workflow  string
	Variables map[string]string
}

type PipelineProvider interface {
	ListPipelines(ctx context.Context, ref domain.RepositoryRef, opts PipelineListOptions) ([]domain.Pipeline, error)
	// GetPipeline returns the pipeline including its jobs/steps.
	GetPipeline(ctx context.Context, ref domain.RepositoryRef, id string) (*domain.Pipeline, error)
}

type PipelineRunner interface {
	// RunPipeline triggers a pipeline. The returned pipeline may be nil when
	// the provider accepts the request asynchronously without returning one.
	RunPipeline(ctx context.Context, ref domain.RepositoryRef, req RunPipelineRequest) (*domain.Pipeline, error)
}

type PipelineCanceler interface {
	CancelPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error
}

type PipelineRetrier interface {
	RetryPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error
}

type PipelineLogger interface {
	// PipelineLogs writes logs for one job, or for all jobs when jobID is empty.
	PipelineLogs(ctx context.Context, ref domain.RepositoryRef, pipelineID, jobID string, w io.Writer) error
}

// --- Releases -----------------------------------------------------------------

type CreateReleaseRequest struct {
	Tag        string
	Name       string
	Body       string
	Target     string // commitish to tag if the tag does not exist
	Draft      bool
	Prerelease bool
}

type ReleaseProvider interface {
	ListReleases(ctx context.Context, ref domain.RepositoryRef, opts ListOptions) ([]domain.Release, error)
	GetRelease(ctx context.Context, ref domain.RepositoryRef, tag string) (*domain.Release, error)
}

type ReleaseCreator interface {
	CreateRelease(ctx context.Context, ref domain.RepositoryRef, req CreateReleaseRequest) (*domain.Release, error)
}

type ReleaseDeleter interface {
	DeleteRelease(ctx context.Context, ref domain.RepositoryRef, tag string) error
}

// --- Namespaces, search, notifications, snippets, keys, settings --------------

type NamespaceProvider interface {
	ListNamespaces(ctx context.Context, opts ListOptions) ([]domain.Namespace, error)
	GetNamespace(ctx context.Context, path string) (*domain.Namespace, error)
}

type Searcher interface {
	Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error)
}

type NotificationListOptions struct {
	ListOptions
	All bool // include already-read notifications
}

type NotificationProvider interface {
	ListNotifications(ctx context.Context, opts NotificationListOptions) ([]domain.Notification, error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	MarkNotificationRead(ctx context.Context, id string) error
	MarkAllNotificationsRead(ctx context.Context) error
}

type CreateSnippetRequest struct {
	Title       string
	Description string
	Visibility  domain.Visibility
	Files       []domain.SnippetFile
	// Namespace is required by providers that scope snippets (Bitbucket workspace).
	Namespace string
}

type SnippetProvider interface {
	ListSnippets(ctx context.Context, opts ListOptions) ([]domain.Snippet, error)
	// GetSnippet returns the snippet with file contents.
	GetSnippet(ctx context.Context, id string) (*domain.Snippet, error)
}

type SnippetCreator interface {
	CreateSnippet(ctx context.Context, req CreateSnippetRequest) (*domain.Snippet, error)
}

type SSHKeyProvider interface {
	ListSSHKeys(ctx context.Context) ([]domain.SSHKey, error)
	AddSSHKey(ctx context.Context, title, key string) (*domain.SSHKey, error)
	RemoveSSHKey(ctx context.Context, id string) error
}

type GPGKeyProvider interface {
	ListGPGKeys(ctx context.Context) ([]domain.GPGKey, error)
}

type SettingsProvider interface {
	ListSettings(ctx context.Context) ([]domain.Setting, error)
	GetSetting(ctx context.Context, key string) (*domain.Setting, error)
	SetSetting(ctx context.Context, key, value string) (*domain.Setting, error)
}

type SummaryProvider interface {
	Summary(ctx context.Context) (*domain.AccountSummary, error)
}
