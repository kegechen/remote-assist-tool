@echo off
echo Building Remote Assist Tool...

if not exist bin mkdir bin

echo Building relay server...
go build -o bin/relay.exe ./cmd/relay
if errorlevel 1 (
    echo Failed to build relay
    exit /b 1
)

echo Building remote client...
go build -o bin/remote.exe ./cmd/remote
if errorlevel 1 (
    echo Failed to build remote
    exit /b 1
)

echo Build complete!
echo Binaries in: %CD%\bin
