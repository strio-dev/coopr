#!/usr/bin/env bash
set -euo pipefail
script_path=$(realpath "${BASH_SOURCE[0]}")
repo_root=$(cd "$(dirname "$script_path")/../.." && pwd)
cd "$repo_root"
if [[ ${COOPR_DOCKER_ACCEPTANCE_IN_NIX:-} != 1 ]]; then
  exec env COOPR_DOCKER_ACCEPTANCE_IN_NIX=1 nix develop "path:$repo_root" -c "$script_path"
fi
coopr_docker_proof_root=$(mktemp -d /tmp/coopr-final-docker.XXXXXX)
coopr_docker_proof_name="coopr-final-docker-$(id -u)-$$"
coopr_docker_proof_id=''
coopr_docker_modern=${COOPR_DOCKER_MODERN_STORE:-true}
coopr_docker_driver=native
coopr_docker_multiplatform=1
if [[ $coopr_docker_modern == false ]]; then
  coopr_docker_driver=vfs
  coopr_docker_multiplatform=0
elif [[ $coopr_docker_modern != true ]]; then
  printf 'COOPR_DOCKER_MODERN_STORE must be true or false\n' >&2
  exit 2
fi
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n $coopr_docker_proof_id ]]; then
    local actual_id
    actual_id=$(podman inspect --format '{{.Id}}' "$coopr_docker_proof_name" 2>/dev/null || true)
    if [[ $actual_id == "$coopr_docker_proof_id" ]]; then
      podman rm --force --volumes "$coopr_docker_proof_id" >/dev/null
    fi
  fi
  rm -rf -- "$coopr_docker_proof_root"
  exit "$status"
}
trap cleanup EXIT
mkdir -m 0777 "$coopr_docker_proof_root/runtime"
mkdir "$coopr_docker_proof_root/bin"
coopr_docker_proof_id=$(podman run -d --name "$coopr_docker_proof_name" \
  --privileged --network=none --security-opt=label=disable \
  --tmpfs /var/lib/docker:size=3g \
  -v "$coopr_docker_proof_root/runtime:/run/coopr" \
  --entrypoint dockerd \
  docker.io/library/docker@sha256:754ce04dd9dee9ef015680b8529fc49608efb75f30322a54780e7aab32698a47 \
  --host unix:///run/coopr/docker.sock --feature "containerd-snapshotter=$coopr_docker_modern" \
  --storage-driver="$coopr_docker_driver" --bridge=none --iptables=false --ip6tables=false \
  --ip-forward=false --ip-masq=false)
export DOCKER_HOST="unix://$coopr_docker_proof_root/runtime/docker.sock"
export COOPR_DOCKER_PARITY_CONTAINER="$coopr_docker_proof_name"
export COOPR_DOCKER_PARITY_DATA_HOME="${XDG_DATA_HOME:-$HOME/.local/share}"
cat > "$coopr_docker_proof_root/bin/docker" <<'DOCKER_WRAPPER'
#!/usr/bin/env bash
export XDG_DATA_HOME="${COOPR_DOCKER_PARITY_DATA_HOME}"
exec podman exec "${COOPR_DOCKER_PARITY_CONTAINER}" docker --host unix:///run/coopr/docker.sock "$@"
DOCKER_WRAPPER
chmod +x "$coopr_docker_proof_root/bin/docker"
export PATH="$coopr_docker_proof_root/bin:$PATH"
for ((attempt=0; attempt<60; attempt++)); do
  if docker info > "$coopr_docker_proof_root/info.log" 2>&1; then
    break
  fi
  sleep 0.5
done
docker info --format '{{.ServerVersion}} {{json .DriverStatus}}'
COOPR_TEST_BUILDAH=1 COOPR_TEST_DOCKER=1 COOPR_TEST_DOCKER_MULTIPLATFORM="$coopr_docker_multiplatform" \
  go test -count=1 -v -run 'TestBuildAndCopy.*DockerEngine|TestExplicitDockerImportThenOfflineFrom' ./internal/build
