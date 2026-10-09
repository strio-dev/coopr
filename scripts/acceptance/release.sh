#!/usr/bin/env bash
# Stage the released image; ordinary Go tests verify the packaged CLI.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
if (($# != 0)); then
  printf 'usage: %s\n' "$0" >&2
  exit 2
fi
[[ $(uname -s) == Linux && $(id -u) != 0 ]] || {
  printf 'packaged acceptance requires a non-root Linux user\n' >&2
  exit 1
}
[[ $(podman info --format '{{.Host.Security.Rootless}}') == true ]] || {
  printf 'rootless Podman is required\n' >&2
  exit 1
}
run_id="$(id -u)-$$"
cli_image="localhost/coopr-release-cli:$run_id"
artifact_image="localhost/coopr-release-artifact:$run_id"
artifact_container="coopr-release-artifact-$run_id"
cli_loaded=false
artifact_created=false
container_created=false
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if $container_created; then podman rm "$artifact_container" >/dev/null 2>&1 || true; fi
  if $artifact_created; then podman rmi "$artifact_image" >/dev/null 2>&1 || true; fi
  if $cli_loaded; then podman rmi "$cli_image" >/dev/null 2>&1 || true; fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ ${COOPR_ACCEPTANCE_IMAGE_COPY+x} ]]; then
  [[ $COOPR_ACCEPTANCE_IMAGE_COPY == /* && -f $COOPR_ACCEPTANCE_IMAGE_COPY && -x $COOPR_ACCEPTANCE_IMAGE_COPY ]] || {
    printf 'COOPR_ACCEPTANCE_IMAGE_COPY must be an absolute executable file\n' >&2
    exit 1
  }
  cli_copy=$COOPR_ACCEPTANCE_IMAGE_COPY
else
  cli_copy_root=$(nix build path:.#container.copyTo --no-link --print-out-paths)
  cli_copy="$cli_copy_root/bin/copy-to"
fi
podman_store=$(podman info --format '{{.Store.GraphDriverName}}@{{.Store.GraphRoot}}+{{.Store.RunRoot}}')
podman unshare "$cli_copy" "containers-storage:[$podman_store]$cli_image"
podman image exists "$cli_image"
cli_loaded=true
export COOPR_ACCEPTANCE_RUN_IMAGE="$cli_image"
unset COOPR_ACCEPTANCE_ENTRYPOINT
if [[ ${COOPR_ACCEPTANCE_CLI+x} ]]; then
  [[ $COOPR_ACCEPTANCE_CLI == /* && -f $COOPR_ACCEPTANCE_CLI && -x $COOPR_ACCEPTANCE_CLI ]] || {
    printf 'COOPR_ACCEPTANCE_CLI must be an absolute executable file\n' >&2
    exit 1
  }
  podman create --name "$artifact_container" --image-volume=ignore "$cli_image" >/dev/null
  container_created=true
  podman cp "$COOPR_ACCEPTANCE_CLI" "$artifact_container:/coopr-acceptance-cli"
  podman commit "$artifact_container" "$artifact_image" >/dev/null
  artifact_created=true
  podman rm "$artifact_container" >/dev/null
  container_created=false
  export COOPR_ACCEPTANCE_RUN_IMAGE="$artifact_image"
  export COOPR_ACCEPTANCE_ENTRYPOINT=/coopr-acceptance-cli
fi
export COOPR_ACCEPTANCE_REPO_ROOT="$repo_root"
if [[ ${COOPR_ACCEPTANCE_TEST_BINARY+x} ]]; then
  [[ $COOPR_ACCEPTANCE_TEST_BINARY == /* && -f $COOPR_ACCEPTANCE_TEST_BINARY && -x $COOPR_ACCEPTANCE_TEST_BINARY ]] || {
    printf 'COOPR_ACCEPTANCE_TEST_BINARY must be an absolute executable file\n' >&2
    exit 1
  }
  inventory=$("$COOPR_ACCEPTANCE_TEST_BINARY" -test.list '^TestPackagedAcceptance$')
  [[ $inventory == TestPackagedAcceptance ]] || {
    printf 'COOPR_ACCEPTANCE_TEST_BINARY does not contain TestPackagedAcceptance\n' >&2
    exit 1
  }
  "$COOPR_ACCEPTANCE_TEST_BINARY" -test.v -test.count=1 -test.timeout=30m -test.run '^TestPackagedAcceptance$'
else
  go test -count=1 -v -timeout=30m ./internal/acceptance -run '^TestPackagedAcceptance$'
fi
