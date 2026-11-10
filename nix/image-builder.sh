# shellcheck shell=bash
# The Nix-generated prefix supplies immutable recipe/default-binary paths.
: "${coopr_nix_recipe:?Nix Coopr recipe unavailable}"
: "${coopr_nix_default_binary:?Nix Coopr default unavailable}"
: "${coopr_nix_kind:?Nix Coopr artifact kind unavailable}"
coopr_nix_fail() {
  printf 'coopr Nix %s: %s\n' "$coopr_nix_kind" "$*" >&2
  exit 2
}

coopr_nix_binary=${COOPR_BINARY:-$coopr_nix_default_binary}
command -v -- "$coopr_nix_binary" >/dev/null || coopr_nix_fail "Coopr executable unavailable: $coopr_nix_binary"

mapfile -d '' -t coopr_nix_arguments < <(
  jq -j '.args | to_entries | sort_by(.key)[] | "\(.key)=\(.value)\u0000"' "$coopr_nix_recipe"
)
declare -A coopr_nix_declared=()
for coopr_nix_argument in "${coopr_nix_arguments[@]}"; do
  coopr_nix_declared[${coopr_nix_argument%%=*}]=1
done

# Reject recipe overrides before loading archives or executing a build. Other
# native options are forwarded unchanged; the native CLI validates their arity.
# Use --option=value for literal values resembling a managed option token.
coopr_nix_target=$(jq -r '.target // empty' "$coopr_nix_recipe")
coopr_nix_expect=''
for coopr_nix_argument in "$@"; do
  if [[ -n $coopr_nix_expect ]]; then
    case $coopr_nix_expect in
      context)
        coopr_nix_context_name=${coopr_nix_argument%%=*}
        [[ ${coopr_nix_context_name,,} != coopr-nix ]] || coopr_nix_fail 'the coopr-nix build context is managed by Nix'
        ;;
      arg)
        coopr_nix_argument_name=${coopr_nix_argument%%=*}
        if [[ $coopr_nix_argument_name =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ && -v coopr_nix_declared[$coopr_nix_argument_name] ]]; then
          coopr_nix_fail "argument $coopr_nix_argument_name is declared by Nix"
        fi
        ;;
    esac
    coopr_nix_expect=''
    continue
  fi
  case $coopr_nix_argument in
    --file|--file=*|-f*|--from|--from=*|--platform|--platform=*|--arch|--arch=*|--os|--os=*|--variant|--variant=*|--all-platforms|--all-platforms=*)
      coopr_nix_fail "option $coopr_nix_argument is managed by the Nix declaration"
      ;;
    --target|--target=*)
      [[ -z $coopr_nix_target ]] || coopr_nix_fail 'target is declared by Nix'
      ;;
    --build-context) coopr_nix_expect=context ;;
    --build-context=*)
      coopr_nix_context_name=${coopr_nix_argument#*=}
      coopr_nix_context_name=${coopr_nix_context_name%%=*}
      [[ ${coopr_nix_context_name,,} != coopr-nix ]] || coopr_nix_fail 'the coopr-nix build context is managed by Nix'
      ;;
    --build-arg) coopr_nix_expect=arg ;;
    --build-arg=*)
      coopr_nix_argument_name=${coopr_nix_argument#*=}
      coopr_nix_argument_name=${coopr_nix_argument_name%%=*}
      if [[ $coopr_nix_argument_name =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ && -v coopr_nix_declared[$coopr_nix_argument_name] ]]; then
        coopr_nix_fail "argument $coopr_nix_argument_name is declared by Nix"
      fi
      ;;
    -[^-]?*)
      coopr_nix_fail 'use separate short options or --option=value; combined short options can override the Nix declaration'
      ;;
    --) coopr_nix_fail 'positional build contexts are managed by the Nix declaration' ;;
  esac
done

coopr_nix_capture=''
coopr_nix_child=''
coopr_nix_cleanup() {
  if [[ -n $coopr_nix_capture ]]; then
    rm -f -- "$coopr_nix_capture"
  fi
}
coopr_nix_cancel() {
  local coopr_nix_signal=$1 coopr_nix_status=$2
  local coopr_nix_active_child
  trap - TERM INT HUP
  # Query this shell's live jobs, including a load forked just before its PID
  # assignment. This also avoids signaling a reaped/reused recorded PID.
  for coopr_nix_active_child in $(jobs -pr); do
    kill -s "$coopr_nix_signal" "$coopr_nix_active_child" 2>/dev/null || true
  done
  wait || true
  exit "$coopr_nix_status"
}
trap coopr_nix_cleanup EXIT
trap 'coopr_nix_cancel TERM 143' TERM
trap 'coopr_nix_cancel INT 130' INT
trap 'coopr_nix_cancel HUP 129' HUP

coopr_nix_run_captured() {
  coopr_nix_capture=$(mktemp "${TMPDIR:-/tmp}/coopr-nix-load.XXXXXXXX")
  "$@" >"$coopr_nix_capture" &
  coopr_nix_child=$!
  if wait "$coopr_nix_child"; then
    coopr_nix_capture_status=0
  else
    coopr_nix_capture_status=$?
  fi
  coopr_nix_child=''
  [[ $coopr_nix_capture_status == 0 ]] || return "$coopr_nix_capture_status"
  coopr_nix_captured_output=$(<"$coopr_nix_capture")
  coopr_nix_cleanup
  coopr_nix_capture=''
}

mapfile -d '' -t coopr_nix_archives < <(
  jq -j '.components[] | select(.archive != null) | .archive, "\u0000", .digest, "\u0000", .archiveName, "\u0000"' "$coopr_nix_recipe"
)
for ((coopr_nix_index = 0; coopr_nix_index < ${#coopr_nix_archives[@]}; coopr_nix_index += 3)); do
  coopr_nix_archive=${coopr_nix_archives[$coopr_nix_index]}
  coopr_nix_expected=${coopr_nix_archives[$((coopr_nix_index + 1))]}
  coopr_nix_archive_name=${coopr_nix_archives[$((coopr_nix_index + 2))]}
  coopr_nix_run_captured "$coopr_nix_binary" component load --tag "$coopr_nix_archive_name" --input "$coopr_nix_archive"
  # Tagged load prints the alias, not its digest. Inspect the current alias
  # without a digest qualifier so an older retained root cannot mask a mismatch.
  coopr_nix_run_captured "$coopr_nix_binary" component inspect "$coopr_nix_archive_name"
  coopr_nix_actual=$(jq -ers '
    select(length == 1 and (.[0] | type == "object"))
    | .[0].digest | select(type == "string")
  ' <<<"$coopr_nix_captured_output") || coopr_nix_fail "component inspect returned no immutable root for $coopr_nix_archive_name"
  [[ $coopr_nix_actual =~ ^sha256:[0-9a-f]{64}$ ]] || coopr_nix_fail "component inspect returned no immutable root for $coopr_nix_archive_name"
  [[ $coopr_nix_actual == "$coopr_nix_expected" ]] || coopr_nix_fail "component archive root $coopr_nix_actual differs from requested $coopr_nix_expected"
done

mapfile -d '' -t coopr_nix_settings < <(
  jq -j '.definition, "\u0000", .context, "\u0000", .platform, "\u0000", .input, "\u0000", (.finalTag // ""), "\u0000"' "$coopr_nix_recipe"
)
coopr_nix_flags=(
  --file "${coopr_nix_settings[0]}"
  --platform "${coopr_nix_settings[2]}"
  --build-context "coopr-nix=oci-layout://${coopr_nix_settings[3]}:image"
)
if [[ -n $coopr_nix_target ]]; then
  coopr_nix_flags+=(--target "$coopr_nix_target")
fi
if [[ -n ${coopr_nix_settings[4]} ]]; then
  coopr_nix_flags+=(--tag "${coopr_nix_settings[4]}")
fi
coopr_nix_fixed_arguments=()
for coopr_nix_argument in "${coopr_nix_arguments[@]}"; do
  coopr_nix_fixed_arguments+=(--build-arg "$coopr_nix_argument")
done
# Declared explicit CLI values also take native precedence over build-arg files.
# Exec preserves native process signals, exit status and cache ownership.
# Keep the declared context ahead of caller options: a missing option value
# must not consume it and make native Coopr fall back to the working directory.
coopr_nix_command=(build)
if [[ $coopr_nix_kind == component ]]; then
  coopr_nix_command=(component build)
  # Native Coopr builds and captures the complete bundle in the same invocation.
  # It validates --bundle-output before execution; no post-build log parsing or
  # wrapper-owned worker lifecycle is needed. Captured coordinates are distinct
  # for archives carrying different deliveries of the same component root.
  mapfile -d '' -t coopr_nix_selected_components < <(
    jq -j '.components[] | select(.archive != null) | .invocationReference, "=", .archiveName, "\u0000"' "$coopr_nix_recipe"
  )
  for coopr_nix_selected_component in "${coopr_nix_selected_components[@]}"; do
    coopr_nix_flags+=(--input-component "$coopr_nix_selected_component")
  done
  mapfile -d '' -t coopr_nix_component_bundles < <(
    jq -j '[.components[].bundle | select(. != null)] | unique[] | ., "\u0000"' "$coopr_nix_recipe"
  )
  for coopr_nix_component_bundle in "${coopr_nix_component_bundles[@]}"; do
    coopr_nix_flags+=(--input-bundle "$coopr_nix_component_bundle")
  done
fi
exec "$coopr_nix_binary" "${coopr_nix_command[@]}" "${coopr_nix_flags[@]}" "${coopr_nix_fixed_arguments[@]}" "${coopr_nix_settings[1]}" "$@"
