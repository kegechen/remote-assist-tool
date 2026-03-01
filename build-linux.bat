
@echo off
setlocal
set GOOS=linux
set GOARCH=amd64
echo Building for Linux...
"C:\Program Files\Go\bin\go.exe" build -o bin/relay-linux ./cmd/relay
"C:\Program Files\Go\bin\go.exe" build -o bin/remote-linux ./cmd/remote
echo Done.
dir bin\
