@echo off
chcp 65001 >nul
call "%~dp0启动服务器.cmd" -OpenClient %*
exit /b %ERRORLEVEL%
