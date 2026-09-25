package git

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// RemoteURL is a parsed Git remote address.
type RemoteURL struct {
	Raw    string `json:"raw"`
	Scheme string `json:"scheme"` // https, http, ssh, git, file
	Host   string `json:"host"`   // lower-cased hostname without port
	Port   string `json:"port,omitempty"`
	User   string `json:"user,omitempty"` // never includes a password
	// Path is the repository path without leading slash or ".git" suffix,
	// e.g. "group/subgroup/project".
	Path string `json:"path"`
}

// Segments splits Path on '/'.
func (r RemoteURL) Segments() []string { return strings.Split(r.Path, "/") }

// scpLike matches "user@host:path" (no scheme, no slash before the colon).
var scpLike = regexp.MustCompile(`^(?:([^@/:]+)@)?([^@/:\[\]]+|\[[^\]]+\]):(.+)$`)

// ParseRemoteURL parses HTTPS, SSH (URL and scp-like), git:// and file
// remotes. It supports arbitrarily nested namespaces (GitLab subgroups).
func ParseRemoteURL(raw string) (*RemoteURL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty remote URL")
	}
	var r RemoteURL
	r.Raw = s
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid remote URL")
		}
		r.Scheme = strings.ToLower(u.Scheme)
		switch r.Scheme {
		case "git+ssh", "ssh+git":
			r.Scheme = "ssh"
		}
		r.Host = strings.ToLower(u.Hostname())
		r.Port = u.Port()
		if u.User != nil {
			r.User = u.User.Username()
		}
		r.Path = u.Path
		if r.Scheme == "file" {
			r.Path = strings.TrimSuffix(u.Path, "/")
			return &r, nil
		}
	} else if m := scpLike.FindStringSubmatch(s); m != nil && !looksLikeLocalPath(s) {
		r.Scheme = "ssh"
		r.User = m[1]
		r.Host = strings.ToLower(strings.Trim(m[2], "[]"))
		r.Path = m[3]
	} else {
		return nil, fmt.Errorf("unrecognized remote URL format")
	}
	// Never keep credentials from the raw URL.
	r.Raw = sanitize(s)
	r.Path = strings.Trim(r.Path, "/")
	r.Path = strings.TrimSuffix(r.Path, ".git")
	r.Path = strings.Trim(r.Path, "/")
	if r.Host == "" {
		return nil, fmt.Errorf("remote URL has no host")
	}
	if r.Path == "" || !strings.Contains(r.Path, "/") {
		return nil, fmt.Errorf("remote URL path %q does not contain a namespace and repository", r.Path)
	}
	for _, seg := range r.Segments() {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("remote URL path %q is invalid", r.Path)
		}
	}
	return &r, nil
}

func looksLikeLocalPath(s string) bool {
	// Windows drive letters ("C:\repo") and relative paths are not scp URLs.
	if len(s) >= 2 && s[1] == ':' && (len(s) == 2 || s[2] == '\\' || s[2] == '/') {
		return true
	}
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "/")
}

// sanitize strips any password from a URL-form remote.
func sanitize(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, hasPw := u.User.Password(); hasPw {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// RepoPath splits a remote into namespace and repository name according to
// the forge type's URL conventions.
func RepoPath(r *RemoteURL, forgeType string) (namespace, name string) {
	segs := r.Segments()
	switch forgeType {
	case "bitbucket-server":
		// HTTPS: /scm/PROJECT/repo ; SSH: /project/repo (port 7999)
		if len(segs) >= 3 && strings.EqualFold(segs[0], "scm") {
			segs = segs[1:]
		}
	case "azure-devops":
		// https://dev.azure.com/org/project/_git/repo
		// git@ssh.dev.azure.com:v3/org/project/repo
		for i, s := range segs {
			if s == "_git" && i+1 < len(segs) {
				return strings.Join(segs[:i], "/"), segs[i+1]
			}
		}
		if len(segs) == 4 && segs[0] == "v3" {
			segs = segs[1:]
		}
	}
	if len(segs) < 2 {
		return "", r.Path
	}
	return strings.Join(segs[:len(segs)-1], "/"), segs[len(segs)-1]
}
