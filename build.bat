@echo off
echo Building Remote Assist Tool...

if not exist bin mkdir bin

rem 版本号自动取 git describe（tag-距离-g短sha；脏树加 -dirty），注入 ldflags
for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set VERSION=%%v
if "%VERSION%"=="" set VERSION=dev
set LDFLAGS=-X github.com/remote-assist/tool/internal/version.Version=%VERSION%
echo Version: %VERSION%

echo Building relay server...
go build -ldflags "%LDFLAGS%" -o bin/relay.exe ./cmd/relay
if errorlevel 1 (
    echo Failed to build relay
    exit /b 1
)

echo Building remote client...
go build -ldflags "%LDFLAGS%" -o bin/remote.exe ./cmd/remote
if errorlevel 1 (
    echo Failed to build remote
    exit /b 1
)

echo Building GUI...
go build -ldflags "%LDFLAGS%" -o bin/remote-gui.exe ./cmd/gui
if errorlevel 1 (
    echo Failed to build gui
    exit /b 1
)

echo Build complete!
echo Binaries in: %CD%\bin
