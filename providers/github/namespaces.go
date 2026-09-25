package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

const (
	labelOrganization = "GitHub Organization"
	labelUser         = "GitHub User"
)

type apiOrg struct {
	ID          int64   `json:"id"`
	Login       string  `json:"login"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	HTMLURL     string  `json:"html_url"`
	AvatarURL   string  `json:"avatar_url"`
}

func (p *Provider) orgNamespace(o apiOrg) domain.Namespace {
	web := o.HTMLURL
	if web == "" {
		// /user/orgs items carry no html_url.
		web = p.web + "/" + o.Login
	}
	return domain.Namespace{
		ID: strconv.FormatInt(o.ID, 10), Name: orDefault(str(o.Name), o.Login), FullPath: o.Login,
		Type: domain.NamespaceOrganization, Description: str(o.Description), WebURL: web,
		AvatarURL: o.AvatarURL, Label: labelOrganization,
	}
}

func userNamespace(u apiUser) domain.Namespace {
	ns := domain.Namespace{
		ID: strconv.FormatInt(u.ID, 10), Name: orDefault(str(u.Name), u.Login), FullPath: u.Login,
		Type: domain.NamespaceUser, Description: str(u.Bio), WebURL: u.HTMLURL, AvatarURL: u.AvatarURL,
		Label: labelUser,
	}
	if u.Type == "Organization" {
		ns.Type, ns.Label = domain.NamespaceOrganization, labelOrganization
	}
	return ns
}

// ListNamespaces implements forge.NamespaceProvider: the user's personal
// account followed by the organizations they belong to. For GitHub App
// accounts it is the account the App is installed on.
func (p *Provider) ListNamespaces(ctx context.Context, opts forge.ListOptions) ([]domain.Namespace, error) {
	const op = "list namespaces"
	c, err := p.credential(ctx)
	if err != nil {
		return nil, err
	}
	if p.isAppCredential(c) {
		ns, err := p.installationNamespace(ctx, c)
		if err != nil {
			return nil, err
		}
		return []domain.Namespace{*ns}, nil
	}
	var u apiUser
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/user", out: &u}); err != nil {
		return nil, err
	}
	out := []domain.Namespace{userNamespace(u)}
	if opts.Limit == 1 {
		return out, nil
	}
	limit := opts.Limit
	if limit > 0 {
		limit--
	}
	orgs, err := paginate(ctx, p, listSpec[apiOrg]{
		request: request{op: op, path: "/user/orgs"}, perPage: maxPerPage, limit: limit,
	}, func(o apiOrg) (domain.Namespace, bool) { return p.orgNamespace(o), true })
	if err != nil {
		return nil, err
	}
	return append(out, orgs...), nil
}

func (p *Provider) installationNamespace(ctx context.Context, c auth.Credential) (*domain.Namespace, error) {
	const op = "get installation"
	jwt, err := p.appJWT(op, c)
	if err != nil {
		return nil, err
	}
	var inst struct {
		Account apiUser `json:"account"`
	}
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/app/installations/" + esc(p.acct.InstallationID),
		out: &inst, auth: authExplicit, token: jwt}); err != nil {
		return nil, appAuthError(err)
	}
	ns := userNamespace(inst.Account)
	return &ns, nil
}

// GetNamespace implements forge.NamespaceProvider, trying organizations
// first and then users.
func (p *Provider) GetNamespace(ctx context.Context, path string) (*domain.Namespace, error) {
	const op = "get namespace"
	name := strings.Trim(strings.TrimSpace(path), "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid GitHub account name %q", path)
	}
	var o apiOrg
	_, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/orgs/" + esc(name), out: &o})
	if err == nil {
		ns := p.orgNamespace(o)
		return &ns, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return nil, err
	}
	var u apiUser
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/users/" + esc(name), out: &u,
		notFoundMsg: fmt.Sprintf("no organization or user named %q", name)}); err != nil {
		return nil, err
	}
	ns := userNamespace(u)
	return &ns, nil
}
