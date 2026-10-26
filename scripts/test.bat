@echo off
setlocal
rem Run from the repo root no matter where this is invoked from; setlocal restores the caller's cwd.
cd /d "%~dp0.."
echo ==========================================
echo Remote Assist Tool - Test Runner
echo ==========================================
echo.

echo [1/4] Running unit tests...
go test -v ./internal/...
if errorlevel 1 (
    echo.
    echo Unit tests FAILED!
    exit /b 1
)

echo.
echo [2/4] Running race detector tests...
go test -race -v ./internal/...
if errorlevel 1 (
    echo.
    echo Race detector tests FAILED!
    exit /b 1
)

echo.
echo [3/4] Checking code formatting...
go fmt ./...
if errorlevel 1 (
    echo.
    echo Code formatting check FAILED!
    exit /b 1
)

rem install.sh 是 "curl | sh" 这条安装链上唯一一道校验，go 工具链同样看不到它。
rem 需要 bash（Git for Windows 自带），没有就跳过。
echo.
echo [4/4] Checking install.sh...
where bash >nul 2>nul
if errorlevel 1 (
    echo bash not found - skipping install.sh checks
) else (
    bash tests/install/install_sh_test.sh
    if errorlevel 1 (
        echo.
        echo install.sh checks FAILED!
        exit /b 1
    )
)

echo.
echo ==========================================
echo All tests PASSED!
echo ==========================================
