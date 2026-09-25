#!/usr/bin/env bash
# Generate shell completion scripts for trove.
#
# Usage: scripts/completions.sh
#
# Environment:
#   TROVE_BIN        trove executable to use (default: "go run ./cmd/trove")
#   COMPLETIONS_DIR  output directory (default: completions)
#
# Used by `make completions` and by the GoReleaser `before` hook, so release
# archives and the Homebrew cask ship ready-made completions.
set -euo pipefail

cd "$(dirname "$0")/.."

out="${COMPLETIONS_DIR:-completions}"
mkdir -p "$out"

run_trove() {
	if [[ -n "${TROVE_BIN:-}" ]]; then
		"$TROVE_BIN" "$@"
	else
		go run ./cmd/trove "$@"
	fi
}

# Completion generation never reads the user's configuration, but point
# TROVE_CONFIG somewhere harmless anyway so the script is hermetic.
export TROVE_CONFIG="${TMPDIR:-/tmp}/trove-completions-config.yaml"

run_trove completion bash >"$out/trove.bash"
run_trove completion zsh >"$out/trove.zsh"
run_trove completion fish >"$out/trove.fish"
run_trove completion powershell >"$out/trove.ps1"

echo "wrote completions to $out/: trove.bash trove.zsh trove.fish trove.ps1"
