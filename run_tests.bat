@echo off
echo ==========================================
echo Remote Assist Tool - Test Runner
echo ==========================================
echo.

echo [1/5] Running unit tests...
go test -v ./internal/...
if errorlevel 1 (
    echo.
    echo Unit tests FAILED!
    exit /b 1
)

echo.
echo [2/5] Running race detector tests...
go test -race -v ./internal/...
if errorlevel 1 (
    echo.
    echo Race detector tests FAILED!
    exit /b 1
)

echo.
echo [3/5] Checking code formatting...
go fmt ./...
if errorlevel 1 (
    echo.
    echo Code formatting check FAILED!
    exit /b 1
)

rem GUI 前端的 JS 内嵌在 assets.go 的字符串里，go 工具链完全看不到它。
rem 有 node 才跑，没有就跳过（不给纯 Go 项目引入硬依赖）。
echo.
echo [4/5] Checking GUI frontend assets...
where node >nul 2>nul
if errorlevel 1 (
    echo node not found - skipping frontend checks
) else (
    node tests/frontend/check_gui_assets.js
    if errorlevel 1 (
        echo.
        echo Frontend checks FAILED!
        exit /b 1
    )
)

rem install.sh 是 "curl | sh" 这条安装链上唯一一道校验，go 工具链同样看不到它。
rem 需要 bash（Git for Windows 自带），没有就跳过。
echo.
echo [5/5] Checking install.sh...
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
