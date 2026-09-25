package app

import (
	"net/http"
	"os"

	"github.com/SurajMazar/trove-cli/internal/cache"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/git"
	"github.com/SurajMazar/trove-cli/internal/logging"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Deps are the pluggable parts of the application. main wires the built-in
// forge drivers and secret providers; tests wire fakes.
type Deps struct {
	RegisterDrivers func(*forge.Registry)
	RegisterSecrets func(*secrets.Resolver, config.SecretsConfig)
	Transport       http.RoundTripper
	Getenv          func(string) string
	CacheDir        string
}

// Build loads configuration and composes the application.
func Build(opts Options, tio *terminal.IO, deps Deps) (*App, error) {
	getenv := deps.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path := opts.ConfigPath
	if path == "" {
		if p := getenv("TROVE_CONFIG"); p != "" {
			path = p
		} else {
			var err error
			if path, err = config.DefaultPath(); err != nil {
				return nil, err
			}
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if opts.Provider == "" {
		opts.Provider = getenv("TROVE_PROVIDER")
	}
	if opts.LogLevel == "" {
		opts.LogLevel = getenv("TROVE_LOG_LEVEL")
	}
	if opts.Debug {
		opts.LogLevel = "debug"
	}
	level, enabled, err := logging.ParseLevel(opts.LogLevel)
	if err != nil {
		return nil, errs.Wrap(errs.ErrInvalidArgument, err, "TROVE_LOG_LEVEL")
	}
	if opts.Output == "" {
		src := getenv("TROVE_OUTPUT")
		if src == "" {
			src = cfg.Output.Format
		}
		mode, err := output.ParseMode(src)
		if err != nil {
			return nil, errs.Wrap(errs.ErrInvalidArgument, err, "output mode")
		}
		opts.Output = mode
	}
	if opts.Output != output.ModeHuman {
		// Machine output must never be interleaved with prompts or TUIs.
		tio.Interactive = false
	}
	if opts.NonInteractive {
		tio.Interactive = false
	}

	reg := forge.NewRegistry()
	if deps.RegisterDrivers != nil {
		deps.RegisterDrivers(reg)
	}
	res := secrets.NewResolver()
	res.Register(secrets.NewEnvProviderWith(func(k string) (string, bool) {
		v := getenv(k)
		return v, v != ""
	}))
	if deps.RegisterSecrets != nil {
		deps.RegisterSecrets(res, cfg.Secrets)
	}

	cacheDir := deps.CacheDir
	if cacheDir == "" {
		cacheDir = cache.DefaultDir()
	}
	exe, _ := os.Executable()
	a := &App{
		Opts:       opts,
		Config:     cfg,
		Registry:   reg,
		Secrets:    res,
		IO:         tio,
		Out:        output.New(tio, opts.Output),
		Log:        logging.New(tio.Err, level, enabled),
		Cache:      cache.New(cacheDir, cfg.CacheTTL(), cfg.CacheEnabled() && !opts.NoCache),
		Git:        git.New(),
		Transport:  deps.Transport,
		Getenv:     getenv,
		Executable: exe,
	}
	return a, nil
}
