#!/usr/bin/env bash
set -euo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

buildkit_version=v0.34.0
buildkit_image="moby/buildkit:$buildkit_version@sha256:b059f8d7226d0b326bc871af489a5dddf65e36af4d20d14251b072974d9ee87c"
alpine_3_22='alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f'
alpine_3_21='alpine:3.21@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd'
busybox_image='busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec'
builder=ichnos-lab
assets=ichnos-lab-assets
platform=linux/amd64
context=.
export_cache=(--cache-to type=local,dest=../cache,mode=max)
import_cache=(--cache-from type=local,src=../cache)

repo=$(cd "$(dirname "$0")/.." && pwd)
out="$repo/testdata/buildkit-$buildkit_version"
work=/tmp/ichnos-lab
records="$work/records"

mkdir "$work"
trap 'rm -rf "$work"' EXIT

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

# Build a committed fixture from an absolute path: buildx leaves out vcs:localdir:* for a
# relative path through a symlink, such as /tmp on macOS.
commit_fixture() {
  local dir=$1
  local -x GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

  git -C "$dir" init -q
  git -C "$dir" remote add origin https://example.invalid/ichnos-fake/lab.git
  git -C "$dir" add .
  git -C "$dir" -c user.name=ichnos-fake -c user.email=ichnos-fake@example.invalid commit -q -m fixture
}

check_record() {
  local record=$1 status=0

  gzip -dc "$record" >"$work/record.tar"
  LC_ALL=C grep -a -q -F -e "$(id -un)" -e "$(hostname -s)" -e /var/folders "$work/record.tar" || status=$?
  case $status in
    0)
      echo "$record contains a local user, host or temporary path" >&2
      exit 1
      ;;
    1) ;;
    *) exit "$status" ;;
  esac
}

build() {
  local scenario=$1 side=$2
  shift 2

  docker buildx prune --builder "$builder" --all --force >/dev/null
  (cd "$work/$scenario/$side" && docker buildx build --builder "$builder" --platform "$platform" --progress=quiet \
    --metadata-file ../metadata.json --output type=oci,dest=../image.tar "$@" "$context" >/dev/null)
}

record() {
  local scenario=$1 side=$2
  local target="$records/$scenario/$side.dockerbuild"
  local ref

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

docker buildx create --name "$builder" --driver docker-container \
  --driver-opt "image=$buildkit_image" unix:///var/run/docker.sock >/dev/null
trap 'docker rm --force "$assets" >/dev/null 2>&1 || true; docker buildx rm "$builder" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
docker buildx inspect --bootstrap "$builder" >/dev/null
docker run --detach --rm --name "$assets" --network "container:buildx_buildkit_${builder}0" "$busybox_image" \
  sh -c 'mkdir -p /www && exec httpd -f -p 8080 -h /www' >/dev/null

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

rm -rf "$out"
mkdir -p "$(dirname "$out")"
mv "$records" "$out"
