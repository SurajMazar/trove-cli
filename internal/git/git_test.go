package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		in, scheme, host, port, path string
	}{
		{"https://github.com/user/repo.git", "https", "github.com", "", "user/repo"},
		{"git@github.com:user/repo.git", "ssh", "github.com", "", "user/repo"},
		{"https://gitlab.company.com/group/project.git", "https", "gitlab.company.com", "", "group/project"},
		{"git@gitlab.company.com:group/project.git", "ssh", "gitlab.company.com", "", "group/project"},
		{"https://gitlab.com/group/sub/deeper/project.git", "https", "gitlab.com", "", "group/sub/deeper/project"},
		{"git@gitlab.com:group/sub/project", "ssh", "gitlab.com", "", "group/sub/project"},
		{"https://bitbucket.org/workspace/repo.git", "https", "bitbucket.org", "", "workspace/repo"},
		{"https://someone@bitbucket.org/workspace/repo.git", "https", "bitbucket.org", "", "workspace/repo"},
		{"git@bitbucket.org:workspace/repo.git", "ssh", "bitbucket.org", "", "workspace/repo"},
		{"ssh://git@bitbucket.company.com:7999/proj/repo.git", "ssh", "bitbucket.company.com", "7999", "proj/repo"},
		{"ssh://git@GitHub.com/User/Repo", "ssh", "github.com", "", "User/Repo"},
		{"git://example.com/a/b.git", "git", "example.com", "", "a/b"},
		{"https://github.com/user/repo/", "https", "github.com", "", "user/repo"},
	}
	for _, c := range cases {
		r, err := ParseRemoteURL(c.in)
		if err != nil {
			t.Errorf("ParseRemoteURL(%q): %v", c.in, err)
			continue
		}
		if r.Scheme != c.scheme || r.Host != c.host || r.Port != c.port || r.Path != c.path {
			t.Errorf("ParseRemoteURL(%q) = %+v; want scheme=%s host=%s port=%s path=%s", c.in, r, c.scheme, c.host, c.port, c.path)
		}
	}
}

func TestParseRemoteURLRejects(t *testing.T) {
	for _, in := range []string{"", "not a url", "https://github.com/", "https://github.com/onlyone", "C:\\repo", "./relative/path", "https://github.com/a/../b"} {
		if _, err := ParseRemoteURL(in); err == nil {
			t.Errorf("ParseRemoteURL(%q) succeeded, want error", in)
		}
	}
}

func TestParseRemoteURLStripsPassword(t *testing.T) {
	r, err := ParseRemoteURL("https://user:ghp_supersecret@github.com/o/r.git")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Raw, "supersecret") {
		t.Fatalf("password retained in Raw: %s", r.Raw)
	}
}

func TestDetect(t *testing.T) {
	accounts := []Account{
		{Alias: "gh-personal", Type: "github", Hosts: []string{"github.com"}},
		{Alias: "gh-work", Type: "github", Hosts: []string{"github.com"}},
		{Alias: "ghe", Type: "github", Hosts: []string{"github.company.com", "https://github.company.com/api/v3"}},
		{Alias: "gl-work", Type: "gitlab", Hosts: []string{"code.company.com", "ssh.code.company.com:2222"}},
	}
	cases := []struct {
		url, typ, provider, ns, name, source string
		candidates                           int
	}{
		{"git@github.com:user/repo.git", "github", "gh-work", "user", "repo", "config", 2},
		{"https://github.company.com/team/app.git", "github", "ghe", "team", "app", "config", 0},
		{"git@code.company.com:group/sub/project.git", "gitlab", "gl-work", "group/sub", "project", "config", 0},
		{"ssh://git@ssh.code.company.com:2222/a/b.git", "gitlab", "gl-work", "a", "b", "config", 0},
		{"https://gitlab.com/g/s/p.git", "gitlab", "", "g/s", "p", "known-host", 0},
		{"https://bitbucket.org/ws/repo.git", "bitbucket", "", "ws", "repo", "known-host", 0},
		{"https://bitbucket.corp.example/scm/PROJ/repo.git", "bitbucket-server", "", "PROJ", "repo", "heuristic", 0},
		{"https://gitlab.internal.example/x/y.git", "gitlab", "", "x", "y", "heuristic", 0},
		{"https://dev.azure.com/org/proj/_git/repo", "azure-devops", "", "org/proj", "repo", "known-host", 0},
		{"https://git.unknown.example/a/b.git", "", "", "a", "b", "unknown", 0},
	}
	for _, c := range cases {
		d, err := Detect(c.url, accounts, "gh-work")
		if err != nil {
			t.Errorf("Detect(%q): %v", c.url, err)
			continue
		}
		if d.Type != c.typ || d.Provider != c.provider || d.Namespace != c.ns || d.Name != c.name || d.Source != c.source || len(d.Candidates) != c.candidates {
			t.Errorf("Detect(%q) = %+v; want type=%s provider=%s ns=%s name=%s source=%s candidates=%d",
				c.url, d, c.typ, c.provider, c.ns, c.name, c.source, c.candidates)
		}
	}
}

func TestParseRemotes(t *testing.T) {
	out := "origin\tgit@github.com:me/repo.git (fetch)\norigin\tgit@github.com:me/repo.git (push)\n" +
		"upstream\thttps://github.com/org/repo.git (fetch)\nupstream\tno_push (push)\n"
	rs := ParseRemotes(out)
	if len(rs) != 2 || rs[0].Name != "origin" || rs[1].Name != "upstream" {
		t.Fatalf("unexpected remotes: %+v", rs)
	}
	if rs[0].PushURL != "" {
		t.Errorf("identical push URL should be omitted: %+v", rs[0])
	}
	if rs[1].PushURL != "no_push" {
		t.Errorf("distinct push URL lost: %+v", rs[1])
	}
}

func TestParseStatus(t *testing.T) {
	out := "# branch.oid abc\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -1\n1 .M N... 100644 100644 100644 a b file.go\n? new.txt\n"
	s := ParseStatus(out)
	if s.Branch != "main" || s.Upstream != "origin/main" || s.Ahead != 2 || s.Behind != 1 || s.Changed != 1 || s.Untracked != 1 || s.Clean() {
		t.Fatalf("unexpected status: %+v", s)
	}
}

func TestCredentialHelperProtocol(t *testing.T) {
	req, err := ReadCredentialRequest(strings.NewReader("protocol=https\nhost=github.com\npath=o/r.git\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Protocol != "https" || req.Host != "github.com" || req.Path != "o/r.git" {
		t.Fatalf("unexpected request: %+v", req)
	}
	var buf bytes.Buffer
	if err := WriteCredential(&buf, "x-access-token", "tok"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "username=x-access-token\npassword=tok\n\n" {
		t.Fatalf("unexpected output %q", buf.String())
	}
	if err := WriteCredential(&buf, "u", "bad\nvalue"); err == nil {
		t.Fatal("newline in password must be rejected")
	}
}

func TestHelperCommandQuotesPaths(t *testing.T) {
	got := HelperCommand("/Users/me/CLI Tools/trove", "auth", "git-credential", "--provider", "it's")
	want := `!'/Users/me/CLI Tools/trove' 'auth' 'git-credential' '--provider' 'it'\''s'`
	if got != want {
		t.Fatalf("HelperCommand = %s\nwant %s", got, want)
	}
}

// TestGitOperations exercises the real git binary against local repositories.
func TestGitOperations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	g := New()
	g.Env = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	if _, err := g.Version(ctx); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	mustGit(t, src, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", "README")
	mustGit(t, src, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")
	mustGit(t, src, "branch", "feature")

	dst := filepath.Join(t.TempDir(), "clone")
	if err := g.Clone(ctx, src, dst, CloneOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !g.IsRepository(ctx, dst) {
		t.Fatal("clone is not a repository")
	}
	br, err := g.CurrentBranch(ctx, dst)
	if err != nil || br != "main" {
		t.Fatalf("CurrentBranch = %q, %v", br, err)
	}
	if err := g.Fetch(ctx, dst, "origin", "feature:refs/remotes/origin/feature"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := g.Checkout(ctx, dst, "feature", "origin/feature"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if br, _ := g.CurrentBranch(ctx, dst); br != "feature" {
		t.Fatalf("branch after checkout = %q", br)
	}
	rs, err := g.Remotes(ctx, dst)
	if err != nil || len(rs) != 1 || rs[0].Name != "origin" {
		t.Fatalf("Remotes = %+v, %v", rs, err)
	}
	st, err := g.Status(ctx, dst)
	if err != nil || !st.Clean() {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	// Failing clone surfaces ErrGit without hanging on prompts.
	if err := g.Clone(ctx, filepath.Join(src, "missing"), filepath.Join(t.TempDir(), "x"), CloneOptions{}); err == nil {
		t.Fatal("clone of missing repo succeeded")
	}
	// Cancellation.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := g.Clone(cctx, src, filepath.Join(t.TempDir(), "y"), CloneOptions{}); err == nil {
		t.Fatal("clone with canceled context succeeded")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
