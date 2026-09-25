#!/bin/sh
# Builds the throwaway demo world: clonable source repos, a work checkout for
# the commit/push scene (pushing to a local bare repo), and a Trove config
# pointing the GitHub account at the local fake API.
set -eu
V="$(cd "$(dirname "$0")" && pwd)"
D="$V/demo"
PORT=18800
rm -rf "$D"
mkdir -p "$D/src" "$D/code" "$D/bare/acme" "$D/cache"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME="Alex Rivera" GIT_AUTHOR_EMAIL="alex@acme.dev" GIT_COMMITTER_NAME="Alex Rivera" GIT_COMMITTER_EMAIL="alex@acme.dev"

for r in acme/web-app acme/api-gateway acme/payments-api acme/infra acme/mobile-app acme/docs alex-dev/dotfiles alex-dev/blog; do
  mkdir -p "$D/src/$r"
  git -C "$D/src/$r" init -q -b main
  echo "# ${r#*/}" > "$D/src/$r/README.md"
  git -C "$D/src/$r" add README.md
  git -C "$D/src/$r" commit -q -m "Initial commit"
done

# Work checkout for the commit/push scene.
git init -q --bare "$D/bare/acme/web-app.git"
W="$D/code/web-app"
git clone -q "$D/src/acme/web-app" "$W"
git -C "$W" push -q "$D/bare/acme/web-app.git" main
git -C "$W" remote set-url origin https://github.com/acme/web-app.git
git -C "$W" remote set-url --push origin "$D/bare/acme/web-app.git"
git -C "$W" checkout -q -b feature/dark-mode
mkdir -p "$W/src/settings"
printf 'export const themes = ["light", "dark", "system"];\n' > "$W/src/theme.ts"
printf 'export function DarkModeToggle() {\n  return <Toggle label="Dark mode" />;\n}\n' > "$W/src/settings/DarkModeToggle.tsx"
git -C "$W" add src/theme.ts && git -C "$W" commit -q -m "Add theme list"
printf 'export const themes = ["light", "dark", "system"] as const;\nexport const defaultTheme = "system";\n' > "$W/src/theme.ts"
rm -rf "$D/code/web-app/.git/hooks"

cat > "$D/config.yaml" <<EOF
version: 1
default_provider: github-personal
providers:
  github-personal:
    type: github
    host: github.com
    name: GitHub Personal
    api_base_url: http://127.0.0.1:$PORT
    auth:
      type: token
      secret_ref: keychain://trove/github/github-personal/token
  gitlab-work:
    type: gitlab
    host: gitlab.acme.dev
    name: GitLab Work
    auth:
      type: token
      secret_ref: bitwarden://trove/gitlab/work/token
  bitbucket-team:
    type: bitbucket
    host: bitbucket.org
    name: Bitbucket Team
    auth:
      type: basic
      username: alex@acme.dev
      secret_ref: keychain://trove/bitbucket/team/token
secrets:
  provider: keychain
clone:
  concurrency: 4
EOF
echo "demo ready: $D"
