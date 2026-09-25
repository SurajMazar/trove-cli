package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH puts an "ssh" first on PATH that records its arguments and fails,
// proving which key git would use without touching the network.
func fakeSSH(t *testing.T) (record string) {
	t.Helper()
	bin := t.TempDir()
	record = filepath.Join(bin, "ssh-args")
	script := "#!/bin/sh\necho \"$@\" >> '" + record + "'\necho 'fake ssh: no network' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// sshKeyHome creates $HOME/.ssh with private keys (0600) named names.
func sshKeyHome(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	for _, n := range names {
		os.WriteFile(filepath.Join(home, ".ssh", n), []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600)
		os.WriteFile(filepath.Join(home, ".ssh", n+".pub"), []byte("ssh-ed25519 AAAA "+n+"@example.com\n"), 0o644)
	}
	return home
}

const sshKeyConfig = `version: 1
default_provider: mock1
providers:
  mock1:
    type: mock
    host: mock.example
    ssh_key: ~/.ssh/id_mock
    auth:
      type: token
      secret_ref: mem://trove/mock/mock1/token
  mock2:
    type: mock
    host: other.example
    ssh_key: ~/.ssh/id_other
secrets:
  provider: mem
`

func TestGitPassthroughUsesAccountSSHKey(t *testing.T) {
	h := newHarness(t)
	home := sshKeyHome(t, "id_mock", "id_other", "id_adhoc")
	h.writeConfig(sshKeyConfig)
	record := fakeSSH(t)
	work, _ := setupWorkRepo(t)
	gitCmd(t, work, "remote", "set-url", "origin", "git@mock.example:acme/api.git")
	gitCmd(t, work, "remote", "add", "upstream", "git@other.example:acme/api.git")
	t.Chdir(work)

	// origin belongs to mock1 → its ssh_key; flags pass through untouched.
	r := h.run("", "git", "fetch", "--prune", "--tags", "origin")
	if r.code == 0 {
		t.Fatalf("fetch through the fake ssh should fail with git's exit code")
	}
	args, _ := os.ReadFile(record)
	if !strings.Contains(string(args), "-i "+filepath.Join(home, ".ssh", "id_mock")) || !strings.Contains(string(args), "IdentitiesOnly=yes") {
		t.Fatalf("origin should use id_mock; ssh args = %q", args)
	}
	if !strings.Contains(r.stderr, "git fetch via mock1 · SSH key ~/.ssh/id_mock") || !strings.Contains(r.stderr, "fake ssh: no network") {
		t.Fatalf("stderr should show the key note and git's own output: %q", r.stderr)
	}
	// The remote named in the command picks the account (upstream → mock2).
	os.Remove(record)
	h.run("", "git", "fetch", "upstream")
	if args, _ := os.ReadFile(record); !strings.Contains(string(args), "id_other") {
		t.Fatalf("upstream should use id_other; ssh args = %q", args)
	}
	// --ssh-key overrides the saved key (bare names resolve in ~/.ssh).
	os.Remove(record)
	h.run("", "git", "--ssh-key", "id_adhoc", "pull", "--rebase", "origin", "main")
	if args, _ := os.ReadFile(record); !strings.Contains(string(args), "id_adhoc") {
		t.Fatalf("--ssh-key should override; ssh args = %q", args)
	}
	// clone detects the account from the URL.
	os.Remove(record)
	h.run("", "git", "clone", "--depth", "1", "git@other.example:acme/lib.git", filepath.Join(t.TempDir(), "lib"))
	if args, _ := os.ReadFile(record); !strings.Contains(string(args), "id_other") {
		t.Fatalf("clone should use the URL's account key; ssh args = %q", args)
	}
}

func TestGitPassthroughLocalCommandsAndExitCodes(t *testing.T) {
	h := newHarness(t)
	sshKeyHome(t) // mock1's ssh_key file does not exist
	h.writeConfig(sshKeyConfig)
	work, _ := setupWorkRepo(t)
	t.Chdir(work)
	os.WriteFile("f.txt", []byte("x\n"), 0o644)
	gitCmd(t, work, "add", "f.txt")
	gitCmd(t, work, "commit", "--quiet", "-m", "first commit")

	r := h.run("", "git", "log", "--format=%s", "-n", "1")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "first commit" {
		t.Fatalf("git log: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "ignoring the saved ssh_key") {
		t.Fatalf("a broken ssh_key should warn, not block: %q", r.stderr)
	}
	r = h.run("", "git", "rev-parse", "--verify", "does-not-exist")
	if r.code != 128 || strings.Contains(r.stderr, "✗") {
		t.Fatalf("git's exit code must pass through without trove's error box: %d %q", r.code, r.stderr)
	}
	r = h.run("", "git", "-C", work, "status", "--short")
	if r.code != 0 {
		t.Fatalf("git -C: %d %s", r.code, r.stderr)
	}
	if r := h.run("", "git", "--help-me"); r.code == 0 {
		t.Fatal("unknown git option should fail through git")
	}
	if r := h.run("", "git"); r.code != 0 || !strings.Contains(r.stdout, "Run any git command") {
		t.Fatalf("bare trove git should show help: %d", r.code)
	}
}

func TestForcePushNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	work, bare := setupWorkRepo(t)
	t.Chdir(work)
	os.WriteFile("a.txt", []byte("1\n"), 0o644)
	gitCmd(t, work, "add", "a.txt")
	gitCmd(t, work, "commit", "--quiet", "-m", "one")
	if r := h.run("", "push"); r.code != 0 {
		t.Fatalf("initial push: %d %s", r.code, r.stderr)
	}
	remoteHead := func() string {
		out, _ := exec.Command("git", "--git-dir", bare, "log", "--format=%s", "-n", "1", "main").Output()
		return strings.TrimSpace(string(out))
	}
	gitCmd(t, work, "commit", "--quiet", "--amend", "-m", "one (amended)")

	// trove git push --force: refused without --yes in scripts.
	if r := h.run("", "git", "push", "--force", "origin", "main"); r.code != 2 || remoteHead() != "one" {
		t.Fatalf("force push without --yes: code=%d remote=%q", r.code, remoteHead())
	}
	if r := h.run("", "git", "--yes", "push", "--force", "origin", "main"); r.code != 0 || remoteHead() != "one (amended)" {
		t.Fatalf("force push with --yes: code=%d remote=%q stderr=%s", r.code, remoteHead(), r.stderr)
	}
	// trove push --force / -f and flags after "--".
	gitCmd(t, work, "commit", "--quiet", "--amend", "-m", "two")
	if r := h.run("", "push", "-f"); r.code != 2 || remoteHead() != "one (amended)" {
		t.Fatalf("push -f without --yes: code=%d", r.code)
	}
	if r := h.run("", "push", "-f", "--yes", "--", "--no-verify"); r.code != 0 || remoteHead() != "two" {
		t.Fatalf("push -f --yes -- --no-verify: code=%d remote=%q stderr=%s", r.code, remoteHead(), r.stderr)
	}
	gitCmd(t, work, "commit", "--quiet", "--amend", "-m", "three")
	if r := h.run("", "push", "--", "--force"); r.code != 2 {
		t.Fatalf("destructive flags after -- must also confirm: %d", r.code)
	}
	if r := h.run("", "push", "--", "not-a-flag"); r.code != 2 || !strings.Contains(r.stderr, "not a flag") {
		t.Fatalf("non-flag after --: %d %s", r.code, r.stderr)
	}
	if r := h.run("", "push", "--force", "--force-with-lease"); r.code != 2 {
		t.Fatalf("--force with --force-with-lease must be rejected: %d", r.code)
	}
}
