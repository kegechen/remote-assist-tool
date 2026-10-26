#!/bin/bash

# 统一以仓库根目录为工作目录，从任何位置调用都一样（bin/、./internal/... 都按根目录解析）
cd "$(dirname "$0")/.." || exit 1

echo "=========================================="
echo "Remote Assist Tool - Test Runner"
echo "=========================================="
echo

echo "[1/4] Running unit tests..."
go test -v ./internal/...
if [ $? -ne 0 ]; then
    echo
    echo "Unit tests FAILED!"
    exit 1
fi

echo
echo "[2/4] Running race detector tests..."
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

# install.sh 是 "curl | sh" 这条安装链上唯一一道校验，go 工具链同样看不到它。
echo
echo "[4/4] Checking install.sh..."
bash tests/install/install_sh_test.sh
if [ $? -ne 0 ]; then
    echo
    echo "install.sh checks FAILED!"
    exit 1
fi

echo
echo "=========================================="
echo "All tests PASSED!"
echo "=========================================="
