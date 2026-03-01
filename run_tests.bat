@echo off
echo ==========================================
echo Remote Assist Tool - Test Runner
echo ==========================================
echo.

echo [1/3] Running unit tests...
go test -v ./internal/...
if errorlevel 1 (
    echo.
    echo Unit tests FAILED!
    exit /b 1
)

echo.
echo [2/3] Running race detector tests...
go test -race -v ./internal/...
if errorlevel 1 (
    echo.
    echo Race detector tests FAILED!
    exit /b 1
)

echo.
echo [3/3] Checking code formatting...
go fmt ./...
if errorlevel 1 (
    echo.
    echo Code formatting check FAILED!
    exit /b 1
)

echo.
echo ==========================================
echo All tests PASSED!
echo ==========================================
