package bitwarden

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

// Backend names accepted by Options.Backend.
const (
	BackendCLI            = "cli"
	BackendSecretsManager = "secrets-manager"
)

// DefaultFolder is the Password Manager folder used when Options.Folder is empty.
const DefaultFolder = "trove"

// managedNote marks items created by Trove.
const managedNote = "Managed by trove"

const providerName = "bitwarden"

// Options configures the Bitwarden provider.
type Options struct {
	// Backend is "cli" (Password Manager via bw, the default) or
	// "secrets-manager" (Secrets Manager via bws).
	Backend string
	// CLIPath is the bw or bws executable; defaults to "bw" / "bws" on PATH.
	CLIPath string
	// Folder is the Password Manager folder holding Trove's items
	// (default "trove"). Ignored by the secrets-manager backend.
	Folder string
	// NoFolder stores and looks up items outside of any folder.
	NoFolder bool
	// OrganizationID and CollectionID place new items in an organization
	// collection (cli backend). Both must be set together, because Bitwarden
	// requires organization items to belong to at least one collection.
	OrganizationID string
	CollectionID   string
	// ProjectID is the Secrets Manager project new secrets are created in and
	// that lookups are scoped to (secrets-manager backend).
	ProjectID string
	// SyncOnStart runs `bw sync` once before the provider's first operation.
	SyncOnStart bool
	// Runner executes the CLI; defaults to ExecRunner.
	Runner Runner
	// Env looks up environment variables (BW_SESSION, BWS_ACCESS_TOKEN) to
	// produce precise diagnostics; defaults to os.Getenv. The CLIs themselves
	// read these variables from the inherited process environment.
	Env func(string) string
}

// backend is the per-CLI implementation behind Provider.
type backend interface {
	get(ctx context.Context, key string) (string, error)
	set(ctx context.Context, key, value string) error
	del(ctx context.Context, key string) error
	exists(ctx context.Context, key string) (bool, error)
	check(ctx context.Context) error
	description() string
}

// Provider is the Bitwarden secret provider. It is safe for concurrent use.
type Provider struct {
	b backend
}

var (
	_ secrets.SecretProvider = (*Provider)(nil)
	_ secrets.Checker        = (*Provider)(nil)
	_ secrets.Describer      = (*Provider)(nil)
)

// New validates opts and returns a provider. It does not run any command.
func New(opts Options) (*Provider, error) {
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	opts.Backend = strings.ToLower(strings.TrimSpace(opts.Backend))
	opts.OrganizationID = strings.TrimSpace(opts.OrganizationID)
	opts.CollectionID = strings.TrimSpace(opts.CollectionID)
	opts.ProjectID = strings.TrimSpace(opts.ProjectID)
	switch opts.Backend {
	case "", BackendCLI:
		if (opts.OrganizationID == "") != (opts.CollectionID == "") {
			return nil, errs.New(errs.ErrInvalidConfiguration,
				"bitwarden: organization_id and collection_id must be set together (organization items must belong to a collection)")
		}
		if opts.CLIPath == "" {
			opts.CLIPath = "bw"
		}
		folder := strings.TrimSpace(opts.Folder)
		if opts.NoFolder {
			folder = ""
		} else if folder == "" {
			folder = DefaultFolder
		}
		return &Provider{b: newCLIBackend(opts, folder)}, nil
	case BackendSecretsManager:
		if opts.CLIPath == "" {
			opts.CLIPath = "bws"
		}
		return &Provider{b: newSMBackend(opts)}, nil
	default:
		return nil, errs.New(errs.ErrInvalidConfiguration,
			"bitwarden: unknown backend %q (want %q or %q)", opts.Backend, BackendCLI, BackendSecretsManager)
	}
}

// Name implements secrets.SecretProvider.
func (p *Provider) Name() string { return providerName }

// Description implements secrets.Describer.
func (p *Provider) Description() string { return p.b.description() }

// Check reports whether the configured CLI is installed and unlocked.
func (p *Provider) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.b.check(ctx)
}

// Get returns the value stored under key.
func (p *Provider) Get(ctx context.Context, key string) (string, error) {
	key, err := prepare(ctx, key)
	if err != nil {
		return "", err
	}
	return p.b.get(ctx, key)
}

// Set creates or replaces the value stored under key.
func (p *Provider) Set(ctx context.Context, key, value string) error {
	key, err := prepare(ctx, key)
	if err != nil {
		return err
	}
	return p.b.set(ctx, key, value)
}

// Delete removes key. Deleting a missing key succeeds.
func (p *Provider) Delete(ctx context.Context, key string) error {
	key, err := prepare(ctx, key)
	if err != nil {
		return err
	}
	return p.b.del(ctx, key)
}

// Exists reports whether key is stored.
func (p *Provider) Exists(ctx context.Context, key string) (bool, error) {
	key, err := prepare(ctx, key)
	if err != nil {
		return false, err
	}
	return p.b.exists(ctx, key)
}

func prepare(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errs.New(errs.ErrInvalidArgument, "bitwarden: secret key is empty")
	}
	return key, nil
}

// semaphore is a context-aware mutex. The Bitwarden CLIs keep local state
// that concurrent invocations can corrupt, so all calls are serialized.
type semaphore chan struct{}

func newSemaphore() semaphore { return make(semaphore, 1) }

func (s semaphore) acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s semaphore) release() { <-s }

// commandError converts a Runner error into a Trove error. secret, when
// non-empty, is scrubbed from anything that ends up in the message. classify
// may map CLI-specific stderr to a more precise error; it returns nil to fall
// back to a generic provider error.
func commandError(op, cli, installURL string, err error, secret string, classify func(*RunError) error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("bitwarden: %s: %w", op, err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &errs.Error{
			Kind:    errs.ErrSecretUnavailable,
			Op:      op,
			Message: fmt.Sprintf("%s not found; install it from %s", cli, installURL),
			Hint:    "install the CLI and make sure it is on PATH",
		}
	}
	var re *RunError
	if errors.As(err, &re) {
		re = re.withoutSecret(secret)
		if re.Err != nil {
			return &errs.Error{Kind: errs.ErrSecretUnavailable, Op: op, Message: re.Error()}
		}
		if classify != nil {
			if cerr := classify(re); cerr != nil {
				return cerr
			}
		}
		return &errs.Error{Kind: errs.ErrProviderAPI, Op: op, Message: "Bitwarden " + re.Error()}
	}
	// A custom Runner returned something else; keep only its message, scrubbed.
	return &errs.Error{Kind: errs.ErrProviderAPI, Op: op, Message: scrub(SanitizeStderr(err.Error()), secret)}
}
