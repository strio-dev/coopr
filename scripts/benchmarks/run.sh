#!/usr/bin/env bash
# Measure equivalent native-platform Coopr and Podman builds on this host.
# This is an end-to-end CLI benchmark, not an image-byte identity test.
set -euo pipefail

script_path=$(realpath "${BASH_SOURCE[0]}")
repo_root=$(cd "$(dirname "$script_path")/../.." && pwd)
iterations=${COOPR_PARITY_BENCH_ITERATIONS:-5}
multiplatform=${COOPR_PARITY_BENCH_MULTIPLATFORM:-0}

fail() {
  printf 'parity benchmark: %s\n' "$*" >&2
  exit 1
}

if [[ ${COOPR_PARITY_BENCH_IN_NIX:-} != 1 ]]; then
  command -v nix >/dev/null 2>&1 || fail 'nix is required'
  exec env COOPR_PARITY_BENCH_IN_NIX=1 \
    nix develop "path:$repo_root" -c "$script_path"
fi

[[ $iterations =~ ^[1-9][0-9]*$ ]] || fail 'COOPR_PARITY_BENCH_ITERATIONS must be positive'
[[ $multiplatform == 0 || $multiplatform == 1 ]] || \
  fail 'COOPR_PARITY_BENCH_MULTIPLATFORM must be 0 or 1'
[[ $(uname -s) == Linux ]] || fail 'Linux is required'
[[ $(id -u) != 0 ]] || fail 'run as a non-root user'
for tool in busybox date go podman; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing $tool"
done
[[ $(podman info --format '{{.Host.Security.Rootless}}') == true ]] || fail 'rootless Podman is required'

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

bench_root=$(mktemp -d /tmp/coopr-parity-benchmark.XXXXXX)
image_tags=()
manifest_tags=()
runtime_image_tags=()
runtime_image_data_homes=()
cleanup() {
  local status=$?
  local child_digest tag
  local -a child_digests=()
  trap - EXIT INT TERM
  for tag in "${image_tags[@]}"; do
    podman image rm "$tag" >/dev/null 2>&1 || true
  done
  for tag in "${manifest_tags[@]}"; do
    mapfile -t child_digests < <(
      podman manifest inspect "$tag" 2>/dev/null | jq -r '.manifests[].digest'
    )
    podman manifest rm --ignore "$tag" >/dev/null 2>&1 || true
    for child_digest in "${child_digests[@]}"; do
      podman image rm "$child_digest" >/dev/null 2>&1 || true
    done
  done
  for ((i=0; i<${#runtime_image_tags[@]}; i++)); do
    env "XDG_DATA_HOME=${runtime_image_data_homes[$i]}" \
      podman image rm "${runtime_image_tags[$i]}" >/dev/null 2>&1 || true
  done
  if [[ $bench_root == /tmp/coopr-parity-benchmark.* ]]; then
    podman unshare rm -rf -- "$bench_root"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

(cd "$repo_root" && go build -o "$bench_root/coopr" ./cmd/coopr)
if [[ $multiplatform == 1 ]]; then
  proof_context="$bench_root/multi-platform-proof"
  mkdir -p "$proof_context"
  cat >"$proof_context/proof.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	if len(os.Args) == 3 {
		marker, err := os.ReadFile(os.Args[1])
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Args[2], []byte(runtime.GOARCH+":"+string(marker)), 0o644); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) == 2 {
		proof, err := os.ReadFile(os.Args[1])
		if err != nil {
			panic(err)
		}
		fmt.Print(string(proof))
		return
	}
	fmt.Print(runtime.GOARCH)
}
EOF
  for arch in amd64 arm64; do
    GOOS=linux GOARCH=$arch CGO_ENABLED=0 \
      go build -trimpath -ldflags='-s -w' -o "$proof_context/proof-$arch" "$proof_context/proof.go"
  done
fi
printf 'builder,iteration,phase,seconds\n'

build_command() {
  local builder=$1 iteration=$2 phase=$3 definition=$4 context=$5 tag=$6
  local -n result=$7
  local -a build_args=()
  if [[ $phase == changed-final-run ]]; then
    build_args=(--build-arg suffix=changed)
  fi
  if [[ $builder == coopr ]]; then
    # Return the command array through the caller's nameref.
    # shellcheck disable=SC2034
    if [[ $phase == multi-platform-* ]]; then
      # shellcheck disable=SC2034
      result=(env "XDG_DATA_HOME=$bench_root/coopr-data-$iteration"
        "$bench_root/coopr" build --file "$definition" "$context"
        --platform linux/amd64 --platform linux/arm64 --tag "$tag")
    else
      # shellcheck disable=SC2034
      result=(env "XDG_DATA_HOME=$bench_root/coopr-data-$iteration"
        "$bench_root/coopr" build --file "$definition" "$context"
        --platform "$platform" --tag "$tag" "${build_args[@]}")
    fi
  else
    if [[ $phase == multi-platform-* ]]; then
      # shellcheck disable=SC2034
      result=(podman build --layers --pull=never --network=none --format oci
        --platform "linux/amd64,linux/arm64" --jobs=2 --manifest "$tag"
        --file "$definition" "$context")
    else
      # shellcheck disable=SC2034
      result=(podman build --layers --pull=never --network=none --format oci
        --platform "$platform" --file "$definition" --tag "$tag"
        "${build_args[@]}" "$context")
    fi
  fi
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
from "scratch"
label "dev.strio.coopr.benchmark"="$marker"
copy "busybox" "/bin/sh"
copy "busybox" "/bin/cp"
copy "busybox" "/bin/busybox"
copy "payload" "/payload"
$coopr_sleep
run "cp /payload /one" network="none"
arg "suffix" "base"
run "cp /one /result && printf '%s' \"\$suffix\" >>/result" network="none"
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
from "scratch"
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
EOF
}

verify_image() {
  local builder=$1 iteration=$2 name=$3 tag=$4 expected=$5
  local actual runtime_tag=$tag runtime_data_home=''
  if [[ $builder == coopr ]]; then
    runtime_data_home="$bench_root/coopr-data-$iteration"
    runtime_image_tags+=("$runtime_tag")
    runtime_image_data_homes+=("$runtime_data_home")
  fi
  if [[ -n $runtime_data_home ]]; then
    actual=$(env "XDG_DATA_HOME=$runtime_data_home" podman run --rm --network=none \
      --entrypoint /bin/busybox "$runtime_tag" cat /result)
  else
    actual=$(podman run --rm --network=none --entrypoint /bin/busybox \
      "$runtime_tag" cat /result)
  fi
  [[ $actual == "$expected" ]] || \
    fail "$builder iteration $iteration $name result differs: $actual"
}

verify_multi_platform_image() {
  local builder=$1 iteration=$2 tag=$3 marker=$4
  local arch actual actual_platform runtime_tag runtime_data_home manifest_json
  if [[ $builder == coopr ]]; then
    runtime_data_home="$bench_root/coopr-data-$iteration"
    for arch in amd64 arm64; do
      runtime_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-multi-$arch-runtime:latest"
      runtime_image_tags+=("$runtime_tag")
      runtime_image_data_homes+=("$runtime_data_home")
      env "XDG_DATA_HOME=$runtime_data_home" \
        "$bench_root/coopr" copy "$tag" "$runtime_tag" --platform "linux/$arch" \
        >"$bench_root/coopr-$iteration-multi-$arch-copy.log" 2>&1 || \
        fail "could not select iteration $iteration Coopr linux/$arch native image"
      actual_platform=$(env "XDG_DATA_HOME=$runtime_data_home" \
        podman image inspect --format '{{.Os}}/{{.Architecture}}' "$runtime_tag")
      [[ $actual_platform == "linux/$arch" ]] || \
        fail "Coopr iteration $iteration child is $actual_platform, expected linux/$arch"
      actual=$(env "XDG_DATA_HOME=$runtime_data_home" podman run --rm --network=none \
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
  work="$bench_root/work-$iteration"
  benchmark_marker="${bench_root##*/}-$iteration"
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
    if [[ $builder == podman ]]; then
      image_tags+=("$tag")
    fi
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
      image_tags+=("$concurrent_tag_a" "$concurrent_tag_b")
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
package as="payload"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="payload"
run "cp /payload /result" network="none"
EOF
      cat >"$component_work/image.coopr" <<EOF
from "scratch"
label "dev.strio.coopr.benchmark"="$benchmark_marker-component"
copy "busybox" "/bin/sh"
copy "busybox" "/bin/cp"
copy "busybox" "/bin/busybox"
component "local:benchmark-component"
EOF
      env "XDG_DATA_HOME=$bench_root/coopr-data-$iteration" \
        "$bench_root/coopr" component build "$component_work/component.coopr" \
        --tag benchmark-component --platform "$platform" \
        >"$bench_root/component-$iteration-publish.log" 2>&1 || { cat "$bench_root/component-$iteration-publish.log" >&2; fail "component publication failed"; }
      component_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-component:latest"
      measure coopr "$iteration" component-cold "$component_work/image.coopr" "$component_work" "$component_tag"
      measure coopr "$iteration" component-warm "$component_work/image.coopr" "$component_work" "$component_tag"
      verify_image coopr "$iteration" component "$component_tag" "component payload $iteration"
    fi

    if [[ $multiplatform == 1 ]]; then
      multi_tag="localhost/coopr-parity-benchmark-$(id -u)-$$-$iteration-$builder-multi:latest"
      if [[ $builder == podman ]]; then
        manifest_tags+=("$multi_tag")
      fi
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

