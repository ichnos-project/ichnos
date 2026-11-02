#!/usr/bin/env bash
set -euo pipefail

source=${1?usage: lab/gha/reduce-record.sh RECORD OUTPUT}
target=${2?usage: lab/gha/reduce-record.sh RECORD OUTPUT}
secret='eyJ[A-Za-z0-9_-]{10,}\.|gh[opsur]_[A-Za-z0-9]|github_pat_|[Bb]earer [A-Za-z0-9]|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}|([?&]|%26|u0026)([Xx]-[A-Za-z]+-)?([Ss]ig|[Ss]ignature)(=|%3[Dd])'

stop() {
  echo "$source $1" >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -xzf "$source" -C "$work"

[ "$(jq '.manifests | length' "$work/index.json")" = 1 ] || stop 'does not have exactly one manifest'
blobs="$work/blobs/sha256"
manifest=$(jq -r '.manifests[0].digest | ltrimstr("sha256:")' "$work/index.json")
config=$(jq -r '.config.digest | ltrimstr("sha256:")' "$blobs/$manifest")
trace=$(jq -r '.trace.digest // "" | ltrimstr("sha256:")' "$blobs/$config")

[ -n "$trace" ] || stop 'has no trace to remove'
jq -e '[.layers[].mediaType] - ["application/json", "application/vnd.buildkit.otlp.json.v0", "application/vnd.buildkit.status.v0", "application/vnd.in-toto+json"] == []' \
  "$blobs/$manifest" >/dev/null || stop 'has an unknown layer type'

jq -r '"cache import attributes: \(.FrontendAttrs["cache-imports"] // "[]" | fromjson | map(.Attrs | keys))"' "$blobs/$config"
jq -e '.FrontendAttrs["cache-imports"] // "[]" | fromjson | all(.[]; (.Attrs | keys) - ["ref", "scope"] == [])' \
  "$blobs/$config" >/dev/null || stop 'has an unexpected cache import attribute'

status=0
matches=$(LC_ALL=C grep -a -c -E "$secret" "$blobs/$trace") || status=$?
[ "$status" -le 1 ] || exit "$status"
echo "secret-shaped lines in the trace: $matches"
rm -f "$blobs/$trace"

status=0
LC_ALL=C grep -a -r -q -E "$secret" "$work" || status=$?
case $status in
  0) stop 'has secret-shaped data outside the trace' ;;
  1) ;;
  *) exit "$status" ;;
esac

tar -czf "$target" -C "$work" index.json oci-layout blobs
