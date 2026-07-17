#!/bin/bash

echo "=========================================="
echo "Remote Assist Tool - Test Runner"
echo "=========================================="
echo

echo "[1/3] Running unit tests..."
go test -v ./internal/...
if [ $? -ne 0 ]; then
    echo
    echo "Unit tests FAILED!"
    exit 1
fi

echo
echo "[2/3] Running race detector tests..."
go test -race -v ./internal/...
if [ $? -ne 0 ]; then
    echo
    echo "Race detector tests FAILED!"
    exit 1
fi

echo
echo "[3/4] Checking code formatting..."
go fmt ./...
if [ $? -ne 0 ]; then
    echo
    echo "Code formatting check FAILED!"
    exit 1
fi

# GUI 前端的 JS 内嵌在 assets.go 的字符串里，go 工具链完全看不到它。
# 有 node 才跑，没有就跳过（不给纯 Go 项目引入硬依赖）。
echo
echo "[4/4] Checking GUI frontend assets..."
if command -v node >/dev/null 2>&1; then
    node tests/frontend/check_gui_assets.js
    if [ $? -ne 0 ]; then
        echo
        echo "Frontend checks FAILED!"
        exit 1
    fi
else
    echo "node not found - skipping frontend checks"
fi

echo
echo "=========================================="
echo "All tests PASSED!"
echo "=========================================="
