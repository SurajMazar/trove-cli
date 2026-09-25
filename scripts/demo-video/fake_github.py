"""A tiny fake GitHub REST API with fictional demo data for the Trove video.

Serves only what the recorded scenes touch. All names are fictional.
Usage: python fake_github.py PORT SRC_DIR
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PORT = int(sys.argv[1])
SRC = sys.argv[2]  # local directory holding clonable repositories

from datetime import datetime, timedelta, timezone


def ago(**kw):
    return (datetime.now(timezone.utc) - timedelta(**kw)).strftime("%Y-%m-%dT%H:%M:%SZ")


NOW = ago(seconds=5)


def repo(owner, name, desc, private=True, lang="TypeScript", updated=None, stars=0):
    return {
        "id": abs(hash(owner + name)) % 10**8, "name": name, "full_name": f"{owner}/{name}",
        "owner": {"login": owner}, "private": private, "visibility": "private" if private else "public",
        "description": desc, "default_branch": "main", "archived": False, "fork": False,
        "language": lang, "stargazers_count": stars, "forks_count": 0, "open_issues_count": 3,
        "html_url": f"https://github.com/{owner}/{name}",
        "clone_url": f"{SRC}/{owner}/{name}", "ssh_url": f"git@github.com:{owner}/{name}.git",
        "created_at": "2025-03-02T10:00:00Z", "updated_at": updated or ago(days=1), "pushed_at": updated or ago(days=1),
    }


REPOS = [
    repo("acme", "web-app", "Customer-facing web application", updated=ago(minutes=12)),
    repo("acme", "api-gateway", "Edge gateway and rate limiting", lang="Go", updated=ago(hours=2)),
    repo("acme", "payments-api", "Payments service (Stripe, invoices)", lang="Go", updated=ago(hours=9)),
    repo("acme", "infra", "Terraform and Kubernetes manifests", lang="HCL", updated=ago(days=2)),
    repo("acme", "mobile-app", "iOS and Android app", lang="Swift", updated=ago(days=3)),
    repo("acme", "docs", "Public developer documentation", private=False, lang="MDX", updated=ago(days=5), stars=128),
    repo("alex-dev", "dotfiles", "zsh, git and editor config", private=False, lang="Shell", updated=ago(days=7), stars=12),
    repo("alex-dev", "blog", "Personal blog", private=False, lang="Astro", updated=ago(days=15), stars=4),
]


def issue(n, title, user, labels, updated, body=""):
    return {"id": 9000 + n, "number": n, "title": title, "state": "open", "user": {"login": user},
            "labels": [{"name": l} for l in labels], "assignees": [], "comments": n % 5,
            "html_url": f"https://github.com/acme/web-app/issues/{n}", "created_at": "2026-09-01T10:00:00Z",
            "updated_at": updated, "body": body}


ISSUES = [
    issue(142, "Checkout button unresponsive on Safari 18", "jordan-lee", ["bug", "checkout"], ago(minutes=40)),
    issue(139, "Add dark mode", "maya-chen", ["feature", "ui"], ago(hours=14), body=(
        "## Why\n\nMost of our users work late, and several asked for a dark theme. "
        "It should follow the operating system setting by default.\n\n"
        "## Scope\n\n- Respect `prefers-color-scheme`\n- Add a toggle in Settings → Appearance\n"
        "- Persist the choice per account\n- Check contrast of charts and code blocks\n\n"
        "## Acceptance\n\n1. Theme switches without a reload\n2. No flash of light theme on first paint\n"
        "3. Screenshots updated in the docs\n\n```css\n:root[data-theme=\"dark\"] { --bg: #0f1117; }\n```\n")),
    issue(137, "Password reset email arrives twice", "sam-patel", ["bug", "auth"], ago(days=1)),
    issue(131, "Dark mode for the marketing site", "maya-chen", ["ui"], ago(days=3)),
    issue(128, "Upgrade to React 19", "alex-dev", ["chore"], ago(days=4)),
    issue(120, "Slow dashboard with 500+ projects", "jordan-lee", ["performance"], ago(days=6)),
]

PRS = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, body, headers=None):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urlparse(self.path)
        p, q = u.path, parse_qs(u.query)
        if p == "/user":
            return self.send(200, {"login": "alex-dev", "id": 1001, "name": "Alex Rivera", "public_repos": 3,
                                   "total_private_repos": 5, "html_url": "https://github.com/alex-dev"},
                             {"X-OAuth-Scopes": "repo, read:org, workflow, gist"})
        if p == "/user/repos":
            return self.send(200, REPOS)
        if p == "/user/orgs":
            return self.send(200, [{"login": "acme", "id": 2}])
        if p == "/notifications":
            host = f"http://127.0.0.1:{PORT}"
            return self.send(200, [{}], {"Link": f'<{host}/notifications?per_page=1&page=4>; rel="last"'})
        if p == "/search/issues":
            query = q.get("q", [""])[0]
            return self.send(200, {"total_count": 3 if "is:pr" in query else 7, "items": []})
        parts = p.strip("/").split("/")
        if len(parts) >= 3 and parts[0] == "repos":
            full = parts[1] + "/" + parts[2]
            r = next((x for x in REPOS if x["full_name"] == full), None)
            if r is None:
                return self.send(404, {"message": "Not Found"})
            if len(parts) == 3:
                return self.send(200, r)
            if parts[3] == "issues" and len(parts) == 4:
                return self.send(200, ISSUES if full == "acme/web-app" else [])
            if parts[3] == "issues" and len(parts) == 5:
                it = next((i for i in ISSUES if str(i["number"]) == parts[4]), None)
                return self.send(200, it) if it else self.send(404, {"message": "Not Found"})
            if parts[3] == "pulls" and len(parts) == 4:
                return self.send(200, PRS)
        return self.send(404, {"message": "Not Found"})

    def do_POST(self):
        u = urlparse(self.path)
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        if u.path == "/repos/acme/web-app/pulls":
            pr = {"id": 77001, "number": 146, "title": body.get("title"), "body": body.get("body", ""),
                  "state": "open", "draft": bool(body.get("draft")), "user": {"login": "alex-dev"},
                  "head": {"ref": body.get("head"), "repo": {"full_name": "acme/web-app"}},
                  "base": {"ref": body.get("base")}, "labels": [], "requested_reviewers": [],
                  "html_url": "https://github.com/acme/web-app/pull/146", "created_at": NOW, "updated_at": NOW}
            PRS.append(pr)
            return self.send(201, pr)
        return self.send(404, {"message": "Not Found"})


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
