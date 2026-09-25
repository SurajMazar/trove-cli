// Package domain contains Trove's provider-neutral models.
//
// These types describe Git-forge concepts (repositories, pull requests,
// pipelines, ...) without assuming any particular provider's API shape.
// Provider drivers translate their native payloads into these models.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// NamespaceType describes what kind of container a namespace is.
type NamespaceType string

const (
	NamespaceOrganization NamespaceType = "organization" // GitHub org, Gitea org, Azure DevOps org
	NamespaceGroup        NamespaceType = "group"        // GitLab group
	NamespaceSubgroup     NamespaceType = "subgroup"     // GitLab subgroup
	NamespaceWorkspace    NamespaceType = "workspace"    // Bitbucket workspace
	NamespaceProject      NamespaceType = "project"      // Bitbucket project, Azure DevOps project
	NamespaceUser         NamespaceType = "user"         // personal namespace
)

// Namespace is a generalized container for repositories. FullPath is the
// provider's canonical path (e.g. "group/subgroup" on GitLab).
type Namespace struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	FullPath    string        `json:"full_path"`
	Type        NamespaceType `json:"type"`
	ParentPath  string        `json:"parent_path,omitempty"`
	Description string        `json:"description,omitempty"`
	WebURL      string        `json:"web_url,omitempty"`
	AvatarURL   string        `json:"avatar_url,omitempty"`
	// Label is the provider's own term for this kind, e.g. "GitHub Organization".
	Label string `json:"label,omitempty"`
}

// Visibility of a repository or snippet.
type Visibility string

const (
	VisibilityPublic   Visibility = "public"
	VisibilityPrivate  Visibility = "private"
	VisibilityInternal Visibility = "internal"
)

// ParseVisibility validates a user-supplied visibility.
func ParseVisibility(s string) (Visibility, error) {
	switch Visibility(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return "", nil
	case VisibilityPublic:
		return VisibilityPublic, nil
	case VisibilityPrivate:
		return VisibilityPrivate, nil
	case VisibilityInternal:
		return VisibilityInternal, nil
	}
	return "", fmt.Errorf("invalid visibility %q (want public, private or internal)", s)
}

// RepositoryURLs are the addresses at which a repository can be reached.
type RepositoryURLs struct {
	Web   string `json:"web"`
	HTTPS string `json:"https"`
	SSH   string `json:"ssh,omitempty"`
	API   string `json:"api,omitempty"`
}

// Repository is a Git repository hosted on a forge. On GitLab this is a
// project; on Bitbucket a repository inside a workspace.
type Repository struct {
	ID            string         `json:"id"`
	Provider      string         `json:"provider"`      // local account alias
	ProviderType  string         `json:"provider_type"` // driver type, e.g. "gitlab"
	Name          string         `json:"name"`
	Namespace     string         `json:"namespace"` // full namespace path (may contain '/')
	FullName      string         `json:"full_name"` // namespace/name
	Description   string         `json:"description,omitempty"`
	Visibility    Visibility     `json:"visibility"`
	DefaultBranch string         `json:"default_branch,omitempty"`
	Archived      bool           `json:"archived"`
	Fork          bool           `json:"fork"`
	Empty         bool           `json:"empty,omitempty"`
	Language      string         `json:"language,omitempty"`
	Stars         int            `json:"stars,omitempty"`
	Forks         int            `json:"forks,omitempty"`
	OpenIssues    int            `json:"open_issues,omitempty"`
	CreatedAt     time.Time      `json:"created_at,omitzero"`
	UpdatedAt     time.Time      `json:"updated_at,omitzero"`
	PushedAt      time.Time      `json:"pushed_at,omitzero"`
	URLs          RepositoryURLs `json:"urls"`
	Topics        []string       `json:"topics,omitempty"`
}

// Ref returns a RepositoryRef pointing at r.
func (r Repository) Ref() RepositoryRef {
	return RepositoryRef{Provider: r.Provider, Namespace: r.Namespace, Name: r.Name, ID: r.ID}
}

// RepositoryRef identifies a repository independent of provider.
type RepositoryRef struct {
	Provider  string `json:"provider,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
}

// FullName returns "namespace/name".
func (r RepositoryRef) FullName() string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

func (r RepositoryRef) String() string {
	if r.Provider != "" {
		return r.Provider + ":" + r.FullName()
	}
	return r.FullName()
}

// IsZero reports whether the ref is empty.
func (r RepositoryRef) IsZero() bool { return r.Name == "" && r.ID == "" }

// ParseRepositoryRef parses "provider:namespace/name" or "namespace/name".
// Namespaces may contain slashes (GitLab subgroups); the final segment is the
// repository name.
func ParseRepositoryRef(s string) (RepositoryRef, error) {
	s = strings.TrimSpace(s)
	var ref RepositoryRef
	if s == "" {
		return ref, fmt.Errorf("empty repository reference")
	}
	if i := strings.Index(s, ":"); i > 0 && !strings.Contains(s[:i], "/") {
		ref.Provider = s[:i]
		s = s[i+1:]
	}
	s = strings.Trim(strings.TrimSuffix(s, ".git"), "/")
	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return RepositoryRef{}, fmt.Errorf("invalid repository reference %q (want [provider:]namespace/name)", s)
	}
	ref.Namespace = s[:i]
	ref.Name = s[i+1:]
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return RepositoryRef{}, fmt.Errorf("invalid repository reference %q", s)
		}
	}
	return ref, nil
}

// User is an account on a forge.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	WebURL    string `json:"web_url,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// Display returns "@username" or a best-effort name.
func (u User) Display() string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.Name
}

// PullRequestState is normalized PR/MR state.
type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "open"
	PullRequestClosed PullRequestState = "closed"
	PullRequestMerged PullRequestState = "merged"
	PullRequestAll    PullRequestState = "all"
)

// PullRequest is a GitHub Pull Request, GitLab Merge Request or Bitbucket
// Pull Request.
type PullRequest struct {
	ID           string           `json:"id"`
	Number       int              `json:"number"` // GitHub number, GitLab IID, Bitbucket ID
	Title        string           `json:"title"`
	Body         string           `json:"body,omitempty"`
	State        PullRequestState `json:"state"`
	Draft        bool             `json:"draft"`
	Author       string           `json:"author,omitempty"`
	SourceBranch string           `json:"source_branch"`
	TargetBranch string           `json:"target_branch"`
	SourceRepo   string           `json:"source_repo,omitempty"` // full name when from a fork
	HeadSHA      string           `json:"head_sha,omitempty"`
	Mergeable    *bool            `json:"mergeable,omitempty"`
	Labels       []string         `json:"labels,omitempty"`
	Reviewers    []string         `json:"reviewers,omitempty"`
	WebURL       string           `json:"web_url"`
	CreatedAt    time.Time        `json:"created_at,omitzero"`
	UpdatedAt    time.Time        `json:"updated_at,omitzero"`
	MergedAt     time.Time        `json:"merged_at,omitzero"`
	ClosedAt     time.Time        `json:"closed_at,omitzero"`
	// Term is the provider terminology, e.g. "Merge Request".
	Term string `json:"term,omitempty"`
}

// MergeMethod requested when merging a PR.
type MergeMethod string

const (
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodRebase MergeMethod = "rebase"
)

// IssueState is normalized issue state.
type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
	IssueAll    IssueState = "all"
)

// Issue is an issue in a repository's tracker.
type Issue struct {
	ID     string     `json:"id"`
	Number int        `json:"number"`
	Title  string     `json:"title"`
	Body   string     `json:"body,omitempty"`
	State  IssueState `json:"state"`
	// RawState is the provider's native state when richer than open/closed
	// (e.g. Bitbucket "resolved", "wontfix").
	RawState  string    `json:"raw_state,omitempty"`
	Author    string    `json:"author,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Comments  int       `json:"comments,omitempty"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
}

// PipelineStatus is a normalized CI status.
type PipelineStatus string

const (
	PipelinePending  PipelineStatus = "pending"
	PipelineRunning  PipelineStatus = "running"
	PipelineSuccess  PipelineStatus = "success"
	PipelineFailed   PipelineStatus = "failed"
	PipelineCanceled PipelineStatus = "canceled"
	PipelineSkipped  PipelineStatus = "skipped"
	PipelineManual   PipelineStatus = "manual"
	PipelineUnknown  PipelineStatus = "unknown"
)

// Finished reports whether the status is terminal.
func (s PipelineStatus) Finished() bool {
	switch s {
	case PipelineSuccess, PipelineFailed, PipelineCanceled, PipelineSkipped:
		return true
	}
	return false
}

// Pipeline is a CI run: a GitHub Actions workflow run, a GitLab pipeline, or a
// Bitbucket Pipelines pipeline.
type Pipeline struct {
	ID         string         `json:"id"`
	Number     int            `json:"number,omitempty"`
	Name       string         `json:"name,omitempty"`
	Status     PipelineStatus `json:"status"`
	RawStatus  string         `json:"raw_status,omitempty"`
	Ref        string         `json:"ref,omitempty"`
	SHA        string         `json:"sha,omitempty"`
	Event      string         `json:"event,omitempty"`
	Actor      string         `json:"actor,omitempty"`
	WebURL     string         `json:"web_url,omitempty"`
	CreatedAt  time.Time      `json:"created_at,omitzero"`
	StartedAt  time.Time      `json:"started_at,omitzero"`
	FinishedAt time.Time      `json:"finished_at,omitzero"`
	Duration   time.Duration  `json:"duration,omitempty"`
	Jobs       []PipelineJob  `json:"jobs,omitempty"`
}

// PipelineJob is a job/step within a pipeline.
type PipelineJob struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Stage      string         `json:"stage,omitempty"`
	Status     PipelineStatus `json:"status"`
	RawStatus  string         `json:"raw_status,omitempty"`
	WebURL     string         `json:"web_url,omitempty"`
	StartedAt  time.Time      `json:"started_at,omitzero"`
	FinishedAt time.Time      `json:"finished_at,omitzero"`
}

// Release is a tagged release.
type Release struct {
	ID          string    `json:"id"`
	Tag         string    `json:"tag"`
	Name        string    `json:"name,omitempty"`
	Body        string    `json:"body,omitempty"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Author      string    `json:"author,omitempty"`
	WebURL      string    `json:"web_url,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	Assets      []Asset   `json:"assets,omitempty"`
}

// Asset is a downloadable file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size,omitempty"`
}

// Snippet is a GitHub Gist, GitLab Snippet or Bitbucket Snippet.
type Snippet struct {
	ID          string        `json:"id"`
	Title       string        `json:"title,omitempty"`
	Description string        `json:"description,omitempty"`
	Visibility  Visibility    `json:"visibility"`
	Owner       string        `json:"owner,omitempty"`
	WebURL      string        `json:"web_url,omitempty"`
	Files       []SnippetFile `json:"files,omitempty"`
	CreatedAt   time.Time     `json:"created_at,omitzero"`
	UpdatedAt   time.Time     `json:"updated_at,omitzero"`
	// Term is the provider term, e.g. "Gist".
	Term string `json:"term,omitempty"`
}

// SnippetFile is one file of a snippet. Content may be empty in list results.
type SnippetFile struct {
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	RawURL  string `json:"raw_url,omitempty"`
}

// Notification is a GitHub notification thread or a GitLab To-Do item.
type Notification struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Reason     string    `json:"reason,omitempty"`
	Type       string    `json:"type,omitempty"`
	Repository string    `json:"repository,omitempty"`
	Unread     bool      `json:"unread"`
	WebURL     string    `json:"web_url,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// SSHKey is a public SSH key registered with an account.
type SSHKey struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Key         string    `json:"key"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
}

// GPGKey is a public GPG key registered with an account.
type GPGKey struct {
	ID        string    `json:"id"`
	KeyID     string    `json:"key_id"`
	Emails    []string  `json:"emails,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Setting is an account-level setting that can be managed through the API.
type Setting struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Writable    bool   `json:"writable"`
}

// AccountSummary powers the dashboard. Nil fields mean "not available from
// this provider" and are rendered as such, never as zero.
type AccountSummary struct {
	Repositories     *int `json:"repositories,omitempty"`
	OpenPullRequests *int `json:"open_pull_requests,omitempty"`
	OpenIssues       *int `json:"open_issues,omitempty"`
	RunningPipelines *int `json:"running_pipelines,omitempty"`
	Unread           *int `json:"unread_notifications,omitempty"`
}

// SearchKind is what to search for.
type SearchKind string

const (
	SearchRepositories SearchKind = "repositories"
	SearchIssues       SearchKind = "issues"
	SearchCode         SearchKind = "code"
)

// SearchQuery is a provider-neutral search request.
type SearchQuery struct {
	Kind  SearchKind
	Query string
	// Namespace/Repository optionally scope the search. Some providers require
	// a scope for some kinds (e.g. Bitbucket code search needs a workspace).
	Namespace  string
	Repository *RepositoryRef
	Limit      int
}

// SearchResult holds the hits of a search; only the slice matching the
// query kind is populated.
type SearchResult struct {
	Kind         SearchKind   `json:"kind"`
	Total        int          `json:"total"`
	Repositories []Repository `json:"repositories,omitempty"`
	Issues       []Issue      `json:"issues,omitempty"`
	Code         []CodeResult `json:"code,omitempty"`
}

// CodeResult is a code search hit.
type CodeResult struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Ref        string `json:"ref,omitempty"`
	Fragment   string `json:"fragment,omitempty"`
	WebURL     string `json:"web_url,omitempty"`
}

// GitProtocol selects the clone transport.
type GitProtocol string

const (
	ProtocolHTTPS GitProtocol = "https"
	ProtocolSSH   GitProtocol = "ssh"
)

// ParseGitProtocol validates a user-supplied protocol.
func ParseGitProtocol(s string) (GitProtocol, error) {
	switch GitProtocol(strings.ToLower(strings.TrimSpace(s))) {
	case "", ProtocolHTTPS:
		return ProtocolHTTPS, nil
	case ProtocolSSH:
		return ProtocolSSH, nil
	}
	return "", fmt.Errorf("invalid protocol %q (want https or ssh)", s)
}

// ProviderAccount is a configured account as shown to the user. It never
// contains secret values.
type ProviderAccount struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Host      string `json:"host"`
	APIURL    string `json:"api_url,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
	Default   bool   `json:"default"`
}

// GitCredential describes how git should authenticate for a host. The secret
// itself is never part of this struct; it is resolved on demand.
type GitCredential struct {
	Host     string      `json:"host"`
	Username string      `json:"username,omitempty"`
	Protocol GitProtocol `json:"protocol"`
}
