#Requires -Version 7.0
<#
.SYNOPSIS
    本机一区一键启动；使用已有程序，保留所有游戏数据。
.DESCRIPTION
    双击根目录启动入口。-OpenClient 同时打开游戏；-CheckOnly 仅检查运行文件。
    运行日志：run/logs/game-launcher。服务在后台运行，关闭启动窗口不影响服务。
.PARAMETER GateRouterMode
    gate 的客户端 RPC 路由模式（环境变量 GATE_CLIENT_RPC_ROUTER，cpp/nodes/gate/gate_router_mode.h），
    默认 '1' = 开：gate 只连 client_rpc_router，登录 / 匹配 / 聊天 / 帮会 / 好友 / 聚宝斋 / 邮件 / 排行榜都经路由服转发。
    '0' = 回退到旧的逐服务直连：**直连模式下 chat / guild / friend / trade / mail / rank 不可达**——gate 直连白名单（cpp/nodes/gate/main.cpp）里没有
    Chat / Guild / Friend / Trade / Mail / Rank，进程照样在跑，但任何玩家的这些请求都到不了它们（docs/design/client-rpc-router.md D34）。
    只用于排障回退，回退前先 killswitch 关这些方法并公告。
    战斗与本开关无关：两种模式下战斗都只走客户端 ↔ battle 直连，gate 不再中继战斗（turn-based §22 D66）。
    翻转落在部署层、不改 C++ 默认值：gate 进程启动时读一次该变量并缓存，所以只对本次新拉起的 gate 生效；
    已在运行的 gate 保持原模式，换模式要先有序停掉 gate 再启动。
    用 '1'/'0' 字符串而不是 [bool]：根目录 .cmd 经 pwsh -File 原样透传 %*，实参都是字符串，
    写成 `start-server.cmd -GateRouterMode 0` 即可；与 k8s_deploy.ps1 -GateRouterMode 同一口径，
    K8s 默认也是 '1'（turn-based §22 D75）。
#>
[CmdletBinding()]
param([switch]$OpenClient, [string]$ClientPath = '', [switch]$CheckOnly, [ValidateSet('1','0')][string]$GateRouterMode = '1')
$ErrorActionPreference = 'Stop'
$serverRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
$logDir = Join-Path $serverRoot "run/logs/game-launcher/$stamp"
# chat / guild 只承诺路由服模式可达（见 -GateRouterMode），与 client_rpc_router 一起列入；缺 exe 时的处理见下方 $optionalServices。
$services = @('db','data_service','client_rpc_router','scene_manager','player_locator','login','match','chat','guild')
# 可选服务：缺 exe 时只告警并跳过，不拒绝整个启动。chat 是新服务，本机 bin/go_services 可能还没有它的 exe
#（Go 工具链不在本机时由 Codex 构建）；为一个聊天服务拒启会让登录、匹配一起不可用。chat 缺席时玩家的聊天请求
# 得到“服务不可用”，其余功能不受影响。
# 已有服务（db / login / match 等）仍是必需的：它们缺 exe 说明环境本身不完整，应当在第 1 步就拒启。
# guild 也是必需的：match 启动时核对 guild 的表，这些表只能由 guild -migrate 建，缺 guild.exe 时 match 同样起不来。
$optionalServices = @('chat')
# 聚宝斋 trade(P1)：新服务，按可选处理，以下两种情况跳过它、其余服务照常启动（缺席时只影响聚宝斋请求）：
#   1. 缺 exe（第 1 步，与 chat 同一逻辑）；
#   2. 第 3 步生成写者启动目录时建表失败（lib/sql_boot_catalog_local.ps1 记进 last-run.json 的 skipped）。
# 第 2 条的取舍依据（AGENTS §11.3）：trade 缺表会在启动期直接退出，Start-LocalGoServices 随即抛错，整个一键启动会停在网关之前，
# 为聚宝斋拖垮登录 / 匹配不可接受。fail-closed 仍在 trade 进程自身（表不在即拒启），这里只是不让它连坐。
# exe 与表都就绪、但 trade 因其他原因（data_service 不可达等）起不来时，仍按原逻辑中止启动。
# -CheckOnly 不启动 Docker，不建表。trade 还依赖 data_service（第 3 步已起）。
# 与 chat / guild 一样只承诺路由服模式可达。单独追加，不改上面两行数组。
$services += 'trade'
$optionalServices += 'trade'
# 好友 friend（docs/design/friend-port-20260918.md）：与 trade 同形态的新服务，同样按可选处理，两种情况跳过它：
#   1. 缺 exe（第 1 步，与 chat / trade 同一逻辑）；
#   2. 第 3 步生成写者启动目录时建表失败。
# 取舍依据与 trade 相同（AGENTS §11.3）：为好友功能拖垮登录 / 匹配不可接受。fail-closed 仍在 friend 进程自身（表不在即拒启），这里只是不让它连坐。
# 与 chat / guild / trade 一样只承诺路由服模式可达（D-12）。单独追加，不改上面两行数组。
$services += 'friend'
$optionalServices += 'friend'
# 玩家持久收件箱 player_inbox（docs/design/player-inbox-service.md）：与 trade / friend 同形态的新服务，同样按可选处理，两种情况跳过它：
#   1. 缺 exe（第 1 步，与 chat / trade / friend 同一逻辑）；
#   2. 第 3 步生成写者启动目录时建表失败。
# 取舍依据与 trade 相同（AGENTS §11.3），且更强：收件箱本来就是弱依赖 —— scene 不等它（不进 DependencyGate，IB-7），
# 缺席时只是 GM 处置不生效、scene 的拉取退避，登录 / 匹配 / 进场都不受影响。fail-closed 仍在 player_inbox 进程自身。
# 调用方是 scene 与运维，不经路由服，与 -GateRouterMode 无关。它向 scene-cmd 发在线提醒，所以登记为 Kafka 命令契约生产者
#（Test-UsesKafkaCommandContract）。单独追加，不改上面的数组。
$services += 'player_inbox'
$optionalServices += 'player_inbox'
# 邮件和排行榜的生产者签名密钥由 go_services.ps1 注入本机开发值（lib/producer_dev_secret.ps1）；SQL 连接来自写者启动目录。
$services += @('mail','rank')
$optionalServices += @('mail','rank')
$skippedServices = @()
$gatewayUrl = 'http://127.0.0.1:8081'
$oldPath = $env:PATH
$oldPassword = $env:LOGIN_DEV_PASSWORD_SHARED_SECRET
$oldRpcPort = $env:RPC_PORT
$oldCommandPartitions = $env:KAFKA_COMMAND_TOPIC_PARTITIONS
$oldCommandGeneration = $env:KAFKA_COMMAND_TOPIC_GENERATION
$oldGateClientRpcRouter = $env:GATE_CLIENT_RPC_ROUTER
$oldGateRunMode = $env:GATE_RUN_MODE
$oldSceneRunMode = $env:SCENE_RUN_MODE
$oldBattleRunMode = $env:BATTLE_RUN_MODE
$oldSqlBootCatalog = $env:MMORPG_SQL_BOOT_CATALOG
$oldAllowedDatabases = $env:MMORPG_DB_ALLOWED_DATABASES
# 资产通道（docs/design/guild-phase2/04-asset-channel.md §4.32）的本机开发密钥来源。
# scene 验签、guild / trade 签名用的是同名环境变量，值必须一致，否则每次资产 RPC 都回 27008。
# 值不写进仓库：由该库在 run/secrets/assetop-dev.env（已被 .gitignore 覆盖）生成一次并复用。
. (Join-Path $PSScriptRoot 'lib/assetop_dev_secret.ps1')
$oldAssetOpSecrets = Backup-AssetOpDevSecrets
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
            # 观战帧 topic 与 deploy/docker-compose.yml 预建清单一致:代号同命令 topic;广播的 16 分区等于 C++ kSpectateBroadcastTopicPartitions。
            @{Name="gate-spectate_g$generation";Partitions=$partitions;RetentionMs=600000},
            @{Name="spectate-bcast_g$generation";Partitions=16;RetentionMs=600000},
            @{Name="transaction_log_topic_g$auditGeneration";Partitions=6;RetentionMs=2592000000},
            @{Name="player_snapshot_topic_g$auditGeneration";Partitions=3;RetentionMs=2592000000}
        )
    }
}
function Assert-LocalKafkaTopic($Spec) {
    $topic = $Spec.Name
    # Kafka 管理命令需要启动 JVM；本机同时编译时可能超过 60 秒，只延长等待、不放宽契约校验。
    $metadata = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--describe','--topic',$topic) 180
    $header = [regex]::Match($metadata.Out,'(?m)^\s*Topic:\s+\S+.*?\bPartitionCount:\s*(\d+)\s+ReplicationFactor:\s*(\d+)')
    if ($metadata.Code -ne 0 -or -not $header.Success) { throw "无法读取 Kafka 主题 $topic 的完整元数据。" }
    if ([long]$header.Groups[1].Value -ne $Spec.Partitions -or [int]$header.Groups[2].Value -ne 1) {
        throw "Kafka 主题 $topic 分区或副本契约不符（应为 $($Spec.Partitions) 分区、1 副本）。保留现有主题，必须协调换代，禁止原地扩分区。"
    }
    $configs = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-configs.sh','--bootstrap-server','localhost:9092','--entity-type','topics','--entity-name',$topic,'--describe','--all') 180
    # 锚定行首，只读取有效值，不能误取 synonyms 内的 broker 默认保留期。
    $retention = [regex]::Match($configs.Out,'(?m)^\s*retention\.ms=(-?\d+)\b')
    $cleanup = [regex]::Match($configs.Out,'(?m)^\s*cleanup\.policy=(\S+)')
    if ($configs.Code -ne 0 -or -not $retention.Success -or [long]$retention.Groups[1].Value -ne $Spec.RetentionMs -or -not $cleanup.Success -or $cleanup.Groups[1].Value -cne 'delete') {
        throw "Kafka 主题 $topic 保留/清理配置不符（应为 retention.ms=$($Spec.RetentionMs)、cleanup.policy=delete），已停止且未修改现有主题。"
    }
}
function Initialize-LocalKafkaTopics($Contract, [string[]]$ComposeArguments) {
    $listing = Invoke-Docker @('exec','kafka','/opt/kafka/bin/kafka-topics.sh','--bootstrap-server','localhost:9092','--list') 180
    if ($listing.Code -ne 0) { throw '无法列出 Kafka 主题，已停止预建。' }
    $existing = @($listing.Out -split '\r?\n' | ForEach-Object { $_.Trim() })
    $missing = @()
    # 所有既有主题先验完，再允许正式初始化器创建缺失项；不删主题、不改分区。
    foreach ($spec in $Contract.Topics) {
        if ($spec.Name -cin $existing) { Assert-LocalKafkaTopic $spec } else { $missing += $spec.Name }
    }
    if ($missing.Count -gt 0) {
        Write-Host "  Create missing Kafka topics: $($missing -join ', ')"
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
    Write-Host "  Kafka contract verified: commands g$($Contract.Generation), $($Contract.Partitions) partitions; audit g$($Contract.AuditGeneration)." -ForegroundColor Green
}
function Wait-Ready([string]$Label, [scriptblock]$Probe, [int]$Seconds = 120) {
    $timer = [Diagnostics.Stopwatch]::StartNew()
    while ($true) {
        if (& $Probe) { Write-Host "  Ready: $Label" -ForegroundColor Green; return }
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
# 网关 PID 以健康检查通过后真正监听 8081 的进程为准。Oracle 的 javapath\java.exe 只是个壳：
# Start-Process 拿到的是壳进程，真正的 JVM 是它的子进程，按壳的 PID 停不掉网关，8081 会一直被占着。
# 监听者不是本仓网关 jar 时不写 PID，并删掉旧记录，避免有人按过期 PID 停错进程。
function Save-GatewayPid([string]$JarPath) {
    $pidPath = Join-Path $serverRoot 'run/pids/gateway_node.pid'
    $listener = Get-NetTCPConnection -State Listen -LocalPort 8081 -ErrorAction SilentlyContinue | Select-Object -First 1
    $owner = if ($listener) { Get-CimInstance Win32_Process -Filter "ProcessId=$($listener.OwningProcess)" -ErrorAction SilentlyContinue }
    if ($owner -and $owner.Name -eq 'java.exe' -and $owner.CommandLine -and $owner.CommandLine.Contains($JarPath)) {
        $owner.ProcessId | Set-Content -LiteralPath $pidPath
        return
    }
    Remove-Item -LiteralPath $pidPath -ErrorAction SilentlyContinue
    Write-Warning 'Could not confirm that port 8081 belongs to this repository''s gateway JVM; run/pids/gateway_node.pid was not written.'
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
# friend / guild / mail 的异步推送经 shared/kafkautil 写 gate-cmd,必须与 gate 使用同一代主题。
# chat v1 只读写 Redis 历史,trade 资产通道走 gRPC,db/router 也不生产命令主题;不纳入此契约。
# 校验与落记录共用名单,防止服务被复用后才在第一次推送时暴露旧 generation。
function Test-UsesKafkaCommandContract([string]$Name) {
    # player_inbox:在线提醒(PlayerInboxNotifyEvent)发往 scene-cmd,与 guild / friend 的推送同为命令主题生产者。
    return $Name -in @('gate','scene','battle','login','scene_manager','player_locator','match','guild','friend','mail','player_inbox')
}
# 已运行进程不会重新读取进程环境；记录本启动器实际拉起的 PID 与启动时间，防止换代后复用旧进程。
function Assert-RunningCommandContract($Process) {
    $name = [IO.Path]::GetFileNameWithoutExtension($Process.Name)
    if (-not (Test-UsesKafkaCommandContract $name)) { return }
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
    if (-not (Test-UsesKafkaCommandContract $Name)) { return }
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
        # 此入口固定使用本机 Docker MySQL；只有 db 与 match 派生运行副本，生产源的 TiDB 要求不变。
        Invoke-Dev ('go-' + ($Names -join '-')) @('-Command','go-svc-start-exe','-GoServices',($Names -join ','),'-TierReadySeconds','30','-LocalMySQL')
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
    Write-Step '1/6 Check the local game runtime environment'
    . (Join-Path $PSScriptRoot 'lib/release_common.ps1')
    # 网关的写者授权走和 Go 服务同一个闸门（Get-SqlWriterBootEnvironment）。
    . (Join-Path $PSScriptRoot 'maintenance_drain.ps1')
    $kafkaContract = Get-LocalKafkaContract
    $script:docker = Find-Program 'docker.exe' @((Join-Path $env:LOCALAPPDATA 'Programs/DockerDesktop/resources/bin/docker.exe'),(Join-Path $env:ProgramFiles 'Docker/Docker/resources/bin/docker.exe'))
    $desktop = Find-Program 'Docker Desktop.exe' @((Join-Path $env:LOCALAPPDATA 'Programs/DockerDesktop/Docker Desktop.exe'),(Join-Path $env:ProgramFiles 'Docker/Docker/Docker Desktop.exe'))
    $java = Find-Program 'java.exe' @($(if ($env:JAVA_HOME) { Join-Path $env:JAVA_HOME 'bin/java.exe' }))
    $javaReply = Invoke-Program $java @('-version')
    if ($javaReply.Code -ne 0 -or ($javaReply.Err + $javaReply.Out) -notmatch 'version "(?<major>\d+)') { throw '无法确认 Java 版本。' }
    if ([int]$Matches.major -lt 23) { throw '网关需要 Java 23 或更高版本，请调整 JAVA_HOME。' }
    $jar = Get-ChildItem -LiteralPath (Join-Path $serverRoot 'java/gateway_node/target') -Filter 'gateway-node-*.jar' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $jar) { throw '缺少已构建的 Java 网关 jar。' }
    foreach ($service in $services) {
        if (-not (Test-Path -LiteralPath (Join-Path $serverRoot "bin/go_services/$service.exe")) -and -not (Test-Path -LiteralPath (Join-Path $serverRoot "go/$service/$service.exe"))) {
            if ($service -in $optionalServices) {
                Write-Warning "Missing $service.exe; skipping $service (its requests will return service unavailable). Other services will start normally; build it and rerun this script."
                $skippedServices += $service
                continue
            }
            throw "缺少 $service.exe，请先构建对应服务。"
        }
    }
    foreach ($node in @('gate','scene','battle')) {
        if (-not (Test-Path -LiteralPath (Join-Path $serverRoot "bin/$node.exe"))) { throw "缺少 bin/$node.exe。" }
    }
    # 写者启动目录的工具：go-svc-build 不带 -GoServices 时一并编出；日常启动不构建。
    foreach ($tool in @('writer_authority','db_migrate','private_control_file')) {
        if (-not (Test-Path -LiteralPath (Join-Path $serverRoot "bin/go_services/$tool.exe"))) { throw "Missing bin/go_services/$tool.exe; run: pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build" }
    }
    if (-not $ClientPath) { $ClientPath = Join-Path (Split-Path -Parent $serverRoot) 'mmorpg-client/Builds/showcase_player/mmorpg.exe' }
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
        Write-Host 'Startup files and runtime environment checks passed; no services started.' -ForegroundColor Green
    } else {
        Write-Step '2/6 Start Docker and database dependencies (preserve existing data)'
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
                        Write-Host '  Backed up the legacy Docker communication directory.'
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
                        Write-Host '  Backed up the legacy Docker socket communication directory.'
                    }
                }
                Start-Process -FilePath $desktop -WindowStyle Hidden
            }
            Wait-Ready 'Docker Linux engine' { try { (Invoke-Docker @('info','--format','{{.OSType}}') 15).Code -eq 0 } catch { $false } } 300
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
        # 所有 Go 命令生产者须继承与 C++、主题预建相同的契约；运行中的进程不能事后改环境。
        $env:KAFKA_COMMAND_TOPIC_PARTITIONS = [string]$kafkaContract.Partitions
        $env:KAFKA_COMMAND_TOPIC_GENERATION = [string]$kafkaContract.Generation
        Write-Host "  Go child Kafka contract (env): generation=$($env:KAFKA_COMMAND_TOPIC_GENERATION), partitions=$($env:KAFKA_COMMAND_TOPIC_PARTITIONS)"
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
        # 写者启动目录：建库建表、给每个实例建独立账号、发单次启动授权。授权启动即消费，所以每次启动都重新生成。
        # 只接受回环数据库；已在运行的实例不重发（go_services 会 skip 它们）。详见 lib/sql_boot_catalog_local.ps1。
        # 玩家库白名单：全球 DS/DB 不从 Zone 推导，本机一区显式给 zone_1_db，调用方已设置时不覆盖。
        if ([string]::IsNullOrWhiteSpace($env:DB_ALLOWED_DATABASES) -and [string]::IsNullOrWhiteSpace($env:MMORPG_DB_ALLOWED_DATABASES)) { $env:MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db' }
        Invoke-Dev 'sql-boot-catalog' @('-Command','sql-boot-catalog-local','-GoServices',((@($services | Where-Object { $_ -notin $skippedServices }) + 'gateway') -join ','))
        $sqlBootRoot = [IO.Path]::GetFullPath((Join-Path $serverRoot 'run/secrets/sql-boot-local'))
        $sqlBootSummary = Get-Content -LiteralPath (Join-Path $sqlBootRoot 'last-run.json') -Raw | ConvertFrom-Json
        $env:MMORPG_SQL_BOOT_CATALOG = $sqlBootSummary.catalog
        foreach ($service in @($sqlBootSummary.skipped)) {
            if ($service -in $optionalServices -and $service -notin $skippedServices) {
                Write-Warning "$service schema step failed; skipping $service. Details: $logDir/sql-boot-catalog.stdout.log"
                $skippedServices += $service
            }
        }
        Start-LocalGoServices @('db','data_service')
        Write-Step '4/6 Start zone 1 Kafka consumers and battle server'
        $env:PATH = (Join-Path $serverRoot 'third_party/protobuf/install_vs2026_dbg/bin') + ';' + $oldPath
        $env:RPC_PORT = $null
        # 路由模式翻转落在部署层（C++ 默认值不改）：gate 启动时读一次 GATE_CLIENT_RPC_ROUTER 并缓存，
        # 必须在 cpp-node-start 之前设好，经 dev_tools 子进程继承；scene / battle / Go 服务继承到也不读。
        $env:GATE_CLIENT_RPC_ROUTER = $GateRouterMode
        # P0-a：GM 客户端指令（GmAddCurrency / GmSetPlayerLevel / GmGrantPet 等六条）默认关闭，
        # 判据是各节点自己的运行模式，默认（未设置）= prod = 拒绝。robot 的 attribute / pet /
        # currency-crash 三个冒烟都靠这些指令造数据，所以本机启动器显式声明 dev。
        # 与 GATE_CLIENT_RPC_ROUTER 同样必须在 cpp-node-start 之前设好，由 dev_tools 子进程继承。
        # 部署链（tools/scripts/k8s_deploy.ps1）从不注入这两个变量 —— 生产恒为 prod，恒关闭。
        # 详见 cpp/nodes/gate/SECURITY.md §3。
        # 只在“没设”时兜底，不覆盖调用者显式声明的值 —— 收口验收要能
        # `$env:GATE_RUN_MODE='prod'` 再起一次，确认 GM 指令确实被拒。
        if ([string]::IsNullOrWhiteSpace($env:GATE_RUN_MODE))  { $env:GATE_RUN_MODE = 'dev' }
        if ([string]::IsNullOrWhiteSpace($env:SCENE_RUN_MODE)) { $env:SCENE_RUN_MODE = 'dev' }
        # battle 的全球事务门禁也依赖显式运行模式；本机默认 dev，保留调用者指定的 prod/test。
        if ([string]::IsNullOrWhiteSpace($env:BATTLE_RUN_MODE)) { $env:BATTLE_RUN_MODE = 'dev' }
        # 资产通道签名密钥（规格 §4.32 / §4.41 第 30 项）：scene 在第 4 步启动、guild 在第 5 步启动，
        # 两边都从这里继承同一把值，否则帮会 / 聚宝斋的每一次资产 RPC 都被判 27008(AssetAuthFailed)。
        # 必须设在 cpp-node-start 之前，由 dev_tools / go_services 子进程继承。
        # 与上面两个运行模式同一条纪律：只在没设时兜底、不回显值、finally 里还原。
        # **仅限本机 dev 的约定**：预发 / 生产的密钥必须由部署侧注入（k8s_deploy.ps1 的
        # Resolve-InjectedSecret -MinLength 32，§4.43 第 29 项），部署链从不调用这里。
        # trade 侧另有开关：go/trade/etc/trade.yaml 的 AssetOp.Enabled 默认 false，注入了密钥也不会自动开。
        Initialize-AssetOpDevSecrets -RepoRoot $serverRoot | Out-Null
        Write-Host "  GM command channel: GATE_RUN_MODE=$($env:GATE_RUN_MODE) / SCENE_RUN_MODE=$($env:SCENE_RUN_MODE) (disabled outside dev/test; production defaults to prod)"
        if ($GateRouterMode -eq '1') {
            Write-Host '  gate routing mode: GATE_CLIENT_RPC_ROUTER=1 (forward through client_rpc_router; battle uses direct battle connections only)'
        } else {
            Write-Host '  gate routing mode: GATE_CLIENT_RPC_ROUTER=0 (direct mode: chat / guild / friend / trade / mail / rank are unreachable; battle still uses direct battle connections)' -ForegroundColor Yellow
        }
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
        Write-Step '5/6 Start login, match, chat, guild services and game gateway'
        # chat / guild 与路由服同为 Tier 1（chat 只依赖 etcd + Redis；guild 另依赖 MySQL 与 data_service）；
        # 直连模式下它们也照样起，只是玩家不可达（见 -GateRouterMode）。
        # 第 1 步因缺 exe 跳过的可选服务不在这里启动，否则 Start-LocalGoServices 会因“没有启动记录”抛错。
        # match 与 guild 启动时互相核对对方的表：两边的表已在第 3 步生成写者启动目录时建好。
        Start-LocalGoServices @(@('client_rpc_router','scene_manager','player_locator','login','match','chat','guild') | Where-Object { $_ -notin $skippedServices })
        # 聚宝斋 trade 单独一批：被跳过（缺 exe，或第 2 步预检发现 mmorpg_trade 未就绪）时不调用，避免把空列表传给 -GoServices（空 = 全部服务）。
        if ('trade' -notin $skippedServices) { Start-LocalGoServices @('trade') }
        # 好友 friend 单独一批，理由与 trade 同：被跳过（缺 exe，或上面预检发现 mmorpg_friend 未就绪）时不调用，
        # 避免把空列表传给 -GoServices（空 = 全部服务）。friend 不拨任何 Go 服务，起在这里即可。
        if ('friend' -notin $skippedServices) { Start-LocalGoServices @('friend') }
        # 玩家收件箱 player_inbox 单独一批，理由同 trade / friend（空列表 = 全部服务）。它依赖第 3 步已起的 data_service
        #（玩家存在校验 / 对账读账本，NonBlock 拨号）；scene 已在第 4 步起来，不等它（弱依赖，IB-7）。
        if ('player_inbox' -notin $skippedServices) { Start-LocalGoServices @('player_inbox') }
        if ('mail' -notin $skippedServices) { Start-LocalGoServices @('mail') }
        if ('rank' -notin $skippedServices) { Start-LocalGoServices @('rank') }
        $gatewayProcess = Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'java.exe' -and $_.CommandLine -and $_.CommandLine.Contains($jar.FullName) } | Select-Object -First 1
        if (-not $gatewayProcess) {
            if (Test-Tcp 8081) { throw '8081 端口被其他程序占用，未启动重复网关。' }
            $javaTmp = Join-Path $serverRoot "run/java-tmp-$stamp"
            New-Item -ItemType Directory -Path $javaTmp | Out-Null
            # 网关和 Go 服务一样消费单次启动授权（WriterAuthority），账号和本机档位在每 boot 配置里。
            # Windows 上 Java 经 private_control_file 读写私有文件，路径与 SHA 由目录生成器固定。
            $gatewayConfig = ''
            $gatewayEnvironment = Get-SqlWriterBootEnvironment -InstanceKey 'gateway' -RepoRoot $serverRoot -ServiceName 'gateway' -SelectedRuntimeConfigFile ([ref]$gatewayConfig)
            $gatewayEnvironment['MMORPG_SQL_PRIVATE_FILE_HELPER'] = $sqlBootSummary.privateFileHelper.path
            $gatewayEnvironment['MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256'] = $sqlBootSummary.privateFileHelper.sha256
            $savedGatewayEnvironment = Enter-SqlWriterBootEnvironment -Environment $gatewayEnvironment
            try {
                $gatewayProcess = Start-Process -FilePath $java -WorkingDirectory (Join-Path $serverRoot 'java/gateway_node') -ArgumentList @("`"-Djdk.net.unixdomain.tmpdir=$javaTmp`"",'-jar',"`"$($jar.FullName)`"","`"--spring.config.additional-location=file:$($gatewayConfig.Replace('\','/'))`"") -WindowStyle Hidden -RedirectStandardOutput (Join-Path $logDir 'gateway.stdout.log') -RedirectStandardError (Join-Path $logDir 'gateway.stderr.log') -PassThru
            } finally {
                Restore-SqlWriterBootEnvironment -Saved $savedGatewayEnvironment
            }
        }
        Wait-Ready 'Gateway health check' { try { (Invoke-RestMethod "$gatewayUrl/actuator/health" -TimeoutSec 5).status -eq 'UP' } catch { $false } }
        Save-GatewayPid $jar.FullName
        Write-Step '6/6 Check zone entry'
        Wait-Ready 'Zone 1 open' { try { @((Invoke-RestMethod "$gatewayUrl/api/server-list" -TimeoutSec 5).zones | Where-Object { $_.zone_id -eq 1 -and $_.status -eq 'OPEN' }).Count -gt 0 } catch { $false } }
        Write-Host "`nServer started. Gateway: $gatewayUrl; select zone 1." -ForegroundColor Green
        if ($OpenClient) {
            $existingClient = Get-Process -Name 'mmorpg' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $ClientPath }
            if (-not $existingClient) {
                Start-Process -FilePath $ClientPath -WorkingDirectory (Split-Path -Parent $ClientPath) -ArgumentList @('-screen-fullscreen','0','-screen-width','1600','-screen-height','900','-gateway',$gatewayUrl) -WindowStyle Normal
            }
            Write-Host 'Game window opened.' -ForegroundColor Green
        }
        Write-Host 'You can close the startup window now; the servers will keep running in the background.'
    }
} catch {
    $resultCode = 1
    Write-Host "`nStartup incomplete: $($_.Exception.Message)" -ForegroundColor Red
    if (Test-Path -LiteralPath $logDir) { Write-Host "Log directory: $logDir" }
} finally {
    $env:PATH = $oldPath
    $env:LOGIN_DEV_PASSWORD_SHARED_SECRET = $oldPassword
    $env:RPC_PORT = $oldRpcPort
    $env:KAFKA_COMMAND_TOPIC_PARTITIONS = $oldCommandPartitions
    $env:KAFKA_COMMAND_TOPIC_GENERATION = $oldCommandGeneration
    $env:GATE_CLIENT_RPC_ROUTER = $oldGateClientRpcRouter
    $env:GATE_RUN_MODE = $oldGateRunMode
    $env:SCENE_RUN_MODE = $oldSceneRunMode
    $env:BATTLE_RUN_MODE = $oldBattleRunMode
    $env:MMORPG_SQL_BOOT_CATALOG = $oldSqlBootCatalog
    $env:MMORPG_DB_ALLOWED_DATABASES = $oldAllowedDatabases
    Restore-AssetOpDevSecrets $oldAssetOpSecrets
    if ($transcribing) { Stop-Transcript | Out-Null }
    if ($ownsMutex) { $mutex.ReleaseMutex() }
    if ($mutex) { $mutex.Dispose() }
}
exit $resultCode
