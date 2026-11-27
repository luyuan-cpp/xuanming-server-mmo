#requires -Version 7
<#
.SYNOPSIS
    battle 客户端直连的连接准入:指标、装配与告警的契约测试(2026-11-19)。

.DESCRIPTION
    deploy/k8s/battle-connection-alerts.yaml 的规则读 battle 的 Prometheus 出口
    (cpp/nodes/battle/client/battle_connection_admission.cpp 的 Admission::CollectMetrics)。这里钉的都是「写错了不报错、只会永远不响」
    或「少接一行就整个不生效」的东西,与 gate_connection_alerts_contract.tests.ps1 同一套:
      - 规则引用的每个指标名都由 CollectMetrics 的 BeginFamily 导出(直方图按 _bucket / _sum / _count 认);
      - reason 取值都在 battle_connection_admission.h 的 kRejectReasonNames 里,与 gate 同义的取值写法相同;
      - 直连面确实把准入回调装到了 TcpServer 上(只绑 weak)、登记了指标,建连 / 握手 / 握手超时 / 纵深防御分支都计了数,
        未认证连接数不把出示过探测授权的连接算进去;
      - 票据先行(准入设计 P2-4):握手之前只有未认证记录、不建 DirectSession,握手成功或探测授权验过才建;
        握手期限由 1 秒节拍从池头扫;
      - battle 的 main.cpp 启动时配了速率闸参数(写错拒启)、关源时同步关了准入闸;
      - 指标标签只有 reason 与 result:对端地址、房间号、玩家号不进指标(AGENTS.md §9);
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1
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

# 某个源文件里 kRejectReasonNames 初始化列表的取值(顺序即枚举顺序)。
function Get-RejectReasonNames {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Where)
    $list = [regex]::Match($Text, '(?s)kRejectReasonNames = std::to_array<std::string_view>\(\{(?<body>.*?)\}\);')
    if (-not $list.Success) { throw "$Where 里找不到 kRejectReasonNames 的初始化列表" }
    return @([regex]::Matches($list.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value })
}

# 一个函数的函数体:从签名行到与签名同缩进的 `}`(匿名空间里的函数缩进一级,首尾花括号跟着签名缩进)。
function Get-FunctionBody {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$SignaturePattern)
    $m = [regex]::Match($Text, ('(?ms)^(?<indent>[ \t]*){0}\n\k<indent>\{{\n(?<body>.*?)^\k<indent>\}}' -f $SignaturePattern))
    if (-not $m.Success) { throw "找不到函数体:$SignaturePattern" }
    return $m.Groups['body'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/battle-connection-alerts.yaml'
$script:AdmissionHeader = Read-RepoText -RelativePath 'cpp/nodes/battle/client/battle_connection_admission.h'
$script:AdmissionSource = Read-RepoText -RelativePath 'cpp/nodes/battle/client/battle_connection_admission.cpp'
$script:EdgeSource = Read-RepoText -RelativePath 'cpp/nodes/battle/client/battle_client_edge.cpp'
$script:EdgeHeader = Read-RepoText -RelativePath 'cpp/nodes/battle/client/battle_client_edge.h'
$script:BattleMain = Read-RepoText -RelativePath 'cpp/nodes/battle/main.cpp'
$script:GateAdmissionHeader = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/connection_admission.h'

# C++ 导出的指标族:名字 → 类型(counter / gauge / histogram)。
$script:Families = [ordered]@{}
foreach ($m in [regex]::Matches($script:AdmissionSource, '(?s)BeginFamily\("(?<n>[a-z_]+)".*?MetricType::k(?<t>Counter|Gauge|Histogram)\)')) {
    $script:Families[$m.Groups['n'].Value] = $m.Groups['t'].Value.ToLowerInvariant()
}
$script:Reasons = Get-RejectReasonNames -Text $script:AdmissionHeader -Where 'battle_connection_admission.h'

# 规则表达式里的一个 battle_* 名字对应哪个指标族:直方图的样本名是族名加 _bucket / _sum / _count。
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
    'BattleConnectionsNearCapacity'        = 'warning'
    'BattleRejectingAtCapacity'            = 'warning'
    'BattleAcceptRateLimited'              = 'warning'
    'BattleUnauthenticatedConnectionsHigh' = 'warning'
    'BattlePreauthBudgetRejecting'         = 'warning'
    'BattleSourceRateLimited'              = 'warning'
    'BattleSourceTableFull'                = 'warning'
    'BattlePreauthConnectionsReaped'       = 'warning'
    'BattleHandshakeSlow'                  = 'warning'
}

Test-Case '夹具:CollectMetrics 导出的指标族与类型,拒绝原因的取值' {
    $actual = @($script:Families.GetEnumerator() | ForEach-Object { '{0}:{1}' -f $_.Key, $_.Value }) -join ','
    Assert-Equal -Expected 'battle_connections_current:gauge,battle_connections_max:gauge,battle_unauthenticated_connections:gauge,battle_accepted_total:counter,battle_rejected_total:counter,battle_accept_rate_limit_per_second:gauge,battle_accept_burst:gauge,battle_handshake_duration_ms:histogram,battle_preauth_reaped_total:counter,battle_preauth_evicted_total:counter,battle_preauth_budget:gauge,battle_preauth_early_drop_start:gauge,battle_preauth_evictable_age_ms:gauge,battle_source_rate_limit_per_second:gauge,battle_source_burst:gauge,battle_source_rate_active:gauge,battle_source_tracked:gauge,battle_source_untracked_total:counter,battle_websocket_handshakes_total:counter,battle_websocket_protocol_closed_total:counter' `
        -Actual $actual -Because 'CollectMetrics 的指标族与类型;增删指标要同步告警、设计稿 D27 与本用例(websocket 两族见 network/codec/websocket.h,与 gate 同义)'
    Assert-Equal -Expected 'capacity,accept_rate,shutting_down,no_token_secret,preauth_budget,preauth_early_drop,source_rate' -Actual ($script:Reasons -join ',') -Because 'battle_rejected_total 的 reason 取值(顺序即 RejectReason 的枚举顺序)'
    foreach ($family in $script:Families.Keys) {
        if ($script:Families[$family] -ceq 'counter') {
            Assert-Match -Text $family -Pattern '_total$' -Because "$family 是 counter,名字以 _total 结尾(Prometheus 命名约定)"
        }
    }
}

Test-Case '与 gate 同义的拒绝原因用同一个取值' {
    $gateReasons = Get-RejectReasonNames -Text $script:GateAdmissionHeader -Where 'gate 的 connection_admission.h'
    foreach ($reason in $script:Reasons) {
        Assert-True -Condition ($gateReasons -ccontains $reason) -Because "battle 的 reason=$reason 在 gate_rejected_total 里也有同义取值,两个节点的告警与看板按 reason 并列"
    }
}

Test-Case '指标标签只有 reason 与 result:对端地址、房间号、玩家号不进指标' {
    $labels = @([regex]::Matches($script:AdmissionSource, 'Sample\(\{\{"(?<l>[a-z_]+)"') | ForEach-Object { $_.Groups['l'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'reason,result' -Actual ($labels -join ',') -Because '两个标签都是封闭集合(拒绝原因、websocket 握手结果);标签基数由攻击者控制时,连接层的洪水会变成监控系统的洪水(AGENTS.md §9)'
    $collect = Get-FunctionBody -Text $script:AdmissionSource -SignaturePattern 'void Admission::CollectMetrics\(.*?\) const'
    Assert-NotMatch -Text $collect -Pattern 'toIp|peer|battleId|battle_id|playerId|player_id' -Because '采集函数里不出现对端地址、房间号与玩家号'
}

Test-Case '直连面:准入回调装在 TcpServer 上且只绑 weak,指标已登记,各计数点都接上了' {
    $install = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'void BattleClientEdge::Install\(muduo::net::TcpServer &server\)'
    Assert-Match -Text $install -Pattern '(?s)server\.setAdmissionCallback\(\[weak\]\(const muduo::net::InetAddress &peer\).{0,200}?self->AdmitConnection\(peer, std::chrono::steady_clock::now\(\)\)' `
        -Because '准入回调只绑 weak 自己(AGENTS.md §11.7),判定用单调时钟'
    Assert-Match -Text $install -Pattern 'server\.setConnectionCallback\(' -Because '连接回调与准入回调在同一处装上'
    Assert-Match -Text $install -Pattern '(?m)^\s*RegisterMetrics\(\);$' -Because '不登记采集回调,这些序列根本不存在'
    Assert-Match -Text $script:EdgeSource -Pattern 'GlobalMetricsRegistry\(\)\.Register\(\s*"battle_connection_admission", \[weak\]' -Because '采集回调也只绑 weak 自己'
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)unauthenticated\.emplace\(connId, UnauthenticatedConnection\{\.conn = conn, \.openedAt = std::chrono::steady_clock::now\(\)\}\);\s*admission\.NoteAccepted\(\);' `
        -Because '建连只登记一条未认证记录,随即计接纳'
    Assert-Match -Text $script:EdgeSource -Pattern 'admission\.NoteHandshake\(record->second\.openedAt, std::chrono::steady_clock::now\(\)\);' -Because '票据握手成功时记握手耗时'
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)it = unauthenticated\.erase\(it\);\s*if \(conn && conn->connected\(\)\)\s*\{\s*admission\.NoteHandshakeTimeout\(\);\s*LogRejectionSampled\("handshake_timeout", conn\);\s*conn->forceClose\(\);' `
        -Because 'battle_preauth_reaped_total 与握手期限的关闭是同一处:先摘记录再关'
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)self->admission\.NoteHandshakeTimeout\(\);\s*LogRejectionSampled\("handshake_timeout",c\);\s*c->forceClose\(\);' `
        -Because '探测连接从不握手,满握手期限还没探完同样计进 battle_preauth_reaped_total'
    foreach ($reason in @('kCapacity', 'kNoTokenSecret')) {
        Assert-Match -Text $script:EdgeSource -Pattern ('admission\.NoteRejectedAfterAccept\(battle_connection_admission::RejectReason::{0}\);' -f $reason) `
            -Because "连接回调里的 $reason 分支要计进 battle_rejected_total"
    }
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)unauthenticated\.erase\(record\);\s*DirectSession& s=sessions\[connId\];.{0,200}?s\.probeGrant=std::move\(grant\);' `
        -Because '出示过探测授权的连接出未认证池、这时才建会话;不出池的话每台的 BattleUnauthenticatedConnectionsHigh 随探测量误报'
}

Test-Case '直连面的未认证池:按 connId 有序的记录表,握手前不建会话,accept 时 O(1) 不扫会话表,六处进出池,loop 刚卡过不淘汰也不收割,淘汰先摘记录再经 weak 连接 forceClose' {
    Assert-Match -Text $script:EdgeHeader -Pattern '(?m)^\s*std::map<uint64_t, UnauthenticatedConnection> unauthenticated;$' -Because 'connId 单调递增,有序即建连先后,begin() 就是最老的;握手之前只有这一条记录'
    Assert-Match -Text $script:EdgeHeader -Pattern 'size_t UnauthenticatedConnectionCount\(\) const \{ return unauthenticated\.size\(\); \}' -Because '指标与判定读同一个数'
    Assert-Match -Text $script:EdgeHeader -Pattern 'size_t ConnectionCount\(\) const \{ return sessions\.size\(\) \+ unauthenticated\.size\(\); \}' -Because '直连数 = 会话 + 未认证,总量闸与停机排空都按它'
    $established = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'void BattleClientEdge::HandleEstablished\(const muduo::net::TcpConnectionPtr &conn\)'
    Assert-NotMatch -Text $established -Pattern 'sessions\.try_emplace|sessions\[|RunAfter\(' -Because '握手之前不建 DirectSession,也不按连接起定时器(票据先行)'
    Assert-Match -Text $established -Pattern 'if \(ConnectionCount\(\) >= EffectiveMaxConnections\(\)\)' -Because '第二道总量闸按 会话 + 未认证 判'
    $admit = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'bool BattleClientEdge::AdmitConnection\(const muduo::net::InetAddress &peer,\n\s+const battle_connection_admission::SteadyTime now\)'
    Assert-NotMatch -Text $admit -Pattern 'for \([^)]*:\s*sessions\)' -Because '准入回调排在速率闸之前,被拒的洪水也要走它:不能每次 accept 扫一遍会话表'
    Assert-Match -Text $admit -Pattern 'const uint64_t unauthenticated = this->unauthenticated\.size\(\);' -Because '池大小 O(1)'
    Assert-Match -Text $admit -Pattern '(?s)unauthenticated >= admission\.CurrentPreAuthBudget\(\)\.earlyDropStart && !loopStall\.RecentlyStalled\(now\).{0,200}?OldestConnectedUnauthenticated\(now\)' `
        -Because '最老一条只在承压时找;本端 loop 刚卡过时不给年龄、不淘汰(握手请求可能还在接收缓冲里没读)。找的时候用本次判定的时刻,不另取时钟'
    $oldest = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'std::optional<uint64_t> BattleClientEdge::OldestConnectedUnauthenticated\(\n\s+const battle_connection_admission::SteadyTime now\) const'
    Assert-NotMatch -Text $oldest -Pattern 'steady_clock::now\(\)' -Because '半关闭连接是否逾期与随后算年龄必须用同一个时刻,两处各取一次时钟会在临界点上得出相反的结论'
    Assert-Match -Text $admit -Pattern 'EvictUnauthenticated\(\*oldestConnId, now\);' -Because '判定要求淘汰时淘汰找出来的最老一条'
    Assert-Match -Text $admit -Pattern 'admission\.Admit\(peer, ConnectionCount\(\), EffectiveMaxConnections\(\), unauthenticated, oldestAge, now\)' -Because '准入回调的总量按 会话 + 未认证 判'
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)unauthenticated\.erase\(record\);\s*\}\s*DirectSession &session = sessions\[connId\];\s*session\.connId = connId;\s*session\.conn = conn;\s*session\.verified = true;' `
        -Because '票据握手成功出池,这时才建会话'
    Assert-Match -Text $script:EdgeSource -Pattern '(?s)if \(unauthenticated\.erase\(connId\) > 0\)\s*\{\s*return;\s*\}' -Because '没握手就断开只摘未认证记录,没有会话要解绑'
    $evict = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'void BattleClientEdge::EvictUnauthenticated\(const uint64_t connId, const battle_connection_admission::SteadyTime now\)'
    Assert-Match -Text $evict -Pattern '(?s)it->second\.conn\.lock\(\);.*?unauthenticated\.erase\(it\);.*?admission\.NotePreAuthEvicted\(\);.*?conn->forceClose\(\);' -Because '先摘记录(这一刻起不再算在池里),再经记录里的 weak 连接 forceClose 并计一次淘汰'
    Assert-NotMatch -Text $evict -Pattern 'sessions\.erase' -Because '不在准入回调里删会话;未认证直连本来就没有会话'
    $sweep = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'void BattleClientEdge::CloseExpiredHandshakes\(const client_liveness::SteadyTime now, const bool late\)'
    Assert-Match -Text $sweep -Pattern '(?s)if \(late\)\s*\{\s*return;' -Because '本端 loop 卡过的那一拍不收割(握手包可能还在接收缓冲里没读)'
    Assert-Match -Text $sweep -Pattern 'while \(it != unauthenticated\.end\(\) && now - it->second\.openedAt >= handshakeTimeout\)' -Because '从池头扫,碰到第一条没满期限的就停'
    $install = Get-FunctionBody -Text $script:EdgeSource -SignaturePattern 'void BattleClientEdge::Install\(muduo::net::TcpServer &server\)'
    Assert-Match -Text $install -Pattern '(?s)loopStallTimer\.RunEvery\(tickSeconds, \[weak\].{0,300}?const bool late = self->loopStall\.Tick\(now\);\s*self->CloseExpiredHandshakes\(now, late\);' `
        -Because 'loop 卡顿的节拍由 1 秒定时器打,只绑 weak 自己;同一拍扫握手期限'
}

Test-Case '按来源限速:缺省关闭,只在承压时、只对公网来源问前缀的桶,放行之后才记账' {
    Assert-Match -Text $script:AdmissionHeader -Pattern 'inline constexpr const char \*kSourceRateEnv = "BATTLE_SOURCE_RATE_PER_SECOND";' -Because '开关变量名是运维手册里写的那个'
    $admit = Get-FunctionBody -Text $script:AdmissionSource -SignaturePattern 'Verdict Admission::Admit\(.*?\)'
    Assert-Match -Text $admit -Pattern 'sourceRate\.has_value\(\) &&\s*accept_admission::SourceRateUnderPressure\(unauthenticated, preAuthBudget, lastAcceptRateRejectAt, now\)' `
        -Because '开了也只在承压时生效,承压的判据与 gate 同一份'
    Assert-Match -Text $admit -Pattern 'sourceRateActive \? accept_admission::RateLimitedSourceOf\(peer\) : std::nullopt' -Because '只对公网源地址按来源限速:边缘 nginx、LB、集群内的连接在 accept 时只看得到内网地址'
    Assert-Match -Text $admit -Pattern 'lastAcceptRateRejectAt = now;' -Because '全局速率闸拒过连接也算承压'
    $decideAt = $admit.IndexOf('Decide(state')
    $takeAt = $admit.IndexOf('sourceRate->Take(')
    Assert-True -Condition ($decideAt -ge 0 -and $takeAt -gt $decideAt) -Because '前缀的桶在所有闸都放行之后才记账,被别的闸拒掉的连接不消耗它的名额'
    Assert-Match -Text $script:AdmissionHeader -Pattern '(?s)if \(state\.sourceOverRate\)\s*\{\s*return Verdict\{\.rejected = RejectReason::kSourceRate\};' -Because '来源超速排在预算之前:超速前缀不会让别人的老连接被淘汰'
}

Test-Case 'battle main.cpp:启动时配速率闸参数(写错拒启),关源时同步关准入闸' {
    Assert-Match -Text $script:BattleMain -Pattern '(?s)context\.clientEdge = std::make_shared<BattleClientEdge>\(\);\s*ConfigureClientAdmissionOrDie\(\*context\.clientEdge\);' `
        -Because '直连面建出来就定下速率闸参数,早于 SetAfterStart 里的 Install'
    Assert-Match -Text $script:BattleMain -Pattern 'battle_connection_admission::ResolveRateLimit\(' -Because '速率闸参数经 ResolveRateLimit 解析(非法值拒启)'
    $configure = Get-FunctionBody -Text $script:BattleMain -SignaturePattern 'void ConfigureClientAdmissionOrDie\(BattleClientEdge &edge\)'
    Assert-Match -Text $configure -Pattern 'LOG_FATAL << "Refusing to start: " << error;' -Because '两个环境变量写错时拒绝启动,不带着一个没人知道的速率上限跑'
    Assert-Match -Text $configure -Pattern 'battle_connection_admission::ResolvePreAuthBudget\(' -Because '未验签预算的覆盖变量经 ResolvePreAuthBudget 解析(非法值拒启)'
    Assert-Match -Text $configure -Pattern 'battle_connection_admission::ResolveSourceRateLimit\(' -Because '按来源限速的开关经 ResolveSourceRateLimit 解析(非法值拒启)'
    Assert-Match -Text $configure -Pattern 'edge\.ConfigureAdmission\(\*rateLimit, \*preAuthBudget, sourceRateLimit\);' -Because '解析出的参数要真的交给直连面'
    Assert-Match -Text $script:BattleMain -Pattern '(?s)auto closeSource = \[&context\]\(Node &n\).{0,600}?context\.clientEdge->CloseSource\(\);' `
        -Because '关源时 accept 处的准入闸要一起关,否则停机期间新直连照样建出来再被关'
}

Test-Case 'battle-connection-alerts.yaml:PrometheusRule 形状,battle.connections 组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*battle-connection-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'battle.connections'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/ops/gate-kernel-tuning-runbook\.md[ ]*$' -Because "$($rule.Name) 指向连接准入的运维手册"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[mh][ ]*$' -Because "$($rule.Name) 要写 for"
    }
}

Test-Case 'battle-connection-alerts.yaml:引用的指标都由 C++ 导出,reason 取值都在导出的取值里' {
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'battle.connections'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\bbattle_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 battle_* 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($null -ne (Resolve-MetricFamily -Name $metric)) -Because "$($rule.Name):$metric 必须由 battle_connection_admission.cpp 导出(改名后告警静默失效)"
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

exit (Complete-TestRun -SuiteName 'battle_connection_alerts_contract')
