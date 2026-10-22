@echo off
setlocal
pushd "%~dp0"

where go >nul 2>nul
if errorlevel 1 (
    echo ERROR: Go was not found in PATH. Install Go 1.27.2 or newer and reopen this terminal.
    popd
    exit /b 1
)

set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"
if not exist "dist" mkdir "dist"

echo Building AudioMixer for Windows x64...
go build -tags production -trimpath -ldflags="-s -w -H windowsgui" -o "dist\AudioMixer.exe" .
if errorlevel 1 (
    echo Build failed.
    popd
    exit /b 1
)

echo Build complete: %CD%\dist\AudioMixer.exe
popd
endlocal
