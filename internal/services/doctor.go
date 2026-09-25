package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

// CheckStatus is the outcome of one diagnostic.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
	CheckSkip CheckStatus = "skip"
)

// Check is one diagnostic line. Detail never contains secret values.
type Check struct {
	Group  string      `json:"group"`
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
	Hint   string      `json:"hint,omitempty"`
}

// DoctorReport aggregates checks.
type DoctorReport struct {
	Checks   []Check `json:"checks"`
	Warnings int     `json:"warnings"`
	Failures int     `json:"failures"`
}

// Doctor runs diagnostics. Provider checks run concurrently (bounded) with a
// per-check timeout so one unreachable host cannot stall the report.
func Doctor(ctx context.Context, a *app.App, only string) DoctorReport {
	var checks []Check
	checks = append(checks, configChecks(a)...)
	checks = append(checks, gitCheck(ctx, a))
	checks = append(checks, sshChecks(ctx, a)...)
	checks = append(checks, secretChecks(ctx, a)...)
	checks = append(checks, providerChecks(ctx, a, only)...)

	r := DoctorReport{Checks: checks}
	for _, c := range checks {
		switch c.Status {
		case CheckWarn:
			r.Warnings++
		case CheckFail:
			r.Failures++
		}
	}
	return r
}

func configChecks(a *app.App) []Check {
	var out []Check
	path := a.Config.Path()
	st, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		out = append(out, Check{Group: "config", Name: "Config file", Status: CheckWarn,
			Detail: path + " does not exist yet", Hint: "trove provider add"})
	case err != nil:
		out = append(out, Check{Group: "config", Name: "Config file", Status: CheckFail, Detail: err.Error()})
	default:
		problems := a.Config.Validate(a.ValidationRules())
		if len(problems) == 0 {
			out = append(out, Check{Group: "config", Name: "Config file", Status: CheckOK, Detail: path})
		} else {
			var ps []string
			for _, p := range problems {
				ps = append(ps, p.String())
			}
			out = append(out, Check{Group: "config", Name: "Config file", Status: CheckFail,
				Detail: strings.Join(ps, "; "), Hint: "trove config validate"})
		}
		if runtime.GOOS != "windows" {
			if mode := st.Mode().Perm(); mode&0o077 != 0 {
				out = append(out, Check{Group: "config", Name: "Config permissions", Status: CheckWarn,
					Detail: fmt.Sprintf("%s is %04o (readable by others)", path, mode), Hint: "chmod 600 " + shellArg(path)})
			} else {
				out = append(out, Check{Group: "config", Name: "Config permissions", Status: CheckOK, Detail: fmt.Sprintf("%04o", mode)})
			}
		}
	}
	return out
}

func gitCheck(ctx context.Context, a *app.App) Check {
	if err := a.Git.Available(); err != nil {
		return Check{Group: "git", Name: "Git", Status: CheckFail, Detail: "git not found on PATH", Hint: "install Git from https://git-scm.com"}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := a.Git.Version(cctx)
	if err != nil {
		return Check{Group: "git", Name: "Git", Status: CheckFail, Detail: err.Error()}
	}
	return Check{Group: "git", Name: "Git", Status: CheckOK, Detail: "version " + v}
}

func sshChecks(ctx context.Context, a *app.App) []Check {
	var out []Check
	if _, err := exec.LookPath("ssh"); err != nil {
		return []Check{{Group: "ssh", Name: "SSH client", Status: CheckWarn, Detail: "ssh not found; SSH cloning unavailable"}}
	}
	home, _ := os.UserHomeDir()
	keys, _ := filepath.Glob(filepath.Join(home, ".ssh", "id_*.pub"))
	agent := os.Getenv("SSH_AUTH_SOCK") != ""
	switch {
	case len(keys) == 0 && !agent:
		out = append(out, Check{Group: "ssh", Name: "SSH keys", Status: CheckWarn,
			Detail: "no ~/.ssh/id_*.pub keys and no ssh-agent", Hint: "ssh-keygen -t ed25519  (or use --protocol https)"})
	default:
		d := fmt.Sprintf("%d public key(s)", len(keys))
		if agent {
			d += ", ssh-agent running"
		}
		out = append(out, Check{Group: "ssh", Name: "SSH keys", Status: CheckOK, Detail: d})
	}
	// known_hosts entries for configured hosts (no network access).
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		seen := map[string]bool{}
		for _, alias := range a.Config.ProviderNames() {
			pc := a.Config.Providers[alias]
			host := pc.SSHHost
			if host == "" {
				host = pc.Host
			}
			if host == "" || seen[host] {
				continue
			}
			seen[host] = true
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := exec.CommandContext(cctx, "ssh-keygen", "-F", host).Run()
			cancel()
			if err != nil {
				out = append(out, Check{Group: "ssh", Name: "known_hosts " + host, Status: CheckWarn,
					Detail: "host key not in ~/.ssh/known_hosts; the first SSH clone will prompt", Hint: "ssh -T git@" + host})
			} else {
				out = append(out, Check{Group: "ssh", Name: "known_hosts " + host, Status: CheckOK})
			}
		}
	}
	return out
}

func secretChecks(ctx context.Context, a *app.App) []Check {
	used := map[string]bool{a.Config.Secrets.Provider: true}
	for _, alias := range a.Config.ProviderNames() {
		if ref, err := a.SecretRef(alias, a.Config.Providers[alias]); err == nil {
			used[ref.Provider] = true
		}
	}
	names := make([]string, 0, len(used))
	for n := range used {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []Check
	for _, n := range names {
		p, err := a.Secrets.Provider(n)
		if err != nil {
			out = append(out, Check{Group: "secrets", Name: n, Status: CheckFail, Detail: err.Error()})
			continue
		}
		label := n
		if d, ok := p.(secrets.Describer); ok {
			label = d.Description()
		}
		c, ok := p.(secrets.Checker)
		if !ok {
			out = append(out, Check{Group: "secrets", Name: label, Status: CheckOK})
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = c.Check(cctx)
		cancel()
		switch {
		case err == nil:
			out = append(out, Check{Group: "secrets", Name: label, Status: CheckOK})
		case errors.Is(err, errs.ErrSecretProviderLocked):
			out = append(out, Check{Group: "secrets", Name: label, Status: CheckWarn, Detail: err.Error(), Hint: errs.HintOf(err)})
		default:
			st := CheckFail
			if n != a.Config.Secrets.Provider && !referenced(a, n) {
				st = CheckWarn
			}
			out = append(out, Check{Group: "secrets", Name: label, Status: st, Detail: err.Error(), Hint: errs.HintOf(err)})
		}
	}
	return out
}

func referenced(a *app.App, scheme string) bool {
	for _, alias := range a.Config.ProviderNames() {
		if ref, err := a.SecretRef(alias, a.Config.Providers[alias]); err == nil && ref.Provider == scheme {
			return true
		}
	}
	return false
}

func providerChecks(ctx context.Context, a *app.App, only string) []Check {
	names := a.Config.ProviderNames()
	if only != "" {
		names = []string{only}
	}
	results := make([][]Check, len(names))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, alias := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = providerCheck(ctx, a, alias)
		}()
	}
	wg.Wait()
	var out []Check
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

func providerCheck(ctx context.Context, a *app.App, alias string) []Check {
	label := a.Label(alias)
	group := "provider:" + alias
	p, err := a.OpenOther(alias)
	if err != nil {
		return []Check{{Group: group, Name: label, Status: CheckFail, Detail: err.Error(), Hint: errs.HintOf(err)}}
	}
	m := p.Metadata()
	var out []Check
	if problems := forge.ValidateCapabilities(p); len(problems) > 0 {
		out = append(out, Check{Group: group, Name: label + " capabilities", Status: CheckFail, Detail: fmt.Sprint(problems)})
	} else {
		out = append(out, Check{Group: group, Name: label + " capabilities", Status: CheckOK,
			Detail: fmt.Sprintf("%s, %d capabilities", m.DisplayName, len(p.Capabilities().List()))})
	}
	authn, ok := p.(forge.Authenticator)
	if !ok {
		out = append(out, Check{Group: group, Name: label + " authentication", Status: CheckSkip, Detail: "provider has no authentication check"})
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := authn.AuthStatus(cctx)
	switch {
	case err == nil && st != nil && st.Authenticated:
		d := "authenticated"
		if st.User != "" {
			d += " as " + st.User
		}
		c := Check{Group: group, Name: label + " authentication", Status: CheckOK, Detail: d}
		if !st.ExpiresAt.IsZero() {
			left := time.Until(st.ExpiresAt)
			switch {
			case left <= 0:
				c.Status, c.Detail, c.Hint = CheckWarn, "credential expired", "trove auth login "+alias
			case left < 7*24*time.Hour:
				c.Status, c.Detail = CheckWarn, d+fmt.Sprintf("; expires in %s", left.Round(time.Hour))
				c.Hint = "trove auth refresh " + alias
			}
		}
		out = append(out, c)
		out = append(out, Check{Group: group, Name: label + " API connectivity", Status: CheckOK, Detail: m.APIURL})
	case errors.Is(err, errs.ErrNotAuthenticated):
		out = append(out, Check{Group: group, Name: label + " authentication", Status: CheckWarn, Detail: "not logged in", Hint: "trove auth login " + alias})
	case errors.Is(err, errs.ErrAuthenticationFailed):
		out = append(out, Check{Group: group, Name: label + " authentication", Status: CheckFail, Detail: "credential rejected (invalid or expired)", Hint: "trove auth login " + alias})
		out = append(out, Check{Group: group, Name: label + " API connectivity", Status: CheckOK, Detail: m.APIURL})
	case errors.Is(err, errs.ErrSecretProviderLocked), errors.Is(err, errs.ErrSecretUnavailable):
		out = append(out, Check{Group: group, Name: label + " authentication", Status: CheckWarn, Detail: err.Error(), Hint: errs.HintOf(err)})
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || isNetErr(err)):
		out = append(out, Check{Group: group, Name: label + " API connectivity", Status: CheckFail, Detail: fmt.Sprintf("cannot reach %s: %v", m.APIURL, err)})
	default:
		out = append(out, Check{Group: group, Name: label + " authentication", Status: CheckFail, Detail: fmt.Sprint(err)})
	}
	return out
}

func isNetErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "i/o timeout") || strings.Contains(s, "tls:") || strings.Contains(s, "network is unreachable")
}

func shellArg(s string) string {
	if strings.ContainsAny(s, " '\"") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
