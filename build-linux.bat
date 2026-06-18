
@echo off
setlocal
set GOOS=linux
set GOARCH=amd64
for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set VERSION=%%v
if "%VERSION%"=="" set VERSION=dev
set LDFLAGS=-X github.com/remote-assist/tool/internal/version.Version=%VERSION%
echo Building for Linux... (Version: %VERSION%)
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/relay-linux ./cmd/relay
"C:\Program Files\Go\bin\go.exe" build -ldflags "%LDFLAGS%" -o bin/remote-linux ./cmd/remote
echo Done.
dir bin\
