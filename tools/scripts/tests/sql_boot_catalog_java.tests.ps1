#requires -Version 7
param([switch]$LibraryOnly)
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/sql_boot_catalog_local.ps1"
$script:JavaRealRepo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

# 全部是本例自产的无功能假产物，永不执行；配置读取R事实源，不复制第二份配置知识。
function New-JavaBootFixture([string]$Root) {
    $null = [IO.Directory]::CreateDirectory($Root)
    $java = Join-Path $Root 'java.exe'; [IO.File]::WriteAllText($java, 'non-executable-test-fixture')
    foreach ($pair in @(@('mall_game','mall-game-0.1.0-SNAPSHOT.jar'), @('gateway_node','gateway-node-0.0.1-SNAPSHOT.jar'))) {
        $module = $pair[0]; $target = Join-Path $Root "java/$module/target"; $resources = Join-Path $Root "java/$module/src/main/resources"
        $null = [IO.Directory]::CreateDirectory($target); $null = [IO.Directory]::CreateDirectory($resources)
        [IO.File]::WriteAllText((Join-Path $target $pair[1]), 'non-executable-jar-fixture')
        [IO.File]::Copy((Join-Path $script:JavaRealRepo "java/$module/src/main/resources/application.yaml"), (Join-Path $resources 'application.yaml'))
    }
    $instances = @(foreach ($key in @('mall','mall-replica','gateway','gateway-replica')) {
        $service = $key -replace '-replica$', ''; $replica = $key.EndsWith('-replica')
        $module = if ($service -ceq 'mall') { 'mall_game' } else { 'gateway_node' }
        $jar = if ($service -ceq 'mall') { 'mall-game-0.1.0-SNAPSHOT.jar' } else { 'gateway-node-0.0.1-SNAPSHOT.jar' }
        $row = [ordered]@{ instanceKey = $key; service = $service; port = $(if ($service -ceq 'mall') { 18120 } else { 18121 }) + $(if ($replica) { 100 } else { 0 }); database = $(if ($service -ceq 'mall') { 'mall_test' } else { 'gateway_test' }); jarSha256 = (Get-MaintenanceSha256 (Join-Path $Root "java/$module/target/$jar")) }
        if ($service -ceq 'gateway') { $row.loginGrpcEndpoints = '990017=127.0.0.1:53000,990018=127.0.0.1:54000'; $row.redisEndpoint = '127.0.0.1:26379'; $row.etcdEndpoint = 'http://127.0.0.1:32379'; $row.mallAdminUrl = 'http://127.0.0.1:18120' }
        $row
    })
    $plan = [ordered]@{ format = 'LOCAL_R_JAVA_FOUR_INSTANCES_V1'; javaExecutable = $java; javaSha256 = (Get-MaintenanceSha256 $java); instances = $instances }
    $path = Join-Path $Root 'java-plan.json'; [IO.File]::WriteAllText($path, ($plan | ConvertTo-Json -Depth 12))
    return @{ root = $Root; path = $path; sha256 = (Get-MaintenanceSha256 $path); plan = $plan }
}
function Save-JavaBootFixturePlan($Fixture) { [IO.File]::WriteAllText($Fixture.path, ($Fixture.plan | ConvertTo-Json -Depth 12)); $Fixture.sha256 = Get-MaintenanceSha256 $Fixture.path }
function Read-JavaBootFixturePlan($Fixture) { @(Read-LocalSqlBootJavaPlan -RepoRoot $Fixture.root -Path $Fixture.path -Sha256 $Fixture.sha256 -Zones 990017,990018) }
function Get-JavaBootFailure([scriptblock]$Body) { try { & $Body | Out-Null; return '' } catch { return $_.Exception.Message } }
if ($LibraryOnly) { return }

$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-java-boot-test-' + [guid]::NewGuid().ToString('N'))
try {
    Test-Case '四角色固定R运行包,同服务双副本共用全局库,四个端口互异' {
        $f = New-JavaBootFixture (Join-Path $fixtureRoot 'valid'); $rows = Read-JavaBootFixturePlan $f
        Assert-Equal 4 $rows.Count '四个Java boot'
        Assert-Equal 2 @($rows | Where-Object Service -CEQ 'mall').Count '两个Mall'
        Assert-Equal 2 @($rows | Where-Object Service -CEQ 'gateway').Count '两个Gateway'
        Assert-Equal 4 @($rows.Port | Select-Object -Unique).Count '四个端口'
        Assert-Equal 1 @($rows | Where-Object Service -CEQ 'mall' | ForEach-Object Database | Select-Object -Unique).Count '不是每zone一个Mall库'
        Assert-True (@($rows | Where-Object { $_.Jar -notlike "$($f.root)*" }).Count -eq 0) '只来自R计划根'
    }
    Test-Case 'J角色路径、散落zone商城、重复key、重复端口和错误Go映射均拒绝' {
        $index = 0
        foreach ($change in @(
            { param($p) $p.instances[0].instanceKey = 'game-mall' },
            { param($p) $p.instances[1].database = 'zone_990018_mall' },
            { param($p) $p.instances[1].instanceKey = 'mall' },
            { param($p) $p.instances[1].port = $p.instances[0].port },
            { param($p) $p.instances[2].loginGrpcEndpoints = '990017=127.0.0.1:53000' },
            { param($p) $p.instances[2].loginGrpcEndpoints = '990017=remote.invalid:53000,990018=127.0.0.1:54000' },
            { param($p) $p.instances[2].mallAdminUrl = 'http://127.0.0.1:9999' }
        )) {
            $f = New-JavaBootFixture (Join-Path $fixtureRoot "bad-$index"); & $change $f.plan; Save-JavaBootFixturePlan $f
            Assert-True (-not [string]::IsNullOrWhiteSpace((Get-JavaBootFailure { Read-JavaBootFixturePlan $f }))) '坏计划必须先于启动和SQL拒绝'
            $index++
        }
    }
    Test-Case '计划或运行包漂移拒绝,不能拿同名旧JAR顶替' {
        $f = New-JavaBootFixture (Join-Path $fixtureRoot 'drift'); $rows = Read-JavaBootFixturePlan $f
        [IO.File]::AppendAllText($rows[0].Jar, 'changed')
        Assert-Match (Get-JavaBootFailure { Read-JavaBootFixturePlan $f }) '运行包' '精确SHA拒绝'
        [IO.File]::AppendAllText($f.path, ' ')
        Assert-Match (Get-JavaBootFailure { Read-JavaBootFixturePlan $f }) 'SHA' '计划原字节漂移拒绝'
    }
    Test-Case '完整Java私有配置实际绑定本boot端口、SQL库和两区Go地址,保留网关池预算' {
        $f = New-JavaBootFixture (Join-Path $fixtureRoot 'config'); $rows = Read-JavaBootFixturePlan $f
        $settings = [pscustomobject]@{ Endpoint = '127.0.0.1:24000'; Backend = 'tidb' }
        foreach ($row in $rows) {
            $text = New-LocalSqlBootJavaConfig -Settings $settings -Instance $row -User 'fixture_normal' -Password 'fixture-only-value'
            $path = Join-Path $f.root ($row.InstanceKey + '.yaml'); [IO.File]::WriteAllText($path, $text)
            Assert-Equal ([string]$row.Port) ([string](Get-YamlScalar -Path $path -KeyPath 'server.port').Value) '私有配置使用本实例端口'
            Assert-Match $text ('jdbc:mysql://127[.]0[.]0[.]1:24000/' + $row.Database) '不使用共享默认库'
            Assert-NotMatch $text '\$\{MMORPG_SQL_(USERNAME|PASSWORD)\}' '数据源凭据由本boot物化'
            if ($row.Service -ceq 'gateway') {
                Assert-Equal '5' ([string](Get-YamlScalar -Path $path -KeyPath 'spring.datasource.hikari.maximum-pool-size').Value) '原连接预算保留'
                Assert-Equal $row.Settings.loginGrpcEndpoints (Get-YamlScalar -Path $path -KeyPath 'login.grpc.endpoints').Value '两区精确Go映射'
                Assert-Match $text '(?m)^  endpoints: \["http://127[.]0[.]0[.]1:32379"\]$' 'etcd列表合法单行替换'
                Assert-NotMatch $text '(?m)^    - http://127[.]0[.]0[.]1:2379$' '旧列表没有遗留'
            }
        }
    }
    Test-Case 'Go与Java计划合并时跨语言端口碰撞也在DDL前拒绝' {
        $f = New-JavaBootFixture (Join-Path $fixtureRoot 'merged'); $rows = Read-JavaBootFixturePlan $f
        $go = [pscustomobject]@{ InstanceKey = 'z990017_login'; Service = 'login'; Running = $true; Port = 18120 }
        Assert-Match (Get-JavaBootFailure { Assert-LocalSqlBootMergedPlan -RepoRoot $f.root -Instances (@($go) + $rows) -BootId 'abcdef12' }) 'both claim port' '跨语言端口冲突拒绝'
    }
    Test-Case '每个Gateway映射必须绑定同zone的实际Go login计划端口' {
        $f = New-JavaBootFixture (Join-Path $fixtureRoot 'login-bind'); $rows = Read-JavaBootFixturePlan $f
        $go = @([pscustomobject]@{ InstanceKey = 'z990017_login'; Service = 'login'; Zone = 990017; Running = $true; Port = 53000 }, [pscustomobject]@{ InstanceKey = 'z990018_login'; Service = 'login'; Zone = 990018; Running = $true; Port = 54000 })
        Assert-LocalSqlBootMergedPlan -RepoRoot $f.root -Instances ($go + $rows) -BootId 'abcdef12'
        foreach ($bad in @('990017=127.0.0.1:53001,990018=127.0.0.1:54000', '990017=127.0.0.1:54000,990018=127.0.0.1:53000')) {
            $gateway = @($rows | Where-Object InstanceKey -CEQ 'gateway-replica')[0]
            $original = $gateway.Settings.loginGrpcEndpoints
            try {
                $gateway.Settings.loginGrpcEndpoints = $bad
                Assert-Match (Get-JavaBootFailure { Assert-LocalSqlBootMergedPlan -RepoRoot $f.root -Instances ($go + $rows) -BootId 'abcdef12' }) '同zone的本轮合并实例计划' '错本机端口或两区互换也拒绝'
            } finally { $gateway.Settings.loginGrpcEndpoints = $original }
        }
        # Go使用自定义offset后，沿用旧的合法loopback端口同样不成立。
        $go[1].Port = 55010
        Assert-Match (Get-JavaBootFailure { Assert-LocalSqlBootMergedPlan -RepoRoot $f.root -Instances ($go + $rows) -BootId 'abcdef12' }) '同zone的本轮合并实例计划' '不绕过自定义offset'
    }
} finally {
    $full = [IO.Path]::GetFullPath($fixtureRoot); $prefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($full) -notlike 'mmorpg-java-boot-test-*') { throw '测试清理越界。' }
    if (Test-Path -LiteralPath $full) { Remove-Item -LiteralPath $full -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'R四Java启动目录合同')
