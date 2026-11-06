#!/usr/bin/env bash
set -euo pipefail

source=${1?usage: lab/gha/reduce-record.sh RECORD OUTPUT}
target=${2?usage: lab/gha/reduce-record.sh RECORD OUTPUT}
secret='eyJ[A-Za-z0-9_-]{10,}\.|gh[opsur]_[A-Za-z0-9]|github_pat_|[Bb]earer [A-Za-z0-9]|[Bb]asic [A-Za-z0-9+/=]{8,}|"auth" *: *"|[A-Za-z0-9._%+-]+(@|%40|%2540|\\u0040)[A-Za-z0-9-]+\.[A-Za-z]{2,}|([?&]|%26|%3[Ff]|u0026)([Xx]-[A-Za-z]+-)?([Ss]ig|[Ss]ignature)(=|%3[Dd])'
otlp=application/vnd.buildkit.otlp.json.v0
status_type=application/vnd.buildkit.status.v0
provenance_type=application/vnd.in-toto+json
# max_entries is the collector's limit. max_bytes is 160 times the largest
# lab record, 414,208 bytes decompressed, and more than the 12 MiB of logs
# that the three steps of lab/gha/Dockerfile can keep, at 2 MiB each of
# stdout and stderr. It keeps a malformed record from filling the disk.
max_entries=1024
max_bytes=$((64 << 20))

stop() {
  echo "$source $1" >&2
  exit 1
}

if [ -d "$target" ]; then
  echo "$target is a directory" >&2
  exit 1
fi

# Without a template, mktemp on macOS ignores TMPDIR.
work=$(mktemp -d "${TMPDIR:-/tmp}/reduce-record.XXXXXX")
# A record can hold a directory that cannot be listed, which rm cannot
# remove, so the work folder is opened first.
trap 'chmod -R u+rwX "$work" 2>/dev/null; rm -rf "$work"' EXIT
record="$work/record"
blobs="$record/blobs/sha256"
mkdir "$record"

# head stops reading one byte past the limit, so a larger archive is never
# decompressed whole.
code=0
gzip -dc <"$source" 2>/dev/null | head -c $((max_bytes + 1)) >"$work/source.tar" || code=$?
[ $(($(wc -c <"$work/source.tar"))) -le "$max_bytes" ] || stop "is larger than $max_bytes bytes when decompressed"
[ "$code" -eq 0 ] || stop 'is not a gzip file'
entries=$(tar -tf "$work/source.tar" 2>/dev/null | wc -l) || stop 'is not a tar archive'
[ $((entries)) -le "$max_entries" ] || stop "has more than $max_entries entries"
# tar also reads an archive compressed again inside the gzip stream, and
# expands sparse entries, so the limit applies to what it extracts as well.
code=0
held=$(tar -xOf "$work/source.tar" 2>/dev/null | head -c $((max_bytes + 1)) | wc -c) || code=$?
[ $((held)) -le "$max_bytes" ] || stop "holds more than $max_bytes bytes"
[ "$code" -eq 0 ] || stop 'is not a tar archive'
tar -xf "$work/source.tar" -C "$record" 2>/dev/null || stop 'could not be extracted'
rm "$work/source.tar"
chmod -R u+rwX "$record" 2>/dev/null || stop 'could not be searched'
found=$(find "$record" ! -type d \( ! -type f -o -links +1 \) 2>/dev/null) || stop 'could not be searched'
[ -z "$found" ] || stop 'has a link, or an entry that is not a file or a directory'

# blob_of DIGEST sets blob to the hex part of a SHA-256 digest, and stops at
# anything else, so that a digest from the record can only name a file in
# blobs.
blob_of() {
  [[ $1 =~ ^sha256:[0-9a-f]{64}$ ]] || stop 'has a digest that is not a SHA-256 digest'
  blob=${1#sha256:}
}

# json FILE FILTER [ARG...] prints what the jq FILTER gives for FILE, which
# must hold exactly one JSON value: jq -e takes its status from the last
# value only. It fails when FILTER gives nothing, false or null, and keeps
# the exit status of a halt_error in FILTER. jq's errors can quote the
# record, and this log is public, so they are dropped.
json() {
  local file=$1 filter=$2
  shift 2
  jq -e -r -s "$@" "if length == 1 then .[0] | $filter else halt_error(9) end" "$file" 2>/dev/null
}

edit() {
  local file=$1 filter=$2
  shift 2
  jq -c -j "$@" "$filter" "$file" >"$work/edited" 2>/dev/null || stop 'has a blob that could not be rewritten'
  mv -f "$work/edited" "$file"
}

# move_to_digest HEX moves the rewritten blob HEX to the name of its new
# digest, and sets blob to that name. It adds the move to moves, through
# which repoint changes every descriptor of HEX, and HEX to replaced, which
# the archive must not name.
move_to_digest() {
  local old=$1 sum size
  sum=$(sha256sum "$blobs/$old")
  blob=${sum%% *}
  size=$(($(wc -c <"$blobs/$old")))
  mv "$blobs/$old" "$blobs/$blob"
  moves="$moves${moves:+,}\"sha256:$old\":{\"digest\":\"sha256:$blob\",\"size\":$size}"
  replaced="$replaced $old"
}
repoint='walk(if type == "object" and (.digest | type) == "string" and $moves[.digest]
  then $moves[.digest] as $to | .digest = $to.digest | .size = $to.size else . end)'

# published reports whether the archive to publish, or the compact form of
# its JSON files, in which JSON escapes are decoded, matches grep's PATTERN.
# It exits on a grep error.
published() {
  local code=0
  LC_ALL=C grep -a -q "$@" "$work/record.tar" "$work/record.json" || code=$?
  [ "$code" -le 1 ] || exit "$code"
  return "$code"
}

manifest=$(json "$record/index.json" '.manifests | select(type == "array" and length == 1) | .[0].digest | strings') ||
  stop 'does not have exactly one manifest'
blob_of "$manifest"
manifest=$blob

code=0
layers=$(json "$blobs/$manifest" '
  if ((.config.digest | type == "string" and test("\\A\\S+\\z")) and (.layers | type == "array" and
    all(.[]; (.mediaType | type == "string" and test("\\A\\S+\\z")) and (.digest | type == "string" and test("\\A\\S+\\z"))))) | not
  then halt_error(10)
  elif [.layers[].mediaType] - ["application/json", $otlp, $status, $provenance] != [] then halt_error(11)
  else "config \(.config.digest)", (.layers[] | "\(.mediaType) \(.digest)") end' \
  --arg otlp "$otlp" --arg status "$status_type" --arg provenance "$provenance_type") || code=$?
case $code in
  0) ;;
  11) stop 'has an unknown layer type' ;;
  *) stop 'has a manifest that is not valid' ;;
esac
config='' traces='' statuses='' provenances=''
while read -r kind digest; do
  case $kind in
    config) config=$digest ;;
    "$otlp") traces="$traces $digest" ;;
    "$status_type") statuses="$statuses $digest" ;;
    "$provenance_type") provenances="$provenances $digest" ;;
  esac
done <<<"$layers"
blob_of "$config"
config=$blob

code=0
history=$(json "$blobs/$config" '
  if ((.FrontendAttrs // {} | type == "object") and (.trace.digest // "" | type == "string" and test("\\A\\S*\\z")) and
    (.FrontendAttrs["cache-imports"] // "[]" | type == "string" and
      (try fromjson catch null | type == "array" and all(.[]; .Attrs | type == "object")))) | not
  then halt_error(10)
  elif .FrontendAttrs // {} | keys | any(startswith("build-arg:")) then halt_error(12)
  elif .FrontendAttrs["cache-imports"] // "[]" | fromjson | any(.[]; (.Attrs | keys) - ["ref", "scope"] != []) then halt_error(13)
  else .trace.digest // "", (.FrontendAttrs["cache-imports"] // "[]" | fromjson | map(.Attrs | keys) | tojson) end') || code=$?
case $code in
  0) ;;
  12) stop 'has a build argument, which could hold a secret' ;;
  13) stop 'has an unexpected cache import attribute' ;;
  *) stop 'has a history record that is not valid' ;;
esac
{
  read -r trace
  read -r attributes
} <<<"$history"

[ -n "$trace" ] || stop 'has no trace to remove'
blob_of "$trace"
trace=$blob
[ "$traces" = " sha256:$trace" ] || stop 'has a trace that its history record does not name'
echo "cache import attributes: $attributes"

code=0
matches=$(LC_ALL=C grep -a -c -E "$secret" "$blobs/$trace") || code=$?
[ "$code" -le 1 ] || exit "$code"
echo "secret-shaped lines in the trace: $matches"
rm -f "$blobs/$trace"
replaced=$trace
moves=''

for digest in $provenances; do
  blob_of "$digest"
  provenance=$blob
  code=0
  event=$(json "$blobs/$provenance" '
    if ((.buildDefinition.internalParameters // {} | type == "object") and
      (.buildDefinition.externalParameters.request.args // {} | type == "object")) | not
    then halt_error(10)
    elif .buildDefinition.externalParameters.request.args // {} | keys | any(startswith("build-arg:")) then halt_error(12)
    elif .buildDefinition.internalParameters // {} | has("github_event_payload") then "event"
    else "none" end') || code=$?
  case $code in
    0) ;;
    12) stop 'has a build argument, which could hold a secret' ;;
    *) stop 'has a provenance that is not valid' ;;
  esac
  if [ "$event" = event ]; then
    edit "$blobs/$provenance" 'del(.buildDefinition.internalParameters.github_event_payload)'
    move_to_digest "$provenance"
  fi
done
edit "$blobs/$config" "del(.trace) | $repoint" --argjson moves "{$moves}"
move_to_digest "$config"
config=$blob
edit "$blobs/$manifest" 'del(.layers[] | select(.digest == $trace)) | '"$repoint" \
  --arg trace "sha256:$trace" --argjson moves "{$moves}"
move_to_digest "$manifest"
manifest=$blob
edit "$record/index.json" "$repoint" --argjson moves "{$moves}"

descriptors=$(jq -r --arg history "$blobs/$config" '
  (if input_filename == $history then "history" else "listed" end) as $tag |
  .. | objects | select(has("digest")) | "\($tag) \(.digest) \(.size)"' \
  "$record/index.json" "$blobs/$manifest" "$blobs/$config" 2>/dev/null) ||
  stop 'has a descriptor that does not match a blob'
listed=$(awk '$1 == "listed" { print $2 }' <<<"$descriptors" | LC_ALL=C sort -u)
entries=(blobs/ blobs/sha256/)
files=()
json_files=("$record/index.json" "$record/oci-layout")
for digest in $listed; do
  blob_of "$digest"
  entries+=("blobs/sha256/$blob")
  files+=("$blobs/$blob")
  case "$statuses " in
    *" $digest "*) ;;
    *) json_files+=("$blobs/$blob") ;;
  esac
done
entries+=(index.json oci-layout)

# The script hashes only the blobs it rewrites, so these checks cover the
# blobs it passes on as they came.
sums=$(sha256sum "${files[@]}" 2>/dev/null) || stop 'is missing a blob that it lists'
while read -r sum path; do
  [ "$sum" = "${path##*/}" ] || stop 'has a blob that does not match its digest'
done <<<"$sums"
counts=$(wc -c "${files[@]}")
sizes=' '
while read -r size path; do
  sizes="$sizes${path##*/}:$size "
done <<<"$counts"
while read -r _ digest size; do
  blob_of "$digest"
  case $sizes in
    *" $blob:$size "*) ;;
    *) stop 'has a descriptor that does not match a blob' ;;
  esac
done <<<"$descriptors"

# In ustar, which has no extended headers, neither GNU tar nor bsdtar stores
# extended attributes, and bsdtar writes no macOS AppleDouble entries, which
# the collector rejects. ustar cannot hold a user or group ID above 2097151:
# GNU tar then fails, but bsdtar leaves the entry out and exits 0, so the
# archive's listing is compared with the entries asked for.
tar --format=ustar --numeric-owner --no-recursion -cf "$work/record.tar" -C "$record" "${entries[@]}"
printf '%s\n' "${entries[@]}" >"$work/requested"
tar -tf "$work/record.tar" >"$work/archived"
cmp -s "$work/requested" "$work/archived" || stop 'could not archive every entry'
# The compact form decodes JSON escapes, and the stream form also keeps
# each value of a name that appears twice, of which the compact form keeps
# only the last.
jq -c . "${json_files[@]}" >"$work/record.json" 2>/dev/null || stop 'has a blob that is not valid JSON'
jq -c --stream . "${json_files[@]}" >>"$work/record.json" 2>/dev/null || stop 'has a blob that is not valid JSON'

if published -E "$secret"; then
  stop 'has secret-shaped data outside the trace'
fi
if published -F github_event_payload; then
  stop 'has a GitHub event outside the provenance'
fi
patterns=()
for old in $replaced; do
  patterns+=(-e "$old")
done
if published -F "${patterns[@]}"; then
  stop 'still names a blob it removed or replaced'
fi

gzip -n "$work/record.tar"
mv -f -- "$work/record.tar.gz" "$target"
