#requires -Version 7
<#
.SYNOPSIS
    Go 本机启动器的端口分配、已有实例复用和监听归属回归。
.DESCRIPTION
    只加载脚本 AST 中的函数;网络、进程查询和启动均使用桩,配置仅写入测试临时目录。
    不启动或停止真实服务,不连接端口,不修改真实 PID 文件与派生配置。
.EXAMPLE
    pwsh -File tools/scripts/tests/go_services_ports.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$launcherPath = Join-Path $PSScriptRoot '../go_services.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($launcherPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
# go_services.ps1 顶层 dot-source 的 MailAdmin 本机令牌库(只加载函数):AST 只抽本文件的函数,不执行顶层语句,mail 用例要它。
. (Join-Path $PSScriptRoot '../lib/mail_admin_dev_token.ps1')
. (Join-Path $PSScriptRoot '../lib/producer_dev_secret.ps1')
. (Join-Path $PSScriptRoot '../lib/assetop_dev_secret.ps1')
. (Join-Path $PSScriptRoot '../lib/account_directory_dev_secret.ps1')
. (Join-Path $PSScriptRoot '../lib/role_fence_dev_secret.ps1')
. (Join-Path $PSScriptRoot '../lib/gate_identity_environment.ps1')
. (Join-Path $PSScriptRoot '../lib/release_common.ps1')

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-go-ports-tests-' + [guid]::NewGuid().ToString('N'))
$RepoRoot = $testRoot
$GoRoot = Join-Path $testRoot 'go'
$GoBinDir = Join-Path $testRoot 'bin'
$DerivedEtcDir = Join-Path $testRoot 'run/etc/go_services'
$LogDir = Join-Path $testRoot 'run/logs'
$PidFile = Join-Path $testRoot 'run/pids.json'
$LegacyPidFile = Join-Path $testRoot 'absent-legacy-pids.json'
$Zone = 0
$ZonePortShift = 1000
$PortStride = 1
$Counts = @{}
$NoTier = $true
$ServiceCatalogue = [ordered]@{
    chat = @{ Dir = 'chat'; Entry = 'chat.go'; Port = 50700; Desc = '聊天'; ConfigFlag = '-f'; ConfigFile = 'etc/chat.yaml'; AllowMultiInstance = $true; Tier = 1 }
    trade = @{ Dir = 'trade'; Entry = 'trade.go'; Port = 50800; Desc = '交易'; ConfigFlag = '-f'; ConfigFile = 'etc/trade.yaml'; AllowMultiInstance = $true; Tier = 1 }
}

function Get-NetTCPConnection {
    [CmdletBinding()] param([string]$State)
    if ($script:NetworkQueryFailed) { throw '网络查询失败桩' }
    if ($State -ne 'Listen') { throw '网络桩只接受监听查询' }
    return $script:Listeners
}
function Get-CimInstance {
    [CmdletBinding()] param([string]$ClassName, [string]$Filter)
    if ($script:ProcessQueryFailed) { throw '进程查询失败桩' }
    if ($ClassName -ne 'Win32_Process') { throw '进程桩只接受 Win32_Process' }
    if ($Filter -match '^ProcessId = (\d+)$') { return $script:Processes[[int]$Matches[1]] }
    if ($Filter -match '^ParentProcessId = (\d+)$') {
        $parentId = [int]$Matches[1]
        return @($script:Processes.Values | Where-Object ParentProcessId -eq $parentId)
    }
    throw "未预期的进程查询: $Filter"
}
function Get-Process {
    [CmdletBinding()] param([int]$Id)
    if ($script:Processes.ContainsKey($Id)) { return [pscustomobject]@{ Id = $Id; HasExited = $false } }
    return $null
}
function Start-Process {
    [CmdletBinding()] param($FilePath, $ArgumentList, $WorkingDirectory, $RedirectStandardOutput, $RedirectStandardError, [switch]$PassThru, $WindowStyle)
    # MailToken:启动那一刻父进程环境里的 MailAdmin 令牌 = 子进程会继承到的值(只给 mail 注入的用例看它)。
    $script:Starts += [pscustomobject]@{ FilePath = $FilePath; Arguments = $ArgumentList; MailToken = [Environment]::GetEnvironmentVariable('MAIL_PRODUCER_SECRET_ADMIN'); ChatDsnPresent = -not [string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable('CHAT_AUTHORITY_DSN', 'Process')); MatchMigrationUserPresent = -not [string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable('MATCH_MIGRATION_USER', 'Process')); MatchMigrationPasswordPresent = -not [string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'Process')) }
    $processPath = if ($script:ProcessPathUnavailable) { '' } else { [string]$FilePath }
    return [pscustomobject]@{ Id = 1000 + $script:Starts.Count; StartTime = [datetime]::Now; Path = $processPath }
}
function Stop-Process { throw '测试禁止终止进程' }
function netsh { return @('开始端口 结束端口', ' 50700 50848 ') }
function Get-Command { param([string]$Name) if ($Name -eq 'go') { return [pscustomobject]@{ Source = 'C:\test-go\go.exe' } }; throw "未预期的工具查询: $Name" }
function Wait-ForStartupBanner { param($LogFile, $ServiceName) }
# 写者启动授权另有 maintenance_drain 回归;这里只认每 boot 配置就是实例自己的配置($cfg 来自调用方 Start-ServiceInstance 的作用域),不碰真实授权原件。
function Get-SqlWriterBootEnvironment {
    param($InstanceKey, $RepoRoot, $ServiceName, [ref]$SelectedRuntimeConfigFile, [ref]$PreparedRuntimeArguments, [switch]$PrepareOnlySupported)
    $SelectedRuntimeConfigFile.Value = $cfg.FullPath
    return [ordered]@{}
}
function Enter-SqlWriterBootEnvironment { param($Environment) return @{} }
function Restore-SqlWriterBootEnvironment { param($Saved) }
function Assert-PublicationStartAllowed { param($RepoRoot) }
function Get-MaintenanceSha256 { param($Path) return 'fixture-sha256' }
function Reset-TestState {
    $script:ExcludedPortRanges = @(,@(50700, 50848))
    $script:Listeners = @()
    $script:Processes = @{}
    $script:Starts = @()
    $script:NetworkQueryFailed = $false
    $script:ProcessQueryFailed = $false
    $script:ProcessPathUnavailable = $false
}
function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}
function New-TestProcess([int]$ProcessId, [string]$Name, [string]$ConfigPath, [int]$ParentProcessId = 0) {
    return [pscustomobject]@{
        ProcessId = $ProcessId; ParentProcessId = $ParentProcessId
        ExecutablePath = Join-Path $GoBinDir "$Name.exe"
        CommandLine = "`"$(Join-Path $GoBinDir "$Name.exe")`" -f `"$ConfigPath`""
    }
}

$chatOuterDsn = [Environment]::GetEnvironmentVariable('CHAT_AUTHORITY_DSN', 'Process')
$outerGateIdentity = [Environment]::GetEnvironmentVariable('MMORPG_INTERNAL_AUTH_PUBLIC_KEYS', 'Process')
# Go 服务只拿验签公钥列表(gate-online-identity.md §10.1);这里是形状合格的夹具值,不连任何服务。
[Environment]::SetEnvironmentVariable('MMORPG_INTERNAL_AUTH_PUBLIC_KEYS', ('b' * 64), 'Process')
try {
    # 仅桩启动器的输入，不连接数据库；真实启动必须提供外部运行账号DSN。
    [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN','fixture@tcp(127.0.0.1:24000)/mmorpg_chat','Process')
    foreach ($name in $ServiceCatalogue.Keys) {
        $configDir = Join-Path $GoRoot "$name/etc"
        New-Item -ItemType Directory -Path $configDir -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $configDir "$name.yaml") -Value "ListenOn: 0.0.0.0:$($ServiceCatalogue[$name].Port)`nZoneId: 1"
    }
    foreach ($configName in @('login','data_service')) { New-Item -ItemType Directory -Path (Join-Path $GoRoot "$configName/etc") -Force | Out-Null }
    Set-Content -LiteralPath (Join-Path $GoRoot 'login/etc/login.yaml') -Value "Node:`n  RedisClient:`n    Host: 127.0.0.1:26379`n    DB: 0"
    Set-Content -LiteralPath (Join-Path $GoRoot 'data_service/etc/data_service.yaml') -Value "SnapshotMySQL:`n  DBName: testdb"
    Add-Content -LiteralPath (Join-Path $GoRoot 'chat/etc/chat.yaml') -Value "`nDSNEnv: CHAT_AUTHORITY_DSN`nBackend: tidb`nMaxOpenConns: 16"

    New-Item -ItemType Directory -Path $GoBinDir, $DerivedEtcDir -Force | Out-Null
    foreach ($name in $ServiceCatalogue.Keys) { Set-Content -LiteralPath (Join-Path $GoBinDir "$name.exe") -Value '进程桩占位文件' }

    Test-Case '单个保留区间在首次查询及缓存读取时都保持数组形状' {
        Reset-TestState
        $script:ExcludedPortRanges = $null
        Assert-Equal $true (Test-PortExcluded 50700) '首次 netsh 输出可识别单区间'
        Assert-Equal $true (Test-PortExcluded 50848) '缓存单区间仍可识别末端'
        Assert-Equal $false (Test-PortExcluded 50849) '保留区间之外不误判'
    }
    Test-Case '同批 chat/trade 共享保留区间时分配不同端口并写入各自配置' {
        Reset-TestState
        $Services = @('chat', 'trade')
        Invoke-Start -UseExe
        $tracked = Get-Content -LiteralPath $PidFile -Raw | ConvertFrom-Json
        Assert-Equal 50849 $tracked.chat.Port 'chat 使用首个可用端口'
        Assert-Equal 50850 $tracked.trade.Port 'trade 避开 chat 尚未开始监听的已计划端口'
        Assert-Match (Get-Content -LiteralPath (Join-Path $DerivedEtcDir 'trade.yaml') -Raw) 'ListenOn: 0.0.0.0:50850' '派生配置与记录一致'
        Assert-Equal 2 $script:Starts.Count '恰好启动两次进程桩'
        Assert-True $script:Starts[0].ChatDsnPresent 'chat取得自己的SQL账号'
        Assert-Equal $false $script:Starts[1].ChatDsnPresent 'trade不得继承chat SQL账号'
    }
    Test-Case '双区及同区多副本为嵌套Prometheus分配不同端口且不改其他Port' {
        Reset-TestState
        $savedZone = $Zone
        $savedOffset = $ZonePortOffset
        $savedStride = $PortStride
        $sourcePath = Join-Path $GoRoot 'trade/etc/trade.yaml'
        $original = [IO.File]::ReadAllText($sourcePath)
        try {
            $source = "ListenOn: 127.0.0.1:50800`nZoneId: 1`nMetricsListenAddr: `":9230`"`nPrometheus:`n  Host: 127.0.0.1`n  Port: 9190`n  Path: /metrics`nOther:`n  Port: 7777`n"
            [IO.File]::WriteAllText($sourcePath, $source)
            $Zone = 990017; $ZonePortOffset = 0; $PortStride = 1
            $first = Resolve-InstanceConfig -Name trade -Info $ServiceCatalogue.trade -Index 1 -Total 1
            Assert-Equal '9190' (Get-YamlScalar -Path $first.FullPath -KeyPath 'Prometheus.Port').Value '第一区保留基准监控端口'
            $Zone = 990018; $ZonePortOffset = 1000
            $second = Resolve-InstanceConfig -Name trade -Info $ServiceCatalogue.trade -Index 1 -Total 1
            Assert-Equal '10190' (Get-YamlScalar -Path $second.FullPath -KeyPath 'Prometheus.Port').Value '第二区监控端口按显式偏移分离'
            Assert-Equal ':10230' (Get-YamlScalar -Path $second.FullPath -KeyPath 'MetricsListenAddr').Value '已有顶层监控地址遵循相同偏移'
            Assert-Equal '7777' (Get-YamlScalar -Path $second.FullPath -KeyPath 'Other.Port').Value '其他配置段的Port不被误改'
            $replica = Resolve-InstanceConfig -Name trade -Info $ServiceCatalogue.trade -Index 2 -Total 2
            Assert-Equal '10191' (Get-YamlScalar -Path $replica.FullPath -KeyPath 'Prometheus.Port').Value '同区第二副本使用独立监控端口'
            Assert-Equal '9190' (Get-YamlScalar -Path $sourcePath -KeyPath 'Prometheus.Port').Value '源配置保持基准值'
        } finally {
            $Zone = $savedZone; $ZonePortOffset = $savedOffset; $PortStride = $savedStride
            [IO.File]::WriteAllText($sourcePath, $original)
        }
    }
    Test-Case '常驻子进程不继承Match迁移凭据且父环境原样恢复' {
        Reset-TestState
        $savedUser = [Environment]::GetEnvironmentVariable('MATCH_MIGRATION_USER', 'Process')
        $savedPassword = [Environment]::GetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'Process')
        try {
            [Environment]::SetEnvironmentVariable('MATCH_MIGRATION_USER', 'fixture-only-migrator', 'Process')
            [Environment]::SetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'fixture-only-password', 'Process')
            $Services = @('chat', 'trade')
            Invoke-Start -UseExe
            Assert-Equal 2 $script:Starts.Count '两个常驻启动都需经过环境隔离'
            foreach ($start in $script:Starts) {
                Assert-Equal $false $start.MatchMigrationUserPresent '子进程不继承DDL账号'
                Assert-Equal $false $start.MatchMigrationPasswordPresent '子进程不继承DDL口令'
            }
            Assert-Equal 'fixture-only-migrator' ([Environment]::GetEnvironmentVariable('MATCH_MIGRATION_USER', 'Process')) '父账号原样恢复'
            Assert-Equal 'fixture-only-password' ([Environment]::GetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'Process')) '父口令原样恢复'
        } finally {
            [Environment]::SetEnvironmentVariable('MATCH_MIGRATION_USER', $savedUser, 'Process')
            [Environment]::SetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', $savedPassword, 'Process')
        }
    }
    Test-Case '协调者直连参与者:-Zone 2 起 RoleFence 段的端口跟着位移,段外的地址不动,没有这一段时原样返回' {
        $source = "Etcd:`n  Endpoints: [127.0.0.1:2379]`nRoleFence:`n  Participants:`n    - Name: guild`n      Endpoints: [127.0.0.1:50300]`n    - Name: friend`n      Endpoints: [127.0.0.1:50400]`nMetricsListenAddr: `":9260`"`n"
        $expected = $source.Replace('127.0.0.1:50300', '127.0.0.1:51300').Replace('127.0.0.1:50400', '127.0.0.1:51400')
        Assert-Equal $expected (ConvertTo-ZoneShiftedRoleFenceConfig -Content $source -ZoneShift 1000) '只动参与者的端口'
        Assert-Equal "ListenOn: 0.0.0.0:9000`n" (ConvertTo-ZoneShiftedRoleFenceConfig -Content "ListenOn: 0.0.0.0:9000`n" -ZoneShift 1000) '没有这一段就没什么可移'
    }
    Test-Case 'Chat派生配置不写登录态段，不再有角色目录的库名' {
        Reset-TestState
        $cfg = Resolve-InstanceConfig -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1
        Assert-Equal $false (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'AccountRoleSchema').Found '身份校验走目录 RPC'
        # 会话身份只认 gate 签名的原件:派生配置不读 login 的 Redis,也不补登录态段。
        Assert-Equal $false (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'GlobalLoginSessions').Found '不再声明登录态开关'
        Assert-Equal $false (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'LoginRedis').Found '不再派生登录态 Redis 段'
        Assert-Equal 'tidb' (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'Backend').Value '本机也不能降级旧Redis权威'
    }
    Test-Case 'Chat配置缺SQL权威字段时在派生前拒绝' {
        Reset-TestState
        $chatConfig = Join-Path $GoRoot 'chat/etc/chat.yaml'
        $original = [IO.File]::ReadAllText($chatConfig)
        try {
            [IO.File]::WriteAllText($chatConfig, $original.Replace('MaxOpenConns: 16', ''))
            Assert-Match (Get-ThrownMessage { Resolve-InstanceConfig -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 }) 'MaxOpenConns' '缺字段只点名字段'
        } finally { [IO.File]::WriteAllText($chatConfig, $original) }
    }
    Test-Case 'Chat缺DSN时在任何子进程前拒绝且不回显凭据' {
        Reset-TestState
        $saved = [Environment]::GetEnvironmentVariable('CHAT_AUTHORITY_DSN', 'Process')
        try {
            [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN', $null, 'Process')
            $message = Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids ([pscustomobject]@{}) -UseExe }
            Assert-Match $message 'CHAT_AUTHORITY_DSN' '拒绝时只点名字段'
            Assert-Equal 0 $script:Starts.Count '不启动任何进程'
        } finally { [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN', $saved, 'Process') }
    }
    Test-Case '分批启动避开前一批实际监听端口' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50849; OwningProcess = 77 })
        Assert-Equal 50850 (Resolve-BindablePort 50800 'trade') '不能把已运行 chat 的监听端口再分配给 trade'
    }
    Test-Case '同批稍后处理的现存 trade 尚未 LISTEN 时,chat 仍避开其实际端口' {
        Reset-TestState
        $configPath = Join-Path $DerivedEtcDir 'trade.yaml'
        Set-Content -LiteralPath $configPath -Value 'trade 仍在初始化,配置不可覆盖'
        $script:Processes[77] = New-TestProcess 77 'trade' $configPath
        $pids = [pscustomobject]@{ trade = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'trade' } }
        Write-PidFile $pids
        $Services = @('chat', 'trade')
        Invoke-Start -UseExe
        $tracked = Read-PidFile
        Assert-Equal 50850 $tracked.chat.Port '先启动的 chat 不能占用后续现存 trade 的端口'
        Assert-Equal 50849 $tracked.trade.Port 'trade 保留原有挪移端口'
        Assert-Equal 1 $script:Starts.Count '只启动新 chat'
        Assert-Match (Get-Content -LiteralPath $configPath -Raw) 'trade 仍在初始化,配置不可覆盖' '复用未就绪实例也不改写配置'
    }
    Test-Case '未请求的现存实例尚未 LISTEN 时,分批启动也预留其端口' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' (Join-Path $DerivedEtcDir 'trade.yaml')
        Write-PidFile ([pscustomobject]@{ trade = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'trade' } })
        $Services = @('chat')
        Invoke-Start -UseExe
        Assert-Equal 50850 (Read-PidFile).chat.Port '不在请求列表里的初始化实例也不可被抢占端口'
    }
    Test-Case '预留其他 zone 的实例时按实例键确认配置身份' {
        Reset-TestState
        $Zone = 2
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Processes[88] = New-TestProcess 88 'chat' (Join-Path $DerivedEtcDir 'z1_chat.yaml')
        $pids = [pscustomobject]@{
            trade = [pscustomobject]@{ Pid = 77; Port = 50850; Service = 'trade' }
            z1_chat = [pscustomobject]@{ Pid = 88; Port = 50849; Service = 'chat' }
        }
        $reserved = @{}
        Reserve-RunningInstancePorts -Pids $pids -ReservedPorts $reserved
        Assert-Equal 'trade' $reserved[50850] '当前请求 zone 不应使默认区的原始配置被误拒'
        Assert-Equal 'z1_chat' $reserved[50849] '其他区的派生配置按实例键核对'
    }
    Test-Case '保留区间出口避开尚未启动实例的原始端口' {
        Reset-TestState
        $reserved = @{ 50849 = 'later' }
        Assert-Equal 50850 (Resolve-BindablePort 50700 'chat' $reserved) '后续实例的原始端口已预留'
    }
    Test-Case '原始端口遭无关进程占用时报错且保留 PID 诊断' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50600; OwningProcess = 987 })
        $message = Get-ThrownMessage { Resolve-BindablePort 50600 'router' }
        Assert-Match $message '原始端口 50600 已占用.*PID=987' '显式端口冲突不可静默换端口'
        Assert-Equal 0 $script:Starts.Count '诊断过程不启动任何进程'
    }
    Test-Case '网络或进程查询失败时中止,不能当作空闲端口或消失实例' {
        Reset-TestState
        $script:NetworkQueryFailed = $true
        Assert-Match (Get-ThrownMessage { Resolve-BindablePort 50600 'router' }) '网络查询失败桩' '查询失败不可变成空闲端口'
        $script:ProcessQueryFailed = $true
        $pids = [pscustomobject]@{ chat = 77 }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '进程查询失败桩' 'CIM 失败不可吞掉并启动重复实例'
        Assert-Equal 0 $script:Starts.Count '失败没有启动副作用'
    }
    Test-Case '搜索到端口上界仍没有候选时报错' {
        Reset-TestState
        $script:ExcludedPortRanges = @(,@(65534, 65535))
        Assert-Match (Get-ThrownMessage { Resolve-BindablePort 65534 'chat' }) '没有可用候选' '不可返回越界或仍被保留的端口'
    }
    Test-Case '已运行实例先核对身份再复用,派生配置逐字保持' {
        Reset-TestState
        $configPath = Join-Path $DerivedEtcDir 'chat.yaml'
        Set-Content -LiteralPath $configPath -Value '运行中的专属配置,不能覆盖'
        $before = [System.IO.File]::ReadAllBytes($configPath)
        $script:Processes[77] = New-TestProcess 77 'chat' $configPath
        $pids = [pscustomobject]@{ chat = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'chat' } }
        $reserved = @{}
        Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe -ReservedPorts $reserved
        Assert-Equal ([Convert]::ToBase64String($before)) ([Convert]::ToBase64String([System.IO.File]::ReadAllBytes($configPath))) '重复启动不能改写运行配置'
        Assert-Equal 0 $script:Starts.Count '正确实例被复用'
        Assert-Equal 'chat' $reserved[50849] '复用端口加入本批预留'
    }
    Test-Case '刚启动的 exe 暂时未返回模块路径时仍保存 PID 记录' {
        Reset-TestState
        $script:ProcessPathUnavailable = $true
        $pids = [pscustomobject]@{}
        $null = Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe
        Assert-Equal $script:Starts[0].FilePath $pids.chat.ExecutablePath '保存传给 Start-Process 的已解析 exe 路径'
        Assert-Equal 1001 $pids.chat.Pid '保存已启动进程的 PID'
    }
    Test-Case '复用同一可执行文件也必须匹配配置实例' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'chat' (Join-Path $DerivedEtcDir 'z2_chat.yaml')
        $pids = [pscustomobject]@{ chat = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'chat' } }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '配置不匹配' '别的区服实例不能被误认'
        Assert-Equal 0 $script:Starts.Count '身份冲突不做进程操作'
    }
    Test-Case 'PID 被另一个可执行文件复用时拒绝跳过启动' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' (Join-Path $DerivedEtcDir 'chat.yaml')
        $pids = [pscustomobject]@{ chat = 77 }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '可执行文件或配置不匹配' '旧格式 PID 也必须验证身份'
    }
    Test-Case 'go run 复用必须匹配本仓库的绝对源码路径' {
        Reset-TestState
        $process = [pscustomobject]@{ ExecutablePath = 'C:\test-go\go.exe'; CommandLine = "go.exe run `"$(Join-Path $GoRoot 'chat/chat.go')`" -f etc/chat.yaml" }
        Assert-Equal $true (Test-InstanceProcessIdentity -Name chat -Info $ServiceCatalogue.chat -InstanceKey chat -Total 1 -Process $process) '绝对源码路径与配置共同确认身份'
        $process.CommandLine = 'go.exe run chat.go -f etc/chat.yaml'
        Assert-Equal $false (Test-InstanceProcessIdentity -Name chat -Info $ServiceCatalogue.chat -InstanceKey chat -Total 1 -Process $process) '相对路径不能证明属于本仓库'
    }
    Test-Case '无关 PID 监听不能让目标进程被判就绪' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50849; OwningProcess = 88 })
        Assert-Equal $false (Wait-TcpListenReady -Port 50849 -ProcessId 77 -TimeoutSeconds 0) 'chat 的端口不能充当 trade 的就绪证据'
    }
    Test-Case '目标 PID 实际监听才判就绪' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 77 })
        Assert-Equal $true (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) '监听归属正确'
    }
    Test-Case 'go run 允许自己的子进程监听,直接 exe 不接受子进程代听' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'go' 'etc/trade.yaml'
        $script:Processes[88] = New-TestProcess 88 'trade' 'etc/trade.yaml' 77
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 88 })
        Assert-Equal $true (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0 -AllowChildProcess) 'go run 包装进程的实际服务子进程可就绪'
        Assert-Equal $false (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) 'exe 模式严格要求目标 PID'
    }
    Test-Case '只给 mail 注入 MailAdmin 令牌:起 mail 时子进程拿得到(≥32 字节),起 chat 时拿不到;启动后父进程还原为没设' {
        Reset-TestState
        $envName = 'MAIL_PRODUCER_SECRET_ADMIN'
        $outerToken = [Environment]::GetEnvironmentVariable($envName)
        Remove-Item "Env:$envName" -ErrorAction SilentlyContinue
        $ServiceCatalogue['mail'] = @{ Dir = 'mail'; Entry = 'mail.go'; Port = 50900; Desc = '邮件'; ConfigFlag = '-f'; ConfigFile = 'etc/mail.yaml'; AllowMultiInstance = $true; Tier = 1 }
        try {
            # 临时根当仓库根:令牌库只认有 go/mail/etc/mail.yaml 的目录,下面这份派生配置的源文件正好就是它。
            New-Item -ItemType Directory -Path (Join-Path $GoRoot 'mail/etc') -Force | Out-Null
            Set-Content -LiteralPath (Join-Path $GoRoot 'mail/etc/mail.yaml') -Value "ListenOn: 0.0.0.0:50900`nZoneId: 1"
            Set-Content -LiteralPath (Join-Path $GoBinDir 'mail.exe') -Value '进程桩占位文件'
            $Services = @('chat', 'mail')
            Invoke-Start -UseExe 6>$null
            $mailStart = @($script:Starts | Where-Object { $_.FilePath -like '*mail.exe' })
            $chatStart = @($script:Starts | Where-Object { $_.FilePath -like '*chat.exe' })
            Assert-Equal 1 $mailStart.Count '夹具前提:mail 恰好启动一次'
            Assert-Equal 1 $chatStart.Count '夹具前提:chat 恰好启动一次'
            Assert-True ($null -ne $mailStart[0].MailToken -and $mailStart[0].MailToken.Trim().Length -ge 32) '起 mail 那一刻环境里有 ≥32 字节的令牌(go/mail 不足 32 字节即静默停用 MailAdmin)'
            Assert-Equal $null $chatStart[0].MailToken '起 chat 时环境里没有令牌(最小权限:只给 mail)'
            Assert-Equal $null ([Environment]::GetEnvironmentVariable($envName)) '启动后父进程还原为没设(后面起的服务不继承)'
            Assert-True (Test-Path -LiteralPath (Join-Path $testRoot 'run/secrets/mail-admin-producer-dev.env')) '令牌落在 RepoRoot 的 run/secrets 下(run/* 已被 .gitignore 覆盖)'
        }
        finally {
            $ServiceCatalogue.Remove('mail')
            if ($null -eq $outerToken) { Remove-Item "Env:$envName" -ErrorAction SilentlyContinue }
            else { [Environment]::SetEnvironmentVariable($envName, $outerToken) }
        }
    }
    Test-Case '目标已经退出时不能被残留监听判就绪' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 77 })
        Assert-Equal $false (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) '先核对目标存活'
    }
    Test-Case '配置区号只认唯一一处:login、guild 的 Node.ZoneId 认得出,两处都有或都没有就拒绝' {
        $path = Join-Path $testRoot 'zone-id.yaml'
        Set-Content -LiteralPath $path -Value "Node:`n  ZoneId: 990017`n"
        Assert-Equal 990017 (Get-InstanceConfigZoneId -ConfigPath $path) 'login、guild 的形状'
        Set-Content -LiteralPath $path -Value "ZoneId: 7`n"
        Assert-Equal 7 (Get-InstanceConfigZoneId -ConfigPath $path) '顶层形状'
        Set-Content -LiteralPath $path -Value "ZoneId: 7`nNode:`n  ZoneId: 7`n"
        Assert-Match (Get-ThrownMessage { Get-InstanceConfigZoneId -ConfigPath $path }) 'Both ZoneId and Node\.ZoneId' '两处都有不猜'
        Set-Content -LiteralPath $path -Value "ListenOn: 0.0.0.0:1`n"
        Assert-Match (Get-ThrownMessage { Get-InstanceConfigZoneId -ConfigPath $path }) 'Neither ZoneId nor Node\.ZoneId' '都没有不猜'
    }
    Test-Case '启动时配置的区号或端口和本次实例对不上就拒绝,Node.ZoneId 的配置能过' {
        Reset-TestState
        $Zone = 2
        $selected = Join-Path $testRoot 'selected-trade.yaml'
        function Get-SqlWriterBootEnvironment {
            param($InstanceKey, $RepoRoot, $ServiceName, [ref]$SelectedRuntimeConfigFile, [ref]$PreparedRuntimeArguments, [switch]$PrepareOnlySupported)
            Set-Content -LiteralPath $selected -Value $script:SelectedTemplate.Replace('PORT', [string]$cfg.Port)
            $SelectedRuntimeConfigFile.Value = $selected
            return [ordered]@{}
        }
        $start = { Start-ServiceInstance -Name trade -Info $ServiceCatalogue.trade -Index 1 -Total 1 -Pids ([pscustomobject]@{}) -UseExe }
        $script:SelectedTemplate = "ListenOn: 0.0.0.0:PORT`nZoneId: 1`n"
        Assert-Match (Get-ThrownMessage $start) 'ZoneId与本次实例不符' '目录里是别的区的配置'
        $script:SelectedTemplate = "ListenOn: 0.0.0.0:1`nZoneId: 2`n"
        Assert-Match (Get-ThrownMessage $start) 'ListenOn与本次实例固定端口不符' '端口和计划不一致'
        Assert-Equal 0 $script:Starts.Count '都没有起进程'
        $script:SelectedTemplate = "ListenOn: 0.0.0.0:PORT`nNode:`n  ZoneId: 2`n"
        $null = & $start
        Assert-Equal 1 $script:Starts.Count 'Node.ZoneId 的配置过得了'
    }
    Test-Case '端口偏移:缺省原公式,语义区号代入原公式越界,显式偏移只动端口不动区号' {
        Reset-TestState
        $Zone = 2
        Assert-Equal 51800 (Get-InstancePort -Info $ServiceCatalogue.trade -Index 1) '缺省原公式'
        $Zone = 990017
        Assert-Match (Get-ThrownMessage { Resolve-BindablePort (Get-InstancePort -Info $ServiceCatalogue.trade -Index 1) 'z990017_trade' }) '无效端口' '原公式越界'
        $ZonePortOffset = 0
        Assert-Equal 50800 (Get-InstancePort -Info $ServiceCatalogue.trade -Index 1) '显式偏移 0'
        $ZonePortOffset = 1000
        Assert-Equal 'z990017_' (Get-ZonePrefix) '实例键仍是语义区号'
        $cfg = Resolve-InstanceConfig -Name trade -Info $ServiceCatalogue.trade -Index 1 -Total 1
        Assert-Equal 51800 $cfg.Port '显式偏移 1000'
        Assert-Equal 990017 (Get-InstanceConfigZoneId -ConfigPath $cfg.FullPath) '配置写语义区号,偏移不当区号'
    }
    Test-Case 'RoleFence 回填:跟实际端口,Etcd 形式不动,缺参与者和多副本拒绝' {
        $source = "RoleFence:`n  Participants:`n    - Name: guild`n      Endpoints: [127.0.0.1:51300]`n    - Name: trade`n      Endpoints: [127.0.0.1:51800]`n    - Name: friend`n      Etcd:`n        Hosts: [127.0.0.1:2379]`n        Key: friend.rpc.z2`n`nMetricsListenAddr: `":10260`"`n"
        Assert-Equal $source.Replace('127.0.0.1:51800', '127.0.0.1:51801') (ConvertTo-PlannedRoleFenceConfig -Content $source -PortsByService @{ guild = @(51300); trade = @(51801) }) '只改实际端口变了的参与者,其余字节不动'
        Assert-Match (Get-ThrownMessage { ConvertTo-PlannedRoleFenceConfig -Content $source -PortsByService @{ guild = @(51300) } }) 'trade is not in this zone' '缺参与者不猜'
        Assert-Match (Get-ThrownMessage { ConvertTo-PlannedRoleFenceConfig -Content $source -PortsByService @{ guild = @(51300); trade = @(51800, 51801) } }) 'cannot address them uniquely' '多副本不猜'
        Assert-Match (Get-ThrownMessage { ConvertTo-PlannedRoleFenceConfig -Content $source.Replace('[127.0.0.1:51300]', '[127.0.0.1:51300, 127.0.0.1:51301]') -PortsByService @{ guild = @(51300); trade = @(51800) } }) 'exactly one direct endpoint' '多个直连地址不猜'
        Assert-Equal "ListenOn: 0.0.0.0:9000`n" (ConvertTo-PlannedRoleFenceConfig -Content "ListenOn: 0.0.0.0:9000`n" -PortsByService @{}) '没有这一段就没什么可填'
    }
    Test-Case 'plan-instances:RoleFence 随 trade 的实际避让端口,缺参与者就拒绝' {
        Reset-TestState
        $script:ExcludedPortRanges = @(,@(51800, 51800))
        $Zone = 2
        $ServiceCatalogue['data_service'] = @{ Dir = 'data_service'; Entry = 'data_service.go'; Port = 9000; Desc = '夹具'; ConfigFlag = '-f'; ConfigFile = 'etc/data_service.yaml'; AllowMultiInstance = $true; Tier = 0 }
        $dataServiceConfig = Join-Path $GoRoot 'data_service/etc/data_service.yaml'
        $originalConfig = Get-Content -LiteralPath $dataServiceConfig -Raw
        $savedTiDbHost = [Environment]::GetEnvironmentVariable('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST', 'Process')
        try {
            [Environment]::SetEnvironmentVariable('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST', $null, 'Process')
            Set-Content -LiteralPath $dataServiceConfig -Value "ListenOn: 0.0.0.0:9000`nZoneId: 1`nRoleFence:`n  Participants:`n    - Name: trade`n      Endpoints: [127.0.0.1:50800]`n`nSnapshotMySQL:`n  DBName: testdb`n"
            $Services = @('data_service', 'trade')
            $InstancePlanOutput = Join-Path $testRoot 'plan-z2.json'
            Invoke-PlanInstances
            $plan = @(Get-Content -LiteralPath $InstancePlanOutput -Raw | ConvertFrom-Json)
            Assert-Equal 51801 @($plan | Where-Object { $_.Service -eq 'trade' })[0].Port 'trade 挪出保留区'
            $planned = @($plan | Where-Object { $_.Service -eq 'data_service' })[0]
            Assert-Match (Get-Content -LiteralPath $planned.ConfigPath -Raw) 'Endpoints: \[127\.0\.0\.1:51801\]' '协调者连实际端口,不是算术位移的 51800'
            $Services = @('data_service')
            Assert-Match (Get-ThrownMessage { Invoke-PlanInstances }) 'trade is not in this zone' '计划里没有参与者就拒绝'
        } finally {
            $ServiceCatalogue.Remove('data_service')
            [Environment]::SetEnvironmentVariable('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST', $savedTiDbHost, 'Process')
            Set-Content -LiteralPath $dataServiceConfig -Value $originalConfig -NoNewline
        }
    }
}
finally {
    [Environment]::SetEnvironmentVariable('MMORPG_INTERNAL_AUTH_PUBLIC_KEYS', $outerGateIdentity, 'Process')
    [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN',$chatOuterDsn,'Process')
    $resolvedRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-go-ports-tests-*') {
        throw "拒绝清理测试临时目录范围之外的路径: $resolvedRoot"
    }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'go_services 端口与实例归属')
