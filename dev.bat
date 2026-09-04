@echo off
setlocal enabledelayedexpansion
cd /d "%~dp0"

rem Runtime artifacts (logs, pid files) live under run/, separate from build
rem outputs in bin/. See docs/ops/log-management.md.
set "GO_LOG=run\logs\go_services"
set "CPP_LOG=run\logs\cpp_nodes"
set "SATOKEN_TITLE=sa-token"
set "SATOKEN_DIR=java\springboot_satoken_auth_starter"
set "SATOKEN_LOG=run\logs\sa_token.log"
set "SATOKEN_PID=run\pids\sa_token.pid"
set "PS=pwsh -NoProfile -ExecutionPolicy Bypass"

if "%~1"=="" goto :menu

set "CMD=%~1"
if /I "%CMD%"=="start"     goto :start
if /I "%CMD%"=="start-multi" goto :start_multi
if /I "%CMD%"=="start-zones" goto :start_zones
if /I "%CMD%"=="start-cpp" goto :start_cpp
if /I "%CMD%"=="start-go"  goto :start_go
if /I "%CMD%"=="start-satoken" goto :start_satoken
if /I "%CMD%"=="stop-satoken"  goto :stop_satoken
if /I "%CMD%"=="stop"      goto :stop
if /I "%CMD%"=="status"    goto :status
if /I "%CMD%"=="restart"   goto :restart
if /I "%CMD%"=="logs"      goto :logs
if /I "%CMD%"=="robot"      goto :robot
if /I "%CMD%"=="robot-zones" goto :robot_zones
if /I "%CMD%"=="infra"      goto :infra
if /I "%CMD%"=="infra-down"  goto :infra_down
if /I "%CMD%"=="build"      goto :build
if /I "%CMD%"=="proto"      goto :proto
if /I "%CMD%"=="export"     goto :export
if /I "%CMD%"=="gen"        goto :gen
if /I "%CMD%"=="clean-logs" goto :clean_logs
if /I "%CMD%"=="ui"         goto :ui
if /I "%CMD%"=="ui-cpp"     goto :ui_cpp
if /I "%CMD%"=="ui-go"      goto :ui_go
if /I "%CMD%"=="help"       goto :help

echo Unknown command: %CMD%
echo.
goto :help

:: ================================================================
::  Double-click / no-args: interactive numbered menu
:: ================================================================
:menu
echo.
echo   ========================================
echo     MMORPG Dev Server
echo   ========================================
echo.
echo     1.  Start all   (build Go + launch C++ and Go)
echo     2.  Start C++   (gate + scene + battle only)
echo     3.  Start Go    (build + launch Go services only)
echo     4.  Stop all
echo     5.  Status
echo     6.  Restart     (stop + build + start)
echo     7.  Logs        (pick a process to tail)
echo     8.  Logs all    (dump last 60 lines)
echo     9.  Robot       (build + run load-test client)
echo     10. Infra       (docker-compose up: Kafka/Redis/MySQL/etcd)
echo     11. Infra down  (docker-compose down)
echo     12. Build       (compile C++ + Go, no launch)
echo     13. Proto       (regenerate proto code)
echo     14. Export      (Excel data table export)
echo     15. Gen         (Export tables + regenerate proto)
echo     16. Clean logs  (delete all log files)
echo     17. UI          (mprocs TUI dashboard)
echo     18. Start SA-Token  (local Spring Boot auth app)
echo     19. Stop SA-Token
echo     20. Start multi (2 gate + 4 scene + login*2 + scene_mgr*2 + locator*2 + data*2)
echo     21. Start zones (2 zones x 2 gate + 4 scene + per-zone Go services)
echo     22. Robot zones (parallel robot.exe per zone for two-zone stress)
echo.
echo     0.  Exit
echo.
set /p "CHOICE=  Pick [0-22]: "

if "%CHOICE%"=="0" exit /b 0

if "%CHOICE%"=="1"  call :start      & goto :menu_return
if "%CHOICE%"=="2"  call :start_cpp  & goto :menu_return
if "%CHOICE%"=="3"  call :start_go   & goto :menu_return
if "%CHOICE%"=="4"  call :stop       & goto :menu_return
if "%CHOICE%"=="5"  call :status     & goto :menu_return
if "%CHOICE%"=="6"  call :restart    & goto :menu_return
if "%CHOICE%"=="7"  call :logs       & goto :menu_return
if "%CHOICE%"=="8"  call :logs_all   & goto :menu_return
if "%CHOICE%"=="9"  call :robot      & goto :menu_return
if "%CHOICE%"=="10" call :infra      & goto :menu_return
if "%CHOICE%"=="11" call :infra_down & goto :menu_return
if "%CHOICE%"=="12" call :build      & goto :menu_return
if "%CHOICE%"=="13" call :proto      & goto :menu_return
if "%CHOICE%"=="14" call :export     & goto :menu_return
if "%CHOICE%"=="15" call :gen        & goto :menu_return
if "%CHOICE%"=="16" call :clean_logs  & goto :menu_return
if "%CHOICE%"=="17" call :ui          & goto :menu_return
if "%CHOICE%"=="18" call :start_satoken & goto :menu_return
if "%CHOICE%"=="19" call :stop_satoken  & goto :menu_return
if "%CHOICE%"=="20" call :start_multi   & goto :menu_return
if "%CHOICE%"=="21" call :start_zones   & goto :menu_return
if "%CHOICE%"=="22" call :robot_zones   & goto :menu_return

echo   Invalid choice.
goto :menu

:menu_return
echo.
echo   Press any key to return to menu...
pause >nul
goto :menu

:: ================================================================
:start
echo [1/2] Building Go executables...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Build failed. & pause & exit /b 1 )
echo.
echo [2/2] Starting C++ nodes + Go services...
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-start-exe -GateCount 2 -SceneCount 4
if errorlevel 1 ( echo Start failed. & pause & exit /b 1 )
echo.
echo Done. Run "dev status" to check.
exit /b 0

:: ================================================================
::  Multi-instance launch: every Go app-tier service runs 2 copies. The
::  tier-staged launcher in go_services.ps1 ensures lower tiers (db,
::  data_service, player_locator) are LISTEN before login/scene_manager
::  start, avoiding first-boot dial races. This is a dev-only convenience;
::  it does NOT change runtime discovery (etcd) or break rolling/canary
::  deploys -- those still work because clients re-resolve via etcd.
::  Override counts: dev start-multi "login=3,scene_manager=2"
:: ================================================================
:start_multi
set "GO_COUNTS=%~2"
if "%GO_COUNTS%"=="" set "GO_COUNTS=login=2,scene_manager=2,player_locator=2,data_service=2"
echo [1/2] Building Go executables...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Build failed. & pause & exit /b 1 )
echo.
echo [2/2] Starting (multi-instance, tier-staged): GoCounts=%GO_COUNTS%
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-start-exe -GateCount 2 -SceneCount 4 -GoCounts "%GO_COUNTS%"
if errorlevel 1 ( echo Start failed. & pause & exit /b 1 )
echo.
echo Done. Run "dev status" to check.
exit /b 0

:: ================================================================
::  Multi-zone local stress launch:
::    dev start-zones                    -> zones 1,2 (default), 2 gate + 4 scene each
::    dev start-zones "1,2,3"            -> custom zone list
::    dev start-zones "1,2" "login=2"     -> custom zones + per-service counts inside each zone
::
::  Per zone:
::    * Go services get a derived yaml with ZoneId rewritten and ListenOn port
::      shifted by (zone-1)*1000 (instance keys: z<zone>_<svc>[_<i>]).
::    * C++ gate/scene inherit ZONE_ID=<zone> env var (instance keys: z<zone>_<node>[_<i>]).
::    * Shared infra (etcd/Redis/Kafka/MySQL) is reused. Per-zone MySQL DB
::      schemas must already exist (see deploy/mysql-init/00_init_zone_dbs.sql).
:: ================================================================
:start_zones
set "ZONE_LIST=%~2"
if "%ZONE_LIST%"=="" set "ZONE_LIST=1,2"
set "GO_COUNTS=%~3"
echo [1/2] Building Go executables...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Build failed. & pause & exit /b 1 )
echo.
echo [2/2] Starting zones [%ZONE_LIST%] (2 gate + 4 scene per zone) ...
if "%GO_COUNTS%"=="" (
    %PS% -File tools\scripts\dev_tools.ps1 -Command dev-start-zones -Zones %ZONE_LIST% -GateCount 2 -SceneCount 4
) else (
    %PS% -File tools\scripts\dev_tools.ps1 -Command dev-start-zones -Zones %ZONE_LIST% -GateCount 2 -SceneCount 4 -GoCounts "%GO_COUNTS%"
)
if errorlevel 1 ( echo Start failed. & pause & exit /b 1 )
echo.
echo Done. Run "dev status" to check.
exit /b 0

:: ================================================================
:start_cpp
echo Starting C++ nodes (2 gate + 4 scene + 1 battle)...
%PS% -File tools\scripts\dev_tools.ps1 -Command cpp-node-start -GateCount 2 -SceneCount 4 -BattleCount 1
if errorlevel 1 ( echo Start C++ failed. & pause & exit /b 1 )
echo C++ nodes started.
exit /b 0

:: ================================================================
:start_go
echo [1/2] Building Go executables...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Build failed. & pause & exit /b 1 )
echo.
echo [2/2] Starting Go services (exe)...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-start-exe
if errorlevel 1 ( echo Start Go failed. & pause & exit /b 1 )
echo Go services started.
exit /b 0

:: ================================================================
:stop
echo Stopping all local services...
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-stop
if errorlevel 1 ( echo Stop failed. & pause & exit /b 1 )
echo All services stopped.
exit /b 0

:: ================================================================
:status
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-status
echo.
call :satoken_status
exit /b 0

:: ================================================================
:restart
echo [1/2] Stopping...
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-stop
if errorlevel 1 ( echo Stop failed. & pause & exit /b 1 )
echo.
echo [2/2] Building + Starting...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Build failed. & pause & exit /b 1 )
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-start-exe -GateCount 2 -SceneCount 4
if errorlevel 1 ( echo Start failed. & pause & exit /b 1 )
echo Restarted. Run "dev status" to check.
exit /b 0

:: ================================================================
::  dev logs          -> interactive menu to pick a process
::  dev logs login    -> open new window tailing login's log
::  dev logs all      -> dump last 60 lines of every process
:: ================================================================
:logs
set "PROC=%~2"

:: --- "dev logs all": dump everything inline ---
if /I "%PROC%"=="all" goto :logs_all

:: --- "dev logs <name>": open new window tailing that process ---
if not "%PROC%"=="" goto :logs_tail

:: --- "dev logs": interactive menu ---
echo.
echo   Available processes:
echo.

set /a N=0

:: Go services
if exist "%GO_LOG%" (
    for %%F in ("%GO_LOG%\*.stderr.log") do (
        set "BASENAME=%%~nF"
        set "BASENAME=!BASENAME:.stderr=!"
        set /a N+=1
        set "P[!N!]=go:!BASENAME!"
        echo     !N!  !BASENAME!  [go]
    )
)

:: C++ nodes
if exist "%CPP_LOG%" (
    for %%F in ("%CPP_LOG%\*.stderr.log") do (
        set "BASENAME=%%~nF"
        set "BASENAME=!BASENAME:.stderr=!"
        set /a N+=1
        set "P[!N!]=cpp:!BASENAME!"
        echo     !N!  !BASENAME!  [cpp]
    )
)

if %N%==0 (
    echo   No log files found.
    pause & exit /b 1
)

echo.
echo     0  (all ^- dump last 60 lines inline)
echo.
set /p "PICK=Pick a number: "

if "%PICK%"=="0" goto :logs_all

set "SEL=!P[%PICK%]!"
if "%SEL%"=="" (
    echo Invalid selection.
    pause & exit /b 1
)

:: Parse "go:login" or "cpp:gate_1"
for /f "tokens=1,2 delims=:" %%A in ("%SEL%") do (
    set "KIND=%%A"
    set "PROC=%%B"
)
goto :logs_tail

:: --- Open a new cmd window tailing the process log ---
:logs_tail
:: Find the log file
set "LOGFILE="
if exist "%GO_LOG%\%PROC%.stderr.log"  set "LOGFILE=%GO_LOG%\%PROC%.stderr.log"
if exist "%CPP_LOG%\%PROC%.stderr.log" set "LOGFILE=%CPP_LOG%\%PROC%.stderr.log"

if "%LOGFILE%"=="" (
    echo No log found for "%PROC%".
    echo.
    echo Available: dir /b "%GO_LOG%\*.stderr.log" "%CPP_LOG%\*.stderr.log" 2^>nul
    pause & exit /b 1
)

echo Opening log window for: %PROC%
echo   %LOGFILE%
start "%PROC% log" cmd /k "title %PROC% log & powershell -NoProfile -Command \"Get-Content '%CD%\%LOGFILE%' -Tail 100 -Wait\""
exit /b 0

:: --- Dump all logs inline (original behavior) ---
:logs_all
echo ===== Go service logs =====
if exist "%GO_LOG%" (
    for %%F in ("%GO_LOG%\*.stderr.log") do (
        echo.
        echo --- %%~nxF ---
        powershell -NoProfile -Command "Get-Content '%%~fF' -Tail 60"
    )
) else ( echo No Go logs found. )

echo.
echo ===== C++ node logs =====
if exist "%CPP_LOG%" (
    for %%F in ("%CPP_LOG%\*.stderr.log") do (
        echo.
        echo --- %%~nxF ---
        powershell -NoProfile -Command "Get-Content '%%~fF' -Tail 60"
    )
) else ( echo No C++ logs found. )
echo.
exit /b 0

:: ================================================================
:start_satoken
if not exist "%SATOKEN_DIR%\pom.xml" (
    echo SA-Token app not found: %SATOKEN_DIR%
    pause & exit /b 1
)
where java >nul 2>&1
if errorlevel 1 (
    echo Java not found. Please install JDK 21+ and reopen the terminal.
    echo Suggested: winget install EclipseAdoptium.Temurin.21.JDK
    pause & exit /b 1
)
set "MAVEN_CMD="
if exist "%CD%\tools\apache-maven\bin\mvn.cmd" (
    set "MAVEN_CMD=%CD%\tools\apache-maven\bin\mvn.cmd"
) else if exist "%SATOKEN_DIR%\mvnw.cmd" (
    set "MAVEN_CMD=%CD%\%SATOKEN_DIR%\mvnw.cmd"
) else (
    where mvn >nul 2>&1
    if errorlevel 1 (
        echo Maven not found, so SA-Token cannot start yet.
        echo.
        echo Quick fix:
        echo   winget install Apache.Maven
        echo   or place a local Maven at tools\apache-maven\bin\mvn.cmd
        echo.
        echo Then reopen the terminal and verify:
        echo   mvn -v
        pause & exit /b 1
    )
    set "MAVEN_CMD=mvn"
)
%PS% -Command "$pidFile='%CD%\%SATOKEN_PID%'; if (Test-Path $pidFile) { $procId=Get-Content $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1; if ($procId -and (Get-Process -Id $procId -ErrorAction SilentlyContinue)) { exit 0 } else { Remove-Item $pidFile -Force -ErrorAction SilentlyContinue; exit 1 } } else { exit 1 }"
if not errorlevel 1 (
    echo SA-Token is already running.
    exit /b 0
)
if not exist "run\logs" mkdir "run\logs"
if not exist "run\pids" mkdir "run\pids"
echo Starting SA-Token dev server...
%PS% -Command "$wd='%CD%\%SATOKEN_DIR%'; $log='%CD%\%SATOKEN_LOG%'; $pidFile='%CD%\%SATOKEN_PID%'; $maven='%MAVEN_CMD%'; $cmd='call "' + $maven + '" spring-boot:run >> "' + $log + '" 2>&1'; $p=Start-Process -FilePath 'cmd.exe' -ArgumentList '/c',$cmd -WorkingDirectory $wd -WindowStyle Hidden -PassThru; Set-Content -Path $pidFile -Value $p.Id"
if errorlevel 1 ( echo SA-Token start failed. & pause & exit /b 1 )
echo SA-Token launched in background.
echo Log: %SATOKEN_LOG%
echo API base: http://localhost:18080/auth
exit /b 0

:: ================================================================
:stop_satoken
%PS% -Command "$pidFile='%CD%\%SATOKEN_PID%'; if (!(Test-Path $pidFile)) { exit 1 }; $procId=Get-Content $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1; if ($procId -and (Get-Process -Id $procId -ErrorAction SilentlyContinue)) { Stop-Process -Id $procId -Force }; Remove-Item $pidFile -Force -ErrorAction SilentlyContinue"
if errorlevel 1 (
    echo SA-Token is not running.
    exit /b 0
)
echo SA-Token stopped.
exit /b 0

:: ================================================================
:satoken_status
echo ===== SA-Token status =====
%PS% -Command "$pidFile='%CD%\%SATOKEN_PID%'; if (Test-Path $pidFile) { $procId=Get-Content $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1; if ($procId -and (Get-Process -Id $procId -ErrorAction SilentlyContinue)) { Write-Host ('RUNNING  PID=' + $procId + '  LOG=%SATOKEN_LOG%'); exit 0 } else { Remove-Item $pidFile -Force -ErrorAction SilentlyContinue } }; Write-Host 'STOPPED'"
exit /b 0

:: ================================================================
:robot
set "ROBOT_EXE=robot\robot.exe"
set "ROBOT_CFG=%~2"
if "%ROBOT_CFG%"=="" set "ROBOT_CFG=robot\etc\robot.yaml"

echo Building robot...
pushd robot
go build -o robot.exe .
if errorlevel 1 ( echo Robot build failed. & popd & pause & exit /b 1 )
popd
echo.
echo Starting robot  [config: %ROBOT_CFG%]
start "robot" cmd /k "title robot ^& \"%CD%\%ROBOT_EXE%\" -c \"%CD%\%ROBOT_CFG%\""
echo Robot launched in new window.
exit /b 0

:: ================================================================
::  robot-zones  -> launch one robot.exe per zone, each in its own console
::    dev robot-zones              -> zones 1,2
::    dev robot-zones "1,2,3"      -> custom zone list
::  Each zone's robot reads run/etc/robot/robot.z<N>.yaml (generated from
::  robot/etc/robot.yaml with zone_id rewritten and account_fmt prefixed).
:: ================================================================
:robot_zones
set "ZONE_LIST=%~2"
if "%ZONE_LIST%"=="" set "ZONE_LIST=1,2"
echo Launching robot per zone [%ZONE_LIST%] ...
%PS% -File tools\scripts\dev_tools.ps1 -Command dev-robot-zones -Zones %ZONE_LIST%
if errorlevel 1 ( echo robot-zones failed. & pause & exit /b 1 )
exit /b 0

:: ================================================================
:infra
echo Starting local infrastructure (docker-compose)...
docker compose -f deploy\docker-compose.yml up -d
if errorlevel 1 ( echo docker-compose up failed. & pause & exit /b 1 )
echo.
echo Infrastructure running (Kafka, Redis, MySQL, etcd).
exit /b 0

:: ================================================================
:infra_down
echo Stopping local infrastructure...
docker compose -f deploy\docker-compose.yml down
if errorlevel 1 ( echo docker-compose down failed. & pause & exit /b 1 )
echo Infrastructure stopped.
exit /b 0

:: ================================================================
:build
echo [1/2] Building C++ (game.sln Debug x64)...
set "MSBUILD_EXE="

:: Use vswhere to find MSBuild dynamically (works with any VS edition/install path).
for /f "usebackq delims=" %%i in (`"%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find "MSBuild\**\Bin\amd64\MSBuild.exe" 2^>nul`) do set "MSBUILD_EXE=%%i"
if not defined MSBUILD_EXE for /f "usebackq delims=" %%i in (`"%ProgramFiles%\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find "MSBuild\**\Bin\amd64\MSBuild.exe" 2^>nul`) do set "MSBUILD_EXE=%%i"

:: Fallback: check well-known hardcoded paths (BuildTools, Enterprise, Community, Professional).
if not defined MSBUILD_EXE if exist "C:\Program Files\Microsoft Visual Studio\18\BuildTools\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=C:\Program Files\Microsoft Visual Studio\18\BuildTools\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "C:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=C:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "C:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=C:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "C:\Program Files\Microsoft Visual Studio\18\Professional\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=C:\Program Files\Microsoft Visual Studio\18\Professional\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "E:\Program Files\Microsoft Visual Studio\18\BuildTools\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=E:\Program Files\Microsoft Visual Studio\18\BuildTools\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "E:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=E:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\amd64\MSBuild.exe"
if not defined MSBUILD_EXE if exist "E:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\amd64\MSBuild.exe" set "MSBUILD_EXE=E:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\amd64\MSBuild.exe"

if not defined MSBUILD_EXE (
    echo.
    echo MSBuild not found. Install Visual Studio 2026 with C++ workload
    echo or BuildTools, and ensure vswhere.exe is available.
    pause & exit /b 1
)

echo Using MSBuild: %MSBUILD_EXE%
"%MSBUILD_EXE%" game.sln /m /p:Configuration=Debug /p:Platform=x64 /v:minimal
if errorlevel 1 (
    echo.
    echo C++ build failed. Check errors above.
    pause & exit /b 1
)
echo.
echo [2/2] Building Go service executables...
%PS% -File tools\scripts\dev_tools.ps1 -Command go-svc-build
if errorlevel 1 ( echo Go build failed. & pause & exit /b 1 )
echo.
echo All builds succeeded.
exit /b 0

:: ================================================================
:prepare_protoc
set "REPO_PROTOC_DIR=%~dp0third_party\grpc\install_vs2026_dbg\bin"
set "REPO_PROTOC=%REPO_PROTOC_DIR%\protoc.exe"
if not exist "%REPO_PROTOC%" (
    echo Required protoc was not found: "%REPO_PROTOC%"
    exit /b 1
)
"%REPO_PROTOC%" --version 2>nul | findstr /x /c:"libprotoc 35.1" >nul
if errorlevel 1 (
    echo Repository generation requires libprotoc 35.1. Found:
    "%REPO_PROTOC%" --version
    exit /b 1
)
set "PATH=%REPO_PROTOC_DIR%;%PATH%"
exit /b 0

:: ================================================================
:proto
setlocal DisableDelayedExpansion
call :prepare_protoc
if errorlevel 1 (
    pause
    endlocal & exit /b 1
)
echo Using protoc: "%REPO_PROTOC%"
"%REPO_PROTOC%" --version
echo [1/2] Building proto generator...
%PS% -File tools\scripts\dev_tools.ps1 -Command proto-gen-build
if errorlevel 1 (
    echo Proto generator build failed.
    pause
    endlocal & exit /b 1
)
echo.
echo [2/2] Regenerating proto code...
%PS% -File tools\scripts\dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools\proto_generator\protogen\etc\proto_gen.yaml
if errorlevel 1 (
    echo Proto generation failed.
    pause
    endlocal & exit /b 1
)
echo Proto generation complete.
endlocal & exit /b 0

:: ================================================================
:gen
echo [1/2] Exporting data tables...
call :export
if errorlevel 1 ( echo Export step failed. & exit /b 1 )
echo.
echo [2/2] Regenerating proto code...
call :proto
if errorlevel 1 ( echo Proto step failed. & exit /b 1 )
echo.
echo All generation complete (tables + proto).
exit /b 0

:: ================================================================
:export
setlocal DisableDelayedExpansion
set "EXPORT_CFG=%~2"
if not defined EXPORT_CFG set "EXPORT_CFG=tools\data_table_exporter\exporter_config.yaml"

set "EXPORT_PYTHON="
if defined PYTHON_EXE set "EXPORT_PYTHON=%PYTHON_EXE:"=%"
if not defined EXPORT_PYTHON (
    py -3 -c "import sys" >nul 2>&1
    if not errorlevel 1 (
        for /f "delims=" %%I in ('py -3 -c "import sys; print(sys.executable)" 2^>nul') do set "EXPORT_PYTHON=%%I"
    )
)
if not defined EXPORT_PYTHON (
    python -c "import sys" >nul 2>&1
    if not errorlevel 1 (
        for /f "delims=" %%I in ('python -c "import sys; print(sys.executable)" 2^>nul') do set "EXPORT_PYTHON=%%I"
    )
)
if not defined EXPORT_PYTHON (
    echo Python 3 was not found. Install Python 3 or make the py launcher available.
    pause
    endlocal & exit /b 1
)
"%EXPORT_PYTHON%" -c "import sys; raise SystemExit(0 if sys.version_info.major == 3 else 1)" >nul 2>&1
if errorlevel 1 (
    echo Python 3 is not usable: "%EXPORT_PYTHON%"
    pause
    endlocal & exit /b 1
)
call :prepare_protoc
if errorlevel 1 (
    pause
    endlocal & exit /b 1
)

echo Using Python: "%EXPORT_PYTHON%"
"%EXPORT_PYTHON%" --version
echo Using protoc: "%REPO_PROTOC%"
"%REPO_PROTOC%" --version
echo Installing Python dependencies...
"%EXPORT_PYTHON%" -m pip install -q -r tools\data_table_exporter\requirements.txt
if errorlevel 1 (
    echo Python dependency installation failed.
    pause
    endlocal & exit /b 1
)
echo Running data table exporter  [config: "%EXPORT_CFG%"]
"%EXPORT_PYTHON%" tools\data_table_exporter\run.py "%EXPORT_CFG%"
if errorlevel 1 (
    echo Export failed.
    pause
    endlocal & exit /b 1
)
echo Data table export complete.
endlocal & exit /b 0

:: ================================================================
:clean_logs
echo Cleaning log files...
if exist "%GO_LOG%" (
    del /q "%GO_LOG%\*" 2>nul
    echo   Cleared: %GO_LOG%
) else ( echo   No Go logs to clean. )
if exist "%CPP_LOG%" (
    del /q "%CPP_LOG%\*" 2>nul
    echo   Cleared: %CPP_LOG%
) else ( echo   No C++ logs to clean. )
echo Done.
exit /b 0

:: ================================================================
::  mprocs TUI dashboard — all processes in one terminal with UI
:: ================================================================
:ui
set "MPROCS_CFG=tools\dev\mprocs.2g4s.yaml"
if not "%~2"=="" set "MPROCS_CFG=%~2"
where mprocs >nul 2>&1
if errorlevel 1 (
    echo mprocs not found. Install with: winget install mprocs
    pause & exit /b 1
)
echo Launching mprocs TUI  [config: %MPROCS_CFG%]
mprocs -c "%MPROCS_CFG%"
exit /b 0

:ui_cpp
where mprocs >nul 2>&1
if errorlevel 1 ( echo mprocs not found. Install: winget install mprocs & pause & exit /b 1 )
mprocs -c tools\dev\mprocs.cpp-only.yaml
exit /b 0

:ui_go
where mprocs >nul 2>&1
if errorlevel 1 ( echo mprocs not found. Install: winget install mprocs & pause & exit /b 1 )
mprocs -c tools\dev\mprocs.go-only.yaml
exit /b 0

:: ================================================================
:help
echo.
echo   dev.bat - Local server management
echo.
echo   Double-click:   Interactive menu
echo   Command line:   dev ^<command^>
echo.
echo   Commands:
echo     start          Build Go exe + start all (2 gate, 4 scene, 1 battle)
echo     start-multi    Build + start with multiple Go instances per service
echo                    (default: login=2,scene_manager=2,player_locator=2,data_service=2)
echo                    Override:  dev start-multi "login=3,scene_manager=2"
echo     start-cpp      Start C++ nodes only (gate + scene + battle)
echo     start-go       Build + start Go services only
echo     start-satoken  Start local SA-Token dev app
echo     stop-satoken   Stop local SA-Token dev app
echo     stop           Stop all C++ and Go processes
echo     status         Show running/gone status (includes SA-Token)
echo     restart        Stop then build + start
echo     logs           Pick a process, open tail window
echo     logs ^<name^>    Tail a specific process (e.g. dev logs login)
echo     logs all       Dump last 60 lines of every process
echo     robot          Build + run robot load-test client
echo     robot ^<cfg^>    Use custom config (default: robot\etc\robot.yaml)
echo     infra          Start local infra (Kafka/Redis/MySQL/etcd)
echo     infra-down     Stop local infra
echo     build          Compile C++ + Go (no launch)
echo     proto          Regenerate proto code
echo     export         Run Excel data table exporter (Python)
echo     export ^<cfg^>   Use custom config (default: exporter_config.yaml)
echo                    Optional Python override: set PYTHON_EXE=C:\path\to\python.exe
echo     gen            Export tables + regenerate proto (both)
echo     clean-logs     Delete all log files under run\logs
echo     ui             mprocs TUI dashboard (all processes)
echo     ui-cpp         mprocs TUI (C++ nodes only)
echo     ui-go          mprocs TUI (Go services only)
echo     help           Show this message
echo.
echo   Example:
echo     dev infra ^& dev start-satoken ^& dev start
echo     dev build
echo     dev proto
echo     dev robot
echo     dev logs login
echo.
exit /b 0
