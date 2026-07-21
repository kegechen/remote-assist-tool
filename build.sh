#!/bin/bash

echo "Building Remote Assist Tool..."

mkdir -p bin

# 版本号自动取 git describe（tag-距离-g短sha，如 0.0.3-15-gbe27eca；脏树加 -dirty），注入 ldflags
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS="-X github.com/remote-assist/tool/internal/version.Version=${VERSION}"
echo "Version: ${VERSION}"

# Windows 的 VERSIONINFO 资源只认四段纯数字，git describe 那串塞不进去，得换算：
#   0.0.6              -> 0.0.6.0
#   0.0.6-15-gbe27eca  -> 0.0.6.15   （第 4 段 = 距 tag 的提交数）
#   0.0.6-dirty        -> 0.0.6.0    （dirty 不是数字，按 0 处理）
VNUM=${VERSION%%-*}
VBUILD=$(echo "${VERSION}" | cut -s -d- -f2)
case "${VBUILD}" in ''|*[!0-9]*) VBUILD=0 ;; esac
VMAJ=$(echo "${VNUM}" | cut -d. -f1)
VMIN=$(echo "${VNUM}" | cut -s -d. -f2)
VPAT=$(echo "${VNUM}" | cut -s -d. -f3)
case "${VMAJ}" in ''|*[!0-9]*) VMAJ=0 ;; esac
case "${VMIN}" in ''|*[!0-9]*) VMIN=0 ;; esac
case "${VPAT}" in ''|*[!0-9]*) VPAT=0 ;; esac

# 产物名自带 os-arch：remote.exe 这种泛名容易跟 PATH 里别的东西撞，也看不出给谁跑的
GOOS_V=$(go env GOOS)
GOARCH_V=$(go env GOARCH)
SUF="${GOOS_V}-${GOARCH_V}"
EXT=""
[ "${GOOS_V}" = "windows" ] && EXT=".exe"

# genversioninfo 生成 Windows 的 VERSIONINFO 资源（.syso），go build 会自动把同目录下的
# .syso 链进 main 包——于是右键属性能看到产品名/描述/版本，而不是一个信息全空的 exe。
# 文件名带 _windows_<arch> 后缀，Go 按文件名做构建约束，交叉编译 Linux 时自动不链。
# 工具已用 tools.go 钉进 go.mod：`go mod download` 一次之后离线可构建（无需每次联网）。
genversioninfo() {
    [ "${GOOS_V}" = "windows" ] || return 0
    local dir=$1 name=$2
    # .syso 的 COFF 机器类型由架构旗标决定：-64 生成 AMD64（0x8664）。ARM64 必须 -arm -64，
    # 否则仍产出 AMD64 目标文件、却存成 resource_windows_arm64.syso，链接 arm64 时机器类型
    # 不匹配而失败。archflags 不加引号，好让 arm64 分支词分成两个参数。
    local archflags="-64"
    [ "${GOARCH_V}" = "arm64" ] && archflags="-arm -64"
    go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo \
        ${archflags} \
        -ver-major "${VMAJ}" -ver-minor "${VMIN}" -ver-patch "${VPAT}" -ver-build "${VBUILD}" \
        -product-version "${VERSION}" \
        -file-version "${VMAJ}.${VMIN}.${VPAT}.${VBUILD}" \
        -original-name "${name}-${SUF}${EXT}" \
        -o "${dir}/resource_windows_${GOARCH_V}.syso" \
        "${dir}/versioninfo.json"
}

build() {
    local dir=$1 name=$2 label=$3
    echo "Building ${label}..."
    if ! genversioninfo "${dir}" "${name}"; then
        echo "Failed to generate version resource for ${label}"
        exit 1
    fi
    go build -ldflags "${LDFLAGS}" -o "bin/${name}-${SUF}${EXT}" "./${dir}"
    if [ $? -ne 0 ]; then
        echo "Failed to build ${label}"
        exit 1
    fi
}

build cmd/relay  remote-assist-relay "relay server"
build cmd/remote remote-assist-cli   "remote client"
build cmd/gui    remote-assist-webui "web console"

echo "Build complete!"
echo "Binaries in: $(pwd)/bin"
