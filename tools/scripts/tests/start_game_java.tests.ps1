#requires -Version 7
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/sql_boot_catalog_java.tests.ps1" -LibraryOnly
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-java-start-test-' + [guid]::NewGuid().ToString('N'))
$script:JavaLaunched = [Collections.Generic.List[object]]::new()
$script:JavaFailAt = 0
$script:JavaNeverListenInstance = ''
$script:JavaKillLeavesAlive = $false
$script:JavaWaitReturnsFalse = $false
$script:JavaTestClock = $null

# 接缝替身只记录本例对象，绝不创建JVM、网络连接或写者资格。
function Get-NetTCPConnection {
    param($State, $LocalPort, $ErrorAction)
    foreach ($row in $script:JavaLaunched) {
        if (-not $row.HasExited -and $row.Port -eq $LocalPort) {
            $birth = Join-Path $script:JavaCurrentFixture.root ('run/logs/java-boot/abcdef12/' + $row.InstanceKey + '.process.birth.actual.json')
            if (-not (Test-Path -LiteralPath $birth)) { throw '进入监听等待前必须已保存出生身份' }
            if ($row.InstanceKey -cne $script:JavaNeverListenInstance) { [pscustomobject]@{ OwningProcess = $row.Id } }
        }
    }
}
function New-LocalSqlBootJavaStartupClock { $script:JavaTestClock = [pscustomobject]@{ Elapsed = [TimeSpan]::Zero }; return $script:JavaTestClock }
function Start-Sleep { param([int]$Milliseconds) if ($null -eq $script:JavaTestClock) { throw '测试等待必须有本例单调时钟替身' }; $script:JavaTestClock.Elapsed = $script:JavaTestClock.Elapsed.Add([TimeSpan]::FromSeconds(120)) }
function Get-SqlWriterBootEnvironment {
    param($InstanceKey,$RepoRoot,$ServiceName,[ref]$SelectedRuntimeConfigFile)
    $SelectedRuntimeConfigFile.Value = Join-Path $RepoRoot ($InstanceKey + '.yaml')
    return [ordered]@{ MMORPG_SQL_DEPLOYMENT_ID = $InstanceKey; MMORPG_SQL_WRITER_GRANT_FILE = 'new-test-only-boot'; MMORPG_SQL_WRITER_GRANT_SHA256 = ('1' * 64); MMORPG_SQL_WRITER_CONSUME_DIR = 'new-test-only-consume'; MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256 = ('2' * 64) }
}
function Start-Process {
    param($FilePath,$ArgumentList,$WorkingDirectory,$WindowStyle,$RedirectStandardOutput,$RedirectStandardError,[switch]$PassThru)
    if ($script:JavaFailAt -gt 0 -and $script:JavaLaunched.Count + 1 -eq $script:JavaFailAt) { throw '本例模拟第三个启动失败' }
    foreach ($control in @('MMORPG_SQL_REGISTRAR_DSN','MMORPG_SQL_MAINTENANCE_DSN','MMORPG_SQL_SCHEMA_DSN','MMORPG_SQL_OPERATOR_PRINCIPAL','MMORPG_SQL_SCHEMA_PRINCIPAL')) {
        if ([Environment]::GetEnvironmentVariable($control,'Process')) { throw '常驻进程不应继承控制能力' }
    }
    if ($WindowStyle -cne 'Hidden') { throw '后台Java启动必须隐藏窗口' }
    $key = $env:MMORPG_SQL_DEPLOYMENT_ID
    $path = Join-Path $script:JavaCurrentFixture.root ($key + '.yaml')
    if (($ArgumentList -join ' ') -notlike '*--spring.config.location=file:*' -or $env:MMORPG_SQL_PRIVATE_FILE_HELPER -cne $script:JavaHelper) { throw '缺本boot配置或helper' }
    $row = [pscustomobject]@{ Id = 100 + $script:JavaLaunched.Count; HasExited = $false; Handle = [IntPtr]::new(1); StartTime = [DateTime]::UtcNow; Port = [int](Get-YamlScalar -Path $path -KeyPath 'server.port').Value; InstanceKey = $key; Killed = $false; Disposed = $false; LastWaitBudget = 0 }
    $row | Add-Member ScriptMethod Kill { $this.Killed = $true; if (-not $script:JavaKillLeavesAlive) { $this.HasExited = $true } }
    $row | Add-Member ScriptMethod WaitForExit { param($Milliseconds) $this.LastWaitBudget = $Milliseconds; if ($script:JavaWaitReturnsFalse) { return $false }; return $this.HasExited }
    $row | Add-Member ScriptMethod Dispose { $this.Disposed = $true }
    $script:JavaLaunched.Add($row)
    return $row
}
function New-JavaStartFixture([string]$Name) {
    $script:JavaNeverListenInstance = ''; $script:JavaKillLeavesAlive = $false; $script:JavaWaitReturnsFalse = $false; $script:JavaTestClock = $null
    $f = New-JavaBootFixture (Join-Path $fixtureRoot $Name)
    $script:JavaCurrentFixture = $f; $script:JavaHelper = Join-Path $f.root 'helper-fixture.exe'; [IO.File]::WriteAllText($script:JavaHelper, 'non-executable-helper-fixture')
    foreach ($row in (Read-JavaBootFixturePlan $f)) { [IO.File]::WriteAllText((Join-Path $f.root ($row.InstanceKey + '.yaml')), (New-LocalSqlBootJavaConfig -Settings ([pscustomobject]@{ Endpoint = '127.0.0.1:24000'; Backend = 'tidb' }) -Instance $row -User 'fixture_normal' -Password 'test-only')) }
    $summary = @{ bootId = 'abcdef12'; catalog = (Join-Path $f.root 'new-test-only-catalog.json'); javaInstancePlan = @{ path = $f.path; sha256 = $f.sha256 }; javaInstances = @($f.plan.instances.instanceKey); privateFileHelper = @{ path = $script:JavaHelper; sha256 = (Get-MaintenanceSha256 $script:JavaHelper) } }
    $f.summaryPath = Join-Path $f.root 'summary.json'; [IO.File]::WriteAllText($f.summaryPath, ($summary | ConvertTo-Json -Depth 10))
    return $f
}
$savedCatalog = $env:MMORPG_SQL_BOOT_CATALOG; $savedSchema = $env:MMORPG_SQL_SCHEMA_DSN
try {
    Test-Case '四个R Java副本各消费本boot接缝,控制身份剥离且父环境恢复' {
        $script:JavaLaunched.Clear(); $script:JavaFailAt = 0; $f = New-JavaStartFixture 'success'
        $env:MMORPG_SQL_BOOT_CATALOG = 'parent-test-only-catalog'; $env:MMORPG_SQL_SCHEMA_DSN = 'parent-test-only-control'
        $result = Start-LocalSqlBootJavaInstances -RepoRoot $f.root -PlanPath $f.path -PlanSha256 $f.sha256 -Zones 990017,990018 -SummaryPath $f.summaryPath
        Assert-Equal 4 $script:JavaLaunched.Count '恰好四个新进程'
        Assert-Equal 'mall,mall-replica,gateway,gateway-replica' (($script:JavaLaunched | ForEach-Object InstanceKey) -join ',') 'Mall先启动，Gateway后启动'
        Assert-Equal 'parent-test-only-catalog' $env:MMORPG_SQL_BOOT_CATALOG '父目录环境恢复'
        Assert-Equal 'parent-test-only-control' $env:MMORPG_SQL_SCHEMA_DSN '父控制身份恢复'
        Assert-Equal $false $result.businessAccepted '启动监听不冒充业务验收'
        Assert-True (@($script:JavaLaunched | Where-Object { $_.Killed -or -not $_.Disposed }).Count -eq 0) '成功不杀进程并释放监督句柄'
        Assert-Equal 4 @(Get-ChildItem -LiteralPath $result.logDirectory -Filter '*.process.actual.json').Count '各进程分别记录创建身份'
        Assert-Equal 4 @(Get-ChildItem -LiteralPath $result.logDirectory -Filter '*.process.birth.actual.json').Count '四份出生身份独立于监听就绪记录'
        Assert-Match (Get-JavaBootFailure { Start-LocalSqlBootJavaInstances -RepoRoot $f.root -PlanPath $f.path -PlanSha256 $f.sha256 -Zones 990017,990018 -SummaryPath $f.summaryPath }) '已有启动尝试' '同boot拒绝盲重试'
    }
    Test-Case '第三个Java启动失败只结束本批前两个句柄并保留原尝试' {
        $script:JavaLaunched.Clear(); $script:JavaFailAt = 3; $f = New-JavaStartFixture 'failure'
        Assert-Match (Get-JavaBootFailure { Start-LocalSqlBootJavaInstances -RepoRoot $f.root -PlanPath $f.path -PlanSha256 $f.sha256 -Zones 990017,990018 -SummaryPath $f.summaryPath }) '第三个' '保留真实失败'
        Assert-Equal 2 $script:JavaLaunched.Count '未继续启动第四个'
        Assert-True (@($script:JavaLaunched | Where-Object { -not $_.Killed -or -not $_.Disposed }).Count -eq 0) '原句柄清理并释放'
        Assert-True (Test-Path -LiteralPath (Join-Path $f.root 'run/logs/java-boot/abcdef12/started.json')) '尝试不删除或伪装未启动'
        $cleanup = Read-MaintenanceJson -Path (Join-Path $f.root 'run/logs/java-boot/abcdef12/cleanup.actual.json')
        Assert-Equal $false $cleanup.cleanup_pending '本例两个句柄实际结束后才确认清理'
    }
    Test-Case 'Java监听启动超时也先保留精确出生身份,不等待真实墙钟' {
        $script:JavaLaunched.Clear(); $script:JavaFailAt = 0; $f = New-JavaStartFixture 'timeout'
        $script:JavaNeverListenInstance = 'mall'
        Assert-Match (Get-JavaBootFailure { Start-LocalSqlBootJavaInstances -RepoRoot $f.root -PlanPath $f.path -PlanSha256 $f.sha256 -Zones 990017,990018 -SummaryPath $f.summaryPath }) '120秒' '仍然是原120秒预算'
        Assert-Equal 1 $script:JavaLaunched.Count '首个超时后不启动后续角色'
        $dir = Join-Path $f.root 'run/logs/java-boot/abcdef12'
        $birth = Read-MaintenanceJson -Path (Join-Path $dir 'mall.process.birth.actual.json')
        Assert-Equal $script:JavaLaunched[0].Id $birth.pid '出生记录保存原PID'
        Assert-Equal ([string]$script:JavaLaunched[0].StartTime.ToUniversalTime().ToFileTimeUtc()) $birth.creationFiletimeUtc '出生记录保存精确创建FILETIME'
        Assert-Equal $f.plan.javaSha256 $birth.javaSha256 '程序SHA在出生记录'
        Assert-Equal $f.plan.instances[0].jarSha256 $birth.jarSha256 'JAR SHA在出生记录'
        Assert-Equal $false $birth.listening_confirmed '出生不是监听通过'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $dir 'mall.process.actual.json')) '超时不能产就绪记录'
        Assert-Equal 120 $script:JavaTestClock.Elapsed.TotalSeconds '单调时钟替身直接抵达原截止，无真实sleep'
    }
    Test-Case 'WaitForExit为false必须保留cleanup_pending与原启动错误,Dispose不能冒退出' {
        $script:JavaLaunched.Clear(); $script:JavaFailAt = 3; $f = New-JavaStartFixture 'cleanup-pending'
        $script:JavaKillLeavesAlive = $true; $script:JavaWaitReturnsFalse = $true
        Assert-Match (Get-JavaBootFailure { Start-LocalSqlBootJavaInstances -RepoRoot $f.root -PlanPath $f.path -PlanSha256 $f.sha256 -Zones 990017,990018 -SummaryPath $f.summaryPath }) '第三个' '清理未确认不能盖掉原启动错误'
        $dir = Join-Path $f.root 'run/logs/java-boot/abcdef12'
        $cleanup = Read-MaintenanceJson -Path (Join-Path $dir 'cleanup.actual.json')
        Assert-Equal $true $cleanup.cleanup_pending '未退出明确留pending'
        Assert-Equal 2 @($cleanup.processes).Count '两个原子进程分别保留状态'
        foreach ($row in $cleanup.processes) {
            Assert-Equal $false $row.waitConfirmed 'Wait false没有吞掉'
            Assert-Equal $false $row.exitConfirmed '不能声称已退出'
            Assert-Equal $true $row.cleanup_pending '逐进程也留pending'
            Assert-Equal 10000 $row.waitBudgetMilliseconds '不延长原10秒收尾预算'
            Assert-True $row.birthRecordPersisted '原出生身份已在等待监听前保存'
            Assert-True (Test-Path -LiteralPath $row.birthRecordPath) '出生原件保留供下一步核实'
        }
        Assert-True (@($script:JavaLaunched | Where-Object { $_.HasExited -or -not $_.Disposed -or $_.LastWaitBudget -ne 10000 }).Count -eq 0) '只释放本地句柄，记录仍如实承认进程存活'
    }
    Test-Case 'start_game正式入口JavaOnly先分流,不碰原Docker/Go/C++启动' {
        $source = [IO.File]::ReadAllText((Join-Path $script:JavaRealRepo 'tools/scripts/start_game.ps1'))
        $early = $source.Substring(0,$source.IndexOf("`$stamp ="))
        Assert-Match $early 'if \(\$JavaOnly\)' '显式分流'
        Assert-Match $early 'Start-LocalSqlBootJavaInstances' '复用正规目录启动接缝'
        Assert-Match $early 'return' '分流结束，不掉入一区启动'
        Assert-NotMatch $early 'Start-Process|docker |cpp-node-start|go-svc-start' '早期入口不直接启动其他栈'
    }
} finally {
    $env:MMORPG_SQL_BOOT_CATALOG = $savedCatalog; $env:MMORPG_SQL_SCHEMA_DSN = $savedSchema
    $full = [IO.Path]::GetFullPath($fixtureRoot); $prefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $full.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($full) -notlike 'mmorpg-java-start-test-*') { throw '测试清理越界。' }
    if (Test-Path -LiteralPath $full) { Remove-Item -LiteralPath $full -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'R四Java正规启动合同')
