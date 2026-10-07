#!/usr/bin/env bash
# Verify the self-contained Coopr image on Linux.
# Invoke this through `just packaged-acceptance`.
set -euo pipefail

script_path=$(realpath "${BASH_SOURCE[0]}")
repo_root=$(cd "$(dirname "$script_path")/../.." && pwd)
cd "$repo_root"

if (($# != 0)); then
  printf 'usage: %s\n' "$0" >&2
  exit 2
fi

run_id="$(id -u)-$$"
readonly run_id
readonly cli_test_image="localhost/coopr-release-cli:${run_id}"
readonly artifact_image="localhost/coopr-release-artifact:${run_id}"
readonly artifact_container="coopr-release-artifact-${run_id}"
cli_run_image="$cli_test_image"
cli_runtime_args=()
cli_mode='packaged dynamic CLI'
if [[ ${COOPR_ACCEPTANCE_CLI+x} ]]; then
  [[ $COOPR_ACCEPTANCE_CLI == /* && -f $COOPR_ACCEPTANCE_CLI && -x $COOPR_ACCEPTANCE_CLI ]] || {
    printf 'release acceptance: COOPR_ACCEPTANCE_CLI must be an absolute path to an executable file\n' >&2
    exit 1
  }
  cli_runtime_args+=(--entrypoint=/coopr-acceptance-cli)
  cli_mode="artifact CLI: $COOPR_ACCEPTANCE_CLI"
fi
readonly cli_mode

acceptance_root=''
runtime_image=''
multi_runtime_images=()
multi_component_runtime_images=()
docker_runtime_image=''
component_runtime_image=''
named_runtime_image=''
containerfile_conformance_image=''
coopr_conformance_image=''
cli_image_loaded=false
artifact_container_created=false
artifact_image_created=false

cleanup() {
  local status=$?
  trap - EXIT INT TERM

  if [[ -n "$runtime_image" ]]; then
    podman rmi "$runtime_image" >/dev/null 2>&1 || true
  fi
  for image in "${multi_runtime_images[@]}"; do
    podman rmi "$image" >/dev/null 2>&1 || true
  done
  for image in "${multi_component_runtime_images[@]}"; do
    podman rmi "$image" >/dev/null 2>&1 || true
  done
  if [[ -n "$docker_runtime_image" ]]; then
    podman rmi "$docker_runtime_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$component_runtime_image" ]]; then
    podman rmi "$component_runtime_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$named_runtime_image" ]]; then
    podman rmi "$named_runtime_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$containerfile_conformance_image" ]]; then
    podman rmi "$containerfile_conformance_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$coopr_conformance_image" ]]; then
    podman rmi "$coopr_conformance_image" >/dev/null 2>&1 || true
  fi
  if [[ $artifact_container_created == true ]]; then
    podman rm "$artifact_container" >/dev/null 2>&1 || true
  fi
  if [[ $artifact_image_created == true ]]; then
    podman rmi "$artifact_image" >/dev/null 2>&1 || true
  fi
  if [[ $cli_image_loaded == true ]]; then
    podman rmi "$cli_test_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$acceptance_root" && $acceptance_root == /tmp/coopr-release-acceptance.* ]]; then
    chmod -R u+w -- "$acceptance_root" >/dev/null 2>&1 || true
    rm -rf -- "$acceptance_root"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'release acceptance: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable in the Nix development shell: $1"
}

for command_name in busybox diff go jq podman tar; do
  require_command "$command_name"
done

if [[ ${COOPR_ACCEPTANCE_IMAGE_COPY+x} ]]; then
  [[ $COOPR_ACCEPTANCE_IMAGE_COPY == /* && -f $COOPR_ACCEPTANCE_IMAGE_COPY && -x $COOPR_ACCEPTANCE_IMAGE_COPY ]] || \
    fail 'COOPR_ACCEPTANCE_IMAGE_COPY must be an absolute path to an executable file'
else
  require_command nix
fi

[[ $(uname -s) == Linux ]] || fail 'this harness requires Linux'
case $(uname -m) in
  x86_64) native_arch=amd64; foreign_binfmt=qemu-aarch64 ;;
  aarch64) native_arch=arm64; foreign_binfmt=qemu-x86_64 ;;
  *) fail "this release profile requires an amd64 or arm64 host, got $(uname -m)" ;;
esac
if [[ ${COOPR_TEST_BINFMT_HANDLER+x} ]]; then
  foreign_binfmt=$COOPR_TEST_BINFMT_HANDLER
  [[ -n $foreign_binfmt && $foreign_binfmt != . && $foreign_binfmt != .. && $foreign_binfmt != */* ]] || \
    fail 'COOPR_TEST_BINFMT_HANDLER must name a binfmt registration'
fi
readonly native_arch foreign_binfmt
native_platform="linux/$native_arch"
readonly native_platform
binfmt_registration="/proc/sys/fs/binfmt_misc/$foreign_binfmt"
[[ -r $binfmt_registration ]] || fail "foreign RUN requires registered $foreign_binfmt binfmt handler"
binfmt_data=$(<"$binfmt_registration")
if [[ ${binfmt_data%%$'\n'*} != enabled ]] || ! grep -Eq '^flags:.*F' <<<"$binfmt_data"; then
  fail "foreign RUN requires enabled $foreign_binfmt binfmt handler with F flag"
fi
[[ $(id -u) != 0 ]] || fail 'run this harness as a non-root user'
[[ $(podman info --format '{{.Host.Security.Rootless}}') == true ]] || fail 'rootless Podman is required'

acceptance_root=$(mktemp -d /tmp/coopr-release-acceptance.XXXXXX)
readonly acceptance_root

printf 'Acceptance executable: %s\n' "$cli_mode"
printf 'Building and loading the self-contained Coopr image\n'
if [[ ${COOPR_ACCEPTANCE_IMAGE_COPY+x} ]]; then
  cli_copy=$COOPR_ACCEPTANCE_IMAGE_COPY
else
  cli_copy_root=$(nix build path:.#container.copyTo --no-link --print-out-paths)
  cli_copy="$cli_copy_root/bin/copy-to"
fi
podman_store=$(podman info --format '{{.Store.GraphDriverName}}@{{.Store.GraphRoot}}+{{.Store.RunRoot}}')
podman unshare "$cli_copy" "containers-storage:[$podman_store]$cli_test_image"
podman image exists "$cli_test_image" || fail "container copy did not create $cli_test_image"
cli_image_loaded=true

if [[ ${COOPR_ACCEPTANCE_CLI+x} ]]; then
  # Standard stopped-container staging gives the artifact normal image labels.
  podman create --name "$artifact_container" --image-volume=ignore "$cli_test_image" >/dev/null
  artifact_container_created=true
  podman cp "$COOPR_ACCEPTANCE_CLI" "$artifact_container:/coopr-acceptance-cli"
  podman commit "$artifact_container" "$artifact_image" >/dev/null
  artifact_image_created=true
  podman rm "$artifact_container" >/dev/null
  artifact_container_created=false
  cli_run_image="$artifact_image"
fi

printf 'Checking unprivileged CLI startup\n'
podman run --rm --network=none --cap-drop=all --security-opt=no-new-privileges \
  "${cli_runtime_args[@]}" "$cli_run_image" --help >/dev/null
podman run --rm --network=none --cap-drop=all --security-opt=no-new-privileges \
  "${cli_runtime_args[@]}" "$cli_run_image" build --help >/dev/null
podman run --rm --network=none --cap-drop=all --security-opt=no-new-privileges \
  "${cli_runtime_args[@]}" "$cli_run_image" component build --help >/dev/null

workspace="$acceptance_root/workspace"
state="$acceptance_root/state"
mkdir -m 0777 "$workspace" "$state"
cp "$(command -v busybox)" "$workspace/busybox"
chmod 0755 "$workspace/busybox"
base_tag="coopr-release-base:${run_id}"
component_tag="coopr-release-component-${run_id}"
multi_component_tag="coopr-release-multi-component-${run_id}"
multi_platform_tag="coopr-release-multi:${run_id}"
cat >"$workspace/base.coopr" <<EOF
from "scratch"
copy "busybox" "/bin/sh"
copy "busybox" "/bin/cat"
copy "busybox" "/bin/busybox"
run "printf 'base-${run_id}\\n' >/proof"
EOF
cat >"$workspace/child.coopr" <<EOF
from "${base_tag}" as="source"
from "${base_tag}"
run "cat /input >/mounted-proof" network="none" {
  mount "bind" from="source" source="/proof" target="/input"
}
run "test -z \"\$(/bin/busybox ip route show default)\"" network="none"
run "printf 'child\\n' >>/proof" network="none"
cmd {
    exec "/bin/cat" "/proof" "/mounted-proof"
}
EOF
cat >"$workspace/component.coopr" <<'EOF'
extend
run "printf 'component\n' >>/proof" network="none"
EOF
cat >"$workspace/component-child.coopr" <<EOF
from "${base_tag}"
component "local:${component_tag}"
cmd {
    exec "/bin/cat" "/proof"
}
EOF
cat >"$workspace/named-child.coopr" <<'EOF'
from "base"
copy "context-payload" "/context-payload" from="assets"
cmd {
    exec "/bin/cat" "/proof" "/context-payload"
}
EOF
cat >"$workspace/multi-platform.coopr" <<'EOF'
from "scratch"
arg "TARGETARCH"
copy "proof-$TARGETARCH" "/foreign-proof" chmod="0755"
run network="none" {
    exec "/foreign-proof"
}
label "dev.strio.coopr.release-acceptance"="multi-platform"
cmd {
    exec "/foreign-proof"
}
EOF
cat >"$workspace/multi-component.coopr" <<'EOF'
package as="payload"
arg "TARGETARCH"
copy "proof-$TARGETARCH" "/component-proof" chmod="0755"
extend
copy "/component-proof" "/component-proof" from="payload"
EOF
cat >"$workspace/multi-component-child.coopr" <<EOF
from "scratch"
component "local:${multi_component_tag}"
run network="none" {
    exec "/component-proof"
}
cmd {
    exec "/component-proof"
}
EOF
cat >"$acceptance_root/foreign-proof.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	if proof, err := os.ReadFile("/platform-proof"); err == nil {
		fmt.Print(string(proof))
		return
	}
	if err := os.WriteFile("/platform-proof", []byte(runtime.GOARCH+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
EOF
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -o "$workspace/proof-$arch" "$acceptance_root/foreign-proof.go"
done
mkdir -m 0777 "$workspace/assets"
printf 'named-context\n' >"$workspace/assets/context-payload"
chmod 0666 "$workspace/base.coopr" "$workspace/child.coopr" \
  "$workspace/component.coopr" "$workspace/component-child.coopr" \
  "$workspace/named-child.coopr" "$workspace/multi-platform.coopr" \
  "$workspace/multi-component.coopr" "$workspace/multi-component-child.coopr" \
  "$workspace/assets/context-payload"

coopr_container() {
  local network=$1
  shift
  podman run --rm --network="$network" \
    --device=/dev/fuse:rw \
    --security-opt=seccomp=unconfined \
    --security-opt=label=disable \
    --mount "type=bind,src=${workspace},dst=/work,rw" \
    --mount "type=bind,src=${state},dst=/var/lib,rw" \
    "${cli_runtime_args[@]}" "$cli_run_image" "$@"
}

printf 'Comparing equivalent Containerfile and Coopr builds\n'
conformance_root="$workspace/containerfile-conformance"
cp -R "$repo_root/scripts/acceptance/fixtures/containerfile-conformance" "$conformance_root"
cp "$(command -v busybox)" "$conformance_root/busybox"
chmod 0755 "$conformance_root/busybox"
printf 'archive:static\n' >"$conformance_root/archive-data"
tar -cf "$conformance_root/payload.tar" -C "$conformance_root" archive-data
containerfile_conformance_image="localhost/coopr-containerfile-conformance:${run_id}"
podman build --pull=never --format docker --layers --network=none \
  --build-arg message=release-acceptance \
  --file "$conformance_root/Containerfile" \
  --tag "$containerfile_conformance_image" \
  "$conformance_root" >/dev/null
coopr_container none \
  build /work/containerfile-conformance/image.coopr \
  --context /work/containerfile-conformance \
  --build-arg message=release-acceptance \
  --format docker \
  --tag oci-archive:/work/containerfile-conformance/coopr.oci.tar \
  --platform "$native_platform"
conformance_load_output=$(podman load -i "$conformance_root/coopr.oci.tar")
coopr_conformance_image=$(awk '/Loaded image:/ { print $NF }' <<<"$conformance_load_output" | tail -n 1)
[[ -n "$coopr_conformance_image" ]] || fail 'could not identify the loaded Coopr conformance image'

containerfile_conformance_output=$(podman run --rm --network=none "$containerfile_conformance_image")
coopr_conformance_output=$(podman run --rm --network=none "$coopr_conformance_image")
expected_conformance_output=$'stage:release-acceptance\ncontext:static\nfinal:release-acceptance\nenv:containerfile\narchive:static'
if [[ $containerfile_conformance_output != "$expected_conformance_output" ]]; then
  fail "unexpected Containerfile conformance output: $containerfile_conformance_output"
fi
if [[ $coopr_conformance_output != "$containerfile_conformance_output" ]]; then
  fail "Coopr conformance output differs from Containerfile output: $coopr_conformance_output"
fi

normalize_image_config() {
  local image=$1
  local archive=$2
  local config_name
  podman save --format docker-archive --output "$archive" "$image" >/dev/null
  config_name=$(tar -xOf "$archive" manifest.json | jq -er '.[0].Config')
  tar -xOf "$archive" "$config_name" | jq --sort-keys '.config |
    def selected_env($name): [.Env[]? | select(startswith($name + "="))] | sort;
    {
      Cmd,
      Entrypoint,
      Env: selected_env("COOPR_PARITY"),
      ExposedPorts: ((.ExposedPorts // {}) | keys | sort),
      Healthcheck: (.Healthcheck // null),
      Labels: {"dev.strio.coopr.conformance": .Labels["dev.strio.coopr.conformance"]},
      OnBuild: (.OnBuild // []),
      Shell: (.Shell // []),
      StopSignal,
      User,
      Volumes: ((.Volumes // {}) | keys | sort),
      WorkingDir
    }'
}
normalize_image_config "$containerfile_conformance_image" \
  "$acceptance_root/containerfile-image.tar" >"$acceptance_root/containerfile-config.json"
normalize_image_config "$coopr_conformance_image" \
  "$acceptance_root/coopr-image.tar" >"$acceptance_root/coopr-config.json"
if ! diff -u "$acceptance_root/containerfile-config.json" "$acceptance_root/coopr-config.json"; then
  fail 'Coopr image configuration differs from the equivalent Containerfile build'
fi

printf 'Building and copying a multi-platform image index in the packaged image\n'
coopr_container none \
  build /work/multi-platform.coopr --tag "$multi_platform_tag" \
  --platform linux/amd64 --platform linux/arm64
coopr_container none \
  copy "$multi_platform_tag" oci-archive:/work/multi-platform.oci.tar

multi_archive_root="$acceptance_root/multi-platform-archive"
mkdir "$multi_archive_root"
tar -xf "$workspace/multi-platform.oci.tar" -C "$multi_archive_root"
multi_root_digest=$(jq -er '
  if .manifests | length == 1 then
    .manifests[0]
    | select(.mediaType == "application/vnd.oci.image.index.v1+json")
    | .digest
  else
    error("archive must contain exactly one root descriptor")
  end
' "$multi_archive_root/index.json")
multi_root_blob="$multi_archive_root/blobs/sha256/${multi_root_digest#sha256:}"
[[ -f "$multi_root_blob" ]] || fail "multi-platform archive lacks root index $multi_root_digest"
jq -e '
  .mediaType == "application/vnd.oci.image.index.v1+json"
  and ([.manifests[] | (.platform.os + "/" + .platform.architecture)] | sort
       == ["linux/amd64", "linux/arm64"])
' "$multi_root_blob" >/dev/null || fail 'multi-platform archive does not contain amd64 and arm64 image manifests'

while IFS=$'\t' read -r manifest_digest expected_os expected_arch; do
  manifest_blob="$multi_archive_root/blobs/sha256/${manifest_digest#sha256:}"
  [[ -f "$manifest_blob" ]] || fail "multi-platform archive lacks child manifest $manifest_digest"
  config_digest=$(jq -er '.config.digest' "$manifest_blob")
  config_blob="$multi_archive_root/blobs/sha256/${config_digest#sha256:}"
  [[ -f "$config_blob" ]] || fail "multi-platform archive lacks config $config_digest"
  actual_platform=$(jq -er '.os + "/" + .architecture' "$config_blob")
  [[ $actual_platform == "$expected_os/$expected_arch" ]] || \
    fail "multi-platform child $manifest_digest config is $actual_platform, expected $expected_os/$expected_arch"
done < <(jq -er '.manifests[] | [.digest, .platform.os, .platform.architecture] | @tsv' "$multi_root_blob")

for arch in amd64 arm64; do
  coopr_container none copy "$multi_platform_tag" \
    "oci-archive:/work/multi-$arch.oci.tar" --platform "linux/$arch"
  child_load_output=$(podman load -i "$workspace/multi-$arch.oci.tar")
  child_image=$(awk '/Loaded image:/ { print $NF }' <<<"$child_load_output" | tail -n 1)
  [[ -n $child_image ]] || fail "could not identify loaded $arch child image: $child_load_output"
  multi_runtime_images+=("$child_image")
  actual_proof=$(podman run --rm --network=none --platform "linux/$arch" "$child_image")
  [[ $actual_proof == "$arch" ]] || \
    fail "multi-platform $arch image did not retain its build-time foreign RUN proof: $actual_proof"
done

printf 'Publishing and invoking a multi-platform component in the packaged image\n'
coopr_container none \
  component build /work/multi-component.coopr --tag "$multi_component_tag" \
  --platform linux/amd64 --platform linux/arm64
for arch in amd64 arm64; do
  coopr_container none \
    build /work/multi-component-child.coopr \
    --tag "oci-archive:/work/multi-component-child-$arch.oci.tar" \
    --platform "linux/$arch"
  component_child_load_output=$(podman load -i "$workspace/multi-component-child-$arch.oci.tar")
  component_child_image=$(awk '/Loaded image:/ { print $NF }' <<<"$component_child_load_output" | tail -n 1)
  [[ -n $component_child_image ]] || \
    fail "could not identify loaded $arch multi-platform component consumer: $component_child_load_output"
  multi_component_runtime_images+=("$component_child_image")
  actual_proof=$(podman run --rm --network=none --platform "linux/$arch" "$component_child_image")
  [[ $actual_proof == "$arch" ]] || \
    fail "multi-platform component consumer for $arch did not retain its build-time proof: $actual_proof"
done

printf 'Building through embedded Buildah in the packaged image\n'
coopr_container slirp4netns \
  build /work/base.coopr --tag "$base_tag" --platform "$native_platform"

printf 'Checking that chroot does not start an unused nested network helper\n'
coopr_container none \
  build /work/base.coopr --network=slirp4netns --no-cache \
  --tag "$base_tag" --platform "$native_platform"

printf 'Reusing the packaged image store without network access\n'
coopr_container none \
  build /work/child.coopr --tag oci-archive:/work/child.oci.tar --platform "$native_platform"

load_output=$(podman load -i "$workspace/child.oci.tar")
printf '%s\n' "$load_output"
runtime_image=$(awk '/Loaded image:/ { print $NF }' <<<"$load_output" | tail -n 1)
[[ -n "$runtime_image" ]] || fail 'could not identify the loaded image'
runtime_output=$(podman run --rm --network=none "$runtime_image")
if [[ $runtime_output != $'base-'"$run_id"$'\nchild\nbase-'"$run_id" ]]; then
  fail "unexpected built image output: $runtime_output"
fi

printf 'Building with local and image-backed named contexts in the packaged image\n'
coopr_container none \
  build /work/named-child.coopr \
  --build-context "base=docker-image://${base_tag}" \
  --build-context assets=/work/assets \
  --tag oci-archive:/work/named-child.oci.tar --platform "$native_platform"
named_load_output=$(podman load -i "$workspace/named-child.oci.tar")
named_runtime_image=$(awk '/Loaded image:/ { print $NF }' <<<"$named_load_output" | tail -n 1)
[[ -n "$named_runtime_image" ]] || fail 'could not identify the named-context image'
named_runtime_output=$(podman run --rm --network=none "$named_runtime_image")
if [[ $named_runtime_output != $'base-'"$run_id"$'\nnamed-context' ]]; then
  fail "unexpected named-context image output: $named_runtime_output"
fi

printf 'Loading and running Docker schema 2 output from the packaged builder\n'
coopr_container none \
  build /work/child.coopr --format docker \
  --tag oci-archive:/work/child-docker.oci.tar --platform "$native_platform"
docker_load_output=$(podman load -i "$workspace/child-docker.oci.tar")
printf '%s\n' "$docker_load_output"
docker_runtime_image=$(awk '/Loaded image:/ { print $NF }' <<<"$docker_load_output" | tail -n 1)
[[ -n "$docker_runtime_image" ]] || fail 'could not identify the loaded Docker-format image'
docker_runtime_output=$(podman run --rm --network=none "$docker_runtime_image")
if [[ $docker_runtime_output != "$runtime_output" ]]; then
  fail "unexpected Docker-format image output: $docker_runtime_output"
fi

printf 'Publishing and invoking a component in the packaged image without network access\n'
coopr_container none \
  component build /work/component.coopr --tag "$component_tag" --platform "$native_platform"
coopr_container none \
  build /work/component-child.coopr --tag oci-archive:/work/component-child.oci.tar \
  --cache oci-layout:/var/lib/coopr/component-cache --platform "$native_platform"
[[ -s "$state/coopr/component-cache/index.json" ]] || fail 'packaged component cache was not populated'

component_load_output=$(podman load -i "$workspace/component-child.oci.tar")
printf '%s\n' "$component_load_output"
component_runtime_image=$(awk '/Loaded image:/ { print $NF }' <<<"$component_load_output" | tail -n 1)
[[ -n "$component_runtime_image" ]] || fail 'could not identify the loaded component image'
component_runtime_output=$(podman run --rm --network=none "$component_runtime_image")
if [[ $component_runtime_output != $'base-'"$run_id"$'\ncomponent' ]]; then
  fail "unexpected component image output: $component_runtime_output"
fi

[[ -f "$state/containers/storage/overlay/.has-mount-program" ]] || fail 'packaged builds did not use fuse-overlayfs'

printf 'Checking the explicit OCI isolation override\n'
oci_state="$acceptance_root/oci-state"
mkdir -m 0777 "$oci_state"
podman run --rm --network=none \
  --userns=keep-id:uid=1000,gid=1000 --user=1000:1000 \
  --cap-add=SYS_ADMIN --device=/dev/fuse:rw \
  --security-opt=seccomp=unconfined --security-opt=label=disable \
  --security-opt=unmask=ALL \
  --env HOME=/home/user --env USER=user --env XDG_RUNTIME_DIR=/run/user/1000 \
  --mount "type=bind,src=${workspace},dst=/work,rw" \
  --mount "type=bind,src=${oci_state},dst=/var/lib,rw" \
  "${cli_runtime_args[@]}" "$cli_run_image" \
  build /work/multi-platform.coopr --isolation=rootless --network=none \
  --platform "$native_platform" --tag oci-archive:/work/oci-override.oci.tar
[[ -s "$workspace/oci-override.oci.tar" ]] || fail 'explicit OCI override did not produce an image'

printf 'Packaged acceptance passed on %s\n' "$native_platform"
