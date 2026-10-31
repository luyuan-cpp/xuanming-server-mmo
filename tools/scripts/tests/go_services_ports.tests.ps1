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
. (Join-Path $PSScriptRoot '../lib/account_directory_dev_secret.ps1')
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
    return [pscustomobject]@{ Id = 1000 + $script:Starts.Count }
}
function Stop-Process { throw '测试禁止终止进程' }
function netsh { return @('开始端口 结束端口', ' 50700 50848 ') }
function Get-Command { param([string]$Name) if ($Name -eq 'go') { return [pscustomobject]@{ Source = 'C:\test-go\go.exe' } }; throw "未预期的工具查询: $Name" }
function Wait-ForStartupBanner { param($LogFile, $ServiceName) }
function Reset-TestState {
    $script:ExcludedPortRanges = @(,@(50700, 50848))
    $script:Listeners = @()
    $script:Processes = @{}
    $script:Starts = @()
    $script:NetworkQueryFailed = $false
    $script:ProcessQueryFailed = $false
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
    Add-Content -LiteralPath (Join-Path $GoRoot 'chat/etc/chat.yaml') -Value "`nDSNEnv: CHAT_AUTHORITY_DSN`nBackend: tidb`nGlobalLoginSessions: true`nMaxOpenConns: 16`nAccountRoleSchema: mmorpg_global`nLoginRedis:`n  Host: 127.0.0.1:6379`n  PasswordEnv: LOGIN_REDIS_PASSWORD`n  DB: 0`n  PoolSize: 4"

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
    Test-Case 'Chat派生目录和原登录Redis取现有权威配置，不按zone分裂' {
        Reset-TestState
        $cfg = Resolve-InstanceConfig -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1
        Assert-Equal 'testdb' (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'AccountRoleSchema').Value '本地角色目录取data_service全局库'
        Assert-Equal '127.0.0.1:26379' (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'LoginRedis.Host').Value '地址与login一致'
        Assert-Equal '0' (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'LoginRedis.DB').Value 'DB与login一致'
        Assert-Equal 'tidb' (Get-YamlScalar -Path $cfg.FullPath -KeyPath 'Backend').Value '本机也不能降级旧Redis权威'
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
}
finally {
    [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN',$chatOuterDsn,'Process')
    $resolvedRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-go-ports-tests-*') {
        throw "拒绝清理测试临时目录范围之外的路径: $resolvedRoot"
    }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'go_services 端口与实例归属')
