package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/config"
)

// setupWorkRepo creates a checkout whose origin fetches from
// https://mock.example/acme/api.git (so trove detects the mock1 account) and
// pushes to a local bare repository.
func setupWorkRepo(t *testing.T) (work, bare string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	bare = filepath.Join(root, "remotes", "acme", "api.git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	gitCmd(t, root, "init", "--quiet", "--bare", bare)
	work = filepath.Join(root, "work")
	os.MkdirAll(work, 0o755)
	gitCmd(t, work, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, work, "remote", "add", "origin", "https://mock.example/acme/api.git")
	gitCmd(t, work, "remote", "set-url", "--push", "origin", bare)
	return work, bare
}

func TestCommitAndPushWithPR(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	work, bare := setupWorkRepo(t)
	t.Chdir(work)

	if r := h.run("", "commit", "-m", "nothing"); r.code != 0 || !strings.Contains(r.stdout, "Nothing to commit") {
		t.Fatalf("clean tree: %d %q %s", r.code, r.stdout, r.stderr)
	}
	os.WriteFile("README.md", []byte("hello\n"), 0o644)
	os.WriteFile("notes.txt", []byte("n\n"), 0o644)
	// Non-interactive with nothing staged must not guess.
	if r := h.run("", "commit", "-m", "x"); r.code != 2 {
		t.Fatalf("nothing staged: exit %d", r.code)
	}
	r := h.run("", "commit", "README.md", "-m", "Add readme", "--json")
	if r.code != 0 {
		t.Fatalf("commit: %d %s", r.code, r.stderr)
	}
	var cr commitResult
	if err := json.Unmarshal([]byte(r.stdout), &cr); err != nil || cr.Commit == "" || len(cr.Files) != 1 || cr.Files[0] != "README.md" {
		t.Fatalf("commit JSON: %v %s", err, r.stdout)
	}
	// Push the first time: upstream is set automatically.
	r = h.run("", "push", "--json")
	if r.code != 0 {
		t.Fatalf("push: %d %s", r.code, r.stderr)
	}
	var pr pushResult
	if err := json.Unmarshal([]byte(r.stdout), &pr); err != nil || pr.Branch != "main" || pr.Provider != "mock1" || !pr.UpstreamSet || pr.Repository != "acme/api" {
		t.Fatalf("push JSON: %v %s", err, r.stdout)
	}
	if out, _ := exec.Command("git", "--git-dir", bare, "log", "--format=%s", "main").Output(); strings.TrimSpace(string(out)) != "Add readme" {
		t.Fatalf("remote not updated: %q", out)
	}
	// commit -a --push --pr on a feature branch: the PR title defaults to the
	// commit subject in non-interactive mode.
	gitCmd(t, work, "checkout", "--quiet", "-b", "feature/notes")
	r = h.run("", "commit", "-a", "-m", "Add notes\n\nWhy we need notes.", "--push", "--pr", "--base", "main")
	if r.code != 0 {
		t.Fatalf("commit --push --pr: %d %s", r.code, r.stderr)
	}
	if len(h.be.prs) != 1 || h.be.prs[0].Title != "Add notes" || h.be.prs[0].Body != "Why we need notes." ||
		h.be.prs[0].SourceBranch != "feature/notes" || h.be.prs[0].TargetBranch != "main" {
		t.Fatalf("pull request = %+v", h.be.prs)
	}
	if !strings.Contains(r.stdout, "changes/1") {
		t.Fatalf("PR URL not printed: %s", r.stdout)
	}
	// A PR from main to main is refused before calling the provider.
	gitCmd(t, work, "checkout", "--quiet", "main")
	if r := h.run("", "push", "--pr", "--base", "main"); r.code != 2 {
		t.Fatalf("same-branch PR: %d %s", r.code, r.stderr)
	}
}

func TestPushSSHKeyValidationAndConfig(t *testing.T) {
	h := newHarness(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	key := filepath.Join(home, ".ssh", "id_work")
	os.WriteFile(key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600)
	os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAA work@example.com\n"), 0o644)

	h.writeConfig("version: 1\nsecrets:\n  provider: mem\n")
	r := h.run("", "provider", "add", "work", "--type", "mock", "--host", "mock.example", "--ssh-key", "id_work.pub", "--non-interactive")
	if r.code != 0 {
		t.Fatalf("provider add: %d %s", r.code, r.stderr)
	}
	cfg, _ := config.Load(h.cfgPath)
	if cfg.Providers["work"].SSHKey != "~/.ssh/id_work" {
		t.Fatalf("ssh_key saved as %q", cfg.Providers["work"].SSHKey)
	}
	if r := h.run("", "provider", "add", "bad", "--type", "mock", "--ssh-key", "missing", "--non-interactive"); r.code != 2 {
		t.Fatalf("missing key accepted: %d", r.code)
	}
	work, _ := setupWorkRepo(t)
	t.Chdir(work)
	if r := h.run("", "push", "--ssh-key", "does-not-exist"); r.code != 2 || !strings.Contains(r.stderr, "not found") {
		t.Fatalf("bad --ssh-key: %d %s", r.code, r.stderr)
	}
	if r := h.run("", "push", "--choose-key"); r.code != 2 {
		t.Fatalf("--choose-key without a terminal: %d %s", r.code, r.stderr)
	}
	if r := h.run("", "push", "--ssh-key", "a", "--choose-key"); r.code != 2 {
		t.Fatalf("mutually exclusive flags accepted: %d", r.code)
	}
	h.writeConfig(strings.Replace(baseConfig, "    host: mock.example\n", "    host: mock.example\n    ssh_key: ~/.ssh/id_work.pub\n", 1))
	if r := h.run("", "config", "validate"); r.code != 2 || !strings.Contains(r.stdout, "ssh_key") {
		t.Fatalf(".pub ssh_key not rejected: %d %s", r.code, r.stdout)
	}
}
