@echo off
setlocal enabledelayedexpansion
echo Building Remote Assist Tool...

rem NOTE: keep this file's comments ASCII-only. cmd.exe reads .bat bytes in the OEM
rem codepage (GBK here), so UTF-8 Chinese misaligns and can accidentally produce a
rem `&` / `|` byte that splits the comment into commands. build.sh carries the
rem Chinese explanations; bash reads UTF-8 correctly.

if not exist bin mkdir bin

rem Version comes from git describe (tag-distance-gsha; -dirty when tree is dirty).
for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set VERSION=%%v
if "%VERSION%"=="" set VERSION=dev
set LDFLAGS=-X github.com/remote-assist/tool/internal/version.Version=%VERSION%
echo Version: %VERSION%

rem Windows VERSIONINFO only accepts four plain numbers, so derive them:
rem   0.0.6             -> 0.0.6.0
rem   0.0.6-15-gbe27eca -> 0.0.6.15   (4th = commits since tag)
rem   0.0.6-dirty       -> 0.0.6.0    ("dirty" is not numeric -> 0)
set VMAJ=0
set VMIN=0
set VPAT=0
set VBUILD=0
for /f "tokens=1,2 delims=-" %%a in ("%VERSION%") do (
    set VNUM=%%a
    set VB=%%b
)
echo !VB!| findstr /r "^[0-9][0-9]*$" >nul && set VBUILD=!VB!
for /f "tokens=1,2,3 delims=." %%a in ("!VNUM!") do (
    echo %%a| findstr /r "^[0-9][0-9]*$" >nul && set VMAJ=%%a
    echo %%b| findstr /r "^[0-9][0-9]*$" >nul && set VMIN=%%b
    echo %%c| findstr /r "^[0-9][0-9]*$" >nul && set VPAT=%%c
)
set FILEVER=!VMAJ!.!VMIN!.!VPAT!.!VBUILD!
echo Resource version: %FILEVER%

call :build cmd/relay  remote-assist-relay "relay server"
if errorlevel 1 exit /b 1
call :build cmd/remote remote-assist-cli   "remote client"
if errorlevel 1 exit /b 1
call :build cmd/gui    remote-assist-webui "web console"
if errorlevel 1 exit /b 1

echo Build complete!
echo Binaries in: %CD%\bin
exit /b 0

:build
rem %1=source dir  %2=artifact base name  %3=display label
echo Building %~3...
rem Generate the VERSIONINFO resource (.syso). go build links any .syso sitting in the
rem main package dir, which is what puts ProductName/description/version into the exe's
rem Properties dialog instead of leaving it blank. The _windows_amd64 suffix is a Go
rem build constraint, so cross-compiling to Linux skips it automatically.
rem The tool is pinned via tools.go, so `go mod download` once is enough - no network per build.
go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo -64 ^
    -ver-major !VMAJ! -ver-minor !VMIN! -ver-patch !VPAT! -ver-build !VBUILD! ^
    -product-version "%VERSION%" -file-version "%FILEVER%" ^
    -original-name "%~2-windows-amd64.exe" ^
    -o "%~1/resource_windows_amd64.syso" "%~1/versioninfo.json"
if errorlevel 1 (
    echo Failed to generate version resource for %~3
    exit /b 1
)
go build -ldflags "%LDFLAGS%" -o "bin/%~2-windows-amd64.exe" "./%~1"
if errorlevel 1 (
    echo Failed to build %~3
    exit /b 1
)
exit /b 0
