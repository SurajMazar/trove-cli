package forge

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

// Registry holds the available drivers keyed by type. New forges are added
// by implementing Driver and registering it; nothing else changes.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{drivers: map[string]Driver{}} }

// Register adds a driver. Registering the same type twice is an error.
func (r *Registry) Register(d Driver) error {
	if d == nil || d.Type() == "" {
		return fmt.Errorf("driver must have a type")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drivers[d.Type()]; ok {
		return fmt.Errorf("driver %q already registered", d.Type())
	}
	r.drivers[d.Type()] = d
	return nil
}

// MustRegister registers or panics; for use during program init.
func (r *Registry) MustRegister(d Driver) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

// Get returns the driver for a type.
func (r *Registry) Get(typ string) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[typ]
	if !ok {
		return nil, errs.New(errs.ErrDriverNotFound, "provider type %q is not supported (available: %v)", typ, r.typesLocked())
	}
	return d, nil
}

// List returns drivers sorted by type.
func (r *Registry) List() []Driver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Driver, 0, len(r.drivers))
	for _, d := range r.drivers {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type() < out[j].Type() })
	return out
}

func (r *Registry) typesLocked() []string {
	out := make([]string, 0, len(r.drivers))
	for t := range r.drivers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// capabilityFeature is the human name used in "does not support X" errors.
var capabilityFeature = map[Capability]string{
	CapRepositories: "repositories", CapRepoCreate: "creating repositories",
	CapRepoDelete: "deleting repositories", CapRepoFork: "forking repositories",
	CapRepoRename: "renaming repositories", CapRepoArchive: "archiving repositories",
	CapPullRequests: "pull requests", CapPRCreate: "creating pull requests",
	CapPRMerge: "merging pull requests", CapPRClose: "closing pull requests",
	CapIssues: "issues", CapIssueCreate: "creating issues", CapIssueState: "closing or reopening issues",
	CapIssueLabels: "issue labels",
	CapPipelines:   "pipelines", CapPipelineRun: "running pipelines", CapPipelineCancel: "canceling pipelines",
	CapPipelineRetry: "retrying pipelines", CapPipelineLogs: "pipeline logs",
	CapReleases: "releases", CapReleaseCreate: "creating releases", CapReleaseDelete: "deleting releases",
	CapNamespaces: "namespaces", CapSearchRepos: "repository search", CapSearchIssues: "issue search",
	CapSearchCode: "code search", CapNotifications: "notifications", CapSnippets: "snippets",
	CapSnippetCreate: "creating snippets", CapSSHKeys: "SSH keys", CapGPGKeys: "GPG keys",
	CapSettings: "settings", CapSummary: "account summaries",
}

// FeatureName returns a human-readable description of a capability.
func FeatureName(c Capability) string {
	if s, ok := capabilityFeature[c]; ok {
		return s
	}
	return string(c)
}

// As checks that p declares capability c and implements interface T, returning
// the implementation or an ErrUnsupportedCapability error. It never panics and
// never emulates missing functionality.
func As[T any](p Provider, c Capability) (T, error) {
	var zero T
	if p == nil {
		return zero, errs.New(errs.ErrProviderNotFound, "no provider selected")
	}
	name := p.Metadata().Name
	if name == "" {
		name = p.Metadata().Type
	}
	if !p.Capabilities().Has(c) {
		return zero, errs.Unsupported(name, FeatureName(c))
	}
	impl, ok := p.(T)
	if !ok {
		// Declared but not implemented: a driver bug, reported as unsupported
		// rather than a crash.
		return zero, errs.Unsupported(name, FeatureName(c))
	}
	return impl, nil
}

// capabilityInterfaces maps each capability to a check that the provider
// implements the corresponding interface. Used by contract tests.
var capabilityInterfaces = map[Capability]func(Provider) bool{
	CapRepositories:   func(p Provider) bool { _, ok := p.(RepositoryProvider); return ok },
	CapRepoCreate:     func(p Provider) bool { _, ok := p.(RepositoryCreator); return ok },
	CapRepoDelete:     func(p Provider) bool { _, ok := p.(RepositoryDeleter); return ok },
	CapRepoFork:       func(p Provider) bool { _, ok := p.(RepositoryForker); return ok },
	CapRepoRename:     func(p Provider) bool { _, ok := p.(RepositoryRenamer); return ok },
	CapRepoArchive:    func(p Provider) bool { _, ok := p.(RepositoryArchiver); return ok },
	CapPullRequests:   func(p Provider) bool { _, ok := p.(PullRequestProvider); return ok },
	CapPRCreate:       func(p Provider) bool { _, ok := p.(PullRequestCreator); return ok },
	CapPRMerge:        func(p Provider) bool { _, ok := p.(PullRequestMerger); return ok },
	CapPRClose:        func(p Provider) bool { _, ok := p.(PullRequestCloser); return ok },
	CapIssues:         func(p Provider) bool { _, ok := p.(IssueProvider); return ok },
	CapIssueCreate:    func(p Provider) bool { _, ok := p.(IssueCreator); return ok },
	CapIssueState:     func(p Provider) bool { _, ok := p.(IssueStateChanger); return ok },
	CapIssueLabels:    func(p Provider) bool { _, ok := p.(IssueProvider); return ok },
	CapPipelines:      func(p Provider) bool { _, ok := p.(PipelineProvider); return ok },
	CapPipelineRun:    func(p Provider) bool { _, ok := p.(PipelineRunner); return ok },
	CapPipelineCancel: func(p Provider) bool { _, ok := p.(PipelineCanceler); return ok },
	CapPipelineRetry:  func(p Provider) bool { _, ok := p.(PipelineRetrier); return ok },
	CapPipelineLogs:   func(p Provider) bool { _, ok := p.(PipelineLogger); return ok },
	CapReleases:       func(p Provider) bool { _, ok := p.(ReleaseProvider); return ok },
	CapReleaseCreate:  func(p Provider) bool { _, ok := p.(ReleaseCreator); return ok },
	CapReleaseDelete:  func(p Provider) bool { _, ok := p.(ReleaseDeleter); return ok },
	CapNamespaces:     func(p Provider) bool { _, ok := p.(NamespaceProvider); return ok },
	CapSearchRepos:    func(p Provider) bool { _, ok := p.(Searcher); return ok },
	CapSearchIssues:   func(p Provider) bool { _, ok := p.(Searcher); return ok },
	CapSearchCode:     func(p Provider) bool { _, ok := p.(Searcher); return ok },
	CapNotifications:  func(p Provider) bool { _, ok := p.(NotificationProvider); return ok },
	CapSnippets:       func(p Provider) bool { _, ok := p.(SnippetProvider); return ok },
	CapSnippetCreate:  func(p Provider) bool { _, ok := p.(SnippetCreator); return ok },
	CapSSHKeys:        func(p Provider) bool { _, ok := p.(SSHKeyProvider); return ok },
	CapGPGKeys:        func(p Provider) bool { _, ok := p.(GPGKeyProvider); return ok },
	CapSettings:       func(p Provider) bool { _, ok := p.(SettingsProvider); return ok },
	CapSummary:        func(p Provider) bool { _, ok := p.(SummaryProvider); return ok },
}

// capabilityParents lists capabilities that require another capability.
var capabilityParents = map[Capability]Capability{
	CapRepoCreate: CapRepositories, CapRepoDelete: CapRepositories, CapRepoFork: CapRepositories,
	CapRepoRename: CapRepositories, CapRepoArchive: CapRepositories,
	CapPRCreate: CapPullRequests, CapPRMerge: CapPullRequests, CapPRClose: CapPullRequests,
	CapIssueCreate: CapIssues, CapIssueState: CapIssues, CapIssueLabels: CapIssues,
	CapPipelineRun: CapPipelines, CapPipelineCancel: CapPipelines, CapPipelineRetry: CapPipelines,
	CapPipelineLogs: CapPipelines, CapReleaseCreate: CapReleases, CapReleaseDelete: CapReleases,
	CapSnippetCreate: CapSnippets,
}

// ValidateCapabilities checks that every declared capability is implemented
// and that sub-capabilities have their parent. It returns all problems.
func ValidateCapabilities(p Provider) []error {
	var problems []error
	caps := p.Capabilities()
	for c, on := range caps {
		if !on {
			continue
		}
		if !c.IsKnown() {
			problems = append(problems, fmt.Errorf("unknown capability %q", c))
			continue
		}
		if check := capabilityInterfaces[c]; check != nil && !check(p) {
			problems = append(problems, fmt.Errorf("capability %q declared but interface not implemented", c))
		}
		if parent, ok := capabilityParents[c]; ok && !caps.Has(parent) {
			problems = append(problems, fmt.Errorf("capability %q requires %q", c, parent))
		}
	}
	return problems
}

// CloneURL returns the clone URL for a repository using the given protocol.
// It uses already-known URLs when available and falls back to the provider.
func CloneURL(ctx context.Context, p Provider, repo domain.Repository, protocol domain.GitProtocol) (string, error) {
	urls := repo.URLs
	if urls.HTTPS == "" && urls.SSH == "" {
		rp, err := As[RepositoryProvider](p, CapRepositories)
		if err != nil {
			return "", err
		}
		urls = rp.RepositoryURLs(repo.Ref())
	}
	switch protocol {
	case domain.ProtocolSSH:
		if urls.SSH == "" {
			return "", errs.Unsupported(p.Metadata().Name, "SSH clone URLs")
		}
		return urls.SSH, nil
	default:
		if urls.HTTPS == "" {
			return "", errs.Unsupported(p.Metadata().Name, "HTTPS clone URLs")
		}
		return urls.HTTPS, nil
	}
}

// Collect is the application-layer pagination normalizer. Each driver
// supplies fetch, which retrieves one page using the provider's own mechanism
// (Link headers, page numbers, cursors, "next" URLs...) and returns the items
// plus an opaque cursor for the next page ("" when done). Collect stops at
// limit (<=0 means no limit) and honors context cancellation.
func Collect[T any](ctx context.Context, limit int, fetch func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		if next == "" || len(items) == 0 || seen[next] {
			return out, nil
		}
		seen[next] = true
		cursor = next
	}
}

// PageSize returns a sensible per-page size for a limit given the provider's
// maximum page size.
func PageSize(limit, max int) int {
	if limit > 0 && limit < max {
		return limit
	}
	return max
}
