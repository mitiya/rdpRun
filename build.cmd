@echo off
REM build.cmd - Build rdprun binaries for Windows and Linux (amd64).
REM
REM Requires the local cgo-free patch of tomatome/grdp (see go.mod `replace`).
REM Both targets are built with CGO_ENABLED=0 so no C toolchain is needed and
REM the Linux binary cross-compiles cleanly from Windows.

setlocal

REM Always build with cgo disabled (pure Go).
set CGO_ENABLED=0
set GOFLAGS=-mod=mod

echo Building Windows amd64 binary ...
set GOOS=windows
set GOARCH=amd64
go build -o rdprun.exe .
if errorlevel 1 (
    echo Windows build FAILED
    exit /b 1
)
echo   -^> rdprun.exe

echo Building Linux amd64 binary ...
set GOOS=linux
set GOARCH=amd64
go build -o rdprun-linux-amd64 .
if errorlevel 1 (
    echo Linux build FAILED
    exit /b 1
)
echo   -^> rdprun-linux-amd64

REM Restore the current shell's target in case this is sourced.
set GOOS=windows
set GOARCH=amd64

echo.
echo Build complete:
echo   rdprun.exe            (Windows amd64)
echo   rdprun-linux-amd64    (Linux amd64)
endlocal