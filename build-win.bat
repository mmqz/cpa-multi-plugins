@echo off
setlocal
set "PATH=C:\Users\22879\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin;%PATH%"
set CC=x86_64-w64-mingw32-gcc
set CGO_ENABLED=1
set GOOS=windows
set GOARCH=amd64
set GOPROXY=https://goproxy.cn,direct
set GOSUMDB=sum.golang.google.cn
cd /d %~dp0plugins\qoder
echo === building qoder.dll ===
go build -buildmode=c-shared -o qoder.dll .
if errorlevel 1 goto :err
cd /d %~dp0plugins\workbuddy
echo === building workbuddy.dll ===
go build -buildmode=c-shared -o workbuddy.dll .
if errorlevel 1 goto :err
echo === BUILD OK ===
exit /b 0
:err
echo === BUILD FAILED ===
exit /b 1
