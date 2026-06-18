#!/bin/bash

echo "Building Remote Assist Tool..."

mkdir -p bin

# 版本号自动取 git describe（tag-距离-g短sha，如 0.0.3-15-gbe27eca；脏树加 -dirty），注入 ldflags
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS="-X github.com/remote-assist/tool/internal/version.Version=${VERSION}"
echo "Version: ${VERSION}"

echo "Building relay server..."
go build -ldflags "${LDFLAGS}" -o bin/relay ./cmd/relay
if [ $? -ne 0 ]; then
    echo "Failed to build relay"
    exit 1
fi

echo "Building remote client..."
go build -ldflags "${LDFLAGS}" -o bin/remote ./cmd/remote
if [ $? -ne 0 ]; then
    echo "Failed to build remote"
    exit 1
fi

echo "Build complete!"
echo "Binaries in: $(pwd)/bin"
