// Package gitlab implements the Trove forge driver for GitLab.com (deployment
// "cloud") and GitLab Self-Managed / Dedicated instances (deployment
// "self-managed") using the REST API v4.
//
// # Hosts and URLs
//
// The API lives at https://<host>/api/v4 and the web UI at https://<host>;
// both can be overridden per account (api_url, web_url), e.g. for instances
// served under a relative URL root. Clone URLs come from the project's
// http_url_to_repo / ssh_url_to_repo unless clone_base_url or ssh_host
// override them.
//
// # Groups, subgroups and projects
//
// GitLab repositories are "projects" living in a namespace: a user's personal
// namespace or a group, and groups nest ("group/subgroup/subsub"). A project
// is addressed by its numeric ID or its URL-encoded full path
// ("group%2Fsubgroup%2Fproject"); Trove uses the ID when a reference carries
// one. Repository.Namespace is the full namespace path and Repository.Name is
// the project path (URL slug), not its display name. Namespaces map to
// domain.NamespaceUser, NamespaceGroup (top level) and NamespaceSubgroup
// (any group with a parent).
//
// # IIDs versus IDs
//
// Merge requests, issues and pipelines have a global ID and a
// project-scoped IID (the "!12" / "#34" users see). Trove's Number is always
// the IID and every per-project endpoint takes the IID. Pipeline operations
// use the pipeline ID, since that is what GitLab's pipeline endpoints accept.
//
// # Merge requests
//
// Pull requests are merge requests. Drafts are created by prefixing the
// title with "Draft: ". Merging goes through PUT
// /projects/:id/merge_requests/:iid/merge and supports two methods: "merge"
// and "squash". Whether a merge creates a merge commit, a semi-linear history
// or a fast-forward is a project setting, and rebasing is a separate
// operation (PUT .../rebase), so the "rebase" merge method is rejected with
// ErrInvalidArgument. Merge requests from forks are created on the fork
// (source) project with target_project_id pointing at the upstream project.
// Every merge request head is fetchable as refs/merge-requests/<iid>/head on
// the target project.
//
// # CI/CD pipelines
//
// Pipelines are GitLab CI pipelines of the project's .gitlab-ci.yml; the
// Workflow field of a run request has no GitLab meaning and is ignored.
// Job logs are the plain-text job traces; when all logs of a pipeline are
// requested, each job is prefixed by "==> <stage>/<job>".
//
// # Releases
//
// Releases are identified by tag. GitLab has no draft releases and no
// prerelease flag (only "upcoming" releases with a future release date), so
// such requests are rejected rather than emulated.
//
// # To-Do items as notifications
//
// GitLab has no notification inbox API; the equivalent is the To-Do list.
// Unread notifications are pending To-Dos, marking one read marks it done,
// and GetNotification scans the To-Do list because GitLab has no endpoint to
// fetch a single To-Do.
//
// # Search
//
// Project search uses GET /projects?search= (full entities, including
// visibility); issue and code search use the Search API at global, group or
// project scope. Code (blobs) search at global or group scope requires
// Advanced Search (Elasticsearch/OpenSearch) or exact code search (Zoekt);
// instances without it answer HTTP 400, which Trove reports as
// ErrUnsupportedCapability with a hint to scope the search to a project.
//
// # Authentication
//
// Personal, project and group access tokens are sent as PRIVATE-TOKEN
// (project and group tokens authenticate as a bot user). OAuth access tokens
// are sent as "Authorization: Bearer"; they expire after two hours and are
// refreshed transparently (and persisted) when expired or rejected. OAuth
// login uses the device authorization grant (GitLab 17.2+, generally
// available in 17.9) with an OAuth application registered on the instance.
// Credentials are revoked with POST /oauth/revoke (OAuth) or DELETE
// /personal_access_tokens/self (access tokens). Git over HTTPS uses the
// username "oauth2" with either kind of token.
//
// # Pagination and rate limits
//
// GitLab uses offset pagination; the driver follows Link rel="next" and
// falls back to X-Next-Page. X-Total may be missing for large collections, in
// which case summary counts are reported as unavailable. Throttling is
// signalled with HTTP 429 (Retry-After / RateLimit-Reset); 403 is always a
// permission error on GitLab.
//
// # Self-managed differences
//
// Feature availability depends on the instance version and tier: token
// self-introspection and self-revocation need GitLab 15.5+, the device grant
// 17.2+, detailed_merge_status 15.6+, and instance/group code search needs
// Advanced Search. Older instances degrade gracefully (for example, auth
// status omits token scopes when /personal_access_tokens/self is missing).
// Administrators may also disable features such as snippets or CI per
// instance or project; the resulting 403/404 responses surface as
// ErrPermissionDenied / ErrNotFound.
package gitlab
