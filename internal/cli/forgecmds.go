package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/cache"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
)

// --- namespaces -----------------------------------------------------------------

func newNamespaceCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "namespace",
		Aliases: []string{"namespaces", "ns", "org", "orgs", "group", "groups", "workspace", "workspaces"},
		Short:   "List organizations, groups and workspaces",
		Long: `Namespaces are the containers repositories live in: GitHub organizations and
users, GitLab groups and subgroups, Bitbucket workspaces.`,
	}
	var limit int
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List namespaces you belong to",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			np, err := capability[forge.NamespaceProvider](p, forge.CapNamespaces)
			if err != nil {
				return err
			}
			m := p.Metadata()
			key := cache.Key(m.Name, m.Host, "namespaces", strconv.Itoa(limit))
			nss, err := spin(cmd.Context(), a, "Fetching namespaces...", func(ctx context.Context) ([]domain.Namespace, error) {
				return cache.Fetch(ctx, a.Cache, key, func(ctx context.Context) ([]domain.Namespace, error) {
					return np.ListNamespaces(ctx, forge.ListOptions{Limit: limit})
				})
			})
			if err != nil {
				return errs.WithProvider(err, m.Name)
			}
			if nss == nil {
				nss = []domain.Namespace{}
			}
			return a.Out.Result(nss, func() []string {
				out := make([]string, len(nss))
				for i, n := range nss {
					out[i] = n.FullPath
				}
				return out
			}, func() error {
				providerHeader(a, p, "")
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "Path", Flex: true}, {Header: "Type"}, {Header: "Name", Flex: true, MinTerminal: 100}, {Header: "URL", MinTerminal: 140}}}
				for _, n := range nss {
					t.Add(output.C(n.FullPath), output.C(namespaceLabel(n)), output.S(n.Name, th.Muted), output.S(n.WebURL, th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	limitFlag(list, &limit, 0)
	view := &cobra.Command{
		Use:   "view <path>",
		Short: "Show a namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			np, err := capability[forge.NamespaceProvider](p, forge.CapNamespaces)
			if err != nil {
				return err
			}
			n, err := spin(cmd.Context(), a, "Fetching namespace...", func(ctx context.Context) (*domain.Namespace, error) { return np.GetNamespace(ctx, args[0]) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			return a.Out.Result(n, func() []string { return []string{n.FullPath} }, func() error {
				providerHeader(a, p, "")
				fmt.Fprintln(a.IO.Out, a.Out.Theme().Title.Render(n.FullPath))
				fmt.Fprintln(a.IO.Out)
				a.Out.KeyValues([]output.KV{
					{Key: "Type", Value: namespaceLabel(*n)}, {Key: "Name", Value: n.Name}, {Key: "ID", Value: n.ID},
					{Key: "Parent", Value: n.ParentPath}, {Key: "Description", Value: n.Description}, {Key: "URL", Value: n.WebURL},
				})
				return nil
			})
		},
	}
	cmd.AddCommand(list, view)
	return cmd
}

func namespaceLabel(n domain.Namespace) string {
	if n.Label != "" {
		return n.Label
	}
	return string(n.Type)
}

// --- search -------------------------------------------------------------------

func newSearchCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search repositories, issues and code",
		Long:  "Search where the provider supports it. Some providers require a scope (e.g. Bitbucket code search needs --namespace).",
	}
	mk := func(use string, aliases []string, kind domain.SearchKind, cap forge.Capability, short string) *cobra.Command {
		var namespace, repo string
		var limit int
		c := &cobra.Command{
			Use:     use + " <query>",
			Aliases: aliases,
			Short:   short,
			Args:    cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := f.App()
				if err != nil {
					return err
				}
				var p forge.Provider
				q := domain.SearchQuery{Kind: kind, Query: strings.Join(args, " "), Namespace: namespace, Limit: limit}
				if repo != "" {
					var ref domain.RepositoryRef
					if p, ref, err = resolveRepo(cmd.Context(), a, repo); err != nil {
						return err
					}
					q.Repository = &ref
				} else if p, err = a.Current(""); err != nil {
					return err
				}
				s, err := capability[forge.Searcher](p, cap)
				if err != nil {
					return err
				}
				res, err := spin(cmd.Context(), a, "Searching...", func(ctx context.Context) (*domain.SearchResult, error) { return s.Search(ctx, q) })
				if err != nil {
					return errs.WithProvider(err, p.Metadata().Name)
				}
				return a.Out.Result(res, func() []string { return searchQuiet(res) }, func() error {
					providerHeader(a, p, "")
					renderSearch(a, res)
					return nil
				})
			},
		}
		c.Flags().StringVarP(&namespace, "namespace", "n", "", "limit to a namespace (organization, group, workspace)")
		repoFlag(c, &repo)
		limitFlag(c, &limit, 30)
		return c
	}
	cmd.AddCommand(
		mk("repo", []string{"repos", "repositories"}, domain.SearchRepositories, forge.CapSearchRepos, "Search repositories"),
		mk("issue", []string{"issues"}, domain.SearchIssues, forge.CapSearchIssues, "Search issues"),
		mk("code", nil, domain.SearchCode, forge.CapSearchCode, "Search code"),
	)
	return cmd
}

func searchQuiet(r *domain.SearchResult) []string {
	var out []string
	for _, x := range r.Repositories {
		out = append(out, x.FullName)
	}
	for _, x := range r.Issues {
		out = append(out, x.WebURL)
	}
	for _, x := range r.Code {
		out = append(out, x.Repository+":"+x.Path)
	}
	return out
}

func renderSearch(a *app.App, r *domain.SearchResult) {
	th := a.Out.Theme()
	switch r.Kind {
	case domain.SearchRepositories:
		if len(r.Repositories) == 0 {
			a.Out.Println("No repositories found.")
			return
		}
		renderRepoTable(a, r.Repositories, false)
	case domain.SearchIssues:
		if len(r.Issues) == 0 {
			a.Out.Println("No issues found.")
			return
		}
		t := &output.Table{Columns: []output.Column{{Header: "#"}, {Header: "Title", Flex: true}, {Header: "State"}, {Header: "URL", MinTerminal: 120, Flex: true}}}
		for _, is := range r.Issues {
			t.Add(output.S("#"+strconv.Itoa(is.Number), th.Accent), output.C(is.Title), output.C(stateText(th, string(is.State))), output.S(is.WebURL, th.Muted))
		}
		a.Out.Table(t)
	case domain.SearchCode:
		if len(r.Code) == 0 {
			a.Out.Println("No code matches.")
			return
		}
		for _, c := range r.Code {
			fmt.Fprintf(a.IO.Out, "%s %s\n", th.Accent.Render(c.Repository), c.Path)
			if frag := strings.TrimSpace(c.Fragment); frag != "" {
				for _, line := range strings.Split(frag, "\n") {
					fmt.Fprintf(a.IO.Out, "  %s\n", th.Muted.Render(line))
				}
			}
		}
	}
	if r.Total > 0 {
		a.Out.Info("%d total matches", r.Total)
	}
}

// --- notifications ------------------------------------------------------------

func newNotificationCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "notification",
		Aliases: []string{"notifications", "todo", "todos"},
		Short:   "Notifications (GitHub) and To-Do items (GitLab)",
	}
	var all bool
	var limit int
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List unread notifications",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			np, err := capability[forge.NotificationProvider](p, forge.CapNotifications)
			if err != nil {
				return err
			}
			ns, err := spin(cmd.Context(), a, "Fetching notifications...", func(ctx context.Context) ([]domain.Notification, error) {
				return np.ListNotifications(ctx, forge.NotificationListOptions{ListOptions: forge.ListOptions{Limit: limit}, All: all})
			})
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if ns == nil {
				ns = []domain.Notification{}
			}
			return a.Out.Result(ns, func() []string {
				out := make([]string, len(ns))
				for i, n := range ns {
					out[i] = n.ID
				}
				return out
			}, func() error {
				providerHeader(a, p, "")
				if len(ns) == 0 {
					a.Out.Println("All caught up.")
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: ""}, {Header: "ID"}, {Header: "Title", Flex: true}, {Header: "Repository", MinTerminal: 100},
					{Header: "Reason", MinTerminal: 120}, {Header: "Updated"}}}
				for _, n := range ns {
					mark := output.C(" ")
					if n.Unread {
						mark = output.S("●", th.Accent)
					}
					t.Add(mark, output.S(n.ID, th.Muted), output.C(n.Title), output.C(n.Repository), output.S(n.Reason, th.Muted), output.S(output.RelTime(n.UpdatedAt), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include read notifications")
	limitFlag(list, &limit, 50)
	read := &cobra.Command{
		Use:   "read <id>",
		Short: "Show a notification and mark it as read",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			np, err := capability[forge.NotificationProvider](p, forge.CapNotifications)
			if err != nil {
				return err
			}
			n, err := spin(cmd.Context(), a, "Fetching notification...", func(ctx context.Context) (*domain.Notification, error) { return np.GetNotification(ctx, args[0]) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if n.Unread {
				if err := np.MarkNotificationRead(cmd.Context(), n.ID); err != nil {
					return errs.WithProvider(err, p.Metadata().Name)
				}
				n.Unread = false
			}
			return a.Out.Result(n, func() []string { return []string{n.ID} }, func() error {
				fmt.Fprintln(a.IO.Out, a.Out.Theme().Title.Render(n.Title))
				fmt.Fprintln(a.IO.Out)
				a.Out.KeyValues([]output.KV{{Key: "Type", Value: n.Type}, {Key: "Reason", Value: n.Reason}, {Key: "Repository", Value: n.Repository},
					{Key: "Updated", Value: output.RelTime(n.UpdatedAt)}, {Key: "URL", Value: n.WebURL}})
				return nil
			})
		},
	}
	var markAll bool
	markRead := &cobra.Command{
		Use:   "mark-read [id]",
		Short: "Mark a notification (or --all) as read",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			if (len(args) == 0) == !markAll {
				return errs.New(errs.ErrInvalidArgument, "pass a notification ID or --all")
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			np, err := capability[forge.NotificationProvider](p, forge.CapNotifications)
			if err != nil {
				return err
			}
			if markAll {
				err = np.MarkAllNotificationsRead(cmd.Context())
			} else {
				err = np.MarkNotificationRead(cmd.Context(), args[0])
			}
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			a.Out.Success("Marked as read")
			return a.Out.Result(map[string]any{"marked_read": true}, nil, func() error { return nil })
		},
	}
	markRead.Flags().BoolVar(&markAll, "all", false, "mark every notification as read")
	cmd.AddCommand(list, read, markRead)
	return cmd
}

// --- snippets -----------------------------------------------------------------

func newSnippetCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snippet",
		Aliases: []string{"snippets", "gist", "gists"},
		Short:   "Snippets (GitHub Gists, GitLab/Bitbucket snippets)",
	}
	var limit int
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your snippets",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			sp, err := capability[forge.SnippetProvider](p, forge.CapSnippets)
			if err != nil {
				return err
			}
			ss, err := spin(cmd.Context(), a, "Fetching snippets...", func(ctx context.Context) ([]domain.Snippet, error) {
				return sp.ListSnippets(ctx, forge.ListOptions{Limit: limit})
			})
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if ss == nil {
				ss = []domain.Snippet{}
			}
			return a.Out.Result(ss, func() []string {
				out := make([]string, len(ss))
				for i, s := range ss {
					out[i] = s.ID
				}
				return out
			}, func() error {
				providerHeader(a, p, "")
				if len(ss) == 0 {
					a.Out.Println("No snippets.")
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "ID"}, {Header: "Title", Flex: true}, {Header: "Files", Flex: true, MinTerminal: 100}, {Header: "Visibility"}, {Header: "Updated"}}}
				for _, s := range ss {
					var files []string
					for _, fl := range s.Files {
						files = append(files, fl.Name)
					}
					t.Add(output.S(s.ID, th.Accent), output.C(firstNonEmpty(s.Title, s.Description)), output.S(strings.Join(files, ", "), th.Muted),
						output.C(string(s.Visibility)), output.S(output.RelTime(s.UpdatedAt), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	limitFlag(list, &limit, 30)
	var rawFile string
	view := &cobra.Command{
		Use:   "view <id>",
		Short: "Show a snippet with its files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			sp, err := capability[forge.SnippetProvider](p, forge.CapSnippets)
			if err != nil {
				return err
			}
			s, err := spin(cmd.Context(), a, "Fetching snippet...", func(ctx context.Context) (*domain.Snippet, error) { return sp.GetSnippet(ctx, args[0]) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if rawFile != "" {
				for _, fl := range s.Files {
					if fl.Name == rawFile {
						_, err := fmt.Fprint(a.IO.Out, fl.Content)
						return err
					}
				}
				return errs.New(errs.ErrNotFound, "snippet %s has no file %q", s.ID, rawFile)
			}
			return a.Out.Result(s, func() []string { return []string{s.ID} }, func() error {
				th := a.Out.Theme()
				fmt.Fprintf(a.IO.Out, "%s %s\n", th.Title.Render(firstNonEmpty(s.Title, s.Description, s.ID)), th.Muted.Render(string(s.Visibility)))
				if s.WebURL != "" {
					fmt.Fprintln(a.IO.Out, th.Muted.Render(s.WebURL))
				}
				for _, fl := range s.Files {
					fmt.Fprintf(a.IO.Out, "\n%s\n%s\n", th.Heading.Render("── "+fl.Name), strings.TrimRight(fl.Content, "\n"))
				}
				return nil
			})
		},
	}
	view.Flags().StringVar(&rawFile, "raw", "", "print only this file's raw content")
	var req forge.CreateSnippetRequest
	var visibility, stdinName string
	create := &cobra.Command{
		Use:   "create [file...]",
		Short: "Create a snippet from files (or stdin with --filename)",
		Example: `  trove snippet create main.go util.go --title "Helpers"
  echo 'hello' | trove snippet create --filename hello.txt --visibility public`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			for _, path := range args {
				b, err := os.ReadFile(path)
				if err != nil {
					return errs.Wrap(errs.ErrInvalidArgument, err, "read %s", path)
				}
				req.Files = append(req.Files, domain.SnippetFile{Name: filepath.Base(path), Content: string(b)})
			}
			if len(args) == 0 {
				if stdinName == "" {
					return errs.New(errs.ErrInvalidArgument, "pass files or --filename to read content from stdin")
				}
				content, err := a.IO.ReadAllIn(10 << 20)
				if err != nil {
					return err
				}
				req.Files = append(req.Files, domain.SnippetFile{Name: stdinName, Content: content + "\n"})
			}
			if req.Visibility, err = domain.ParseVisibility(visibility); err != nil {
				return errs.Wrap(errs.ErrInvalidArgument, err, "%v", err)
			}
			if req.Visibility == "" {
				req.Visibility = domain.VisibilityPrivate
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			sc, err := capability[forge.SnippetCreator](p, forge.CapSnippetCreate)
			if err != nil {
				return err
			}
			s, err := spin(cmd.Context(), a, "Creating snippet...", func(ctx context.Context) (*domain.Snippet, error) { return sc.CreateSnippet(ctx, req) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			a.Out.Success("Created %s %s", strings.ToLower(firstNonEmpty(p.Metadata().Terms.Snippet, "snippet")), s.ID)
			return a.Out.Result(s, func() []string { return []string{s.ID} }, func() error {
				if s.WebURL != "" {
					a.Out.Println(s.WebURL)
				}
				return nil
			})
		},
	}
	create.Flags().StringVarP(&req.Title, "title", "t", "", "title")
	create.Flags().StringVarP(&req.Description, "description", "d", "", "description")
	create.Flags().StringVar(&visibility, "visibility", "private", "public, private or internal")
	create.Flags().StringVarP(&req.Namespace, "namespace", "n", "", "workspace (Bitbucket)")
	create.Flags().StringVar(&stdinName, "filename", "", "file name for content read from stdin")
	cmd.AddCommand(list, view, create)
	return cmd
}

// --- keys ---------------------------------------------------------------------

func newKeyCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{Use: "key", Aliases: []string{"keys"}, Short: "Manage SSH and GPG keys on your account"}
	ssh := &cobra.Command{Use: "ssh", Short: "Manage SSH keys"}
	gpg := &cobra.Command{Use: "gpg", Short: "List GPG keys"}

	sshList := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List SSH keys", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			kp, err := capability[forge.SSHKeyProvider](p, forge.CapSSHKeys)
			if err != nil {
				return err
			}
			keys, err := spin(cmd.Context(), a, "Fetching SSH keys...", kp.ListSSHKeys)
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if keys == nil {
				keys = []domain.SSHKey{}
			}
			return a.Out.Result(keys, func() []string {
				out := make([]string, len(keys))
				for i, k := range keys {
					out[i] = k.ID
				}
				return out
			}, func() error {
				providerHeader(a, p, "")
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "ID"}, {Header: "Title", Flex: true}, {Header: "Key", Flex: true}, {Header: "Added"}}}
				for _, k := range keys {
					t.Add(output.S(k.ID, th.Muted), output.C(k.Title), output.S(firstNonEmpty(k.Fingerprint, abbreviateKey(k.Key)), th.Muted), output.S(output.RelTime(k.CreatedAt), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	var title string
	sshAdd := &cobra.Command{
		Use: "add [public-key-file]", Short: "Add an SSH public key (default ~/.ssh/id_ed25519.pub)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			path := ""
			if len(args) == 1 {
				path = expandHome(args[0])
			} else {
				home, _ := os.UserHomeDir()
				path = filepath.Join(home, ".ssh", "id_ed25519.pub")
			}
			if !strings.HasSuffix(path, ".pub") {
				return errs.New(errs.ErrInvalidArgument, "%s does not look like a public key (.pub); refusing to upload a private key", path)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return errs.Wrap(errs.ErrInvalidArgument, err, "read %s", path)
			}
			key := strings.TrimSpace(string(b))
			if strings.Contains(key, "PRIVATE KEY") {
				return errs.New(errs.ErrInvalidArgument, "%s contains a private key; refusing to upload it", path)
			}
			if title == "" {
				if host, err := os.Hostname(); err == nil {
					title = host
				} else {
					title = filepath.Base(path)
				}
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			kp, err := capability[forge.SSHKeyProvider](p, forge.CapSSHKeys)
			if err != nil {
				return err
			}
			k, err := spin(cmd.Context(), a, "Adding SSH key...", func(ctx context.Context) (*domain.SSHKey, error) { return kp.AddSSHKey(ctx, title, key) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			a.Out.Success("Added SSH key %q (%s)", k.Title, k.ID)
			return a.Out.Result(k, func() []string { return []string{k.ID} }, func() error { return nil })
		},
	}
	sshAdd.Flags().StringVarP(&title, "title", "t", "", "key title (default: hostname)")
	sshRemove := &cobra.Command{
		Use: "remove <id>", Aliases: []string{"rm", "delete"}, Short: "Remove an SSH key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			kp, err := capability[forge.SSHKeyProvider](p, forge.CapSSHKeys)
			if err != nil {
				return err
			}
			if err := confirm(a, fmt.Sprintf("Remove SSH key %s from %s? Machines using it will lose access.", args[0], a.Label(p.Metadata().Name))); err != nil {
				return err
			}
			if err := kp.RemoveSSHKey(cmd.Context(), args[0]); err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			a.Out.Success("Removed SSH key %s", args[0])
			return a.Out.Result(map[string]string{"removed": args[0]}, func() []string { return []string{args[0]} }, func() error { return nil })
		},
	}
	gpgList := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List GPG keys", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			kp, err := capability[forge.GPGKeyProvider](p, forge.CapGPGKeys)
			if err != nil {
				return err
			}
			keys, err := spin(cmd.Context(), a, "Fetching GPG keys...", kp.ListGPGKeys)
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			if keys == nil {
				keys = []domain.GPGKey{}
			}
			return a.Out.Result(keys, func() []string {
				out := make([]string, len(keys))
				for i, k := range keys {
					out[i] = firstNonEmpty(k.KeyID, k.ID)
				}
				return out
			}, func() error {
				providerHeader(a, p, "")
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "ID"}, {Header: "Key ID"}, {Header: "Emails", Flex: true}, {Header: "Expires"}}}
				for _, k := range keys {
					exp := ""
					if !k.ExpiresAt.IsZero() {
						exp = k.ExpiresAt.Format("2006-01-02")
					}
					t.Add(output.S(k.ID, th.Muted), output.C(k.KeyID), output.C(strings.Join(k.Emails, ", ")), output.S(exp, th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	ssh.AddCommand(sshList, sshAdd, sshRemove)
	gpg.AddCommand(gpgList)
	cmd.AddCommand(ssh, gpg)
	return cmd
}

func abbreviateKey(k string) string {
	f := strings.Fields(k)
	if len(f) >= 2 && len(f[1]) > 16 {
		return f[0] + " …" + f[1][len(f[1])-12:]
	}
	return k
}

// --- account settings ---------------------------------------------------------

func newSettingsCmd(f *Factory) *cobra.Command {
	list := func(cmd *cobra.Command, args []string) error {
		a, err := f.App()
		if err != nil {
			return err
		}
		p, err := a.Current("")
		if err != nil {
			return err
		}
		sp, err := capability[forge.SettingsProvider](p, forge.CapSettings)
		if err != nil {
			return err
		}
		ss, err := spin(cmd.Context(), a, "Fetching settings...", sp.ListSettings)
		if err != nil {
			return errs.WithProvider(err, p.Metadata().Name)
		}
		return a.Out.Result(ss, func() []string {
			out := make([]string, len(ss))
			for i, s := range ss {
				out[i] = s.Key + "=" + s.Value
			}
			return out
		}, func() error {
			providerHeader(a, p, "")
			th := a.Out.Theme()
			t := &output.Table{Columns: []output.Column{{Header: "Key"}, {Header: "Value", Flex: true}, {Header: "Writable"}, {Header: "Description", Flex: true, MinTerminal: 110}}}
			for _, s := range ss {
				t.Add(output.C(s.Key), output.C(s.Value), output.S(yesNo(s.Writable), th.Muted), output.S(s.Description, th.Muted))
			}
			a.Out.Table(t)
			return nil
		})
	}
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View and change account settings the provider API can manage safely",
		Long:  "Provider account settings (GitHub profile fields, GitLab preferences and status). Only settings the API can manage reliably are exposed.",
		Args:  cobra.NoArgs,
		RunE:  list,
	}
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List settings", Args: cobra.NoArgs, RunE: list})
	cmd.AddCommand(&cobra.Command{
		Use: "get <key>", Short: "Get a setting", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			sp, err := capability[forge.SettingsProvider](p, forge.CapSettings)
			if err != nil {
				return err
			}
			s, err := sp.GetSetting(cmd.Context(), args[0])
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			return a.Out.Result(s, func() []string { return []string{s.Value} }, func() error { a.Out.Println(s.Value); return nil })
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "set <key> <value>", Short: "Change a setting", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			sp, err := capability[forge.SettingsProvider](p, forge.CapSettings)
			if err != nil {
				return err
			}
			s, err := spin(cmd.Context(), a, "Updating setting...", func(ctx context.Context) (*domain.Setting, error) { return sp.SetSetting(ctx, args[0], args[1]) })
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			a.Out.Success("Set %s", s.Key)
			return a.Out.Result(s, func() []string { return []string{s.Value} }, func() error { return nil })
		},
	})
	return cmd
}
