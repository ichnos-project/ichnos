#!/usr/bin/env bash
set -euo pipefail

script=lab/record.sh
control=testdata/buildkit-v0.34.0/control/pr.dockerbuild
otlp=application/vnd.buildkit.otlp.json.v0
manifest_hex=$(printf 'a%.0s' {1..64})
trace_hex=$(printf 'b%.0s' {1..64})
n=0

fail() {
  echo "$1" >&2
  exit 1
}

# expect_pass runs a command in a subshell and fails if it stops.
expect_pass() {
  ("$@") >"$top/stdout" 2>"$top/stderr" || fail "stopped: $*: $(cat "$top/stderr")"
  n=$((n + 1))
}

# expect_stop MESSAGE COMMAND... runs a command in a subshell and fails unless
# it stops and, when MESSAGE is not empty, prints MESSAGE.
expect_stop() {
  local message=$1 status=0
  shift
  ("$@") >"$top/stdout" 2>"$top/stderr" || status=$?
  [ "$status" -ne 0 ] || fail "did not stop: $*"
  if [ -n "$message" ] && ! grep -q -F -- "$message" "$top/stderr"; then
    fail "stopped for another reason: $*: $(cat "$top/stderr")"
  fi
  n=$((n + 1))
}

# expect_equal NAME GOT WANT fails unless GOT is WANT.
expect_equal() {
  [ "$2" = "$3" ] || fail "$1 = $(printf '%q' "$2"), want $(printf '%q' "$3")"
  n=$((n + 1))
}

# craft OUT TYPE TRACE writes a record whose manifest has one layer, of media
# type TYPE, holding TRACE.
craft() {
  local out=$1 type=$2 trace=$3 dir
  dir=$(mktemp -d "$top/craft.XXXXXX")
  mkdir -p "$dir/blobs/sha256"
  printf '%s\n' "$trace" >"$dir/blobs/sha256/$trace_hex"
  printf '{"layers":[{"mediaType":"%s","digest":"sha256:%s"}]}\n' "$type" "$trace_hex" >"$dir/blobs/sha256/$manifest_hex"
  printf '{"manifests":[{"digest":"sha256:%s"}]}\n' "$manifest_hex" >"$dir/index.json"
  (cd "$dir" && COPYFILE_DISABLE=1 tar -cf - index.json blobs) | gzip >"$out"
}

# span KEY ADDRESS prints a trace span with one event that has the address
# attribute KEY.
span() {
  printf '{"Name":"HTTP GET","Events":[{"Attributes":[{"Key":"%s","Value":{"Type":"STRING","Value":"%s"}}]}]}' "$1" "$2"
}

# The cases below run in a subshell, through expect_pass or expect_stop, so the
# stubs that they define for what record.sh calls end with them.
private_values_found() {
  host_addresses() { printf '%s\n' 192.0.2.10 2001:db8::10; }
  private_values
}

private_values_none() {
  host_addresses() { :; }
  private_values
}

builder_left() {
  docker() { [ "$*" = "container inspect buildx_buildkit_ichnos-lab0" ]; }
  check_no_builder
}

no_builder() {
  docker() { return 1; }
  check_no_builder
}

builder_address() {
  docker() {
    echo "$*" >"$top/docker-args"
    echo 172.17.0.2
  }
  allowed_addresses
}

# registry_answers_third answers with the status on the third request.
registry_answers_third() {
  sleep() { :; }
  docker() {
    local tries
    tries=$(($(cat "$top/tries") + 1))
    echo "$tries" >"$top/tries"
    echo "$*" >"$top/docker-args"
    if [ "$tries" -ge 3 ]; then
      echo "  HTTP/1.1 401 Unauthorized"
    else
      echo "wget: can't connect to remote host"
    fi
  }
  wait_for_registry ichnos-lab-locked-registry 5002 401
}

registry_answers_otherwise() {
  sleep() { :; }
  docker() { echo "  HTTP/1.1 200 OK"; }
  wait_for_registry ichnos-lab-locked-registry 5002 401
}

top=$(mktemp -d "${TMPDIR:-/tmp}/record-test.XXXXXX")
trap 'rm -rf "$top"' EXIT
unset GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT

# shellcheck source=lab/record.sh
. "$script"
work="$top/work"
records="$top/records"
out="$top/out"
mkdir "$work"

# The scenario list names each recorded scenario once.
listed=$(printf '%s\n' "${scenarios[@]}" | LC_ALL=C sort)
recorded=$(cd "testdata/buildkit-$buildkit_version" && for d in */; do echo "${d%/}"; done | LC_ALL=C sort)
expect_equal "scenario list" "$listed" "$recorded"
expect_equal "repeated scenarios" "$(printf '%s\n' "${scenarios[@]}" | LC_ALL=C sort | uniq -d)" ""

# Scenario names.
expect_pass known bind-mount
expect_stop "" known bind
selection=
select_scenarios
expect_equal "selection with no names" "$selection" all
select_scenarios bind-mount cache-mount-from
expect_equal "selection with names" "$selection" " bind-mount cache-mount-from "
for name in nope '' .. ../testdata Control bind-mount/ 'bind-mount cache-mount-from' '-rf' "$(printf 'bind-mount\ncontrol')"; do
  expect_stop "$name: no such scenario" select_scenarios control "$name"
done
selection=all
expect_pass selected control
selection=" one-failed-cache-import bind-mount "
expect_pass selected one-failed-cache-import
expect_pass selected bind-mount
expect_stop "" selected failed-cache-import
expect_stop "" selected control
expect_stop "nope is missing from the list of scenarios" selected nope

# The environment: no git or docker setting of the user's reaches a command,
# and BuildKit fetches registry tokens itself.
env_after=$(
  export GIT_DIR=x GIT_WORK_TREE=x GIT_INDEX_FILE=x GIT_CONFIG_PARAMETERS="'commit.gpgsign'='true'" GIT_CONFIG_COUNT=1
  export BUILDX_CONFIG=x DOCKER_CONTEXT=x DOCKER_HOST=tcp://example.invalid:2375
  unset BUILDKIT_NO_CLIENT_TOKEN
  isolate_environment
  env
)
for var in GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT BUILDX_CONFIG DOCKER_CONTEXT DOCKER_HOST; do
  if printf '%s\n' "$env_after" | grep -q "^$var="; then
    fail "isolate_environment left $var"
  fi
  n=$((n + 1))
done
printf '%s\n' "$env_after" | grep -q -x 'BUILDKIT_NO_CLIENT_TOKEN=true' || fail "isolate_environment did not export BUILDKIT_NO_CLIENT_TOKEN=true"
n=$((n + 1))

# The lab's Docker configuration keeps only where the CLI plugins are, and an
# auths entry for a host that cannot exist, so the CLI asks no credential
# store.
mkdir -p "$top/user/cli-plugins"
cat >"$top/user/config.json" <<'EOF'
{
  "auths": {"https://index.docker.io/v1/": {"auth": "cGxhbnRlZDpwbGFudGVk"}},
  "credsStore": "desktop",
  "credHelpers": {"gcr.io": "gcloud"},
  "proxies": {"default": {"httpProxy": "http://proxy.example.invalid"}},
  "currentContext": "planted",
  "cliPluginsExtraDirs": ["/opt/planted-plugins"]
}
EOF
expect_pass docker_config "$top/user" "$top/lab-docker"
expect_equal "lab configuration" "$(jq -S -c . "$top/lab-docker/config.json")" \
  '{"auths":{"ichnos-lab.invalid":{}},"cliPluginsExtraDirs":["/opt/planted-plugins"]}'
expect_equal "plugin link" "$(readlink "$top/lab-docker/cli-plugins")" "$top/user/cli-plugins"
expect_pass docker_config "$top/no-user" "$top/lab-docker-2"
expect_equal "lab configuration with none of the user's" "$(jq -S -c . "$top/lab-docker-2/config.json")" \
  '{"auths":{"ichnos-lab.invalid":{}}}'

# Addresses in the output of ip addr and ifconfig, without loopback, prefixes
# or zones.
cat >"$top/ip.txt" <<'EOF'
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP group default qlen 1000
    link/ether 02:00:00:00:00:01 brd ff:ff:ff:ff:ff:ff
    inet 198.51.100.7/24 brd 198.51.100.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 2001:db8::7/64 scope global
       valid_lft forever preferred_lft forever
    inet6 fe80::ff:fe00:1/64 scope link
       valid_lft forever preferred_lft forever
EOF
cat >"$top/ifconfig.txt" <<'EOF'
lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	inet 127.0.0.1 netmask 0xff000000
	inet6 ::1 prefixlen 128
	inet6 fe80::1%lo0 prefixlen 64 scopeid 0x1
en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet6 fe80::1c2b:3d4e:5f60:7a8b%en0 prefixlen 64 secured scopeid 0xb
	inet 192.0.2.10 netmask 0xffffff00 broadcast 192.0.2.255
	inet6 2001:db8:1:2:3:4:5:6 prefixlen 64 autoconf secured
EOF
ip_addresses=$'198.51.100.7\n2001:db8::7\nfe80::ff:fe00:1'
ifconfig_addresses=$'fe80::1\nfe80::1c2b:3d4e:5f60:7a8b\n192.0.2.10\n2001:db8:1:2:3:4:5:6'
expect_equal "addresses from ip" "$(parse_addresses <"$top/ip.txt")" "$ip_addresses"
expect_equal "addresses from ifconfig" "$(parse_addresses <"$top/ifconfig.txt")" "$ifconfig_addresses"
expect_equal "addresses from loopback" "$(head -6 "$top/ip.txt" | parse_addresses)" ""

# host_addresses reads ip where there is one, and ifconfig otherwise.
for tools in ip ifconfig; do
  mkdir "$top/bin-$tools"
  for tool in awk sed grep cat; do
    ln -s "$(command -v "$tool")" "$top/bin-$tools/$tool"
  done
  printf '#!/bin/sh\ncat "%s"\n' "$top/$tools.txt" >"$top/bin-$tools/$tools"
  chmod +x "$top/bin-$tools/$tools"
done
expect_equal "host addresses with ip" "$(PATH="$top/bin-ip" host_addresses)" "$ip_addresses"
expect_equal "host addresses with ifconfig" "$(PATH="$top/bin-ifconfig" host_addresses)" "$ifconfig_addresses"

# private_values lists the addresses, user, host and temporary path, and
# stops when it finds no address.
expect_pass private_values_found
expect_equal "private values" "$(cat "$work/private.txt")" \
  "$(printf '%s\n' 192.0.2.10 2001:db8::10 "$(id -un)" "$(hostname -s)" /var/folders)"
expect_stop "found no network address" private_values_none

# A builder container left from an earlier run stops the run.
expect_stop "buildx_buildkit_ichnos-lab0 is left from an earlier run" builder_left
expect_pass no_builder

# Connections may start from loopback and the builder's address.
expect_pass builder_address
expect_equal "allowed addresses" "$(cat "$work/allowed.txt")" $'127.0.0.1\n::1\n172.17.0.2'
case $(cat "$top/docker-args") in
  *buildx_buildkit_ichnos-lab0*) n=$((n + 1)) ;;
  *) fail "allowed_addresses inspected another container: $(cat "$top/docker-args")" ;;
esac

# wait_for_registry asks the registry on loopback, with a timeout, until it
# answers with the status, and stops after 30 tries.
echo 0 >"$top/tries"
expect_pass registry_answers_third
expect_equal "tries until the registry answered" "$(cat "$top/tries")" 3
expect_equal "readiness request" "$(cat "$top/docker-args")" \
  "exec ichnos-lab-locked-registry wget -S -T 2 -O /dev/null http://127.0.0.1:5002/v2/"
expect_stop "ichnos-lab-locked-registry did not answer with 401 after 30 tries" registry_answers_otherwise

# check_record passes records whose connections start from the builder or
# loopback, and stops on a private value, an address read again for each
# record, a connection from any other address, and a record with no trace.
printf '%s\n' 192.0.2.10 ichnos-test-user >"$work/private.txt"
printf '%s\n' 127.0.0.1 ::1 172.17.0.2 >"$work/allowed.txt"
host_addresses() { echo 203.0.113.5; }
expect_pass check_record "$control"
craft "$top/clean.dockerbuild" "$otlp" \
  "$(span http.local 172.17.0.2:41578)$(span http.local 127.0.0.1:5000)$(span http.local '[::1]:5000')"
expect_pass check_record "$top/clean.dockerbuild"
for key in http.local network.local.address net.sock.host.addr; do
  craft "$top/$key.dockerbuild" "$otlp" "$(span http.local 172.17.0.2:41578)$(span "$key" 198.51.100.99:41000)"
  expect_stop "has a connection from an address other than the builder's or loopback" check_record "$top/$key.dockerbuild"
done
craft "$top/ipv6.dockerbuild" "$otlp" "$(span http.local '[2001:db8::99]:443')"
expect_stop "has a connection from an address other than the builder's or loopback" check_record "$top/ipv6.dockerbuild"
for value in 192.0.2.10 ichnos-test-user 203.0.113.5; do
  craft "$top/private.dockerbuild" "$otlp" "$(span http.local 172.17.0.2:41578){\"Name\":\"$value\"}"
  expect_stop "contains a local user, host, network address or temporary path" check_record "$top/private.dockerbuild"
done
craft "$top/no-trace.dockerbuild" application/vnd.buildkit.status.v0 "$(span http.local 198.51.100.99:41000)"
expect_stop "" check_record "$top/no-trace.dockerbuild"

# replace_records replaces only the named scenarios' records, replaces nothing
# when a named scenario recorded nothing, and replaces every record when no
# scenario is named.
reset_records() {
  rm -rf "$out" "$records"
  mkdir -p "$out/a" "$out/b" "$records/b" "$records/z"
  echo old >"$out/a/base.dockerbuild"
  echo old >"$out/b/base.dockerbuild"
  echo new >"$records/b/base.dockerbuild"
  echo new >"$records/z/base.dockerbuild"
}
reset_records
selection=" b b "
expect_pass replace_records b b
expect_equal "unnamed record" "$(cat "$out/a/base.dockerbuild")" old
expect_equal "named record" "$(cat "$out/b/base.dockerbuild")" new
expect_equal "unnamed scenario in the run" "$([ -e "$out/z" ] && echo replaced || echo kept)" kept
reset_records
selection=" b c "
expect_stop "c: recorded nothing" replace_records b c
expect_equal "record after a failed replacement" "$(cat "$out/b/base.dockerbuild")" old
reset_records
selection=all
expect_pass replace_records
expect_equal "records after a full run" "$(cd "$out" && echo *)" "b z"

# lab_git and commit_fixture make the same commit in every recording, with a
# fake identity, a fixed date and a fake remote.
for dir in git-a git-b; do
  mkdir "$top/$dir"
  echo same >"$top/$dir/file"
  (
    lab_git -C "$top/$dir" init -q -b main
    lab_git -C "$top/$dir" add .
    lab_git -C "$top/$dir" commit -q -m fixture
  )
  mkdir "$top/fixture-$dir"
  echo same >"$top/fixture-$dir/file"
  (commit_fixture "$top/fixture-$dir")
done
expect_equal "repeated commit" "$(git -C "$top/git-a" rev-parse HEAD)" "$(git -C "$top/git-b" rev-parse HEAD)"
expect_equal "commit identity and date" "$(git -C "$top/git-a" log -1 --format='%an %ae %at %ct')" \
  "ichnos-fake ichnos-fake@example.invalid 1767225600 1767225600"
expect_equal "repeated fixture commit" "$(git -C "$top/fixture-git-a" rev-parse HEAD)" "$(git -C "$top/fixture-git-b" rev-parse HEAD)"
expect_equal "fixture remote" "$(git -C "$top/fixture-git-a" remote get-url origin)" https://example.invalid/ichnos-fake/lab.git

echo "record: all $n checks pass"
