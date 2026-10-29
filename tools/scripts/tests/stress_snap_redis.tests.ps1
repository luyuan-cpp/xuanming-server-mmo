#requires -Version 7
<#
.SYNOPSIS
    stress_snap.ps1 的 Redis INFO 快照(-RedisTargets / -RedisInfoProvider)契约测试(L-18 观测批,2026-10-23)。
.DESCRIPTION
    在本进程里 `& stress_snap.ps1` 跑一个时刻(-Stages 0、StartTime 取过去,不等待;-Endpoints @{} 不发 HTTP),
    用 -RedisInfoProvider 注入 INFO 文本,不起 docker / kubectl,不访问任何 Redis。覆盖:
      - target 名与 spec 的校验在任何抓取、建目录之前完成;保留名(会撞 prom 快照后缀)与非法写法一律拒绝;
      - 成功时写 t<N>m_redis_<name>.txt,首行是 '# stress_snap target=… spec=… captured_at=…' 元信息,之后是 INFO 原文;
      - 某个 target 回了错误回复或抛异常:只在 job.log 记 FAIL、不写文件,后面的 target 照抓。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/stress_snap_redis.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$snapScript = Join-Path $repoRoot 'tools/scripts/stress_snap.ps1'
$tmpRoot = Join-Path ([IO.Path]::GetTempPath()) ('stress_snap_redis_' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $tmpRoot
$pastStart = (Get-Date).AddMinutes(-5).ToString('yyyy-MM-dd HH:mm:ss')

# 一份最小但形状合法的 INFO 回复(stress_snap 只校验至少有一行小写 key:value、且不是错误回复)。
$goodInfo = "# Server`nredis_version:7.2.4`nrun_id:fixture`nuptime_in_seconds:10`n# Stats`ntotal_commands_processed:5"

function New-RunDir {
    $dir = Join-Path $tmpRoot ([guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $dir
    return $dir
}

function Invoke-Snap {
    param(
        [Parameter(Mandatory = $true)][string]$RunDir,
        [Parameter(Mandatory = $true)][hashtable]$Targets,
        [scriptblock]$Provider = { param($name, $spec) "# Server`nrun_id:fixture`nuptime_in_seconds:10" },
        [string[]]$Sections = @('server', 'stats')
    )
    & $snapScript -RunDir $RunDir -StartTime $pastStart -Stages '0' -Endpoints @{} `
        -RedisTargets $Targets -RedisInfoSections $Sections -RedisInfoProvider $Provider
}

# 断言调用抛出且消息匹配,并且没有建 prom-snapshots(校验必须早于任何落盘)。
function Assert-RejectedBeforeCapture {
    param([string]$RunDir, [scriptblock]$Action, [string]$MessagePattern, [string]$Because)
    $message = $null
    try { & $Action } catch { $message = $_.Exception.Message }
    Assert-True -Condition ($null -ne $message) -Because "${Because}:必须抛错"
    Assert-Match -Text $message -Pattern $MessagePattern -Because "${Because}:错误信息要指明原因"
    Assert-True -Condition (-not (Test-Path (Join-Path $RunDir 'prom-snapshots'))) -Because "${Because}:校验必须在建目录、抓取之前"
}

Test-Case 'target 名:保留名 login / sm / db / ds 与非法写法在抓取之前拒绝' {
    foreach ($name in @('login', 'sm', 'db', 'ds')) {
        $dir = New-RunDir
        Assert-RejectedBeforeCapture -RunDir $dir -Action { Invoke-Snap -RunDir $dir -Targets @{ $name = 'docker:redis' } } `
            -MessagePattern '冲突' -Because "保留名 $name 会让 t<N>m_redis_$name.txt 被汇总脚本当成 _$name 快照"
    }
    foreach ($name in @('Bad_Name', '1core', 'core.x', ('a' * 33))) {
        $dir = New-RunDir
        Assert-RejectedBeforeCapture -RunDir $dir -Action { Invoke-Snap -RunDir $dir -Targets @{ $name = 'docker:redis' } } `
            -MessagePattern '非法' -Because "名字 $name 不匹配 ^[a-z][a-z0-9-]{0,31}$"
    }
}

Test-Case 'spec:只接受 docker:<container> 与 k8s:<ns>/<sts>[@<host>:<port>]' {
    foreach ($spec in @('redis://127.0.0.1:6379', 'docker:', 'k8s:infra', 'k8s:Infra/redis', 'k8s:infra/redis@host:99999', 'docker:redis;rm -rf /')) {
        $dir = New-RunDir
        Assert-RejectedBeforeCapture -RunDir $dir -Action { Invoke-Snap -RunDir $dir -Targets @{ core = $spec } } `
            -MessagePattern 'spec' -Because "spec '$spec' 必须拒绝"
    }
    $dir = New-RunDir
    Invoke-Snap -RunDir $dir -Targets @{ core = 'k8s:mmorpg-infra/redis@redis.mmorpg-infra:6379'; mapping = 'docker:redis' }
    Assert-True -Condition (Test-Path (Join-Path $dir 'prom-snapshots/t0m_redis_core.txt')) -Because '合法的 k8s spec 被接受(取数走注入的 provider)'
    Assert-True -Condition (Test-Path (Join-Path $dir 'prom-snapshots/t0m_redis_mapping.txt')) -Because '合法的 docker spec 被接受'
}

Test-Case 'INFO section 列表必须含 server(汇总按 run_id / uptime_in_seconds 求差)' {
    $dir = New-RunDir
    Assert-RejectedBeforeCapture -RunDir $dir -Action { Invoke-Snap -RunDir $dir -Targets @{ core = 'docker:redis' } -Sections @('clients', 'stats') } `
        -MessagePattern 'server' -Because '没有 server section 就无法判重启、算窗口'
}

Test-Case '成功:写 t0m_redis_<name>.txt,首行是抓取元信息,随后是 INFO 原文' {
    $dir = New-RunDir
    $provider = [scriptblock]::Create("param(`$name, `$spec) @'`n$goodInfo`n'@")
    Invoke-Snap -RunDir $dir -Targets @{ core = 'docker:redis' } -Provider $provider
    $file = Join-Path $dir 'prom-snapshots/t0m_redis_core.txt'
    Assert-True -Condition (Test-Path $file) -Because '成功时必须落盘'
    $lines = @(Get-Content -LiteralPath $file)
    Assert-Match -Text $lines[0] -Pattern '^# stress_snap target=core spec=docker:redis captured_at=\S+$' -Because '首行是元信息,汇总脚本按 "# stress_snap " 前缀跳过'
    Assert-True -Condition ($lines -contains 'uptime_in_seconds:10' -and $lines -contains 'total_commands_processed:5') -Because 'INFO 原文原样保留'
    Assert-Match -Text (Get-Content -LiteralPath (Join-Path $dir 'prom-snapshots/job.log') -Raw) -Pattern 't0m redis_core saved' -Because 'job.log 记一行 saved'
}

Test-Case '失败不中断:错误回复与异常只记 FAIL、不写文件,后面的 target 照抓' {
    $dir = New-RunDir
    $provider = {
        param($name, $spec)
        switch ($name) {
            'alpha' { 'NOAUTH Authentication required.' }
            'beta'  { throw 'simulated docker exec failure' }
            default { "# Server`nrun_id:fixture`nuptime_in_seconds:10" }
        }
    }
    Invoke-Snap -RunDir $dir -Targets @{ alpha = 'docker:a'; beta = 'docker:b'; gamma = 'docker:c' } -Provider $provider
    $snapDir = Join-Path $dir 'prom-snapshots'
    Assert-True -Condition (-not (Test-Path (Join-Path $snapDir 't0m_redis_alpha.txt'))) -Because '错误回复(NOAUTH)不能当成 INFO 落盘'
    Assert-True -Condition (-not (Test-Path (Join-Path $snapDir 't0m_redis_beta.txt'))) -Because '抓取抛异常时不落盘'
    Assert-True -Condition (Test-Path (Join-Path $snapDir 't0m_redis_gamma.txt')) -Because '前面的 target 失败不影响后面的 target'
    $log = Get-Content -LiteralPath (Join-Path $snapDir 'job.log') -Raw
    Assert-Match -Text $log -Pattern 'redis_alpha FAIL' -Because '错误回复记 FAIL'
    Assert-Match -Text $log -Pattern 'redis_beta FAIL' -Because '异常记 FAIL'
}

Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue
exit (Complete-TestRun -SuiteName 'stress_snap_redis')
