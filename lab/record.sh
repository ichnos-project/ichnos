#!/usr/bin/env bash
set -euo pipefail

buildkit_version=v0.34.0
buildkit_image="moby/buildkit:$buildkit_version@sha256:b059f8d7226d0b326bc871af489a5dddf65e36af4d20d14251b072974d9ee87c"
alpine_3_22='alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f'
alpine_3_21='alpine:3.21@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd'
busybox_image='busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec'
registry_image='registry:3.1.2@sha256:01cd312e67026f06f5d89c3e133a4a9adc7cb9eeb9b1194e0b2954dfd770efd2'
builder=ichnos-lab
assets=ichnos-lab-assets
registry=ichnos-lab-registry
locked_registry=ichnos-lab-locked-registry
open_cache=localhost:5000/ichnos-lab
locked_cache=localhost:5002/ichnos-lab
unreachable_cache=localhost:5999/ichnos-lab
platform=linux/amd64
context=.
export_cache=(--cache-to type=local,dest=../cache,mode=max)
import_cache=(--cache-from type=local,src=../cache)

repo=$(cd "$(dirname "$0")/.." && pwd)
out="$repo/testdata/buildkit-$buildkit_version"
work=/tmp/ichnos-lab
records="$work/records"
scenarios=(
  control changed-instruction changed-copy-file changed-base-image no-cache-import added-step added-copy
  changed-dockerignore multi-stage removed-step reordered-steps many-steps added-env added-user
  changed-build-arg rotating-build-arg min-mode-needed overwritten-cache identical-lines changed-next-to-added
  two-platforms subdirectory-context wildcard-copy two-cache-imports copy-link remote-add named-context
  no-cache-filter registry-cache failed-cache-import one-failed-cache-import git-context-dockerignore
  bind-mount cache-mount-from
)

known() {
  local name

  for name in "${scenarios[@]}"; do
    [ "$name" = "$1" ] && return 0
  done
  return 1
}

# parse_addresses prints the addresses in the output of ip addr or ifconfig on
# its input, other than loopback.
parse_addresses() {
  awk '{ for (i = 1; i < NF; i++) if ($i == "inet" || $i == "inet6") print $(i + 1) }' |
    sed 's|/.*||; s|%.*||' | grep -v -x -E '127\..*|::1' || true
}

# host_addresses prints this machine's network addresses, other than loopback.
host_addresses() {
  if command -v ip >/dev/null; then
    ip addr show
  else
    ifconfig
  fi | parse_addresses
}

context_files() {
  local dir=$1 app_text=$2

  mkdir -p "$dir"
  printf 'level=info\n' >"$dir/config.txt"
  printf '%s\n' "$app_text" >"$dir/app.txt"
  printf 'extra\n' >"$dir/extra.txt"
}

fixture() {
  local dir=$1 image=$2 wc_flag=$3 app_text=$4 inserted=$5 appended=$6

  context_files "$dir" "$app_text"
  {
    printf 'FROM %s\nWORKDIR /app\n' "$image"
    [ -z "$inserted" ] || printf '%s\n' "$inserted"
    printf 'COPY config.txt .\nRUN wc %s config.txt > size.txt\n' "$wc_flag"
    printf 'COPY app.txt .\nRUN cat config.txt app.txt > bundle.txt\n'
    [ -z "$appended" ] || printf '%s\n' "$appended"
  } >"$dir/Dockerfile"
}

fixture_lines() {
  local dir=$1 app_text=$2
  shift 2

  context_files "$dir" "$app_text"
  printf '%s\n' "$@" >"$dir/Dockerfile"
}

same_dockerfile() {
  local name=$1 base_app_text=$2 pr_app_text=$3
  shift 3

  fixture_lines "$work/$name/base" "$base_app_text" "$@"
  fixture_lines "$work/$name/pr" "$pr_app_text" "$@"
}

# lab_git runs git with a fake identity and a fixed date, so that a commit is
# the same in every recording.
lab_git() {
  local -x GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  local -x GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z

  git -c user.name=ichnos-fake -c user.email=ichnos-fake@example.invalid "$@"
}

# Build a committed fixture from an absolute path: buildx leaves out vcs:localdir:* for a
# relative path through a symlink, such as /tmp on macOS.
commit_fixture() {
  local dir=$1

  lab_git -C "$dir" init -q
  lab_git -C "$dir" remote add origin https://example.invalid/ichnos-fake/lab.git
  lab_git -C "$dir" add .
  lab_git -C "$dir" commit -q -m fixture
}

# wait_for_registry waits until a registry answers with the status that its
# scenarios need, so that one still starting is not recorded as one that cannot
# be reached.
wait_for_registry() {
  local container=$1 port=$2 status=$3 answer try

  for try in $(seq 30); do
    answer=$(docker exec "$container" wget -S -T 2 -O /dev/null "http://127.0.0.1:$port/v2/" 2>&1 || true)
    case $answer in
      *"HTTP/1.1 $status"*) return 0 ;;
    esac
    sleep 1
  done
  echo "$container did not answer with $status after $try tries" >&2
  exit 1
}

# check_record stops the run if a record holds the local user name, host name,
# macOS temporary path or a network address of this machine, read again for each
# record in case one changed during the run. Every connection in the trace
# starts from the builder or from loopback, so any other local address in it is
# this machine's, even one that it no longer has.
check_record() {
  local record=$1 status=0 blob

  gzip -dc "$record" >"$work/record.tar"
  { cat "$work/private.txt"; host_addresses; } >"$work/private-now.txt"
  LC_ALL=C grep -a -q -F -f "$work/private-now.txt" "$work/record.tar" || status=$?
  case $status in
    0)
      echo "$record contains a local user, host, network address or temporary path" >&2
      exit 1
      ;;
    1) ;;
    *) exit "$status" ;;
  esac

  blob=$(tar -xOf "$work/record.tar" index.json | jq -r '.manifests[0].digest | ltrimstr("sha256:")')
  blob=$(tar -xOf "$work/record.tar" "blobs/sha256/$blob" |
    jq -r '.layers[] | select(.mediaType == "application/vnd.buildkit.otlp.json.v0") | .digest | ltrimstr("sha256:")')
  tar -xOf "$work/record.tar" "blobs/sha256/$blob" |
    jq -r '.. | objects | select(.Key? | IN("http.local", "network.local.address", "net.sock.host.addr")) | .Value.Value' |
    sed -E 's/^\[(.*)\]:[0-9]+$/\1/; s/^([0-9.]+):[0-9]+$/\1/' | sort -u >"$work/local.txt"
  status=0
  grep -q -v -x -F -f "$work/allowed.txt" "$work/local.txt" || status=$?
  case $status in
    0)
      echo "$record has a connection from an address other than the builder's or loopback" >&2
      exit 1
      ;;
    1) ;;
    *) exit "$status" ;;
  esac
}

# selected reports whether this run records the scenario: every one when no
# scenario is named. A scenario must be in the list, which checks the names a
# run is given.
selected() {
  if ! known "$1"; then
    echo "$1 is missing from the list of scenarios" >&2
    exit 1
  fi
  case $selection in
    all | *" $1 "*) return 0 ;;
  esac
  return 1
}

build() {
  local scenario=$1 side=$2
  shift 2

  selected "$scenario" || return 0
  docker buildx prune --builder "$builder" --all --force >/dev/null
  (cd "$work/$scenario/$side" && docker buildx build --builder "$builder" --platform "$platform" --progress=quiet \
    --metadata-file ../metadata.json --output type=oci,dest=../image.tar "$@" "$context" >/dev/null)
}

record() {
  local scenario=$1 side=$2
  local target="$records/$scenario/$side.dockerbuild"
  local ref

  selected "$scenario" || return 0
  build "$@"
  ref=$(jq -r '."buildx.build.ref"' "$work/$scenario/metadata.json")

  mkdir -p "$records/$scenario"
  docker buildx history export --builder "$builder" --finalize "${ref##*/}" -o "$target"
  check_record "$target"
  echo "$scenario/$side: recorded"
}

scenario() {
  local name=$1 pr_image=$2 pr_wc_flag=$3 pr_app_text=$4 cache_import=$5
  local pr_inserted=${6:-} pr_appended=${7:-}

  fixture "$work/$name/base" "$alpine_3_22" -c one '' ''
  fixture "$work/$name/pr" "$pr_image" "$pr_wc_flag" "$pr_app_text" "$pr_inserted" "$pr_appended"
  record_pair "$name" "$cache_import"
}

record_pair() {
  local name=$1 cache_import=$2

  record "$name" base "${export_cache[@]}"
  if [ "$cache_import" = yes ]; then
    record "$name" pr "${import_cache[@]}"
  else
    record "$name" pr
  fi
}

# isolate_environment makes every git and docker command use the lab's own
# configuration, and docker the default socket. buildx would otherwise fetch
# registry tokens itself, on this machine, and export that request, with this
# machine's network address, into the record.
isolate_environment() {
  unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT \
    BUILDX_CONFIG DOCKER_CONTEXT DOCKER_HOST
  export BUILDKIT_NO_CLIENT_TOKEN=true
}

# select_scenarios stops the run unless every name it is given is a scenario,
# and selects those scenarios, or every one when it is given none.
select_scenarios() {
  local name

  for name in "$@"; do
    if ! known "$name"; then
      echo "$name: no such scenario" >&2
      exit 1
    fi
  done
  if [ "$#" -eq 0 ]; then
    selection=all
  else
    selection=" $* "
  fi
}

# docker_config writes the lab's own Docker configuration into dir, from the
# user's configuration directory. The lab logs in to no registry, so the
# configuration keeps the user's credential store and settings out of every
# build. It keeps where the user's CLI plugins are, because buildx is one. A
# configuration with no auths entry makes the CLI fall back to the platform's
# credential store, such as the macOS keychain, so it gets one for a host that
# cannot exist.
docker_config() {
  local user_config=$1 dir=$2

  mkdir "$dir"
  ln -s "$user_config/cli-plugins" "$dir/cli-plugins"
  if [ -f "$user_config/config.json" ]; then
    jq '{cliPluginsExtraDirs}' "$user_config/config.json"
  else
    echo '{}'
  fi | jq '. + {auths: {"ichnos-lab.invalid": {}}}' >"$dir/config.json"
}

# private_values writes what a record must not hold to $work/private.txt: this
# machine's network addresses other than loopback, the local user name, the host
# name and the macOS temporary path. It stops the run if it finds no address.
private_values() {
  host_addresses >"$work/private.txt"
  if [ ! -s "$work/private.txt" ]; then
    echo "found no network address of this machine to check records against" >&2
    exit 1
  fi
  printf '%s\n' "$(id -un)" "$(hostname -s)" /var/folders >>"$work/private.txt"
}

# check_no_builder stops the run if a builder container is left from an earlier
# run. The builder's metadata is in the lab's own configuration, so buildx would
# start that container as if it were new.
check_no_builder() {
  if docker container inspect "buildx_buildkit_${builder}0" >/dev/null 2>&1; then
    echo "buildx_buildkit_${builder}0 is left from an earlier run: remove it first" >&2
    exit 1
  fi
}

# allowed_addresses writes the addresses that a connection in a trace may start
# from to $work/allowed.txt: loopback and the builder's own.
allowed_addresses() {
  {
    printf '%s\n' 127.0.0.1 ::1
    docker container inspect --format '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' "buildx_buildkit_${builder}0"
  } >"$work/allowed.txt"
}

# replace_records moves the run's records into testdata: all of them when no
# scenario is named, and otherwise only the named ones, so that the other
# records stay as they are. It stops, replacing nothing, if a named scenario
# recorded nothing.
replace_records() {
  local name

  if [ "$selection" = all ]; then
    rm -rf "${out:?}"
    mkdir -p "$(dirname "$out")"
    mv "$records" "$out"
    return 0
  fi
  for name in "$@"; do
    if [ ! -d "$records/$name" ]; then
      echo "$name: recorded nothing" >&2
      exit 1
    fi
  done
  mkdir -p "$out"
  for name in "$@"; do
    if [ -d "$records/$name" ]; then
      rm -rf "${out:?}/$name"
      mv "$records/$name" "$out/$name"
    fi
  done
}

# A shell that sources this file, as lab/record-test.sh does, gets the
# functions above and runs nothing below.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

isolate_environment
select_scenarios "$@"
mkdir "$work"
trap 'rm -rf "$work"' EXIT
docker_config "${DOCKER_CONFIG:-$HOME/.docker}" "$work/docker"
export DOCKER_CONFIG="$work/docker"
private_values
check_no_builder
docker buildx create --name "$builder" --driver docker-container \
  --driver-opt "image=$buildkit_image" unix:///var/run/docker.sock >/dev/null
trap 'docker rm --force "$assets" "$registry" "$locked_registry" >/dev/null 2>&1 || true; docker buildx rm "$builder" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
docker buildx inspect --bootstrap "$builder" >/dev/null
allowed_addresses
# The web server and registries join the builder's network namespace and listen
# on its loopback only, so nothing but the builder can reach them. A missing
# htpasswd file is created with a random password, which the registry logs, and
# the lab sends no credentials, so the locked registry denies every request.
builder_network="container:buildx_buildkit_${builder}0"
docker run --detach --rm --name "$assets" --network "$builder_network" "$busybox_image" \
  sh -c 'mkdir -p /www && exec httpd -f -p 127.0.0.1:8080 -h /www' >/dev/null
docker run --detach --rm --name "$registry" --network "$builder_network" \
  --env REGISTRY_HTTP_ADDR=127.0.0.1:5000 --env REGISTRY_HTTP_DEBUG_ADDR=127.0.0.1:5001 "$registry_image" >/dev/null
docker run --detach --rm --name "$locked_registry" --network "$builder_network" \
  --env REGISTRY_HTTP_ADDR=127.0.0.1:5002 --env REGISTRY_HTTP_DEBUG_ADDR=127.0.0.1:5003 \
  --env REGISTRY_AUTH=htpasswd --env REGISTRY_AUTH_HTPASSWD_REALM=ichnos-lab \
  --env REGISTRY_AUTH_HTPASSWD_PATH=/tmp/htpasswd "$registry_image" >/dev/null
wait_for_registry "$registry" 5000 200
wait_for_registry "$locked_registry" 5002 401

scenario control "$alpine_3_22" -c one yes
scenario changed-instruction "$alpine_3_22" -l one yes
scenario changed-copy-file "$alpine_3_22" -c two yes
scenario changed-base-image "$alpine_3_21" -c one yes
scenario no-cache-import "$alpine_3_22" -c one no
scenario added-step "$alpine_3_22" -c one yes 'RUN echo ready > ready.txt'
scenario added-copy "$alpine_3_22" -c two yes '' 'COPY extra.txt .'

fixture "$work/changed-dockerignore/base" "$alpine_3_22" -c one '' 'COPY . all/'
fixture "$work/changed-dockerignore/pr" "$alpine_3_22" -c one '' 'COPY . all/'
printf '.dockerignore\nunused.txt\n' >"$work/changed-dockerignore/base/.dockerignore"
printf '.dockerignore\nextra.txt\n' >"$work/changed-dockerignore/pr/.dockerignore"
record_pair changed-dockerignore yes

from="FROM $alpine_3_22"

same_dockerfile multi-stage one two \
  "$from AS builder" \
  'WORKDIR /src' \
  'COPY app.txt .' \
  'RUN wc -c app.txt > size.txt' \
  'RUN cp app.txt copy.txt' \
  "$from" \
  'WORKDIR /src' \
  'COPY config.txt .' \
  'COPY --from=builder /src/size.txt .' \
  'COPY --from=builder /src/copy.txt .'
record_pair multi-stage yes

fixture "$work/removed-step/base" "$alpine_3_22" -c one 'RUN echo ready > ready.txt' ''
fixture "$work/removed-step/pr" "$alpine_3_22" -c one '' ''
record_pair removed-step yes

fixture "$work/reordered-steps/base" "$alpine_3_22" -c one '' ''
fixture_lines "$work/reordered-steps/pr" one \
  "$from" \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'COPY app.txt .' \
  'RUN wc -c config.txt > size.txt' \
  'RUN cat config.txt app.txt > bundle.txt'
record_pair reordered-steps yes

many_steps=("$from")
for i in 2 3 4 5 6 7 8 9 10 11 12; do
  many_steps+=("RUN echo $i > f$i")
done
fixture_lines "$work/many-steps/base" one "${many_steps[@]}"
many_steps[10]='RUN echo changed > f11'
fixture_lines "$work/many-steps/pr" one "${many_steps[@]}"
record_pair many-steps yes

scenario added-env "$alpine_3_22" -c one yes 'ENV MODE=ichnos-fake-mode'
scenario added-user "$alpine_3_22" -c one yes 'USER 0:0'

same_dockerfile changed-build-arg one one \
  "$from" \
  'ARG MODE' \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'RUN echo "$MODE" > mode.txt' \
  'COPY app.txt .' \
  'RUN cat config.txt app.txt > bundle.txt'
record changed-build-arg base --build-arg MODE=ichnos-fake-one "${export_cache[@]}"
record changed-build-arg pr --build-arg MODE=ichnos-fake-two "${import_cache[@]}"

same_dockerfile rotating-build-arg one one \
  "$from" \
  'ARG BUILD_ID' \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'RUN wc -c config.txt > size.txt' \
  'COPY app.txt .' \
  'RUN cat config.txt app.txt > bundle.txt'
record rotating-build-arg base --build-arg BUILD_ID=ichnos-fake-build-1 "${export_cache[@]}"
record rotating-build-arg pr --build-arg BUILD_ID=ichnos-fake-build-2 "${import_cache[@]}"

min_mode_lines=(
  "$from AS builder"
  'WORKDIR /src'
  'COPY app.txt .'
  'RUN wc -c app.txt > size.txt'
  "$from"
  'COPY config.txt .'
)
fixture_lines "$work/min-mode-needed/base" one "${min_mode_lines[@]}" 'COPY --from=builder /src/size.txt /size.txt'
fixture_lines "$work/min-mode-needed/pr" one "${min_mode_lines[@]}" 'COPY --from=builder /src/size.txt /renamed.txt'
record min-mode-needed base --cache-to type=local,dest=../cache,mode=min
record min-mode-needed pr "${import_cache[@]}"

fixture "$work/overwritten-cache/base" "$alpine_3_22" -c one '' ''
fixture "$work/overwritten-cache/other" "$alpine_3_22" -c other '' ''
fixture "$work/overwritten-cache/pr" "$alpine_3_22" -c one '' ''
record overwritten-cache base "${export_cache[@]}"
build overwritten-cache other "${export_cache[@]}"
record overwritten-cache pr "${import_cache[@]}"

repeated='RUN echo step >> log.txt'
fixture_lines "$work/identical-lines/base" one \
  "$from" \
  'WORKDIR /app' \
  "$repeated" \
  'COPY config.txt .' \
  "$repeated" \
  'COPY app.txt .' \
  "$repeated"
fixture_lines "$work/identical-lines/pr" one \
  "$from" \
  'WORKDIR /app' \
  "$repeated" \
  "$repeated" \
  'COPY config.txt .' \
  "$repeated" \
  'COPY app.txt .' \
  "$repeated"
record_pair identical-lines yes

fixture "$work/changed-next-to-added/base" "$alpine_3_22" -c one '' ''
fixture_lines "$work/changed-next-to-added/pr" one \
  "$from" \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'RUN echo ready > ready.txt' \
  'RUN wc -l config.txt > size.txt' \
  'COPY app.txt .' \
  'RUN cat config.txt app.txt > bundle.txt'
record_pair changed-next-to-added yes

same_dockerfile two-platforms one one \
  "$from" \
  'ARG TARGETARCH' \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'RUN wc -c config.txt > size.txt' \
  'COPY app-$TARGETARCH.txt app.txt' \
  'RUN cat config.txt app.txt > bundle.txt'
printf 'amd64\n' >"$work/two-platforms/base/app-amd64.txt"
printf 'amd64\n' >"$work/two-platforms/pr/app-amd64.txt"
printf 'arm64 one\n' >"$work/two-platforms/base/app-arm64.txt"
printf 'arm64 two\n' >"$work/two-platforms/pr/app-arm64.txt"
platform=linux/amd64,linux/arm64 record_pair two-platforms yes

fixture "$work/subdirectory-context/base/services/api" "$alpine_3_22" -c one '' ''
fixture "$work/subdirectory-context/pr/services/api" "$alpine_3_22" -c two '' ''
commit_fixture "$work/subdirectory-context/base"
commit_fixture "$work/subdirectory-context/pr"
context="$work/subdirectory-context/base/services/api" record subdirectory-context base "${export_cache[@]}"
context="$work/subdirectory-context/pr/services/api" record subdirectory-context pr "${import_cache[@]}"

same_dockerfile wildcard-copy one one \
  "$from" \
  'WORKDIR /app' \
  'COPY config.txt .' \
  'RUN wc -c config.txt > size.txt' \
  'COPY app-*.txt .' \
  'RUN cat config.txt app-*.txt > bundle.txt'
printf 'one\n' >"$work/wildcard-copy/base/app-one.txt"
printf 'one\n' >"$work/wildcard-copy/pr/app-one.txt"
printf 'one\n' >"$work/wildcard-copy/base/app-two.txt"
printf 'two\n' >"$work/wildcard-copy/pr/app-two.txt"
record_pair wildcard-copy yes

fixture "$work/two-cache-imports/base" "$alpine_3_22" -c one '' ''
fixture "$work/two-cache-imports/branch" "$alpine_3_22" -c two '' ''
fixture "$work/two-cache-imports/pr" "$alpine_3_22" -c two '' 'COPY extra.txt .'
record two-cache-imports base "${export_cache[@]}"
build two-cache-imports branch --cache-to type=local,dest=../branch-cache,mode=max
record two-cache-imports pr "${import_cache[@]}" --cache-from type=local,src=../branch-cache

link_lines=(
  "$from"
  'WORKDIR /app'
  'COPY --link config.txt .'
  'RUN wc -c config.txt > size.txt'
  'COPY --link app.txt .'
  'RUN cat config.txt app.txt > bundle.txt'
)
fixture_lines "$work/copy-link/base" one "${link_lines[@]}"
link_lines[3]='RUN wc -l config.txt > size.txt'
fixture_lines "$work/copy-link/pr" one "${link_lines[@]}"
record_pair copy-link yes

same_dockerfile remote-add one one \
  "$from" \
  'WORKDIR /app' \
  'ADD http://localhost:8080/tool.txt .' \
  'RUN wc -c tool.txt > size.txt' \
  'COPY app.txt .' \
  'RUN cat tool.txt app.txt > bundle.txt'
docker exec "$assets" sh -c 'mkdir -p /www && echo ichnos-fake-tool-one >/www/tool.txt'
record remote-add base "${export_cache[@]}"
docker exec "$assets" sh -c 'echo ichnos-fake-tool-two >/www/tool.txt'
record remote-add pr "${import_cache[@]}"

named_context_lines=(
  "$from"
  'WORKDIR /app'
  'COPY config.txt .'
  'RUN wc -c config.txt > size.txt'
  'COPY --from=assets shared.txt .'
  'RUN cat config.txt shared.txt > bundle.txt'
)
fixture_lines "$work/named-context/base/app" one "${named_context_lines[@]}"
fixture_lines "$work/named-context/pr/app" one "${named_context_lines[@]}"
mkdir "$work/named-context/base/assets" "$work/named-context/pr/assets"
printf 'one\n' >"$work/named-context/base/assets/shared.txt"
printf 'two\n' >"$work/named-context/pr/assets/shared.txt"
commit_fixture "$work/named-context/base"
commit_fixture "$work/named-context/pr"
context="$work/named-context/base/app" record named-context base \
  --build-context "assets=$work/named-context/base/assets" "${export_cache[@]}"
context="$work/named-context/pr/app" record named-context pr \
  --build-context "assets=$work/named-context/pr/assets" "${import_cache[@]}"

same_dockerfile no-cache-filter one one \
  "$from AS builder" \
  'WORKDIR /src' \
  'COPY app.txt .' \
  'RUN wc -c app.txt > size.txt' \
  "$from" \
  'WORKDIR /srv' \
  'COPY config.txt .' \
  'COPY --from=builder /src/size.txt .' \
  'RUN cat config.txt size.txt > bundle.txt'
record no-cache-filter base "${export_cache[@]}"
record no-cache-filter pr "${import_cache[@]}" --no-cache-filter builder

fixture "$work/registry-cache/base" "$alpine_3_22" -c one '' ''
fixture "$work/registry-cache/pr" "$alpine_3_22" -c two '' ''
record registry-cache base --cache-to "type=registry,ref=$open_cache/registry-cache:cache,mode=max"
record registry-cache pr --cache-from "type=registry,ref=$open_cache/registry-cache:cache"

fixture "$work/failed-cache-import/base" "$alpine_3_22" -c one '' ''
fixture "$work/failed-cache-import/pr" "$alpine_3_22" -c one '' ''
record failed-cache-import base --cache-to "type=registry,ref=$open_cache/failed-cache-import:cache,mode=max"
record failed-cache-import pr \
  --cache-from "type=registry,ref=$open_cache/failed-cache-import:missing" \
  --cache-from "type=registry,ref=$locked_cache/failed-cache-import:cache" \
  --cache-from "type=registry,ref=$unreachable_cache/failed-cache-import:cache"

fixture "$work/one-failed-cache-import/base" "$alpine_3_22" -c one '' ''
fixture "$work/one-failed-cache-import/pr" "$alpine_3_22" -c two '' ''
record one-failed-cache-import base --cache-to "type=registry,ref=$open_cache/one-failed-cache-import:cache,mode=max"
record one-failed-cache-import pr \
  --cache-from "type=registry,ref=$open_cache/one-failed-cache-import:cache" \
  --cache-from "type=registry,ref=$open_cache/one-failed-cache-import:missing"

# The web server holds the repository as static files. BuildKit fetches a commit
# without a shallow fetch, which such a server can answer, and a branch with
# one, which it cannot.
git_repo="$work/git-context-dockerignore/repo"
fixture "$git_repo" "$alpine_3_22" -c one '' 'COPY . all/'
printf '.dockerignore\nextra.txt\n' >"$git_repo/.dockerignore"
lab_git -C "$git_repo" init -q -b main
lab_git -C "$git_repo" add .
lab_git -C "$git_repo" commit -q -m base
printf 'extra two\n' >"$git_repo/extra.txt"
lab_git -C "$git_repo" commit -q -a -m pr
lab_git clone -q --bare "$git_repo" "$work/lab.git"
lab_git -C "$work/lab.git" update-server-info
docker cp --quiet "$work/lab.git" "$assets:/www/lab.git"
mkdir "$work/git-context-dockerignore/base" "$work/git-context-dockerignore/pr"
context="http://localhost:8080/lab.git#$(lab_git -C "$git_repo" rev-parse HEAD~1)" \
  record git-context-dockerignore base "${export_cache[@]}"
context="http://localhost:8080/lab.git#$(lab_git -C "$git_repo" rev-parse HEAD)" \
  record git-context-dockerignore pr "${import_cache[@]}"

same_dockerfile bind-mount one two \
  "$from" \
  'WORKDIR /app' \
  'RUN --mount=type=bind,source=config.txt,target=/mnt/config.txt wc -c /mnt/config.txt > size.txt' \
  'RUN --mount=type=bind,source=app.txt,target=/mnt/app.txt cat /mnt/app.txt > app-copy.txt' \
  'RUN cat size.txt app-copy.txt > bundle.txt'
record_pair bind-mount yes

same_dockerfile cache-mount-from one one \
  "$from" \
  'WORKDIR /app' \
  'RUN --mount=type=cache,from=seed,source=config,target=/mnt/config wc -c /mnt/config/config.txt > size.txt' \
  'RUN --mount=type=cache,from=seed,source=app,target=/mnt/app cat /mnt/app/app.txt > app-copy.txt' \
  'RUN cat size.txt app-copy.txt > bundle.txt'
for side in base pr; do
  mkdir -p "$work/cache-mount-from/$side/seed/config" "$work/cache-mount-from/$side/seed/app"
  printf 'level=info\n' >"$work/cache-mount-from/$side/seed/config/config.txt"
done
printf 'one\n' >"$work/cache-mount-from/base/seed/app/app.txt"
printf 'two\n' >"$work/cache-mount-from/pr/seed/app/app.txt"
record cache-mount-from base --build-context "seed=$work/cache-mount-from/base/seed" "${export_cache[@]}"
record cache-mount-from pr --build-context "seed=$work/cache-mount-from/pr/seed" "${import_cache[@]}"

replace_records "$@"
