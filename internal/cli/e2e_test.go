package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/providers/github"
	"github.com/SurajMazar/trove-cli/providers/gitlab"
)

// TestEndToEndRealDrivers runs the CLI against the real GitHub and GitLab
// drivers talking to fake API servers, verifying config → account → driver
// wiring, auth headers, and output.
func TestEndToEndRealDrivers(t *testing.T) {
	const ghToken = "ghp_e2eTestToken0000000000000000000000"
	const glToken = "glpat-e2eTestToken000000"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ghToken {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":1,"login":"octocat","name":"The Octocat"}`)
		case "/user/repos":
			fmt.Fprint(w, `[{"id":10,"name":"hello-world","full_name":"octocat/hello-world","owner":{"login":"octocat"},"private":false,"visibility":"public","default_branch":"main","html_url":"https://github.com/octocat/hello-world","clone_url":"https://github.com/octocat/hello-world.git","ssh_url":"git@github.com:octocat/hello-world.git"}]`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	defer gh.Close()
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != glToken {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"401 Unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/user":
			fmt.Fprint(w, `{"id":2,"username":"tanuki"}`)
		case "/api/v4/projects":
			fmt.Fprint(w, `[{"id":20,"path":"api","name":"API","path_with_namespace":"platform/backend/api","namespace":{"full_path":"platform/backend"},"visibility":"internal","default_branch":"main","web_url":"https://gitlab.example/platform/backend/api","http_url_to_repo":"https://gitlab.example/platform/backend/api.git","ssh_url_to_repo":"git@gitlab.example:platform/backend/api.git"}]`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"404 Not Found"}`)
		}
	}))
	defer gl.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1
default_provider: gh
providers:
  gh:
    type: github
    host: github.com
    api_base_url: %s
    auth:
      type: token
      secret_ref: mem://gh
  gl:
    type: gitlab
    host: gitlab.example
    api_base_url: %s/api/v4
    auth:
      type: token
      secret_ref: mem://gl
secrets:
  provider: mem
`, gh.URL, gl.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	mem := secrets.NewMemoryProvider("mem")
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		f := &Factory{IO: terminal.Test(strings.NewReader(stdin), &out, &errb), Deps: app.Deps{
			RegisterDrivers: func(r *forge.Registry) { r.MustRegister(github.NewDriver()); r.MustRegister(gitlab.NewDriver()) },
			RegisterSecrets: func(r *secrets.Resolver, _ config.SecretsConfig) { r.Register(mem) },
			Getenv:          func(string) string { return "" },
			CacheDir:        filepath.Join(dir, "cache"),
		}}
		code := Execute(context.Background(), f, append([]string{"--config", cfgPath}, args...))
		return code, out.String(), errb.String()
	}

	if code, _, stderr := run(ghToken+"\n", "auth", "login", "gh", "--with-token"); code != 0 {
		t.Fatalf("gh login: %d %s", code, stderr)
	}
	if code, _, stderr := run(glToken+"\n", "auth", "login", "gl", "--with-token"); code != 0 {
		t.Fatalf("gl login: %d %s", code, stderr)
	}
	code, stdout, stderr := run("", "repo", "list", "--all-providers", "--json")
	if code != 0 {
		t.Fatalf("repo list: %d %s", code, stderr)
	}
	var repos []domain.Repository
	if err := json.Unmarshal([]byte(stdout), &repos); err != nil {
		t.Fatalf("JSON: %v\n%s", err, stdout)
	}
	got := map[string]string{}
	for _, r := range repos {
		got[r.Provider+":"+r.FullName] = string(r.Visibility)
	}
	if got["gh:octocat/hello-world"] != "public" || got["gl:platform/backend/api"] != "internal" {
		t.Fatalf("repos = %v", got)
	}
	code, stdout, _ = run("", "auth", "status", "--json")
	if code != 0 || !strings.Contains(stdout, `"octocat"`) || !strings.Contains(stdout, `"tanuki"`) {
		t.Fatalf("auth status: %d %s", code, stdout)
	}
	if strings.Contains(stdout, ghToken) || strings.Contains(stdout, glToken) {
		t.Fatal("status leaked a token")
	}
	// Wrong token → normalized auth error, token never printed.
	_ = mem.Set(context.Background(), "gh", "ghp_wrongwrongwrongwrongwrongwrong0000")
	code, _, stderr = run("", "--no-cache", "repo", "list", "-P", "gh")
	if code != 3 || strings.Contains(stderr, "ghp_wrong") {
		t.Fatalf("bad token: %d %s", code, stderr)
	}
}
