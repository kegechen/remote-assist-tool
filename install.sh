#!/bin/sh

set -eu

REPOSITORY="kegechen/remote-assist-tool"

info() {
    printf '[remote-install] %s\n' "$1"
}

fail() {
    printf '[remote-install] Error: %s\n' "$1" >&2
    exit 1
}

detect_os() {
    case "$(uname -s)" in
        Linux*)
            printf 'linux'
            ;;
        Darwin*)
            printf 'darwin'
            ;;
        CYGWIN*|MINGW*|MSYS*)
            printf 'windows'
            ;;
        *)
            fail "unsupported operating system: $(uname -s)"
            ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)
            printf 'amd64'
            ;;
        arm64|aarch64)
            printf 'arm64'
            ;;
        *)
            fail "unsupported architecture: $(uname -m)"
            ;;
    esac
}

download() {
    url=$1
    output=$2

    if command -v curl >/dev/null 2>&1; then
        if curl --fail --location --silent --show-error \
            --connect-timeout 10 --max-time 300 --retry 3 \
            --output "$output" "$url"; then
            return 0
        fi
        info "curl failed; trying another downloader..."
    fi

    if command -v wget >/dev/null 2>&1; then
        if wget -q -T 30 -t 3 -O "$output" "$url"; then
            return 0
        fi
        info "wget failed; trying another downloader..."
    fi

    if [ "${os:-}" = 'windows' ] && command -v powershell.exe >/dev/null 2>&1; then
        windows_output=$output
        if command -v cygpath >/dev/null 2>&1; then
            windows_output=$(cygpath -w "$output") || return 1
        fi
        REMOTE_DOWNLOAD_URL=$url REMOTE_DOWNLOAD_OUTPUT=$windows_output \
            powershell.exe -NoProfile -NonInteractive -Command \
            '$ErrorActionPreference = "Stop"; $ProgressPreference = "SilentlyContinue"; Invoke-WebRequest -UseBasicParsing -Uri $env:REMOTE_DOWNLOAD_URL -OutFile $env:REMOTE_DOWNLOAD_OUTPUT -TimeoutSec 300'
        return $?
    fi

    return 1
}

os=$(detect_os)
arch=$(detect_arch)

extension=''
command_name='remote'
if [ "$os" = 'windows' ]; then
    extension='.exe'
    command_name='remote.exe'
fi

asset="remote-assist-cli-${os}-${arch}${extension}"
download_url="https://github.com/${REPOSITORY}/releases/latest/download/${asset}"

if [ -n "${REMOTE_INSTALL_DIR:-}" ]; then
    install_dir=$REMOTE_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
    install_dir="${HOME}/.local/bin"
else
    fail "HOME is not set; set REMOTE_INSTALL_DIR to choose an install directory"
fi

mkdir -p "$install_dir" || fail "cannot create install directory: ${install_dir}"
install_path="${install_dir}/${command_name}"
temp_file=$(mktemp "${install_dir}/.${command_name}.tmp.XXXXXX") || \
    fail "cannot create a temporary file in: ${install_dir}"

cleanup() {
    rm -f "$temp_file"
}
trap cleanup 0 1 2 15

info "Downloading latest ${asset} release..."
if ! download "$download_url" "$temp_file"; then
    fail "download failed: ${download_url}"
fi

if [ ! -s "$temp_file" ]; then
    fail "downloaded file is empty: ${download_url}"
fi

if [ "$os" != 'windows' ]; then
    chmod 755 "$temp_file" || fail "cannot make the downloaded file executable"
fi

mv -f "$temp_file" "$install_path" || fail "cannot install to: ${install_path}"
trap - 0 1 2 15

info "Installed ${command_name} to ${install_path}"

case ":${PATH:-}:" in
    *":${install_dir}:"*)
        ;;
    *)
        info "Add ${install_dir} to PATH before running ${command_name}."
        ;;
esac
