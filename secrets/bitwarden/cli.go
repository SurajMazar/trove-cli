package bitwarden

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

const (
	bwInstallURL = "https://bitwarden.com/help/cli/"
	bwUnlockHint = `export BW_SESSION="$(bw unlock --raw)"`
	bwLoginHint  = "bw login"
	// bwLoginType is Bitwarden's cipher type for Login items.
	bwLoginType = 1
)

// cliBackend talks to the Bitwarden Password Manager through bw.
type cliBackend struct {
	path        string
	folder      string // "" means no folder
	orgID       string
	collection  string
	syncOnStart bool
	run         Runner
	env         func(string) string

	sem semaphore
	// The fields below are guarded by sem.
	unlocked bool
	synced   bool
	folderID string // cached once the folder is known to exist
}

func newCLIBackend(opts Options, folder string) *cliBackend {
	return &cliBackend{
		path:        opts.CLIPath,
		folder:      folder,
		orgID:       opts.OrganizationID,
		collection:  opts.CollectionID,
		syncOnStart: opts.SyncOnStart,
		run:         opts.Runner,
		env:         opts.Env,
		sem:         newSemaphore(),
	}
}

func (b *cliBackend) description() string {
	d := "Bitwarden Password Manager (bw CLI"
	if b.folder != "" {
		d += fmt.Sprintf(", folder %q", b.folder)
	}
	if b.orgID != "" {
		d += fmt.Sprintf(", organization %s, collection %s", b.orgID, b.collection)
	}
	return d + ")"
}

// bwStatus is the JSON printed by `bw status`.
type bwStatus struct {
	ServerURL *string `json:"serverUrl"`
	UserEmail string  `json:"userEmail"`
	Status    string  `json:"status"` // unauthenticated | locked | unlocked
}

// bwItem holds the fields Trove inspects; raw keeps the full object so that
// edits round-trip every field Trove does not know about.
type bwItem struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Type           int     `json:"type"`
	FolderID       *string `json:"folderId"`
	OrganizationID *string `json:"organizationId"`
	Login          *struct {
		Password *string `json:"password"`
	} `json:"login"`

	raw json.RawMessage
}

type bwFolder struct {
	ID   *string `json:"id"`
	Name string  `json:"name"`
}

// bw runs a bw subcommand with --nointeraction so it never prompts. The
// session key is never passed on the command line: bw reads BW_SESSION from
// the inherited environment.
func (b *cliBackend) bw(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	args = append(args, "--nointeraction")
	out, _, err := b.run.Run(ctx, stdin, b.path, args...)
	return out, err
}

func (b *cliBackend) cmdErr(op string, err error, secret string) error {
	return commandError(op, "Bitwarden CLI (bw)", bwInstallURL, err, secret, b.classify)
}

// classify maps bw's own error messages for a locked or logged-out vault.
// It runs with b.sem held; a session that expired mid-run forgets the cached
// unlocked status so the next call re-checks `bw status`.
func (b *cliBackend) classify(re *RunError) error {
	s := strings.ToLower(re.Stderr)
	switch {
	case strings.Contains(s, "you are not logged in"):
		b.unlocked = false
		return b.unauthenticated()
	case strings.Contains(s, "vault is locked"), strings.Contains(s, "master password"):
		b.unlocked = false
		return b.locked()
	}
	return nil
}

func (b *cliBackend) locked() error {
	msg := "Bitwarden vault is locked"
	if b.env("BW_SESSION") == "" {
		msg += " (BW_SESSION is not set)"
	} else {
		msg += " (BW_SESSION is set but invalid or expired)"
	}
	return &errs.Error{Kind: errs.ErrSecretProviderLocked, Message: msg, Hint: bwUnlockHint}
}

func (b *cliBackend) unauthenticated() error {
	return &errs.Error{Kind: errs.ErrSecretProviderLocked, Message: "Bitwarden CLI is not logged in", Hint: bwLoginHint}
}

// ready ensures the vault is unlocked (cached for the provider's lifetime once
// it is) and performs the optional one-time sync. Callers must hold b.sem.
func (b *cliBackend) ready(ctx context.Context) error {
	if !b.unlocked {
		out, err := b.bw(ctx, nil, "status")
		if err != nil {
			return b.cmdErr("check Bitwarden vault status", err, "")
		}
		var st bwStatus
		if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
			return errs.New(errs.ErrSecretUnavailable, "bitwarden: could not parse `bw status` output; is %q the Bitwarden CLI?", b.path)
		}
		switch st.Status {
		case "unlocked":
			b.unlocked = true
		case "locked":
			return b.locked()
		case "unauthenticated":
			return b.unauthenticated()
		default:
			return errs.New(errs.ErrSecretUnavailable, "bitwarden: unexpected vault status %q from `bw status`", st.Status)
		}
	}
	if b.syncOnStart && !b.synced {
		if _, err := b.bw(ctx, nil, "sync"); err != nil {
			return b.cmdErr("sync Bitwarden vault", err, "")
		}
		b.synced = true
	}
	return nil
}

// lock acquires the semaphore and makes sure the vault is usable.
func (b *cliBackend) lock(ctx context.Context) (func(), error) {
	if err := b.sem.acquire(ctx); err != nil {
		return nil, err
	}
	if err := b.ready(ctx); err != nil {
		b.sem.release()
		return nil, err
	}
	return b.sem.release, nil
}

func (b *cliBackend) check(ctx context.Context) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

// lookupFolder returns the configured folder's id. found is false when no
// folder is configured or it does not exist (and create is false).
func (b *cliBackend) lookupFolder(ctx context.Context, create bool) (id string, found bool, err error) {
	if b.folder == "" {
		return "", false, nil
	}
	if b.folderID != "" {
		return b.folderID, true, nil
	}
	out, err := b.bw(ctx, nil, "list", "folders", "--search", b.folder)
	if err != nil {
		return "", false, b.cmdErr("list Bitwarden folders", err, "")
	}
	var folders []bwFolder
	if err := json.Unmarshal(bytes.TrimSpace(out), &folders); err != nil {
		return "", false, errs.New(errs.ErrProviderAPI, "bitwarden: could not parse `bw list folders` output")
	}
	var ids []string
	for _, f := range folders {
		if f.ID != nil && *f.ID != "" && f.Name == b.folder {
			ids = append(ids, *f.ID)
		}
	}
	switch {
	case len(ids) == 1:
		b.folderID = ids[0]
		return b.folderID, true, nil
	case len(ids) > 1:
		return "", false, errs.New(errs.ErrConflict,
			"bitwarden: found %d folders named %q (ids %s); rename or remove the duplicates", len(ids), b.folder, strings.Join(ids, ", "))
	case !create:
		return "", false, nil
	}

	payload, err := json.Marshal(map[string]string{"name": b.folder})
	if err != nil {
		return "", false, err
	}
	out, err = b.bw(ctx, encode(payload), "create", "folder")
	if err != nil {
		return "", false, b.cmdErr(fmt.Sprintf("create Bitwarden folder %q", b.folder), err, "")
	}
	var created bwFolder
	if err := json.Unmarshal(bytes.TrimSpace(out), &created); err != nil || created.ID == nil || *created.ID == "" {
		return "", false, errs.New(errs.ErrProviderAPI, "bitwarden: could not parse `bw create folder` output")
	}
	b.folderID = *created.ID
	return b.folderID, true, nil
}

// find returns the single Login item named key in the configured scope, or
// nil when there is none. Callers must hold b.sem.
func (b *cliBackend) find(ctx context.Context, key string) (*bwItem, error) {
	folderID, found, err := b.lookupFolder(ctx, false)
	if err != nil {
		return nil, err
	}
	if b.folder != "" && !found {
		return nil, nil // the folder does not exist yet, so neither does the item
	}
	// --search is a substring match over several fields; filter exactly below.
	// Other filters are applied client-side because bw ORs multiple filters.
	out, err := b.bw(ctx, nil, "list", "items", "--search", key)
	if err != nil {
		return nil, b.cmdErr("list Bitwarden items", err, "")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(out), &raws); err != nil {
		return nil, errs.New(errs.ErrProviderAPI, "bitwarden: could not parse `bw list items` output")
	}
	var matches []*bwItem
	for _, raw := range raws {
		var it bwItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, errs.New(errs.ErrProviderAPI, "bitwarden: could not parse an item in `bw list items` output")
		}
		if it.Name != key {
			continue
		}
		if b.folder != "" && (it.FolderID == nil || *it.FolderID != folderID) {
			continue
		}
		if b.orgID != "" && (it.OrganizationID == nil || *it.OrganizationID != b.orgID) {
			continue
		}
		it.raw = raw
		matches = append(matches, &it)
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, errs.New(errs.ErrConflict,
			"bitwarden: found %d items named %q%s (ids %s); Trove cannot tell which one to use, remove or rename the duplicates",
			len(matches), key, b.scopeText(), strings.Join(ids, ", "))
	}
	it := matches[0]
	if it.Type != bwLoginType || it.Login == nil {
		return nil, errs.New(errs.ErrInvalidConfiguration,
			"bitwarden: item %q (id %s) is not a Login item; Trove stores secrets in the password of Login items", key, it.ID)
	}
	return it, nil
}

func (b *cliBackend) scopeText() string {
	if b.folder != "" {
		return fmt.Sprintf(" in folder %q", b.folder)
	}
	return ""
}

func (b *cliBackend) get(ctx context.Context, key string) (string, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	it, err := b.find(ctx, key)
	if err != nil {
		return "", err
	}
	if it == nil {
		return "", secrets.NotFound(providerName, key)
	}
	// Only the password is the value: notes carry Trove's marker.
	if it.Login.Password == nil {
		return "", nil
	}
	return *it.Login.Password, nil
}

func (b *cliBackend) exists(ctx context.Context, key string) (bool, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	it, err := b.find(ctx, key)
	return it != nil, err
}

func (b *cliBackend) set(ctx context.Context, key, value string) error {
	if !utf8.ValidString(value) {
		return errs.New(errs.ErrInvalidArgument, "bitwarden: the value for %q is not valid UTF-8 text", key)
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	it, err := b.find(ctx, key)
	if err != nil {
		return err
	}
	if it != nil {
		payload, err := withPassword(it.raw, value)
		if err != nil {
			return errs.New(errs.ErrProviderAPI, "bitwarden: could not update item %q: unexpected item JSON", key)
		}
		// The item is passed base64-encoded on stdin, never as an argument.
		if _, err := b.bw(ctx, encode(payload), "edit", "item", it.ID); err != nil {
			return b.cmdErr(fmt.Sprintf("update Bitwarden item %q", key), err, value)
		}
		return nil
	}

	folderID, _, err := b.lookupFolder(ctx, true)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(b.newItem(key, value, folderID))
	if err != nil {
		return err
	}
	if _, err := b.bw(ctx, encode(payload), "create", "item"); err != nil {
		return b.cmdErr(fmt.Sprintf("create Bitwarden item %q", key), err, value)
	}
	return nil
}

func (b *cliBackend) del(ctx context.Context, key string) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	it, err := b.find(ctx, key)
	if err != nil || it == nil {
		return err
	}
	// Moves the item to the trash; permanent deletion is left to the user.
	if _, err := b.bw(ctx, nil, "delete", "item", it.ID); err != nil {
		return b.cmdErr(fmt.Sprintf("delete Bitwarden item %q", key), err, "")
	}
	return nil
}

// newItem builds a Login item following the shape of `bw get template item`
// with `bw get template item.login` as its login.
func (b *cliBackend) newItem(key, value, folderID string) map[string]any {
	item := map[string]any{
		"organizationId": nil,
		"collectionIds":  nil,
		"folderId":       nil,
		"type":           bwLoginType,
		"name":           key,
		"notes":          managedNote,
		"favorite":       false,
		"fields":         []any{},
		"login": map[string]any{
			"uris":     []any{},
			"username": nil,
			"password": value,
			"totp":     nil,
		},
		"secureNote": nil,
		"card":       nil,
		"identity":   nil,
		"reprompt":   0,
	}
	if folderID != "" {
		item["folderId"] = folderID
	}
	if b.orgID != "" {
		item["organizationId"] = b.orgID
		item["collectionIds"] = []string{b.collection}
	}
	return item
}

// withPassword returns raw with login.password replaced, preserving every
// other field of the item exactly.
func withPassword(raw json.RawMessage, value string) ([]byte, error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	login := map[string]json.RawMessage{}
	if l, ok := item["login"]; ok && string(l) != "null" {
		if err := json.Unmarshal(l, &login); err != nil {
			return nil, err
		}
	}
	pw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	login["password"] = pw
	if item["login"], err = json.Marshal(login); err != nil {
		return nil, err
	}
	return json.Marshal(item)
}

// encode is the equivalent of `bw encode`: standard base64 of the JSON.
func encode(payload []byte) []byte {
	out := make([]byte, base64.StdEncoding.EncodedLen(len(payload)))
	base64.StdEncoding.Encode(out, payload)
	return out
}
