@echo off
setlocal
rem Usage: build-linux.bat [amd64|arm64]  (defaults to amd64)
rem NOTE: keep comments ASCII-only here - cmd.exe reads .bat in the OEM codepage,
rem so UTF-8 Chinese misaligns and can split a comment into commands. See build.bat.
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
rem No VERSIONINFO here: that resource is a Windows PE concept. The .syso files in
rem cmd/* carry a _windows_amd64 suffix, so Go's build constraints skip them for linux.
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/remote-assist-relay-linux-%GOARCH% ./cmd/relay
if errorlevel 1 exit /b 1
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/remote-assist-cli-linux-%GOARCH% ./cmd/remote
if errorlevel 1 exit /b 1
echo Done.
dir bin\
