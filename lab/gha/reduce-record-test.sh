#!/usr/bin/env bash
set -euo pipefail

script=lab/gha/reduce-record.sh
fixture=testdata/buildkit-v0.34.0/control/pr.dockerbuild
escaped_ampersand=$(printf '\\%s' u0026)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

unpack() {
  rm -rf "$work/record" "$work/output"
  mkdir "$work/record"
  tar -xzf "$fixture" -C "$work/record"
  chmod -R u+w "$work/record"
  blobs="$work/record/blobs/sha256"
  manifest=$(jq -r '.manifests[0].digest | ltrimstr("sha256:")' "$work/record/index.json")
  config=$(jq -r '.config.digest | ltrimstr("sha256:")' "$blobs/$manifest")
  trace=$(jq -r '.trace.digest | ltrimstr("sha256:")' "$blobs/$config")
  status_stream=$(jq -r '.layers[] | select(.mediaType == "application/vnd.buildkit.status.v0") | .digest | ltrimstr("sha256:")' "$blobs/$manifest")
  provenance=$(jq -r '.layers[] | select(.mediaType == "application/vnd.in-toto+json") | .digest | ltrimstr("sha256:")' "$blobs/$manifest")
}

edit_json() {
  jq "$2" "$1" >"$work/edited"
  mv "$work/edited" "$1"
}

expect_stop() {
  tar -czf "$work/input" -C "$work/record" index.json oci-layout blobs
  if "$script" "$work/input" "$work/output" >/dev/null 2>&1; then
    echo "expected a stop: $1" >&2
    exit 1
  fi
  if [ -e "$work/output" ]; then
    echo "wrote output: $1" >&2
    exit 1
  fi
}

unpack
"$script" "$fixture" "$work/output" >"$work/log"
tar -tzf "$work/output" >"$work/listing"
if grep -q "$trace" "$work/listing"; then
  echo 'the trace is still in the output' >&2
  exit 1
fi
grep -q "$provenance" "$work/listing"
grep -q "$status_stream" "$work/listing"
if ! grep -q 'secret-shaped lines in the trace: [1-9]' "$work/log"; then
  echo 'the signed URLs in the trace were not counted' >&2
  exit 1
fi

unpack
edit_json "$blobs/$provenance" '.buildDefinition.internalParameters.github_event_payload = {repository: {ssh_url: "git@github.com:a/b.git"}, pusher: {email: "someone@example.test"}}'
tar -czf "$work/input" -C "$work/record" index.json oci-layout blobs
"$script" "$work/input" "$work/output" >/dev/null
tar -xzOf "$work/output" >"$work/contents"
if LC_ALL=C grep -a -q github_event_payload "$work/contents"; then
  echo 'the GitHub event is still in the output' >&2
  exit 1
fi

unpack
printf 'https://example.test/a?sv=1%ssig=planted' "$escaped_ampersand" >>"$blobs/$status_stream"
expect_stop 'escaped signed URL in the status stream'

unpack
printf 'https://example.test/a?sv=1&sig=planted' >>"$blobs/$status_stream"
expect_stop 'signed URL in the status stream'

unpack
printf 'https://example.test/a?X-Amz-Signature=planted' >>"$blobs/$status_stream"
expect_stop 'AWS signed URL in the status stream'

unpack
edit_json "$blobs/$provenance" '.planted = "someone@example.test"'
expect_stop 'email address in the provenance'

unpack
printf 'ghr_planted' >>"$blobs/$status_stream"
expect_stop 'GitHub token in the status stream'

unpack
edit_json "$blobs/$config" '.FrontendAttrs.planted = "eyJhbGciOiJIUzI1NiJ9.eyJ4IjoxfQ.sig"'
expect_stop 'JWT in the history record'

unpack
edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":{\"scope\":\"lab\",\"token\":\"opaque\"}}]"'
expect_stop 'token attribute on a cache import'

unpack
edit_json "$blobs/$config" 'del(.trace)'
expect_stop 'record with no trace'

unpack
edit_json "$blobs/$manifest" '(.layers[] | select(.mediaType == "application/vnd.buildkit.otlp.json.v0") | .mediaType) = "application/vnd.buildkit.otlp.json.v1"'
expect_stop 'unknown layer type'

unpack
edit_json "$work/record/index.json" '.manifests += .manifests'
expect_stop 'more than one manifest'

printf 'not a record' >"$work/input"
if "$script" "$work/input" "$work/output" >/dev/null 2>&1; then
  echo 'accepted a file that is not a record' >&2
  exit 1
fi

echo 'reduce-record: all cases pass'
