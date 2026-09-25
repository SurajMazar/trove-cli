// Package cli defines Trove's Cobra command tree. Commands are thin: they
// parse flags, resolve the provider through the application core, check
// capabilities, call the provider, and render results through the output
// package. Interactive views come from the tui package.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Factory lazily builds the App from global flags.
type Factory struct {
	Deps app.Deps
	// IO overrides the system streams (tests).
	IO *terminal.IO

	opts        app.Options
	jsonOut     bool
	quiet       bool
	color       string
	interactive bool

	app *app.App
}

// App returns the composed application, building it on first use.
func (f *Factory) App() (*app.App, error) {
	if f.app != nil {
		return f.app, nil
	}
	tio := f.IO
	if tio == nil {
		mode := terminal.ColorMode(f.color)
		if mode == "" {
			mode = terminal.ColorAuto
		}
		tio = terminal.System(f.opts.NonInteractive, mode)
	}
	if f.jsonOut && f.quiet {
		return nil, errs.New(errs.ErrInvalidArgument, "--json and --quiet cannot be combined")
	}
	opts := f.opts
	switch {
	case f.jsonOut:
		opts.Output = output.ModeJSON
	case f.quiet:
		opts.Output = output.ModeQuiet
	}
	a, err := app.Build(opts, tio, f.Deps)
	if err != nil {
		return nil, err
	}
	f.app = a
	return a, nil
}

// NewRoot builds the command tree.
func NewRoot(f *Factory) *cobra.Command {
	root := &cobra.Command{
		Use:   "trove",
		Short: "One CLI for GitHub, GitLab, Bitbucket and other Git forges",
		Long: `Trove manages repositories, pull/merge requests, issues, pipelines, releases
and more across GitHub, GitLab, Bitbucket and custom forges from one terminal
interface. Credentials live in a secret provider (Bitwarden, macOS Keychain,
Linux Secret Service), never in the configuration file.

Run "trove" without arguments in a terminal to open the dashboard.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			if !a.IO.Interactive {
				return cmd.Help()
			}
			return runDashboard(cmd.Context(), f, a)
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if f.interactive {
				a, err := f.App()
				if err != nil {
					return err
				}
				return pickProvider(cmd.Context(), a, false)
			}
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&f.opts.ConfigPath, "config", "", "configuration file (default $TROVE_CONFIG, $XDG_CONFIG_HOME/trove/config.yaml or ~/.config/trove/config.yaml)")
	pf.StringVarP(&f.opts.Provider, "provider", "P", "", "provider account to use (default: $TROVE_PROVIDER or default_provider)")
	pf.BoolVar(&f.jsonOut, "json", false, "output machine-readable JSON")
	pf.BoolVarP(&f.quiet, "quiet", "q", false, "print only identifiers")
	pf.BoolVar(&f.opts.NonInteractive, "non-interactive", false, "never prompt or open interactive views")
	pf.BoolVarP(&f.opts.Yes, "yes", "y", false, "assume yes for confirmations (automation)")
	pf.BoolVar(&f.opts.Debug, "debug", false, "enable debug logging (secrets are always redacted)")
	pf.StringVar(&f.opts.LogLevel, "log-level", "", "log level: debug, info, warn, error (default $TROVE_LOG_LEVEL or off)")
	pf.BoolVar(&f.opts.NoCache, "no-cache", false, "bypass the response cache")
	pf.StringVar(&f.color, "color", "auto", "color output: auto, always, never (NO_COLOR is respected)")
	pf.BoolVar(&f.interactive, "interactive", false, "choose the provider interactively")

	_ = root.RegisterFlagCompletionFunc("provider", completeProviders(f))
	_ = root.RegisterFlagCompletionFunc("color", cobra.FixedCompletions([]string{"auto", "always", "never"}, cobra.ShellCompDirectiveNoFileComp))

	root.AddGroup(
		&cobra.Group{ID: "core", Title: "Core commands:"},
		&cobra.Group{ID: "forge", Title: "Forge commands:"},
		&cobra.Group{ID: "setup", Title: "Setup commands:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("core", newRepoCmd(f), newPRCmd(f), newIssueCmd(f), newPipelineCmd(f), newReleaseCmd(f))
	add("forge", newNamespaceCmd(f), newSearchCmd(f), newNotificationCmd(f), newSnippetCmd(f), newKeyCmd(f), newSettingsCmd(f))
	add("setup", newProviderCmd(f), newAuthCmd(f), newConfigCmd(f), newDoctorCmd(f), newVersionCmd(f))
	strictGroups(root)
	root.SetHelpCommandGroupID("setup")
	root.SetCompletionCommandGroupID("setup")
	root.CompletionOptions.HiddenDefaultCmd = false
	return root
}

// strictGroups makes parent commands (repo, pr, ...) fail on unknown
// subcommands instead of printing help and exiting 0.
func strictGroups(c *cobra.Command) {
	for _, sub := range c.Commands() {
		strictGroups(sub)
	}
	if c.HasParent() && c.HasSubCommands() && c.Run == nil && c.RunE == nil {
		c.Args = cobra.ArbitraryArgs
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		}
	}
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, f *Factory, args []string) int {
	root := NewRoot(f)
	root.SetArgs(args)
	if f.IO != nil {
		root.SetOut(f.IO.Out)
		root.SetErr(f.IO.Err)
	}
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	if ctx.Err() != nil && !errors.Is(err, errs.ErrCanceled) {
		err = errs.Wrap(errs.ErrCanceled, ctx.Err(), "canceled")
	}
	mode := output.ModeHuman
	tio := f.IO
	debug := f.opts.Debug
	if f.app != nil {
		mode = f.app.Out.Mode
		tio = f.app.IO
	} else if f.jsonOut {
		mode = output.ModeJSON
	}
	if tio == nil {
		tio = terminal.System(true, terminal.ColorMode(f.color))
	}
	if isUsageError(err) {
		err = errs.Wrap(errs.ErrInvalidArgument, err, "%s", err.Error())
		if mode != output.ModeJSON {
			fmt.Fprintf(tio.Err, "%s\n", err.(*errs.Error).Message)
			fmt.Fprintf(tio.Err, "Run 'trove --help' for usage.\n")
			return errs.ExitCode(err)
		}
	}
	output.RenderError(tio, mode, err, debug)
	return errs.ExitCode(err)
}

func isUsageError(err error) bool {
	var e *errs.Error
	if errors.As(err, &e) {
		return false
	}
	s := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "accepts ", "requires at least", "requires at most", "flag needs an argument", "invalid argument", "required flag"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func completeProviders(f *Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		a, err := f.App()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, n := range a.Config.ProviderNames() {
			out = append(out, n+"\t"+a.Config.Providers[n].Type+" "+a.Config.Providers[n].Host)
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// Main is the entry point used by cmd/trove.
func Main(deps app.Deps) int {
	ctx, stop := signalContext()
	defer stop()
	return Execute(ctx, &Factory{Deps: deps}, os.Args[1:])
}
