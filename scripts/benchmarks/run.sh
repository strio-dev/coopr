#!/usr/bin/env bash
# Measure equivalent native-platform Coopr and Podman builds on this host.
# This is an end-to-end CLI benchmark, not an image-byte identity test.
set -euo pipefail

script_path=$(realpath "${BASH_SOURCE[0]}")
repo_root=$(cd "$(dirname "$script_path")/../.." && pwd)
iterations=${1:-5}
multiplatform=${2:-0}

fail() {
  printf 'parity benchmark: %s\n' "$*" >&2
  exit 1
}

[[ $# -le 2 ]] || fail 'usage: run.sh [iterations] [multiplatform]'
[[ $iterations =~ ^[1-9][0-9]*$ ]] || fail 'iterations must be positive'
[[ $multiplatform == 0 || $multiplatform == 1 ]] || \
  fail 'multiplatform must be 0 or 1'
[[ $(uname -s) == Linux ]] || fail 'Linux is required'
[[ $(id -u) != 0 ]] || fail 'run as a non-root user'
for tool in busybox date go podman; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing $tool"
done
[[ $(date +%s%N) =~ ^[0-9]{15,}$ ]] || \
  fail 'date must support numeric +%s%N timestamps'
[[ $(podman info --format '{{.Host.Security.Rootless}}') == true ]] || fail 'rootless Podman is required'
storage_driver=$(podman info --format '{{.Store.GraphDriverName}}')
[[ $storage_driver =~ ^[a-z0-9_-]+$ ]] || fail 'invalid native storage driver'

case $(uname -m) in
  x86_64)
    platform=linux/amd64
    counterpart_binfmt=qemu-aarch64
    ;;
  aarch64)
    platform=linux/arm64
    counterpart_binfmt=qemu-x86_64
    ;;
  *) fail "unsupported benchmark host architecture: $(uname -m)" ;;
esac
if [[ $multiplatform == 1 ]]; then
  binfmt_path="/proc/sys/fs/binfmt_misc/$counterpart_binfmt"
  [[ -r $binfmt_path ]] || fail "multi-platform benchmark requires $counterpart_binfmt binfmt"
  [[ $(sed -n '1p' "$binfmt_path") == enabled ]] || \
    fail "multi-platform benchmark requires enabled $counterpart_binfmt binfmt"
  grep -Eq '^flags:.*F' "$binfmt_path" || \
    fail "multi-platform benchmark requires $counterpart_binfmt binfmt with the F flag"
  command -v jq >/dev/null 2>&1 || fail 'multi-platform benchmark requires jq'
fi

bench_parent=$(mktemp -d "${TMPDIR:-/tmp}/coopr-parity-benchmark.XXXXXX")
bench_root="$bench_parent/data"
mkdir "$bench_root"
# A fresh operation-owned config prevents inherited writable image stores and
# driver options from escaping scratch. Keep the host's driver for both builders;
# overlay retains upstream native/fuse-overlayfs capability selection.
unset CONTAINERS_STORAGE_CONF_OVERRIDE STORAGE_OPTS STORAGE_DRIVER
write_storage_config() {
  local prefix=$1 driver=${2:-$storage_driver}
  # Escape path characters for TOML basic strings; argv stays literal below.
  local escaped=${prefix//\\/\\\\}
  escaped=${escaped//\"/\\\"}
  escaped=${escaped//$'\n'/\\n}
  escaped=${escaped//$'\r'/\\r}
  escaped=${escaped//$'\t'/\\t}
  escaped=${escaped//$'\b'/\\b}
  escaped=${escaped//$'\f'/\\f}
  cat >"$prefix.conf" <<EOF
[storage]
driver = "$driver"
graphroot = "$escaped-graph"
runroot = "$escaped-run"
imagestore = ""
[storage.options]
additionalimagestores = []
additionallayerstores = []
EOF
}
export CONTAINERS_STORAGE_CONF="$bench_parent/control.conf"
in_coopr_store() {
  local iteration=$1
  shift
  env "CONTAINERS_STORAGE_CONF=$bench_root/coopr-$iteration.conf" \
    "XDG_DATA_HOME=$bench_root/coopr-data-$iteration" "$@"
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  # The only asynchronous jobs are measure_concurrent's two native commands.
  # Bash job IDs avoid signaling an already-reaped PID that another process reused.
  kill -TERM %1 %2 2>/dev/null || true
  wait
  # Podman opens storage before unshare and shuts it down after the child.
  # Keep its empty VFS control store outside the data being removed.
  if ! env CONTAINERS_STORAGE_CONF="$bench_parent/control.conf" \
    podman --root "$bench_parent/control-graph" --runroot "$bench_parent/control-run" \
      --storage-driver vfs unshare rm -rf -- "$bench_root"; then
    printf 'benchmark cleanup failed; retained scratch: %s\n' "$bench_parent" >&2
    ((status != 0)) || status=1
  elif ! rm -rf -- "$bench_parent"; then
    printf 'benchmark control cleanup failed; retained scratch: %s\n' "$bench_parent" >&2
    ((status != 0)) || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
write_storage_config "$bench_parent/control" vfs

(cd "$repo_root" && GOMAXPROCS=2 go build -p=1 -o "$bench_root/coopr" ./cmd/coopr)
if [[ $multiplatform == 1 ]]; then
  proof_context="$bench_root/multi-platform-proof"
  mkdir -p "$proof_context"
  for arch in amd64 arm64; do
    GOOS=linux GOARCH=$arch CGO_ENABLED=0 GOMAXPROCS=2 \
      go build -p=1 -trimpath -ldflags='-s -w' -o "$proof_context/proof-$arch" \
        "$repo_root/scripts/benchmarks/fixtures/platform-proof/main.go"
  done
fi
printf 'builder,iteration,phase,seconds\n'

# ShellCheck does not follow assignments through the caller's nameref.
# shellcheck disable=SC2034
build_command() {
  local builder=$1 iteration=$2 phase=$3 definition=$4 context=$5 tag=$6
  local -n result=$7
  local -a build_args=()
  if [[ $phase == changed-final-run ]]; then
    build_args=(--build-arg suffix=changed)
  fi
  if [[ $builder == coopr ]]; then
    result=(env "CONTAINERS_STORAGE_CONF=$bench_root/coopr-$iteration.conf"
      "XDG_DATA_HOME=$bench_root/coopr-data-$iteration" "$bench_root/coopr" build
      --file "$definition" --tag "$tag" "${build_args[@]}")
    if [[ $phase == multi-platform-* ]]; then
      result+=(--platform linux/amd64 --platform linux/arm64)
    else
      result+=(--platform "$platform")
    fi
  else
    result=(podman build --layers --pull=never --network=none --format oci
      --file "$definition" "${build_args[@]}")
    if [[ $phase == multi-platform-* ]]; then
      result+=(--platform "linux/amd64,linux/arm64" --jobs=2 --manifest "$tag")
    else
      result+=(--platform "$platform" --tag "$tag")
    fi
  fi
  result+=("$context")
}

measure() {
  local builder=$1 iteration=$2 phase=$3 definition=$4 context=$5 tag=$6
  local start end log="$bench_root/$builder-$iteration-$phase.log"
  local -a command
  build_command "$builder" "$iteration" "$phase" "$definition" "$context" "$tag" command
  start=$(date +%s%N)
  if ! "${command[@]}" >"$log" 2>&1; then
    cat "$log" >&2
    fail "$builder iteration $iteration phase $phase failed"
  fi
  end=$(date +%s%N)
  awk -v builder="$builder" -v iteration="$iteration" -v phase="$phase" \
    -v start="$start" -v end="$end" \
    'BEGIN { printf "%s,%s,%s,%.6f\n", builder, iteration, phase, (end-start)/1000000000 }'
}

measure_concurrent() {
  local builder=$1 iteration=$2
  local definition_a=$3 context_a=$4 tag_a=$5
  local definition_b=$6 context_b=$7 tag_b=$8
  local phase=concurrent-two-builds
  local start end pid_a pid_b status_a=0 status_b=0
  local log_a="$bench_root/$builder-$iteration-$phase-a.log"
  local log_b="$bench_root/$builder-$iteration-$phase-b.log"
  local -a command_a command_b
  build_command "$builder" "$iteration" "$phase" "$definition_a" "$context_a" "$tag_a" command_a
  build_command "$builder" "$iteration" "$phase" "$definition_b" "$context_b" "$tag_b" command_b

  start=$(date +%s%N)
  "${command_a[@]}" >"$log_a" 2>&1 &
  pid_a=$!
  "${command_b[@]}" >"$log_b" 2>&1 &
  pid_b=$!
  wait "$pid_a" || status_a=$?
  wait "$pid_b" || status_b=$?
  end=$(date +%s%N)
  if ((status_a != 0 || status_b != 0)); then
    ((status_a == 0)) || cat "$log_a" >&2
    ((status_b == 0)) || cat "$log_b" >&2
    fail "$builder iteration $iteration phase $phase failed (a=$status_a b=$status_b)"
  fi
  awk -v builder="$builder" -v iteration="$iteration" -v phase="$phase" \
    -v start="$start" -v end="$end" \
    'BEGIN { printf "%s,%s,%s,%.6f\n", builder, iteration, phase, (end-start)/1000000000 }'
}

write_fixture() {
  local work=$1 marker=$2 payload=$3 sleep_seconds=$4
  local container_sleep='' coopr_sleep=''
  mkdir -p "$work"
  cp "$(command -v busybox)" "$work/busybox"
  chmod 0755 "$work/busybox"
  printf '%s\n' "$payload" >"$work/payload"
  if ((sleep_seconds > 0)); then
    container_sleep="RUN --network=none /bin/busybox sleep $sleep_seconds"
    coopr_sleep="run \"/bin/busybox sleep $sleep_seconds\" network=\"none\""
  fi
  cat >"$work/Containerfile" <<EOF
FROM scratch
LABEL dev.strio.coopr.benchmark=$marker
COPY busybox /bin/sh
COPY busybox /bin/cp
COPY busybox /bin/busybox
COPY payload /payload
$container_sleep
RUN --network=none cp /payload /one
ARG suffix=base
RUN --network=none cp /one /result && printf '%s' "\$suffix" >>/result
EOF
  cat >"$work/image.coopr" <<EOF
from "scratch" {
  label "dev.strio.coopr.benchmark"="$marker"
  copy "busybox" "/bin/sh"
  copy "busybox" "/bin/cp"
  copy "busybox" "/bin/busybox"
  copy "payload" "/payload"
  $coopr_sleep
  run "cp /payload /one" network="none"
  arg "suffix" "base"
  run "cp /one /result && printf '%s' \"\$suffix\" >>/result" network="none"
}
EOF
}

write_multi_platform_fixture() {
  local work=$1 marker=$2
  mkdir -p "$work"
  cp "$proof_context/proof-amd64" "$proof_context/proof-arm64" "$work/"
  printf '%s' "$marker" >"$work/marker"
  cat >"$work/Containerfile" <<EOF
FROM scratch
ARG TARGETARCH
LABEL dev.strio.coopr.benchmark=$marker
COPY proof-\${TARGETARCH} /proof
COPY marker /marker
RUN ["/proof", "/marker", "/build-proof"]
CMD ["/proof", "/build-proof"]
EOF
  cat >"$work/image.coopr" <<EOF
from "scratch" {
  arg "TARGETARCH"
  label "dev.strio.coopr.benchmark"="$marker"
  copy "proof-\$TARGETARCH" "/proof" chmod="0755"
  copy "marker" "/marker"
  run network="none" {
      exec "/proof" "/marker" "/build-proof"
  }
  cmd {
      exec "/proof" "/build-proof"
  }
}
EOF
}

verify_image() {
  local builder=$1 iteration=$2 name=$3 tag=$4 expected=$5
  local actual
  if [[ $builder == coopr ]]; then
    actual=$(in_coopr_store "$iteration" podman run --rm --network=none \
      --entrypoint /bin/busybox "$tag" cat /result)
  else
    actual=$(podman run --rm --network=none --entrypoint /bin/busybox \
      "$tag" cat /result)
  fi
  [[ $actual == "$expected" ]] || \
    fail "$builder iteration $iteration $name result differs: $actual"
}

verify_multi_platform_image() {
  local builder=$1 iteration=$2 tag=$3 marker=$4
  local arch actual actual_platform runtime_tag manifest_json runtime_archive
  if [[ $builder == coopr ]]; then
    for arch in amd64 arm64; do
      runtime_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-multi-$arch-runtime:latest"
      runtime_archive="$bench_root/coopr-$iteration-multi-$arch.oci.tar"
      in_coopr_store "$iteration" \
        "$bench_root/coopr" push "$tag" "oci-archive:$runtime_archive:$runtime_tag" --platform "linux/$arch" \
        >"$bench_root/coopr-$iteration-multi-$arch-push.log" 2>&1 || \
        fail "could not export iteration $iteration Coopr linux/$arch native image"
      in_coopr_store "$iteration" podman load --input "$runtime_archive" \
        >"$bench_root/coopr-$iteration-multi-$arch-load.log" 2>&1 || \
        fail "could not load iteration $iteration Coopr linux/$arch native image"
      actual_platform=$(in_coopr_store "$iteration" \
        podman image inspect --format '{{.Os}}/{{.Architecture}}' "$runtime_tag")
      [[ $actual_platform == "linux/$arch" ]] || \
        fail "Coopr iteration $iteration child is $actual_platform, expected linux/$arch"
      actual=$(in_coopr_store "$iteration" podman run --rm --network=none \
        --platform "linux/$arch" "$runtime_tag")
      [[ $actual == "$arch:$marker" ]] || \
        fail "Coopr iteration $iteration linux/$arch proof differs: $actual"
    done
  else
    manifest_json=$(podman manifest inspect "$tag")
    for arch in amd64 arm64; do
      [[ $(jq --arg arch "$arch" \
        '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)] | length' \
        <<<"$manifest_json") == 1 ]] || \
        fail "Podman iteration $iteration index lacks one linux/$arch child"
      actual=$(podman run --rm --network=none --platform "linux/$arch" "$tag")
      [[ $actual == "$arch:$marker" ]] || \
        fail "Podman iteration $iteration linux/$arch proof differs: $actual"
    done
  fi
}

for ((iteration=1; iteration<=iterations; iteration++)); do
  write_storage_config "$bench_root/coopr-$iteration"
  write_storage_config "$bench_root/podman-$iteration"
  export CONTAINERS_STORAGE_CONF="$bench_root/podman-$iteration.conf"
  work="$bench_root/work-$iteration"
  benchmark_marker="${bench_parent##*/}-$iteration"
  write_fixture "$work" "$benchmark_marker" "payload for iteration $iteration" 0

  concurrent_a="$bench_root/concurrent-$iteration-a"
  concurrent_b="$bench_root/concurrent-$iteration-b"
  write_fixture "$concurrent_a" "$benchmark_marker-concurrent-a" \
    "concurrent payload a for iteration $iteration" 2
  write_fixture "$concurrent_b" "$benchmark_marker-concurrent-b" \
    "concurrent payload b for iteration $iteration" 2
  if [[ $multiplatform == 1 ]]; then
    multi_work="$bench_root/multi-$iteration"
    multi_marker="$benchmark_marker-multi"
    write_multi_platform_fixture "$multi_work" "$multi_marker"
  fi

  for builder in coopr podman; do
    tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-$builder:latest"
    for phase in cold warm changed-final-run; do
      if [[ $builder == coopr ]]; then
        measure "$builder" "$iteration" "$phase" "$work/image.coopr" "$work" "$tag"
      else
        measure "$builder" "$iteration" "$phase" "$work/Containerfile" "$work" "$tag"
      fi
    done
    verify_image "$builder" "$iteration" sequential "$tag" \
      "$(printf 'payload for iteration %d\nchanged' "$iteration")"

    concurrent_tag_a="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-$builder-concurrent-a:latest"
    concurrent_tag_b="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-$builder-concurrent-b:latest"
    if [[ $builder == podman ]]; then
      measure_concurrent "$builder" "$iteration" \
        "$concurrent_a/Containerfile" "$concurrent_a" "$concurrent_tag_a" \
        "$concurrent_b/Containerfile" "$concurrent_b" "$concurrent_tag_b"
    else
      measure_concurrent "$builder" "$iteration" \
        "$concurrent_a/image.coopr" "$concurrent_a" "$concurrent_tag_a" \
        "$concurrent_b/image.coopr" "$concurrent_b" "$concurrent_tag_b"
    fi
    verify_image "$builder" "$iteration" concurrent-a "$concurrent_tag_a" \
      "$(printf 'concurrent payload a for iteration %d\nbase' "$iteration")"
    verify_image "$builder" "$iteration" concurrent-b "$concurrent_tag_b" \
      "$(printf 'concurrent payload b for iteration %d\nbase' "$iteration")"

    if [[ $builder == coopr ]]; then
      component_work="$bench_root/component-$iteration"
      mkdir -p "$component_work"
      cp "$(command -v busybox)" "$component_work/busybox"
      printf 'component payload %s\n' "$iteration" >"$component_work/payload"
      cat >"$component_work/component.coopr" <<'EOF'
package as="payload" {
  copy "payload" "/payload"
}
extend {
  copy "/payload" "/payload" from="payload"
  run "cp /payload /result" network="none"
}
EOF
      cat >"$component_work/image.coopr" <<EOF
from "scratch" {
  label "dev.strio.coopr.benchmark"="$benchmark_marker-component"
  copy "busybox" "/bin/sh"
  copy "busybox" "/bin/cp"
  copy "busybox" "/bin/busybox"
  component "benchmark-component"
}
EOF
      in_coopr_store "$iteration" "$bench_root/coopr" component build "$component_work/component.coopr" \
        --tag benchmark-component --platform "$platform" \
        >"$bench_root/component-$iteration-publish.log" 2>&1 || { cat "$bench_root/component-$iteration-publish.log" >&2; fail "component publication failed"; }
      component_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-component:latest"
      measure coopr "$iteration" component-cold "$component_work/image.coopr" "$component_work" "$component_tag"
      measure coopr "$iteration" component-warm "$component_work/image.coopr" "$component_work" "$component_tag"
      verify_image coopr "$iteration" component "$component_tag" "component payload $iteration"
    fi

    if [[ $multiplatform == 1 ]]; then
      multi_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-$builder-multi:latest"
      if [[ $builder == coopr ]]; then
        multi_definition="$multi_work/image.coopr"
      else
        multi_definition="$multi_work/Containerfile"
      fi
      measure "$builder" "$iteration" multi-platform-cold \
        "$multi_definition" "$multi_work" "$multi_tag"
      measure "$builder" "$iteration" multi-platform-warm \
        "$multi_definition" "$multi_work" "$multi_tag"
      verify_multi_platform_image "$builder" "$iteration" "$multi_tag" "$multi_marker"
    fi
  done
done

