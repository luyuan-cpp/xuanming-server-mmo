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
}
$fixture = @'
ListenOn: 0.0.0.0:6000
ZoneId: 1
ServerConfig:
  Database:
    Backend: tidb # 保持源严格配置
    Hosts: "127.0.0.1:3306"
    Passwd: fixture-only
    AutoCreateDatabase: false
    AutoMigrateSchema: false
  PlayerStore:
    MaxRowBytes: 1048576
'@
$fixture = ($fixture -replace '\r?\n', "`r`n") + "`r`n"
$sourceConfig = Join-Path $GoRoot 'db/etc/db.yaml'
$realSourceConfig = Join-Path $PSScriptRoot '../../../go/db/etc/db.yaml'
$realSourceHash = (Get-FileHash -LiteralPath $realSourceConfig -Algorithm SHA256).Hash
$oldRpcPort = $env:RPC_PORT
$oldGlobalTiDBHost = $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST
$oldDatabaseAllowlist = $env:DB_ALLOWED_DATABASES
function Resolve-BindablePort { param([int]$Port, [string]$Label, [hashtable]$ReservedPorts) if ($script:PortOverride) { return $script:PortOverride }; return $Port }
function Start-Process {
    [CmdletBinding()] param($FilePath, $ArgumentList, $WorkingDirectory, $RedirectStandardOutput, $RedirectStandardError, [switch]$PassThru, $WindowStyle)
    $script:Starts += [pscustomobject]@{ FilePath = $FilePath; Arguments = $ArgumentList; WorkingDirectory = $WorkingDirectory; WindowStyle = $WindowStyle }
    return [pscustomobject]@{ Id = 901 }
}
function Get-CimInstance { throw '测试禁止查询真实进程' }
function Stop-Process { throw '测试禁止终止进程' }
function Wait-ForStartupBanner { param($LogFile, $ServiceName) }
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
        Assert-Equal $derived $script:Starts[0].Arguments[1] 'db进程实参指向本机副本'
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
