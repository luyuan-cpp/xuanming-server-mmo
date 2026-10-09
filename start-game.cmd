@echo off
chcp 65001 >nul
call "%~dp0start-server.cmd" -OpenClient %*
exit /b %ERRORLEVEL%
