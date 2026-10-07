@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
set "GAME_PWSH=%ProgramFiles%\PowerShell\7\pwsh.exe"
if exist "%GAME_PWSH%" goto launch
set "GAME_PWSH=pwsh.exe"
where pwsh.exe >nul 2>nul
if errorlevel 1 (
  echo 未找到 PowerShell 7，请先恢复本机的 PowerShell 7。
  pause
  exit /b 1
)
:launch
"%GAME_PWSH%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start_game.ps1" %*
set "GAME_RESULT=%ERRORLEVEL%"
echo.
pause
exit /b %GAME_RESULT%
