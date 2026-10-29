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
    sums_url="${release_base}/SHA256SUMS"
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

# resolve_latest_tag 打印最新正式 Release 的 tag，拿不到时什么都不打印。
#
# 走 github.com/<repo>/releases/latest 的 302 跳转（落到 .../releases/tag/<tag>），不调
# REST API：匿名 API 每小时 60 次的限额在公司出口 NAT 后面很容易被别人用光。
resolve_latest_tag() {
    latest_url="https://github.com/${REPOSITORY}/releases/latest"
    location=''

    if command -v curl >/dev/null 2>&1; then
        location=$(curl --fail --location --silent --show-error \
            --connect-timeout 10 --max-time 30 --retry 2 \
            --output /dev/null --write-out '%{url_effective}' "$latest_url" 2>/dev/null) || location=''
    fi

    if [ -z "$location" ] && command -v wget >/dev/null 2>&1; then
        location=$(wget --max-redirect=0 -S -T 30 -t 2 -O /dev/null "$latest_url" 2>&1 |
            awk 'tolower($1) == "location:" { print $2 }' | tail -n 1 | tr -d '\r')
    fi

    # 与 download 一样，Windows 上 curl/wget 都不通时用系统 PowerShell 兜底：不跟随跳转，
    # 直接读 302 的 Location。
    if [ -z "$location" ] && [ "${os:-}" = 'windows' ] && command -v powershell.exe >/dev/null 2>&1; then
        location=$(REMOTE_LATEST_URL=$latest_url \
            powershell.exe -NoProfile -NonInteractive -Command \
            '$ErrorActionPreference = "Stop"; $r = [Net.WebRequest]::Create($env:REMOTE_LATEST_URL); $r.Method = "HEAD"; $r.AllowAutoRedirect = $false; $r.Timeout = 30000; $resp = $r.GetResponse(); try { $resp.Headers["Location"] } finally { $resp.Close() }' \
            2>/dev/null | tr -d '\r') || location=''
    fi

    case "$location" in
        */releases/tag/?*)
            printf '%s' "${location##*/releases/tag/}"
            ;;
    esac
}

# installed_version 打印已安装二进制自报的版本（`remote --version` 输出 "remote-assist <版本>"），
# 未安装或读不出来时什么都不打印。
installed_version() {
    [ -f "$install_path" ] || return 0
    "$install_path" --version 2>/dev/null | sed -n 's/^remote-assist //p' | head -n 1
}

# 发布过的 tag 有带 v 也有不带 v 的（1.0.0 / v1.1.0），比较前统一去掉前导 v。
strip_v() {
    printf '%s' "${1#v}"
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

if [ -n "${REMOTE_INSTALL_DIR:-}" ]; then
    install_dir=$REMOTE_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
    install_dir="${HOME}/.local/bin"
else
    fail "HOME is not set; set REMOTE_INSTALL_DIR to choose an install directory"
fi

install_path="${install_dir}/${command_name}"

# install_release 下载、校验并原子替换 install_path。
install_release() {
    mkdir -p "$install_dir" || fail "cannot create install directory: ${install_dir}"
    temp_file=$(mktemp "${install_dir}/.${command_name}.tmp.XXXXXX") || \
        fail "cannot create a temporary file in: ${install_dir}"

    cleanup() {
        rm -f "$temp_file" "${temp_file}.sums"
    }
    trap cleanup 0 1 2 15

    download_url="${release_base}/${asset}"
    info "Downloading ${asset} (${release_label})..."
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

    # Windows 上正在运行的 exe 不能被替换，mv 会失败——先关掉正在跑的 remote 再重试。
    mv -f "$temp_file" "$install_path" || \
        fail "cannot install to: ${install_path} (is ${command_name} still running?)"
    trap - 0 1 2 15

    info "Installed ${command_name} ${release_label} to ${install_path}"
}

latest_tag=$(resolve_latest_tag)
if [ -n "$latest_tag" ]; then
    # 下载钉到具体 tag 而不是 latest/download：版本比较和下载之间 latest 若恰好换了，
    # 装上去的仍是刚才比较过的那一版，校验清单也来自同一个 Release。
    release_base="https://github.com/${REPOSITORY}/releases/download/${latest_tag}"
    release_label=$latest_tag
else
    release_base="https://github.com/${REPOSITORY}/releases/latest/download"
    release_label='latest'
fi

current_version=$(installed_version)
if [ -n "$current_version" ] && [ -n "$latest_tag" ] && \
    [ "$(strip_v "$current_version")" = "$(strip_v "$latest_tag")" ]; then
    info "${command_name} ${current_version} is already the latest version: ${install_path}"
else
    if [ -n "$current_version" ]; then
        info "Updating ${command_name} ${current_version} -> ${release_label}"
    elif [ -z "$latest_tag" ]; then
        info "Cannot determine the latest release tag; installing whatever 'latest' points to."
    fi
    install_release
fi

case ":${PATH:-}:" in
    *":${install_dir}:"*)
        ;;
    *)
        info "Add ${install_dir} to PATH to run ${command_name} by name."
        ;;
esac

# 装完直接运行。无参数即 share 协助端模式；`curl ... | sh -s -- help --code XXX` 这类
# 写法里 `--` 之后的参数原样透传给 remote。
if [ "${REMOTE_INSTALL_NO_RUN:-}" = '1' ]; then
    exit 0
fi

# `curl | sh` 时脚本自己的 stdin 是管道（脚本正文），remote 必须改接终端；没有可用终端
# （CI、无人值守部署）就不启动——share 会一直挂着等人连，不能把自动化流程卡死。
if ! (exec </dev/tty) 2>/dev/null; then
    info "No terminal available; not starting ${command_name}. Run: ${install_path}"
    exit 0
fi

info "Starting ${command_name}..."
exec "$install_path" "$@" </dev/tty
