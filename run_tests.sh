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
echo "[3/3] Checking code formatting..."
go fmt ./...
if [ $? -ne 0 ]; then
    echo
    echo "Code formatting check FAILED!"
    exit 1
fi

echo
echo "=========================================="
echo "All tests PASSED!"
echo "=========================================="
