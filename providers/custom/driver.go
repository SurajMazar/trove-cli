// Package custom implements the "custom" forge driver.
//
// A custom provider does not emulate GitHub, GitLab or Bitbucket. It speaks
// the Trove Forge Protocol v1 (TFP): a small REST+JSON contract whose payloads
// are Trove's own domain models (see internal/domain). A company exposes TFP,
// usually through a thin adapter in front of an in-house forge, and describes
// it declaratively in Trove's configuration: which capabilities exist, how to
// authenticate, how pagination works and how clone URLs are built.
//
// The protocol is specified in docs/custom-providers.md.
package custom

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// Type is the driver type used in configuration files.
const Type = "custom"

type driver struct{}

// NewDriver returns the custom (Trove Forge Protocol) driver.
func NewDriver() forge.Driver { return driver{} }

func (driver) Type() string        { return Type }
func (driver) DisplayName() string { return "Custom Forge" }
func (driver) Description() string {
	return "Any forge exposing the Trove Forge Protocol v1 (REST+JSON), configured declaratively"
}

// DefaultHost is empty: a custom forge has no canonical host.
func (driver) DefaultHost() string { return "" }

func (driver) AuthMethods() []auth.Method {
	return []auth.Method{auth.MethodToken, auth.MethodBasic}
}

// New validates the configuration and returns a provider. It never touches
// the network or loads credentials.
func (driver) New(acct forge.Account) (forge.Provider, error) {
	var fc fileConfig
	if acct.Decode != nil {
		if err := acct.Decode(&fc); err != nil {
			return nil, &errs.Error{
				Kind: errs.ErrInvalidConfiguration, Provider: acct.Name, Cause: err,
				Message: "custom provider configuration could not be decoded",
			}
		}
	}
	s, problems := buildSettings(acct, fc)
	if len(problems) > 0 {
		return nil, &errs.Error{
			Kind:     errs.ErrInvalidConfiguration,
			Provider: acct.Name,
			Message:  "invalid custom provider configuration: " + strings.Join(problems, "; "),
			Hint:     "see docs/custom-providers.md for the configuration reference",
		}
	}

	logger := acct.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := &Provider{acct: acct, s: s, logger: logger}
	base := acct.Transport
	// The credential is attached by authTransport, which sits *below* the
	// logging and retry transports: debug logs never see the (possibly
	// non-standard, hence not auto-redacted) auth header, and the header is
	// only ever sent to the api_base_url origin, even across redirects.
	p.http = httpx.NewClient(httpx.Options{
		Base:   &authTransport{base: base, origin: originOf(s.apiBase)},
		Logger: acct.Logger,
		Retry:  httpx.RetryPolicy{RateLimited: rateLimited},
	})
	return p, nil
}

// Provider is a forge provider speaking the Trove Forge Protocol v1.
//
// Go interfaces are static, so Provider implements every TFP capability
// interface; Capabilities returns exactly the configured set and every
// capability method refuses to run when its capability is not declared.
// forge.As checks the declared set first, so undeclared capabilities are
// reported as unsupported without any request being made.
type Provider struct {
	acct   forge.Account
	s      *settings
	logger *slog.Logger
	http   *http.Client

	mu   sync.Mutex
	cred *resolvedCredential
}

// Metadata implements forge.Provider.
func (p *Provider) Metadata() forge.Metadata {
	methods := []auth.Method{auth.MethodToken}
	if p.s.basic {
		methods = append(methods, auth.MethodBasic)
	}
	return forge.Metadata{
		Name:        p.acct.Name,
		Type:        Type,
		DisplayName: p.s.displayName,
		Host:        p.s.host,
		APIURL:      p.s.apiBaseStr,
		WebURL:      p.s.webBaseURL,
		Deployment:  "custom",
		AuthMethods: methods,
		Terms:       p.s.terms,
	}
}

// Capabilities implements forge.Provider. It returns a copy of the
// configured set.
func (p *Provider) Capabilities() forge.Capabilities {
	out := make(forge.Capabilities, len(p.s.caps))
	for c, v := range p.s.caps {
		out[c] = v
	}
	return out
}

// require returns ErrUnsupportedCapability unless c was configured. It
// guards direct calls that bypass forge.As.
func (p *Provider) require(c forge.Capability) error {
	if p.s.caps.Has(c) {
		return nil
	}
	name := p.acct.Name
	if name == "" {
		name = Type
	}
	return errs.Unsupported(name, forge.FeatureName(c))
}

// Compile-time checks: Provider implements every TFP v1 capability.
var (
	_ forge.Provider            = (*Provider)(nil)
	_ forge.Authenticator       = (*Provider)(nil)
	_ forge.GitAuthenticator    = (*Provider)(nil)
	_ forge.RepositoryProvider  = (*Provider)(nil)
	_ forge.RepositoryCreator   = (*Provider)(nil)
	_ forge.RepositoryDeleter   = (*Provider)(nil)
	_ forge.PullRequestProvider = (*Provider)(nil)
	_ forge.PullRequestCreator  = (*Provider)(nil)
	_ forge.PullRequestMerger   = (*Provider)(nil)
	_ forge.PullRequestCloser   = (*Provider)(nil)
	_ forge.IssueProvider       = (*Provider)(nil)
	_ forge.IssueCreator        = (*Provider)(nil)
	_ forge.IssueStateChanger   = (*Provider)(nil)
	_ forge.PipelineProvider    = (*Provider)(nil)
	_ forge.ReleaseProvider     = (*Provider)(nil)
	_ forge.NamespaceProvider   = (*Provider)(nil)
	_ forge.SummaryProvider     = (*Provider)(nil)
)
