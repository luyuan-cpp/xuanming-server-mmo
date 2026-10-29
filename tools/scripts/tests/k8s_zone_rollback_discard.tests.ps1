#requires -Version 7
<#
.SYNOPSIS
    k8s_zone_rollback.ps1 回档丢弃语义的契约测试(2026-10-18,取代 k8s_zone_rollback_flush_guard.tests.ps1)。

.DESCRIPTION
    背景:负责人 2026-10-17 裁决回档 =「回到 TargetTime 时刻」,丢弃 T 之后的写。R7 复核发现 K8s 与本机的 go/db 游标、重试收据、
    归属映射、账号目录与各 zone 的玩家缓存同在一个 Redis DB:旧 Step 4 的 FLUSHDB 防护在 K8s 上必拒(Step 4 被跳过、游标不清、
    T 之后的父快照留在缓存里被 login 快路径直接返回),带 -ConfirmFlushSharedRedis 硬清又等于事故。改为:Step 5 -ToLatest、
    读 MySQL / 写 Redis 一律交给回档工具 zone_rollback_discard(-RollbackDiscardTool),本脚本只编排。

    本套钉住(k8s_zone_rollback_gate_router_mode.tests.ps1 另钉 -Apply 的调用顺序与各步拒绝):
      - FLUSHDB 与 redis-cli 从回档里消失:防护函数随旧 Step 4 删除,dry-run 与 -Apply 沙箱的输出与调用里都没有它们;
      - E2:-ConfirmFlushSharedRedis、-RedisPassword(以及 dev_tools 的 -RollbackConfirmFlushSharedRedis / -RollbackRedisPassword)在入口被拒,
        没有任何下游调用;
      - E1:TargetTime 只收 UTC 的 yyyy-MM-ddTHH:mm:ss[.fff]Z 且早于当前,-PitrRestoredTo 必须等于它;
      - E3:-Apply 缺 -RedisHost / -MySqlDsnEnv(或该变量为空)/ -RollbackDiscardTool / -BarrierProtocol、-SkipMySqlPause 缺 -PitrRestoredTo,
        都在入口被拒;dry-run 放行并打印占位符;
      - 待负责人裁决的路径 fail-closed:-BarrierProtocol strict(#3)、-AcceptBlockedPlayers(#4);
      - 口令与 DSN 只以环境变量名出现:不进 argv、不进输出、不进报告目录;
      - 第 0 步 f 的部署事实:只取非机密字段,与本次参数 / 本机 db.yaml 逐项核对,namespace 已删时复用同一 rid 的存档;
      - dry-run 打印第 0 步 f、1b、4a、4b、5b 的完整工具命令,都带 -zone / -topic-generation / -rollback-id。

    子进程用例全部是 dry-run;-Apply 用例在进程内沙箱里跑(lib/k8s_zone_rollback_sandbox.ps1),每一步都落在假下游上。
    纯函数按 AST 从被测脚本取出执行。

.EXAMPLE
    pwsh -File tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. "$PSScriptRoot/lib/k8s_zone_rollback_sandbox.ps1"

Write-Host ""
Write-Host "=== k8s_zone_rollback.ps1 discard-after-TargetTime contract tests ==="

$rollbackPath = Join-Path (Get-ToolsScriptsDir) 'k8s_zone_rollback.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($rollbackPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw "k8s_zone_rollback.ps1 解析失败: $($parseErrors | Out-String)" }
$pureFunctions = @('ConvertFrom-RollbackUtcTime', 'New-RollbackId', 'Get-DiscardToolArgs', 'Format-CommandLine', 'Get-DeploySnapshotKubectlArgs',
    'ConvertFrom-ZoneDeploySnapshot', 'Get-DeployFactsProblems', 'ConvertTo-DeployFactsKey',
    # 回档复核修正(2026-10-18):[4] Redis 实例身份、[5] 工具构建提交、[10] 入口决定。
    'Get-GoDbRedisRunIdExecArgs', 'ConvertFrom-RedisInfoRunId', 'ConvertFrom-DiscardToolVersion', 'Test-DiscardToolMatchesDbImage',
    'Get-ZoneEntryDecision', 'Get-ZoneEntryDecisionValue', 'Compare-ZoneEntryDecision', 'Format-ZoneEntryDecisionArgs')
foreach ($name in $pureFunctions) {
    $matched = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true))
    if ($matched.Count -ne 1) { throw "k8s_zone_rollback.ps1 里应有且只有一个 $name,实际 $($matched.Count) 个(契约测试的前提被改掉了)" }
    . ([scriptblock]::Create($matched[0].Extent.Text))
}

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Body)
    try { & $Body; return $null } catch { return $_.Exception.Message }
}

$RollbackBaseArgs = @(
    "-ZoneName", "contract-test", "-ZoneId", "101",
    "-TargetTime", "2026-05-15T14:23:00Z",
    "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab"
)
# 三项入口参数都显式,沙箱用例不读集群的入口现状,只看回档丢弃相关的步骤。
$QuietEntryArgs = @{ GateRouterMode = '1'; ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }

$RepoDbYaml = Join-Path (Get-RepoRoot) 'go' 'db' 'etc' 'db.yaml'
$DbGroupBase = [string](Get-YamlScalar -Path $RepoDbYaml -KeyPath 'ServerConfig.Kafka.GroupID').Value
$DbTopicGeneration = [string](Get-YamlScalar -Path $RepoDbYaml -KeyPath 'ServerConfig.Kafka.TopicGeneration').Value
$DbPartitionCnt = [string](Get-YamlScalar -Path $RepoDbYaml -KeyPath 'ServerConfig.Kafka.PartitionCnt').Value

# 断言一次运行在入口被拒:没有任何下游调用。
function Assert-RefusedAtEntry {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)][string]$Pattern
    )
    Assert-True -Condition ($null -ne $Run.Error) -Because "${Label}:必须在入口拒绝。输出: $($Run.Output)"
    Assert-Match -Text $Run.Error -Pattern $Pattern -Because "${Label}:错误文本要点名原因"
    $calls = $Run.DevToolsCalls.Count + $Run.KubectlCalls.Count + $Run.SnapshotCalls.Count + $Run.DiscardCalls.Count + $Run.KafkaCliCalls.Count + $Run.KafkaExecCalls.Count + $Run.KafkaResetBound.Count + $Run.LoginAuthCalls.Count
    Assert-Equal -Expected 0 -Actual $calls -Because "${Label}:入口校验早于任何下游调用(时间线: $($Run.Timeline -join ' > '))"
}

# ─────────────────────────────────────────────────────────────────
# FLUSHDB 与 redis-cli 从回档里消失
# ─────────────────────────────────────────────────────────────────

Test-Case 'FLUSHDB 防护函数随旧 Step 4 一并删除,脚本里没有任何 redis-cli 调用' {
    foreach ($name in @('Get-SharedRedisGuardPatterns', 'Find-SharedRedisGuardHits', 'Assert-RedisFlushTargetIsZoneLocal', 'New-RedisCliScanProbe')) {
        $left = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true))
        Assert-Equal -Expected 0 -Actual $left.Count -Because "$name 已随 FLUSHDB 一起删除"
    }
    $redisCalls = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'redis-cli' }, $true))
    Assert-Equal -Expected 0 -Actual $redisCalls.Count -Because '写 Redis 只经回档工具,本脚本不直接调 redis-cli'
}

Test-Case 'dry-run 与 -Apply 沙箱:输出里没有 FLUSHDB,沙箱里一次 redis-cli 都没调' {
    $dry = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-RedisHost', 'redis.infra', '-SkipMySqlPause'))
    Assert-Equal -Expected 0 -Actual $dry.ExitCode -Because "dry-run 不该失败。输出: $($dry.Output)"
    Assert-NotMatch -Text $dry.Output -Pattern 'FLUSHDB' -Because '回档不再 FLUSHDB(Step 7 的人工核对清单里只有只读的 EXISTS / LLEN)'
    $apply = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs
    Assert-True -Condition ($null -eq $apply.Error) -Because "全流程不该失败。错误: $($apply.Error)"
    Assert-NotMatch -Text $apply.Output -Pattern 'FLUSHDB' -Because '-Apply 全程没有 FLUSHDB'
    Assert-Equal -Expected 0 -Actual $apply.RedisCliCalls.Count -Because '-Apply 全程不调 redis-cli'
}

# ─────────────────────────────────────────────────────────────────
# E2:废弃参数在入口被拒
# ─────────────────────────────────────────────────────────────────

Test-Case 'E2:-ConfirmFlushSharedRedis / -RedisPassword 在入口被拒,没有任何下游调用' {
    $flush = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-ConfirmFlushSharedRedis'))
    Assert-True -Condition ($flush.ExitCode -ne 0) -Because "-ConfirmFlushSharedRedis 必须被拒,不能静默忽略。输出: $($flush.Output)"
    Assert-Match -Text $flush.Output -Pattern '回档已不做 FLUSHDB' -Because '错误文本要说明为什么不再支持'
    Assert-Match -Text $flush.Output -Pattern 'AGENTS\.md §6\.2' -Because '整套环境重建另有流程'
    Assert-NotMatch -Text $flush.Output -Pattern 'Preflight|Step 1:' -Because '入口即拒'

    $password = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-RedisPassword', 'Sup3rSecret'))
    Assert-True -Condition ($password.ExitCode -ne 0) -Because "-RedisPassword 必须被拒。输出: $($password.Output)"
    Assert-Match -Text $password.Output -Pattern '-RedisPasswordEnv' -Because '要指向替代做法'
    Assert-NotMatch -Text $password.Output -Pattern 'Sup3rSecret' -Because '拒绝文本也不得回显口令'

    $sandbox = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ ConfirmFlushSharedRedis = $true } + $QuietEntryArgs)
    Assert-RefusedAtEntry -Run $sandbox -Label '-Apply -ConfirmFlushSharedRedis' -Pattern '回档已不做 FLUSHDB'
}

Test-Case 'E2:dev_tools 的 -RollbackConfirmFlushSharedRedis / -RollbackRedisPassword 在调用回滚脚本之前被拒' {
    $devArgs = @(
        '-Command', 'k8s-zone-rollback',
        '-ZoneName', 'contract-test', '-ZoneId', '101',
        '-RollbackTargetTime', '2026-05-15T14:23:00Z',
        '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab',
        '-RollbackSkipMySqlPause'
    )
    $flush = Invoke-ToolScript -ScriptName 'dev_tools.ps1' -Arguments ($devArgs + @('-RollbackConfirmFlushSharedRedis'))
    Assert-True -Condition ($flush.ExitCode -ne 0) -Because "必须拒绝。输出: $($flush.Output)"
    Assert-Match -Text $flush.Output -Pattern '不再支持 -RollbackConfirmFlushSharedRedis' -Because '错误文本要点名参数'
    Assert-NotMatch -Text $flush.Output -Pattern 'k8s-zone-rollback \((DRY-RUN|APPLY)\)' -Because '拒绝早于调用回滚脚本'

    $password = Invoke-ToolScript -ScriptName 'dev_tools.ps1' -Arguments ($devArgs + @('-RollbackRedisPassword', 'Sup3rSecret'))
    Assert-True -Condition ($password.ExitCode -ne 0) -Because "必须拒绝。输出: $($password.Output)"
    Assert-Match -Text $password.Output -Pattern '-RollbackRedisPasswordEnv' -Because '要指向替代做法'
    Assert-NotMatch -Text $password.Output -Pattern 'Sup3rSecret' -Because '不得回显口令'
    Assert-NotMatch -Text $password.Output -Pattern 'k8s-zone-rollback \((DRY-RUN|APPLY)\)' -Because '拒绝早于调用回滚脚本'
}

# ─────────────────────────────────────────────────────────────────
# E1:TargetTime / PitrRestoredTo
# ─────────────────────────────────────────────────────────────────

Test-Case 'E1:TargetTime 只收早于当前的 UTC yyyy-MM-ddTHH:mm:ss[.fff]Z,其余写法在入口被拒' {
    foreach ($bad in @('2026-05-15 14:23:00', '2026-05-15T14:23:00+08:00', '2026-05-15T14:23:00', '2099-01-01T00:00:00Z', '2026-02-30T00:00:00Z', '2026-05-15T14:23:00.1234Z')) {
        $run = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments @('-ZoneName', 'contract-test', '-ZoneId', '101', '-TargetTime', $bad, '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "TargetTime '$bad' 必须在入口被拒。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern 'TargetTime' -Because "TargetTime '$bad':错误文本要点名参数"
        Assert-NotMatch -Text $run.Output -Pattern 'Preflight|Step 1:' -Because "TargetTime '$bad':入口即拒"
    }
    $empty = Invoke-RollbackInSandbox -DeployDefault '1' -ExtraArgs (@{ TargetTime = '' } + $QuietEntryArgs)
    Assert-True -Condition ($null -ne $empty.Error) -Because '空串必须被拒'
    Assert-Equal -Expected 0 -Actual ($empty.DevToolsCalls.Count + $empty.DiscardCalls.Count) -Because '空串:没有任何下游调用'
    foreach ($good in @('2026-05-15T14:23:00Z', '2026-05-15T14:23:00.250Z', '2026-05-15T14:23:00.5Z')) {
        $run = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments @('-ZoneName', 'contract-test', '-ZoneId', '101', '-TargetTime', $good, '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "TargetTime '$good' 是合法写法,dry-run 不该失败。输出: $($run.Output)"
    }
}

Test-Case 'E1:ConvertFrom-RollbackUtcTime 解析成 Kind=Utc 的同一时刻;非法日期与空串 throw' {
    $parsed = ConvertFrom-RollbackUtcTime -Text '2026-05-15T14:23:00.250Z' -Name 'TargetTime'
    Assert-Equal -Expected 'Utc' -Actual $parsed.Kind -Because '结果必须是 UTC,不被本机时区换算'
    Assert-Equal -Expected '2026-05-15 14:23:00.250' -Actual $parsed.ToString('yyyy-MM-dd HH:mm:ss.fff', [System.Globalization.CultureInfo]::InvariantCulture) -Because '毫秒保留'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-RollbackUtcTime -Text '' -Name 'TargetTime' })) -Because '空串必须 throw'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RollbackUtcTime -Text '2026-13-01T00:00:00Z' -Name 'TargetTime' }) -Pattern '越界' -Because '月份越界要说明原因'
    Assert-Equal -Expected 'z101-20260515T142300Z-20261018T031500Z' -Actual (New-RollbackId -ZoneId 101 -TargetUtc $parsed -NowUtc ([datetime]::SpecifyKind([datetime]'2026-10-18T03:15:00', 'Utc'))) -Because 'rid 缺省格式 z<id>-<T 紧凑>-<当前紧凑>'
}

Test-Case 'E1:-PitrRestoredTo 必须与 TargetTime 同一时刻;-RollbackId 只收 [A-Za-z0-9_-]' {
    $mismatch = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-PitrRestoredTo', '2026-05-15T14:24:00Z'))
    Assert-True -Condition ($mismatch.ExitCode -ne 0) -Because "PITR 恢复到别的时刻必须拒绝。输出: $($mismatch.Output)"
    Assert-Match -Text $mismatch.Output -Pattern 'PitrRestoredTo' -Because '错误文本要点名参数'
    $same = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-PitrRestoredTo', '2026-05-15T14:23:00.000Z'))
    Assert-Equal -Expected 0 -Actual $same.ExitCode -Because "同一时刻的不同写法放行。输出: $($same.Output)"
    $badId = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-RollbackId', 'z101:bad*'))
    Assert-True -Condition ($badId.ExitCode -ne 0) -Because "rid 会拼进 Redis 键名,含 : 与 * 必须拒绝。输出: $($badId.Output)"
    Assert-Match -Text $badId.Output -Pattern 'RollbackId' -Because '错误文本要点名参数'
    $quiesce = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-QuiesceSeconds', '30'))
    Assert-True -Condition ($quiesce.ExitCode -ne 0) -Because "静默窗口不得低于 300s(librdkafka message.timeout.ms 缺省值)。输出: $($quiesce.Output)"
    Assert-Match -Text $quiesce.Output -Pattern 'QuiesceSeconds' -Because '错误文本要点名参数'
}

# ─────────────────────────────────────────────────────────────────
# E3:-Apply 的显式目标;待裁决路径 fail-closed
# ─────────────────────────────────────────────────────────────────

Test-Case 'E3:-Apply 缺 -RedisHost / -MySqlDsnEnv(或变量为空)/ -RollbackDiscardTool / -BarrierProtocol / -PitrRestoredTo,都在入口被拒' {
    $cases = @(
        @{ Label = '缺 -RedisHost'; Extra = @{ RedisHost = $null }; Pattern = '-RedisHost <go/db 的共享 Redis' },
        @{ Label = '缺 -MySqlDsnEnv'; Extra = @{ MySqlDsnEnv = $null }; Pattern = '-MySqlDsnEnv <存放 zone_101_db DSN' },
        @{ Label = '缺 -RollbackDiscardTool'; Extra = @{ RollbackDiscardTool = $null }; Pattern = '-RollbackDiscardTool <与线上 go/db 同一提交构建' },
        @{ Label = '工具文件不存在'; Extra = @{ RollbackDiscardTool = (Join-Path ([IO.Path]::GetTempPath()) 'no-such-zone_rollback_discard.exe') }; Pattern = '不是存在的文件' },
        @{ Label = '缺 -BarrierProtocol'; Extra = @{ BarrierProtocol = $null }; Pattern = '-BarrierProtocol legacy' },
        @{ Label = '-SkipMySqlPause 缺 -PitrRestoredTo'; Extra = @{ PitrRestoredTo = $null }; Pattern = '-PitrRestoredTo <PITR 实际恢复到的时刻' }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs ($c.Extra + $QuietEntryArgs)
        Assert-RefusedAtEntry -Run $run -Label $c.Label -Pattern ([regex]::Escape($c.Pattern))
        Assert-Match -Text $run.Error -Pattern '-Apply 缺少以下' -Because "$($c.Label):要一次列全缺的项"
    }
    $emptyDsn = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -EnvVars @{ MMORPG_CONTRACT_ROLLBACK_DSN = $null }
    Assert-RefusedAtEntry -Run $emptyDsn -Label 'DSN 变量为空' -Pattern '环境变量 MMORPG_CONTRACT_ROLLBACK_DSN 为空'
    $emptyPassword = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RedisPasswordEnv = 'MMORPG_CONTRACT_REDIS_PW_UNSET' } + $QuietEntryArgs) `
        -EnvVars @{ MMORPG_CONTRACT_ROLLBACK_DSN = 'contract:dsn'; MMORPG_CONTRACT_REDIS_PW_UNSET = $null }
    Assert-RefusedAtEntry -Run $emptyPassword -Label '口令变量为空' -Pattern '环境变量 MMORPG_CONTRACT_REDIS_PW_UNSET 为空'
    $all = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RedisHost = $null; MySqlDsnEnv = $null; BarrierProtocol = $null } + $QuietEntryArgs)
    Assert-Match -Text $all.Error -Pattern '缺少以下 3 项' -Because '缺的项一次列全,不让操作者修一项跑一次'
    $badName = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ MySqlDsnEnv = 'user:pw@tcp(x)/db' } + $QuietEntryArgs)
    Assert-RefusedAtEntry -Run $badName -Label '把 DSN 当成变量名' -Pattern '不是合法的环境变量名'
    Assert-NotMatch -Text $badName.Error -Pattern 'pw@tcp' -Because '误把 DSN 本身传进来时,错误文本不得回显它(可能带口令)'
}

Test-Case 'E3:dry-run 不要求这些参数,命令里打印占位符' {
    $run = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-SkipMySqlPause'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dry-run 不该失败。输出: $($run.Output)"
    foreach ($placeholder in @('<zone_rollback_discard>', '<mysql dsn env var>', "'<go/db shared redis host:port>'", '<legacy>')) {
        Assert-Match -Text $run.Output -Pattern ([regex]::Escape($placeholder)) -Because "dry-run 缺参数时打印占位符 $placeholder"
    }
    Assert-Match -Text $run.Output -Pattern 'not set; -Apply requires -RedisHost' -Because '摘要区提醒 -Apply 必填'
}

Test-Case '待裁决路径 fail-closed:-BarrierProtocol strict(#3)与 -AcceptBlockedPlayers(#4)在入口被拒' {
    $strict = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-BarrierProtocol', 'strict'))
    Assert-True -Condition ($strict.ExitCode -ne 0) -Because "strict 世代的处置待负责人裁决。输出: $($strict.Output)"
    Assert-Match -Text $strict.Output -Pattern '待负责人裁决\(#3\)' -Because '要点明这是待裁决事项'
    $strictUpper = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-BarrierProtocol', 'Strict'))
    Assert-True -Condition ($strictUpper.ExitCode -ne 0) -Because 'ValidateSet 不分大小写,Strict 同样要拒'
    $accept = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-AcceptBlockedPlayers', '1'))
    Assert-True -Condition ($accept.ExitCode -ne 0) -Because "blocked 玩家的处置待负责人裁决。输出: $($accept.Output)"
    Assert-Match -Text $accept.Output -Pattern '待负责人裁决\(#4\)' -Because '要点明这是待裁决事项'
    Assert-NotMatch -Text $accept.Output -Pattern 'Preflight|Step 1:' -Because '入口即拒'
}

# ─────────────────────────────────────────────────────────────────
# 口令与 DSN 只以环境变量名出现
# ─────────────────────────────────────────────────────────────────

Test-Case '口令与 DSN 只在环境变量里:argv、输出、报告目录都没有它们的值,也没有线上 ConfigMap 里的口令' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RedisPasswordEnv = 'MMORPG_CONTRACT_REDIS_PW' } + $QuietEntryArgs) `
        -EnvVars @{ MMORPG_CONTRACT_ROLLBACK_DSN = 'contract:Dsn5ecret@tcp(mysql:3306)/zone_101_db'; MMORPG_CONTRACT_REDIS_PW = 'Sup3rSecret' }
    Assert-True -Condition ($null -eq $run.Error) -Because "全流程不该失败。错误: $($run.Error)"
    $everything = @($run.Output, ($run.DiscardCalls -join "`n"), (@($run.ReportFiles.Values) -join "`n")) -join "`n"
    foreach ($secret in @('Sup3rSecret', 'Dsn5ecret', 'contract-live-secret-never-read')) {
        Assert-NotMatch -Text $everything -Pattern ([regex]::Escape($secret)) -Because "$secret 不得出现在命令行、输出或报告目录里(AGENTS §9)"
    }
    Assert-Match -Text ($run.DiscardCalls -join "`n") -Pattern '-redis-password-env MMORPG_CONTRACT_REDIS_PW(\s|$)' -Because '工具只收口令变量名'
    Assert-True -Condition (@($run.DiscardEnv | Where-Object { $_ -match 'MMORPG_CONTRACT_REDIS_PW=set$' }).Count -eq 5) -Because "工具进程里看得到口令变量(实际: $($run.DiscardEnv -join ' | '))"
    Assert-True -Condition ($run.ReportFiles.ContainsKey('deploy-facts.json') -and $run.ReportFiles.ContainsKey('run-log.jsonl')) -Because '部署事实与运行记录要落盘'
    Assert-Match -Text $run.ReportFiles['run-log.jsonl'] -Pattern '"redisPasswordEnv":"MMORPG_CONTRACT_REDIS_PW"' -Because '运行记录只记变量名'
}

# ─────────────────────────────────────────────────────────────────
# dry-run 打印完整命令
# ─────────────────────────────────────────────────────────────────

Test-Case 'dry-run:第 0 步 f、1b、4a、4b、5b 的工具命令都完整打印,且都带 -zone / -topic-generation / -rollback-id;Step 5 为 -ToLatest' {
    $run = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-SkipMySqlPause', '-RollbackId', 'z101-dryrun', '-RedisHost', 'redis.infra',
            '-MySqlDsnEnv', 'MMORPG_ROLLBACK_MYSQL_DSN', '-BarrierProtocol', 'legacy', '-RollbackDiscardTool', 'C:/tools/zone_rollback_discard.exe'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dry-run 不该失败。输出: $($run.Output)"
    foreach ($mode in @('preflight', 'stopped', 'inventory', 'purge', 'verify')) {
        $lines = @([regex]::Matches($run.Output, "(?m)^[^\r\n]*C:/tools/zone_rollback_discard\.exe [^\r\n]*-mode $mode(?=\s|$)[^\r\n]*") | ForEach-Object { $_.Value })
        Assert-True -Condition ($lines.Count -ge 1) -Because "dry-run 要打印 $mode 的完整命令。输出: $($run.Output)"
        foreach ($pattern in @('-zone 101(\s|$)', "-topic-generation $DbTopicGeneration(\s|$)", '-rollback-id z101-dryrun(\s|$)', '-redis redis\.infra:6379(\s|$)', '-mysql-dsn-env MMORPG_ROLLBACK_MYSQL_DSN(\s|$)',
                "-expect-run-id '<redis run_id read in step 0 f>'")) {
            Assert-Match -Text $lines[0] -Pattern $pattern -Because "$mode 的命令要带 ${pattern}:$($lines[0])"
        }
    }
    Assert-Match -Text $run.Output -Pattern '-mode verify -target-time 2026-05-15T14:23:00Z -fingerprint-baseline [^\r\n]*-kafka-cut ''<Step 5 cut p=offset,\.\.\.>'' -purge-report ' -Because 'dry-run 没有切点,打印占位符;verify 带同一组判定参数与 purge.json'
    Assert-Match -Text $run.Output -Pattern 'Redis identity \(read-only\): kubectl exec -n [A-Za-z0-9.-]+ pod/redis-0 -c redis -- ' -Because 'dry-run 要预告第 0 步 f 读 run_id 的 exec(回档复核 [4])'
    Assert-Match -Text $run.Output -Pattern 'Discard tool build: C:/tools/zone_rollback_discard\.exe -version' -Because 'dry-run 要预告工具构建提交的核对(回档复核 [5])'
    $step5 = @([regex]::Matches($run.Output, '(?m)^[ \t]*kafka_offset_reset\.ps1 [^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $step5.Count -Because "应当恰有一行 Step 5 参数。输出: $($run.Output)"
    Assert-Match -Text $step5[0] -Pattern '-ToLatest True' -Because 'Step 5 拨到日志末端'
    Assert-NotMatch -Text $step5[0] -Pattern 'ToDatetime' -Because 'Step 5 不再拨回 TargetTime'
    foreach ($text in @('Step 1b: post-stop checks', 'Step 4a: inventory', 'Step 4b: discard post-TargetTime Redis state', 'Step 5b: verify nothing was written after the Step 5 cut',
            'Restore ONLY zone_101_db', 'Do NOT roll back global databases')) {
        Assert-Match -Text $run.Output -Pattern ([regex]::Escape($text)) -Because "dry-run 要预告:$text"
    }
    $order = @('Step 1: k8s-zone-down', 'Step 1b:', 'Step 3: MySQL', 'Step 4a:', 'Step 5: Kafka offsets', 'Step 4b:', 'Step 5b:', 'Step 6: k8s-zone-up') | ForEach-Object { $run.Output.IndexOf($_) }
    for ($i = 1; $i -lt $order.Count; $i++) {
        Assert-True -Condition ($order[$i - 1] -ge 0 -and $order[$i - 1] -lt $order[$i]) -Because "dry-run 的步骤顺序必须是 1 → 1b → 3 → 4a → 5 → 4b → 5b → 6(第 $i 项错位)"
    }
    Assert-Match -Text $run.Output -Pattern ([regex]::Escape("PlayerAllData:{<pid>}")) -Because 'Step 7 的父快照键名是 PlayerAllData:{<pid>}(玩家域 hash tag)'
    Assert-NotMatch -Text $run.Output -Pattern 'player:\{<known-pid>\}:player_database' -Because '旧的错误键名不再出现'
}

# 展示命令也走真实 dry-run：空参数不能占一个数组元素，含空格的集群身份必须能按原参数边界照抄。
$KubectlDisplayCases = @(
    @{ Label = '空 context 与 kubeconfig'; Args = @(); Prefix = 'kubectl ' },
    @{ Label = 'context 与 kubeconfig 都含空格'; Args = @('-KubeContext', 'contract context', '-KubeConfig', 'C:/contract kube/config file.yaml')
        Prefix = "kubectl --context 'contract context' --kubeconfig 'C:/contract kube/config file.yaml' " }
)
foreach ($displayCase in $KubectlDisplayCases) {
    Test-Case "dry-run 展示命令:$($displayCase.Label)，参数完整且不泄漏数组类型" {
        $run = Invoke-ToolScript -ScriptName 'k8s_zone_rollback.ps1' -Arguments ($RollbackBaseArgs + @('-SkipMySqlPause', '-RollbackId', 'z101-display', '-RedisHost', 'redis.infra',
                '-MySqlDsnEnv', 'MMORPG_ROLLBACK_MYSQL_DSN', '-BarrierProtocol', 'legacy', '-RollbackDiscardTool', 'C:/tools/zone_rollback_discard.exe') + $displayCase.Args)
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "只打印计划，不执行集群操作。输出:$($run.Output)"
        Assert-Match -Text $run.Output -Pattern 'k8s-zone-rollback \(DRY-RUN\)' -Because '参数组合不能切到真实执行'
        Assert-NotMatch -Text $run.Output -Pattern 'System\.Object\[\]' -Because '每个参数都必须展开成可读命令，不能只打印数组类型'
        $identity = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^  Redis identity \(read-only\):' })
        $facts = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^  Deploy facts \(read-only, secrets are never read\):' })
        Assert-Equal -Expected 1 -Actual $identity.Count -Because 'Redis 实例身份只预告一次'
        Assert-Equal -Expected 1 -Actual $facts.Count -Because '部署事实只预告一次'
        Assert-Match -Text $identity[0] -Pattern ('^  Redis identity \(read-only\): ' + [regex]::Escape($displayCase.Prefix) + 'exec -n [A-Za-z0-9.-]+ pod/redis-0 -c redis -- ') -Because 'Redis 命令保留参数边界；空 base 不得多出空参数'
        $snapshotCommand = $displayCase.Prefix + 'get configmap/go-svc-db-config configmap/go-svc-data-service-config deployment/db deployment/login -n mmorpg-zone-contract-test -o json --ignore-not-found --request-timeout=15s'
        Assert-Match -Text $facts[0] -Pattern ([regex]::Escape($snapshotCommand)) -Because '部署事实的四个对象、namespace、只读 JSON 和截止参数必须按序完整打印'
        $namespaceCommand = $displayCase.Prefix + 'get namespace mmorpg-zone-contract-test -o name --ignore-not-found'
        Assert-Match -Text $run.Output -Pattern ([regex]::Escape('then: ' + $namespaceCommand + ' must print nothing')) -Because '停服后的 namespace 只读复核必须保留同一集群身份'
        Assert-Match -Text $run.Output -Pattern ([regex]::Escape('then, before Step 5 (read-only): ' + $namespaceCommand + ' must still print nothing')) -Because '切位点前的第二次 namespace 复核也必须保留同一集群身份'
        Assert-Equal -Expected 2 -Actual @([regex]::Matches($run.Output, [regex]::Escape($namespaceCommand))).Count -Because '两次 namespace 只读预告都完整打印'
        $kafkaFacts = @($run.Output -split "`r?`n" | Where-Object { $_ -match ('^    ' + [regex]::Escape($displayCase.Prefix) + 'exec -n [A-Za-z0-9.-]+ kafka-0 -c kafka -- /bin/sh -c ') })
        Assert-Equal -Expected 2 -Actual $kafkaFacts.Count -Because 'zone 组与旧组的 Kafka 事实查询都必须保留参数边界和显式集群身份'
        foreach ($group in @("${DbGroupBase}_z101", $DbGroupBase)) {
            Assert-Match -Text ($kafkaFacts -join "`n") -Pattern ([regex]::Escape('--describe --group ' + $group) + "(?=\s|'|$)") -Because 'Kafka 事实预告保留两个消费组的只读 describe 命令'
        }
        Assert-NotMatch -Text $facts[0] -Pattern '(?i)(?:get|/)secret(?:\s|/|$)' -Because '展示的部署事实不得扩大到读取 Secret'
    }
}

# ─────────────────────────────────────────────────────────────────
# 第 0 步 f:部署事实
# ─────────────────────────────────────────────────────────────────

Test-Case '部署快照的对象名与 k8s_deploy.ps1 的 $GoSvcCatalogue 一致' {
    $deployText = Get-Content -LiteralPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Raw
    Assert-Match -Text $deployText -Pattern 'db\s+= @\{ ConfigMap = "go-svc-db-config";[^\r\n]*ConfigFile = "db\.yaml"' -Because '回档读的 db ConfigMap 名与数据键必须是部署脚本生成的那一份'
    Assert-Match -Text $deployText -Pattern '"data-service"\s+= @\{ ConfigMap = "go-svc-data-service-config";[^\r\n]*ConfigFile = "data_service\.yaml"' -Because 'data-service 同上'
    $snapshotArgs = Get-DeploySnapshotKubectlArgs -Namespace 'mmorpg-zone-x'
    foreach ($name in @('configmap/go-svc-db-config', 'configmap/go-svc-data-service-config', 'deployment/db', 'deployment/login', '--ignore-not-found')) {
        Assert-True -Condition ($snapshotArgs -ccontains $name) -Because "部署快照要读 $name"
    }
    Assert-True -Condition (-not ($snapshotArgs -ccontains 'secret') -and -not (($snapshotArgs -join ' ') -match 'secret')) -Because '部署快照不读任何 Secret'
}

Test-Case 'ConvertFrom-ZoneDeploySnapshot:只取白名单字段,不读口令;namespace 已删返回 $null;读不全即 throw' {
    $reply = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '2' -PartitionCnt '4'
    $facts = ConvertFrom-ZoneDeploySnapshot -Json $reply.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101'
    Assert-Equal -Expected '101' -Actual $facts.ZoneId -Because 'ZoneId'
    Assert-Equal -Expected 'legacy' -Actual $facts.FencingMode -Because 'Fencing.Mode'
    Assert-Equal -Expected 'redis.mmorpg-infra.svc.cluster.local:6379|0' -Actual "$($facts.Redis.Hosts)|$($facts.Redis.DB)" -Because 'RedisClient 的实例与 DB'
    Assert-True -Condition ($null -eq $facts.OwnerEpochRedis -and $null -eq $facts.PlacementRedis) -Because '两段缺省 = 复用 RedisClient'
    Assert-Equal -Expected 'final|g||2|4' -Actual "$($facts.KafkaGroup.Shape)|$($facts.KafkaGroup.Group)|$($facts.KafkaGroup.HandoffFrom)|$($facts.KafkaTopicGeneration)|$($facts.KafkaPartitionCnt)" -Because 'db 消费组形状(不是 GroupID 原值)与 Kafka 两项'
    Assert-Equal -Expected 2 -Actual ([int]$facts.Schema) -Because 'KafkaGroupBase 换成 KafkaGroup,Schema 升到 2'
    Assert-Equal -Expected 'ghcr.io/luyuancpp/mmorpg-db:0123456789ab' -Actual $facts.DbImage -Because 'db 镜像'
    Assert-NotMatch -Text ($facts | ConvertTo-Json -Depth 6) -Pattern 'contract-live-secret-never-read|Password' -Because '口令一概不读'

    Assert-True -Condition ($null -eq (ConvertFrom-ZoneDeploySnapshot -Json '' -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101')) -Because 'namespace 已删:无输出返回 $null,由调用方改用存档'
    $noFencing = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '1' -PartitionCnt '4'
    $noFencing.Output = $noFencing.Output.Replace('\n  Fencing:\n    Mode: legacy', '')
    Assert-Equal -Expected 'dual' -Actual (ConvertFrom-ZoneDeploySnapshot -Json $noFencing.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101').FencingMode -Because 'Fencing 整段缺省 = dual(与 go/db EffectiveMode 同口径,存盘协议世代门禁)'
    $withPlacement = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '1' -PartitionCnt '4' -ExtraDbYaml @('Placement:', '  Redis:', '    Hosts: "10.0.0.9:6379"', '    DB: 3')
    $placementFacts = ConvertFrom-ZoneDeploySnapshot -Json $withPlacement.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101'
    Assert-Equal -Expected '10.0.0.9:6379|3' -Actual "$($placementFacts.PlacementRedis.Hosts)|$($placementFacts.PlacementRedis.DB)" -Because 'Placement.Redis 显式填了就记下来'

    $document = $reply.Output | ConvertFrom-Json
    $document.items = @($document.items | Where-Object { $_.metadata.name -ne 'go-svc-db-config' })
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-ZoneDeploySnapshot -Json ($document | ConvertTo-Json -Depth 30) -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101' }) -Pattern 'go-svc-db-config' -Because 'namespace 还在却缺 db ConfigMap,不猜'
    $document = $reply.Output | ConvertFrom-Json
    $document.items = @($document.items | Where-Object { $_.metadata.name -ne 'db' })
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-ZoneDeploySnapshot -Json ($document | ConvertTo-Json -Depth 30) -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101' }) -Pattern 'Deployment db' -Because '读不到 db 镜像就无法核对工具与线上同一提交'
}

Test-Case '[0] ConvertFrom-ZoneDeploySnapshot:db 消费组按 k8s_deploy.ps1 的四种形状归类 —— final(Z)、handoff(Z←L)、handoff(L←Z) 都认得,第三个组名记为 unrecognized(不 throw)' {
    $shape = {
        param([string[]]$Lines)
        $reply = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '1' -PartitionCnt '4' -GroupLines $Lines
        $group = (ConvertFrom-ZoneDeploySnapshot -Json $reply.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101').KafkaGroup
        return "$($group.Shape)|$($group.Group)|$($group.HandoffFrom)"
    }
    Assert-Equal -Expected 'final|g_z101|' -Actual (& $shape @('    GroupID: "g_z101"')) -Because 'final(Z):回档 Step 6 渲染的就是它,回档过一次的 zone 都是这个形状'
    Assert-Equal -Expected 'handoff|g_z101|g' -Actual (& $shape @('    ConsumerGroup: "g_z101"', '    GroupHandoffFrom: "g"')) -Because 'handoff(Z←L):交接形状不写 GroupID'
    Assert-Equal -Expected 'handoff|g|g_z101' -Actual (& $shape @('    ConsumerGroup: "g"', '    GroupHandoffFrom: "g_z101"')) -Because 'handoff(L←Z)'
    $reply = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '1' -PartitionCnt '4' -GroupLines @('    GroupID: "someone_else"')
    $odd = (ConvertFrom-ZoneDeploySnapshot -Json $reply.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101').KafkaGroup
    Assert-Equal -Expected 'unrecognized' -Actual $odd.Shape -Because '第三个组名归不进四种形状,记下来交给 Get-DeployFactsProblems 拒绝'
    Assert-Match -Text $odd.Detail -Pattern 'someone_else' -Because '要带上原因(lib ConvertFrom-LiveDbConsumerGroup 的原文)'
}

Test-Case 'Get-DeployFactsProblems:与本次参数、本机 db.yaml 一致才放行;Redis 拓扑不在同一实例同一 DB、Fencing 不是 legacy 都报问题' {
    $base = { param([string[]]$Extra = @(), [string]$Fencing = 'legacy', [string]$RedisDB = '0', [string]$Mapping = 'redis.mmorpg-infra.svc.cluster.local:6379', [string]$ZoneId = '101', [string]$Generation = '1')
        $reply = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration $Generation -PartitionCnt '4' -ZoneId $ZoneId -FencingMode $Fencing -RedisDB $RedisDB -MappingHost $Mapping -ExtraDbYaml $Extra
        return (ConvertFrom-ZoneDeploySnapshot -Json $reply.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101')
    }
    # Get-DeployFactsProblems 以 `, $数组` 返回;这里原样转交(不套 @(...),否则空表也成了 Count 1)。
    $check = { param($Facts) return , (Get-DeployFactsProblems -Facts $Facts -ZoneId 101 -RedisDB 0 -BarrierProtocol 'legacy' -LocalGroupBase 'g' -LocalTopicGeneration 1 -LocalPartitionCnt 4) }
    Assert-Equal -Expected 0 -Actual (& $check (& $base)).Count -Because '一致时没有问题'
    $cases = @(
        @{ Label = 'Fencing dual'; Facts = (& $base -Fencing 'dual'); Pattern = '裁决 #3' },
        @{ Label = 'RedisClient.DB 不同'; Facts = (& $base -RedisDB '2'); Pattern = '不是 -RedisDB 0' },
        @{ Label = 'OwnerEpochRedis 另一个实例'; Facts = (& $base -Extra @('  OwnerEpochRedis:', '    Hosts: "10.0.0.8:6379"', '    DB: 0')); Pattern = 'ServerConfig\.OwnerEpochRedis' },
        @{ Label = 'Placement.Redis 另一个 DB'; Facts = (& $base -Extra @('Placement:', '  Redis:', '    Hosts: "redis.mmorpg-infra.svc.cluster.local:6379"', '    DB: 1')); Pattern = 'Placement\.Redis' },
        @{ Label = 'MappingRedis 另一个实例'; Facts = (& $base -Mapping '10.0.0.7:6379'); Pattern = 'MappingRedis' },
        @{ Label = 'ZoneId 不同'; Facts = (& $base -ZoneId '102'); Pattern = "ZoneId 是 '102'" },
        @{ Label = 'TopicGeneration 不同'; Facts = (& $base -Generation '2'); Pattern = 'ServerConfig\.Kafka\.TopicGeneration' }
    )
    foreach ($c in $cases) {
        $problems = & $check $c.Facts
        Assert-True -Condition (@($problems | Where-Object { $_ -match $c.Pattern }).Count -ge 1) -Because "$($c.Label):必须报问题(实际: $($problems -join '; '))"
    }
    $placementSame = & $base -Extra @('Placement:', '  Redis:', '    Hosts: "redis.mmorpg-infra.svc.cluster.local:6379"', '    DB: 0')
    Assert-Equal -Expected 0 -Actual (& $check $placementSame).Count -Because 'Placement.Redis 显式写成同一实例同一 DB 时放行'
}

Test-Case '[0] Get-DeployFactsProblems:db 消费组 final(L) / final(Z) / handoff(Z←L) / handoff(L←Z) 都放行,第三个组名拒绝且不再建议改 db.yaml' {
    $factsWith = {
        param([string[]]$Lines)
        $reply = New-RollbackDeploySnapshotReply -GroupBase 'g' -TopicGeneration '1' -PartitionCnt '4' -GroupLines $Lines
        return (ConvertFrom-ZoneDeploySnapshot -Json $reply.Output -Namespace 'ns' -LegacyGroup 'g' -ZoneGroup 'g_z101')
    }
    $check = { param($Facts) return , (Get-DeployFactsProblems -Facts $Facts -ZoneId 101 -RedisDB 0 -BarrierProtocol 'legacy' -LocalGroupBase 'g' -LocalTopicGeneration 1 -LocalPartitionCnt 4) }
    foreach ($c in @(
            @{ Label = 'final(L)'; Lines = @('    GroupID: "g"') },
            @{ Label = 'final(Z)'; Lines = @('    GroupID: "g_z101"') },
            @{ Label = 'handoff(Z←L)'; Lines = @('    ConsumerGroup: "g_z101"', '    GroupHandoffFrom: "g"') },
            @{ Label = 'handoff(L←Z)'; Lines = @('    ConsumerGroup: "g"', '    GroupHandoffFrom: "g_z101"') })) {
        $problems = & $check (& $factsWith $c.Lines)
        Assert-Equal -Expected 0 -Actual $problems.Count -Because "$($c.Label) 是 k8s_deploy.ps1 生成的合法形状,必须放行(实际: $($problems -join '; '))"
    }
    $third = & $check (& $factsWith @('    GroupID: "someone_else"'))
    Assert-True -Condition (@($third | Where-Object { $_ -match '不属于 \{g, g_z101\}' }).Count -eq 1) -Because "第三个组名必须拒绝,文案写明线上形状不属于 {L, Z}(实际: $($third -join '; '))"
    Assert-NotMatch -Text ($third -join "`n") -Pattern '对齐|db\.yaml 对齐' -Because '不再建议把工作树的 db.yaml 改成线上值(照做会推导出 L_z101_z101 这类错位组名)'

    # 存档经 JSON 往返后不信存档里的结论:形状写着 final、组名却不属于 {L, Z}(本机 db.yaml 的基名换过)同样拒绝。
    $roundTrip = (& $factsWith @('    GroupID: "g_z101"')) | ConvertTo-Json -Depth 6 | ConvertFrom-Json
    $mismatch = Get-DeployFactsProblems -Facts $roundTrip -ZoneId 101 -RedisDB 0 -BarrierProtocol 'legacy' -LocalGroupBase 'other' -LocalTopicGeneration 1 -LocalPartitionCnt 4
    Assert-True -Condition (@($mismatch | Where-Object { $_ -match '最终形状 GroupID=g_z101' }).Count -eq 1) -Because "本机基名换成 other 时 g_z101 不属于 {other, other_z101}(实际: $($mismatch -join '; '))"
    $zoneMismatch = Get-DeployFactsProblems -Facts $roundTrip -ZoneId 102 -RedisDB 0 -BarrierProtocol 'legacy' -LocalGroupBase 'g' -LocalTopicGeneration 1 -LocalPartitionCnt 4
    Assert-True -Condition (@($zoneMismatch | Where-Object { $_ -match '不属于 \{g, g_z102\}' }).Count -eq 1) -Because 'Z 按 -ZoneId 推导'
}

Test-Case '[0] 第 0 步 f(-Apply 沙箱):线上已是 final(Z) 或交接形状时放行(以前一律拒绝);第三个组名在停服之前拒绝' {
    foreach ($c in @(
            @{ Label = 'final(Z)'; Lines = @("    GroupID: `"${DbGroupBase}_z101`"") },
            @{ Label = 'handoff(Z←L)'; Lines = @("    ConsumerGroup: `"${DbGroupBase}_z101`"", "    GroupHandoffFrom: `"$DbGroupBase`"") },
            @{ Label = 'handoff(L←Z)'; Lines = @("    ConsumerGroup: `"$DbGroupBase`"", "    GroupHandoffFrom: `"${DbGroupBase}_z101`"") })) {
        $reply = New-RollbackDeploySnapshotReply -GroupBase $DbGroupBase -TopicGeneration $DbTopicGeneration -PartitionCnt $DbPartitionCnt -GroupLines $c.Lines
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -SnapshotReply $reply
        Assert-True -Condition ($null -eq $run.Error) -Because "$($c.Label):k8s_deploy.ps1 生成的合法形状,第 0 步 f 不该拒绝。错误: $($run.Error)"
    }
    $reply = New-RollbackDeploySnapshotReply -GroupBase $DbGroupBase -TopicGeneration $DbTopicGeneration -PartitionCnt $DbPartitionCnt -GroupLines @('    GroupID: "someone_else"')
    $third = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -SnapshotReply $reply
    Assert-Match -Text $third.Error -Pattern '在停服之前拒绝执行[^\r\n]*部署事实' -Because '第三个组名在停服之前拒绝'
    Assert-Match -Text $third.Error -Pattern '消费组形状不属于' -Because '文案写明线上形状不属于 {L, Z}'
    Assert-Equal -Expected 0 -Actual $third.DiscardCalls.Count -Because '事实不一致时不调工具'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($third.DevToolsCalls -join '|') -Because '没有停服'
}

# ─────────────────────────────────────────────────────────────────
# 回档复核 [4]:Redis 实例身份(run_id);[5]:回档工具与线上 go/db 同一提交
# ─────────────────────────────────────────────────────────────────

Test-Case '[4] Get-GoDbRedisRunIdExecArgs / ConvertFrom-RedisInfoRunId:exec 进 infra 的 redis-0、对 go/db 的地址读 INFO server,口令取 Pod 自己的环境变量;run_id 恰好一行' {
    $argv = Get-GoDbRedisRunIdExecArgs -InfraNamespace 'mmorpg-infra' -RedisHosts 'redis.mmorpg-infra.svc.cluster.local:6379'
    Assert-Equal -Expected 'exec|-n|mmorpg-infra|pod/redis-0|-c|redis|--|sh|-c' -Actual (($argv | Select-Object -First 9) -join '|') -Because '与 k8s_deploy.ps1 Test-RedisLiveMaxClients 同一个 Pod 与容器'
    Assert-Match -Text $argv[9] -Pattern 'REDISCLI_AUTH=\$REDIS_PASSWORD' -Because '口令取 Pod 自己的 REDIS_PASSWORD,不上本机命令行'
    Assert-Match -Text $argv[9] -Pattern 'redis-cli --raw -h redis\.mmorpg-infra\.svc\.cluster\.local -p 6379 INFO server$' -Because '对 go/db 自己连的地址读,HA 下经代理落到当前主库'
    foreach ($bad in @('10.0.0.1:6379,10.0.0.2:6379', 'host;rm -rf /:6379', 'redis', '[::1]:6379')) {
        Assert-True -Condition ($null -ne (Get-ThrownMessage { Get-GoDbRedisRunIdExecArgs -InfraNamespace 'ns' -RedisHosts $bad })) -Because "'$bad' 不能拼进 sh -c,必须拒绝"
    }
    $runId = 'aa11bb22cc33dd44ee55ff6677889900aabbccdd'
    Assert-Equal -Expected $runId -Actual (ConvertFrom-RedisInfoRunId -Text "# Server`r`nredis_version:7.2.4`r`nrun_id:$runId`r`ntcp_port:6379") -Because 'CRLF 输出也认'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-RedisInfoRunId -Text 'NOAUTH Authentication required.' })) -Because '读不到 run_id 必须 throw'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-RedisInfoRunId -Text "run_id:$runId`nrun_id:$runId" })) -Because '两行 run_id 不猜'
}

Test-Case '[4] 第 0 步 f(-Apply 沙箱):每次工具调用都带 -expect-run-id;run_id 不一致(另一个环境的 Redis)或读不到,都在停服之前拒绝' {
    $ok = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs
    Assert-True -Condition ($null -eq $ok.Error) -Because "run_id 一致时不该失败。错误: $($ok.Error)"
    Assert-Equal -Expected 1 -Actual $ok.RedisExecCalls.Count -Because '读一次 run_id'
    Assert-Match -Text $ok.RedisExecCalls[0] -Pattern 'exec -n contract-infra pod/redis-0 -c redis -- sh -c ' -Because 'infra namespace 与 Step 6 同一来源(dev_tools.ps1 的默认值)'
    foreach ($line in $ok.DiscardCalls) {
        Assert-Match -Text $line -Pattern "-expect-run-id $([regex]::Escape($script:RollbackSandboxRedisRunId))(\s|$)" -Because "每次工具调用都要带 -expect-run-id:$line"
    }

    $other = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -ToolRedisRunId 'ffffffffffffffffffffffffffffffffffffffff'
    Assert-Match -Text $other.Error -Pattern '在停服之前拒绝执行[^\r\n]*preflight 以退出码 3' -Because '-RedisHost 指向另一个实例时工具 preflight 以 3 拒绝,停服之前就停'
    Assert-Match -Text $other.Error -Pattern 'run_id 不一致' -Because '错误文本要提示 run_id 这一种原因'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($other.DevToolsCalls -join '|') -Because '没有停服'

    $forbidden = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -RedisInfoReply @{ ExitCode = 1; Output = 'Error from server (Forbidden): pods "redis-0" is forbidden' }
    Assert-Match -Text $forbidden.Error -Pattern '读不到 go/db 所用 Redis[^\r\n]*的 run_id' -Because 'exec 失败即拒绝,不当成「没法核对就放行」'
    Assert-Equal -Expected 0 -Actual $forbidden.DiscardCalls.Count -Because '读不到 run_id 时不调工具'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($forbidden.DevToolsCalls -join '|') -Because '没有停服'

    $noAuth = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -RedisInfoReply @{ ExitCode = 0; Output = 'NOAUTH Authentication required.' }
    Assert-Match -Text $noAuth.Error -Pattern 'run_id 行有 0 行' -Because '输出里没有 run_id 同样拒绝'
}

Test-Case '[5] ConvertFrom-DiscardToolVersion / Test-DiscardToolMatchesDbImage:提交以镜像 tag 开头才算同一提交;digest、非 sha tag、脏树构建一律无法核对' {
    $commit = '0123456789ab' + ('0' * 28)
    $version = ConvertFrom-DiscardToolVersion -Lines @("commit=$commit", 'dirty=false')
    Assert-Equal -Expected "$commit|False" -Actual "$($version.Commit)|$($version.Dirty)" -Because '契约格式:一行 commit、一行 dirty'
    foreach ($bad in @(@('commit=0123'), @("commit=$commit"), @("commit=$commit", 'dirty=maybe'), @("commit=$commit", "commit=$commit", 'dirty=false'))) {
        Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-DiscardToolVersion -Lines $bad })) -Because "不是约定格式必须 throw:$($bad -join ' | ')"
    }
    Assert-True -Condition ($null -eq (Test-DiscardToolMatchesDbImage -Version $version -DbImage 'ghcr.io/luyuancpp/mmorpg-db:0123456789ab')) -Because '12 位 tag 是 commit 的前缀:一致'
    Assert-True -Condition ($null -eq (Test-DiscardToolMatchesDbImage -Version $version -DbImage 'registry:5000/mmorpg-db:0123456789AB')) -Because '带端口的仓库地址、大写 tag 也认'
    $cases = @(
        @{ Label = '另一个提交'; Image = 'ghcr.io/luyuancpp/mmorpg-db:fedcba987654'; Pattern = '不是同一提交' },
        @{ Label = 'digest'; Image = 'ghcr.io/luyuancpp/mmorpg-db@sha256:' + ('a' * 64); Pattern = 'digest' },
        @{ Label = '脏树镜像'; Image = 'ghcr.io/luyuancpp/mmorpg-db:0123456789ab-dirty'; Pattern = '不是 git sha' },
        @{ Label = '可变 tag'; Image = 'ghcr.io/luyuancpp/mmorpg-db:latest'; Pattern = '不是 git sha' }
    )
    foreach ($c in $cases) {
        Assert-Match -Text ([string](Test-DiscardToolMatchesDbImage -Version $version -DbImage $c.Image)) -Pattern $c.Pattern -Because "$($c.Label):必须报问题"
    }
    $dirtyTool = ConvertFrom-DiscardToolVersion -Lines @("commit=$commit", 'dirty=true')
    Assert-Match -Text ([string](Test-DiscardToolMatchesDbImage -Version $dirtyTool -DbImage 'ghcr.io/luyuancpp/mmorpg-db:0123456789ab')) -Pattern '脏树构建' -Because '工具自己是脏树构建,提交号代表不了它的代码'
}

Test-Case '[5] 第 0 步 f(-Apply 沙箱):工具 -version 与线上 db 镜像同一提交才放行;不一致、脏树、-version 失败都在停服之前拒绝' {
    $ok = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs
    Assert-True -Condition ($null -eq $ok.Error) -Because "同一提交时不该失败。错误: $($ok.Error)"
    Assert-Equal -Expected 1 -Actual $ok.VersionCalls.Count -Because '核对一次构建提交'
    Assert-True -Condition ((Get-TimelineIndex -Timeline $ok.Timeline -Event 'tool:version') -lt (Get-TimelineIndex -Timeline $ok.Timeline -Event 'tool:preflight')) -Because '先核对提交,再跑 preflight'
    Assert-Match -Text $ok.ReportFiles['run-log.jsonl'] -Pattern ('"discardToolCommit":"' + [regex]::Escape($script:RollbackSandboxToolCommit) + '"') -Because '运行记录要写下工具的构建提交'

    $cases = @(
        @{ Label = '另一个提交'; Args = @{ ToolVersionLines = @("commit=fedcba987654$('0' * 28)", 'dirty=false') }; Pattern = '不是同一提交' },
        @{ Label = '脏树构建'; Args = @{ ToolVersionLines = @("commit=$($script:RollbackSandboxToolCommit)", 'dirty=true') }; Pattern = '脏树构建' },
        @{ Label = '-version 退出码非 0'; Args = @{ ToolVersionLines = @('flag provided but not defined: -version'); ToolVersionExit = 2 }; Pattern = '读不到回档工具的构建提交' },
        @{ Label = '-version 输出不合约定'; Args = @{ ToolVersionLines = @('zone_rollback_discard dev') }; Pattern = '不是约定格式' },
        @{ Label = 'db 镜像以 digest 部署'; Args = @{ SnapshotReply = (New-RollbackDeploySnapshotReply -GroupBase $DbGroupBase -TopicGeneration $DbTopicGeneration -PartitionCnt $DbPartitionCnt -DbImage ('ghcr.io/luyuancpp/mmorpg-db@sha256:' + ('b' * 64))) }; Pattern = 'digest' }
    )
    foreach ($c in $cases) {
        $sandboxArgs = @{ Apply = $true; DeployDefault = '1'; ExtraArgs = $QuietEntryArgs } + $c.Args
        $run = Invoke-RollbackInSandbox @sandboxArgs
        Assert-Match -Text $run.Error -Pattern '在停服之前拒绝执行' -Because "$($c.Label):必须在停服之前拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Error -Pattern $c.Pattern -Because "$($c.Label):错误文本要带上原因"
        Assert-Equal -Expected 0 -Actual $run.DiscardCalls.Count -Because "$($c.Label):提交对不上就不跑工具的任何模式"
        Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($run.DevToolsCalls -join '|') -Because "$($c.Label):没有停服"
    }
}

Test-Case '第 0 步 f(-Apply 沙箱):部署事实不一致或读不到,都在停服之前拒绝' {
    $dual = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs `
        -SnapshotReply (New-RollbackDeploySnapshotReply -GroupBase $DbGroupBase -TopicGeneration $DbTopicGeneration -PartitionCnt $DbPartitionCnt -FencingMode 'dual')
    Assert-True -Condition ($null -ne $dual.Error) -Because 'Fencing.Mode=dual 与 -BarrierProtocol legacy 矛盾,必须拒绝'
    Assert-Match -Text $dual.Error -Pattern '在停服之前拒绝执行[^\r\n]*部署事实' -Because '要点明是部署事实不一致'
    Assert-Equal -Expected 0 -Actual $dual.DiscardCalls.Count -Because '事实不一致时不调工具'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($dual.DevToolsCalls -join '|') -Because '没有停服'

    $forbidden = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $QuietEntryArgs -SnapshotReply @{ ExitCode = 1; Output = 'Error from server (Forbidden): configmaps is forbidden' }
    Assert-Match -Text $forbidden.Error -Pattern '读不到 namespace mmorpg-zone-contract-test 的部署事实' -Because '读不到不等于没有'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($forbidden.DevToolsCalls -join '|') -Because '没有停服'
}

Test-Case '第 0 步 f(-Apply 沙箱):namespace 已删时复用同一 rid 的存档;没有存档或存档与现读不同都拒绝' {
    $first = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RollbackId = 'z101-reuse' } + $QuietEntryArgs)
    Assert-True -Condition ($null -eq $first.Error) -Because "首次运行不该失败。错误: $($first.Error)"
    $saved = $first.ReportFiles['deploy-facts.json']
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($saved)) -Because '首次运行要把部署事实落盘'

    $gone = @{ ExitCode = 0; Output = '' }
    $reused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RollbackId = 'z101-reuse' } + $QuietEntryArgs) -SnapshotReply $gone -SeedReportFiles @{ 'deploy-facts.json' = $saved }
    Assert-True -Condition ($null -eq $reused.Error) -Because "同一 rid 重跑、namespace 已删时复用存档。错误: $($reused.Error)"
    Assert-Match -Text $reused.Output -Pattern 'reusing deploy facts saved by an earlier run' -Because '要写明复用了存档'

    $identical = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RollbackId = 'z101-reuse' } + $QuietEntryArgs) -SeedReportFiles @{ 'deploy-facts.json' = $saved }
    Assert-True -Condition ($null -eq $identical.Error) -Because "namespace 还在、现读与存档相同(经过一次 JSON 往返)时放行。错误: $($identical.Error)"

    $noSaved = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RollbackId = 'z101-reuse' } + $QuietEntryArgs) -SnapshotReply $gone
    Assert-Match -Text $noSaved.Error -Pattern '必须带上一次打印的 -RollbackId' -Because 'namespace 已删又没有存档,无从核对'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($noSaved.DevToolsCalls -join '|') -Because '拒绝在停服之前'

    $changed = ($saved | ConvertFrom-Json)
    $changed.DbImage = 'ghcr.io/luyuancpp/mmorpg-db:fedcba987654'
    $drift = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs (@{ RollbackId = 'z101-reuse' } + $QuietEntryArgs) -SeedReportFiles @{ 'deploy-facts.json' = ($changed | ConvertTo-Json -Depth 6) }
    Assert-Match -Text $drift.Error -Pattern '与同一 rollback-id 存档' -Because '部署在两次运行之间变了,或 rid 用错了'
    Assert-Equal -Expected 0 -Actual $drift.DiscardCalls.Count -Because '不调工具'
}

Test-Case 'Get-DiscardToolArgs:公共参数顺序固定、空值不输出、-mode 之后才是模式参数;Format-CommandLine 给含空白的参数加引号' {
    $common = [ordered]@{ 'deploy-facts' = 'f.json'; 'zone' = '101'; 'redis-password-env' = ''; 'rollback-id' = 'r1' }
    $argv = Get-DiscardToolArgs -Mode 'purge' -Common $common -Extra @('-confirm')
    Assert-Equal -Expected '-deploy-facts|f.json|-zone|101|-rollback-id|r1|-mode|purge|-confirm' -Actual ($argv -join '|') -Because '空值(没开认证的口令变量)不输出,模式参数在 -mode 之后'
    Assert-Equal -Expected "tool -x 'a b' ''" -Actual (Format-CommandLine -Program 'tool' -Arguments @('-x', 'a b', '')) -Because '打印时含空白或为空的参数加单引号'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { Get-DiscardToolArgs -Mode 'flush' -Common $common })) -Because '契约之外的模式直接拒绝'
}

exit (Complete-TestRun -SuiteName 'k8s_zone_rollback 回档丢弃语义')
