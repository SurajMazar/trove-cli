package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit() *Git {
	g := New()
	g.Env = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	return g
}

func TestCommitAndPushToBareRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	g := testGit()
	bare := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, t.TempDir(), "init", "--quiet", "--bare", bare)
	work := t.TempDir()
	mustGit(t, work, "init", "--quiet", "--initial-branch=main")
	mustGit(t, work, "remote", "add", "origin", bare)

	os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644)
	cs, err := g.Changes(ctx, work)
	if err != nil || len(cs) != 2 || !cs[0].Untracked() || cs[0].Describe() != "new file" {
		t.Fatalf("Changes = %+v, %v", cs, err)
	}
	if err := g.Add(ctx, work, "a.txt"); err != nil {
		t.Fatal(err)
	}
	cs, _ = g.Changes(ctx, work)
	staged := 0
	for _, c := range cs {
		if c.Staged() {
			staged++
		}
	}
	if staged != 1 {
		t.Fatalf("staged = %d (%+v)", staged, cs)
	}
	sha, err := g.Commit(ctx, work, "Add a\n\nBody text", CommitOptions{})
	if err != nil || sha == "" {
		t.Fatalf("Commit = %q, %v", sha, err)
	}
	subj, body, _ := g.LastCommit(ctx, work)
	if subj != "Add a" || body != "Body text" {
		t.Fatalf("LastCommit = %q / %q", subj, body)
	}
	if g.Upstream(ctx, work, "main") != "" {
		t.Fatal("unexpected upstream before first push")
	}
	if _, err := g.Push(ctx, work, "origin", PushOptions{SetUpstream: true}, "main"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if up := g.Upstream(ctx, work, "main"); up != "origin/main" {
		t.Fatalf("upstream = %q", up)
	}
	if r := g.BranchRemote(ctx, work, "main"); r != "origin" {
		t.Fatalf("branch remote = %q", r)
	}
	out, err := exec.Command("git", "--git-dir", bare, "log", "--format=%s", "main").Output()
	if err != nil || strings.TrimSpace(string(out)) != "Add a" {
		t.Fatalf("remote log = %q, %v", out, err)
	}
}

func TestSSHKeyIsPassedToSSH(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// A fake "ssh" first on PATH records its arguments and fails, proving
	// which key git would have used without touching the network.
	bin := t.TempDir()
	record := filepath.Join(bin, "args")
	script := "#!/bin/sh\necho \"$@\" > '" + record + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	key := filepath.Join(t.TempDir(), "my key") // space: quoting must hold
	os.WriteFile(key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600)
	work := t.TempDir()
	mustGit(t, work, "init", "--quiet")
	g := testGit().WithSSHKey(key)
	_ = g.Fetch(context.Background(), work, "ssh://git@example.invalid/acme/api.git", "main")
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake ssh was not invoked: %v", err)
	}
	if !strings.Contains(string(got), "-i "+key) || !strings.Contains(string(got), "IdentitiesOnly=yes") {
		t.Fatalf("ssh args = %q", got)
	}
}

func TestResolveAndListSSHKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	os.MkdirAll(sshDir, 0o700)
	os.WriteFile(filepath.Join(sshDir, "id_github"), []byte("private"), 0o600)
	os.WriteFile(filepath.Join(sshDir, "id_github.pub"), []byte("ssh-ed25519 AAAA me@example.com\n"), 0o644)
	os.WriteFile(filepath.Join(sshDir, "loose"), []byte("private"), 0o644)
	os.WriteFile(filepath.Join(sshDir, "loose.pub"), []byte("ssh-rsa AAAA\n"), 0o644)
	os.WriteFile(filepath.Join(sshDir, "orphan.pub"), []byte("ssh-rsa AAAA\n"), 0o644)

	keys, err := ListSSHKeys(sshDir)
	if err != nil || len(keys) != 2 || keys[0].Name != "id_github" || keys[0].Type != "ssh-ed25519" || keys[0].Comment != "me@example.com" {
		t.Fatalf("ListSSHKeys = %+v, %v", keys, err)
	}
	want := filepath.Join(sshDir, "id_github")
	for _, spec := range []string{"id_github", "id_github.pub", "~/.ssh/id_github", want} {
		if got, err := ResolveSSHKey(spec); err != nil || got != want {
			t.Errorf("ResolveSSHKey(%q) = %q, %v", spec, got, err)
		}
	}
	if _, err := ResolveSSHKey("nope"); err == nil {
		t.Error("missing key accepted")
	}
	if _, err := ResolveSSHKey("loose"); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("world-readable key: %v", err)
	}
	if got, _ := ResolveSSHKey(""); got != "" {
		t.Error("empty spec should resolve to empty")
	}
}

func TestParseChangesRenames(t *testing.T) {
	cs := ParseChanges("R  new.go\x00old.go\x00 M mod.go\x00?? n.txt\x00")
	if len(cs) != 3 || cs[0].Path != "new.go" || cs[0].OrigPath != "old.go" || cs[0].Describe() != "renamed" ||
		cs[1].Staged() || cs[1].Describe() != "modified" || !cs[2].Untracked() {
		t.Fatalf("ParseChanges = %+v", cs)
	}
}
