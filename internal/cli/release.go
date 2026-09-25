package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
)

func newReleaseCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases"},
		Short:   "Work with releases",
		Long:    "Work with releases. Provider limitations apply: GitLab has no draft or prerelease flags, Bitbucket Cloud has no releases.",
	}
	cmd.AddCommand(newReleaseListCmd(f), newReleaseViewCmd(f), newReleaseCreateCmd(f), newReleaseDeleteCmd(f))
	return cmd
}

func newReleaseListCmd(f *Factory) *cobra.Command {
	var repo string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List releases",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			rp, err := capability[forge.ReleaseProvider](p, forge.CapReleases)
			if err != nil {
				return err
			}
			rels, err := spin(cmd.Context(), a, "Fetching releases...", func(ctx context.Context) ([]domain.Release, error) {
				return rp.ListReleases(ctx, ref, forge.ListOptions{Limit: limit})
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			if rels == nil {
				rels = []domain.Release{}
			}
			return a.Out.Result(rels, func() []string {
				out := make([]string, len(rels))
				for i, r := range rels {
					out[i] = r.Tag
				}
				return out
			}, func() error {
				providerHeader(a, p, ref.FullName())
				if len(rels) == 0 {
					a.Out.Println("No releases.")
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "Tag"}, {Header: "Name", Flex: true}, {Header: "Type"}, {Header: "Published"}}}
				for _, r := range rels {
					typ := ""
					switch {
					case r.Draft:
						typ = "draft"
					case r.Prerelease:
						typ = "prerelease"
					}
					t.Add(output.S(r.Tag, th.Accent), output.C(r.Name), output.S(typ, th.Warning), output.S(output.RelTime(firstTime(r.PublishedAt, r.CreatedAt)), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 30)
	return cmd
}

func newReleaseViewCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "view <tag>",
		Short: "Show a release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			rp, err := capability[forge.ReleaseProvider](p, forge.CapReleases)
			if err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Fetching release...", func(ctx context.Context) (*domain.Release, error) { return rp.GetRelease(ctx, ref, args[0]) })
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			return a.Out.Result(r, func() []string { return []string{r.Tag} }, func() error {
				th := a.Out.Theme()
				providerHeader(a, p, ref.FullName())
				fmt.Fprintf(a.IO.Out, "%s %s\n\n", th.Title.Render(firstNonEmpty(r.Name, r.Tag)), th.Muted.Render(r.Tag))
				a.Out.KeyValues([]output.KV{
					{Key: "Draft", Value: boolIf(r.Draft)}, {Key: "Prerelease", Value: boolIf(r.Prerelease)},
					{Key: "Author", Value: r.Author}, {Key: "Published", Value: output.RelTime(r.PublishedAt)}, {Key: "URL", Value: r.WebURL},
				})
				if len(r.Assets) > 0 {
					fmt.Fprintln(a.IO.Out)
					fmt.Fprintln(a.IO.Out, th.Heading.Render("Assets"))
					for _, as := range r.Assets {
						fmt.Fprintf(a.IO.Out, "  %s  %s\n", as.Name, th.Muted.Render(as.URL))
					}
				}
				if strings.TrimSpace(r.Body) != "" {
					fmt.Fprintln(a.IO.Out)
					fmt.Fprintln(a.IO.Out, strings.TrimSpace(r.Body))
				}
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func boolIf(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func newReleaseCreateCmd(f *Factory) *cobra.Command {
	var repo, notesFile string
	var req forge.CreateReleaseRequest
	cmd := &cobra.Command{
		Use:   "create <tag>",
		Short: "Create a release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			req.Tag = args[0]
			if notesFile != "" {
				var b []byte
				if notesFile == "-" {
					s, err := a.IO.ReadAllIn(10 << 20)
					if err != nil {
						return err
					}
					b = []byte(s)
				} else if b, err = os.ReadFile(notesFile); err != nil {
					return errs.Wrap(errs.ErrInvalidArgument, err, "read notes file")
				}
				req.Body = string(b)
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			rc, err := capability[forge.ReleaseCreator](p, forge.CapReleaseCreate)
			if err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Creating release...", func(ctx context.Context) (*domain.Release, error) { return rc.CreateRelease(ctx, ref, req) })
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			a.Out.Success("Created release %s", r.Tag)
			return a.Out.Result(r, func() []string { return []string{r.Tag} }, func() error {
				if r.WebURL != "" {
					a.Out.Println(r.WebURL)
				}
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	fs := cmd.Flags()
	fs.StringVarP(&req.Name, "title", "t", "", "release title")
	fs.StringVarP(&req.Body, "notes", "n", "", "release notes")
	fs.StringVarP(&notesFile, "notes-file", "F", "", "read release notes from file (\"-\" for stdin)")
	fs.StringVar(&req.Target, "target", "", "commit/branch to tag when the tag does not exist")
	fs.BoolVarP(&req.Draft, "draft", "d", false, "create a draft (GitHub)")
	fs.BoolVarP(&req.Prerelease, "prerelease", "p", false, "mark as prerelease (GitHub)")
	cmd.MarkFlagsMutuallyExclusive("notes", "notes-file")
	return cmd
}

func newReleaseDeleteCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "delete <tag>",
		Short: "Delete a release (asks for confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			rd, err := capability[forge.ReleaseDeleter](p, forge.CapReleaseDelete)
			if err != nil {
				return err
			}
			if err := confirm(a, fmt.Sprintf("Delete release %q from %s?\n\nThis action may be irreversible.\n\n", args[0], ref.FullName())); err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Deleting release...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, rd.DeleteRelease(ctx, ref, args[0])
			}); err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			a.Out.Success("Deleted release %s", args[0])
			return a.Out.Result(map[string]string{"deleted": args[0]}, func() []string { return []string{args[0]} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}
