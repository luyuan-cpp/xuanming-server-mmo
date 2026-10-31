#requires -Version 7
<#
.SYNOPSIS
    本机 MySQL 配置派生与正式启动入口传递回归。
.DESCRIPTION
    从 AST 加载现役函数，进程和端口全用桩；仅写独立临时目录，不连接数据库或启动服务。
#>
param([string]$LauncherPath = (Join-Path $PSScriptRoot '../go_services.ps1'))
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/account_directory_dev_secret.ps1"
. "$PSScriptRoot/../lib/match_dev_secret.ps1"
. "$PSScriptRoot/../lib/release_common.ps1"
$parseErrors = $null
$launcherAst = [System.Management.Automation.Language.Parser]::ParseFile($LauncherPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($definition in $launcherAst.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-local-mysql-tests-' + [guid]::NewGuid().ToString('N'))
$RepoRoot = $testRoot
$GoRoot = Join-Path $testRoot 'go'
$GoBinDir = Join-Path $testRoot 'bin'
$DerivedEtcDir = Join-Path $testRoot 'run/etc/go_services'
$LogDir = Join-Path $testRoot 'run/logs'
$PidFile = Join-Path $testRoot 'run/pids/go_services.pid.json'
$LegacyPidFile = Join-Path $testRoot 'absent-legacy.json'
$Zone = 0
$ZonePortShift = 1000
$PortStride = 1
$Counts = @{}
$NoTier = $true
$LocalMySQL = $false
$ServiceCatalogue = [ordered]@{
    db = @{ Dir = 'db'; Entry = 'db.go'; Port = 6000; ConfigFlag = '-f'; ConfigFile = 'etc/db.yaml'; AllowMultiInstance = $true; Tier = 0 }
    data_service = @{ Dir = 'data_service'; Entry = 'data_service.go'; Port = 9000; ConfigFlag = '-f'; ConfigFile = 'etc/data_service.yaml'; AllowMultiInstance = $true; Tier = 0 }
    match = @{ Dir = 'match'; Entry = '.'; Port = 50500; ConfigFlag = '-f'; ConfigFile = 'etc/match_service.yaml'; AllowMultiInstance = $true; Tier = 3 }
    guild = @{ Dir = 'guild'; Entry = 'guild.go'; Port = 50750; ConfigFlag = '-f'; ConfigFile = 'etc/guild.yaml'; AllowMultiInstance = $true; Tier = 1 }
}
$fixture = @'
ListenOn: 0.0.0.0:6000
ZoneId: 1
ServerConfig:
  Database:
    Backend: tidb # 保持源严格配置
    Hosts: "127.0.0.1:3306"
    User: fixture-user
    Passwd: fixture-only
    AutoCreateDatabase: false
    AutoMigrateSchema: false
  PlayerStore:
    MaxRowBytes: 1048576
'@
$fixture = ($fixture -replace '\r?\n', "`r`n") + "`r`n"
$matchFixture = @'
Name: matchservice.rpc
GlobalLoginSessions: true
LoginRedis:
  Addrs: [127.0.0.1:6379]
  DB: 0
  PasswordEnv: MMORPG_LOGIN_REDIS_PASSWORD
  PoolSizePerNode: 8
# 源配置保持 TiDB 入口
GlobalMatch:
  Backend: tidb
  Host: 127.0.0.1:4000
  User: root
  Database: mmorpg_match
  AccountRoleSchema: mmorpg_global
  OutcomeOutbox:
    IntervalMs: 2000
ListenOn: 127.0.0.1:50500
'@
$matchFixture = ($matchFixture -replace '\r?\n', "`n") + "`n"
$guildFixture = @'
Name: guild.rpc
MySQL:
  # 档位注释
  Backend: tidb
  DataSource: "appuser:fixture-app-password@tcp(127.0.0.1:3306)/mmorpg_guild?charset=utf8mb4"
  MaxOpenConn: 20

Schema:
  AutoMigrate: false
'@
$guildFixture = ($guildFixture -replace '\r?\n', "`n") + "`n"
$sourceConfig = Join-Path $GoRoot 'db/etc/db.yaml'
$realSourceConfig = Join-Path $PSScriptRoot '../../../go/db/etc/db.yaml'
$realSourceHash = (Get-FileHash -LiteralPath $realSourceConfig -Algorithm SHA256).Hash
$oldRpcPort = $env:RPC_PORT
$oldGlobalTiDBHost = $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST
$oldDatabaseAllowlist = $env:DB_ALLOWED_DATABASES
function Resolve-BindablePort { param([int]$Port, [string]$Label, [hashtable]$ReservedPorts) if ($script:PortOverride) { return $script:PortOverride }; return $Port }
function Start-Process {
    [CmdletBinding()] param($FilePath, $ArgumentList, $WorkingDirectory, $RedirectStandardOutput, $RedirectStandardError, [switch]$PassThru, $WindowStyle)
    $script:Starts += [pscustomobject]@{ FilePath = $FilePath; Arguments = $ArgumentList; WorkingDirectory = $WorkingDirectory; WindowStyle = $WindowStyle; ProbeSecret = $env:MATCH_PROBE_SECRET; OutcomeSecret = $env:MMORPG_MATCH_OUTCOME_SECRET_BATTLE; InboxSecret = $env:MMORPG_PLAYER_INBOX_SECRET_MATCH; RankSecret = $env:RANK_PRODUCER_SECRET_MATCH; MigrationPassword = $env:MATCH_MIGRATION_PASSWORD }
    return [pscustomobject]@{ Id = 901; StartTime = [datetime]::UtcNow; Path = $FilePath }
}
function Get-CimInstance { throw '测试禁止查询真实进程' }
function Stop-Process { throw '测试禁止终止进程' }
function Wait-ForStartupBanner { param($LogFile, $ServiceName) }
# 写者启动授权另有 maintenance_drain 回归;这里只认每 boot 配置就是派生配置,不碰真实授权原件。
function Get-SqlWriterBootEnvironment {
    param($InstanceKey, $RepoRoot, $ServiceName, [ref]$SelectedRuntimeConfigFile, [ref]$PreparedRuntimeArguments, [switch]$PrepareOnlySupported)
    $SelectedRuntimeConfigFile.Value = Join-Path $DerivedEtcDir "$ServiceName.yaml"
    return [ordered]@{}
}
function Enter-SqlWriterBootEnvironment { param($Environment) return @{} }
function Restore-SqlWriterBootEnvironment { param($Saved) }
function Assert-PublicationStartAllowed { param($RepoRoot) }
function Get-MaintenanceSha256 { param($Path) return 'fixture-sha256' }
function Write-MatchFixture {
    Reset-Fixture
    foreach ($service in @('match', 'guild', 'login', 'data_service')) { New-Item -ItemType Directory -Path (Join-Path $GoRoot "$service/etc") -Force | Out-Null }
    Set-Content -LiteralPath (Join-Path $GoRoot 'match/etc/match_service.yaml') -Value $matchFixture -Encoding utf8 -NoNewline
    Set-Content -LiteralPath (Join-Path $GoRoot 'guild/etc/guild.yaml') -Value $guildFixture -Encoding utf8 -NoNewline
    Set-Content -LiteralPath (Join-Path $GoRoot 'login/etc/login.yaml') -Value "Node:`n  RedisClient:`n    Host: 127.0.0.1:26379`n    DB: 2`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $GoRoot 'data_service/etc/data_service.yaml') -Value "SnapshotMySQL:`n  DBName: fixture_global`n" -NoNewline
    foreach ($path in @((Join-Path $DerivedEtcDir 'match.yaml'), (Join-Path $RepoRoot 'run/secrets/match-dev.env'))) {
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
    }
}
function Reset-Fixture {
    $script:Starts = @()
    $script:PortOverride = 0
    $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = $null
    Set-Content -LiteralPath $sourceConfig -Value $fixture -Encoding utf8 -NoNewline
    foreach ($path in @((Join-Path $DerivedEtcDir 'db.yaml'), $PidFile)) {
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
    }
}
function Get-ThrownMessage([scriptblock]$Action) { try { & $Action | Out-Null } catch { return $_.Exception.Message }; return '' }
try {
    $env:DB_ALLOWED_DATABASES = 'zone_1_db'
    New-Item -ItemType Directory -Path (Split-Path -Parent $sourceConfig), $GoBinDir, $DerivedEtcDir, (Split-Path -Parent $PidFile) -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $GoBinDir 'db.exe') -Value '仅供进程桩识别'
    Test-Case '显式开关默认关闭，单实例仍使用原配置且不生成副本' {
        Reset-Fixture
        $LocalMySQL = $false
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal 'etc/db.yaml' $result.Path '默认路径未改变'
        Assert-Equal $false $result.Derived '默认不派生'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'db.yaml')) '默认不生成可降低要求的副本'
        $parameter = @($launcherAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'LocalMySQL' })
        Assert-Equal 1 $parameter.Count '正式入口显式声明 LocalMySQL'
        Assert-Equal $null $parameter[0].DefaultValue '开关没有默认开启表达式'
    }
    Test-Case '本机副本只改布尔值，源和其余字节不变' {
        Reset-Fixture
        $LocalMySQL = $true
        $sourceHash = (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal (Join-Path $DerivedEtcDir 'db.yaml') $result.Path '标准派生绝对路径'
        Assert-Equal $true $result.Derived '显式本机档必定派生'
        $actual = Get-Content -LiteralPath $result.Path -Raw
        Assert-Equal ($fixture.Replace('Backend: tidb', 'Backend: mysql')) $actual '只替换该布尔值，保留 CRLF 和所有其它配置'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash '源文件字节不变'
    }
    foreach ($address in @('localhost:3306', '[::1]:3306', '127.0.0.2:3306')) {
        Test-Case "回环单端点 $address 可显式使用本机档" {
            $content = $fixture.Replace('127.0.0.1:3306', $address)
            Assert-Equal ($content.Replace('Backend: tidb', 'Backend: mysql')) (ConvertTo-LocalMySQLDbConfig $content) '回环地址可转换且不改其它内容'
        }
    }
    foreach ($address in @('203.0.113.9:3306', 'mysql.example.com:3306', 'localhost.example.com:3306', '127.0.0.1:0', '127.0.0.1:65536', '127.0.0.999:3306')) {
        Test-Case "拒绝远端或畸形单端点 $address" {
            Assert-Match (Get-ThrownMessage { ConvertTo-LocalMySQLDbConfig ($fixture.Replace('127.0.0.1:3306', $address)) }) '回环 SQL 单端点' '不得降低非本机连接的 TiDB 要求'
        }
    }
    Test-Case '重复字段、缺少字段和别名均拒绝，不静默猜测' {
        foreach ($invalid in @(
            $fixture.Replace('    Hosts:', "    Hosts: 127.0.0.1:3306`r`n    Hosts:"),
            $fixture.Replace('    Backend: tidb # 保持源严格配置', ''),
            $fixture.Replace('  Database:', '  Database: *other')
        )) { Assert-True (-not [string]::IsNullOrEmpty((Get-ThrownMessage { ConvertTo-LocalMySQLDbConfig $invalid }))) '歧义配置明确拒绝' }
    }
    Test-Case '远端源在写派生文件或启动进程之前拒绝' {
        Reset-Fixture
        $LocalMySQL = $true
        Set-Content -LiteralPath $sourceConfig -Value ($fixture.Replace('127.0.0.1:3306', '203.0.113.9:3306')) -NoNewline
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1 -Pids ([pscustomobject]@{}) -UseExe }) '回环 SQL 单端点' '远端在启动前拒绝'
        Assert-Equal 0 $script:Starts.Count '没有任何进程调用'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'db.yaml')) '没有生成降级副本'
    }
    Test-Case '全球 SQL 地址覆盖在未启用本机档时保留 TiDB 要求' {
        Reset-Fixture
        $LocalMySQL = $false
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = 'tidb.example.com:4000'
        $sourceHash = (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal $true $result.Derived '显式全球地址仍触发正式派生流程'
        $actual = Get-Content -LiteralPath $result.Path -Raw
        Assert-Match $actual 'Hosts: "tidb\.example\.com:4000"' '进程使用显式全球地址'
        Assert-Match $actual 'Backend: tidb' '未启用本机档不降低 TiDB 要求'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash '源配置不变'
    }
    Test-Case '全球 SQL 地址覆盖不能绕过本机档的回环护栏' {
        Reset-Fixture
        $LocalMySQL = $true
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = 'tidb.example.com:4000'
        $sourceHash = (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1 -Pids ([pscustomobject]@{}) -UseExe }) '回环 SQL 单端点' '按最终覆盖地址拒绝远端降级'
        Assert-Equal 0 $script:Starts.Count '拒绝发生在进程启动之前'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'db.yaml')) '没有生成远端降级副本'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash '拒绝不改源配置'
    }
    Test-Case '全球 SQL 地址覆盖为回环端点时可组合显式本机档' {
        Reset-Fixture
        $LocalMySQL = $true
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = '127.0.0.2:4000'
        $sourceHash = (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal (Join-Path $DerivedEtcDir 'db.yaml') $result.Path '组合开关使用同一正式派生路径'
        $actual = Get-Content -LiteralPath $result.Path -Raw
        Assert-Match $actual 'Hosts: "127\.0\.0\.2:4000"' '最终回环覆盖地址生效'
        Assert-Match $actual 'Backend: mysql' '明确回环端点可以启用本机档'
        Assert-Match $actual 'MaxRowBytes: 1048576' 'combined switches keep the rest of the config'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath $sourceConfig -Algorithm SHA256).Hash '源配置不变'
    }
    Test-Case '端口避让仍使用同一标准派生路径并保留本机档' {
        Reset-Fixture
        $LocalMySQL = $true
        $script:PortOverride = 6001
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal 6001 $result.Port '保留既有端口分配结果'
        Assert-Equal (Join-Path $DerivedEtcDir 'db.yaml') $result.Path '不创建平行配置路径'
        Assert-Match (Get-Content -LiteralPath $result.Path -Raw) 'ListenOn: 0.0.0.0:6001' '配置监听端口一致'
        Assert-Match (Get-Content -LiteralPath $result.Path -Raw) 'Backend: mysql' '避让不丢本机档'
    }
    Test-Case '本机档不改变其他服务的 TiDB 配置' {
        Reset-Fixture
        $LocalMySQL = $true
        $otherConfig = Join-Path $GoRoot 'data_service/etc/data_service.yaml'
        New-Item -ItemType Directory -Path (Split-Path -Parent $otherConfig) -Force | Out-Null
        Set-Content -LiteralPath $otherConfig -Value ($fixture.Replace('127.0.0.1:3306', '203.0.113.9:3306')) -NoNewline
        $result = Resolve-InstanceConfig -Name data_service -Info $ServiceCatalogue.data_service -Index 1 -Total 1
        Assert-Equal $false $result.Derived '其他服务不受本机db档影响'
        Assert-Match (Get-Content -LiteralPath $otherConfig -Raw) 'Backend: tidb' '其他服务的严格要求不变'
    }
    Test-Case '多区仍沿用正式区前缀路径和监听位移' {
        Reset-Fixture
        $LocalMySQL = $true
        $Zone = 2
        $result = Resolve-InstanceConfig -Name db -Info $ServiceCatalogue.db -Index 1 -Total 1
        Assert-Equal (Join-Path $DerivedEtcDir 'z2_db.yaml') $result.Path '多区继续使用正式路径'
        Assert-Equal 7000 $result.Port '沿用区端口位移'
        Assert-Match (Get-Content -LiteralPath $result.Path -Raw) 'Backend: mysql' '多区显式本机档生效'
        Assert-Match (Get-Content -LiteralPath $result.Path -Raw) 'MaxRowBytes: 1048576' 'multi-zone keeps the rest of the config'
    }
    Test-Case '正式进程启动收到派生配置，普通PID身份核对也承认' {
        Reset-Fixture
        $LocalMySQL = $true
        $Services = @('db')
        Invoke-Start -UseExe
        Assert-Equal 1 $script:Starts.Count '恰好调用一次进程桩'
        $derived = Join-Path $DerivedEtcDir 'db.yaml'
        # 启动参数逐个加引号(ConvertTo-ServiceProcessArgument),比较路径本身。
        Assert-Equal $derived ([string]$script:Starts[0].Arguments[1]).Trim('"') 'db进程实参指向本机副本'
        Assert-Equal (Join-Path $GoRoot 'db') $script:Starts[0].WorkingDirectory '相对资源路径仍按原服务目录解析'
        Assert-Equal 'Hidden' $script:Starts[0].WindowStyle '保留独立隐藏控制台'
        $process = [pscustomobject]@{ ExecutablePath = Join-Path $GoBinDir 'db.exe'; CommandLine = "db.exe -f `"$derived`"" }
        Assert-Equal $true (Test-InstanceProcessIdentity -Name db -Info $ServiceCatalogue.db -InstanceKey db -Process $process) '普通管理入口可识别派生路径'
        Assert-Equal 901 (Read-PidFile).db.Pid '正式PID记录落到原路径'
    }
    Test-Case '一键本机入口明确传递 LocalMySQL 给db批次' {
        $startAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../start_game.ps1'), [ref]$null, [ref]$null)
        $definition = @($startAst.FindAll({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Start-LocalGoServices' }, $false))
        . ([scriptblock]::Create($definition[0].Extent.Text))
        $serverRoot = $testRoot
        Write-PidFile ([pscustomobject]@{ db = [pscustomobject]@{ Pid = 901; Port = 6000 } })
        function Invoke-Dev { param($Label, $Arguments) $script:LocalStartArguments = $Arguments }
        function Wait-Ready { param($Label, $Probe, $Timeout) if (-not (& $Probe)) { throw '就绪探针桩失败' } }
        function Get-MatchingProcess { param($ProcessId, $Paths) return $true }
        function Test-Tcp { param($Port) return $true }
        function Save-LocalCommandContract { param($Name, $ProcessId) }
        Start-LocalGoServices @('db')
        Assert-True ($script:LocalStartArguments -contains '-LocalMySQL') '固定DockerMySQL入口显式启用本机档'
        Assert-True ($script:LocalStartArguments -contains 'db') '档位传入真实db批次'
    }
    $driverAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../dev_tools.ps1'), [ref]$null, [ref]$null)
    Test-Case 'K8s入口拒绝本机档，且在任何部署动作前退出' {
        $driverOutput = & pwsh -NoProfile -File (Join-Path $PSScriptRoot '../dev_tools.ps1') -Command k8s-zone-up -LocalMySQL 2>&1 | Out-String
        Assert-True ($LASTEXITCODE -ne 0) '错误组合明确失败'
        Assert-Match $driverOutput '只用于本地 Go 启动命令' '执行到入口拒绝而不是部署'
    }
    foreach ($driverCommand in @('go-svc-start', 'go-svc-start-exe', 'dev-start', 'dev-start-exe', 'dev-start-zones')) {
        Test-Case "$driverCommand 正式转发调用保留显式 LocalMySQL" {
            $dispatch = @($driverAst.FindAll({ param($node) $node -is [Management.Automation.Language.SwitchStatementAst] -and $node.Condition.Extent.Text -eq '$Command' }, $false))[-1]
            $clause = @($dispatch.Clauses | Where-Object { $_.Item1.Value -eq $driverCommand })[0]
            $call = @($clause.Item2.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.Extent.Text.Contains('go_services.ps1') }, $true))[0]
            Assert-Match $call.Extent.Text '-LocalMySQL:\$LocalMySQL' '不静默丢失显式档位'
        }
    }
    Test-Case 'match 本机档只改档位、SQL入口、全局角色库和登录 Redis' {
        Write-MatchFixture
        $LocalMySQL = $true
        $sourceHash = (Get-FileHash -LiteralPath (Join-Path $GoRoot 'match/etc/match_service.yaml') -Algorithm SHA256).Hash
        $result = Resolve-InstanceConfig -Name match -Info $ServiceCatalogue.match -Index 1 -Total 1
        Assert-Equal (Join-Path $DerivedEtcDir 'match.yaml') $result.Path '标准派生路径'
        $expected = $matchFixture.Replace('  Backend: tidb', '  Backend: mysql').Replace('  Host: 127.0.0.1:4000', '  Host: 127.0.0.1:3306').Replace('  AccountRoleSchema: mmorpg_global', '  AccountRoleSchema: fixture_global').Replace('  Addrs: [127.0.0.1:6379]', '  Addrs: [127.0.0.1:26379]').Replace('  DB: 0', '  DB: 2')
        Assert-Equal $expected (Get-Content -LiteralPath $result.Path -Raw) '只改这五个标量，其余字节不变'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath (Join-Path $GoRoot 'match/etc/match_service.yaml') -Algorithm SHA256).Hash '源配置不变'
    }
    Test-Case 'match 未启用本机档时用源配置的 TiDB 入口' {
        Write-MatchFixture
        $LocalMySQL = $false
        $result = Resolve-InstanceConfig -Name match -Info $ServiceCatalogue.match -Index 1 -Total 1
        Assert-Equal $false $result.Derived '默认不派生'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'match.yaml')) '默认不生成副本'
    }
    Test-Case 'match 全球 SQL 地址覆盖只改 GlobalMatch.Host，档位不降' {
        Write-MatchFixture
        $LocalMySQL = $false
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = 'tidb.example.com:4000'
        $actual = Get-Content -LiteralPath (Resolve-InstanceConfig -Name match -Info $ServiceCatalogue.match -Index 1 -Total 1).Path -Raw
        Assert-Match $actual '(?m)^  Host: "tidb\.example\.com:4000"' '进程使用显式全球地址'
        Assert-Match $actual '(?m)^  Backend: tidb' '未启用本机档不降低 TiDB 要求'
        Assert-Match $actual '(?m)^  Addrs: \[127\.0\.0\.1:6379\]' '登录 Redis 地址不受影响'
    }
    Test-Case 'match 本机档拒绝远端 SQL 入口，不写派生文件' {
        Write-MatchFixture
        $LocalMySQL = $true
        Set-Content -LiteralPath $sourceConfig -Value ($fixture.Replace('127.0.0.1:3306', '203.0.113.9:3306')) -NoNewline
        Assert-Match (Get-ThrownMessage { Resolve-InstanceConfig -Name match -Info $ServiceCatalogue.match -Index 1 -Total 1 }) '回环 SQL 单端点' '远端不走本机档'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'match.yaml')) '没有生成降级副本'
    }
    Test-Case 'match 启动拿到开发密钥，不继承建表账号，父环境原样恢复' {
        Write-MatchFixture
        $LocalMySQL = $true
        Set-Content -LiteralPath (Join-Path $GoBinDir 'match.exe') -Value '仅供进程桩识别'
        $names = @((Get-MatchDevSecretAllNames) + 'MATCH_MIGRATION_PASSWORD')
        $saved = @{}; foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
        try {
            foreach ($name in (Get-MatchDevSecretAllNames)) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
            [Environment]::SetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'fixture-ddl-password', 'Process')
            $null = Start-ServiceInstance -Name match -Info $ServiceCatalogue.match -Index 1 -Total 1 -Pids ([pscustomobject]@{}) -UseExe
            Assert-Equal 1 $script:Starts.Count '恰好启动一次'
            foreach ($secret in @($script:Starts[0].ProbeSecret, $script:Starts[0].OutcomeSecret, $script:Starts[0].InboxSecret, $script:Starts[0].RankSecret)) {
                Assert-True (([string]$secret).Length -ge 32) '四把校验密钥都不短于32字节'
            }
            Assert-True ([string]::IsNullOrEmpty($script:Starts[0].MigrationPassword)) '常驻进程拿不到建表口令'
            Assert-True ([string]::IsNullOrEmpty($env:MATCH_PROBE_SECRET)) '开发密钥只给子进程'
            Assert-Equal 'fixture-ddl-password' $env:MATCH_MIGRATION_PASSWORD '父环境原样恢复'
        } finally { foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') } }
    }
    Test-Case 'battle 与 match 拿同一把探测和终局密钥，只按需注入' {
        Write-MatchFixture
        $names = @((Get-MatchDevSecretAllNames) + @('GLOBAL_MATCH_ENABLED', 'PHYSICAL_REGION'))
        $saved = @{}; foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
        try {
            foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
            $backup = Backup-MatchDevSecrets -Service match
            Initialize-MatchDevSecrets -RepoRoot $RepoRoot -Service match
            $probe = $env:MATCH_PROBE_SECRET; $outcome = $env:MMORPG_MATCH_OUTCOME_SECRET_BATTLE
            Assert-True ([string]::IsNullOrEmpty($env:GLOBAL_MATCH_ENABLED)) 'match 不写 battle 的机房身份'
            Restore-MatchDevSecrets $backup
            Assert-True ([string]::IsNullOrEmpty($env:MATCH_PROBE_SECRET)) '还原后父环境为空'

            $backup = Backup-MatchDevSecrets -Service battle
            Initialize-MatchDevSecrets -RepoRoot $RepoRoot -Service battle
            Assert-Equal $probe $env:MATCH_PROBE_SECRET '探测密钥两端同值'
            Assert-Equal $outcome $env:MMORPG_MATCH_OUTCOME_SECRET_BATTLE '终局密钥两端同值'
            Assert-Equal 'true' $env:GLOBAL_MATCH_ENABLED 'battle 开全球匹配'
            Assert-Equal '1' $env:PHYSICAL_REGION 'battle 有非零机房号'
            Assert-True ([string]::IsNullOrEmpty($env:RANK_PRODUCER_SECRET_MATCH)) 'battle 不拿投榜密钥'
            Assert-True ([string]::IsNullOrEmpty($env:MMORPG_PLAYER_INBOX_SECRET_MATCH)) 'battle 不拿入箱密钥'
            Restore-MatchDevSecrets $backup

            $env:PHYSICAL_REGION = '7'
            $backup = Backup-MatchDevSecrets -Service battle
            Initialize-MatchDevSecrets -RepoRoot $RepoRoot -Service battle
            Assert-Equal '7' $env:PHYSICAL_REGION '显式机房号不覆盖'
            Restore-MatchDevSecrets $backup
        } finally { foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') } }
    }
    Test-Case '调用方显式给的密钥优先，并写回文件给后起的对端' {
        Write-MatchFixture
        $explicit = 'fixture-explicit-probe-secret-at-least-32-bytes'
        $names = @((Get-MatchDevSecretAllNames) + @('GLOBAL_MATCH_ENABLED', 'PHYSICAL_REGION'))
        $saved = @{}; foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
        try {
            foreach ($name in (Get-MatchDevSecretAllNames)) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
            $env:MATCH_PROBE_SECRET = $explicit
            Initialize-MatchDevSecrets -RepoRoot $RepoRoot -Service battle
            Assert-Equal $explicit $env:MATCH_PROBE_SECRET '不覆盖调用方'
            $env:MATCH_PROBE_SECRET = $null
            Initialize-MatchDevSecrets -RepoRoot $RepoRoot -Service match
            Assert-Equal $explicit $env:MATCH_PROBE_SECRET '后起的 match 从文件拿到同一把'
        } finally { foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') } }
    }
    Test-Case '本机建表把账号只交给迁移子进程，口令不落盘，结束后还原' {
        Write-MatchFixture
        $LocalMySQL = $true
        $Services = @('match,guild')
        $script:Migrations = @()
        function Invoke-ServiceMigrationProcess {
            param($Name, $Info, $ConfigPath)
            $script:Migrations += [pscustomobject]@{ Name = $Name; ConfigPath = $ConfigPath; Config = Get-Content -LiteralPath $ConfigPath -Raw; User = $env:MATCH_MIGRATION_USER; Password = $env:MATCH_MIGRATION_PASSWORD; Principal = $env:MMORPG_SQL_SCHEMA_PRINCIPAL; GuildDsn = $env:MMORPG_GUILD_SQL_DSN }
            return 0
        }
        $names = @('MATCH_MIGRATION_USER', 'MATCH_MIGRATION_PASSWORD', 'MMORPG_SQL_SCHEMA_PRINCIPAL', 'MMORPG_GUILD_SQL_DSN')
        $saved = @{}; foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process'); [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
        try {
            Invoke-LocalSchemaMigrate
            Assert-Equal 2 $script:Migrations.Count '两个服务各迁一次'
            $match = $script:Migrations[0]; $guild = $script:Migrations[1]
            Assert-Equal 'match' $match.Name '先 match'
            Assert-Equal 'fixture-user' $match.User '建表账号取 go/db'
            Assert-Equal 'fixture-only' $match.Password '建表口令取 go/db'
            Assert-Equal 'fixture-user@%' $match.Principal '主体是容器外连入的账号'
            Assert-Match $match.Config '(?m)^  Backend: mysql' 'match 用本机档配置'
            Assert-Equal 'guild' $guild.Name '后 guild'
            Assert-Match $guild.Config '(?m)^  Backend: mysql' 'guild 用本机档配置'
            Assert-Match $guild.Config '(?m)^  DataSourceEnv: MMORPG_GUILD_SQL_DSN' '连接串只走环境变量'
            Assert-NotMatch $guild.Config 'fixture-only|fixture-app-password' '配置文件不含任何口令'
            Assert-Equal 'fixture-user:fixture-only@tcp(127.0.0.1:3306)/mmorpg_guild?charset=utf8mb4' $guild.GuildDsn '连接串沿用源配置的端点和参数'
            Assert-Equal $false (Test-Path -LiteralPath $guild.ConfigPath) '临时配置结束即删'
            foreach ($name in $names) { Assert-True ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable($name, 'Process'))) "$name 结束后还原" }
        } finally { foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') } }
    }
    Test-Case '本机建表失败时报出退出码并还原环境' {
        Write-MatchFixture
        $LocalMySQL = $true
        $Services = @('guild')
        function Invoke-ServiceMigrationProcess { param($Name, $Info, $ConfigPath) return 4 }
        $saved = $env:MMORPG_GUILD_SQL_DSN
        try {
            $env:MMORPG_GUILD_SQL_DSN = $null
            Assert-Match (Get-ThrownMessage { Invoke-LocalSchemaMigrate }) '退出码 4' '失败不吞'
            Assert-True ([string]::IsNullOrEmpty($env:MMORPG_GUILD_SQL_DSN)) '失败也还原连接串'
            Assert-Equal $false (Test-Path -LiteralPath (Join-Path $DerivedEtcDir 'guild_migrate.yaml')) '失败也删临时配置'
        } finally { $env:MMORPG_GUILD_SQL_DSN = $saved }
    }
    Test-Case '本机建表只管 match 与 guild，且必须显式本机档' {
        Write-MatchFixture
        $LocalMySQL = $true
        $Services = @('db')
        Assert-Match (Get-ThrownMessage { Invoke-LocalSchemaMigrate }) '只管 match 与 guild' '其他服务的建表不归这里'
        $LocalMySQL = $false
        $Services = @('match')
        Assert-Match (Get-ThrownMessage { Invoke-LocalSchemaMigrate }) '只支持 -LocalMySQL' 'TiDB 不用开发账号建表'
    }
    # 建库建表的顺序和内容由 sql_boot_catalog_local.tests.ps1 钉住:写者授权要枚举表,全部挪进了目录生成器。
    Test-Case '一键启动在起 match 与 guild 前生成写者目录(含建库建表)，guild 变成必需' {
        $startAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../start_game.ps1'), [ref]$null, [ref]$null)
        $commands = @($startAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] }, $true))
        $generate = @($commands | Where-Object { $_.GetCommandName() -eq 'Invoke-Dev' -and $_.Extent.Text.Contains('sql-boot-catalog-local') })
        Assert-Equal 1 $generate.Count '建表接线唯一'
        $firstBatch = @($commands | Where-Object { $_.GetCommandName() -eq 'Start-LocalGoServices' -and $_.Extent.Text.Contains("'match'") })
        Assert-Equal 1 $firstBatch.Count 'match 只在一个批次启动'
        Assert-True ($generate[0].Extent.EndOffset -lt $firstBatch[0].Extent.StartOffset) '建表先于常驻进程'
        $optional = @($startAst.FindAll({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -ceq '$optionalServices' -and $node.Operator -eq 'Equals' }, $true))
        Assert-Equal 1 $optional.Count '可选服务清单唯一'
        Assert-NotMatch $optional[0].Right.Extent.Text 'guild' 'guild 不可选：match 启动要核对它的表'
    }
    Test-Case '真实源数据库配置未被本组回归修改' {
        Assert-Equal $realSourceHash (Get-FileHash -LiteralPath $realSourceConfig -Algorithm SHA256).Hash '生产默认源文件哈希原样保留'
    }
}
finally {
    $env:RPC_PORT = $oldRpcPort
    $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = $oldGlobalTiDBHost
    $env:DB_ALLOWED_DATABASES = $oldDatabaseAllowlist
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolved) -notlike 'mmorpg-local-mysql-tests-*') { throw '拒绝清理临时测试范围之外的目录。' }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'go_services 本机 MySQL 配置')
