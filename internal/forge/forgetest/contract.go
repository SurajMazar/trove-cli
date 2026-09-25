// Package forgetest provides a reusable contract test suite that every forge
// provider (built-in or custom) should pass. Drivers run it against a fake
// API server built with httptest.Server.
package forgetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// Options describe the fixture data served by the fake backing the provider.
type Options struct {
	// ExistingRepo must exist on the fake server.
	ExistingRepo domain.RepositoryRef
	// MissingRepo must not exist on the fake server.
	MissingRepo domain.RepositoryRef
	// MinRepositories is the number of repositories the fake serves in total.
	// Use more than one page's worth to exercise pagination.
	MinRepositories int
	// Secret is the credential configured on Provider; the suite asserts it
	// never appears in error messages.
	Secret string
	// Unauthenticated is the same provider configured with an invalid
	// credential; CurrentUser must fail with ErrAuthenticationFailed.
	Unauthenticated forge.Provider
}

// RunForgeProviderContractTests verifies provider invariants.
func RunForgeProviderContractTests(t *testing.T, p forge.Provider, o Options) {
	t.Helper()
	ctx := context.Background()

	t.Run("metadata", func(t *testing.T) {
		m := p.Metadata()
		for field, v := range map[string]string{"Name": m.Name, "Type": m.Type, "DisplayName": m.DisplayName, "Host": m.Host, "APIURL": m.APIURL, "WebURL": m.WebURL} {
			if strings.TrimSpace(v) == "" {
				t.Errorf("metadata %s is empty", field)
			}
		}
		if len(m.AuthMethods) == 0 {
			t.Error("metadata declares no auth methods")
		}
		if p.Capabilities().Has(forge.CapPullRequests) && m.Terms.PullRequest == "" {
			t.Error("provider supports pull requests but Terms.PullRequest is empty")
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		for _, problem := range forge.ValidateCapabilities(p) {
			t.Error(problem)
		}
		for _, c := range forge.AllCapabilities {
			if p.Capabilities().Has(c) {
				continue
			}
			_, err := forge.As[forge.RepositoryProvider](p, c)
			if !errors.Is(err, errs.ErrUnsupportedCapability) {
				t.Errorf("undeclared capability %q: want ErrUnsupportedCapability, got %v", c, err)
			}
		}
	})

	t.Run("current user", func(t *testing.T) {
		u, err := p.CurrentUser(ctx)
		if err != nil {
			t.Fatalf("CurrentUser: %v", err)
		}
		if u == nil || (u.Username == "" && u.ID == "") {
			t.Fatalf("CurrentUser returned an empty user: %+v", u)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := p.CurrentUser(cctx)
		if err == nil {
			t.Fatal("CurrentUser with canceled context succeeded")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	})

	if o.Unauthenticated != nil {
		t.Run("authentication errors are normalized", func(t *testing.T) {
			_, err := o.Unauthenticated.CurrentUser(ctx)
			if !errors.Is(err, errs.ErrAuthenticationFailed) {
				t.Fatalf("want ErrAuthenticationFailed, got %v", err)
			}
			assertNoSecret(t, err, o.Secret)
		})
	}

	if !p.Capabilities().Has(forge.CapRepositories) {
		return
	}
	rp, err := forge.As[forge.RepositoryProvider](p, forge.CapRepositories)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("list repositories (pagination)", func(t *testing.T) {
		repos, err := rp.ListRepositories(ctx, forge.ListRepositoryOptions{IncludeArchived: true})
		if err != nil {
			t.Fatalf("ListRepositories: %v", err)
		}
		if len(repos) < o.MinRepositories {
			t.Fatalf("got %d repositories, want at least %d", len(repos), o.MinRepositories)
		}
		seen := map[string]bool{}
		for _, r := range repos {
			ValidateRepository(t, p, r)
			if seen[r.FullName] {
				t.Errorf("duplicate repository %q (pagination bug?)", r.FullName)
			}
			seen[r.FullName] = true
		}
	})

	t.Run("list repositories honors limit", func(t *testing.T) {
		repos, err := rp.ListRepositories(ctx, forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: 1}, IncludeArchived: true})
		if err != nil {
			t.Fatalf("ListRepositories: %v", err)
		}
		if len(repos) != 1 {
			t.Fatalf("limit 1 returned %d repositories", len(repos))
		}
	})

	t.Run("get repository", func(t *testing.T) {
		r, err := rp.GetRepository(ctx, o.ExistingRepo)
		if err != nil {
			t.Fatalf("GetRepository(%s): %v", o.ExistingRepo, err)
		}
		ValidateRepository(t, p, *r)
		if !strings.EqualFold(r.FullName, o.ExistingRepo.FullName()) {
			t.Errorf("FullName = %q, want %q", r.FullName, o.ExistingRepo.FullName())
		}
	})

	t.Run("missing repository is ErrNotFound", func(t *testing.T) {
		_, err := rp.GetRepository(ctx, o.MissingRepo)
		if err == nil {
			t.Fatal("GetRepository on missing repo succeeded")
		}
		if !errors.Is(err, errs.ErrNotFound) && !errors.Is(err, errs.ErrRepositoryNotFound) {
			t.Errorf("want ErrNotFound/ErrRepositoryNotFound, got %v", err)
		}
		assertNoSecret(t, err, o.Secret)
	})

	t.Run("repository URLs", func(t *testing.T) {
		u := rp.RepositoryURLs(o.ExistingRepo)
		if u.Web == "" || u.HTTPS == "" {
			t.Errorf("RepositoryURLs incomplete: %+v", u)
		}
		if u.HTTPS != "" && !strings.HasPrefix(u.HTTPS, "http") {
			t.Errorf("HTTPS clone URL %q is not http(s)", u.HTTPS)
		}
	})
}

// ValidateRepository checks invariants of a repository model.
func ValidateRepository(t *testing.T, p forge.Provider, r domain.Repository) {
	t.Helper()
	m := p.Metadata()
	if r.Name == "" {
		t.Errorf("repository has empty name: %+v", r)
	}
	if r.Namespace == "" {
		t.Errorf("repository %q has empty namespace", r.Name)
	}
	if r.FullName != r.Namespace+"/"+r.Name {
		t.Errorf("repository FullName %q != Namespace/Name %q/%q", r.FullName, r.Namespace, r.Name)
	}
	if r.Provider != m.Name {
		t.Errorf("repository %q Provider = %q, want %q", r.FullName, r.Provider, m.Name)
	}
	if r.ProviderType != m.Type {
		t.Errorf("repository %q ProviderType = %q, want %q", r.FullName, r.ProviderType, m.Type)
	}
	switch r.Visibility {
	case domain.VisibilityPublic, domain.VisibilityPrivate, domain.VisibilityInternal:
	default:
		t.Errorf("repository %q has invalid visibility %q", r.FullName, r.Visibility)
	}
	if r.URLs.Web == "" || r.URLs.HTTPS == "" {
		t.Errorf("repository %q has incomplete URLs: %+v", r.FullName, r.URLs)
	}
}

func assertNoSecret(t *testing.T, err error, secret string) {
	t.Helper()
	if err != nil && secret != "" && strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks the credential: %q", err.Error())
	}
}
