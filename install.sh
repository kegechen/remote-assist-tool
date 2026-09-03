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

# compute_sha256 各平台能凑出 SHA-256 的三条路：Linux/Git Bash 有 sha256sum，macOS 自带
# 的是 perl 版 shasum（没有 sha256sum），再不济还有 openssl。三个都没有才算失败。
#
# 一律从标准输入喂而不是传文件名：传文件名的话输出行里会带上路径，而 GNU coreutils 碰到
# 路径里有反斜杠或换行会在整行前面加一个转义用的 '\'，cut 取到的第一段就不再是纯哈希了
# ——Git Bash 下 REMOTE_INSTALL_DIR=C:\foo 就够触发。走 stdin 后输出里压根没有文件名。
compute_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum < "$1" | cut -d ' ' -f 1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 < "$1" | cut -d ' ' -f 1
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 < "$1" | sed 's/.*= *//'
    else
        return 1
    fi
}

# verify_checksum 拿 release 上的 SHA256SUMS 核对刚下载的二进制。
#
# 这一步挡的是"下到的东西不是发布的东西"：代理缓存吐了半截、CDN 边缘节点被换、传输中被
# 截断。它不是信任锚——SHA256SUMS 和二进制走的是同一条 HTTPS、来自同一个 release，能替
# 换其中一个的人多半也能替换另一个；真正的信任锚是签名，本项目暂时没有。所以这里的定位
# 是完整性校验，但仍然失败即中止：能悄悄让校验"跳过"的检查等于没有检查。
verify_checksum() {
    file=$1

    sums_file="${temp_file}.sums"
    sums_url="https://github.com/${REPOSITORY}/releases/latest/download/SHA256SUMS"
    if ! download "$sums_url" "$sums_file"; then
        fail "cannot download the checksum list: ${sums_url}
  (set REMOTE_INSTALL_SKIP_CHECKSUM=1 to install without verification)"
    fi

    # SHA256SUMS 的行格式是 "<hash>  <文件名>"；openssl/coreutils 的二进制模式会在文件名
    # 前加个 '*'，两种都认。
    expected=$(awk -v want="$asset" '$2 == want || $2 == "*" want { print $1; exit }' "$sums_file")
    if [ -z "$expected" ]; then
        fail "${asset} is not listed in SHA256SUMS
  (set REMOTE_INSTALL_SKIP_CHECKSUM=1 to install without verification)"
    fi

    if ! actual=$(compute_sha256 "$file"); then
        fail "no SHA-256 tool found (need sha256sum, shasum, or openssl)
  (set REMOTE_INSTALL_SKIP_CHECKSUM=1 to install without verification)"
    fi

    if [ "$actual" != "$expected" ]; then
        fail "checksum mismatch for ${asset}
  expected: ${expected}
  actual:   ${actual}
  The download is corrupt or has been tampered with; nothing was installed."
    fi

    info "Checksum verified."
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
    rm -f "$temp_file" "${temp_file}.sums"
}
trap cleanup 0 1 2 15

info "Downloading latest ${asset} release..."
if ! download "$download_url" "$temp_file"; then
    fail "download failed: ${download_url}"
fi

if [ ! -s "$temp_file" ]; then
    fail "downloaded file is empty: ${download_url}"
fi

# 校验放在 chmod / mv 之前：不合格的文件一次都不该以可执行的形式落到 install_path 上。
if [ "${REMOTE_INSTALL_SKIP_CHECKSUM:-}" = '1' ]; then
    info "REMOTE_INSTALL_SKIP_CHECKSUM=1: skipping checksum verification."
else
    verify_checksum "$temp_file"
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
