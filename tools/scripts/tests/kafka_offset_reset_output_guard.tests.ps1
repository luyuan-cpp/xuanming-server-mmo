#requires -Version 7
<#
.SYNOPSIS
    kafka_offset_reset.ps1 位点重置结果判定的契约测试(2026-10-12)。

.DESCRIPTION
    背景:对仍有活跃成员的消费组执行 --reset-offsets 时,kafka-consumer-groups.sh 只打印
    "Error: Assignments can only be reset if the group ... is inactive ..." 而退出码可能仍为 0,位点没动,旧脚本照样打印 OK。

    本套钉住:
      - 退出码非 0:失败;
      - 退出码 0 但任一行以 "Error:" 开头(允许前导空白):失败,错误文本带上原始错误行;
      - 正常的计划表 / 执行结果输出:放行;
      - 行中间出现 error 字样(不是行首的 "Error:"):放行;
      - 脚本在调用 kafka-consumer-groups 之后确实走这道判定,且 stderr 一并收集。

    另钉住:
      - lib/kafka_consumer_groups.ps1 的解析:describe 按表头列名定位、只取本 topic、"-" 记 null;
        「组不存在」白名单(does not exist / GroupIdNotFoundException / GROUP_ID_NOT_FOUND)、无表头 + has no active members
        判为 no-offsets、Error 行与无法解析的输出一律 throw;白名单与表头矛盾时 throw;重置计划表的解析;
      - 重置计划覆盖检查与 -Apply 后逐分区复核;
      - 重建要显式 -ReplicationFactor、topic describe 摘要解析、删除后等待(--list 失败 / 超时即 throw);
        重置分支以 exit 0 结束、Invoke-KafkaToolCaptured 复位 LASTEXITCODE;
      - 已删除的 -RetireGroup / -FinalizeHandoff 不再是参数(2026-10-30:玩家存盘不再经 Kafka)。

    判定函数按 AST 取出执行,Kafka CLI 用本文件里的假函数顶替,不连任何 Kafka。
    「组不存在」等文案是按版本行为推定的;钉版 apache/kafka:4.3.1 的原文由 Codex 实测后(设计文档 §11 V3)追加为 fixture。

.EXAMPLE
    pwsh -File tools/scripts/tests/kafka_offset_reset_output_guard.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"

Write-Host ""
Write-Host "=== kafka_offset_reset.ps1 offset reset result contract tests ==="

$scriptPath = Join-Path (Split-Path $PSScriptRoot -Parent) 'kafka_offset_reset.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($scriptPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw "kafka_offset_reset.ps1 解析失败: $($parseErrors | Out-String)" }
$guard = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Assert-KafkaToolOutputClean' }, $true))
if ($guard.Count -ne 1) { throw "kafka_offset_reset.ps1 里应有且只有一个 Assert-KafkaToolOutputClean,实际 $($guard.Count) 个" }
. ([scriptblock]::Create($guard[0].Extent.Text))

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Body)
    try { & $Body; return $null } catch { return $_.Exception.Message }
}

$activeGroupError = "Error: Assignments can only be reset if the group 'data_service-transaction-log' is inactive, but the current state is Stable."

Test-Case '退出码 0 但输出 Error:(活跃组不能重置):失败,并带上原始错误行' {
    $lines = @('', $activeGroupError, '')
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines $lines -ExitCode 0 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -ne $message) -Because '位点没动却放行,就是这次要修的静默失败'
    Assert-Match -Text $message -Pattern '位点没有重置' -Because '错误文本要说明位点没有重置'
    Assert-Match -Text $message -Pattern 'current state is Stable' -Because '要带上 Kafka 原始错误,方便定位'
}

Test-Case '"Error:" 前有空白也算失败' {
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines @("   $activeGroupError") -ExitCode 0 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -ne $message) -Because 'Kafka 工具常在错误行前打空行或缩进'
}

Test-Case '退出码非 0:失败' {
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines @() -ExitCode 1 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -ne $message) -Because '退出码非 0 必须失败'
    Assert-Match -Text $message -Pattern 'exit code 1' -Because '错误文本要带上退出码'
}

Test-Case '正常的计划表输出:放行' {
    $lines = @(
        '',
        'GROUP                          TOPIC                          PARTITION  NEW-OFFSET',
        'data_service-transaction-log   transaction_log_topic_g1       0          12345',
        'data_service-transaction-log   transaction_log_topic_g1       1          23456'
    )
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines $lines -ExitCode 0 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -eq $message) -Because "正常输出不应被拒。错误: $message"
}

Test-Case '没有任何输出且退出码 0:放行' {
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines @() -ExitCode 0 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -eq $message) -Because "空输出不是错误。错误: $message"
}

Test-Case '行中间出现 error 字样:放行(只认行首的 Error:)' {
    $lines = @('[2026-10-12 03:00:00,000] WARN [AdminClient] Connection error, retrying (org.apache.kafka.clients.NetworkClient)')
    $message = Get-ThrownMessage { Assert-KafkaToolOutputClean -Lines $lines -ExitCode 0 -Tool 'kafka-consumer-groups' }
    Assert-True -Condition ($null -eq $message) -Because "重试类告警不代表重置失败,不应误拒。错误: $message"
}

Test-Case '脚本调用 kafka-consumer-groups 后走这道判定,且收集 stderr' {
    $text = Get-Content $scriptPath -Raw
    Assert-Match -Text $text -Pattern '& \$cgTool @cmdArgs 2>&1' -Because 'Error: 行可能走 stderr,必须一并收集'
    Assert-Match -Text $text -Pattern 'Assert-KafkaToolOutputClean -Lines \$cgOutput -ExitCode \$exitCode' -Because '调用之后必须判定输出'
}

# ─────────────────────────────────────────────────────────────────
# lib/kafka_consumer_groups.ps1 的解析与重置复核(2026-10-15)
# ─────────────────────────────────────────────────────────────────

. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'lib' 'kafka_consumer_groups.ps1')

$resetFunctions = @('Invoke-KafkaToolCaptured', 'Invoke-KafkaConsumerGroupDescribe', 'Get-ResetPlanProblems', 'Get-ResetVerificationProblems')
$found = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $resetFunctions }, $true))
foreach ($name in $resetFunctions) {
    $matched = @($found | Where-Object { $_.Name -eq $name })
    if ($matched.Count -ne 1) { throw "kafka_offset_reset.ps1 里应有且只有一个 $name,实际 $($matched.Count) 个" }
    . ([scriptblock]::Create($matched[0].Extent.Text))
}

$Topic = 'transaction_log_topic_g1'
$DescribeHeader = 'GROUP                      TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG   CONSUMER-ID  HOST  CLIENT-ID'

# 拼一行 describe 表格行(列顺序同 $DescribeHeader)。
function New-DescribeRow {
    param([string]$Group = 'g', [string]$RowTopic = $Topic, [int]$Partition, [string]$Current, [string]$End, [string]$Lag)
    return "$Group  $RowTopic  $Partition  $Current  $End  $Lag  -  -  -"
}

Test-Case 'describe 解析:按表头列名定位、只取本 topic、"-" 记为 null' {
    $lines = @(
        "Consumer group 'g' has no active members.",
        '',
        $DescribeHeader,
        (New-DescribeRow -Partition 0 -Current '10' -End '15' -Lag '5'),
        (New-DescribeRow -Partition 1 -Current '-' -End '7' -Lag '-'),
        (New-DescribeRow -RowTopic 'player_snapshot_topic_g1' -Partition 0 -Current '99' -End '100' -Lag '1')
    )
    $parsed = ConvertFrom-KafkaConsumerGroupDescribe -Lines $lines -Topic $Topic
    Assert-True -Condition $parsed.HeaderFound -Because '"has no active members" 行之后的表头要能识别'
    Assert-True -Condition (-not $parsed.GroupMissing) -Because 'has no active members 不是组不存在'
    Assert-Equal -Expected 2 -Actual @($parsed.Rows).Count -Because '别的 topic 的行不计入'
    $p1 = @($parsed.Rows | Where-Object { $_.Partition -eq 1 })[0]
    Assert-True -Condition ($null -eq $p1.CurrentOffset -and $null -eq $p1.Lag) -Because '"-" 表示没有已提交位点,必须记为 null,不能当成 0'
    $map = Get-KafkaCommittedOffsetMap -Parsed $parsed
    Assert-Equal -Expected 1 -Actual $map.Count -Because '只有数值 CURRENT-OFFSET 的分区算已提交'
    Assert-Equal -Expected 10 -Actual $map[0] -Because '分区 0 的已提交位点'
}

Test-Case 'describe 解析:列顺序打乱也按表头识别' {
    $lines = @(
        'LAG  PARTITION  TOPIC  GROUP  LOG-END-OFFSET  CURRENT-OFFSET',
        "3  4  $Topic  g  50  47"
    )
    $parsed = ConvertFrom-KafkaConsumerGroupDescribe -Lines $lines -Topic $Topic
    $row = @($parsed.Rows)[0]
    Assert-Equal -Expected 4 -Actual $row.Partition -Because '分区按 PARTITION 列取,不按下标'
    Assert-Equal -Expected 47 -Actual $row.CurrentOffset -Because 'CURRENT-OFFSET 按列名取'
    Assert-Equal -Expected 50 -Actual $row.LogEndOffset -Because 'LOG-END-OFFSET 按列名取'
    Assert-Equal -Expected 3 -Actual $row.Lag -Because 'LAG 按列名取'
}

Test-Case 'describe 解析:"Error: Consumer group ''x'' does not exist." 判为 GroupMissing' {
    $parsed = ConvertFrom-KafkaConsumerGroupDescribe -Lines @('', "Error: Consumer group 'data_service-transaction-log' does not exist.") -Topic $Topic
    Assert-True -Condition $parsed.GroupMissing -Because 'Kafka 3.x 对不存在的组打印这一行'
    Assert-True -Condition (-not $parsed.HeaderFound) -Because '没有表头'
}

Test-Case 'Resolve-KafkaGroupTopicOffsets:组不存在的两种文案都判 missing(不论退出码)' {
    $cases = @(
        @{ Lines = @("Error: Consumer group 'g' does not exist."); Exit = 0 },
        @{ Lines = @('Error: Executing consumer group command failed due to org.apache.kafka.common.errors.GroupIdNotFoundException: Group g not found.'); Exit = 1 },
        @{ Lines = @('Error: GROUP_ID_NOT_FOUND'); Exit = 1 }
    )
    foreach ($c in $cases) {
        $state = Resolve-KafkaGroupTopicOffsets -Lines $c.Lines -ExitCode $c.Exit -Topic $Topic
        Assert-Equal -Expected 'missing' -Actual $state.State -Because "白名单文案 '$($c.Lines[0])' 必须判为组不存在"
        Assert-Equal -Expected 0 -Actual $state.Committed.Count -Because '组不存在时没有任何位点'
    }
}

Test-Case 'Resolve-KafkaGroupTopicOffsets:无表头 + has no active members 判 no-offsets;有表头但本 topic 只有 "-" 也是 no-offsets' {
    $empty = Resolve-KafkaGroupTopicOffsets -Lines @('', "Consumer group 'g' has no active members.") -ExitCode 0 -Topic $Topic
    Assert-Equal -Expected 'no-offsets' -Actual $empty.State -Because 'Empty 组一个位点都没有时 CLI 不打表头'
    $dashOnly = Resolve-KafkaGroupTopicOffsets -Lines @($DescribeHeader, (New-DescribeRow -Partition 0 -Current '-' -End '5' -Lag '-')) -ExitCode 0 -Topic $Topic
    Assert-Equal -Expected 'no-offsets' -Actual $dashOnly.State -Because '分配了但还没提交过,不算有位点'
    $other = Resolve-KafkaGroupTopicOffsets -Lines @($DescribeHeader, (New-DescribeRow -RowTopic 'other_topic' -Partition 0 -Current '5' -End '5' -Lag '0')) -ExitCode 0 -Topic $Topic
    Assert-Equal -Expected 'no-offsets' -Actual $other.State -Because '组只在别的 topic 上有位点,本 topic 视为没有'
    $held = Resolve-KafkaGroupTopicOffsets -Lines @($DescribeHeader, (New-DescribeRow -Partition 2 -Current '8' -End '9' -Lag '1')) -ExitCode 0 -Topic $Topic
    Assert-Equal -Expected 'offsets' -Actual $held.State -Because '本 topic 有数值位点'
    Assert-Equal -Expected 8 -Actual $held.Committed[2] -Because 'Committed 以分区号为键'
}

Test-Case 'Resolve-KafkaGroupTopicOffsets:Error 行、退出码非 0、无表头无关键字一律 throw(fail-closed)' {
    $cases = @(
        @{ Label = 'Error 行'; Lines = @('Error: Executing consumer group command failed due to org.apache.kafka.common.errors.TimeoutException'); Exit = 0 },
        @{ Label = '退出码非 0'; Lines = @($DescribeHeader, (New-DescribeRow -Partition 0 -Current '1' -End '1' -Lag '0')); Exit = 1 },
        @{ Label = '无表头无关键字'; Lines = @('Warning: Consumer group ''g'' is rebalancing.'); Exit = 0 },
        @{ Label = '无输出'; Lines = @(); Exit = 0 }
    )
    foreach ($c in $cases) {
        $message = Get-ThrownMessage { Resolve-KafkaGroupTopicOffsets -Lines $c.Lines -ExitCode $c.Exit -Topic $Topic }
        Assert-True -Condition ($null -ne $message) -Because "$($c.Label):判不出就不能当成「没有位点」"
    }
}

Test-Case 'ConvertFrom-KafkaResetPlan:按 NEW-OFFSET 表头解析,只取本 topic;缺表头、非数值、本 topic 无行都 throw' {
    $lines = @(
        'GROUP                          TOPIC                          PARTITION  NEW-OFFSET',
        "data_service-transaction-log   $Topic               0          12345",
        "data_service-transaction-log   $Topic               1          23456",
        'data_service-transaction-log   player_snapshot_topic_g1       0          1'
    )
    $plan = ConvertFrom-KafkaResetPlan -Lines $lines -Topic $Topic
    Assert-Equal -Expected 2 -Actual $plan.Count -Because '别的 topic 不计入'
    Assert-Equal -Expected 23456 -Actual $plan[1] -Because '分区 1 的 NEW-OFFSET'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-KafkaResetPlan -Lines @('OK') -Topic $Topic })) -Because '没有计划表就没法复核,必须失败'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-KafkaResetPlan -Lines @($lines[0], "g  $Topic  0  -") -Topic $Topic })) -Because 'NEW-OFFSET 不是数值必须失败'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-KafkaResetPlan -Lines @($lines[0]) -Topic $Topic })) -Because '计划里没有本 topic 的分区必须失败'
}

Test-Case '重置计划覆盖检查与 -Apply 后复核' {
    $plan = @{ 0 = 10; 1 = 20 }
    Assert-Equal -Expected 0 -Actual (Get-ResetPlanProblems -Plan $plan -Partitions 2).Count -Because '计划覆盖 0..1'
    Assert-Equal -Expected 0 -Actual (Get-ResetPlanProblems -Plan $plan -Partitions 0).Count -Because '没给 -Partitions 不做覆盖检查'
    $short = Get-ResetPlanProblems -Plan $plan -Partitions 3
    Assert-True -Condition (@($short | Where-Object { $_ -match '缺少分区 2' }).Count -eq 1) -Because "计划缺分区必须报出。实际: $($short -join '; ')"
    $extra = Get-ResetPlanProblems -Plan @{ 0 = 1; 1 = 1; 2 = 1 } -Partitions 2
    Assert-True -Condition (@($extra | Where-Object { $_ -match '分区 2' }).Count -eq 1) -Because '计划里多出分区说明实际分区数与 -Partitions 不一致'

    $verified = Get-ResetVerificationProblems -Plan $plan -Described ([pscustomobject]@{ State = 'offsets'; Committed = @{ 0 = 10; 1 = 20 } }) -Partitions 2
    Assert-Equal -Expected 0 -Actual $verified.Count -Because "CURRENT-OFFSET 与计划一致应当通过。实际: $($verified -join '; ')"
    $stillMissing = Get-ResetVerificationProblems -Plan $plan -Described ([pscustomobject]@{ State = 'missing'; Committed = @{} }) -Partitions 2
    Assert-True -Condition (@($stillMissing | Where-Object { $_ -match '仍不存在' }).Count -eq 1) -Because '重置后组仍不存在 = --execute 没建出位点,必须失败'
    $mismatch = Get-ResetVerificationProblems -Plan $plan -Described ([pscustomobject]@{ State = 'offsets'; Committed = @{ 0 = 10; 1 = 25 } }) -Partitions 2
    Assert-True -Condition (@($mismatch | Where-Object { $_ -match '分区 1' }).Count -eq 1) -Because "位点与计划不符必须报出。实际: $($mismatch -join '; ')"
    $absent = Get-ResetVerificationProblems -Plan $plan -Described ([pscustomobject]@{ State = 'offsets'; Committed = @{ 0 = 10 } }) -Partitions 2
    Assert-True -Condition (@($absent | Where-Object { $_ -match '分区 1' -and $_ -match '没有已提交位点' }).Count -eq 1) -Because '计划内的分区重置后没有位点必须报出'
}

Test-Case '重置模式:-Apply 之后必须按 --execute 的计划逐分区复核,复核失败即 throw' {
    $text = Get-Content $scriptPath -Raw
    $marker = $text.IndexOf('# ── Offset reset (group + topic mode)')
    Assert-True -Condition ($marker -ge 0) -Because '找不到重置模式段落的锚点注释(契约测试的前提被改掉了)'
    $section = $text.Substring($marker)
    Assert-Match -Text $section -Pattern 'Get-ResetVerificationProblems -Plan \$plan -Described \$afterReset' -Because '-Apply 之后必须按 --execute 的计划逐分区复核'
    Assert-True -Condition ($section.IndexOf('重置后复核失败') -ge 0) -Because '复核不过必须 throw,不能打印 OK'
}

# 假的 kafka-consumer-groups.sh:按调用顺序回放 $script:FakeCgReplies(@{ Lines; ExitCode }),并记下每次的参数。
$script:FakeCgCalls = [System.Collections.Generic.List[string]]::new()
$script:FakeCgReplies = [System.Collections.Generic.Queue[object]]::new()
function Invoke-FakeCg {
    $script:FakeCgCalls.Add(($args -join ' '))
    if ($script:FakeCgReplies.Count -eq 0) { throw '假 kafka-consumer-groups.sh 没有准备好的回应(被测函数多调了一次)' }
    $reply = $script:FakeCgReplies.Dequeue()
    $global:LASTEXITCODE = $reply.ExitCode
    return $reply.Lines
}
function Reset-FakeCg {
    param([object[]]$Replies)
    $script:FakeCgCalls.Clear()
    $script:FakeCgReplies.Clear()
    foreach ($reply in $Replies) { $script:FakeCgReplies.Enqueue($reply) }
}

# 参数校验:子进程跑真脚本,参数校验先于解析任何 Kafka 工具,不连 Kafka。
function Invoke-ResetScript {
    param([string[]]$Arguments)
    return Invoke-ToolScript -ScriptName 'kafka_offset_reset.ps1' -Arguments (@('-BootstrapServer', 'b:9092', '-Topic', $Topic) + $Arguments)
}

Test-Case '已删除的模式与参数:-RetireGroup / -FinalizeHandoff 不再声明;只给一种模式的校验还在' {
    $paramNames = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
    foreach ($name in @('RetireGroup', 'FinalizeHandoff')) {
        Assert-True -Condition ($name -notin $paramNames) -Because "-$name 只服务已删除的存盘消费组交接,不能留着"
    }
    $text = Get-Content $scriptPath -Raw
    Assert-NotMatch -Text $text -Pattern 'db_task|db_rpc_consumer_group|Remove-RetiredGroupTopicOffsets' -Because '说明与示例只写现存的 topic 与消费组'
    $both = Invoke-ResetScript -Arguments @('-Group', 'z', '-ToLatest', '-ToEarliest')
    Assert-True -Condition ($both.ExitCode -ne 0) -Because "两种模式同时给必须拒绝。输出: $($both.Output)"
    Assert-Match -Text $both.Output -Pattern 'Pass exactly one of' -Because '错误文本要列出可选的模式'
}

# ─────────────────────────────────────────────────────────────────
# R7 复核(2026-10-17):[1] 删 topic 重建、[5] 重置分支退出码、[8] 白名单与表头矛盾
# ─────────────────────────────────────────────────────────────────

$recreateFunctions = @('ConvertFrom-KafkaTopicDescribe', 'Wait-KafkaTopicDeleted')
foreach ($name in $recreateFunctions) {
    $matched = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true))
    if ($matched.Count -ne 1) { throw "kafka_offset_reset.ps1 里应有且只有一个 $name,实际 $($matched.Count) 个" }
    . ([scriptblock]::Create($matched[0].Extent.Text))
}

Test-Case '[8] 白名单命中又列出本 topic 的数值位点 = 矛盾输出,throw;有表头但本 topic 没有数值位点 → no-offsets;没有表头仍判 missing' {
    # 管理客户端在新旧 describe API 之间回退时可能先打一行带 GroupIdNotFoundException 的 WARN,之后照常打印表格(4.3.1 上是否如此待 V3 实测)。
    $warn = '[2026-10-17 03:00:00,000] WARN describe fell back to the classic API after org.apache.kafka.common.errors.GroupIdNotFoundException'
    $contradictory = @($warn, $DescribeHeader, (New-DescribeRow -Partition 0 -Current '40' -End '40' -Lag '0'))
    $message = Get-ThrownMessage { Resolve-KafkaGroupTopicOffsets -Lines $contradictory -ExitCode 0 -Topic $Topic }
    Assert-True -Condition ($null -ne $message) -Because '一边说组不存在、一边列出已提交位点,不猜哪一边为真'
    Assert-Match -Text $message -Pattern '自相矛盾' -Because '错误文本要点明是矛盾输出'
    Assert-Match -Text $message -Pattern '分区 0' -Because '要列出表里有位点的分区'

    $otherTopic = @($warn, $DescribeHeader, (New-DescribeRow -RowTopic 'player_snapshot_topic_g1' -Partition 0 -Current '5' -End '5' -Lag '0'))
    Assert-Equal -Expected 'no-offsets' -Actual (Resolve-KafkaGroupTopicOffsets -Lines $otherTopic -ExitCode 0 -Topic $Topic).State -Because '有表头时不认定 missing;本 topic 没有数值位点就是 no-offsets'
    $dashOnly = @($warn, $DescribeHeader, (New-DescribeRow -Partition 0 -Current '-' -End '5' -Lag '-'))
    Assert-Equal -Expected 'no-offsets' -Actual (Resolve-KafkaGroupTopicOffsets -Lines $dashOnly -ExitCode 0 -Topic $Topic).State -Because '"-" 行不算位点'
    $missing = Resolve-KafkaGroupTopicOffsets -Lines @('Error: Executing consumer group command failed due to org.apache.kafka.common.errors.GroupIdNotFoundException: Group g not found.') -ExitCode 1 -Topic $Topic
    Assert-Equal -Expected 'missing' -Actual $missing.State -Because '没有表头时白名单照旧认定组不存在(不论退出码)'
}

Test-Case '[5] 「组不存在」的 describe 以非 0 退出时,Invoke-KafkaConsumerGroupDescribe 判为 missing,且不把非 0 留在 $LASTEXITCODE' {
    # 有的 Kafka 版本对不存在的组以非 0 退出(lib 判为 missing);以前这个退出码留在全局 LASTEXITCODE 上,重置分支又不以 exit 结束,
    # dev_tools 把成功判成失败。
    Reset-FakeCg -Replies @(@{ Lines = @('Error: Executing consumer group command failed due to org.apache.kafka.common.errors.GroupIdNotFoundException: Group g not found.'); ExitCode = 1 })
    $global:LASTEXITCODE = 0
    $state = Invoke-KafkaConsumerGroupDescribe -CgTool 'Invoke-FakeCg' -BootstrapServer 'b:9092' -Group 'g' -Topic $Topic 6>$null
    Assert-Equal -Expected 'missing' -Actual $state.State -Because '组不存在应当正常返回 missing'
    Assert-Equal -Expected 0 -Actual $global:LASTEXITCODE -Because 'Invoke-KafkaToolCaptured 只经返回值报告退出码,全局 LASTEXITCODE 必须复位为 0'
}

Test-Case '[5] 重置模式的成功路径以 exit 0 结束(与 -DeleteAndRecreateTopic 一致)' {
    $last = @($ast.EndBlock.Statements)[-1]
    Assert-True -Condition ($last -is [System.Management.Automation.Language.ExitStatementAst]) -Because "脚本最后一条顶层语句必须是 exit(实际: $($last.Extent.Text))"
    Assert-Equal -Expected '0' -Actual $last.Pipeline.Extent.Text -Because '成功路径以 0 退出,失败一律 throw'
}

Test-Case '[1] -DeleteAndRecreateTopic:缺 -ReplicationFactor 拒绝,早于解析 Kafka 工具' {
    $noRf = Invoke-ToolScript -ScriptName 'kafka_offset_reset.ps1' -Arguments @('-BootstrapServer', 'b:9092', '-Topic', 'contract_recreate_topic', '-DeleteAndRecreateTopic', '-Partitions', '3')
    Assert-True -Condition ($noRf.ExitCode -ne 0) -Because "缺 -ReplicationFactor 必须拒绝。输出: $($noRf.Output)"
    Assert-Match -Text $noRf.Output -Pattern '必须显式给 -ReplicationFactor' -Because '以前缺省 1,3 broker 档上重建出的 topic 写入全部 NotEnoughReplicas'
    Assert-NotMatch -Text $noRf.Output -Pattern 'Step 1: describe' -Because '拒绝早于任何 Kafka 调用'
}

Test-Case '[1] ConvertFrom-KafkaTopicDescribe:读摘要行的 PartitionCount / ReplicationFactor(新旧两种写法);找不到或多于一行即 throw' {
    $lines = @("Topic: contract_t`tTopicId: abc`tPartitionCount: 10`tReplicationFactor: 3`tConfigs: min.insync.replicas=2",
        "`tTopic: contract_t`tPartition: 0`tLeader: 1`tReplicas: 1,2,3`tIsr: 1,2,3")
    $contract = ConvertFrom-KafkaTopicDescribe -Lines $lines -Topic 'contract_t'
    Assert-Equal -Expected 10 -Actual $contract.PartitionCount -Because '分区数取摘要行'
    Assert-Equal -Expected 3 -Actual $contract.ReplicationFactor -Because '副本数取摘要行(分区明细行不算)'
    $old = ConvertFrom-KafkaTopicDescribe -Lines @("Topic:contract_t`tPartitionCount:5`tReplicationFactor:1`tConfigs:") -Topic 'contract_t'
    Assert-Equal -Expected '5/1' -Actual "$($old.PartitionCount)/$($old.ReplicationFactor)" -Because '旧版本冒号后没有空格'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-KafkaTopicDescribe -Lines @("Topic: contract_t_other`tPartitionCount: 1`tReplicationFactor: 1") -Topic 'contract_t' })) -Because '别的 topic 的摘要行不算,读不出就不删'
    Assert-True -Condition ($null -ne (Get-ThrownMessage { ConvertFrom-KafkaTopicDescribe -Lines @() -Topic 'contract_t' })) -Because '没有输出必须失败'
}

Test-Case '[1] Wait-KafkaTopicDeleted:topic 消失才返回;--list 失败或超时一律 throw(不进入 create)' {
    Reset-FakeCg -Replies @(@{ Lines = @('other_topic', 'contract_t'); ExitCode = 0 }, @{ Lines = @('other_topic'); ExitCode = 0 })
    $message = Get-ThrownMessage { Wait-KafkaTopicDeleted -TopicTool 'Invoke-FakeCg' -BootstrapServer 'b:9092' -Topic 'contract_t' -TimeoutSec 30 -PollMilliseconds 0 -RecreateHint 'RECREATE-HINT' }
    Assert-True -Condition ($null -eq $message) -Because "第二次 --list 里已没有该 topic,应当返回。错误: $message"
    Assert-Equal -Expected 2 -Actual $script:FakeCgCalls.Count -Because '轮询到消失为止'

    Reset-FakeCg -Replies @(@{ Lines = @('Error: Timed out waiting for a node assignment.'); ExitCode = 1 })
    $message = Get-ThrownMessage { Wait-KafkaTopicDeleted -TopicTool 'Invoke-FakeCg' -BootstrapServer 'b:9092' -Topic 'contract_t' -TimeoutSec 30 -PollMilliseconds 0 -RecreateHint 'RECREATE-HINT' }
    Assert-Match -Text $message -Pattern '读不到 topic 列表' -Because '以前 --list 失败被 2>$null 当成「已消失」直接 create'
    Assert-Match -Text $message -Pattern 'RECREATE-HINT' -Because '要告诉操作者 topic 可能已删、怎么手工重建'

    Reset-FakeCg -Replies @(@{ Lines = @('contract_t'); ExitCode = 0 })
    $message = Get-ThrownMessage { Wait-KafkaTopicDeleted -TopicTool 'Invoke-FakeCg' -BootstrapServer 'b:9092' -Topic 'contract_t' -TimeoutSec 0 -PollMilliseconds 0 -RecreateHint 'RECREATE-HINT' }
    Assert-Match -Text $message -Pattern '仍在 --list' -Because '超时不能照样 create'
    Assert-Equal -Expected 1 -Actual $script:FakeCgCalls.Count -Because '-TimeoutSec 0 只看一次'
}

Test-Case '-ToLatest + -Partitions + -Apply 通过参数校验;重置分支把 -ToLatest 映射成 --to-latest、-Apply 映射成 --execute' {
    # -KafkaBin 指向空目录:参数校验通过后在解析 Kafka 工具那一步停下,保证不连任何 Kafka(本机装了 CLI 也不会被调到)。
    $emptyBin = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-empty-kafka-bin-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $emptyBin | Out-Null
    try {
        $run = Invoke-ResetScript -Arguments @('-Group', 'data_service-transaction-log', '-ToLatest', '-Partitions', '6', '-KafkaBin', $emptyBin, '-Apply')
    }
    finally {
        Remove-Item -LiteralPath $emptyBin -Recurse -Force -ErrorAction SilentlyContinue
    }
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "空的 -KafkaBin 必须在解析工具时失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Kafka tool not found at' -Because "参数组合必须通过参数校验,停在解析工具那一步。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'mode\s+: ToLatest' -Because '模式是 ToLatest'

    $text = Get-Content $scriptPath -Raw
    Assert-Match -Text $text -Pattern '"ToLatest"\s*\{\s*\$cmdArgs \+= "--to-latest"\s*\}' -Because '-ToLatest 映射成 --to-latest'
    Assert-Match -Text $text -Pattern 'if \(\$Apply\) \{\s*\$cmdArgs \+= "--execute"' -Because '-Apply 映射成 --execute'
}

exit (Complete-TestRun -SuiteName 'kafka_offset_reset 位点重置结果判定')
