#requires -Version 7
<#
.SYNOPSIS
    控制域 Redis 监控(2026-10-23)的静态契约:每个控制域 Redis Pod 带钉了版本的 redis-exporter sidecar,
    口令只走环境变量,抓取注解与告警规则齐全;redis-match-cluster 为 noeviction。

.DESCRIPTION
    只读仓库里的静态文件:不调用 k8s_deploy.ps1,不访问集群、容器或网络。
    设计与拍板:docs/design/shared-redis-split-by-purpose.md §25.3(原 §13 第 6 条、第 11 条)。
    玩家域 Redis Cluster 的监控归 docs/design/redis-player-store-cluster.md,不在本文件。

    守住的东西:
      - E1 redis.yaml、redis-ha.yaml、redis-match-cluster.yaml 各有一个名为 redis-exporter 的容器,镜像钉到 vX.Y.Z,三份同一版本;
      - E2 exporter 连本 Pod 的 127.0.0.1:6379,指标端口 9121;口令只经 secretKeyRef 注入 REDIS_PASSWORD,命令行不出现口令参数;
      - E3 Pod 模板带 kubectl.kubernetes.io/default-container: redis 与 prometheus.io/port: "9121";
      - E4 deploy/k8s/redis-alerts.yaml 是 PrometheusRule,含约定的 8 条告警,描述里不引用玩家维度 label;
      - E5 redis-match-cluster 的淘汰策略是 noeviction(票据与锁带 TTL,volatile-* 会先淘汰它们)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/redis_exporter_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:InfraDir = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra'
# redis-purpose.yaml 是按用途拆出的 queue 实例模板(G4a 第 1a 批),exporter 版本与三份核心 / 集群清单一起钉住。
$script:ManifestNames = @('redis.yaml', 'redis-ha.yaml', 'redis-match-cluster.yaml', 'redis-purpose.yaml')
$script:AlertsPath = Join-Path $script:RepoRoot 'deploy/k8s/redis-alerts.yaml'

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { throw "找不到 $Path" }
    return ([IO.File]::ReadAllText($Path) -replace "`r`n", "`n")
}

# 取容器块:从 `- name: <Name>` 那一行起,到下一个缩进不大于该行 `-` 的非空行为止。
# 返回 $null 表示没有这个容器;多于一个时 throw,免得断言只看了其中一个。
function Get-ContainerBlock {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $lines = $Text -split "`n"
    $blocks = @()
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $m = [regex]::Match($lines[$i], '^(?<indent>[ ]*)- name:[ ]*' + [regex]::Escape($Name) + '[ ]*$')
        if (-not $m.Success) { continue }
        $dashIndent = $m.Groups['indent'].Value.Length
        $body = @($lines[$i])
        for ($j = $i + 1; $j -lt $lines.Count; $j++) {
            $line = $lines[$j]
            if ($line.Trim() -eq '') { $body += $line; continue }
            $indent = $line.Length - $line.TrimStart().Length
            if ($indent -le $dashIndent) { break }
            $body += $line
        }
        $blocks += ($body -join "`n")
    }
    if ($blocks.Count -gt 1) { throw "容器 $Name 出现了 $($blocks.Count) 次" }
    if ($blocks.Count -eq 0) { return $null }
    return $blocks[0]
}

# 去掉整行注释,只对生效的内容断言。
function Get-NonCommentText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return ((($Text -split "`n") | Where-Object { -not $_.TrimStart().StartsWith('#') }) -join "`n")
}

Test-Case 'E1:三份控制域 Redis 清单各有一个 redis-exporter 容器,镜像钉到 vX.Y.Z 且三份同版本' {
    $images = @{}
    foreach ($name in $script:ManifestNames) {
        $text = Get-NonCommentText (Read-RepoText (Join-Path $script:InfraDir $name))
        $block = Get-ContainerBlock -Text $text -Name 'redis-exporter'
        Assert-True -Condition ($null -ne $block) -Because "$name 必须有名为 redis-exporter 的容器"
        $image = [regex]::Match($block, '(?m)^[ ]+image:[ ]*(?<ref>\S+)[ ]*$')
        Assert-True -Condition $image.Success -Because "$name 的 redis-exporter 必须写 image"
        Assert-Match -Text $image.Groups['ref'].Value -Pattern '^oliver006/redis_exporter:v\d+\.\d+\.\d+$' -Because "$name 的 exporter 镜像必须钉到具体版本,不能用 latest 或浮动标签"
        $images[$name] = $image.Groups['ref'].Value
    }
    Assert-Equal -Expected 1 -Actual (@($images.Values | Sort-Object -Unique).Count) -Because '三份清单的 exporter 必须是同一个版本,升级时一起改'
}

Test-Case 'E2:exporter 连 127.0.0.1:6379、端口 9121;口令只经 secretKeyRef 注入,命令行不出现口令参数' {
    foreach ($name in $script:ManifestNames) {
        $text = Get-NonCommentText (Read-RepoText (Join-Path $script:InfraDir $name))
        $block = Get-ContainerBlock -Text $text -Name 'redis-exporter'
        Assert-Match -Text $block -Pattern '(?m)^[ ]+- name: REDIS_ADDR\n[ ]+value: redis://127\.0\.0\.1:6379[ ]*$' -Because "${name}:exporter 只连本 Pod 的 redis-server"
        Assert-Match -Text $block -Pattern '(?m)^[ ]+- containerPort: 9121\n[ ]+name: metrics[ ]*$' -Because "${name}:指标端口 9121,与抓取注解一致"
        Assert-NotMatch -Text $block -Pattern 'redis\.password|--redis\.password|requirepass' -Because "${name}:口令不得出现在 exporter 的参数里(会进 ps 与审计日志)"
        Assert-NotMatch -Text $block -Pattern '(?m)^[ ]+(args|command):' -Because "${name}:exporter 只用环境变量配置,不写 args / command"
        if ($block -match 'REDIS_PASSWORD') {
            Assert-Match -Text $block -Pattern '(?m)^[ ]+- name: REDIS_PASSWORD\n[ ]+valueFrom:\n[ ]+secretKeyRef:\n[ ]+name: redis-auth\n[ ]+key: password\n[ ]+optional: true[ ]*$' -Because "${name}:口令只能来自与 redis 容器同一个可选 Secret redis-auth"
        }
    }
    $coreText = Get-NonCommentText (Read-RepoText (Join-Path $script:InfraDir 'redis.yaml'))
    Assert-Match -Text (Get-ContainerBlock -Text $coreText -Name 'redis-exporter') -Pattern 'REDIS_PASSWORD' -Because 'redis.yaml 的实例在 release 档带口令,exporter 必须注入 REDIS_PASSWORD,否则 release 档 redis_up 恒为 0'
}

Test-Case 'E3:Pod 模板带 default-container: redis 与 9121 抓取注解' {
    foreach ($name in $script:ManifestNames) {
        $text = Get-NonCommentText (Read-RepoText (Join-Path $script:InfraDir $name))
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]+kubectl\.kubernetes\.io/default-container:[ ]*redis[ ]*$').Count) -Because "${name}:不带 -c 的 kubectl exec / logs 必须落到 redis 容器"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]+prometheus\.io/port:[ ]*"9121"[ ]*$').Count) -Because "${name}:抓取注解指向 exporter 的 9121"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]+prometheus\.io/scrape:[ ]*"true"[ ]*$').Count) -Because "${name}:抓取注解开启"
    }
}

Test-Case 'E4:redis-alerts.yaml 是 PrometheusRule,含约定的 8 条告警,不引用玩家维度 label' {
    $text = Read-RepoText $script:AlertsPath
    Assert-Match -Text $text -Pattern '(?m)^kind: PrometheusRule[ ]*$' -Because '与仓库其他告警文件同为 PrometheusRule CR'
    foreach ($alert in @('RedisDown', 'RedisMemoryNearMax', 'RedisOomRejected', 'RedisConnectionsRejected', 'RedisConnectionsNearMax', 'RedisAofWriteFailed', 'RedisReplicaLinkDown', 'RedisClusterStateNotOk')) {
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]+- alert: ' + $alert + '[ ]*$').Count) -Because "告警 $alert 必须恰好出现一次"
    }
    Assert-NotMatch -Text $text -Pattern 'player_id|\$labels\.player' -Because 'Redis 告警不得用 player_id 一类高基数维度(AGENTS.md §9)'
}

Test-Case 'E5:redis-match-cluster 的淘汰策略是 noeviction,不再是 volatile-lru' {
    $text = Get-NonCommentText (Read-RepoText (Join-Path $script:InfraDir 'redis-match-cluster.yaml'))
    Assert-Match -Text $text -Pattern '(?m)^[ ]+maxmemory-policy noeviction[ ]*$' -Because '票据、匹配锁、挑战锁带 TTL,volatile-* 写满时会先淘汰它们,互斥随之失效'
    Assert-NotMatch -Text $text -Pattern 'volatile-lru' -Because '生效配置里不得再有 volatile-lru'
}

exit (Complete-TestRun -SuiteName 'redis_exporter_contract')
