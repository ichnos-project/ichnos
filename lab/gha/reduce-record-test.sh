#!/usr/bin/env bash
set -euo pipefail

script=lab/gha/reduce-record.sh
control=testdata/buildkit-v0.34.0/control/pr.dockerbuild
two_platforms=testdata/buildkit-v0.34.0/two-platforms/pr.dockerbuild
otlp=application/vnd.buildkit.otlp.json.v0
status_type=application/vnd.buildkit.status.v0
provenance_type=application/vnd.in-toto+json
escaped_ampersand=$(printf '\\%s' u0026)
escaped_at=$(printf '\\%s' u0040)
escaped_d=$(printf '\\%s' u0064)
escaped_dot=$(printf '\\%s' u002e)
traversal=sha256:../../../planted
repoint='walk(if type == "object" and (.digest | type) == "string" and $moves[.digest]
  then $moves[.digest] as $to | .digest = $to.digest | .size = $to.size else . end)'
# The script stops on any build argument, so the records of these two
# scenarios, base and pull request, are expected to stop.
build_arg_scenarios='changed-build-arg rotating-build-arg'
max_entries=1024
max_bytes=$((64 << 20))
secrets=(
  ghp_planted gho_planted ghs_planted ghu_planted ghr_planted github_pat_planted 'Bearer planted'
  'Basic dXNlcjpwYXNz' '"auth": "dXNlcjpwYXNz"'
  someone@example.test someone%40example.test someone%2540example.test "someone${escaped_at}example.test"
  'https://example.test/a?sv=1&sig=planted' "https://example.test/a?sv=1${escaped_ampersand}sig=planted"
  'https://example.test/a?sv=1%26sig%3Dplanted' 'https://example.test/a%3Fsig%3Dplanted'
  'https://example.test/a?X-Amz-Signature=planted' 'https://example.test/a?X-Goog-Signature=planted'
)

fail() {
  echo "$1" >&2
  exit 1
}

# unpack FIXTURE copies the record FIXTURE into $work/record, and sets the
# names of its blobs. Each fixture is extracted once, and its names are read
# again only when the fixture changes.
unpack() {
  local cache="$work/cache/${1//\//_}"
  if [ ! -d "$cache" ]; then
    mkdir -p "$cache"
    tar -xzf "$1" -C "$cache"
    chmod -R u+w "$cache"
  fi
  rm -rf "$work/record" "$work/output"
  cp -R "$cache" "$work/record"
  blobs="$work/record/blobs/sha256"
  if [ "$1" != "${unpacked:-}" ]; then
    unpacked=$1
    read_names
  fi
}

# read_names sets the names of the blobs of $work/record, and leaves a name
# empty when the record no longer says it.
read_names() {
  manifest=$(jq -r '.manifests[0].digest | ltrimstr("sha256:")' "$work/record/index.json" 2>/dev/null) || manifest=''
  config=$(jq -r '.config.digest | ltrimstr("sha256:")' "$blobs/$manifest" 2>/dev/null) || config=''
  trace=$(jq -r '.trace.digest | ltrimstr("sha256:")' "$blobs/$config" 2>/dev/null) || trace=''
  status_stream=$(jq -r --arg type "$status_type" '.layers[] | select(.mediaType == $type) | .digest | ltrimstr("sha256:")' "$blobs/$manifest" 2>/dev/null) || status_stream=''
  provenances=$(jq -r --arg type "$provenance_type" '.layers[] | select(.mediaType == $type) | .digest | ltrimstr("sha256:")' "$blobs/$manifest" 2>/dev/null) || provenances=''
}

# seal names every blob that a case changed by its digest again, and points
# the descriptors in index.json and in the JSON blobs at the new names, so
# that the record is valid apart from what the case planted. Files whose
# names are not digests are left alone.
seal() {
  local pass sums sum path name size moves file files olds
  for pass in 1 2 3 4 5; do
    sums=$(sha256sum "$blobs"/* 2>/dev/null) || true
    moves=''
    olds=()
    while read -r sum path; do
      name=${path##*/}
      if [[ $name =~ ^[0-9a-f]{64}$ ]] && [ "$sum" != "$name" ]; then
        mv "$path" "$blobs/$sum"
        size=$(($(wc -c <"$blobs/$sum")))
        moves="$moves${moves:+,}\"sha256:$name\":{\"digest\":\"sha256:$sum\",\"size\":$size}"
        olds+=(-e "$name")
      fi
    done <<<"$sums"
    if [ -z "$moves" ]; then
      unpacked=''
      read_names
      return
    fi
    files=$(grep -l -F "${olds[@]}" "$work/record/index.json" "$blobs"/* 2>/dev/null) || true
    for file in $files; do
      if jq --argjson moves "{$moves}" "$repoint" "$file" >"$work/sealed" 2>/dev/null; then
        mv "$work/sealed" "$file"
      fi
    done
  done
  fail "could not name every blob by its digest after $pass passes"
}

pack() {
  tar --format=ustar -czf "$work/input" -C "$work/record" index.json oci-layout blobs
}

edit_json() {
  jq "$2" "$1" >"$work/edited"
  mv "$work/edited" "$1"
}

plant_event() {
  local provenance
  for provenance in $provenances; do
    edit_json "$blobs/$provenance" '.buildDefinition.internalParameters.github_event_payload = {repository: {ssh_url: "git@github.com:a/b.git"}, pusher: {email: "someone@example.test"}}'
  done
}

# check_reduced FULL REDUCED GONE fails unless REDUCED is an OCI layout
# whose blobs are named by their digests and match every descriptor, holds
# neither a trace, nor the digests in GONE, nor the GitHub event, and
# passes TestReducedRecord against FULL. It leaves a file named builds in
# $work when FULL has builds.
check_reduced() {
  local full=$1 reduced=$2 gone=$3 out="$work/reduced" sums sum path digest size reduced_manifest reduced_config
  rm -rf "$out"
  mkdir "$out"
  tar -xzf "$reduced" -C "$out"
  sums=$(sha256sum "$out"/blobs/sha256/*)
  : >"$work/held"
  while read -r sum path; do
    [ "$sum" = "${path##*/}" ] || fail "$full: the reduced record names a blob by another digest"
    echo "${path##*/}" >>"$work/held"
  done <<<"$sums"
  reduced_manifest=$(jq -r '.manifests[0].digest | ltrimstr("sha256:")' "$out/index.json")
  reduced_config=$(jq -r '.config.digest | ltrimstr("sha256:")' "$out/blobs/sha256/$reduced_manifest")
  jq -r '.. | objects | select(has("digest") and has("size")) | "\(.digest) \(.size)"' \
    "$out/index.json" "$out/blobs/sha256/$reduced_manifest" "$out/blobs/sha256/$reduced_config" >"$work/descriptors"
  while read -r digest size; do
    path="$out/blobs/sha256/${digest#sha256:}"
    if [ ! -f "$path" ] || [ "$(wc -c <"$path" | tr -d ' ')" != "$size" ]; then
      fail "$full: the reduced record has a descriptor that does not match a blob"
    fi
  done <"$work/descriptors"
  LC_ALL=C sort -o "$work/held" "$work/held"
  cut -d ' ' -f 1 "$work/descriptors" | sed 's/^sha256://' | LC_ALL=C sort -u >"$work/listed"
  cmp -s "$work/held" "$work/listed" || fail "$full: the reduced record holds a blob that it does not list"
  jq -e --arg otlp "$otlp" 'all(.layers[]; .mediaType != $otlp)' "$out/blobs/sha256/$reduced_manifest" >/dev/null ||
    fail "$full: the reduced manifest still lists a trace"
  jq -e 'has("trace") | not' "$out/blobs/sha256/$reduced_config" >/dev/null ||
    fail "$full: the reduced history record still names a trace"
  gzip -dc "$reduced" >"$work/contents"
  for digest in $gone github_event_payload; do
    if LC_ALL=C grep -a -q -F "$digest" "$work/contents"; then
      fail "$full: the reduced record still holds $digest"
    fi
  done
  if ! ICHNOS_FULL_RECORD=$full ICHNOS_REDUCED_RECORD=$reduced "$top/collect.test" \
    -test.run '^TestReducedRecord$' -test.v >"$work/collect.log" 2>&1 ||
    ! grep -q -- '--- PASS: TestReducedRecord' "$work/collect.log"; then
    cat "$work/collect.log" >&2
    fail "$full: TestReducedRecord does not pass on the reduced record"
  fi
  if grep -q 'has [1-9][0-9]* builds' "$work/collect.log"; then
    : >"$work/builds"
  fi
}

# run_stop DESCRIPTION MESSAGE runs the script on $work/input, and fails
# unless it stops with MESSAGE and writes no output.
run_stop() {
  if "$script" "$work/input" "$work/output" >"$work/stdout" 2>"$work/stderr"; then
    fail "expected a stop: $1"
  fi
  [ ! -e "$work/output" ] || fail "wrote output: $1"
  grep -q -F -- "$2" "$work/stderr" || fail "stopped for another reason: $1: $(cat "$work/stderr")"
}

expect_stop() {
  seal
  pack
  run_stop "$@"
}

# expect_raw_stop is expect_stop for a case whose record must not be sealed:
# one that plants a link, or a blob that does not match its name.
expect_raw_stop() {
  pack
  run_stop "$@"
}

# expect_unprinted VALUE fails if the last stop printed VALUE.
expect_unprinted() {
  if grep -q -F -- "$1" "$work/stdout" "$work/stderr"; then
    fail "printed a value from the record: $1"
  fi
}

# check_fixture FIXTURE expects the build argument stop for the records of
# build_arg_scenarios, and reduces every other record and checks the result.
# It leaves a file named build-arg-stop in $work for each stop.
check_fixture() {
  local fixture=$1 scenario value
  scenario=${fixture%/*}
  scenario=${scenario##*/}
  unpack "$fixture"
  case " $build_arg_scenarios " in
    *" $scenario "*)
      cp "$fixture" "$work/input"
      run_stop "$fixture" 'has a build argument, which could hold a secret'
      jq -r '.FrontendAttrs | to_entries[] | select(.key | startswith("build-arg:")) | .value' "$blobs/$config" >"$work/values"
      [ -s "$work/values" ] || fail "$fixture: has no build argument to look for"
      while read -r value; do
        expect_unprinted "$value"
      done <"$work/values"
      : >"$work/build-arg-stop"
      ;;
    *)
      "$script" "$fixture" "$work/output" >/dev/null
      check_reduced "$fixture" "$work/output" "$trace"
      ;;
  esac
}

check_secret() {
  unpack "$control"
  printf '%s' "$1" >>"$blobs/$status_stream"
  expect_stop "$1 in the status stream" 'has secret-shaped data outside the trace'
}

case_control_keeps_unchanged_blobs() {
  unpack "$control"
  "$script" "$control" "$work/output" >"$work/log"
  tar -tzf "$work/output" >"$work/listing"
  grep -q "$provenances" "$work/listing" || fail 'the provenance changed with no GitHub event to remove'
  grep -q "$status_stream" "$work/listing" || fail 'the status stream changed'
  grep -q 'secret-shaped lines in the trace: [1-9]' "$work/log" || fail 'the signed URLs in the trace were not counted'
}

case_unlisted_blob() {
  local unlisted
  unpack "$control"
  unlisted=$(printf 'unlisted' | sha256sum)
  unlisted=${unlisted%% *}
  printf 'unlisted' >"$blobs/$unlisted"
  pack
  "$script" "$work/input" "$work/output" >/dev/null
  check_reduced "$control" "$work/output" "$trace $unlisted"
}

case_event_in_one_provenance() {
  unpack "$control"
  plant_event
  pack
  "$script" "$work/input" "$work/output" >/dev/null
  check_reduced "$control" "$work/output" "$trace $provenances"
}

case_event_in_two_provenances() {
  unpack "$two_platforms"
  plant_event
  pack
  "$script" "$work/input" "$work/output" >/dev/null
  check_reduced "$two_platforms" "$work/output" "$trace $provenances"
}

case_layer_listed_twice() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '.layers += [.layers[] | select(.mediaType == "%s")]' "$status_type")"
  pack
  "$script" "$work/input" "$work/output" >/dev/null
  [ -z "$(tar -tzf "$work/output" | LC_ALL=C sort | uniq -d)" ] || fail 'the archive holds an entry twice'
}

case_directory_output() {
  mkdir "$work/directory"
  if "$script" "$control" "$work/directory" >/dev/null 2>"$work/stderr"; then
    fail 'accepted a directory as the output'
  fi
  [ -z "$(find "$work/directory" -mindepth 1)" ] || fail 'wrote into a directory given as the output'
  grep -q -F 'is a directory' "$work/stderr" || fail "stopped for another reason: directory output: $(cat "$work/stderr")"
}

case_output_named_like_an_option() {
  (cd "$work" && "$OLDPWD/$script" "$OLDPWD/$control" -reduced.dockerbuild >/dev/null) ||
    fail 'could not write an output whose name starts with a dash'
  [ -f "$work/-reduced.dockerbuild" ] || fail 'wrote no output whose name starts with a dash'
}

case_tar_leaves_out_entries() {
  unpack "$control"
  PATH="$top/tar-shim:$PATH" expect_stop 'archive that leaves out every entry' 'could not archive every entry'
}

case_jq_fails_to_rewrite() {
  unpack "$control"
  PATH="$top/jq-shim:$PATH" expect_stop 'blob that jq fails to rewrite' 'has a blob that could not be rewritten'
}

case_entry_limit() {
  local entries i
  unpack "$control"
  pack
  entries=$(tar -tzf "$work/input" | wc -l)
  for i in $(seq $((entries + 1)) "$max_entries"); do
    : >"$blobs/ichnos-fake-token-$i"
  done
  pack
  [ "$(tar -tzf "$work/input" | wc -l | tr -d ' ')" -eq "$max_entries" ] || fail "could not build a record of $max_entries entries"
  "$script" "$work/input" "$work/output" >/dev/null || fail "stopped on a record of $max_entries entries"
  rm "$work/output"
  : >"$blobs/ichnos-fake-token-0"
  pack
  run_stop "record of more than $max_entries entries" "has more than $max_entries entries"
  expect_unprinted ichnos-fake-token
}

case_size_limit() {
  unpack "$control"
  tar --format=ustar -b 1 -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  dd if=/dev/zero of="$blobs/padding" bs=1 count=0 seek=$((max_bytes - $(wc -c <"$work/input.tar") - 512)) 2>/dev/null
  tar --format=ustar -b 1 -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  [ "$(wc -c <"$work/input.tar" | tr -d ' ')" -eq "$max_bytes" ] || fail "could not build a record of $max_bytes bytes"
  gzip -1 -c "$work/input.tar" >"$work/input"
  "$script" "$work/input" "$work/output" >/dev/null || fail "stopped on a record of $max_bytes bytes"
  rm "$work/output"
  printf x >>"$work/input.tar"
  gzip -1 -c "$work/input.tar" >"$work/input"
  run_stop "record one byte over $max_bytes bytes" "is larger than $max_bytes bytes when decompressed"
}

case_build_arg_in_history_record() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["build-arg:PLANTED"] = "ichnos-fake-value"'
  expect_stop 'build argument in the history record' 'has a build argument, which could hold a secret'
  expect_unprinted ichnos-fake-value
}

case_build_arg_in_provenance() {
  unpack "$control"
  edit_json "$blobs/$provenances" '.buildDefinition.externalParameters.request.args["build-arg:PLANTED"] = "ichnos-fake-value"'
  expect_stop 'build argument in the provenance' 'has a build argument, which could hold a secret'
  expect_unprinted ichnos-fake-value
}

case_email_in_provenance() {
  unpack "$control"
  edit_json "$blobs/$provenances" '.planted = "someone@example.test"'
  expect_stop 'email address in the provenance' 'has secret-shaped data outside the trace'
}

case_email_in_rewritten_provenance() {
  unpack "$control"
  plant_event
  edit_json "$blobs/$provenances" '.planted = "someone@example.test"'
  expect_stop 'email address in a provenance that is rewritten' 'has secret-shaped data outside the trace'
}

case_jwt_in_history_record() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs.planted = "eyJhbGciOiJIUzI1NiJ9.eyJ4IjoxfQ.sig"'
  expect_stop 'JWT in the history record' 'has secret-shaped data outside the trace'
}

case_token_cache_import_attribute() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":{\"scope\":\"lab\",\"token\":\"opaque\"}}]"'
  expect_stop 'token attribute on a cache import' 'has an unexpected cache import attribute'
}

case_cache_import_attribute_named_like_a_token() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":{\"ichnos-fake-token\":\"x\"}}]"'
  expect_stop 'cache import attribute named like a token' 'has an unexpected cache import attribute'
  expect_unprinted ichnos-fake-token
}

case_cache_imports_not_json() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":{\"token\":\"ichnos-fake-token\"}"'
  expect_stop 'cache imports that are not JSON' 'has a history record that is not valid'
  expect_unprinted ichnos-fake-token
}

case_layers_not_a_list() {
  unpack "$control"
  edit_json "$blobs/$manifest" '.layers = "ichnos-fake-token"'
  expect_stop 'layers that are not a list' 'has a manifest that is not valid'
  expect_unprinted ichnos-fak
}

case_history_record_with_two_values() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":{\"token\":\"x\",\"ichnos-fake-token\":\"x\"}}]"'
  printf '{}' >>"$blobs/$config"
  expect_stop 'history record followed by a second JSON value' 'has a history record that is not valid'
  expect_unprinted ichnos-fake-token
}

case_provenance_with_two_values() {
  unpack "$control"
  printf '{}' >>"$blobs/$provenances"
  expect_stop 'provenance followed by a second JSON value' 'has a provenance that is not valid'
}

case_provenance_parameters_not_an_object() {
  unpack "$control"
  edit_json "$blobs/$provenances" '.buildDefinition.internalParameters = "x"'
  expect_stop 'provenance whose internal parameters are not an object' 'has a provenance that is not valid'
}

case_escaped_event_key() {
  local result
  unpack "$control"
  result=$(jq -r '.layers[] | select(.mediaType == "application/json") | .digest | ltrimstr("sha256:")' "$blobs/$manifest")
  printf '{"github_event_payloa%s":{}}' "$escaped_d" >"$blobs/$result"
  expect_stop 'JSON-escaped GitHub event key' 'has a GitHub event outside the provenance'
}

case_hard_link() {
  unpack "$control"
  ln "$blobs/$status_stream" "$blobs/extra"
  expect_raw_stop 'hard link to a blob' 'has a link, or an entry that is not a file or a directory'
}

case_status_stream_linked_to_trace() {
  unpack "$control"
  rm "$blobs/$status_stream"
  ln "$blobs/$trace" "$blobs/$status_stream"
  expect_raw_stop 'status stream linked to the trace' 'has a link, or an entry that is not a file or a directory'
}

case_symbolic_link() {
  unpack "$control"
  printf 'outside the record' >"$work/outside"
  rm "$blobs/$status_stream"
  ln -s ../../../outside "$blobs/$status_stream"
  expect_raw_stop 'symbolic link in the record' 'has a link, or an entry that is not a file or a directory'
}

case_blob_changed_in_place() {
  unpack "$control"
  printf 'X' | dd of="$blobs/$status_stream" bs=1 seek=4 conv=notrunc 2>/dev/null
  expect_raw_stop 'blob changed in place' 'has a blob that does not match its digest'
}

case_blob_checked_before_archiving() {
  unpack "$control"
  printf 'someone@example.test' >>"$blobs/$status_stream"
  expect_raw_stop 'changed blob that holds an email address' 'has a blob that does not match its digest'
}

case_blob_copied_under_another_name() {
  unpack "$control"
  cp "$blobs/$provenances" "$blobs/$status_stream"
  expect_raw_stop 'blob copied under another name' 'has a blob that does not match its digest'
}

case_descriptor_with_another_size() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s") | .size) = 1' "$status_type")"
  expect_stop 'descriptor with another size' 'has a descriptor that does not match a blob'
}

# case_descriptor_without_a_size leaves out the size in both descriptors of
# the status stream, so that no descriptor checks the blob's size.
case_descriptor_without_a_size() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s")) |= del(.size)' "$status_type")"
  edit_json "$blobs/$config" '.logs |= del(.size)'
  expect_stop 'status stream listed without a size' 'has a descriptor that does not match a blob'
}

case_descriptor_of_a_missing_blob() {
  unpack "$control"
  edit_json "$blobs/$config" "$(printf '.planted = {digest: "sha256:%064d", size: 1}' 0)"
  expect_stop 'descriptor of a blob that the record lacks' 'has a descriptor that does not match a blob'
}

case_event_outside_provenance() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs.planted = "github_event_payload"'
  expect_stop 'GitHub event outside the provenance' 'has a GitHub event outside the provenance'
}

case_trace_digest_in_history_record() {
  unpack "$control"
  edit_json "$blobs/$config" "$(printf '.FrontendAttrs.planted = "sha256:%s"' "$trace")"
  expect_stop 'trace digest in the history record' 'still names a blob it removed or replaced'
}

case_replaced_provenance_digest_in_status_stream() {
  unpack "$control"
  plant_event
  seal
  printf '%s' "$provenances" >>"$blobs/$status_stream"
  expect_stop 'replaced provenance digest in the status stream' 'still names a blob it removed or replaced'
}

case_history_record_names_another_trace() {
  unpack "$control"
  edit_json "$blobs/$config" "$(printf '.trace.digest = "sha256:%s"' "$status_stream")"
  expect_stop 'history record naming another trace' 'has a trace that its history record does not name'
}

case_two_trace_layers() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '.layers += [.layers[] | select(.mediaType == "%s")]' "$otlp")"
  expect_stop 'two trace layers' 'has a trace that its history record does not name'
}

case_manifest_digest_outside_blobs() {
  unpack "$control"
  edit_json "$work/record/index.json" "$(printf '.manifests[0].digest = "%s"' "$traversal")"
  expect_stop 'manifest digest outside blobs' 'has a digest that is not a SHA-256 digest'
}

case_history_record_digest_outside_blobs() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '.config.digest = "%s"' "$traversal")"
  expect_stop 'history record digest outside blobs' 'has a digest that is not a SHA-256 digest'
}

case_trace_digest_outside_blobs() {
  unpack "$control"
  edit_json "$blobs/$config" "$(printf '.trace.digest = "%s"' "$traversal")"
  expect_stop 'trace digest outside blobs' 'has a digest that is not a SHA-256 digest'
}

case_provenance_digest_outside_blobs() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s") | .digest) = "%s"' "$provenance_type" "$traversal")"
  expect_stop 'provenance digest outside blobs' 'has a digest that is not a SHA-256 digest'
}

case_status_stream_digest_outside_blobs() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s") | .digest) = "%s"' "$status_type" "$traversal")"
  expect_stop 'status stream digest outside blobs' 'has a digest that is not a SHA-256 digest'
}

case_missing_status_stream() {
  unpack "$control"
  rm "$blobs/$status_stream"
  expect_stop 'missing status stream' 'is missing a blob that it lists'
}

case_no_trace() {
  unpack "$control"
  edit_json "$blobs/$config" 'del(.trace)'
  expect_stop 'record with no trace' 'has no trace to remove'
}

case_unknown_layer_type() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s") | .mediaType) = "application/vnd.buildkit.otlp.json.v1"' "$otlp")"
  expect_stop 'unknown layer type' 'has an unknown layer type'
}

case_two_manifests() {
  unpack "$control"
  edit_json "$work/record/index.json" '.manifests += .manifests'
  expect_stop 'more than one manifest' 'does not have exactly one manifest'
}

case_not_a_record() {
  printf 'not a record' >"$work/input"
  run_stop 'file that is not a record' 'is not a gzip file'
}

case_not_a_tar_archive() {
  printf 'not a tar archive' | gzip >"$work/input"
  run_stop 'gzip file that is not a tar archive' 'is not a tar archive'
}

case_entry_under_a_file() {
  unpack "$control"
  tar --format=ustar -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  mkdir -p "$work/extra/index.json"
  printf 'x' >"$work/extra/index.json/x"
  tar --format=ustar -rf "$work/input.tar" -C "$work/extra" index.json/x
  gzip -c "$work/input.tar" >"$work/input"
  run_stop 'entry under a file' 'could not be extracted'
}

case_compressed_twice() {
  local held
  unpack "$control"
  tar --format=ustar -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  held=$(tar -xOf "$work/input.tar" | wc -c)
  dd if=/dev/zero of="$blobs/padding" bs=1 count=0 seek=$((max_bytes - held)) 2>/dev/null
  tar --format=ustar -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  [ "$(tar -xOf "$work/input.tar" | wc -c | tr -d ' ')" -eq "$max_bytes" ] || fail "could not build a record that holds $max_bytes bytes"
  gzip -1 -c "$work/input.tar" | gzip -1 >"$work/input"
  "$script" "$work/input" "$work/output" >/dev/null || fail "stopped on a record compressed twice that holds $max_bytes bytes"
  rm "$work/output"
  printf 'x' >>"$blobs/padding"
  tar --format=ustar -cf "$work/input.tar" -C "$work/record" index.json oci-layout blobs
  gzip -1 -c "$work/input.tar" | gzip -1 >"$work/input"
  run_stop "record compressed twice that holds one byte more than $max_bytes" "holds more than $max_bytes bytes"
}

case_sparse_entry() {
  local flags
  unpack "$control"
  rm "$blobs/$status_stream"
  dd if=/dev/zero of="$blobs/$status_stream" bs=1 count=0 seek="$max_bytes" 2>/dev/null
  printf 'x' >>"$blobs/$status_stream"
  if gnu_tar; then
    flags=(--sparse --format=gnu)
  else
    flags=(--format=pax)
  fi
  tar "${flags[@]}" -czf "$work/input" -C "$work/record" index.json oci-layout blobs
  [ "$(gzip -dc "$work/input" | wc -c | tr -d ' ')" -lt "$max_bytes" ] || fail 'could not build an archive with a sparse entry'
  run_stop 'sparse entry larger than the limit' "holds more than $max_bytes bytes"
}

gnu_tar() {
  case $(tar --version 2>/dev/null) in
    *'GNU tar'*) return 0 ;;
    *) return 1 ;;
  esac
}

# pack_closed writes $work/record to $work/input.tar with its blobs/sha256
# directory stored first and with mode 0300, so that once extracted it
# cannot be listed. Coming first, it gets that mode after tar extracts the
# files in it. GNU tar cannot open such a directory to archive it, so there
# --mode sets the mode instead.
pack_closed() {
  local names=() file
  for file in "$blobs"/*; do
    names+=("blobs/sha256/${file##*/}")
  done
  if gnu_tar; then
    tar --format=ustar --no-recursion --mode=0300 -cf "$work/input.tar" -C "$work/record" blobs/sha256/
    tar --format=ustar --no-recursion -rf "$work/input.tar" -C "$work/record" index.json oci-layout blobs/ "${names[@]}"
  else
    chmod 0300 "$blobs"
    tar --format=ustar --no-recursion -cf "$work/input.tar" -C "$work/record" blobs/sha256/ index.json oci-layout blobs/ "${names[@]}"
    chmod 0700 "$blobs"
  fi
}

case_unreadable_directory() {
  unpack "$control"
  printf 'outside the record' >"$work/outside"
  rm "$blobs/$status_stream"
  ln -s ../../../outside "$blobs/$status_stream"
  pack_closed
  gzip -c "$work/input.tar" >"$work/input"
  run_stop 'link in a directory that cannot be listed' 'has a link, or an entry that is not a file or a directory'
}

case_work_folder_removed_after_a_stop() {
  unpack "$control"
  : >"$work/tmp"
  if TMPDIR="$work/tmp" "$script" "$control" "$work/output" >/dev/null 2>&1 || [ -e "$work/output" ]; then
    fail 'did not make its work folder in TMPDIR, so this case cannot see it'
  fi
  rm "$work/tmp"
  pack_closed
  mkdir -p "$work/extra/index.json" "$work/tmp"
  printf 'x' >"$work/extra/index.json/x"
  tar --format=ustar -rf "$work/input.tar" -C "$work/extra" index.json/x
  gzip -c "$work/input.tar" >"$work/input"
  export TMPDIR="$work/tmp"
  run_stop 'stop before the work folder is opened' 'could not be extracted'
  [ -z "$(find "$work/tmp" -mindepth 1 2>/dev/null)" ] || fail 'left a work folder that cannot be listed'
}

case_find_fails() {
  unpack "$control"
  PATH="$top/find-shim:$PATH" expect_stop 'record that find cannot search' 'could not be searched'
}

case_value_hidden_by_a_duplicate_name() {
  local result
  unpack "$control"
  result=$(jq -r '.layers[] | select(.mediaType == "application/json") | .digest | ltrimstr("sha256:")' "$blobs/$manifest")
  {
    printf '{"planted":"someone@example%stest","planted":0,' "$escaped_dot"
    tail -c +2 "$blobs/$result"
  } >"$work/edited"
  mv "$work/edited" "$blobs/$result"
  expect_stop 'email address hidden by a duplicate name' 'has secret-shaped data outside the trace'
}

case_history_record_digest_with_a_trailing_newline() {
  unpack "$control"
  edit_json "$blobs/$manifest" '.config.digest += "\n"'
  expect_stop 'history record digest with a trailing newline' 'has a manifest that is not valid'
}

case_media_type_with_a_trailing_newline() {
  unpack "$control"
  edit_json "$blobs/$manifest" "$(printf '(.layers[] | select(.mediaType == "%s") | .mediaType) += "\\n"' "$status_type")"
  expect_stop 'media type with a trailing newline' 'has a manifest that is not valid'
}

case_trace_digest_with_a_trailing_newline() {
  unpack "$control"
  edit_json "$blobs/$config" '.trace.digest += "\n"'
  expect_stop 'trace digest with a trailing newline' 'has a history record that is not valid'
}

case_blob_not_json() {
  local result
  unpack "$control"
  result=$(jq -r '.layers[] | select(.mediaType == "application/json") | .digest | ltrimstr("sha256:")' "$blobs/$manifest")
  printf 'not json' >"$blobs/$result"
  expect_stop 'result manifest that is not JSON' 'has a blob that is not valid JSON'
}

case_request_args_not_an_object() {
  unpack "$control"
  edit_json "$blobs/$provenances" '.buildDefinition.externalParameters.request.args = "x"'
  expect_stop 'request arguments that are not an object' 'has a provenance that is not valid'
}

case_frontend_attributes_not_an_object() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs = "x"'
  expect_stop 'frontend attributes that are not an object' 'has a history record that is not valid'
}

case_cache_import_attributes_not_an_object() {
  unpack "$control"
  edit_json "$blobs/$config" '.FrontendAttrs["cache-imports"] = "[{\"Type\":\"gha\",\"Attrs\":\"x\"}]"'
  expect_stop 'cache import attributes that are not an object' 'has a history record that is not valid'
}

case_trace_digest_not_a_string() {
  unpack "$control"
  edit_json "$blobs/$config" '.trace.digest = 1'
  expect_stop 'trace digest that is not a string' 'has a history record that is not valid'
}

# run CHECK runs one check: fixture=PATH, secret=VALUE or the name of a
# case_ function.
run() {
  case $1 in
    fixture=*) check_fixture "${1#fixture=}" ;;
    secret=*) check_secret "${1#secret=}" ;;
    case_*) "$1" ;;
    *) fail "unknown check: $1" ;;
  esac
}

if [ "${1:-}" = --run ]; then
  top=$2
  work=$3
  run "$4"
  exit
fi

top=$(mktemp -d "${TMPDIR:-/tmp}/reduce-record-test.XXXXXX")
# Some checks leave a directory that cannot be listed, which rm cannot
# remove, so the folder is opened first.
trap 'chmod -R u+rwX "$top" 2>/dev/null; rm -rf "$top"' EXIT
go test -c -o "$top/collect.test" ./internal/collect

# The shim tar writes an empty archive in place of the one asked for, as
# bsdtar does when no entry fits the ustar format, and the shim jq fails to
# rewrite a blob. Each runs the real tool for everything else. The shim
# find always fails.
REAL_TAR=$(command -v tar)
REAL_JQ=$(command -v jq)
export REAL_TAR REAL_JQ
mkdir "$top/tar-shim" "$top/jq-shim" "$top/find-shim"
printf '#!/bin/sh\nexit 1\n' >"$top/find-shim/find"
chmod +x "$top/find-shim/find"
cat >"$top/tar-shim/tar" <<'EOF'
#!/usr/bin/env bash
previous=
for arg; do
  if [ "$previous" = -cf ]; then
    head -c 1024 /dev/zero >"$arg"
    exit 0
  fi
  previous=$arg
done
exec "$REAL_TAR" "$@"
EOF
cat >"$top/jq-shim/jq" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = -c ] && [ "$2" = -j ]; then
  exit 5
fi
exec "$REAL_JQ" "$@"
EOF
chmod +x "$top/tar-shim/tar" "$top/jq-shim/jq"

# The cases come first because the slowest checks are among them, so that
# none of those starts last.
checks=()
for name in $(declare -F | awk '$3 ~ /^case_/ { print $3 }'); do
  checks+=("$name")
done
expected_build_arg_stops=0
for fixture in testdata/buildkit-*/*/*.dockerbuild; do
  checks+=("fixture=$fixture")
  scenario=${fixture%/*}
  case " $build_arg_scenarios " in
    *" ${scenario##*/} "*) expected_build_arg_stops=$((expected_build_arg_stops + 1)) ;;
  esac
done
for value in "${secrets[@]}"; do
  checks+=("secret=$value")
done

# Each check runs in another run of this script, in its own folder, with at
# most one run for each processor, up to 16. The results are read in the
# order of checks, so the report does not depend on timing, and every
# failure is reported.
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)
[ "$jobs" -le 16 ] || jobs=16
n=0
for check in "${checks[@]}"; do
  n=$((n + 1))
  mkdir "$top/check-$n"
  printf '%s\0%s\0' "$top/check-$n" "$check"
done | xargs -0 -n 2 -P "$jobs" sh -c '"$0" --run "$1" "$2" "$3" >"$2/log" 2>&1; echo $? >"$2/status"' "$0" "$top"

failed=0
with_builds=0
build_arg_stops=0
n=0
for check in "${checks[@]}"; do
  n=$((n + 1))
  if [ "$(cat "$top/check-$n/status")" != 0 ]; then
    {
      echo "--- $check"
      cat "$top/check-$n/log"
    } >&2
    failed=$((failed + 1))
  fi
  case $check in
    fixture=*)
      [ ! -e "$top/check-$n/builds" ] || with_builds=$((with_builds + 1))
      [ ! -e "$top/check-$n/build-arg-stop" ] || build_arg_stops=$((build_arg_stops + 1))
      ;;
  esac
done
[ "$failed" -eq 0 ] || fail "reduce-record: $failed of $n checks failed"
[ "$with_builds" -gt 0 ] || fail 'no fixture has builds, so none checks that the collector reads them'
[ "$expected_build_arg_stops" -gt 0 ] || fail 'no fixture is a build argument scenario'
[ "$build_arg_stops" -eq "$expected_build_arg_stops" ] ||
  fail "expected the build argument stop on $expected_build_arg_stops fixtures, not $build_arg_stops"

echo "reduce-record: all $n checks pass"
