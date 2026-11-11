<#
.SYNOPSIS
    审计管道部署面的契约:审计 topic 的 retention.bytes 渲染进 data-service 并计入 Kafka 盘预算,
    进退场审计快照的开关从仓库的 game_config.yaml 整段复制进 node-config。

.DESCRIPTION
    缺口清单第 3 条(docs/handoff/server-gap-audit-20261101.md)。这里钉的都是「错了不会报错、出事才发现」的东西:
      - data-service 没拿到审计 topic 的 retention.bytes,topic 沿用 broker 缺省,积压时未消费的审计消息被 Kafka 删掉;
      - 盘预算不按审计 retention.bytes 算,放宽之后 broker 写满;
      - node-config 漏了 audit_snapshot 段,K8s 上永远按加载器默认值跑,运维改了 yaml 也不生效。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/audit_pipeline_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:DeployPath = Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'
$script:GameConfigPath = Join-Path $script:RepoRoot 'bin/etc/game_config.yaml'
$script:DataServiceConfigGo = Join-Path $script:RepoRoot 'go/data_service/internal/config/config.go'

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    return ([IO.File]::ReadAllText($Path)) -replace "`r`n", "`n"
}

# 盘预算的两个纯函数按 AST 抽出来直接调用,与 k8s_deploy_contract 同一做法。
$tokens = $null
$parseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile($script:DeployPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败: $($parseErrors[0].Message)" }
foreach ($fn in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if (@('Get-KafkaDiskBudgetConstants', 'Get-KafkaDiskPlan', 'Apply-KafkaProfileDefaults') -contains $fn.Name) {
        Set-Item -Path "Function:script:$($fn.Name)" -Value $fn.Body.GetScriptBlock()
    }
}
foreach ($name in @('Get-KafkaDiskBudgetConstants', 'Get-KafkaDiskPlan')) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 $name" }
}

# prod 档的缺省输入:审计 6 + 3 个分区,命令 topic 2 × 256 个分区,segment 32MiB,broker 缺省 512MiB,其它预留 4GiB。
function New-ProdDiskPlan {
    param([long]$AuditRetentionBytes = 0)
    $p = @{
        Brokers = 3; ReplicationFactor = 3; SegmentBytes = 33554432; BrokerRetentionBytes = 536870912
        AuditPartitions = 9; CommandPartitions = 512; OtherTopicsReserveGi = 4
        LiveStorageGi = $null; LivePvcGi = $null; ExplicitStorageGi = 0
    }
    if ($AuditRetentionBytes -ne 0) { $p['AuditRetentionBytes'] = $AuditRetentionBytes }
    return Get-KafkaDiskPlan @p
}

Test-Case '审计 retention.bytes 不给时按 broker 缺省算,盘预算与以前相同' {
    $plan = New-ProdDiskPlan
    Assert-Equal -Expected 40 -Actual $plan.RequiredGi -Because '1.3 × (9 × (512 + 32)MiB + 512 × 32MiB + 4GiB) ≈ 32.2GiB,取到 40'
    Assert-Equal -Expected 536870912 -Actual $plan.AuditRetentionBytes -Because '缺省沿用 broker 缺省'
}

Test-Case '放宽审计 retention.bytes 时盘预算按它算' {
    $plan = New-ProdDiskPlan -AuditRetentionBytes 2147483648
    Assert-Equal -Expected 50 -Actual $plan.RequiredGi -Because '1.3 × (9 × (2048 + 32)MiB + 16GiB + 4GiB) ≈ 49.8GiB,取到 50'
    Assert-Equal -Expected 2147483648 -Actual $plan.AuditRetentionBytes -Because '预算要带回实际用的值'
}

# 档位缺省值(缺口清单第 3 条 2026-11-11 定案)。Apply-KafkaProfileDefaults 只读写脚本级变量,喂一组全 0 的输入就能直接跑。
function Invoke-ProfileDefaults {
    param([Parameter(Mandatory = $true)][string]$Tier, [long]$AuditRetentionBytes = 0)
    $script:KafkaProfile = $Tier
    $script:KafkaBrokerRetentionMs = 0
    $script:KafkaRetentionCheckIntervalMs = 0
    $script:KafkaRetentionBytes = [long]0
    $script:KafkaSegmentBytes = [long]0
    $script:KafkaHeapOpts = ''
    $script:KafkaOtherTopicsReserveGi = 0
    $script:AuditKafkaRetentionBytes = $AuditRetentionBytes
    Apply-KafkaProfileDefaults
}

Test-Case '审计 retention.bytes 的档位缺省:prod / prod-like 4GiB(扛住 2 小时消费中断),dev 沿用 broker 缺省;显式值不被覆盖' {
    foreach ($case in @(
        @{ Tier = 'prod'; Want = [long]4294967296 }
        @{ Tier = 'prod-like'; Want = [long]4294967296 }
        @{ Tier = 'dev'; Want = [long]134217728 }
    )) {
        Invoke-ProfileDefaults -Tier $case.Tier
        Assert-Equal -Expected $case.Want -Actual ([long]$script:AuditKafkaRetentionBytes) -Because "$($case.Tier) 档的审计 retention.bytes(渲染出来不能是 0)"
    }
    Invoke-ProfileDefaults -Tier 'prod' -AuditRetentionBytes 2147483648
    Assert-Equal -Expected ([long]2147483648) -Actual ([long]$script:AuditKafkaRetentionBytes) -Because '显式 -AuditKafkaRetentionBytes 不被档位缺省覆盖'
    $plan = New-ProdDiskPlan -AuditRetentionBytes 4294967296
    Assert-Equal -Expected 80 -Actual $plan.RequiredGi -Because 'prod 档缺省:1.3 × (9 × (4096 + 32)MiB + 512 × 32MiB + 4GiB) ≈ 73.2GiB,取到 80'
}

Test-Case '审计 retention.bytes 为负时拒绝' {
    $thrown = $null
    try { New-ProdDiskPlan -AuditRetentionBytes -1 | Out-Null } catch { $thrown = $_.Exception.Message }
    Assert-True -Condition ($null -ne $thrown) -Because '负数不是合法的 retention.bytes'
}

Test-Case 'data-service 的 Kafka 段渲染两个审计 topic 的 retention.bytes,写路径把同一个值交给盘预算' {
    $deploy = Read-RepoText $script:DeployPath
    Assert-Match -Text $deploy -Pattern '(?m)^  TxLogRetentionBytes: \$\{AuditKafkaRetentionBytes\}\s*$' -Because '漏了 data-service 就按 broker 缺省建 topic'
    Assert-Match -Text $deploy -Pattern '(?m)^  SnapshotRetentionBytes: \$\{AuditKafkaRetentionBytes\}\s*$' -Because '快照 topic 同一个值'
    Assert-Match -Text $deploy -Pattern '-AuditRetentionBytes \$AuditKafkaRetentionBytes' -Because '盘预算要和渲染进 topic 的值相同'
    $configGo = Read-RepoText $script:DataServiceConfigGo
    Assert-Match -Text $configGo -Pattern 'TxLogRetentionBytes\s+int64' -Because 'data_service 要认这个键'
    Assert-Match -Text $configGo -Pattern 'SnapshotRetentionBytes\s+int64' -Because 'data_service 要认这个键'
}

Test-Case 'node-config 从仓库的 game_config.yaml 整段复制 audit_snapshot' {
    $deploy = Read-RepoText $script:DeployPath
    Assert-Match -Text $deploy -Pattern "Get-AuthoritativeYamlBlock -RelativePath 'bin/etc/game_config.yaml' -Key 'audit_snapshot'" -Because '开关只认仓库那一份 yaml'
    Assert-Match -Text $deploy -Pattern '(?m)^\$\{auditSnapshotBlock\}\s*$' -Because '漏了这一行,K8s 上改 yaml 不生效'
    $game = Read-RepoText $script:GameConfigPath
    Assert-Match -Text $game -Pattern '(?m)^audit_snapshot:\s*$' -Because 'Get-AuthoritativeYamlBlock 找不到顶层块会拒绝部署'
    foreach ($key in @('login_enabled', 'logout_enabled', 'sample_per_mille')) {
        Assert-Match -Text $game -Pattern "(?m)^  ${key}:" -Because "键名要和 config.cpp::readGameConfig 一致:$key"
    }
}

exit (Complete-TestRun -SuiteName 'audit_pipeline_deploy_contract')
