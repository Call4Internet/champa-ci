@echo off
setlocal enabledelayedexpansion

echo ============================================================
echo  champa-client -- Windows Build Script
echo ============================================================
echo.

:: Check Go
where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Go is not installed or not in PATH.
    echo.
    echo Download Go from: https://go.dev/dl/
    echo Install it, then run this script again.
    echo.
    pause
    exit /b 1
)

for /f "tokens=3" %%v in ('go version') do set GOVERSION=%%v
echo [OK] Go found: %GOVERSION%
echo.

:: Download dependencies
echo [1/3] Downloading dependencies...
go mod tidy
if errorlevel 1 (
    echo [ERROR] go mod tidy failed. Check your internet connection.
    pause
    exit /b 1
)
echo [OK] Dependencies ready.
echo.

:: Build Windows exe
echo [2/3] Building champa-client.exe for Windows...
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -ldflags="-s -w" -trimpath -o champa-client.exe .
if errorlevel 1 (
    echo [ERROR] Build failed.
    pause
    exit /b 1
)
echo [OK] champa-client.exe built successfully.
echo.

:: Also build Linux binary (useful if you want to run on a server)
echo [3/3] Building champa-client (Linux amd64)...
set GOOS=linux
set GOARCH=amd64
go build -ldflags="-s -w" -trimpath -o champa-client-linux .
if errorlevel 1 (
    echo [WARN] Linux build failed (Windows binary is still OK).
) else (
    echo [OK] champa-client-linux built successfully.
)

echo.
echo ============================================================
echo  Build complete!
echo.
echo  Files created:
echo    champa-client.exe    -- Windows 64-bit
echo    champa-client-linux  -- Linux 64-bit
echo.
echo  Usage example:
echo    champa-client.exe -pubkey-file server.pub ^
echo        -caches "https://cdn.ampproject.org/,https://amp.cloudflare.com/" ^
echo        -front www.google.com ^
echo        https://your-server.example/champa/ 127.0.0.1:7000
echo.
echo  Monitor session:
echo    curl http://127.0.0.1:7001/status
echo ============================================================
echo.
pause
