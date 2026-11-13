@echo off
rem Install tmvp on Windows (cmd, no PowerShell needed). Re-run to upgrade.
rem   curl -fsSL -o install.cmd https://raw.githubusercontent.com/muthuishere/tmvp/main/install.cmd && install.cmd
rem Options: set TMVP_VERSION=v0.1.0 (default latest), set TMVP_BIN_DIR=... (default %USERPROFILE%\.local\bin)
setlocal enabledelayedexpansion
if "%TMVP_REPO%"=="" set "TMVP_REPO=muthuishere/tmvp"
if "%TMVP_BIN_DIR%"=="" (set "DEST=%USERPROFILE%\.local\bin") else (set "DEST=%TMVP_BIN_DIR%")
if "%TMVP_VERSION%"=="" (set "BASE=https://github.com/%TMVP_REPO%/releases/latest/download") else (set "BASE=https://github.com/%TMVP_REPO%/releases/download/%TMVP_VERSION%")
if not "%TMVP_BASE_URL%"=="" set "BASE=%TMVP_BASE_URL%"
set "ARCH=amd64"
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "ARCH=arm64"
set "ASSET=tmvp_windows_%ARCH%.exe"
set "TMP=%TEMP%\tmvp-install-%RANDOM%"
mkdir "%TMP%" || exit /b 1
curl.exe -fsSL "%BASE%/%ASSET%" -o "%TMP%\%ASSET%" || exit /b 1
curl.exe -fsSL "%BASE%/checksums.txt" -o "%TMP%\checksums.txt" || exit /b 1
set "WANT="
for /f "tokens=1,2" %%a in (%TMP%\checksums.txt) do if "%%b"=="%ASSET%" set "WANT=%%a"
set "GOT="
for /f "skip=1 delims=" %%h in ('certutil -hashfile "%TMP%\%ASSET%" SHA256') do if not defined GOT set "GOT=%%h"
set "GOT=!GOT: =!"
if /i not "!GOT!"=="!WANT!" (echo tmvp: checksum mismatch for %ASSET%; not installed & rmdir /s /q "%TMP%" & exit /b 1)
if not exist "%DEST%" mkdir "%DEST%"
move /y "%TMP%\%ASSET%" "%DEST%\tmvp.exe" >nul || exit /b 1
rmdir /s /q "%TMP%"
"%DEST%\tmvp.exe" version
echo next: tmvp skill install --discover, then in your product repo ask your agent: /tmvp-discoverer
echo tmvp installed in %DEST% - add it to PATH if it is not already
