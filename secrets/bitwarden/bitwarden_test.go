package bitwarden

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets/secrettest"
)

const pemValue = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBmYWtlLWtleS1mb3ItdGVzdHMtb25seS0wMDAwMDAwMDAwMA==
-----END OPENSSH PRIVATE KEY-----
`

const jsonValue = `{"client_id":"abc","client_secret":"s3cr3t\"quoted\"","nested":{"a":[1,2,3]},"html":"<b>&amp;</b>"}`

func env(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

func newCLI(t *testing.T, f *fakeBW, opts Options) *Provider {
	t.Helper()
	opts.Runner = f
	if opts.Env == nil {
		opts.Env = env(map[string]string{"BW_SESSION": "fake-session"})
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func newSM(t *testing.T, f *fakeBWS, opts Options) *Provider {
	t.Helper()
	opts.Backend = BackendSecretsManager
	opts.Runner = f
	if opts.Env == nil {
		opts.Env = env(map[string]string{"BWS_ACCESS_TOKEN": "0.fake-token"})
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestContractCLI(t *testing.T) {
	t.Run("default folder", func(t *testing.T) {
		secrettest.RunSecretProviderContractTests(t, newCLI(t, newFakeBW(), Options{}))
	})
	t.Run("no folder", func(t *testing.T) {
		secrettest.RunSecretProviderContractTests(t, newCLI(t, newFakeBW(), Options{NoFolder: true}))
	})
	t.Run("organization collection", func(t *testing.T) {
		secrettest.RunSecretProviderContractTests(t, newCLI(t, newFakeBW(), Options{OrganizationID: "org-1", CollectionID: "col-1", SyncOnStart: true}))
	})
}

func TestContractSecretsManager(t *testing.T) {
	secrettest.RunSecretProviderContractTests(t, newSM(t, newFakeBWS("proj-1"), Options{ProjectID: "proj-1"}))
}

func TestRoundTripExactValues(t *testing.T) {
	ctx := context.Background()
	providers := map[string]*Provider{
		"cli":             newCLI(t, newFakeBW(), Options{}),
		"secrets-manager": newSM(t, newFakeBWS("proj-1"), Options{ProjectID: "proj-1"}),
	}
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			for i, v := range []string{pemValue, jsonValue, "  leading and trailing spaces \n", "-dash-first", "unicodé ✓", ""} {
				key := "trove/github/personal/token"
				if err := p.Set(ctx, key, v); err != nil {
					t.Fatalf("Set #%d: %v", i, err)
				}
				got, err := p.Get(ctx, key)
				if err != nil {
					t.Fatalf("Get #%d: %v", i, err)
				}
				if got != v {
					t.Fatalf("value #%d did not round-trip: got %q want %q", i, got, v)
				}
			}
		})
	}
}

func TestCLIValueNeverInArgv(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{})
	key := "trove/github/personal/token"
	for _, v := range []string{pemValue, "ghp_argvcheck000000000000000000000000"} {
		if err := p.Set(ctx, key, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	var creates, edits int
	for _, c := range f.recorded() {
		if c.name != "bw" {
			t.Errorf("ran %q, want bw", c.name)
		}
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "ghp_argvcheck") || strings.Contains(joined, "PRIVATE KEY") || strings.Contains(joined, "fake-session") {
			t.Fatalf("secret material in argv: %q", joined)
		}
		if !strings.Contains(joined, "--nointeraction") {
			t.Errorf("missing --nointeraction: %q", joined)
		}
		if strings.Contains(joined, "--session") {
			t.Errorf("session passed on argv: %q", joined)
		}
		switch {
		case strings.HasPrefix(joined, "create item"):
			creates++
			var item map[string]any
			if err := decodeRequest(c.stdin, &item); err != nil {
				t.Fatalf("create stdin is not base64 JSON: %v", err)
			}
			login := item["login"].(map[string]any)
			if login["password"] != pemValue || item["notes"] != managedNote || item["type"] != float64(1) || item["name"] != key {
				t.Errorf("unexpected created item shape: name=%v type=%v notes=%v", item["name"], item["type"], item["notes"])
			}
			if item["folderId"] == nil {
				t.Error("created item is not in the trove folder")
			}
		case strings.HasPrefix(joined, "edit item"):
			edits++
			var item map[string]any
			if err := decodeRequest(c.stdin, &item); err != nil {
				t.Fatalf("edit stdin is not base64 JSON: %v", err)
			}
			if item["login"].(map[string]any)["password"] != "ghp_argvcheck000000000000000000000000" {
				t.Error("edit did not carry the new password")
			}
		}
	}
	if creates != 1 || edits != 1 {
		t.Fatalf("creates=%d edits=%d, want 1 and 1", creates, edits)
	}
}

func TestCLIStatusMapping(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		status   string
		env      map[string]string
		missing  bool
		kind     error
		hint     string
		contains string
	}{
		{name: "locked without session", status: "locked", env: map[string]string{}, kind: errs.ErrSecretProviderLocked, hint: `export BW_SESSION="$(bw unlock --raw)"`, contains: "BW_SESSION is not set"},
		{name: "locked with stale session", status: "locked", env: map[string]string{"BW_SESSION": "old"}, kind: errs.ErrSecretProviderLocked, hint: `export BW_SESSION="$(bw unlock --raw)"`, contains: "invalid or expired"},
		{name: "unauthenticated", status: "unauthenticated", kind: errs.ErrSecretProviderLocked, hint: "bw login", contains: "not logged in"},
		{name: "not installed", missing: true, kind: errs.ErrSecretUnavailable, contains: "Bitwarden CLI (bw) not found; install it from https://bitwarden.com/help/cli/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBW()
			f.status = tc.status
			f.notInstalled = tc.missing
			opts := Options{}
			if tc.env != nil {
				opts.Env = env(tc.env)
			}
			p := newCLI(t, f, opts)
			for _, err := range []error{
				p.Check(ctx),
				func() error { _, e := p.Get(ctx, "trove/x/y/token"); return e }(),
				p.Set(ctx, "trove/x/y/token", "value-that-must-not-leak"),
			} {
				if !errors.Is(err, tc.kind) {
					t.Fatalf("want %v, got %v", tc.kind, err)
				}
				if tc.hint != "" && errs.HintOf(err) != tc.hint {
					t.Errorf("hint = %q, want %q", errs.HintOf(err), tc.hint)
				}
				if !strings.Contains(err.Error(), tc.contains) {
					t.Errorf("error %q does not mention %q", err, tc.contains)
				}
				if strings.Contains(err.Error(), "value-that-must-not-leak") {
					t.Fatal("error leaks value")
				}
			}
		})
	}
}

func TestCLIStatusCachedOnceUnlocked(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{SyncOnStart: true})
	for i := 0; i < 3; i++ {
		if err := p.Set(ctx, "trove/a/b/token", "v"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Get(ctx, "trove/a/b/token"); err != nil {
			t.Fatal(err)
		}
	}
	var status, syncs int
	firstList := -1
	for i, c := range f.recorded() {
		switch c.args[0] {
		case "status":
			status++
		case "sync":
			syncs++
			if firstList != -1 {
				t.Error("sync ran after the first read")
			}
		case "list":
			if firstList == -1 {
				firstList = i
			}
		}
	}
	if status != 1 || syncs != 1 {
		t.Fatalf("status calls = %d, sync calls = %d; want 1 and 1", status, syncs)
	}
}

func TestCLINoSyncByDefault(t *testing.T) {
	f := newFakeBW()
	p := newCLI(t, f, Options{})
	if err := p.Set(context.Background(), "trove/a/b/token", "v"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.recorded() {
		if c.args[0] == "sync" {
			t.Fatal("bw sync ran without SyncOnStart")
		}
	}
}

func TestCLISessionExpiresMidRun(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{NoFolder: true})
	if _, err := p.Exists(ctx, "trove/a/b/token"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lockAfter = len(f.calls)
	f.mu.Unlock()
	_, err := p.Get(ctx, "trove/a/b/token")
	if !errors.Is(err, errs.ErrSecretProviderLocked) || errs.HintOf(err) != bwUnlockHint {
		t.Fatalf("want locked error with unlock hint, got %v", err)
	}
	// The cached status was dropped, so the next call runs bw status again.
	f.mu.Lock()
	f.lockAfter = 0
	f.status = "locked"
	before := len(f.calls)
	f.mu.Unlock()
	if _, err := p.Get(ctx, "trove/a/b/token"); !errors.Is(err, errs.ErrSecretProviderLocked) {
		t.Fatalf("want locked, got %v", err)
	}
	if c := f.recorded()[before]; c.args[0] != "status" {
		t.Fatalf("expected a fresh bw status, got %v", c.args)
	}
}

func TestCLIFolderCreatedOnceAndReused(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{Folder: "my-trove"})
	// Reads never create the folder.
	if _, err := p.Get(ctx, "trove/a/b/token"); !errors.Is(err, errs.ErrSecretNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if len(f.folders) != 0 {
		t.Fatal("read created a folder")
	}
	for _, k := range []string{"trove/a/b/token", "trove/c/d/token"} {
		if err := p.Set(ctx, k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	var creates int
	for _, c := range f.recorded() {
		if c.args[0] == "create" && c.args[1] == "folder" {
			creates++
			var req map[string]any
			if err := decodeRequest(c.stdin, &req); err != nil || req["name"] != "my-trove" {
				t.Fatalf("bad folder request: %v %v", req, err)
			}
		}
	}
	if creates != 1 || len(f.folders) != 1 {
		t.Fatalf("folder created %d times (%d folders), want once", creates, len(f.folders))
	}
	var folderID string
	for id := range f.folders {
		folderID = id
	}
	for _, it := range f.items {
		if it["folderId"] != folderID {
			t.Fatalf("item not in folder: %v", it["folderId"])
		}
	}

	// A fresh provider reuses the existing folder.
	p2 := newCLI(t, f, Options{Folder: "my-trove"})
	if v, err := p2.Get(ctx, "trove/a/b/token"); err != nil || v != "v" {
		t.Fatalf("Get via existing folder = %q, %v", v, err)
	}
	if len(f.folders) != 1 {
		t.Fatal("second provider created another folder")
	}
}

func TestCLIScopesLookupsToFolderAndOrganization(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	f.folders["folder-x"] = "trove"
	f.addItem(map[string]any{"type": 1, "name": "trove/a/b/token", "folderId": nil, "login": map[string]any{"password": "outside"}})
	f.addItem(map[string]any{"type": 1, "name": "trove/a/b/token-other", "folderId": "folder-x", "login": map[string]any{"password": "prefix"}})
	p := newCLI(t, f, Options{})
	if _, err := p.Get(ctx, "trove/a/b/token"); !errors.Is(err, errs.ErrSecretNotFound) {
		t.Fatalf("items outside the folder or with a longer name must not match, got %v", err)
	}
	f.addItem(map[string]any{"type": 1, "name": "trove/a/b/token", "folderId": "folder-x", "organizationId": "org-2", "login": map[string]any{"password": "other-org"}})
	porg := newCLI(t, f, Options{OrganizationID: "org-1", CollectionID: "col-1"})
	if ok, err := porg.Exists(ctx, "trove/a/b/token"); err != nil || ok {
		t.Fatalf("item from another organization matched: %v %v", ok, err)
	}
	if v, err := p.Get(ctx, "trove/a/b/token"); err != nil || v != "other-org" {
		t.Fatalf("Get = %q, %v", v, err)
	}
}

func TestCLIAmbiguousItems(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{NoFolder: true})
	f.addItem(map[string]any{"type": 1, "name": "trove/a/b/token", "login": map[string]any{"password": "first-secret-value"}})
	f.addItem(map[string]any{"type": 1, "name": "trove/a/b/token", "login": map[string]any{"password": "second-secret-value"}})
	for _, err := range []error{
		func() error { _, e := p.Get(ctx, "trove/a/b/token"); return e }(),
		func() error { _, e := p.Exists(ctx, "trove/a/b/token"); return e }(),
		p.Set(ctx, "trove/a/b/token", "third-secret-value"),
		p.Delete(ctx, "trove/a/b/token"),
	} {
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "found 2 items named \"trove/a/b/token\"") || !strings.Contains(msg, "item-0001") {
			t.Errorf("unhelpful ambiguity message: %s", msg)
		}
		if strings.Contains(msg, "secret-value") {
			t.Fatalf("ambiguity error leaks a value: %s", msg)
		}
	}
	if len(f.items) != 2 {
		t.Fatal("ambiguous delete removed an item")
	}
}

func TestCLINonLoginItem(t *testing.T) {
	f := newFakeBW()
	p := newCLI(t, f, Options{NoFolder: true})
	f.addItem(map[string]any{"type": 2, "name": "trove/a/b/token", "secureNote": map[string]any{"type": 0}, "notes": "hello"})
	if _, err := p.Get(context.Background(), "trove/a/b/token"); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("want ErrInvalidConfiguration, got %v", err)
	}
	if err := p.Delete(context.Background(), "trove/a/b/token"); err == nil || len(f.items) != 1 {
		t.Fatal("deleted a non-login item")
	}
}

func TestCLIEditPreservesOtherFields(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{NoFolder: true})
	id := f.addItem(map[string]any{
		"type": 1, "name": "trove/a/b/token", "notes": "user notes", "favorite": true,
		"fields":          []any{map[string]any{"name": "scope", "value": "repo", "type": 0}},
		"passwordHistory": []any{map[string]any{"password": "older"}},
		"login":           map[string]any{"username": "octocat", "password": "old", "uris": []any{map[string]any{"uri": "https://github.com"}}, "totp": nil},
	})
	if err := p.Set(ctx, "trove/a/b/token", "new"); err != nil {
		t.Fatal(err)
	}
	it := f.items[id]
	login := it["login"].(map[string]any)
	if login["password"] != "new" || login["username"] != "octocat" || it["notes"] != "user notes" || it["favorite"] != true {
		t.Fatalf("edit clobbered fields: %v", it)
	}
	if b, _ := json.Marshal(it["fields"]); !strings.Contains(string(b), "scope") {
		t.Fatal("custom fields lost")
	}
	if _, ok := it["passwordHistory"]; !ok {
		t.Fatal("password history lost")
	}
}

func TestCLIDeleteMovesToTrash(t *testing.T) {
	ctx := context.Background()
	f := newFakeBW()
	p := newCLI(t, f, Options{})
	if err := p.Set(ctx, "trove/a/b/token", "v"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "trove/a/b/token"); err != nil {
		t.Fatal(err)
	}
	if len(f.trash) != 1 || len(f.items) != 0 {
		t.Fatalf("items=%d trash=%d", len(f.items), len(f.trash))
	}
	if err := p.Delete(ctx, "trove/a/b/token"); err != nil {
		t.Fatalf("Delete(missing) = %v", err)
	}
	for _, c := range f.recorded() {
		for _, a := range c.args {
			if a == "--permanent" {
				t.Fatal("Trove must not permanently delete items")
			}
		}
	}
}

// echoRunner wraps a runner and makes writes fail with a stderr that echoes
// the request, like a misbehaving CLI would.
type echoRunner struct {
	Runner
	echo string
}

func (e echoRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	if len(args) > 1 && (args[0] == "create" || args[0] == "edit") && args[1] == "item" || len(args) > 1 && args[0] == "secret" && args[1] != "list" {
		return nil, nil, &RunError{Command: name, ExitCode: 1, Stderr: SanitizeStderr("bad request: " + e.echo)}
	}
	return e.Runner.Run(ctx, stdin, name, args...)
}

func TestErrorsScrubEchoedValues(t *testing.T) {
	ctx := context.Background()
	value := "line-one-of-secret\nline-two-of-secret"
	pc, _ := New(Options{Runner: echoRunner{Runner: newFakeBW(), echo: value}, Env: env(nil)})
	ps, _ := New(Options{Backend: BackendSecretsManager, ProjectID: "p", Runner: echoRunner{Runner: newFakeBWS("p"), echo: value}, Env: env(map[string]string{"BWS_ACCESS_TOKEN": "t"})})
	for name, p := range map[string]*Provider{"cli": pc, "secrets-manager": ps} {
		err := p.Set(ctx, "trove/a/b/token", value)
		if !errors.Is(err, errs.ErrProviderAPI) {
			t.Fatalf("%s: want ErrProviderAPI, got %v", name, err)
		}
		if strings.Contains(err.Error(), "line-one-of-secret") || strings.Contains(err.Error(), "line-two-of-secret") {
			t.Fatalf("%s: error leaks value: %v", name, err)
		}
		if !strings.Contains(err.Error(), "exited with status 1") {
			t.Errorf("%s: error lacks exit status: %v", name, err)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{Backend: "vaultwarden-api"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("unknown backend: %v", err)
	}
	if _, err := New(Options{OrganizationID: "org"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("organization without collection: %v", err)
	}
	if _, err := New(Options{CollectionID: "col"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("collection without organization: %v", err)
	}
	for _, b := range []string{"", "cli", "CLI", " secrets-manager "} {
		p, err := New(Options{Backend: b})
		if err != nil {
			t.Fatalf("backend %q: %v", b, err)
		}
		if p.Name() != "bitwarden" || p.Description() == "" {
			t.Fatalf("backend %q: name=%q description=%q", b, p.Name(), p.Description())
		}
	}
	p, _ := New(Options{})
	if _, err := p.Get(context.Background(), "  "); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("empty key: %v", err)
	}
}

func TestSecretsManagerErrors(t *testing.T) {
	ctx := context.Background()

	p := newSM(t, newFakeBWS("p"), Options{ProjectID: "p", Env: env(map[string]string{})})
	for _, err := range []error{p.Check(ctx), p.Set(ctx, "k", "v"), func() error { _, e := p.Get(ctx, "k"); return e }()} {
		if !errors.Is(err, errs.ErrSecretProviderLocked) || errs.HintOf(err) != "export BWS_ACCESS_TOKEN=..." {
			t.Fatalf("missing token: %v (hint %q)", err, errs.HintOf(err))
		}
	}

	rej := newFakeBWS("p")
	rej.reject = true
	p = newSM(t, rej, Options{ProjectID: "p"})
	if err := p.Check(ctx); !errors.Is(err, errs.ErrSecretProviderLocked) || errs.HintOf(err) != "export BWS_ACCESS_TOKEN=..." {
		t.Fatalf("rejected token: %v", err)
	}

	missing := newFakeBWS()
	missing.notInstalled = true
	p = newSM(t, missing, Options{})
	if _, err := p.Get(ctx, "k"); !errors.Is(err, errs.ErrSecretUnavailable) || !strings.Contains(err.Error(), "bws") {
		t.Fatalf("not installed: %v", err)
	}

	p = newSM(t, newFakeBWS("p"), Options{})
	if err := p.Set(ctx, "k", "v"); !errors.Is(err, errs.ErrInvalidConfiguration) || !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("Set without project: %v", err)
	}
	if err := p.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	p = newSM(t, newFakeBWS("p"), Options{ProjectID: "missing"})
	if err := p.Check(ctx); !errors.Is(err, errs.ErrProviderAPI) {
		t.Fatalf("Check with unknown project: %v", err)
	}
}

func TestSecretsManagerArgsAndAmbiguity(t *testing.T) {
	ctx := context.Background()
	f := newFakeBWS("p1", "p2")
	p := newSM(t, f, Options{ProjectID: "p1"})
	if err := p.Set(ctx, "trove/a/b/token", pemValue); err != nil {
		t.Fatal(err)
	}
	if err := p.Set(ctx, "trove/a/b/token", "-starts-with-dash"); err != nil {
		t.Fatal(err)
	}
	var sawCreate, sawEdit bool
	for _, c := range f.recorded() {
		for _, a := range c.args {
			if strings.HasPrefix(a, "--access-token") {
				t.Fatal("access token passed on argv")
			}
		}
		switch {
		case c.args[0] == "secret" && c.args[1] == "create":
			sawCreate = true
			if got := c.args[len(c.args)-4:]; got[0] != "--" || got[1] != "trove/a/b/token" || got[2] != pemValue || got[3] != "p1" {
				t.Errorf("create args = %q", got)
			}
		case c.args[0] == "secret" && c.args[1] == "edit":
			sawEdit = true
			if !contains(c.args, "--value=-starts-with-dash") {
				t.Errorf("edit args = %q", c.args)
			}
		}
		if c.args[0] == "secret" && c.args[1] != "list" && !contains(c.args, "none") {
			t.Errorf("write without --output none: %q", c.args)
		}
	}
	if !sawCreate || !sawEdit {
		t.Fatal("expected one create and one edit")
	}

	// The same key in two projects is ambiguous when no project is configured.
	f.secrets["secret-9999"] = map[string]any{"id": "secret-9999", "key": "trove/a/b/token", "value": "other-project-value", "projectId": "p2"}
	all := newSM(t, f, Options{})
	if _, err := all.Get(ctx, "trove/a/b/token"); !errors.Is(err, errs.ErrConflict) || strings.Contains(err.Error(), "other-project-value") {
		t.Fatalf("want ErrConflict without values, got %v", err)
	}
	// Scoped to a project it is not.
	if v, err := p.Get(ctx, "trove/a/b/token"); err != nil || v != "-starts-with-dash" {
		t.Fatalf("scoped Get = %q, %v", v, err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestSemaphoreHonorsContext(t *testing.T) {
	f := newFakeBW()
	p := newCLI(t, f, Options{})
	b := p.b.(*cliBackend)
	if err := b.sem.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.sem.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled while waiting for the CLI lock, got %v", err)
	}
}

func TestExecRunner(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	r := ExecRunner{}

	out, _, err := r.Run(ctx, []byte("piped-stdin"), "sh", "-c", "cat")
	if err != nil || string(out) != "piped-stdin" {
		t.Fatalf("stdin not delivered: %q %v", out, err)
	}

	_, _, err = r.Run(ctx, []byte("top-secret-stdin"), "sh", "-c", "cat >/dev/null; echo 'Vault is locked.' >&2; exit 3")
	var re *RunError
	if !errors.As(err, &re) || re.ExitCode != 3 || re.Stderr != "Vault is locked." {
		t.Fatalf("unexpected error %#v", err)
	}
	if strings.Contains(err.Error(), "top-secret-stdin") {
		t.Fatal("error contains stdin")
	}

	_, _, err = r.Run(ctx, nil, "sh", "-c", "echo 'token ghp_abcdefghijklmnopqrstuvwxyz0123456789' >&2; exit 1")
	if err == nil || strings.Contains(err.Error(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("stderr not redacted: %v", err)
	}

	_, _, err = r.Run(ctx, nil, "trove-definitely-missing-binary")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("want exec.ErrNotFound, got %v", err)
	}
	cerr := commandError("op", "Bitwarden CLI (bw)", bwInstallURL, err, "", nil)
	if !errors.Is(cerr, errs.ErrSecretUnavailable) {
		t.Fatalf("missing binary not mapped to ErrSecretUnavailable: %v", cerr)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := r.Run(cctx, nil, "sh", "-c", "sleep 5"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	long := strings.Repeat("x", 2000)
	if got := SanitizeStderr(long); len(got) > maxStderr+3 {
		t.Fatalf("stderr not truncated: %d", len(got))
	}
}
