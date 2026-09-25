package custom

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/forge"
)

// fileConfig mirrors a provider block in Trove's configuration file. The
// top-level fields normally also arrive pre-parsed in forge.Account; they are
// decoded here as a fallback so the driver works with either source.
type fileConfig struct {
	Host         string       `yaml:"host" json:"host"`
	APIBaseURL   string       `yaml:"api_base_url" json:"api_base_url"`
	CloneBaseURL string       `yaml:"clone_base_url" json:"clone_base_url"`
	WebBaseURL   string       `yaml:"web_base_url" json:"web_base_url"`
	SSHHost      string       `yaml:"ssh_host" json:"ssh_host"`
	Custom       customConfig `yaml:"custom" json:"custom"`
}

// customConfig is the `custom:` block: everything that describes how the
// Trove Forge Protocol endpoint behaves.
type customConfig struct {
	DisplayName  string           `yaml:"display_name" json:"display_name"`
	Capabilities []string         `yaml:"capabilities" json:"capabilities"`
	GitUsername  string           `yaml:"git_username" json:"git_username"`
	Auth         authConfig       `yaml:"auth" json:"auth"`
	Pagination   paginationConfig `yaml:"pagination" json:"pagination"`
	Clone        cloneConfig      `yaml:"clone" json:"clone"`
	Terms        termsConfig      `yaml:"terms" json:"terms"`
}

type authConfig struct {
	// Header is the request header carrying the credential.
	Header string `yaml:"header" json:"header"`
	// Scheme is the value prefix. nil means "Bearer"; "" sends the raw token;
	// "basic" sends HTTP Basic credentials built from Username and the token.
	Scheme   *string `yaml:"scheme" json:"scheme"`
	Username string  `yaml:"username" json:"username"`
}

type paginationConfig struct {
	Strategy     string `yaml:"strategy" json:"strategy"`
	PageParam    string `yaml:"page_param" json:"page_param"`
	PerPageParam string `yaml:"per_page_param" json:"per_page_param"`
	CursorParam  string `yaml:"cursor_param" json:"cursor_param"`
	MaxPageSize  int    `yaml:"max_page_size" json:"max_page_size"`
}

type cloneConfig struct {
	Strategy string `yaml:"strategy" json:"strategy"`
	HTTPS    string `yaml:"https" json:"https"`
	SSH      string `yaml:"ssh" json:"ssh"`
	Web      string `yaml:"web" json:"web"`
}

type termsConfig struct {
	Repository       string `yaml:"repository" json:"repository"`
	PullRequest      string `yaml:"pull_request" json:"pull_request"`
	PullRequestShort string `yaml:"pull_request_short" json:"pull_request_short"`
	Pipeline         string `yaml:"pipeline" json:"pipeline"`
	Snippet          string `yaml:"snippet" json:"snippet"`
	Notification     string `yaml:"notification" json:"notification"`
	Namespace        string `yaml:"namespace" json:"namespace"`
}

// Pagination strategies understood by the driver.
const (
	paginateLink   = "link"
	paginatePage   = "page"
	paginateCursor = "cursor"
	paginateNone   = "none"
)

// Clone URL strategies.
const (
	cloneTemplate = "template"
	cloneAPI      = "api"
)

// Defaults applied when the configuration leaves a value out.
const (
	defaultHeader       = "Authorization"
	defaultScheme       = "Bearer"
	defaultGitUsername  = "trove"
	defaultMaxPageSize  = 100
	defaultHTTPSPattern = "{clone_base_url}/{namespace}/{name}.git"
	defaultSSHPattern   = "git@{ssh_host}:{namespace}/{name}.git"
	defaultWebPattern   = "{web_base_url}/{namespace}/{name}"
)

// tfpCapabilities are the capabilities Trove Forge Protocol v1 defines
// endpoints for. Anything else is rejected at configuration time, even if
// Trove knows the capability from another driver.
var tfpCapabilities = []forge.Capability{
	forge.CapRepositories, forge.CapRepoCreate, forge.CapRepoDelete,
	forge.CapPullRequests, forge.CapPRCreate, forge.CapPRMerge, forge.CapPRClose,
	forge.CapIssues, forge.CapIssueCreate, forge.CapIssueState, forge.CapIssueLabels,
	forge.CapPipelines, forge.CapReleases, forge.CapNamespaces, forge.CapSummary,
}

func isTFPCapability(c forge.Capability) bool {
	for _, k := range tfpCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// placeholders usable in clone/web templates.
var knownPlaceholders = map[string]bool{
	"namespace": true, "name": true, "full_name": true,
	"clone_base_url": true, "web_base_url": true, "ssh_host": true, "host": true,
}

// settings is the validated, defaulted configuration a Provider runs with.
type settings struct {
	displayName  string
	host         string
	apiBase      *url.URL // no trailing slash on Path
	apiBaseStr   string
	cloneBaseURL string // may be empty when no template needs it
	webBaseURL   string
	sshHost      string
	caps         forge.Capabilities

	authHeader  string
	authScheme  string // "" = raw token; "basic" handled via basic
	basic       bool
	username    string // basic-auth username from configuration
	gitUsername string // explicit custom.git_username, may be empty

	pagination paginationConfig
	clone      cloneConfig
	terms      forge.Terms
}

// buildSettings merges the account and decoded config, applies defaults and
// validates everything. It returns every problem at once so operators can
// fix a configuration in one pass.
func buildSettings(acct forge.Account, fc fileConfig) (*settings, []string) {
	var problems []string
	c := fc.Custom
	s := &settings{}

	apiRaw := firstNonEmpty(acct.APIURL, fc.APIBaseURL)
	if apiRaw == "" {
		problems = append(problems, "api_base_url is required")
	} else if u, err := parseHTTPURL(apiRaw); err != nil {
		problems = append(problems, fmt.Sprintf("api_base_url %q: %v", apiRaw, err))
	} else {
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
		s.apiBase = u
		s.apiBaseStr = u.String()
	}

	s.host = firstNonEmpty(acct.Host, fc.Host)
	if s.host == "" && s.apiBase != nil {
		s.host = s.apiBase.Host
	}

	s.cloneBaseURL = strings.TrimRight(firstNonEmpty(acct.CloneBaseURL, fc.CloneBaseURL), "/")
	if s.cloneBaseURL != "" {
		if _, err := parseHTTPURL(s.cloneBaseURL); err != nil {
			problems = append(problems, fmt.Sprintf("clone_base_url %q: %v", s.cloneBaseURL, err))
		}
	}
	s.webBaseURL = strings.TrimRight(firstNonEmpty(acct.WebURL, fc.WebBaseURL), "/")
	if s.webBaseURL == "" && s.host != "" {
		s.webBaseURL = "https://" + s.host
	}
	if s.webBaseURL != "" {
		if _, err := parseHTTPURL(s.webBaseURL); err != nil {
			problems = append(problems, fmt.Sprintf("web_base_url %q: %v", s.webBaseURL, err))
		}
	}
	s.sshHost = firstNonEmpty(acct.SSHHost, fc.SSHHost)
	if s.sshHost == "" {
		s.sshHost = hostWithoutPort(s.host)
	}

	s.displayName = strings.TrimSpace(c.DisplayName)
	if s.displayName == "" {
		s.displayName = "Custom Forge"
	}

	problems = append(problems, s.setCapabilities(c.Capabilities)...)
	problems = append(problems, s.setAuth(acct, c)...)
	problems = append(problems, s.setPagination(c.Pagination)...)
	problems = append(problems, s.setClone(c.Clone)...)
	s.setTerms(c.Terms)
	return s, problems
}

func (s *settings) setCapabilities(list []string) []string {
	var problems []string
	if len(list) == 0 {
		// A forge adapter that exposes nothing but the user endpoint is not
		// useful to Trove; default to read-only repository access.
		list = []string{string(forge.CapRepositories)}
	}
	s.caps = forge.Capabilities{}
	for _, raw := range list {
		c := forge.Capability(strings.ToLower(strings.TrimSpace(raw)))
		switch {
		case !c.IsKnown():
			problems = append(problems, fmt.Sprintf("capabilities: unknown capability %q", raw))
		case !isTFPCapability(c):
			problems = append(problems, fmt.Sprintf("capabilities: %q is not supported by Trove Forge Protocol v1", raw))
		default:
			s.caps[c] = true
		}
	}
	for c := range s.caps {
		if parent, _, ok := strings.Cut(string(c), "."); ok && !s.caps.Has(forge.Capability(parent)) {
			problems = append(problems, fmt.Sprintf("capabilities: %q requires %q", c, parent))
		}
	}
	return problems
}

func (s *settings) setAuth(acct forge.Account, c customConfig) []string {
	var problems []string
	s.authHeader = strings.TrimSpace(c.Auth.Header)
	if s.authHeader == "" {
		s.authHeader = defaultHeader
	}
	if !validHeaderName(s.authHeader) {
		problems = append(problems, fmt.Sprintf("auth.header %q is not a valid HTTP header name", s.authHeader))
	}
	scheme := defaultScheme
	if c.Auth.Scheme != nil {
		scheme = strings.TrimSpace(*c.Auth.Scheme)
	}
	if strings.EqualFold(scheme, "basic") {
		s.basic = true
		scheme = ""
	} else if strings.ContainsAny(scheme, " \t\r\n") {
		problems = append(problems, fmt.Sprintf("auth.scheme %q must be a single word", scheme))
	}
	s.authScheme = scheme
	s.username = firstNonEmpty(strings.TrimSpace(c.Auth.Username), acct.Username)
	s.gitUsername = strings.TrimSpace(c.GitUsername)
	return problems
}

func (s *settings) setPagination(p paginationConfig) []string {
	var problems []string
	p.Strategy = strings.ToLower(strings.TrimSpace(p.Strategy))
	switch p.Strategy {
	case "":
		p.Strategy = paginateLink
	case paginateLink, paginatePage, paginateCursor, paginateNone:
	default:
		problems = append(problems, fmt.Sprintf("pagination.strategy %q is invalid (want link, page, cursor or none)", p.Strategy))
	}
	p.PageParam = firstNonEmpty(strings.TrimSpace(p.PageParam), "page")
	p.PerPageParam = firstNonEmpty(strings.TrimSpace(p.PerPageParam), "per_page")
	p.CursorParam = firstNonEmpty(strings.TrimSpace(p.CursorParam), "cursor")
	switch {
	case p.MaxPageSize == 0:
		p.MaxPageSize = defaultMaxPageSize
	case p.MaxPageSize < 0:
		problems = append(problems, fmt.Sprintf("pagination.max_page_size %d must be positive", p.MaxPageSize))
	}
	s.pagination = p
	return problems
}

func (s *settings) setClone(c cloneConfig) []string {
	var problems []string
	c.Strategy = strings.ToLower(strings.TrimSpace(c.Strategy))
	switch c.Strategy {
	case "":
		c.Strategy = cloneTemplate
	case cloneTemplate, cloneAPI:
	default:
		problems = append(problems, fmt.Sprintf("clone.strategy %q is invalid (want template or api)", c.Strategy))
	}
	c.HTTPS = firstNonEmpty(strings.TrimSpace(c.HTTPS), defaultHTTPSPattern)
	c.SSH = firstNonEmpty(strings.TrimSpace(c.SSH), defaultSSHPattern)
	c.Web = firstNonEmpty(strings.TrimSpace(c.Web), defaultWebPattern)
	for _, t := range []struct{ key, tmpl string }{{"clone.https", c.HTTPS}, {"clone.ssh", c.SSH}, {"clone.web", c.Web}} {
		names, err := templatePlaceholders(t.tmpl)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s template %q: %v", t.key, t.tmpl, err))
			continue
		}
		for _, n := range names {
			if n == "clone_base_url" && s.cloneBaseURL == "" && c.Strategy == cloneTemplate {
				problems = append(problems, fmt.Sprintf("%s template uses {clone_base_url}: clone_base_url is required for the template clone strategy", t.key))
			}
		}
	}
	s.clone = c
	if len(problems) == 0 && c.Strategy == cloneTemplate {
		// Render with sample values so obviously broken templates (e.g. an
		// HTTPS template that does not produce an http(s) URL) fail early.
		sample := s.render(c.HTTPS, "ns", "repo")
		if !isHTTPURL(sample) {
			problems = append(problems, fmt.Sprintf("clone.https template %q does not produce an http(s) URL", c.HTTPS))
		}
		if sample := s.render(c.Web, "ns", "repo"); !isHTTPURL(sample) {
			problems = append(problems, fmt.Sprintf("clone.web template %q does not produce an http(s) URL", c.Web))
		}
	}
	return problems
}

func (s *settings) setTerms(t termsConfig) {
	s.terms = forge.Terms{
		Repository:       firstNonEmpty(t.Repository, "Repository"),
		PullRequest:      firstNonEmpty(t.PullRequest, "Pull Request"),
		PullRequestShort: firstNonEmpty(t.PullRequestShort, "PR"),
		Pipeline:         firstNonEmpty(t.Pipeline, "Pipeline"),
		Snippet:          firstNonEmpty(t.Snippet, "Snippet"),
		Notification:     firstNonEmpty(t.Notification, "Notification"),
		Namespace:        firstNonEmpty(t.Namespace, "Namespace"),
	}
}

// templatePlaceholders returns the {placeholder} names used in tmpl, failing
// on unknown names or unbalanced braces.
func templatePlaceholders(tmpl string) ([]string, error) {
	var names []string
	rest := tmpl
	for {
		open := strings.IndexByte(rest, '{')
		closeIdx := strings.IndexByte(rest, '}')
		if open < 0 {
			if closeIdx >= 0 {
				return nil, fmt.Errorf("unbalanced '}'")
			}
			return names, nil
		}
		if closeIdx >= 0 && closeIdx < open {
			return nil, fmt.Errorf("unbalanced '}'")
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unbalanced '{'")
		}
		name := rest[open+1 : open+end]
		if !knownPlaceholders[name] {
			return nil, fmt.Errorf("unknown placeholder {%s} (known: {namespace}, {name}, {full_name}, {clone_base_url}, {web_base_url}, {ssh_host}, {host})", name)
		}
		names = append(names, name)
		rest = rest[open+end+1:]
	}
}

// render substitutes placeholders. Values are inserted verbatim: namespaces
// keep their '/' separators, matching how forges lay out clone URLs.
func (s *settings) render(tmpl, namespace, name string) string {
	full := name
	if namespace != "" {
		full = namespace + "/" + name
	}
	r := strings.NewReplacer(
		"{namespace}", namespace,
		"{name}", name,
		"{full_name}", full,
		"{clone_base_url}", s.cloneBaseURL,
		"{web_base_url}", s.webBaseURL,
		"{ssh_host}", s.sshHost,
		"{host}", s.host,
	)
	return r.Replace(tmpl)
}

// canRender reports whether every placeholder in tmpl has a value.
func (s *settings) canRender(tmpl string) bool {
	names, err := templatePlaceholders(tmpl)
	if err != nil {
		return false
	}
	for _, n := range names {
		if n == "clone_base_url" && s.cloneBaseURL == "" {
			return false
		}
	}
	return true
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a valid URL")
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.User != nil {
		return nil, fmt.Errorf("must not embed credentials")
	}
	return u, nil
}

func isHTTPURL(raw string) bool {
	_, err := parseHTTPURL(raw)
	return err == nil
}

// validHeaderName checks the RFC 9110 token grammar.
func validHeaderName(h string) bool {
	if h == "" {
		return false
	}
	for _, r := range h {
		if r > 0x7e || r <= ' ' || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r) {
			return false
		}
	}
	return true
}

func hostWithoutPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
