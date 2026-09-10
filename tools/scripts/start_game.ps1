#Requires -Version 7.0
<#
.SYNOPSIS
    本机一区一键启动；使用已有程序，保留所有游戏数据。
.DESCRIPTION
    双击根目录启动入口。-OpenClient 同时打开游戏；-CheckOnly 仅检查运行文件。
    运行日志：run/logs/game-launcher。服务在后台运行，关闭启动窗口不影响服务。
#>
[CmdletBinding()]
param([switch]$OpenClient, [string]$ClientPath = '', [switch]$CheckOnly)
$ErrorActionPreference = 'Stop'
$serverRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
$logDir = Join-Path $serverRoot "run/logs/game-launcher/$stamp"
$services = @('db','data_service','scene_manager','player_locator','login','match')
$gatewayUrl = 'http://127.0.0.1:8081'
$oldPath = $env:PATH
$oldPassword = $env:LOGIN_DEV_PASSWORD_SHARED_SECRET
$oldRpcPort = $env:RPC_PORT
$ownsMutex = $false
$transcribing = $false
$resultCode = 0
$mutex = $null

function Write-Step([string]$Message) {
    Write-Host "`n[$(Get-Date -Format HH:mm:ss)] $Message" -ForegroundColor Cyan
}
function Find-Program([string]$Name, [string[]]$Candidates) {
    foreach ($candidate in $Candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) { return $candidate }
    }
    $found = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($found) { return $found.Source }
    throw "找不到 $Name，请恢复本机已有运行环境。"
}
# 不经过 shell 解析；按 Windows 参数规则引用路径中的空格和中文。
function Read-SharedLog([string]$Path) {
    $stream = [IO.File]::Open($Path,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
    $reader = [IO.StreamReader]::new($stream)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}
function Invoke-Program([string]$File, [string[]]$Arguments, [int]$Timeout = 30) {
    # 后台孙进程可能继承匿名管道，ReadToEnd 会永远等不到 EOF；改用独立文件。
    $commandTag = [Guid]::NewGuid().ToString('N')
    $outPath = Join-Path $logDir "$commandTag.stdout.log"
    $errPath = Join-Path $logDir "$commandTag.stderr.log"
    $quoted = foreach ($argument in $Arguments) {
        $escaped = [regex]::Replace($argument, '(\\*)"', '$1$1\"')
        $escaped = [regex]::Replace($escaped, '(\\+)$', '$1$1')
        '"' + $escaped + '"'
    }
    $process = Start-Process -FilePath $File -ArgumentList ($quoted -join ' ') -WorkingDirectory $serverRoot -WindowStyle Hidden -RedirectStandardOutput $outPath -RedirectStandardError $errPath -PassThru
    try {
        if (-not $process.WaitForExit($Timeout * 1000)) {
            $process.Kill()
            throw "命令超时：$([IO.Path]::GetFileName($File))（${Timeout} 秒）"
        }
        return [pscustomobject]@{ Code=$process.ExitCode; Out=Read-SharedLog $outPath; Err=Read-SharedLog $errPath }
    } finally { $process.Dispose() }
}
function Invoke-Docker([string[]]$Arguments, [int]$Timeout = 30) {
    # 固定本机引擎，不受用户终端里远端 Docker context 的影响。
    Invoke-Program $script:docker (@('--context','desktop-linux') + $Arguments) $Timeout
}
function Wait-Ready([string]$Label, [scriptblock]$Probe, [int]$Seconds = 120) {
    $timer = [Diagnostics.Stopwatch]::StartNew()
    while ($true) {
        if (& $Probe) { Write-Host "  已就绪：$Label" -ForegroundColor Green; return }
        if ($timer.Elapsed.TotalSeconds -ge $Seconds) { throw "$Label 在 ${Seconds} 秒内未就绪。日志：$logDir" }
        Start-Sleep -Seconds 2
    }
}
function Test-Tcp([int]$Port) {
    $socket = [Net.Sockets.TcpClient]::new()
    try { return ($socket.ConnectAsync('127.0.0.1',$Port).Wait(500) -and $socket.Connected) }
    catch { return $false }
    finally { $socket.Dispose() }
}
function Get-MatchingProcess([int]$ProcessId, [string[]]$Paths) {
    $candidate = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if ($candidate -and $candidate.Path -and ($candidate.Path -in $Paths)) { return $candidate }
    return $null
}
# 重启后 PID 可能被别的程序复用，必须核对可执行文件路径，备份后仅移除失效记录。
function Repair-PidRecords([string]$FileName, [switch]$Cpp) {
    $path = Join-Path $serverRoot "run/pids/$FileName"
    $source = $path
    if (-not (Test-Path -LiteralPath $source)) { $source = Join-Path $serverRoot "bin/$FileName" }
    $records = if (Test-Path -LiteralPath $source) { Get-Content -LiteralPath $source -Raw | ConvertFrom-Json -AsHashtable } else { @{} }
    $changed = $false
    foreach ($key in @($records.Keys)) {
        $entry = $records[$key]
        $name = $key -replace '^z\d+_', '' -replace '_\d+$', ''
        $processId = if ($Cpp) { [int]$entry } elseif ($entry -is [System.Collections.IDictionary]) { [int]$entry.Pid } else { [int]$entry }
        $paths = if ($Cpp) { @(Join-Path $serverRoot "bin/$name.exe") } else { @((Join-Path $serverRoot "bin/go_services/$name.exe"),(Join-Path $serverRoot "go/$name/$name.exe")) }
        if (-not (Get-MatchingProcess $processId $paths)) { $records.Remove($key); $changed = $true }
    }
    if ($changed -or $source -ne $path -or -not (Test-Path -LiteralPath $path)) {
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $logDir "$FileName.backup") }
        $records | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $path -Encoding utf8
    }
    return $records
}
function Assert-NodeStartup([string]$Name) {
    $nodeLog = Join-Path $serverRoot "run/logs/cpp_nodes/z1_$Name.stderr.log"
    if (Test-Path -LiteralPath $nodeLog) {
        $failure = Get-Content -LiteralPath $nodeLog -Tail 80 | Where-Object { $_ -match '^Assertion failed:|^Unhandled exception:|^terminate called' } | Select-Object -First 1
        if ($failure) { throw "$Name 服务程序启动崩溃：$failure。需要修复或重新构建该服务；日志：$nodeLog" }
    }
}
function Invoke-Dev([string]$Label, [string[]]$Arguments) {
    $reply = Invoke-Program (Join-Path $PSHOME 'pwsh.exe') (@('-NoProfile','-ExecutionPolicy','Bypass','-File',(Join-Path $PSScriptRoot 'dev_tools.ps1')) + $Arguments) 240
    $reply.Out | Set-Content -LiteralPath (Join-Path $logDir "$Label.stdout.log")
    $reply.Err | Set-Content -LiteralPath (Join-Path $logDir "$Label.stderr.log")
    if ($reply.Code -ne 0) { throw "$Label 启动失败，查看 $logDir 下同名日志。" }
}
try {
    $hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($serverRoot.ToLowerInvariant())))
    $mutex = [Threading.Mutex]::new($false,"Local\MMORPG-Start-$hash")
    try { $ownsMutex = $mutex.WaitOne(0) } catch [Threading.AbandonedMutexException] { $ownsMutex = $true }
    if (-not $ownsMutex) { throw '另一个启动窗口正在工作，请等待它完成。' }
    New-Item -ItemType Directory -Force -Path $logDir,(Join-Path $serverRoot 'run/pids') | Out-Null
    Start-Transcript -LiteralPath (Join-Path $logDir 'launcher.log') | Out-Null
    $transcribing = $true
    Write-Step '1/6 检查本机游戏运行环境'
    $script:docker = Find-Program 'docker.exe' @((Join-Path $env:LOCALAPPDATA 'Programs/DockerDesktop/resources/bin/docker.exe'),(Join-Path $env:ProgramFiles 'Docker/Docker/resources/bin/docker.exe'))
    $desktop = Find-Program 'Docker Desktop.exe' @((Join-Path $env:LOCALAPPDATA 'Programs/DockerDesktop/Docker Desktop.exe'),(Join-Path $env:ProgramFiles 'Docker/Docker/Docker Desktop.exe'))
    $java = Find-Program 'java.exe' @($(if ($env:JAVA_HOME) { Join-Path $env:JAVA_HOME 'bin/java.exe' }))
    $javaReply = Invoke-Program $java @('-version')
    if ($javaReply.Code -ne 0 -or ($javaReply.Err + $javaReply.Out) -notmatch 'version "(?<major>\d+)') { throw '无法确认 Java 版本。' }
    if ([int]$Matches.major -lt 21) { throw '网关需要 Java 21 或更高版本，请调整 JAVA_HOME。' }
    $jar = Get-ChildItem -LiteralPath (Join-Path $serverRoot 'java/gateway_node/target') -Filter 'gateway-node-*.jar' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $jar) { throw '缺少已构建的 Java 网关 jar。' }
    foreach ($service in $services) {
        if (-not (Test-Path -LiteralPath (Join-Path $serverRoot "bin/go_services/$service.exe")) -and -not (Test-Path -LiteralPath (Join-Path $serverRoot "go/$service/$service.exe"))) { throw "缺少 $service.exe，请先构建对应服务。" }
    }
    foreach ($node in @('gate','scene','battle')) {
        if (-not (Test-Path -LiteralPath (Join-Path $serverRoot "bin/$node.exe"))) { throw "缺少 bin/$node.exe。" }
    }
    if (-not $ClientPath) { $ClientPath = Join-Path (Split-Path -Parent $serverRoot) 'tmp/showcase_player/mmorpg.exe' }
    $ClientPath = [IO.Path]::GetFullPath($ClientPath)
    if ($OpenClient -and -not (Test-Path -LiteralPath $ClientPath -PathType Leaf)) { throw "找不到游戏客户端：$ClientPath，可用 -ClientPath 指定。" }
    if (-not $env:LOGIN_DEV_PASSWORD_SHARED_SECRET) {
        # 密码仅从已有本地配置读入子进程环境，不回显、不写入脚本或日志。
        $config = Get-Content -LiteralPath (Join-Path $serverRoot 'robot/etc/robot.yaml') -Raw
        $passwordMatch = [regex]::Match($config,'(?m)^password:\s*"([^"\r\n]+)"')
        if (-not $passwordMatch.Success) { throw '本地开发登录密码未配置，请设置 LOGIN_DEV_PASSWORD_SHARED_SECRET。' }
        $env:LOGIN_DEV_PASSWORD_SHARED_SECRET = $passwordMatch.Groups[1].Value
        $config = $null
        $passwordMatch = $null
    }
    if ($CheckOnly) {
        Write-Host '启动文件与运行环境检查通过；未启动服务。' -ForegroundColor Green
    } else {
        Write-Step '2/6 启动 Docker 和数据库依赖（保留已有数据）'
        try { $engine = Invoke-Docker @('info','--format','{{.OSType}}') 15 } catch { $engine = @{ Code = -1 } }
        if ($engine.Code -ne 0) {
            if (-not (Get-Process -Name 'Docker Desktop','com.docker.backend' -ErrorAction SilentlyContinue)) {
                $dockerRoot = [IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'Docker'))
                $runPath = [IO.Path]::GetFullPath((Join-Path $dockerRoot 'run'))
                if ((Split-Path -Parent $runPath) -ne $dockerRoot) { throw 'Docker 临时目录校验失败。' }
                if (Test-Path -LiteralPath $runPath) {
                    # 此目录只放通信端点。重命名备份，不删除 socket 或任何数据卷。
                    $staleSockets = @(Get-ChildItem -LiteralPath $runPath -Force | Where-Object { $_.Name -in @('dockerInference','dockerEthernetVfkit','userAnalyticsOtlpHttp.sock') })
                    if ($staleSockets.Count -gt 0) {
                        Rename-Item -LiteralPath $runPath -NewName "run-before-game-$stamp"
                        Write-Host '  已备份 Docker 遗留通信目录。'
                    }
                }
                # 失效的 socket 重解析点可能无法直接重命名；只备份其独立通信目录。
                $socketParent = [IO.Path]::GetFullPath($env:LOCALAPPDATA).TrimEnd([IO.Path]::DirectorySeparatorChar)
                $socketRoot = [IO.Path]::GetFullPath((Join-Path $socketParent 'docker-secrets-engine'))
                if ((Split-Path -Parent $socketRoot) -ne $socketParent) { throw 'Docker 通信目录不在 LOCALAPPDATA 直属目录，已中止。' }
                if (Test-Path -LiteralPath $socketRoot) {
                    $socketDirectory = Get-Item -LiteralPath $socketRoot -Force
                    if (-not $socketDirectory.PSIsContainer -or ($socketDirectory.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                        throw 'Docker 通信目录不是普通目录或自身是重解析点，已中止。'
                    }
                    $socketEntries = @(Get-ChildItem -LiteralPath $socketRoot -Force)
                    if ($socketEntries.Count -gt 0) {
                        if ($socketEntries.Count -ne 1 -or $socketEntries[0].Name -cne 'engine.sock' -or $socketEntries[0].PSIsContainer) {
                            throw 'Docker 通信目录包含 engine.sock 以外的内容，已中止且未移动任何内容。'
                        }
                        $socketBackupName = "docker-secrets-engine.before-game-$stamp"
                        $socketBackupPath = [IO.Path]::GetFullPath((Join-Path $socketParent $socketBackupName))
                        if ((Split-Path -Parent $socketBackupPath) -ne $socketParent -or (Test-Path -LiteralPath $socketBackupPath)) {
                            throw 'Docker 通信目录备份目标异常或已存在，已中止且不会覆盖。'
                        }
                        if (Get-Process -Name 'Docker Desktop','com.docker.backend','dockerd','docker' -ErrorAction SilentlyContinue) {
                            throw 'Docker 已在运行，已中止通信目录备份，请勿并行启动。'
                        }
                        Rename-Item -LiteralPath $socketRoot -NewName $socketBackupName
                        Write-Host '  已备份 Docker 遗留 socket 通信目录。'
                    }
                }
                Start-Process -FilePath $desktop -WindowStyle Hidden
            }
            Wait-Ready 'Docker Linux 引擎' { try { (Invoke-Docker @('info','--format','{{.OSType}}') 15).Code -eq 0 } catch { $false } } 300
        }
        $compose = @('compose','-f',(Join-Path $serverRoot 'deploy/docker-compose.yml'),'--profile','redis-cluster')
        $infra = Invoke-Docker ($compose + @('up','-d','--no-recreate','--pull','never','etcd','redis','mysql','kafka','redis-cluster-0','redis-cluster-1','redis-cluster-2','redis-cluster-3','redis-cluster-4','redis-cluster-5','redis-cluster-init')) 180
        ($infra.Out + $infra.Err) | Set-Content -LiteralPath (Join-Path $logDir 'docker.log')
        if ($infra.Code -ne 0) { throw "Docker 依赖启动失败，查看 $logDir/docker.log。" }
        Wait-Ready 'MySQL' { (Invoke-Docker @('inspect','--format','{{.State.Health.Status}}','mysql')).Out.Trim() -eq 'healthy' }
        Wait-Ready 'etcd' { (Invoke-Docker @('exec','etcd','etcdctl','endpoint','health')).Code -eq 0 }
        Wait-Ready 'Redis' { (Invoke-Docker @('exec','redis','redis-cli','ping')).Out.Trim() -eq 'PONG' }
        Wait-Ready 'Redis Cluster' { (Invoke-Docker @('exec','redis-cluster-0','redis-cli','-p','7000','cluster','info')).Out -match 'cluster_state:ok' }
        Wait-Ready 'Kafka' { (Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--list') 20).Code -eq 0 }
        Write-Step '3/6 启动登录、存档和匹配服务'
        $goRecords = Repair-PidRecords 'go_services.pid.json'
        $cppRecords = Repair-PidRecords 'cpp_nodes.pid.json' -Cpp
        $running = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($serverRoot + '\',[StringComparison]::OrdinalIgnoreCase) })
        foreach ($item in $running) {
            $name = [IO.Path]::GetFileNameWithoutExtension($item.Name)
            if ($name -in $services -and -not $goRecords.ContainsKey($name)) { throw "$name 正在运行但缺少默认实例记录，请检查 run/pids。" }
            if ($name -in @('gate','scene','battle') -and -not ($cppRecords.Values -contains $item.ProcessId)) { throw "$name 正在运行但缺少有效记录，请检查 run/pids。" }
        }
        Invoke-Dev 'go-services' @('-Command','go-svc-start-exe','-GoServices',($services -join ','),'-TierReadySeconds','30')
        $goRecords = Get-Content -LiteralPath (Join-Path $serverRoot 'run/pids/go_services.pid.json') -Raw | ConvertFrom-Json -AsHashtable
        foreach ($service in $services) {
            $entry = $goRecords[$service]
            if (-not $entry) { throw "$service 没有启动记录。" }
            Wait-Ready $service {
                if (-not (Get-MatchingProcess $entry.Pid @((Join-Path $serverRoot "bin/go_services/$service.exe"),(Join-Path $serverRoot "go/$service/$service.exe")))) { throw "$service 已退出，查看 run/logs/go_services/$service.stderr.log。" }
                Test-Tcp $entry.Port
            } 60
        }
        Write-Step '4/6 启动游戏网关'
        $gatewayProcess = Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'java.exe' -and $_.CommandLine -and $_.CommandLine.Contains($jar.FullName) } | Select-Object -First 1
        if (-not $gatewayProcess) {
            if (Test-Tcp 8081) { throw '8081 端口被其他程序占用，未启动重复网关。' }
            $javaTmp = Join-Path $serverRoot "run/java-tmp-$stamp"
            New-Item -ItemType Directory -Path $javaTmp | Out-Null
            $gatewayProcess = Start-Process -FilePath $java -WorkingDirectory (Join-Path $serverRoot 'java/gateway_node') -ArgumentList @("`"-Djdk.net.unixdomain.tmpdir=$javaTmp`"",'-jar',"`"$($jar.FullName)`"") -WindowStyle Hidden -RedirectStandardOutput (Join-Path $logDir 'gateway.stdout.log') -RedirectStandardError (Join-Path $logDir 'gateway.stderr.log') -PassThru
            $gatewayProcess.Id | Set-Content -LiteralPath (Join-Path $serverRoot 'run/pids/gateway_node.pid')
        }
        Wait-Ready '网关健康检查' { try { (Invoke-RestMethod "$gatewayUrl/actuator/health" -TimeoutSec 5).status -eq 'UP' } catch { $false } }
        Write-Step '5/6 启动一区场景和战斗服'
        $env:PATH = (Join-Path $serverRoot 'third_party/grpc/install_vs2026_dbg/bin') + ';' + $oldPath
        $env:RPC_PORT = $null
        Invoke-Dev 'gate' @('-Command','cpp-node-start','-CppNodes','gate','-GateCount','1','-SceneCount','0','-BattleCount','0','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'gate'
        Invoke-Dev 'scene' @('-Command','cpp-node-start','-CppNodes','scene','-GateCount','0','-SceneCount','1','-BattleCount','0','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'scene'
        # battle 和 scene 的自动端口基址相同，本机必须为 battle 指定独立端口。
        $env:RPC_PORT = '20010'
        Invoke-Dev 'battle' @('-Command','cpp-node-start','-CppNodes','battle','-GateCount','0','-SceneCount','0','-BattleCount','1','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'battle'
        $cppRecords = Get-Content -LiteralPath (Join-Path $serverRoot 'run/pids/cpp_nodes.pid.json') -Raw | ConvertFrom-Json -AsHashtable
        foreach ($node in @('gate','scene','battle')) {
            $nodeProcessId = [int]$cppRecords["z1_$node"]
            Wait-Ready $node {
                if (-not (Get-MatchingProcess $nodeProcessId @((Join-Path $serverRoot "bin/$node.exe")))) { throw "$node 已退出，查看 run/logs/cpp_nodes/z1_$node.stderr.log。" }
                @(Get-NetTCPConnection -State Listen -OwningProcess $nodeProcessId -ErrorAction SilentlyContinue).Count -gt 0
            } 90
        }
        Write-Step '6/6 检查区服入口'
        Wait-Ready '一区开放' { try { @((Invoke-RestMethod "$gatewayUrl/api/server-list" -TimeoutSec 5).zones | Where-Object { $_.zone_id -eq 1 -and $_.status -eq 'OPEN' }).Count -gt 0 } catch { $false } }
        Write-Host "`n服务器已启动。网关：$gatewayUrl，选择一区。" -ForegroundColor Green
        if ($OpenClient) {
            $existingClient = Get-Process -Name 'mmorpg' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $ClientPath }
            if (-not $existingClient) {
                Start-Process -FilePath $ClientPath -WorkingDirectory (Split-Path -Parent $ClientPath) -ArgumentList @('-screen-fullscreen','0','-screen-width','1600','-screen-height','900','-gateway',$gatewayUrl) -WindowStyle Normal
            }
            Write-Host '游戏窗口已打开。' -ForegroundColor Green
        }
        Write-Host '现在可以关闭启动窗口，服务器会继续在后台运行。'
    }
} catch {
    $resultCode = 1
    Write-Host "`n启动未完成：$($_.Exception.Message)" -ForegroundColor Red
    if (Test-Path -LiteralPath $logDir) { Write-Host "日志目录：$logDir" }
} finally {
    $env:PATH = $oldPath
    $env:LOGIN_DEV_PASSWORD_SHARED_SECRET = $oldPassword
    $env:RPC_PORT = $oldRpcPort
    if ($transcribing) { Stop-Transcript | Out-Null }
    if ($ownsMutex) { $mutex.ReleaseMutex() }
    if ($mutex) { $mutex.Dispose() }
}
exit $resultCode
