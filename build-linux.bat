@echo off
setlocal
rem 用法: build-linux.bat [amd64|arm64]  (不传参默认 amd64)
set GOARCH=%1
if "%GOARCH%"=="" set GOARCH=amd64
if /i not "%GOARCH%"=="amd64" if /i not "%GOARCH%"=="arm64" (
    echo Invalid arch: %GOARCH% ^(expected amd64 or arm64^)
    exit /b 1
)
set GOOS=linux
for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set VERSION=%%v
if "%VERSION%"=="" set VERSION=dev
set LDFLAGS=-X github.com/remote-assist/tool/internal/version.Version=%VERSION%
echo Building for Linux/%GOARCH%... (Version: %VERSION%)
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/relay-linux-%GOARCH% ./cmd/relay
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/remote-linux-%GOARCH% ./cmd/remote
echo Done.
dir bin\
