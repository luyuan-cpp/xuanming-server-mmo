#requires -Version 7
<#
.SYNOPSIS
    gate 客户端连接准入:指标、装配与告警的契约测试(2026-11-12)。

.DESCRIPTION
    deploy/k8s/gate-connection-alerts.yaml 的规则读 gate 的 Prometheus 出口
    (cpp/nodes/gate/handler/rpc/connection_admission.cpp 的 CollectMetrics)。这里钉的都是「写错了不报错、只会永远不响」
    或「少接一行就整个不生效」的东西:
      - 规则引用的每个指标名都由 CollectMetrics 的 BeginFamily 导出(直方图按 _bucket / _sum / _count 认);
      - reason 取值都在 connection_admission.h 的 kRejectReasonNames 里;
      - gate 的 main.cpp 确实把准入回调装到了 TcpServer 上、登记了指标、启动时配了参数、关源时同步关了准入闸;
      - 建连 / 验签路径确实调了未验签占位的两个钩子;
      - 指标标签只有 reason:对端地址、会话号、账号、玩家号不进指标(AGENTS.md §9);
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 告警文件里某一组的文本:从 `    - name: <组名>` 到下一个组或文件尾。
function Get-AlertGroupText {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Group)
    $m = [regex]::Match($Text, ('(?ms)^    - name:[ ]*{0}[ ]*\n(?<body>.*?)(?=^    - name:|\z)' -f [regex]::Escape($Group)))
    if (-not $m.Success) { throw "找不到告警组 $Group" }
    return $m.Groups['body'].Value
}

# 一组里的每条规则:名字与规则体(到下一条规则、规则前注释或组尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$GroupText)
    return @([regex]::Matches($GroupText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        #|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/gate-connection-alerts.yaml'
$script:AdmissionHeader = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/connection_admission.h'
$script:AdmissionSource = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/connection_admission.cpp'
$script:GateMain = Read-RepoText -RelativePath 'cpp/nodes/gate/main.cpp'
$script:SessionHandler = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/client_session_handler.cpp'

# C++ 导出的指标族:名字 → 类型(counter / gauge / histogram)。
$script:Families = [ordered]@{}
foreach ($m in [regex]::Matches($script:AdmissionSource, '(?s)BeginFamily\("(?<n>[a-z_]+)".*?MetricType::k(?<t>Counter|Gauge|Histogram)\)')) {
    $script:Families[$m.Groups['n'].Value] = $m.Groups['t'].Value.ToLowerInvariant()
}
# 拒绝原因的标签取值:kRejectReasonNames 的初始化列表。
$reasonList = [regex]::Match($script:AdmissionHeader, '(?s)kRejectReasonNames = std::to_array<std::string_view>\(\{(?<body>.*?)\}\);')
if (-not $reasonList.Success) { throw 'connection_admission.h 里找不到 kRejectReasonNames 的初始化列表' }
$script:Reasons = @([regex]::Matches($reasonList.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value })

# 规则表达式里的一个 gate_* 名字对应哪个指标族:直方图的样本名是族名加 _bucket / _sum / _count。
function Resolve-MetricFamily {
    param([Parameter(Mandatory = $true)][string]$Name)
    if ($script:Families.Contains($Name)) { return $Name }
    foreach ($suffix in @('_bucket', '_sum', '_count')) {
        if ($Name.EndsWith($suffix)) {
            $base = $Name.Substring(0, $Name.Length - $suffix.Length)
            if ($script:Families.Contains($base) -and $script:Families[$base] -ceq 'histogram') { return $base }
        }
    }
    return $null
}

$script:ExpectedRules = [ordered]@{
    'GateConnectionsNearCapacity'        = 'warning'
    'GateRejectingAtCapacity'            = 'warning'
    'GateAcceptRateLimited'              = 'warning'
    'GateUnauthenticatedConnectionsHigh' = 'warning'
    'GatePreauthConnectionsReaped'       = 'warning'
    'GateHandshakeSlow'                  = 'warning'
}

Test-Case '夹具:CollectMetrics 导出的指标族与类型,拒绝原因的取值' {
    $actual = @($script:Families.GetEnumerator() | ForEach-Object { '{0}:{1}' -f $_.Key, $_.Value }) -join ','
    Assert-Equal -Expected 'gate_connections_current:gauge,gate_connections_max:gauge,gate_unauthenticated_connections:gauge,gate_accepted_total:counter,gate_rejected_total:counter,gate_accept_rate_limit_per_second:gauge,gate_accept_burst:gauge,gate_handshake_duration_ms:histogram,gate_preauth_reaped_total:counter' `
        -Actual $actual -Because 'CollectMetrics 的指标族与类型;增删指标要同步告警、设计稿 P1-4 与本用例'
    Assert-Equal -Expected 'capacity,accept_rate,shutting_down,not_ready,no_token_secret' -Actual ($script:Reasons -join ',') -Because 'gate_rejected_total 的 reason 取值(顺序即 RejectReason 的枚举顺序)'
    foreach ($family in $script:Families.Keys) {
        if ($script:Families[$family] -ceq 'counter') {
            Assert-Match -Text $family -Pattern '_total$' -Because "$family 是 counter,名字以 _total 结尾(Prometheus 命名约定)"
        }
    }
}

Test-Case '指标标签只有 reason:对端地址、会话号、账号、玩家号不进指标' {
    $labels = @([regex]::Matches($script:AdmissionSource, 'Sample\(\{\{"(?<l>[a-z_]+)"') | ForEach-Object { $_.Groups['l'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'reason' -Actual ($labels -join ',') -Because '标签基数由攻击者控制时,连接层的洪水会变成监控系统的洪水(AGENTS.md §9)'
    $collect = [regex]::Match($script:AdmissionSource, '(?ms)^void CollectMetrics\(prometheus_text::Writer &writer\)\n\{\n(?<body>.*?)^\}')
    Assert-True -Condition $collect.Success -Because '找得到 CollectMetrics 的函数体'
    Assert-NotMatch -Text $collect.Groups['body'].Value -Pattern 'toIp|peer|sessionId|playerId|account' -Because '采集函数里不出现对端地址、会话号、玩家号与账号'
}

Test-Case 'gate main.cpp:准入回调装在 TcpServer 上,指标已登记,启动时配参数,关源时同步关准入闸' {
    Assert-Match -Text $script:GateMain -Pattern '(?m)^\s*ConfigureConnectionAdmissionOrDie\(\);$' -Because '启动时要定下上限与速率闸参数'
    Assert-Match -Text $script:GateMain -Pattern 'gate_connection_admission::ResolveRateLimit\(' -Because '速率闸参数经 ResolveRateLimit 解析(非法值拒启)'
    Assert-Match -Text $script:GateMain -Pattern 'LOG_FATAL << "Refusing to start: " << error;' -Because '两个环境变量写错时拒绝启动,不带着一个没人知道的速率上限跑'
    Assert-Match -Text $script:GateMain -Pattern '(?m)^\s*gate_connection_admission::RegisterMetrics\(\);$' -Because '不登记采集回调,这些序列根本不存在'
    Assert-Match -Text $script:GateMain -Pattern '(?s)n\.GetTcpServer\(\)\.setAdmissionCallback\(\s*\[\]\(const muduo::net::InetAddress& peer\) \{\s*return gate_connection_admission::AdmitConnection\(peer, std::chrono::steady_clock::now\(\)\);' `
        -Because '准入回调不捕获任何对象(AGENTS.md §11.7),判定用单调时钟'
    $admissionAt = $script:GateMain.IndexOf('n.GetTcpServer().setAdmissionCallback(')
    $connectionAt = $script:GateMain.IndexOf('n.GetTcpServer().setConnectionCallback(')
    $nodeIdAt = $script:GateMain.IndexOf('tlsSessionManager.session_id_gen().set_node_id(n.GetNodeId());')
    Assert-True -Condition ($nodeIdAt -ge 0 -and $admissionAt -gt $nodeIdAt -and $connectionAt -gt $admissionAt) -Because '顺序:先种 session_id 的 node 段,再装准入回调,再装连接回调'
    Assert-Match -Text $script:GateMain -Pattern '(?s)context\.sourceClosed = true;.{0,400}?gate_connection_admission::CloseSource\(\);' -Because '关源时 accept 处的准入闸要一起关,否则停机期间新连接照样建出来再被关'
}

Test-Case '建连与验签路径:未验签占位的两个钩子都接上了,纵深防御分支计入拒绝原因' {
    Assert-Match -Text $script:SessionHandler -Pattern '(?s)sessions\(\)\.emplace\(sessionId, std::move\(session\)\)\.first;.{0,400}?gate_connection_admission::OnSessionOpened\(sessionEntry->second, establishedAt\);' `
        -Because '会话插入之后立刻计接纳、发未验签占位'
    Assert-Match -Text $script:SessionHandler -Pattern 'gate_connection_admission::OnSessionVerified\(session, verifiedAt\);' -Because '验签成功时归还占位、记握手耗时'
    foreach ($reason in @('kCapacity', 'kNotReady', 'kNoTokenSecret')) {
        Assert-Match -Text $script:SessionHandler -Pattern ('gate_connection_admission::NoteRejectedAfterAccept\(gate_connection_admission::RejectReason::{0}\);' -f $reason) `
            -Because "连接回调里的 $reason 分支要计进 gate_rejected_total"
    }
    Assert-Match -Text $script:AdmissionSource -Pattern 'std::make_shared<PreAuthSlot>\(tlsUnauthenticated, now\)' -Because '占位是 RAII:会话从任何路径销毁都归还,不靠各出口手写减一'
    Assert-Match -Text $script:AdmissionSource -Pattern 'gate_client_heartbeat::GetCounters\(\)\.handshakeTimeouts' -Because 'gate_preauth_reaped_total 与握手超时的汇总日志读同一份计数'
}

Test-Case 'gate-connection-alerts.yaml:PrometheusRule 形状,gate.connections 组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*gate-connection-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'gate.connections'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/ops/gate-kernel-tuning-runbook\.md[ ]*$' -Because "$($rule.Name) 指向 gate 的运维手册"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[mh][ ]*$' -Because "$($rule.Name) 要写 for"
    }
}

Test-Case 'gate-connection-alerts.yaml:引用的指标都由 C++ 导出,reason 取值都在导出的取值里' {
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'gate.connections'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\bgate_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 gate_* 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($null -ne (Resolve-MetricFamily -Name $metric)) -Because "$($rule.Name):$metric 必须由 connection_admission.cpp 导出(改名后告警静默失效)"
        }
        foreach ($m in [regex]::Matches($expr, '\breason\s*=~?\s*"(?<v>[^"]*)"')) {
            foreach ($v in ($m.Groups['v'].Value -split '\|')) {
                Assert-True -Condition ($script:Reasons -ccontains $v) -Because "$($rule.Name):reason=$v 必须是 C++ 导出的取值"
            }
        }
    }
}

Test-Case '本文件钉的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:ExpectedRules.Keys)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'gate_connection_alerts_contract')
