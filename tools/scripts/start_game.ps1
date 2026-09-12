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
$services = @('db','data_service','client_rpc_router','scene_manager','player_locator','login','match')
$gatewayUrl = 'http://127.0.0.1:8081'
$oldPath = $env:PATH
$oldPassword = $env:LOGIN_DEV_PASSWORD_SHARED_SECRET
$oldRpcPort = $env:RPC_PORT
$oldCommandPartitions = $env:KAFKA_COMMAND_TOPIC_PARTITIONS
$oldCommandGeneration = $env:KAFKA_COMMAND_TOPIC_GENERATION
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
# 与部署工具共用 YAML 读取器；命令寻址配置以 C++ 消费者的本机配置为准。
function Get-LocalKafkaContract {
    $basePath = Join-Path $serverRoot 'bin/etc/base_deploy_config.yaml'
    $auditPath = Join-Path $serverRoot 'go/data_service/etc/data_service.yaml'
    $values = @{}
    foreach ($key in @('Kafka.CommandTopicPartitions','Kafka.CommandTopicGeneration','AuditTopicGeneration')) {
        $value = Get-YamlScalar -Path $basePath -KeyPath $key
        $number = 0L
        if (-not $value.Found -or -not [long]::TryParse($value.Value,[ref]$number) -or $number -le 0 -or $number -gt [int]::MaxValue) {
            throw "本机 Kafka 配置 $key 必须是有效正整数，未启动任何服务。"
        }
        $values[$key] = $number
    }
    foreach ($key in @('TopicGeneration','TransactionLogTopic','TransactionLogPartitions','SnapshotTopic','SnapshotPartitions','RetentionMs')) {
        $value = Get-YamlScalar -Path $auditPath -KeyPath "Kafka.$key"
        if (-not $value.Found -or [string]::IsNullOrWhiteSpace($value.Value)) { throw "data_service 缺少 Kafka.$key，已中止主题预建。" }
        $values[$key] = $value.Value
    }
    if ($values['TopicGeneration'] -cne [string]$values['AuditTopicGeneration']) {
        throw 'C++ AuditTopicGeneration 与 data_service Kafka.TopicGeneration 不一致，已中止主题预建。'
    }
    # 正式 Compose 预建清单的审计规则当前固定；漂移时显式拒绝，避免给生产者建错主题。
    foreach ($entry in @{TransactionLogTopic='transaction_log_topic';TransactionLogPartitions='6';SnapshotTopic='player_snapshot_topic';SnapshotPartitions='3';RetentionMs='2592000000'}.GetEnumerator()) {
        if ($values[$entry.Key] -cne $entry.Value) { throw "data_service Kafka.$($entry.Key) 与 deploy/docker-compose.yml 预建清单不一致，需先同步正式规则。" }
    }
    $generation = $values['Kafka.CommandTopicGeneration']
    $partitions = $values['Kafka.CommandTopicPartitions']
    $auditGeneration = $values['AuditTopicGeneration']
    return [pscustomobject]@{
        Generation=$generation; Partitions=$partitions; AuditGeneration=$auditGeneration
        Topics=@(
            @{Name="gate-cmd_g$generation";Partitions=$partitions;RetentionMs=3600000},
            @{Name="scene-cmd_g$generation";Partitions=$partitions;RetentionMs=3600000},
            @{Name='game-events';Partitions=1;RetentionMs=3600000},
            @{Name="transaction_log_topic_g$auditGeneration";Partitions=6;RetentionMs=2592000000},
            @{Name="player_snapshot_topic_g$auditGeneration";Partitions=3;RetentionMs=2592000000}
        )
    }
}
function Assert-LocalKafkaTopic($Spec) {
    $topic = $Spec.Name
    $metadata = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--describe','--topic',$topic) 60
    $header = [regex]::Match($metadata.Out,'(?m)^\s*Topic:\s+\S+.*?\bPartitionCount:\s*(\d+)\s+ReplicationFactor:\s*(\d+)')
    if ($metadata.Code -ne 0 -or -not $header.Success) { throw "无法读取 Kafka 主题 $topic 的完整元数据。" }
    if ([long]$header.Groups[1].Value -ne $Spec.Partitions -or [int]$header.Groups[2].Value -ne 1) {
        throw "Kafka 主题 $topic 分区或副本契约不符（应为 $($Spec.Partitions) 分区、1 副本）。保留现有主题，必须协调换代，禁止原地扩分区。"
    }
    $configs = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-configs.sh','--bootstrap-server','localhost:9092','--entity-type','topics','--entity-name',$topic,'--describe','--all') 60
    # 锚定行首，只读取有效值，不能误取 synonyms 内的 broker 默认保留期。
    $retention = [regex]::Match($configs.Out,'(?m)^\s*retention\.ms=(-?\d+)\b')
    $cleanup = [regex]::Match($configs.Out,'(?m)^\s*cleanup\.policy=(\S+)')
    if ($configs.Code -ne 0 -or -not $retention.Success -or [long]$retention.Groups[1].Value -ne $Spec.RetentionMs -or -not $cleanup.Success -or $cleanup.Groups[1].Value -cne 'delete') {
        throw "Kafka 主题 $topic 保留/清理配置不符（应为 retention.ms=$($Spec.RetentionMs)、cleanup.policy=delete），已停止且未修改现有主题。"
    }
}
function Initialize-LocalKafkaTopics($Contract, [string[]]$ComposeArguments) {
    $listing = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--list') 60
    if ($listing.Code -ne 0) { throw '无法列出 Kafka 主题，已停止预建。' }
    $existing = @($listing.Out -split '\r?\n' | ForEach-Object { $_.Trim() })
    $missing = @()
    # 所有既有主题先验完，再允许正式初始化器创建缺失项；不删主题、不改分区。
    foreach ($spec in $Contract.Topics) {
        if ($spec.Name -cin $existing) { Assert-LocalKafkaTopic $spec } else { $missing += $spec.Name }
    }
    if ($missing.Count -gt 0) {
        Write-Host "  预建缺失 Kafka 主题：$($missing -join ', ')"
        $init = Invoke-Docker ($ComposeArguments + @(
            'run','--rm','--no-deps','--pull','never',
            '-e',"KAFKA_INIT_COMMAND_PARTITIONS=$($Contract.Partitions)",
            '-e',"KAFKA_INIT_COMMAND_TOPIC_GENERATION=$($Contract.Generation)",
            '-e',"KAFKA_INIT_AUDIT_TOPIC_GENERATION=$($Contract.AuditGeneration)",
            'kafka-topic-init'
        )) 240
        ($init.Out + $init.Err) | Set-Content -LiteralPath (Join-Path $logDir 'kafka-topic-init.log')
        if ($init.Code -ne 0) { throw "Kafka 正式主题预建失败，查看 $logDir/kafka-topic-init.log。" }
        foreach ($spec in $Contract.Topics) { Assert-LocalKafkaTopic $spec }
    }
    Write-Host "  Kafka 契约已核对：命令 g$($Contract.Generation)、$($Contract.Partitions) 分区；审计 g$($Contract.AuditGeneration)。" -ForegroundColor Green
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
# StartRpcServer 先装 Kafka handler 再打印 banner，业务 DependencyGate 随后才等 Go 服务。
# 不能在这里等 scene 的 world ready，否则会与尚未启动的 scene_manager 相互等待。
function Test-LocalNodeRegistration([string]$Name, [int]$ProcessId) {
    $serviceName = @{gate='GateNodeService';scene='SceneNodeService';battle='BattleNodeService'}[$Name]
    $reply = Invoke-Docker @('exec','etcd','etcdctl','get',"$serviceName.rpc/zone/1/",'--prefix','--write-out=json') 15
    if ($reply.Code -ne 0) { return $false }
    $records = $reply.Out | ConvertFrom-Json
    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $false }
    $ports = @(Get-NetTCPConnection -State Listen -OwningProcess $ProcessId -ErrorAction SilentlyContinue | Select-Object -ExpandProperty LocalPort)
    foreach ($record in $records.kvs) {
        if ([long]$record.lease -le 0) { continue }
        $nodeInfo = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($record.value)) | ConvertFrom-Json
        if ([int]$nodeInfo.endpoint.port -notin $ports) { continue }
        $nodeUuid = if ($nodeInfo.node_uuid) { $nodeInfo.node_uuid } else { $nodeInfo.nodeUuid }
        if (-not $nodeUuid) { continue }
        $logs = @(Get-ChildItem -LiteralPath (Join-Path $serverRoot 'bin/logs/cpp_nodes') -Filter "$Name*.log" -ErrorAction SilentlyContinue |
            Where-Object { $_.LastWriteTime -ge $process.StartTime })
        foreach ($log in $logs) {
            $content = Read-SharedLog $log.FullName
            if ($content.Contains('NODE STARTED SUCCESSFULLY') -and $content.Contains([string]$nodeUuid)) { return $true }
        }
    }
    return $false
}
function Invoke-Dev([string]$Label, [string[]]$Arguments) {
    $reply = Invoke-Program (Join-Path $PSHOME 'pwsh.exe') (@('-NoProfile','-ExecutionPolicy','Bypass','-File',(Join-Path $PSScriptRoot 'dev_tools.ps1')) + $Arguments) 240
    $reply.Out | Set-Content -LiteralPath (Join-Path $logDir "$Label.stdout.log")
    $reply.Err | Set-Content -LiteralPath (Join-Path $logDir "$Label.stderr.log")
    if ($reply.Code -ne 0) { throw "$Label 启动失败，查看 $logDir 下同名日志。" }
}
# 已运行进程不会重新读取进程环境；记录本启动器实际拉起的 PID 与启动时间，防止换代后复用旧进程。
function Assert-RunningCommandContract($Process) {
    $name = [IO.Path]::GetFileNameWithoutExtension($Process.Name)
    if ($name -notin @('gate','scene','battle','login','scene_manager','player_locator','match')) { return }
    $path = Join-Path $serverRoot 'run/pids/kafka_command_contract.json'
    $records = if (Test-Path -LiteralPath $path) { Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable } else { @{} }
    $record = $records[$name]
    $current = Get-Process -Id $Process.ProcessId -ErrorAction Stop
    if (-not $record -or $record.ProcessId -ne $current.Id -or $record.StartTimeUtcTicks -cne [string]$current.StartTime.ToUniversalTime().Ticks -or
        $record.Generation -ne $kafkaContract.Generation -or $record.Partitions -ne $kafkaContract.Partitions) {
        throw "$name 已在运行，但不能确认它使用本次 Kafka 契约。请有序停止旧控制面进程后重新启动；启动器不会擅自重启或让两代混跑。"
    }
}
function Save-LocalCommandContract([string]$Name, [int]$ProcessId) {
    if ($Name -notin @('gate','scene','battle','login','scene_manager','player_locator','match')) { return }
    $path = Join-Path $serverRoot 'run/pids/kafka_command_contract.json'
    $records = if (Test-Path -LiteralPath $path) { Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable } else { @{} }
    $process = Get-Process -Id $ProcessId -ErrorAction Stop
    $records[$Name] = @{
        ProcessId=$process.Id; StartTimeUtcTicks=[string]$process.StartTime.ToUniversalTime().Ticks
        Generation=$kafkaContract.Generation; Partitions=$kafkaContract.Partitions
    }
    $records | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $path -Encoding utf8
}
function Wait-LocalNode([string]$Name) {
    $records = Get-Content -LiteralPath (Join-Path $serverRoot 'run/pids/cpp_nodes.pid.json') -Raw | ConvertFrom-Json -AsHashtable
    $processId = [int]$records["z1_$Name"]
    Wait-Ready $Name {
        if (-not (Get-MatchingProcess $processId @((Join-Path $serverRoot "bin/$Name.exe")))) {
            throw "$Name 已退出，查看 run/logs/cpp_nodes/z1_$Name.stderr.log。"
        }
        Assert-NodeStartup $Name
        Test-LocalNodeRegistration $Name $processId
    } 90
    Save-LocalCommandContract $Name $processId
}
function Start-LocalGoServices([string[]]$Names) {
    $env:RPC_PORT = $null
        Invoke-Dev ('go-' + ($Names -join '-')) @('-Command','go-svc-start-exe','-GoServices',($Names -join ','),'-TierReadySeconds','30')
        $goRecords = Get-Content -LiteralPath (Join-Path $serverRoot 'run/pids/go_services.pid.json') -Raw | ConvertFrom-Json -AsHashtable
        foreach ($service in $Names) {
            $entry = $goRecords[$service]
            if (-not $entry) { throw "$service 没有启动记录。" }
            Wait-Ready $service {
                if (-not (Get-MatchingProcess $entry.Pid @((Join-Path $serverRoot "bin/go_services/$service.exe"),(Join-Path $serverRoot "go/$service/$service.exe")))) { throw "$service 已退出，查看 run/logs/go_services/$service.stderr.log。" }
                Test-Tcp $entry.Port
            } 60
            Save-LocalCommandContract $service $entry.Pid
        }
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
    . (Join-Path $PSScriptRoot 'lib/release_common.ps1')
    $kafkaContract = Get-LocalKafkaContract
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
        Wait-Ready 'Kafka' { (Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--list') 60).Code -eq 0 }
        Write-Step '3/6 启动存档服务'
        # login、scene_manager、player_locator、match 由 dev_tools 的子进程继承同一契约。
        $env:KAFKA_COMMAND_TOPIC_PARTITIONS = [string]$kafkaContract.Partitions
        $env:KAFKA_COMMAND_TOPIC_GENERATION = [string]$kafkaContract.Generation
        $goRecords = Repair-PidRecords 'go_services.pid.json'
        $cppRecords = Repair-PidRecords 'cpp_nodes.pid.json' -Cpp
        $running = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($serverRoot + '\',[StringComparison]::OrdinalIgnoreCase) })
        foreach ($item in $running) {
            Assert-RunningCommandContract $item
            $name = [IO.Path]::GetFileNameWithoutExtension($item.Name)
            if ($name -in $services -and -not $goRecords.ContainsKey($name)) { throw "$name 正在运行但缺少默认实例记录，请检查 run/pids。" }
            if ($name -in @('gate','scene','battle') -and -not ($cppRecords.Values -contains $item.ProcessId)) { throw "$name 正在运行但缺少有效记录，请检查 run/pids。" }
        }
        Initialize-LocalKafkaTopics $kafkaContract $compose
        Start-LocalGoServices @('db','data_service')
        Write-Step '4/6 启动一区 Kafka 消费者和战斗服'
        $env:PATH = (Join-Path $serverRoot 'third_party/grpc/install_vs2026_dbg/bin') + ';' + $oldPath
        $env:RPC_PORT = $null
        Invoke-Dev 'gate' @('-Command','cpp-node-start','-CppNodes','gate','-GateCount','1','-SceneCount','0','-BattleCount','0','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'gate'
        Invoke-Dev 'scene' @('-Command','cpp-node-start','-CppNodes','scene','-GateCount','0','-SceneCount','1','-BattleCount','0','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'scene'
        Wait-LocalNode 'gate'
        Wait-LocalNode 'scene'
        # battle 和 scene 的自动端口基址相同，本机必须为 battle 指定独立端口。
        $env:RPC_PORT = '20010'
        Invoke-Dev 'battle' @('-Command','cpp-node-start','-CppNodes','battle','-GateCount','0','-SceneCount','0','-BattleCount','1','-Zone','1','-NodeIp','loopback')
        Assert-NodeStartup 'battle'
        Wait-LocalNode 'battle'
        Write-Step '5/6 启动登录、匹配服务和游戏网关'
        Start-LocalGoServices @('client_rpc_router','scene_manager','player_locator','login','match')
        $gatewayProcess = Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'java.exe' -and $_.CommandLine -and $_.CommandLine.Contains($jar.FullName) } | Select-Object -First 1
        if (-not $gatewayProcess) {
            if (Test-Tcp 8081) { throw '8081 端口被其他程序占用，未启动重复网关。' }
            $javaTmp = Join-Path $serverRoot "run/java-tmp-$stamp"
            New-Item -ItemType Directory -Path $javaTmp | Out-Null
            $gatewayProcess = Start-Process -FilePath $java -WorkingDirectory (Join-Path $serverRoot 'java/gateway_node') -ArgumentList @("`"-Djdk.net.unixdomain.tmpdir=$javaTmp`"",'-jar',"`"$($jar.FullName)`"") -WindowStyle Hidden -RedirectStandardOutput (Join-Path $logDir 'gateway.stdout.log') -RedirectStandardError (Join-Path $logDir 'gateway.stderr.log') -PassThru
            $gatewayProcess.Id | Set-Content -LiteralPath (Join-Path $serverRoot 'run/pids/gateway_node.pid')
        }
        Wait-Ready '网关健康检查' { try { (Invoke-RestMethod "$gatewayUrl/actuator/health" -TimeoutSec 5).status -eq 'UP' } catch { $false } }
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
    $env:KAFKA_COMMAND_TOPIC_PARTITIONS = $oldCommandPartitions
    $env:KAFKA_COMMAND_TOPIC_GENERATION = $oldCommandGeneration
    if ($transcribing) { Stop-Transcript | Out-Null }
    if ($ownsMutex) { $mutex.ReleaseMutex() }
    if ($mutex) { $mutex.Dispose() }
}
exit $resultCode
