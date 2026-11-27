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
      - gate 的 main.cpp 确实把准入回调装到了 TcpServer 上、登记了指标、启动时配了参数、关源时同步关了准入闸,
        关源时未验签连接一起关;
      - 票据先行(设计稿 P2-4):建连只登记未验签记录、不发号不建会话;票据全部校验通过之后才发号建会话;
        断开回调第一步摘掉未验签记录就返回;握手期限扫未验签表;
      - 未验签预算(设计稿 P0-2):淘汰未验签表的表头(按代次有序,最老),先摘记录再经 weak 连接 forceClose、不删会话,
        预算排在速率闸之前判;
      - 收包路径确实接上了登录过渡闸与未授权消息号闸(2026-11-26):这三处少接一行编译照过,闸直接失效;
      - 指标标签只有 reason 与 result 两个封闭集合:对端地址、会话号、账号、玩家号不进指标(AGENTS.md §9);
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
$script:HeartbeatSource = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/client_heartbeat.cpp'
$script:SessionManagerHeader = Read-RepoText -RelativePath 'cpp/libs/services/gate/session/manager/session_manager.h'
# 与 battle 直连共用的纯逻辑(速率闸、未验签预算、按来源前缀限速)。
$script:SharedAdmissionHeader = Read-RepoText -RelativePath 'cpp/libs/engine/core/network/accept_admission.h'

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
    'GatePreauthBudgetRejecting'         = 'warning'
    'GateSourceRateLimited'              = 'warning'
    'GateSourceTableFull'                = 'warning'
}

Test-Case '夹具:CollectMetrics 导出的指标族与类型,拒绝原因的取值' {
    $actual = @($script:Families.GetEnumerator() | ForEach-Object { '{0}:{1}' -f $_.Key, $_.Value }) -join ','
    Assert-Equal -Expected 'gate_connections_current:gauge,gate_connections_max:gauge,gate_unauthenticated_connections:gauge,gate_accepted_total:counter,gate_rejected_total:counter,gate_accept_rate_limit_per_second:gauge,gate_accept_burst:gauge,gate_handshake_duration_ms:histogram,gate_preauth_reaped_total:counter,gate_preauth_evicted_total:counter,gate_preauth_budget:gauge,gate_preauth_early_drop_start:gauge,gate_preauth_evictable_age_ms:gauge,gate_source_rate_limit_per_second:gauge,gate_source_burst:gauge,gate_source_rate_active:gauge,gate_source_tracked:gauge,gate_source_untracked_total:counter,gate_websocket_handshakes_total:counter,gate_websocket_protocol_closed_total:counter' `
        -Actual $actual -Because 'CollectMetrics 的指标族与类型;增删指标要同步告警、设计稿 P0-2 / P1-4 与本用例(websocket 两族见 network/codec/websocket.h)'
    Assert-Equal -Expected 'capacity,accept_rate,shutting_down,not_ready,no_token_secret,preauth_budget,preauth_early_drop,source_rate' -Actual ($script:Reasons -join ',') -Because 'gate_rejected_total 的 reason 取值(顺序即 RejectReason 的枚举顺序)'
    foreach ($family in $script:Families.Keys) {
        if ($script:Families[$family] -ceq 'counter') {
            Assert-Match -Text $family -Pattern '_total$' -Because "$family 是 counter,名字以 _total 结尾(Prometheus 命名约定)"
        }
    }
}

Test-Case '指标标签只有 reason 与 result:对端地址、会话号、账号、玩家号不进指标' {
    $labels = @([regex]::Matches($script:AdmissionSource, 'Sample\(\{\{"(?<l>[a-z_]+)"') | ForEach-Object { $_.Groups['l'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'reason,result' -Actual ($labels -join ',') -Because '两个标签都是封闭集合(拒绝原因、websocket 握手结果);标签基数由攻击者控制时,连接层的洪水会变成监控系统的洪水(AGENTS.md §9)'
    $collect = [regex]::Match($script:AdmissionSource, '(?ms)^void CollectMetrics\(prometheus_text::Writer &writer\)\n\{\n(?<body>.*?)^\}')
    Assert-True -Condition $collect.Success -Because '找得到 CollectMetrics 的函数体'
    Assert-NotMatch -Text $collect.Groups['body'].Value -Pattern 'toIp|peer|sessionId|playerId|account' -Because '采集函数里不出现对端地址、会话号、玩家号与账号'
}

Test-Case 'gate main.cpp:准入回调装在 TcpServer 上,指标已登记,启动时配参数,关源时同步关准入闸' {
    Assert-Match -Text $script:GateMain -Pattern '(?m)^\s*ConfigureConnectionAdmissionOrDie\(\);$' -Because '启动时要定下上限与速率闸参数'
    Assert-Match -Text $script:GateMain -Pattern '(gate_connection_admission|admission)::ResolveRateLimit\(' -Because '速率闸参数经 ResolveRateLimit 解析(非法值拒启)'
    Assert-Match -Text $script:GateMain -Pattern '(gate_connection_admission|admission)::ResolvePreAuthBudget\(' -Because '未验签预算的覆盖变量经 ResolvePreAuthBudget 解析(非法值拒启)'
    Assert-Match -Text $script:GateMain -Pattern '(gate_connection_admission|admission)::ResolveSourceRateLimit\(' -Because '按来源限速的开关经 ResolveSourceRateLimit 解析(非法值拒启)'
    Assert-Match -Text $script:GateMain -Pattern '(gate_connection_admission|admission)::Configure\(configuredMaxConnections, \*rateLimit, \*preAuthBudget, sourceRateLimit\);' `
        -Because '解析出的三组参数都要真的交给准入模块'
    Assert-Match -Text $script:GateMain -Pattern 'LOG_FATAL << "Refusing to start: " << error;' -Because '两个环境变量写错时拒绝启动,不带着一个没人知道的速率上限跑'
    Assert-Match -Text $script:GateMain -Pattern '(?m)^\s*gate_connection_admission::RegisterMetrics\(\);$' -Because '不登记采集回调,这些序列根本不存在'
    Assert-Match -Text $script:GateMain -Pattern '(?s)n\.GetTcpServer\(\)\.setAdmissionCallback\(\s*\[\]\(const muduo::net::InetAddress& peer\) \{\s*return gate_connection_admission::AdmitConnection\(peer, std::chrono::steady_clock::now\(\)\);' `
        -Because '准入回调不捕获任何对象(AGENTS.md §11.7),判定用单调时钟'
    $admissionAt = $script:GateMain.IndexOf('n.GetTcpServer().setAdmissionCallback(')
    $connectionAt = $script:GateMain.IndexOf('n.GetTcpServer().setConnectionCallback(')
    $nodeIdAt = $script:GateMain.IndexOf('tlsSessionManager.session_id_gen().set_node_id(n.GetNodeId());')
    Assert-True -Condition ($nodeIdAt -ge 0 -and $admissionAt -gt $nodeIdAt -and $connectionAt -gt $admissionAt) -Because '顺序:先种 session_id 的 node 段,再装准入回调,再装连接回调'
    Assert-Match -Text $script:GateMain -Pattern '(?s)context\.sourceClosed = true;.{0,400}?gate_connection_admission::CloseSource\(\);' -Because '关源时 accept 处的准入闸要一起关,否则停机期间新连接照样建出来再被关'
    Assert-Match -Text $script:GateMain -Pattern '(?s)context\.sourceClosed = true;.{0,2000}?for \(auto &\[incarnation, preAuth\] : preAuthConnections\)\s*\{\s*if \(const auto conn = preAuth\.conn\.lock\(\)\)\s*\{\s*conn->forceClose\(\);' `
        -Because '还没验签的连接不在会话表里(票据先行),停机 / 维护关源时要单独一起关,只经记录里的 weak 连接'
}

Test-Case '票据先行:建连只登记未验签记录,票据校验全部通过之后才发号建会话,未验签断开只摘记录' {
    $established = [regex]::Match($script:SessionHandler, '(?ms)^void GateClientSessionHandler::HandleConnectionEstablished\(.*?^\}')
    Assert-True -Condition $established.Success -Because '找得到 HandleConnectionEstablished 的函数体'
    Assert-NotMatch -Text $established.Value -Pattern 'session_id_gen\(\)\.Generate\(\)|sessions\(\)\.emplace|sessions\(\)\[' -Because '建连时不发 session_id、不建 SessionInfo(会话表与发号器挡在票据之后)'
    Assert-Match -Text $established.Value -Pattern 'gate_connection_admission::OpenPreAuthConnection\(conn, establishedAt\);' -Because '还没验签的连接只登记一条未验签记录'
    Assert-Match -Text $established.Value -Pattern '(?s)TokenSecretVerdict::kDevBypass\).{0,1200}?gate_connection_admission::OpenVerifiedSession\(conn, std::string\{\}, establishedAt\);' `
        -Because 'dev 免验签仍然建连即建会话(与验签成功同一个入口)'
    Assert-Match -Text $established.Value -Pattern 'tlsSessionManager\.ClientConnectionCount\(\) >= effectiveMaxConnections' -Because '第二道总量闸按 会话数 + 未验签连接数 判'
    foreach ($reason in @('kCapacity', 'kNotReady', 'kNoTokenSecret')) {
        Assert-Match -Text $script:SessionHandler -Pattern ('gate_connection_admission::NoteRejectedAfterAccept\(gate_connection_admission::RejectReason::{0}\);' -f $reason) `
            -Because "连接回调里的 $reason 分支要计进 gate_rejected_total"
    }
    $verify = [regex]::Match($script:SessionHandler, '(?ms)^void GateClientSessionHandler::DispatchTokenVerify\(.*?^\}')
    Assert-True -Condition $verify.Success -Because '找得到 DispatchTokenVerify 的函数体'
    $hmacAt = $verify.Value.IndexOf('rejectAndClose("token_hmac_mismatch"')
    $expiryAt = $verify.Value.IndexOf('rejectAndClose("token_expired"')
    $openAt = $verify.Value.IndexOf('gate_connection_admission::OpenVerifiedSession(conn, clientSigStr, verifiedAt);')
    Assert-True -Condition ($hmacAt -ge 0 -and $expiryAt -gt $hmacAt -and $openAt -gt $expiryAt) -Because '签名、载荷、节点、区、期限全部校验通过之后才发号建会话'
    Assert-Match -Text $verify.Value -Pattern '(?s)OpenVerifiedSession\(conn, clientSigStr, verifiedAt\);\s*if \(!sessionId\)\s*\{\s*rejectAndClose\("token_in_use"' -Because '票据被另一条活着的连接占着时不建会话,按 token_in_use 拒绝'
    $disconnect = [regex]::Match($script:SessionHandler, '(?ms)^void GateClientSessionHandler::HandleConnectionDisconnection\(.*?^\}')
    Assert-True -Condition $disconnect.Success -Because '找得到 HandleConnectionDisconnection 的函数体'
    $forgetAt = $disconnect.Value.IndexOf('if (gate_connection_admission::ForgetPreAuthConnection(conn))')
    $sceneAt = $disconnect.Value.IndexOf('NotifySceneExitGame(')
    $loginAt = $disconnect.Value.IndexOf('gate_login_renewal::NoteConnectionClosed(')
    Assert-True -Condition ($forgetAt -ge 0 -and $sceneAt -gt $forgetAt -and $loginAt -gt $forgetAt) -Because '未验签断开在第一步摘记录就返回,不通知 scene / login'
    Assert-Match -Text $script:AdmissionSource -Pattern '(?s)void OpenPreAuthConnection\(.*?preAuthConnections\(\)\.emplace\(incarnation, PreAuthConnection\{\.conn = conn, \.connectedAt = now\}\);\s*conn->setContext\(PreAuthConnectionKey\{incarnation\}\);' `
        -Because '未验签记录只有 weak 连接、建连时刻与代次(键),连接上下文换成它的键'
    Assert-Match -Text $script:AdmissionSource -Pattern '(?s)std::optional<SessionId> OpenVerifiedSession\(.*?IssueSessionId\(\);.*?ClaimTicket\(ticketKey, sessionId, incarnation\).*?return std::nullopt;.*?preAuthConnections\.erase\(it\);.*?conn->setContext\(sessionId\);' `
        -Because '发号 → 占票据(被占就不建会话)→ 摘未验签记录 → 换上下文、建会话'
    Assert-Match -Text $script:AdmissionSource -Pattern '(?s)SessionId IssueSessionId\(\).*?while \(sessionId == kInvalidSessionId \|\| sessions\.contains\(sessionId\)\)' -Because '发号的保留哨兵与回绕碰撞检查照旧,只是挪到验签成功时'
    Assert-Match -Text $script:HeartbeatSource -Pattern 'const uint64_t deferred = result\.deferred \+ CloseExpiredHandshakes\(now, result\.late\);' -Because '握手期限扫未验签表,本端 loop 卡过的那一轮只推迟'
    Assert-Match -Text $script:HeartbeatSource -Pattern '(?s)uint64_t CloseExpiredHandshakes\(.*?it = preAuthConnections\.erase\(it\);.*?conn->forceClose\(\);' -Because '握手超时先摘记录再关连接,不等断开回调'
    Assert-Match -Text $script:AdmissionSource -Pattern 'gate_client_heartbeat::GetCounters\(\)\.handshakeTimeouts' -Because 'gate_preauth_reaped_total 与握手超时的汇总日志读同一份计数'
}

Test-Case '未验签预算:淘汰未验签表的表头,先摘记录再经 weak 连接 forceClose、不删会话,预算排在速率闸之前' {
    Assert-NotMatch -Text $script:AdmissionSource -Pattern 'sessions\(\)\.erase|sessions\.erase' -Because '淘汰不在 muduo 的回调栈上同步摘会话;未验签连接本来就没有会话'
    Assert-Match -Text $script:AdmissionSource -Pattern '(?s)void EvictOldestUnauthenticated\(.*?preAuthConnections\.begin\(\);.*?oldest->second\.conn\.lock\(\);.*?preAuthConnections\.erase\(oldest\);.*?conn->forceClose\(\);' `
        -Because '先摘记录(这一刻起不再占未验签池),再经记录里的 weak 连接 forceClose(AGENTS.md §11.7)'
    Assert-Match -Text $script:SessionManagerHeader -Pattern 'using PreAuthConnectionMap = std::map<uint64_t, PreAuthConnection>;' `
        -Because '未验签表按代次有序:代次建连时单调分配,begin() 就是最老的一条;表里只有还没验签的连接'
    Assert-Match -Text $script:AdmissionSource -Pattern '\.currentConnections = tlsSessionManager\.ClientConnectionCount\(\),' -Because '准入回调的总量按 会话数 + 未验签连接数 判'
    $decide = [regex]::Match($script:AdmissionHeader, '(?ms)^inline Admission Decide\(.*?^\}')
    Assert-True -Condition $decide.Success -Because '找得到 Decide 的函数体'
    $preAuthAt = $decide.Value.IndexOf('DecidePreAuth(')
    $rateAt = $decide.Value.IndexOf('rateGate.TryTake(now)')
    Assert-True -Condition ($preAuthAt -ge 0 -and $rateAt -gt $preAuthAt) -Because '预算排在速率闸之前判:被预算拒掉的连接不消耗速率名额'
    Assert-Match -Text $script:AdmissionSource -Pattern '!gate_client_heartbeat::LoopRecentlyStalled\(now\)' -Because '本端 loop 刚卡过时不淘汰(验签请求可能还在接收缓冲里没读)'
    Assert-Match -Text $script:GateMain -Pattern 'preauth_evictable_age_ms=' -Because '启动日志打出生效的预算、早拒起点与可淘汰年龄,运维以那一行为准'
}

Test-Case '按来源限速:缺省关闭,只在承压时问前缀的桶,放行之后才记账,指标不带前缀' {
    Assert-Match -Text $script:AdmissionHeader -Pattern 'inline constexpr const char \*kSourceRateEnv = "GATE_SOURCE_RATE_PER_SECOND";' -Because '开关变量名是运维手册里写的那个'
    Assert-Match -Text $script:AdmissionSource -Pattern 'tlsState\.sourceRate\.has_value\(\) && UnderPressure\(unauthenticated, now\)' -Because '开了也只在承压时生效(设计稿 §7「告警后自动开启」)'
    $admit = [regex]::Match($script:AdmissionSource, '(?ms)^bool AdmitConnection\(.*?^\}')
    Assert-True -Condition $admit.Success -Because '找得到 AdmitConnection 的函数体'
    $decideAt = $admit.Value.IndexOf('Decide(state')
    $takeAt = $admit.Value.IndexOf('tlsState.sourceRate->Take(')
    Assert-True -Condition ($decideAt -ge 0 -and $takeAt -gt $decideAt) -Because '前缀的桶在所有闸都放行之后才记账,被别的闸拒掉的连接不消耗它的名额'
    Assert-Match -Text $admit.Value -Pattern 'tlsState\.sourceRateActive \? RateLimitedSourceOf\(peer\) : std::nullopt' -Because '只对公网源地址按来源限速:代理、LB、边缘 nginx 后面的连接在 accept 时只看得到内网地址'
    Assert-Match -Text $script:SharedAdmissionHeader -Pattern 'return SourcePrefix\{false, hostOrderAddress >> 8\};' -Because 'IPv4 按 /24 聚合(core accept_admission.h)'
    Assert-Match -Text $script:SharedAdmissionHeader -Pattern '(?s)for \(size_t i = 0; i < 8; \+\+i\)\s*\{\s*bits = \(bits << 8\) \| address\[i\];' -Because 'IPv6 按 /64 聚合(按 /128 等于没做)'
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

# 判定与处置在 client_entry_admission.h,行为由 GateClientEntryWiring.*(client_entry_admission_test.cpp)测;这里只钉「接上了」:
# 抽出判定之后,调用点少一行不报错、单测照绿(单测自己装钩子、自己调判定),线上两道闸却不生效。
Test-Case 'gate 收包路径:登录过渡闸装进 lane 的发送钩子、直连发出前各判一次,未授权消息号闸在收包入口' {
    $handler = Read-RepoText -RelativePath 'cpp/nodes/gate/handler/rpc/client_session_handler.cpp'
    Assert-Match -Text $handler -Pattern 'gate_lane::SetSendHooks\(&gate_client_entry::LoginTransitionRejectTip,\s*&ApplyLoginTransition\);' -Because '经场景 lane 排队的 LeaveGame / EnterGame 在发出前一刻按登录过渡判拒:少了这一行钩子是空的,一律放行'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($handler, '(?m)^\s*if \(gate_client_entry::RejectUnauthorizedMessageId\(sessionId, session, conn, request->message_id\(\)\)\)').Count) -Because '收包入口按会话已授权的消息号把关(含只能由 gate 合成的号),只有这一处调用'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($handler, '(?m)^\s*if \(gate_client_entry::RejectLoginTransitionBeforeDirectSend\(sessionId, ').Count) -Because '不经 lane 的两条发出路径(直连 gRPC 节点、非 gRPC 协议)各自在真正发出前判一次登录过渡'
}

exit (Complete-TestRun -SuiteName 'gate_connection_alerts_contract')
