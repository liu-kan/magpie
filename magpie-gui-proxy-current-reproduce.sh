#!/usr/bin/env bash
set -euo pipefail
magpie_repo=${1:-$PWD}
cd "$magpie_repo"
printf 'branch=%s commit=%s\n' "$(git branch --show-current)" "$(git rev-parse HEAD)"
verification_gopath=$(go env GOPATH)
verification_modcache=$(go env GOMODCACHE)
verification_buildcache=$(go env GOCACHE)
verification_home=$(mktemp -d)
trap 'chmod -R u+w "$verification_home"; rm -rf "$verification_home"' EXIT
mkdir -p "$verification_home/tmp"
export HOME="$verification_home" USERPROFILE="$verification_home" TMPDIR="$verification_home/tmp"
export XDG_CONFIG_HOME="$verification_home/.config" XDG_CACHE_HOME="$verification_home/.cache"
export GOPATH="$verification_gopath" GOMODCACHE="$verification_modcache" GOCACHE="$verification_buildcache"
go test -tags nogui ./internal/gui \
  -run '^TestSettingsProxyRechecksAFailedUpdate$' -count=1 -v -timeout=30s
