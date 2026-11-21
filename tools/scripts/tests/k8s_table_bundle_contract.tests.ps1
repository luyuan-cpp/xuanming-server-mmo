#requires -Version 7
<#
.SYNOPSIS
    配置表热更分发 sidecar(table-sync)与告警规则的契约测试(docs/design/config-table-hot-reload.md §7、§11)。

.DESCRIPTION
    核对:
      1. deploy/k8s/table-bundle-alerts.yaml 的 4 条告警(TableBundleRejected / TableBundleNotConverged /
         TableSyncStale / TableBundleNeedsRestart)、指标名与 C++/Go/sidecar 代码一致、runbook 文件存在、告警名全仓唯一;
      2. k8s_deploy.ps1 里的 table-sync sidecar 注入:
         - 仅作用于 6 个读表 Go 服务(guild/login/mail/match/rank/scene-manager),排除 player-locator / trade / chat / db 等;
         - 给已有 initContainers 的 StatefulSet(guild)和无 initContainers 的 Deployment(scene-manager)都能干净插入;
         - 给 Agones Fleet 挂 table-sync(哪怕 -NoCppLogSidecar 关了 Alloy)时仍显式写出 container: <FleetName>;
         - 未启用 table-sync 时原样返回(不改写既有清单);
      3. go_svc_image.ps1 登记了 table-sync 镜像条目。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_table_bundle_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

function Import-K8sDeployFunctions {
    param([Parameter(Mandatory = $true)][string[]]$Names)
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        (Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
    Assert-Equal -Expected 0 -Actual $parseErrors.Count -Because 'k8s_deploy.ps1 必须无语法错误'
    foreach ($name in $Names) {
        $fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -ceq $name }, $true)
        if ($null -eq $fn) { throw "k8s_deploy.ps1 缺少函数 $name" }
        Set-Item -Path "Function:script:$name" -Value $fn.Body.GetScriptBlock()
    }
}

Test-Case 'table-bundle-alerts.yaml 包含 4 条告警、指标与代码同源、runbook 存在且告警名全仓唯一' {
    $alertsText = Read-RepoText 'deploy/k8s/table-bundle-alerts.yaml'
    Assert-Match -Text $alertsText -Pattern '(?m)^kind:\s*PrometheusRule\s*$' -Because '标准 PrometheusRule 清单'

    $rules = @([regex]::Matches($alertsText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|\z)') |
        ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
    $names = @($rules | ForEach-Object { $_.Name })
    Assert-Equal -Expected 'TableBundleRejected,TableBundleNotConverged,TableSyncStale,TableBundleNeedsRestart' `
        -Actual ($names -join ',') -Because '四条告警名与设计 §11 对齐'

    $expectedSeverity = @{
        TableBundleRejected     = 'critical'
        TableBundleNotConverged = 'warning'
        TableSyncStale          = 'warning'
        TableBundleNeedsRestart = 'info'
    }
    foreach ($r in $rules) {
        Assert-Match -Text $r.Body -Pattern ("severity:\s*" + $expectedSeverity[$r.Name]) -Because "$($r.Name) 严重级别对齐 §11"
        Assert-Match -Text $r.Body -Pattern 'runbook_url:\s*docs/ops/table-hot-reload-runbook\.md' -Because "$($r.Name) 指向运维手册"
    }
    Assert-True -Condition (Test-Path (Join-Path $script:RepoRoot 'docs/ops/table-hot-reload-runbook.md')) -Because 'runbook 文件必须存在'

    # 指标名与 C++ / Go / sidecar 源码一致。
    $cppMetrics = Read-RepoText 'cpp/libs/engine/core/table_bundle/table_bundle_manager.cpp'
    $goMetrics = Read-RepoText 'go/shared/tablebundle/metrics.go'
    $syncMain = Read-RepoText 'go/table_sync/cmd/table-sync/main.go'
    Assert-Match -Text $cppMetrics -Pattern 'table_bundle_info' -Because 'C++ 暴露 table_bundle_info'
    Assert-Match -Text $cppMetrics -Pattern 'table_reload_total' -Because 'C++ 暴露 table_reload_total'
    Assert-Match -Text $goMetrics -Pattern '"table_bundle_info"' -Because 'Go 暴露 table_bundle_info'
    Assert-Match -Text $goMetrics -Pattern '"table_reload_total"' -Because 'Go 暴露 table_reload_total'
    Assert-Match -Text $syncMain -Pattern '"table_sync_etcd_last_read_timestamp_seconds"' -Because 'sidecar 暴露 table_sync_etcd_last_read_timestamp_seconds'

    # 告警名在 deploy/k8s/*alerts*.yaml 中不重名。
    $allAlertFiles = Get-ChildItem (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*alerts*.yaml'
    $otherAlertNames = @()
    foreach ($f in $allAlertFiles) {
        if ($f.Name -eq 'table-bundle-alerts.yaml') { continue }
        $txt = ([IO.File]::ReadAllText($f.FullName)) -replace "`r`n", "`n"
        foreach ($m in [regex]::Matches($txt, '(?m)^\s*- alert:\s*(\S+)')) {
            $otherAlertNames += $m.Groups[1].Value
        }
    }
    foreach ($n in $names) {
        Assert-True -Condition ($n -notin $otherAlertNames) -Because "告警名 $n 不得与其它告警文件重名"
    }
}

Test-Case 'table-sync sidecar 只注入读表 Go 服务,兼容有/无 initContainers,关闭时原样返回' {
    Import-K8sDeployFunctions -Names @(
        'Resolve-ImagePullPolicy',
        'Get-TableSyncSettings',
        'New-TableSyncMainEnvLines',
        'New-TableSyncMainVolumeMountYaml',
        'New-TableSyncVolumeYaml',
        'New-TableSyncSidecarContainerYaml',
        'Add-TableSyncSidecarToGoSvcManifest'
    )
    $script:InfraNamespace = 'mmorpg-infra'
    $script:GoSvcRegistry = 'ghcr.io/luyuancpp'
    $script:GoSvcTag = 'v1'
    $script:TableSyncImage = ''
    $script:TableSyncS3Endpoint = 'https://s3.example.internal'
    $script:TableSyncS3Bucket = 'mmorpg-tables'
    $script:TableSyncS3Region = 'ap-east-1'
    $script:TableSyncS3SecretName = 'table-sync-s3'
    $script:TableSyncDevFixture = [switch]$false
    $script:TableSyncGoServices = @('guild', 'login', 'mail', 'match', 'rank', 'scene-manager')
    $script:TableSyncMountPath = '/var/lib/mmorpg/tables'
    $script:TableSyncStatusPath = '/var/lib/mmorpg/tables-status'

    $guildYaml = Read-RepoText 'deploy/k8s/manifests/go-svc/guild.yaml'
    $smYaml = Read-RepoText 'deploy/k8s/manifests/go-svc/scene-manager.yaml'
    $plYaml = Read-RepoText 'deploy/k8s/manifests/go-svc/player-locator.yaml'

    # 未启用时逐字不变。
    $script:TableSyncEnabled = $false
    Assert-Equal -Expected $guildYaml -Actual (Add-TableSyncSidecarToGoSvcManifest -SvcName 'guild' -ManifestContent $guildYaml) -Because '关着不改 guild'

    # 启用后:已有 initContainers 的 guild、无 initContainers 的 scene-manager 都注入;不读表的 player-locator 不注入。
    $script:TableSyncEnabled = $true
    $guildOut = Add-TableSyncSidecarToGoSvcManifest -SvcName 'guild' -ManifestContent $guildYaml -Track 'canary'
    Assert-Match -Text $guildOut -Pattern '- name: table-sync' -Because 'guild 挂上 table-sync'
    Assert-Match -Text $guildOut -Pattern 'restartPolicy: Always' -Because '原生 sidecar'
    Assert-Match -Text $guildOut -Pattern 'path: /readyz' -Because 'startupProbe 探 /readyz'
    Assert-Match -Text $guildOut -Pattern 'MMORPG_RELEASE_TRACK\n\s+value: "canary"' -Because '传递轨道'
    Assert-Match -Text $guildOut -Pattern 'MMORPG_TABLE_BUNDLE_ROOT' -Because '主容器注入表包路径'
    Assert-Match -Text $guildOut -Pattern '- name: sql-writer-registrar' -Because '保留原有 initContainer'

    $smOut = Add-TableSyncSidecarToGoSvcManifest -SvcName 'scene-manager' -ManifestContent $smYaml -Track 'stable'
    Assert-Match -Text $smOut -Pattern 'initContainers:\n\s+- name: table-sync' -Because 'scene-manager 新建 initContainers 段'
    Assert-Match -Text $smOut -Pattern 'name: table-bundles' -Because '挂载 table-bundles 卷'

    $plOut = Add-TableSyncSidecarToGoSvcManifest -SvcName 'player-locator' -ManifestContent $plYaml -Track 'stable'
    Assert-Equal -Expected $plYaml -Actual $plOut -Because 'player-locator 不读表,不挂 table-sync'
}

Test-Case 'go_svc_image.ps1 登记了 table-sync 镜像条目且 Agones Fleet 在仅开 table-sync 时仍写出 container 行' {
    $imgText = Read-RepoText 'tools/scripts/go_svc_image.ps1'
    Assert-Match -Text $imgText -Pattern '"table-sync"\s*=\s*@\{\s*Dir\s*=\s*"table_sync";\s*Entry\s*=\s*"\./cmd/table-sync";\s*ImageName\s*=\s*"mmorpg-table-sync"\s*\}' `
        -Because 'go_svc_image.ps1 必须登记 mmorpg-table-sync'

    $deployText = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    Assert-Match -Text $deployText -Pattern 'if \(\$sidecarInitItems\.Count -gt 0\)\s*\{\s*\$gameServerContainerLine = "      container: \$FleetName`n"\s*\}' `
        -Because 'Agones Fleet 在仅有 table-sync(无 Alloy)时也必须写出 container: $FleetName'
}

Complete-TestSuite
