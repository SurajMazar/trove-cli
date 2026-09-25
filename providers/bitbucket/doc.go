// Package bitbucket groups Trove's Bitbucket forge drivers.
//
// Bitbucket ships as two unrelated products that happen to share a name, and
// Trove treats them as separate driver types:
//
//   - Bitbucket Cloud (bitbucket.org) is implemented in the cloud sub-package
//     as driver type "bitbucket". It speaks the REST API 2.0 at
//     https://api.bitbucket.org/2.0, where repositories live in a
//     Workspace -> (Project) -> Repository hierarchy, collections are paged
//     with a JSON envelope whose "next" member is an absolute URL, and
//     authentication uses Atlassian API tokens (HTTP Basic with the Atlassian
//     account email), repository/project/workspace access tokens (Bearer), or
//     OAuth 2.0 consumers. Bitbucket Cloud app passwords are NOT offered:
//     Atlassian stopped issuing them on 9 September 2025 and disabled the
//     remaining ones in 2026, in favor of API tokens.
//
//   - Bitbucket Server / Data Center is self-hosted and uses a completely
//     different REST API rooted at /rest/api/1.0: repositories live in
//     projects (not workspaces), collections are paged with
//     start/limit/isLastPage/nextPageStart instead of "next" URLs, and
//     authentication uses HTTP access tokens or personal access tokens with
//     different permission semantics. It will be implemented in
//     providers/bitbucket/server as a separate driver type "bitbucket-server".
//     Adding it does not require touching the Cloud driver: the two share no
//     API client code, only this parent directory.
package bitbucket
