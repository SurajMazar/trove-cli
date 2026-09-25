package forge

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

type stubProvider struct{ caps Capabilities }

func (s stubProvider) Metadata() Metadata         { return Metadata{Name: "stub", Type: "stub"} }
func (s stubProvider) Capabilities() Capabilities { return s.caps }
func (s stubProvider) CurrentUser(context.Context) (*domain.User, error) {
	return &domain.User{Username: "u"}, nil
}

type stubRepos struct{ stubProvider }

func (stubRepos) ListRepositories(context.Context, ListRepositoryOptions) ([]domain.Repository, error) {
	return nil, nil
}
func (stubRepos) GetRepository(context.Context, domain.RepositoryRef) (*domain.Repository, error) {
	return nil, nil
}
func (stubRepos) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	return domain.RepositoryURLs{HTTPS: "https://x/" + ref.FullName() + ".git", SSH: "git@x:" + ref.FullName() + ".git"}
}

type stubDriver struct{ typ string }

func (d stubDriver) Type() string                  { return d.typ }
func (d stubDriver) DisplayName() string           { return d.typ }
func (d stubDriver) Description() string           { return "" }
func (d stubDriver) DefaultHost() string           { return "" }
func (d stubDriver) AuthMethods() []auth.Method    { return []auth.Method{auth.MethodToken} }
func (d stubDriver) New(Account) (Provider, error) { return stubProvider{}, nil }

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubDriver{"b"}); err != nil {
		t.Fatal(err)
	}
	r.MustRegister(stubDriver{"a"})
	if err := r.Register(stubDriver{"a"}); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, err := r.Get("zzz"); !errors.Is(err, errs.ErrDriverNotFound) {
		t.Fatalf("Get unknown: %v", err)
	}
	list := r.List()
	if len(list) != 2 || list[0].Type() != "a" {
		t.Fatalf("List not sorted: %v", list)
	}
}

func TestAsChecksDeclarationAndImplementation(t *testing.T) {
	// Declared and implemented.
	p := stubRepos{stubProvider{caps: NewCapabilities(CapRepositories)}}
	if _, err := As[RepositoryProvider](p, CapRepositories); err != nil {
		t.Fatal(err)
	}
	// Implemented but not declared → unsupported (never emulate).
	p2 := stubRepos{stubProvider{caps: NewCapabilities()}}
	_, err := As[RepositoryProvider](p2, CapRepositories)
	if !errors.Is(err, errs.ErrUnsupportedCapability) || err.Error() != `provider "stub" does not support repositories` {
		t.Fatalf("got %v", err)
	}
	// Declared but not implemented → unsupported, no panic.
	p3 := stubProvider{caps: NewCapabilities(CapPipelines)}
	if _, err := As[PipelineProvider](p3, CapPipelines); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Fatalf("got %v", err)
	}
	if problems := ValidateCapabilities(p3); len(problems) != 1 {
		t.Fatalf("ValidateCapabilities = %v", problems)
	}
}

func TestValidateCapabilitiesParentAndUnknown(t *testing.T) {
	p := stubRepos{stubProvider{caps: Capabilities{CapRepositories: true, CapPRCreate: true, "teleport": true}}}
	problems := ValidateCapabilities(p)
	if len(problems) != 3 { // PR create lacks parent + not implemented, unknown capability
		t.Fatalf("problems = %v", problems)
	}
}

func TestCapabilitiesList(t *testing.T) {
	c := NewCapabilities(CapSearchCode, CapRepositories, "zzz-future")
	got := c.List()
	if len(got) != 3 || got[0] != CapRepositories || got[1] != CapSearchCode || got[2] != "zzz-future" {
		t.Fatalf("List = %v", got)
	}
}

func TestCollectPaginationAndLimit(t *testing.T) {
	pages := map[string][]int{"": {1, 2, 3}, "p2": {4, 5, 6}, "p3": {7}}
	next := map[string]string{"": "p2", "p2": "p3", "p3": ""}
	calls := 0
	fetch := func(ctx context.Context, cursor string) ([]int, string, error) {
		calls++
		return pages[cursor], next[cursor], nil
	}
	all, err := Collect(context.Background(), 0, fetch)
	if err != nil || len(all) != 7 || calls != 3 {
		t.Fatalf("Collect all = %v (%d calls), %v", all, calls, err)
	}
	calls = 0
	some, _ := Collect(context.Background(), 4, fetch)
	if len(some) != 4 || calls != 2 {
		t.Fatalf("Collect limit = %v (%d calls)", some, calls)
	}
	// Cursor loops terminate.
	loop := func(ctx context.Context, cursor string) ([]int, string, error) { return []int{1}, "same", nil }
	if got, _ := Collect(context.Background(), 0, loop); len(got) != 2 {
		t.Fatalf("loop guard failed: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx, 0, fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	boom := errors.New("boom")
	if _, err := Collect(context.Background(), 0, func(context.Context, string) ([]int, string, error) { return nil, "", boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

func TestCloneURL(t *testing.T) {
	p := stubRepos{stubProvider{caps: NewCapabilities(CapRepositories)}}
	r := domain.Repository{Name: "r", Namespace: "o"}
	if u, err := CloneURL(context.Background(), p, r, domain.ProtocolSSH); err != nil || u != "git@x:o/r.git" {
		t.Fatalf("ssh = %q %v", u, err)
	}
	r.URLs.HTTPS = "https://known/o/r.git"
	if u, _ := CloneURL(context.Background(), p, r, domain.ProtocolHTTPS); u != "https://known/o/r.git" {
		t.Fatalf("known URL not preferred: %s", u)
	}
	if _, err := CloneURL(context.Background(), p, r, domain.ProtocolSSH); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Fatalf("missing SSH URL: %v", err)
	}
}

func TestPageSize(t *testing.T) {
	for _, c := range []struct{ limit, max, want int }{{0, 100, 100}, {10, 100, 10}, {500, 100, 100}} {
		if got := PageSize(c.limit, c.max); got != c.want {
			t.Errorf("PageSize(%d,%d) = %d", c.limit, c.max, got)
		}
	}
	_ = strconv.Itoa
}
