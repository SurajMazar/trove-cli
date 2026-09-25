package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/services"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/version"
)

// --- config -------------------------------------------------------------------

func newConfigCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and edit the configuration file",
		Long: `Inspect and edit ~/.config/trove/config.yaml (or $TROVE_CONFIG). Keys use dots:
clone.concurrency, providers.github-personal.host, secrets.provider.
Secret values are never stored in the configuration; keys that look like
secrets are rejected.`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "path", Short: "Print the configuration file path", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				p := a.Config.Path()
				return a.Out.Result(map[string]string{"path": p}, func() []string { return []string{p} }, func() error {
					fmt.Fprintln(a.IO.Out, p)
					return nil
				})
			},
		},
		&cobra.Command{
			Use: "show", Short: "Print the configuration (never contains secrets)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				if a.Out.JSON() {
					return a.Out.PrintJSON(configJSON(a.Config))
				}
				b, err := a.Config.Marshal()
				if err != nil {
					return err
				}
				_, err = a.IO.Out.Write(b)
				return err
			},
		},
		&cobra.Command{
			Use: "get <key>", Short: "Print a configuration value", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				v, err := a.Config.Get(args[0])
				if err != nil {
					return err
				}
				return a.Out.Result(map[string]string{"key": args[0], "value": v}, func() []string { return []string{v} }, func() error {
					fmt.Fprintln(a.IO.Out, v)
					return nil
				})
			},
		},
		&cobra.Command{
			Use: "set <key> <value>", Short: "Set a configuration value", Args: cobra.ExactArgs(2),
			Example: "  trove config set clone.concurrency 8\n  trove config set clone.protocol ssh\n  trove config set providers.gl.api_base_url https://gitlab.example.com/api/v4",
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				if err := a.Config.Set(args[0], args[1]); err != nil {
					return err
				}
				if ps := a.Config.Validate(a.ValidationRules()); len(ps) > 0 {
					return configProblemsError(ps)
				}
				if err := a.Config.Save(); err != nil {
					return err
				}
				_ = a.Cache.Invalidate()
				a.Out.Success("Set %s", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use: "unset <key>", Short: "Remove a configuration value", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				if err := a.Config.Unset(args[0]); err != nil {
					return err
				}
				if ps := a.Config.Validate(a.ValidationRules()); len(ps) > 0 {
					return configProblemsError(ps)
				}
				if err := a.Config.Save(); err != nil {
					return err
				}
				a.Out.Success("Unset %s", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use: "validate", Short: "Validate the configuration", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				ps := a.Config.Validate(a.ValidationRules())
				// Drivers validate their own provider-specific settings.
				for _, n := range a.Config.ProviderNames() {
					if _, err := a.OpenOther(n); err != nil {
						ps = append(ps, config.Problem{Path: "providers." + n, Message: err.Error()})
					}
				}
				if ps == nil {
					ps = []config.Problem{}
				}
				if err := a.Out.Result(map[string]any{"valid": len(ps) == 0, "path": a.Config.Path(), "problems": ps}, func() []string {
					var out []string
					for _, p := range ps {
						out = append(out, p.String())
					}
					return out
				}, func() error {
					th := a.Out.Theme()
					if len(ps) == 0 {
						fmt.Fprintf(a.IO.Out, "%s %s is valid\n", th.Success.Render(terminal.SymSuccess), a.Config.Path())
						return nil
					}
					for _, p := range ps {
						fmt.Fprintf(a.IO.Out, "%s %s %s\n", th.Error.Render(terminal.SymError), th.Bold.Render(p.Path), p.Message)
					}
					return nil
				}); err != nil {
					return err
				}
				if len(ps) > 0 {
					return &errs.Error{Kind: errs.ErrInvalidConfiguration, Message: fmt.Sprintf("%d problem(s) found in %s", len(ps), a.Config.Path())}
				}
				return nil
			},
		},
	)
	return cmd
}

// configJSON converts the YAML-shaped config into a JSON-friendly map.
func configJSON(c *config.Config) any {
	b, err := c.Marshal()
	if err != nil {
		return nil
	}
	v, err := yamlToJSON(b)
	if err != nil {
		return nil
	}
	return v
}

// --- doctor -------------------------------------------------------------------

func newDoctorCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [alias]",
		Short: "Diagnose configuration, git, secret providers and provider connectivity",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			only := ""
			if len(args) == 1 {
				if _, err := a.Config.Provider(args[0]); err != nil {
					return err
				}
				only = args[0]
			}
			rep, _ := spin(cmd.Context(), a, "Running checks...", func(ctx context.Context) (services.DoctorReport, error) {
				return services.Doctor(ctx, a, only), nil
			})
			if err := a.Out.Result(rep, func() []string {
				var out []string
				for _, c := range rep.Checks {
					if c.Status == services.CheckFail || c.Status == services.CheckWarn {
						out = append(out, string(c.Status)+"\t"+c.Name)
					}
				}
				return out
			}, func() error {
				renderDoctor(a.IO, a.Out, rep)
				return nil
			}); err != nil {
				return err
			}
			if rep.Failures > 0 {
				return &errs.Error{Kind: errs.ErrProviderAPI, Message: fmt.Sprintf("%d check(s) failed", rep.Failures), Status: -1}
			}
			return nil
		},
	}
}

func renderDoctor(tio *terminal.IO, out *output.Printer, rep services.DoctorReport) {
	th := out.Theme()
	fmt.Fprintln(tio.Out, th.Title.Render("Trove Doctor"))
	fmt.Fprintln(tio.Out)
	width := 0
	for _, c := range rep.Checks {
		if l := len([]rune(c.Name)); l > width {
			width = l
		}
	}
	width = min(width, 40)
	for _, c := range rep.Checks {
		var sym string
		switch c.Status {
		case services.CheckOK:
			sym = th.Success.Render(terminal.SymSuccess)
		case services.CheckWarn:
			sym = th.Warning.Render(terminal.SymWarning)
		case services.CheckFail:
			sym = th.Error.Render(terminal.SymError)
		default:
			sym = th.Muted.Render("−")
		}
		name := c.Name
		if pad := width - len([]rune(name)); pad > 0 {
			name += strings.Repeat(" ", pad)
		}
		fmt.Fprintf(tio.Out, "%s %s  %s\n", sym, name, th.Muted.Render(c.Detail))
		if c.Hint != "" && c.Status != services.CheckOK {
			fmt.Fprintf(tio.Out, "  %s %s\n", strings.Repeat(" ", width), th.Accent.Render("→ "+c.Hint))
		}
	}
	fmt.Fprintln(tio.Out)
	switch {
	case rep.Failures == 0 && rep.Warnings == 0:
		fmt.Fprintln(tio.Out, th.Success.Render("No issues found."))
	default:
		var parts []string
		if rep.Failures > 0 {
			parts = append(parts, th.Error.Render(plural(rep.Failures, "error")))
		}
		if rep.Warnings > 0 {
			parts = append(parts, th.Warning.Render(plural(rep.Warnings, "warning")))
		}
		fmt.Fprintln(tio.Out, strings.Join(parts, ", ")+" found.")
	}
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// --- version ------------------------------------------------------------------

func newVersionCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			v := version.Get()
			return a.Out.Result(v, func() []string { return []string{v.Version} }, func() error {
				th := a.Out.Theme()
				fmt.Fprintf(a.IO.Out, "%s %s\n", th.Title.Render("trove"), v.Version)
				a.Out.KeyValues([]output.KV{
					{Key: "Commit", Value: v.Commit}, {Key: "Built", Value: v.BuildDate},
					{Key: "Go", Value: v.GoVersion}, {Key: "Platform", Value: v.OS + "/" + v.Arch},
				})
				return nil
			})
		},
	}
}
