#!/usr/bin/env bash
set -euo pipefail

buildkit_version=v0.34.0
buildkit_image="moby/buildkit:$buildkit_version@sha256:b059f8d7226d0b326bc871af489a5dddf65e36af4d20d14251b072974d9ee87c"
alpine_3_22='alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f'
alpine_3_21='alpine:3.21@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd'
builder=ichnos-lab

repo=$(cd "$(dirname "$0")/.." && pwd)
out="$repo/testdata/buildkit-$buildkit_version"
work=/tmp/ichnos-lab
records="$work/records"

mkdir "$work"
trap 'rm -rf "$work"' EXIT

fixture() {
  local dir=$1 image=$2 wc_flag=$3 app_text=$4

  mkdir -p "$dir"
  printf 'level=info\n' >"$dir/config.txt"
  printf '%s\n' "$app_text" >"$dir/app.txt"
  cat >"$dir/Dockerfile" <<EOF
FROM $image
WORKDIR /app
COPY config.txt .
RUN wc $wc_flag config.txt > size.txt
COPY app.txt .
RUN cat config.txt app.txt > bundle.txt
EOF
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

record() {
  local scenario=$1 side=$2
  shift 2
  local dir="$work/$scenario"
  local target="$records/$scenario/$side.dockerbuild"
  local ref

  docker buildx prune --builder "$builder" --all --force >/dev/null
  (cd "$dir/$side" && docker buildx build --builder "$builder" --platform linux/amd64 --progress=quiet \
    --metadata-file ../metadata.json --output type=oci,dest=../image.tar "$@" . >/dev/null)
  ref=$(jq -r '."buildx.build.ref"' "$dir/metadata.json")

  mkdir -p "$records/$scenario"
  docker buildx history export --builder "$builder" --finalize "${ref##*/}" -o "$target"
  check_record "$target"
}

scenario() {
  local name=$1 pr_image=$2 pr_wc_flag=$3 pr_app_text=$4 cache_import=$5

  fixture "$work/$name/base" "$alpine_3_22" -c one
  fixture "$work/$name/pr" "$pr_image" "$pr_wc_flag" "$pr_app_text"

  record "$name" base --cache-to type=local,dest=../cache,mode=max
  if [ "$cache_import" = yes ]; then
    record "$name" pr --cache-from type=local,src=../cache
  else
    record "$name" pr
  fi
  echo "$name: recorded"
}

docker buildx create --name "$builder" --driver docker-container \
  --driver-opt "image=$buildkit_image" unix:///var/run/docker.sock >/dev/null
trap 'docker buildx rm "$builder" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
docker buildx inspect --bootstrap "$builder" >/dev/null

scenario control "$alpine_3_22" -c one yes
scenario changed-instruction "$alpine_3_22" -l one yes
scenario changed-copy-file "$alpine_3_22" -c two yes
scenario changed-base-image "$alpine_3_21" -c one yes
scenario no-cache-import "$alpine_3_22" -c one no

rm -rf "$out"
mkdir -p "$(dirname "$out")"
mv "$records" "$out"
