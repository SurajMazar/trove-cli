package git

import (
	"sort"
	"strings"
)

// Account is a configured provider account used for remote detection.
type Account struct {
	Alias string
	Type  string
	// Hosts are all hostnames associated with the account: host, ssh_host and
	// the hosts of api/web/clone base URLs.
	Hosts []string
}

// Detection is the result of identifying the forge behind a remote.
type Detection struct {
	Remote     string   `json:"remote,omitempty"` // remote name, e.g. "origin"
	URL        string   `json:"url"`
	Host       string   `json:"host"`
	Type       string   `json:"type,omitempty"`     // driver type; "" when unknown
	Provider   string   `json:"provider,omitempty"` // configured alias; "" when none matches
	Candidates []string `json:"candidates,omitempty"`
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	// Source explains how the type was determined: "config", "known-host",
	// "heuristic" or "unknown".
	Source string `json:"source"`
}

// FullName returns namespace/name.
func (d Detection) FullName() string { return d.Namespace + "/" + d.Name }

// wellKnownHosts are public SaaS forges.
var wellKnownHosts = map[string]string{
	"github.com":              "github",
	"ssh.github.com":          "github",
	"gitlab.com":              "gitlab",
	"altssh.gitlab.com":       "gitlab",
	"bitbucket.org":           "bitbucket",
	"altssh.bitbucket.org":    "bitbucket",
	"ssh.bitbucket.org":       "bitbucket",
	"dev.azure.com":           "azure-devops",
	"ssh.dev.azure.com":       "azure-devops",
	"vs-ssh.visualstudio.com": "azure-devops",
	"codeberg.org":            "forgejo",
	"gitea.com":               "gitea",
	"git.sr.ht":               "sourcehut",
}

// heuristics map hostname fragments of self-hosted instances to types.
// Self-hosted Bitbucket is always Server/Data Center.
var heuristics = []struct{ fragment, typ string }{
	{"github", "github"},
	{"gitlab", "gitlab"},
	{"bitbucket", "bitbucket-server"},
	{"stash", "bitbucket-server"},
	{"forgejo", "forgejo"},
	{"gitea", "gitea"},
	{"visualstudio.com", "azure-devops"},
}

// Detect identifies provider, host, namespace and repository for a remote
// URL. Configured accounts win over well-known hosts, which win over
// heuristics. When several accounts share a host, defaultAlias is preferred
// and all matches are listed as candidates.
func Detect(raw string, accounts []Account, defaultAlias string) (Detection, error) {
	r, err := ParseRemoteURL(raw)
	if err != nil {
		return Detection{}, err
	}
	d := Detection{URL: r.Raw, Host: r.Host, Source: "unknown"}

	var matches []Account
	for _, a := range accounts {
		for _, h := range a.Hosts {
			if strings.EqualFold(hostOnly(h), r.Host) {
				matches = append(matches, a)
				break
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Alias < matches[j].Alias })
	switch {
	case len(matches) > 0:
		pick := matches[0]
		for _, m := range matches {
			if m.Alias == defaultAlias {
				pick = m
			}
		}
		d.Type, d.Provider, d.Source = pick.Type, pick.Alias, "config"
		if len(matches) > 1 {
			for _, m := range matches {
				d.Candidates = append(d.Candidates, m.Alias)
			}
		}
	case wellKnownHosts[r.Host] != "":
		d.Type, d.Source = wellKnownHosts[r.Host], "known-host"
	default:
		for _, h := range heuristics {
			if strings.Contains(r.Host, h.fragment) {
				d.Type, d.Source = h.typ, "heuristic"
				break
			}
		}
	}
	d.Namespace, d.Name = RepoPath(r, d.Type)
	return d, nil
}

func hostOnly(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/"); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			return h[1:i]
		}
	}
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return h
}
