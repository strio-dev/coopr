#!/bin/sh
set -eu

fail() {
    printf 'coopr installer: %s\n' "$*" >&2
    exit 1
}

main() {
    case "${1:-}" in
        -h|--help)
            printf 'Usage: sh install.sh [VERSION]\nInstall the latest stable release, or an exact release tag.\nCOOPR_INSTALL_PREFIX defaults to ~/.local.\n'
            return
            ;;
    esac
    [ "$#" -le 1 ] || fail 'Expected at most one release version.'
    [ "$(uname -s)" = Linux ] || fail 'Coopr requires Linux.'
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail 'Supported architectures are amd64 and arm64.' ;;
    esac
    for tool in curl tar sha256sum awk mktemp install; do
        command -v "$tool" >/dev/null 2>&1 || fail "Required tool not found: $tool"
    done

    releases=https://github.com/strio-dev/coopr/releases
    if [ "$#" -eq 0 ]; then
        release_url=$(curl -fsSLI --proto '=https' --proto-redir '=https' \
            -o /dev/null -w '%{url_effective}' "$releases/latest")
        case "$release_url" in
            "$releases/tag/"*) download_url="$releases/download/${release_url##*/}" ;;
            *) fail "No published release found. See $releases" ;;
        esac
    else
        case "$1" in
            ''|*[!a-zA-Z0-9.+-]*) fail 'Use an exact release tag, such as 1.2.3.' ;;
        esac
        download_url="$releases/download/$1"
    fi

    prefix=${COOPR_INSTALL_PREFIX:-"$HOME/.local"}
    temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/coopr-install.XXXXXX")
    trap 'chmod -R u+w "$temp_dir"; rm -rf "$temp_dir"' 0
    trap 'exit 1' 1 2 15
    asset="coopr-linux-$arch.tar.gz"
    curl -fsSL --proto '=https' --proto-redir '=https' -o "$temp_dir/$asset" "$download_url/$asset"
    curl -fsSL --proto '=https' --proto-redir '=https' -o "$temp_dir/SHA256SUMS" "$download_url/SHA256SUMS"
    awk -v asset="$asset" '
        $2 == asset || $2 == "./" asset { print $1 "  " asset; found = 1 }
        END { if (!found) exit 1 }
    ' "$temp_dir/SHA256SUMS" > "$temp_dir/checksum" || fail "Missing checksum for $asset."
    (cd "$temp_dir" && sha256sum -c checksum)
    tar -xzf "$temp_dir/$asset" -C "$temp_dir" coopr LICENSE third-party
    chmod -R u+w "$temp_dir"
    mkdir -p "$prefix/bin" "$prefix/share/licenses/coopr"
    cp -R "$temp_dir/third-party" "$prefix/share/licenses/coopr/"
    install -m 644 "$temp_dir/LICENSE" "$prefix/share/licenses/coopr/LICENSE"
    install -m 755 "$temp_dir/coopr" "$prefix/bin/coopr"

    printf 'Installed Coopr to %s/bin/coopr\n' "$prefix"
    case ":${PATH:-}:" in
        *":$prefix/bin:"*) ;;
        *) printf 'Add %s/bin to your PATH.\n' "$prefix" ;;
    esac
}

main "$@"
