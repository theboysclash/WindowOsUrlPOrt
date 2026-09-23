@echo off
REM Builds vmserver.exe next to this file. Needs Go from https://go.dev/dl/
setlocal
cd /d "%~dp0"
set GOTOOLCHAIN=auto
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o vmserver.exe ./cmd/vmserver
if errorlevel 1 (
  echo.
  echo Build failed. Install Go from https://go.dev/dl/ and reopen this window.
  echo Or skip building and use the copy already in release\vmserver.exe
  pause
  exit /b 1
)
echo.
echo Built vmserver.exe
pause
