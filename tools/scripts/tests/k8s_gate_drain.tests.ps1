#requires -Version 7
<#
.SYNOPSIS
    gate 排空脚本(tools/scripts/k8s_gate_drain.ps1)的契约与流程回归测试。
.DESCRIPTION
    只从脚本 AST 提取函数,不执行脚本入口;入口只做 kubectl 存在性检查后调用 Invoke-GateDrain,
    主流程的顺序与退出码经 Invoke-GateDrain 用同一套模拟驱动。kubectl 全部换成内存模拟的「集群」:
    login ConfigMap、gate Pod / StatefulSet、etcd(etcdctl --write-out=json)与 login 所用的 Redis(redis-cli --no-raw;
    EVAL 按脚本文本认出 MarkScript / ReleaseScript 后按其语义模拟,Lua 本身靠 kind e2e 验证)。
    进程截止用临时 pwsh 夹具验证。不访问集群、Redis、etcd。
.EXAMPLE
    pwsh -File tools/scripts/tests/k8s_gate_drain.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:DrainScript = Join-Path $script:RepoRoot 'tools/scripts/k8s_gate_drain.ps1'
# ConvertFrom-YamlToFlatMap / Get-YamlScalar:与被测脚本 dot-source 的是同一份。
. (Join-Path $script:RepoRoot 'tools/scripts/lib/release_common.ps1')

$parseTokens = $null
$parseErrors = $null
$script:DrainAst = [Management.Automation.Language.Parser]::ParseFile($script:DrainScript, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "排空脚本语法错误: $($parseErrors.Message -join '; ')" }
$script:ProductionFunctions = @{}
foreach ($node in $script:DrainAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    $script:ProductionFunctions[$node.Name] = $node.Body.GetScriptBlock()
}

function Restore-ProductionFunction {
    param([string]$Name)
    if (-not $script:ProductionFunctions.ContainsKey($Name)) { throw "排空脚本缺少函数 $Name" }
    Set-Item -Path "Function:script:$Name" -Value $script:ProductionFunctions[$Name]
}

function Read-RepoText {
    param([string]$RelativePath)
    return [IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))
}

function Get-ThrownMessage {
    param([scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function New-MockResult {
    param([string]$Output = '', [int]$ExitCode = 0, [bool]$TimedOut = $false, [string]$ErrorOutput = '')
    [pscustomobject]@{ ExitCode = $ExitCode; Output = $Output; ErrorOutput = $ErrorOutput; TimedOut = $TimedOut }
}

# 与 k8s_deploy.ps1 生成的 login ConfigMap 同形的最小 login.yaml。Etcd.Hosts 故意指向另一个 namespace:
# login 的 GateWatcher 优先用 Registry.Etcd.Hosts,脚本取错就会 exec 到错的 Service。
function New-LoginYaml {
    param([string]$GateDrainBlock = '', [string]$RedisHost = 'redis.audit-infra:6379', [string]$RedisDbLine = '    DB: 0')
    @"
Name: login.rpc
Etcd:
  Hosts:
    - "etcd.wrong-ns:2379"
  Key: login.rpc
Node:
  ZoneId: 7
  RedisClient:
    Host: $RedisHost
    Password: "p#ss"
$RedisDbLine
Registry:
  Etcd:
    Hosts:
      - "etcd.audit-infra:2379"
    Key: loginservice.rpc
$GateDrainBlock
"@
}

function New-MockGatePod {
    param([string]$Uid = 'uid-old', [string]$PodIp = '10.0.0.11', [bool]$Ready = $true, [bool]$Terminating = $false,
        [string]$OwnerKind = 'StatefulSet', [string]$Name = 'gate-1')
    $metadata = @{ name = $Name; uid = $Uid; ownerReferences = @(@{ kind = $OwnerKind; name = 'gate'; controller = $true }) }
    if ($Terminating) { $metadata.deletionTimestamp = '2026-09-29T00:00:00Z' }
    $status = if ($Ready) { 'True' } else { 'False' }
    @{ metadata = $metadata; spec = @{ terminationGracePeriodSeconds = 30 }
        status = @{ podIP = $PodIp; phase = 'Running'; conditions = @(@{ type = 'Ready'; status = $status }) } } |
        ConvertTo-Json -Depth 8 -Compress
}

# 一条 etcd 记录。值用 C++ MessageToJsonString 的 lowerCamelCase(uint64 写成字符串);-ProtoNames 切到 proto 原名。
# 客户端地址缺省不填(podip 形态);-ClientIp / -ClientPort 模拟 external 形态自报的地址。
function New-EtcdGate {
    param([uint32]$NodeId, [string]$Ip, [string]$Uuid, [uint32]$Players = 0, [int]$ValueNodeId = -1, [switch]$ProtoNames,
        [uint32]$ZoneId = 7, [uint64]$LaunchTime = 100, [string]$ClientIp = '', [uint32]$ClientPort = 0)
    $valueId = if ($ValueNodeId -ge 0) { $ValueNodeId } else { $NodeId }
    $info = if ($ProtoNames) {
        @{ node_id = $valueId; node_type = 4; endpoint = @{ ip = $Ip; port = 18000 }; node_uuid = $Uuid; player_count = $Players
            zone_id = $ZoneId; launch_time = [string]$LaunchTime }
    } else {
        @{ nodeId = $valueId; nodeType = 4; endpoint = @{ ip = $Ip; port = 18000 }; nodeUuid = $Uuid; playerCount = $Players
            zoneId = $ZoneId; launchTime = [string]$LaunchTime }
    }
    if ($Players -eq 0 -and -not $ProtoNames) { $info.Remove('playerCount') }   # protojson 省略零值
    if (-not [string]::IsNullOrEmpty($ClientIp) -or $ClientPort -ne 0) {
        $client = @{}
        if (-not [string]::IsNullOrEmpty($ClientIp)) { $client.ip = $ClientIp }
        if ($ClientPort -ne 0) { $client.port = $ClientPort }
        if ($ProtoNames) { $info.client_endpoint = $client } else { $info.clientEndpoint = $client }
    }
    [pscustomobject]@{ Key = "GateNodeService.rpc/zone/7/node_type/4/node_id/$NodeId"; Value = ($info | ConvertTo-Json -Compress) }
}

function ConvertTo-EtcdJson {
    param([object[]]$Records)
    $kvs = @($Records | Where-Object { $null -ne $_ } | ForEach-Object {
            @{ key = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($_.Key))
                value = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($_.Value)); lease = 42 }
        })
    $reply = @{ header = @{ revision = 9 } }
    if ($kvs.Count -gt 0) { $reply.kvs = $kvs; $reply.count = $kvs.Count }
    return ($reply | ConvertTo-Json -Depth 6 -Compress)
}

# ─── 模拟集群 ───

# 响应队列:剩一条时粘住最后一条(之后的查询都看到它),多于一条时逐条出队。
function Get-NextMock {
    param([Collections.Generic.Queue[string]]$Queue)
    if ($Queue.Count -gt 1) { return $Queue.Dequeue() }
    return $Queue.Peek()
}

function Format-MockRedisValue {
    param([AllowNull()]$Value)
    if ($null -eq $Value) { return '(nil)' }
    return '"' + $Value + '"'
}

# EVAL 按脚本文本认出是生产脚本里的哪一个(MarkScript / ReleaseScript),按其注释写明的语义模拟;
# 这里跑不了 Lua,脚本文本本身由「标记脚本文本」用例钉住关键片段,真 Redis 上的行为靠 kind e2e 验证。
function Invoke-MockEval {
    param([string[]]$Command)
    $contract = Get-GateDrainContract
    $numKeys = [int]$Command[2]
    $evalKeys = @($Command[3..(2 + $numKeys)])
    $argv = @(if ($Command.Count -gt 3 + $numKeys) { $Command[(3 + $numKeys)..($Command.Count - 1)] })
    if ($Command[1] -eq $contract.MarkScript) {
        $free = @($evalKeys | Select-Object -Skip 2 | Where-Object { -not $script:Redis.ContainsKey($_) }).Count
        if ($free -eq 0) { return '"' + $contract.NoUndrainedPeerReply + '"' }
        $script:Redis.Remove($evalKeys[1])
        if ($script:Redis.ContainsKey($evalKeys[0])) { return '(nil)' }
        $script:Redis[$evalKeys[0]] = $argv[0]
        $script:RedisTtl[$evalKeys[0]] = $argv[1]
        return 'OK'
    }
    if ($Command[1] -eq $contract.ReleaseScript) {
        if (-not $script:Redis.ContainsKey($evalKeys[0]) -or $script:Redis[$evalKeys[0]] -ne $argv[0]) { return '(integer) -1' }
        $removed = 0
        foreach ($key in $evalKeys) { if ($script:Redis.ContainsKey($key)) { $script:Redis.Remove($key); $removed++ } }
        return "(integer) $removed"
    }
    throw '未模拟的 EVAL 脚本'
}

# redis-cli --no-raw 的输出形状;只实现脚本用到的命令。MGET 前先跑 OnMget 钩子,模拟 login 的判定循环。
# EVAL 在日志里记成 "EVAL mark|release <numkeys> <keys...> <args...>",不带脚本全文。
function Invoke-MockRedis {
    param([string[]]$Command)
    $logLine = $Command -join ' '
    if ($Command[0] -eq 'EVAL') {
        $contract = Get-GateDrainContract
        $name = if ($Command[1] -eq $contract.MarkScript) { 'mark' } elseif ($Command[1] -eq $contract.ReleaseScript) { 'release' } else { '<unknown>' }
        $logLine = "EVAL $name " + (@($Command[2..($Command.Count - 1)]) -join ' ')
    }
    $script:RedisLog.Add($logLine)
    $keys = if ($Command.Count -gt 1) { @($Command[1..($Command.Count - 1)]) } else { @() }
    switch ($Command[0]) {
        'DEL' {
            $removed = 0
            foreach ($key in $keys) { if ($script:Redis.ContainsKey($key)) { $script:Redis.Remove($key); $removed++ } }
            return "(integer) $removed"
        }
        'TIME' { return "1) `"$($script:RedisNow)`"`n2) `"123456`"" }
        'EVAL' { return Invoke-MockEval -Command $Command }
        'TTL' {
            if (-not $script:Redis.ContainsKey($Command[1])) { return '(integer) -2' }
            if ($script:RedisTtl.ContainsKey($Command[1])) { return "(integer) $($script:RedisTtl[$Command[1]])" }
            return '(integer) -1'
        }
        'GET' { return Format-MockRedisValue $script:Redis[$Command[1]] }
        'MGET' {
            $script:MgetCount++
            if ($null -ne $script:OnMget) { & $script:OnMget }
            $lines = for ($i = 0; $i -lt $keys.Count; $i++) { "$($i + 1)) $(Format-MockRedisValue $script:Redis[$keys[$i]])" }
            return ($lines -join "`n")
        }
    }
    throw "未模拟的 Redis 命令: $($Command -join ' ')"
}

function Reset-DrainFixture {
    foreach ($name in @($script:ProductionFunctions.Keys)) { Restore-ProductionFunction $name }
    $script:KubeContext = 'kind-mmorpg'
    $script:KubeConfig = ''
    $script:Calls = [Collections.Generic.List[object]]::new()
    $script:Events = [Collections.Generic.List[string]]::new()
    $script:RedisLog = [Collections.Generic.List[string]]::new()
    $script:Redis = @{}
    $script:RedisTtl = @{}
    $script:RedisNow = '1790000000'
    $script:RedisFailures = 0
    $script:MgetCount = 0
    $script:OnMget = $null
    $script:RedisExec = $null
    $script:EtcdExec = $null
    $script:LoginYaml = New-LoginYaml
    $script:ConfigMapExitCode = 0
    $script:PodResponses = [Collections.Generic.Queue[string]]::new()
    $script:PodResponses.Enqueue((New-MockGatePod))
    $script:EtcdResponses = [Collections.Generic.Queue[string]]::new()
    $script:EtcdResponses.Enqueue((ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'uuid-a' -Players 5),
                (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'uuid-b'))))
    $script:StsJson = @{ spec = @{ replicas = 2 }; status = @{ readyReplicas = 2 } } | ConvertTo-Json -Compress

    # 最后一层防护:意外绕过封装也只能失败,不会连接真实集群;睡眠换成空操作,预算仍按真实单调时钟走。
    Set-Item Function:script:kubectl { throw '测试禁止执行真实 kubectl' }
    Set-Item Function:script:Start-Sleep { param([int]$Milliseconds, [double]$Seconds) }
    Set-Item Function:script:Invoke-GateDrainKubectl {
        param([string[]]$Arguments, [double]$TimeoutSeconds = 10)
        $a = @($Arguments)
        $script:Calls.Add([pscustomobject]@{ Args = $a; TimeoutSeconds = $TimeoutSeconds })
        if ($a[0] -eq 'get' -and $a[1] -eq 'configmap') {
            $script:Events.Add('get:configmap')
            if ($script:ConfigMapExitCode -ne 0) { return New-MockResult -ExitCode $script:ConfigMapExitCode -ErrorOutput 'forbidden' }
            return New-MockResult -Output (@{ data = @{ 'login.yaml' = $script:LoginYaml } } | ConvertTo-Json -Compress)
        }
        if ($a[0] -eq 'get' -and $a[1] -eq 'pod') { $script:Events.Add('get:pod'); return New-MockResult -Output (Get-NextMock $script:PodResponses) }
        if ($a[0] -eq 'get' -and $a[1] -eq 'statefulset') { $script:Events.Add('get:sts'); return New-MockResult -Output $script:StsJson }
        if ($a[0] -eq 'delete') { $script:Events.Add("delete:$($a[1]):$($a[2])"); return New-MockResult }
        if ($a[0] -eq 'exec') {
            $tail = @($a[([Array]::IndexOf($a, '--') + 1)..($a.Count - 1)])
            if ($tail[0] -eq 'etcdctl') {
                $script:Events.Add('etcd:get')
                $script:EtcdExec = $a
                return New-MockResult -Output (Get-NextMock $script:EtcdResponses)
            }
            if ($tail[0] -eq 'sh') {
                $script:RedisExec = $a
                if ($script:RedisFailures -gt 0) { $script:RedisFailures--; return New-MockResult -ExitCode 1 -ErrorOutput 'Could not connect to Redis' }
                $command = @($tail[7..($tail.Count - 1)])
                $script:Events.Add("redis:$($command[0])")
                return New-MockResult -Output (Invoke-MockRedis -Command $command)
            }
        }
        throw "未模拟的 kubectl 调用: $($a -join ' ')"
    }
}

# 用真实入口的定位结果形状构造 Login / Pod / Record,供流程用例直接驱动。
function Get-FixtureContext {
    $login = ConvertFrom-LoginDrainConfig -Text $script:LoginYaml -Source 'audit/go-svc-login-config'
    $pod = Get-GateStatefulSetPod -Namespace 'mmorpg-zone-e2e' -Ordinal 1
    $record = Select-GateNodeRecord -Records (Get-GateEtcdRecords -Login $login) -PodIp $pod.PodIp -Prefix (Get-GateEtcdPrefix -ZoneId $login.ZoneId)
    return [pscustomobject]@{ Login = $login; Pod = $pod; Record = $record }
}

# ─── 用例 ───

Write-Host '=== gate 排空脚本:契约与入口校验 ==='

Test-Case '参数块:必备参数齐全,TTL 与等待预算默认 1800' {
    $params = @{}
    foreach ($p in $script:DrainAst.ParamBlock.Parameters) { $params[$p.Name.VariablePath.UserPath] = $p }
    foreach ($name in @('ZoneName', 'Ordinal', 'DrainTtlSeconds', 'WaitTimeoutSeconds', 'DeletePod', 'DryRun', 'KubeContext', 'KubeConfig')) {
        Assert-True -Condition $params.ContainsKey($name) -Because "缺少参数 -$name"
    }
    Assert-Equal -Expected 1800 -Actual $params['DrainTtlSeconds'].DefaultValue.Value -Because 'TTL 默认 1800s,须大于 login 默认 Deadline 25m'
    Assert-Equal -Expected 1800 -Actual $params['WaitTimeoutSeconds'].DefaultValue.Value -Because '等待预算默认 1800s'
}

Test-Case '脚本入口:只检查 kubectl 后调用 Invoke-GateDrain,以其返回值作退出码' {
    $statements = @($script:DrainAst.EndBlock.Statements | Where-Object { $_ -isnot [Management.Automation.Language.FunctionDefinitionAst] })
    Assert-Equal -Expected 6 -Actual $statements.Count -Because '函数之外只有 $ErrorActionPreference、$ScriptDir、dot-source 与入口三条,主流程不得漏在入口里(测不到)'
    $tail = @($statements | Select-Object -Last 3 | ForEach-Object { $_.Extent.Text })
    Assert-Match -Text $tail[0] -Pattern '^if \(\$null -eq \(Get-Command kubectl' -Because '先确认 kubectl 存在'
    foreach ($name in @('ZoneName', 'Ordinal', 'DrainTtlSeconds', 'WaitTimeoutSeconds', 'ReadyTimeoutSeconds', 'NamespacePrefix')) {
        Assert-Match -Text $tail[1] -Pattern "(?s)^\`$exitCode = Invoke-GateDrain .*-$name \`$$name\b" -Because "入口必须把 -$name 原样传给 Invoke-GateDrain"
    }
    Assert-Match -Text $tail[1] -Pattern '-DeletePod:\$DeletePod -DryRun:\$DryRun' -Because '两个开关原样透传'
    Assert-Equal -Expected 'exit ([int]$exitCode)' -Actual $tail[2] -Because '以主流程返回值作退出码(3 = 残留标记)'
}

Test-Case '契约字面量与 login / proto / 部署生成器的源头一致' {
    Reset-DrainFixture
    $c = Get-GateDrainContract
    $nodeProto = Read-RepoText 'proto/common/base/node.proto'
    Assert-Match -Text $nodeProto -Pattern ("(?m)^\s*{0}\s*=\s*{1}\s*;" -f $c.GateServiceName, $c.GateNodeType) -Because 'gate 的 node_type 必须与 proto 一致,否则 etcd 前缀对不上'
    $drainGo = Read-RepoText 'go/login/internal/logic/pkg/loginqueue/gatedrain.go'
    Assert-Match -Text $drainGo -Pattern ('GateDrainingKeyFmt\s*=\s*"{0}"' -f [regex]::Escape(($c.DrainingKeyFormat -replace '\{0\}', '%d'))) -Because 'draining 键名必须与 login 一致'
    $monitorGo = Read-RepoText 'go/login/internal/logic/pkg/loginqueue/gatedrain_monitor.go'
    Assert-Match -Text $monitorGo -Pattern ('GateDrainedKeyFmt\s*=\s*"{0}"' -f [regex]::Escape(($c.DrainedKeyFormat -replace '\{0\}', '%d'))) -Because 'drained 键名必须与 login 一致'
    Assert-Match -Text $monitorGo -Pattern ('DrainReasonBelowThreshold\s*=\s*"{0}"' -f $c.DrainedReasonBelowThreshold) -Because '判定理由 below_threshold'
    Assert-Match -Text $monitorGo -Pattern ('DrainReasonDeadline\s*=\s*"{0}"' -f $c.DrainedReasonDeadline) -Because '判定理由 deadline'
    $configGo = Read-RepoText 'go/login/internal/config/config.go'
    $interval = [regex]::Match($configGo, 'json:"Interval,default=(?<d>[^"]+)"')
    $deadline = [regex]::Match($configGo, 'json:"Deadline,default=(?<d>[^"]+)"')
    Assert-True -Condition ($interval.Success -and $deadline.Success) -Because 'GateDrainConf 的 default 标签必须还在'
    Assert-Equal -Expected $c.DefaultMonitorIntervalSeconds -Actual (ConvertFrom-GoDurationSeconds -Text $interval.Groups['d'].Value -Name 'Interval') -Because '缺省判定周期与 login 结构体一致'
    Assert-Equal -Expected $c.DefaultDeadlineSeconds -Actual (ConvertFrom-GoDurationSeconds -Text $deadline.Groups['d'].Value -Name 'Deadline') -Because '缺省期限与 login 结构体一致'
    $nodeGo = Read-RepoText 'go/login/internal/logic/pkg/node/node.go'
    Assert-Match -Text $nodeGo -Pattern '"%s\.rpc"' -Because 'GetRpcPrefix 形状变了要同步 Get-GateEtcdPrefix'
    Assert-Match -Text $nodeGo -Pattern '"%s/zone/%d/node_type/%d/"' -Because 'BuildRpcPrefix 形状变了要同步 Get-GateEtcdPrefix'
    Assert-Equal -Expected 'GateNodeService.rpc/zone/7/node_type/4/' -Actual (Get-GateEtcdPrefix -ZoneId 7) -Because '前缀与 login GateWatcher 相同'
    $deployText = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    Assert-Match -Text $deployText -Pattern ('(?m)^\s*login\s*=\s*@\{{\s*ConfigMap\s*=\s*"{0}".*ConfigFile\s*=\s*"{1}"' -f $c.LoginConfigMap, [regex]::Escape($c.LoginConfigKey)) -Because 'login ConfigMap 名与数据键来自部署目录'
    $ttl = Get-YamlScalar -Path (Join-Path $script:RepoRoot 'bin/etc/base_deploy_config.yaml') -KeyPath 'Etcd.NodeTTLSeconds'
    Assert-True -Condition ($ttl.Found -and $c.StaleRecordWaitSeconds -gt [int]$ttl.Value) -Because '等旧记录消失的上限必须覆盖 C++ 节点租约 TTL(强杀后租约自然过期)'
}

Test-Case '同 zone 候选口径与 login 源头一致(Select-GateDrainPeers 镜像的每一条)' {
    Reset-DrainFixture
    $drainGo = Read-RepoText 'go/login/internal/logic/pkg/loginqueue/gatedrain.go'
    Assert-Match -Text $drainGo -Pattern '(?s)if len\(kept\) == 0 \{.*?return candidates\s*\}' -Because 'login 在候选全部 draining 时忽略标记照常分配 —— 这正是脚本必须先确认还有别的候选的原因'
    $svcGo = Read-RepoText 'go/login/internal/svc/servicecontext.go'
    Assert-Match -Text $svcGo -Pattern 'FilterDrainingGates\(out, loginqueue\.DrainingGates\(ctx, rdb, out\)\)' -Because '排空过滤作用在 buildGateCandidates 的结果上'
    Assert-Match -Text $svcGo -Pattern 'if n == nil \|\| n\.Endpoint == nil \{\s*continue' -Because '缺 endpoint 的记录不进候选'
    Assert-Match -Text $svcGo -Pattern 'if zoneID != 0 && n\.ZoneId != zoneID \{\s*continue' -Because '只算本 zone 的 gate'
    Assert-Match -Text $svcGo -Pattern 'clientendpoint\.Select\(\s*n\.GetClientEndpoint\(\)\.GetIp\(\), n\.GetClientEndpoint\(\)\.GetPort\(\),\s*n\.Endpoint\.Ip, n\.Endpoint\.Port, require\)' -Because '地址选择规则'
    Assert-Match -Text $svcGo -Pattern 'clientendpoint\.DedupeNewest\(' -Because '按地址去重'
    $selectGo = Read-RepoText 'go/shared/clientendpoint/select.go'
    Assert-Match -Text $selectGo -Pattern 'const maxPort = 65535' -Because '端口上限'
    Assert-Match -Text $selectGo -Pattern 'return host != "" && port >= 1 && port <= maxPort' -Because '可用地址判据'
    $dedupeGo = Read-RepoText 'go/shared/clientendpoint/dedupe.go'
    Assert-Match -Text $dedupeGo -Pattern 'if !seen \|\| launchTime\(item\) > launchTime\(items\[j\]\)' -Because '严格大于才替换,并列时先出现者胜'
    $configGo = Read-RepoText 'go/login/internal/config/config.go'
    Assert-Match -Text $configGo -Pattern 'RequireClientEndpoint\s+bool\s+`json:"RequireClientEndpoint,default=false"`' -Because '顶层键,缺省 false'
    $monitorGo = Read-RepoText 'go/login/internal/logic/pkg/loginqueue/gatedrain_monitor.go'
    Assert-Match -Text $monitorGo -Pattern 'for _, g := range gates \{\s*markedAtRaw, err := rdb\.Get\(ctx, gateDrainingKey\(g\.NodeID\)\)' -Because 'login 只评估快照里的 node_id:进程没了,drained 永远不会来(脚本据此中途复核进程身份)'
}

Test-Case '标记 / 撤回脚本文本:检查与写入同在一个 EVAL,撤回按值比对' {
    Reset-DrainFixture
    $c = Get-GateDrainContract
    Assert-Match -Text $c.MarkScript -Pattern "for i=3,#KEYS do if redis\.call\('EXISTS',KEYS\[i\]\)==0" -Because '逐个看其它候选的 draining'
    Assert-Match -Text $c.MarkScript -Pattern "if free==0 then return '$($c.NoUndrainedPeerReply)' end" -Because '全部在排空时什么都不写'
    $check = $c.MarkScript.IndexOf("return '$($c.NoUndrainedPeerReply)'")
    Assert-True -Condition ($check -ge 0 -and $check -lt $c.MarkScript.IndexOf("redis.call('DEL',KEYS[2])")) -Because '拒绝判定必须在第一处写之前'
    Assert-Match -Text $c.MarkScript -Pattern "redis\.call\('SET',KEYS\[1\],ARGV\[1\],'NX','EX',ARGV\[2\]\)$" -Because '最后 SET NX EX,已存在返回 nil'
    Assert-Match -Text $c.ReleaseScript -Pattern "^if redis\.call\('GET',KEYS\[1\]\)==ARGV\[1\] then return redis\.call\('DEL',KEYS\[1\],KEYS\[2\]\) end return -1$" -Because '只删本次写入的值'
}

Test-Case '部署生成器的真实 login 模板能被解析出同一组定位参数' {
    Reset-DrainFixture
    $deployText = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    $template = [regex]::Match($deployText, '(?s)"login"\s*\{\s*@"\r?\n(?<body>.*?)\r?\n"@')
    Assert-True -Condition $template.Success -Because '找不到 New-GoSvcConfigMapYaml 的 login 模板'
    $body = $template.Groups['body'].Value -replace '\$\{InfraNamespace\}', 'audit-infra'
    # RequireClientEndpoint 由生成器按 -ClientEntryMode 填 true / false;通用的 "变量 → 1" 替换会把它变成非法值。
    $body = $body -replace '(?m)^(\s*RequireClientEndpoint:\s*)\S.*$', '${1}true'
    $body = $body -replace '\$\{[A-Za-z_][A-Za-z0-9_]*\}', '1' -replace '\$\([^()]*\)', '' -replace '\$[A-Za-z_][A-Za-z0-9_]*', ''
    $login = ConvertFrom-LoginDrainConfig -Text $body -Source 'k8s_deploy login 模板'
    Assert-Equal -Expected 'redis' -Actual $login.Redis.Service -Because 'Redis 来自 Node.RedisClient.Host'
    Assert-Equal -Expected 'audit-infra' -Actual $login.Redis.Namespace -Because 'Redis 在 infra namespace'
    Assert-Equal -Expected 'etcd' -Actual $login.Etcd.Service -Because 'etcd 来自 Registry.Etcd.Hosts'
    Assert-Equal -Expected 'audit-infra' -Actual $login.Etcd.Namespace -Because 'etcd 在 infra namespace'
    Assert-True -Condition ($login.DeadlineSeconds -lt 1800) -Because '生成器产出的 Deadline 必须小于脚本默认 TTL 1800s'
}

Test-Case '全局 kubectl 参数排在子命令之前,exec 的 -- 之后原样保留' {
    Reset-DrainFixture
    $script:KubeConfig = 'C:\kube\cfg'
    $out = Get-GateDrainKubectlArgs -Arguments @('exec', '-n', 'x', 'svc/redis', '--', 'redis-cli', 'GET', 'k') -TimeoutSeconds 2.5
    Assert-Equal -Expected '--context kind-mmorpg --kubeconfig C:\kube\cfg --request-timeout=2500ms exec -n x svc/redis -- redis-cli GET k' -Actual ($out -join ' ') -Because '追加在末尾会被当成容器命令的参数'
    $script:KubeContext = ''
    $script:KubeConfig = ''
    $out = Get-GateDrainKubectlArgs -Arguments @('get', 'pod') -TimeoutSeconds 10
    Assert-Equal -Expected '--request-timeout=10000ms get pod' -Actual ($out -join ' ') -Because '留空不带 --context / --kubeconfig'
}

Test-Case 'Go duration 解析:合法形状按秒返回,非法形状拒绝' {
    Reset-DrainFixture
    $cases = @{ '5s' = 5; '25m' = 1500; '1h30m' = 5400; '1m30.5s' = 90.5; '500ms' = 0.5; '0' = 0; '-5s' = -5 }
    foreach ($key in $cases.Keys) {
        Assert-Equal -Expected $cases[$key] -Actual (ConvertFrom-GoDurationSeconds -Text $key -Name 'X') -Because "解析 $key"
    }
    foreach ($bad in @('5', 'abc', '5 m', '', '1d')) {
        Assert-Match -Text (Get-ThrownMessage { ConvertFrom-GoDurationSeconds -Text $bad -Name 'GateDrain.Deadline' }) -Pattern 'GateDrain.Deadline' -Because "'$bad' 必须拒绝"
    }
}

Test-Case '集群内 Service 地址:只接受 <service>.<namespace>[.svc[.cluster.local]]:<port>' {
    Reset-DrainFixture
    $parsed = Split-ClusterServiceHost -HostPort 'redis.mmorpg-infra.svc.cluster.local:6379' -Name 'R'
    Assert-Equal -Expected 'redis|mmorpg-infra|6379|redis.mmorpg-infra.svc.cluster.local' -Actual ("{0}|{1}|{2}|{3}" -f $parsed.Service, $parsed.Namespace, $parsed.Port, $parsed.Host) -Because '长名拆分'
    $etcd = Split-ClusterServiceHost -HostPort 'http://etcd.mmorpg-infra:2379' -Name 'E'
    Assert-Equal -Expected 'http://etcd.mmorpg-infra:2379' -Actual $etcd.Endpoint -Because 'etcdctl 拿原样地址'
    foreach ($bad in @('10.0.0.5:6379', 'redis:6379', 'redis.ns', 'redis.ns:0', 'redis.ns:70000', 'https://etcd.ns:2379')) {
        Assert-Match -Text (Get-ThrownMessage { Split-ClusterServiceHost -HostPort $bad -Name 'Node.RedisClient.Host' }) -Pattern 'Node.RedisClient.Host' -Because "'$bad' 推不出 Service,必须拒绝"
    }
}

Test-Case '入口校验:TTL 与等待预算必须大于 Deadline 并留出 drained 观察窗口' {
    Reset-DrainFixture
    $c = Get-GateDrainContract
    Assert-Equal -Expected '' -Actual (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -DeadlineSeconds 1500 -MonitorIntervalSeconds 5 }) -Because '默认值必须放行'
    Assert-True -Condition ($c.PollIntervalSeconds * 10 -le (Get-RequiredDrainedWindowSeconds -MonitorIntervalSeconds 5)) -Because '轮询间隔必须明显短于 drained 的存活窗口'
    Assert-Match -Text (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1500 -WaitTimeoutSeconds 1800 -DeadlineSeconds 1500 -MonitorIntervalSeconds 5 }) -Pattern 'DrainTtlSeconds=1500' -Because 'TTL 等于期限时 draining 先过期'
    Assert-Match -Text (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1559 -WaitTimeoutSeconds 1800 -DeadlineSeconds 1500 -MonitorIntervalSeconds 5 }) -Pattern '1560' -Because '窗口不足 60s 也要拒绝'
    Assert-Equal -Expected '' -Actual (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1560 -WaitTimeoutSeconds 1560 -DeadlineSeconds 1500 -MonitorIntervalSeconds 5 }) -Because '恰好留足窗口放行'
    Assert-Match -Text (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1500 -DeadlineSeconds 1500 -MonitorIntervalSeconds 5 }) -Pattern 'WaitTimeoutSeconds=1500' -Because '脚本不能先于期限超时'
    Assert-Match -Text (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1640 -WaitTimeoutSeconds 1800 -DeadlineSeconds 1500 -MonitorIntervalSeconds 60 }) -Pattern '1650' -Because '判定周期变长,窗口按两轮放大'
    Assert-Match -Text (Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -DeadlineSeconds 1500 -MonitorIntervalSeconds 0 }) -Pattern 'Interval' -Because '判定循环关闭时 drained 永远不会出现'
    $message = Get-ThrownMessage { Assert-DrainBudget -DrainTtlSeconds 60 -WaitTimeoutSeconds 60 -DeadlineSeconds 0 -MonitorIntervalSeconds 5 -WarningAction SilentlyContinue }
    Assert-Equal -Expected '' -Actual $message -Because 'Deadline=0 只认人走干净,期限约束不适用(只告警)'
}

# ─── 定位 ───

Write-Host '=== gate 排空脚本:login 配置、Pod 与 etcd 定位 ==='

Test-Case 'login 配置:缺 GateDrain 块取结构体默认值,etcd 取 Registry.Etcd.Hosts' {
    Reset-DrainFixture
    $login = ConvertFrom-LoginDrainConfig -Text (New-LoginYaml) -Source 'audit'
    Assert-Equal -Expected 7 -Actual $login.ZoneId -Because 'Node.ZoneId'
    Assert-Equal -Expected 5 -Actual $login.MonitorIntervalSeconds -Because '缺省判定周期 5s'
    Assert-Equal -Expected 1500 -Actual $login.DeadlineSeconds -Because '缺省期限 25m'
    Assert-Equal -Expected 'etcd.audit-infra:2379' -Actual $login.Etcd.Endpoint -Because 'GateWatcher 优先用 Registry.Etcd.Hosts,不能取到 Etcd.Hosts'
    Assert-Equal -Expected 'redis.audit-infra|6379|0' -Actual ("{0}|{1}|{2}" -f $login.Redis.Host, $login.Redis.Port, $login.RedisDb) -Because '排空判定循环用的是 Node.RedisClient'
}

Test-Case 'login 配置:显式 GateDrain 块按 login 口径换算,判定循环关闭或键缺失即拒绝' {
    Reset-DrainFixture
    $block = "GateDrain:`n  Interval: 2s`n  DrainedBelowPlayers: 3`n  Deadline: 10m"
    $login = ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -GateDrainBlock $block) -Source 'audit'
    Assert-Equal -Expected '2|600|3' -Actual ("{0}|{1}|{2}" -f $login.MonitorIntervalSeconds, $login.DeadlineSeconds, $login.DrainedBelowPlayers) -Because '显式值优先'
    $login = ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -GateDrainBlock "GateDrain:`n  Deadline: 1500ms") -Source 'audit'
    Assert-Equal -Expected 2 -Actual $login.DeadlineSeconds -Because '不足 1s 的正数向上取整,不能截成 0(那是「永不放行」)'
    $login = ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -GateDrainBlock "GateDrain:`n  Deadline: 0") -Source 'audit'
    Assert-Equal -Expected 0 -Actual $login.DeadlineSeconds -Because '0 = 永不因超时放行'
    $failure = Get-ThrownMessage { ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -GateDrainBlock "GateDrain:`n  Interval: 0s") -Source 'audit' }
    Assert-Match -Text $failure -Pattern 'GateDrain.Interval' -Because '判定循环关闭时 drained 永远等不到'
    $failure = Get-ThrownMessage { ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -RedisDbLine '') -Source 'audit' }
    Assert-Match -Text $failure -Pattern 'Node.RedisClient.DB' -Because '缺 DB 不替 login 猜 0'
    $failure = Get-ThrownMessage { ConvertFrom-LoginDrainConfig -Text (New-LoginYaml -RedisHost '10.1.2.3:6379') -Source 'audit' }
    Assert-Match -Text $failure -Pattern 'Node.RedisClient.Host' -Because 'IP 推不出要 exec 的 Service'
}

Test-Case 'login 配置:RequireClientEndpoint 缺省 false,true / false 大小写不敏感,其它值拒绝' {
    Reset-DrainFixture
    Assert-True -Condition (-not (ConvertFrom-LoginDrainConfig -Text (New-LoginYaml) -Source 'audit').RequireClientEndpoint) -Because '缺省同 login default=false'
    Assert-True -Condition (ConvertFrom-LoginDrainConfig -Text ((New-LoginYaml) + "`nRequireClientEndpoint: True") -Source 'audit').RequireClientEndpoint -Because 'external 形态'
    Assert-True -Condition (-not (ConvertFrom-LoginDrainConfig -Text ((New-LoginYaml) + "`nRequireClientEndpoint: false") -Source 'audit').RequireClientEndpoint) -Because 'podip 形态'
    $failure = Get-ThrownMessage { ConvertFrom-LoginDrainConfig -Text ((New-LoginYaml) + "`nRequireClientEndpoint: 1") -Source 'audit' }
    Assert-Match -Text $failure -Pattern 'RequireClientEndpoint' -Because '不认识的值不猜'
}

Test-Case 'login ConfigMap 从 zone namespace 读取,读取失败 fail-closed' {
    Reset-DrainFixture
    $login = Get-LoginDrainContext -ZoneNamespace 'mmorpg-zone-e2e'
    Assert-Equal -Expected 7 -Actual $login.ZoneId -Because '内容来自模拟 ConfigMap'
    Assert-Equal -Expected 'get configmap go-svc-login-config -n mmorpg-zone-e2e -o json' -Actual ($script:Calls[0].Args -join ' ') -Because '必须读 zone namespace 里 login 的 ConfigMap'
    $script:ConfigMapExitCode = 1
    Assert-Match -Text (Get-ThrownMessage { Get-LoginDrainContext -ZoneNamespace 'mmorpg-zone-e2e' }) -Pattern 'login ConfigMap.*forbidden' -Because '读不到配置就不知道 Redis 在哪,不能猜'
}

Test-Case 'gate Pod:只处理 StatefulSet/gate 管的、未终止、有 podIP 的 Pod' {
    Reset-DrainFixture
    $pod = Get-GateStatefulSetPod -Namespace 'mmorpg-zone-e2e' -Ordinal 1
    Assert-Equal -Expected 'gate-1|uid-old|10.0.0.11|30' -Actual ("{0}|{1}|{2}|{3}" -f $pod.Name, $pod.Uid, $pod.PodIp, $pod.GraceSeconds) -Because '正常 Pod'
    $cases = @(
        @{ Json = ''; Pattern = 'podip 形态' },
        @{ Json = (New-MockGatePod -OwnerKind 'ReplicaSet'); Pattern = '不归 StatefulSet/gate' },
        @{ Json = (New-MockGatePod -Terminating $true); Pattern = '正在终止' },
        @{ Json = (New-MockGatePod -PodIp ''); Pattern = '没有 podIP' }
    )
    foreach ($case in $cases) {
        Reset-DrainFixture
        $script:PodResponses.Clear()
        $script:PodResponses.Enqueue($case.Json)
        Assert-Match -Text (Get-ThrownMessage { Get-GateStatefulSetPod -Namespace 'mmorpg-zone-e2e' -Ordinal 1 }) -Pattern $case.Pattern -Because "必须拒绝:$($case.Pattern)"
    }
}

Test-Case 'etcd:按 podIP 唯一定位 node_id,兼容两种字段名,exec 参数取自 login 配置' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    Assert-Equal -Expected '3|uuid-a|5' -Actual ("{0}|{1}|{2}" -f $context.Record.NodeId, $context.Record.Uuid, $context.Record.PlayerCount) -Because 'podIP 10.0.0.11 对应 node_id 3'
    Assert-Equal -Expected 'exec -n audit-infra svc/etcd -- etcdctl --endpoints=etcd.audit-infra:2379 --dial-timeout=3s --command-timeout=5s get GateNodeService.rpc/zone/7/node_type/4/ --prefix --write-out=json' `
        -Actual ($script:EtcdExec -join ' ') -Because '与 login GateWatcher 读同一个 etcd、同一前缀'
    $json = ConvertTo-EtcdJson @((New-EtcdGate -NodeId 9 -Ip '10.0.0.9' -Uuid 'u9' -Players 2 -ProtoNames),
        [pscustomobject]@{ Key = 'GateNodeService.rpc/zone/7/node_type/4/other'; Value = '{}' })
    $records = ConvertFrom-GateEtcdRecords -Json $json -Prefix 'GateNodeService.rpc/zone/7/node_type/4/'
    Assert-Equal -Expected '1|9|10.0.0.9|u9|2' -Actual ("{0}|{1}|{2}|{3}|{4}" -f $records.Count, $records[0].NodeId, $records[0].Ip, $records[0].Uuid, $records[0].PlayerCount) -Because 'proto 原名也要认;非 node_id/<n> 的键跳过'
    $empty = ConvertFrom-GateEtcdRecords -Json (ConvertTo-EtcdJson @()) -Prefix 'GateNodeService.rpc/zone/7/node_type/4/'
    Assert-Equal -Expected 0 -Actual $empty.Count -Because '前缀下没有键时 etcdctl 不输出 kvs'
}

Test-Case 'etcd:0 条、多条、记录损坏一律拒绝' {
    Reset-DrainFixture
    $prefix = 'GateNodeService.rpc/zone/7/node_type/4/'
    $records = ConvertFrom-GateEtcdRecords -Json (ConvertTo-EtcdJson @((New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'b'))) -Prefix $prefix
    Assert-Match -Text (Get-ThrownMessage { Select-GateNodeRecord -Records $records -PodIp '10.0.0.11' -Prefix $prefix }) -Pattern '没有 endpoint.ip == 10.0.0.11' -Because '0 条拿不到 node_id'
    $records = ConvertFrom-GateEtcdRecords -Json (ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a'), (New-EtcdGate -NodeId 9 -Ip '10.0.0.11' -Uuid 'old'))) -Prefix $prefix
    Assert-Match -Text (Get-ThrownMessage { Select-GateNodeRecord -Records $records -PodIp '10.0.0.11' -Prefix $prefix }) -Pattern 'node_id=3,9' -Because '同 IP 多条 = 陈旧记录,不能猜是哪条'
    $corrupt = ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -ValueNodeId 8))
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-GateEtcdRecords -Json $corrupt -Prefix $prefix }) -Pattern '记录损坏' -Because '键与值的 node_id 不一致,标记会打到别的 gate 上'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-GateEtcdRecords -Json 'not json' -Prefix $prefix }) -Pattern 'JSON' -Because '输出无法解析不能当成空'
}

Test-Case 'etcd:客户端地址、launch_time(字符串)、zone_id 按两种字段名解析' {
    Reset-DrainFixture
    $prefix = 'GateNodeService.rpc/zone/7/node_type/4/'
    foreach ($proto in @($false, $true)) {
        $json = ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -LaunchTime 1790000000123 -ClientIp 'gate-1.example.com' -ClientPort 30001 -ProtoNames:$proto))
        $r = (ConvertFrom-GateEtcdRecords -Json $json -Prefix $prefix)[0]
        Assert-Equal -Expected 'True|18000|gate-1.example.com|30001|1790000000123|7' -Actual ("{0}|{1}|{2}|{3}|{4}|{5}" -f $r.HasEndpoint, $r.EndpointPort, $r.ClientIp, $r.ClientPort, $r.LaunchTime, $r.ZoneId) -Because "protoNames=$proto"
    }
}

Test-Case '同 zone 候选:按 login 口径选地址、去重;一台别的候选都没有就拒绝' {
    Reset-DrainFixture
    $prefix = 'GateNodeService.rpc/zone/7/node_type/4/'
    $podip = ConvertFrom-LoginDrainConfig -Text (New-LoginYaml) -Source 'audit'
    $external = ConvertFrom-LoginDrainConfig -Text ((New-LoginYaml) + "`nRequireClientEndpoint: true") -Source 'audit'
    $pick = {
        param($Login, [object[]]$Gates)
        $records = ConvertFrom-GateEtcdRecords -Json (ConvertTo-EtcdJson $Gates) -Prefix $prefix
        $target = Select-GateNodeRecord -Records $records -PodIp '10.0.0.11' -Prefix $prefix
        return ,(Select-GateDrainPeers -Records $records -Target $target -Login $Login -Prefix $prefix)
    }
    $peers = & $pick $podip @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a'), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'b'))
    Assert-Equal -Expected '4' -Actual (@($peers | ForEach-Object { $_.NodeId }) -join ',') -Because 'podip 形态回落 endpoint,另一台算候选'

    $cases = @(
        @{ Login = $podip; Why = '单副本 zone'; Gates = @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a')) },
        @{ Login = $podip; Why = '别的 zone 的记录不算'; Gates = @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a'), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'b' -ZoneId 8)) },
        @{ Login = $external; Why = 'external 下没自报客户端地址的不算'; Gates = @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -ClientIp 'g1' -ClientPort 30001), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'b')) },
        @{ Login = $external; Why = '半填的客户端地址不算'; Gates = @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -ClientIp 'g1' -ClientPort 30001), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'b' -ClientIp 'g2')) },
        @{ Login = $external; Why = '与目标同客户端地址的陈旧影子不算'; Gates = @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -LaunchTime 200 -ClientIp 'g1' -ClientPort 30001), (New-EtcdGate -NodeId 9 -Ip '10.0.0.5' -Uuid 'old' -LaunchTime 100 -ClientIp 'g1' -ClientPort 30001)) }
    )
    foreach ($case in $cases) {
        $failure = Get-ThrownMessage { & $pick $case.Login $case.Gates }
        Assert-Match -Text $failure -Pattern '除 node_id=3 外没有别的可分配 gate.*忽略标记' -Because "必须拒绝:$($case.Why)"
    }

    # 别的 gate 的新旧两条记录同地址:去重后留新的那条,仍算一台候选。
    $peers = & $pick $external @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'a' -ClientIp 'g1' -ClientPort 30001),
        (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'old' -LaunchTime 100 -ClientIp 'g2' -ClientPort 30002),
        (New-EtcdGate -NodeId 5 -Ip '10.0.0.13' -Uuid 'new' -LaunchTime 300 -ClientIp 'g2' -ClientPort 30002))
    Assert-Equal -Expected '5' -Actual (@($peers | ForEach-Object { $_.NodeId }) -join ',') -Because '同地址只留 launch_time 最大者'
}

# ─── 标记与等待 ───

Write-Host '=== gate 排空脚本:draining 标记与 drained 轮询 ==='

Test-Case '新标记:取服务器时间,一次 EVAL 里判定其它候选、清残留 drained、SET NX EX' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Redis['gate:3:drained'] = 'deadline'   # 上一轮留下的判定,不能被这一轮误读
    $mark = Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @(4) -TtlSeconds 1800 6>$null
    Assert-Equal -Expected 'TIME|EVAL mark 3 gate:3:draining gate:3:drained gate:4:draining 1790000000 1800' -Actual ($script:RedisLog -join '|') -Because '只读的 TIME 之后,判定与写入同在一个原子脚本里'
    Assert-Match -Text $script:Redis['gate:3:draining'] -Pattern '^\d+$' -Because 'login 用 ParseInt 读,非整数会被当成 0 立刻按 deadline 放行'
    Assert-Equal -Expected '1800' -Actual $script:RedisTtl['gate:3:draining'] -Because 'TTL 取 -DrainTtlSeconds'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:drained')) -Because '残留 drained 必须先清掉'
    Assert-True -Condition ($mark.Created -and $mark.MarkedAt -eq 1790000000 -and $mark.Value -eq '1790000000') -Because '返回本次写入的值,供撤回时比对'
}

Test-Case '新标记:其它候选全部在排空时原子拒绝,不写任何键' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Redis['gate:4:draining'] = '1789990000'
    $script:Redis['gate:3:drained'] = 'deadline'
    $failure = Get-ThrownMessage { Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @(4) -TtlSeconds 1800 }
    Assert-Match -Text $failure -Pattern '全部在排空.*未做任何写入' -Because 'login 会忽略全部排空的标记'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining')) -Because '不得写 draining'
    Assert-Equal -Expected 'deadline' -Actual $script:Redis['gate:3:drained'] -Because '拒绝发生在 DEL drained 之前'
    $failure = Get-ThrownMessage { Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @() -TtlSeconds 1800 }
    Assert-Match -Text $failure -Pattern '全部在排空' -Because '没有候选时脚本同样拒绝(纵深防御)'
}

Test-Case 'Redis 走 login 配置的地址,密码只经目标 Pod 的环境变量,不上命令行' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $null = Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @(4) -TtlSeconds 1800 6>$null
    $exec = @($script:RedisExec)
    Assert-Equal -Expected 'exec -n audit-infra svc/redis -- sh -c' -Actual ($exec[0..6] -join ' ') -Because 'exec 进 login 所连 Redis 的 Service'
    Assert-Equal -Expected 'sh redis.audit-infra 6379 0' -Actual ($exec[8..11] -join ' ') -Because '地址与 DB 经位置参数传入,不拼进脚本文本'
    Assert-Match -Text $exec[7] -Pattern 'REDISCLI_AUTH=\$REDIS_PASSWORD' -Because '密码取自 Pod 自己的 Secret 环境变量'
    Assert-Match -Text $exec[7] -Pattern 'if \[ -n "\$REDIS_PASSWORD" \]' -Because '空密码时不能设 REDISCLI_AUTH(会发 AUTH "")'
    Assert-Match -Text $exec[7] -Pattern '--no-raw' -Because 'nil 与空串必须可区分'
    Assert-True -Condition (($exec -join ' ') -notmatch 'p#ss' -and ($exec -notcontains '-a')) -Because 'login 配置里的密码不能出现在本机命令行'
}

Test-Case '已有合法标记:沿用、不重置期限;无 TTL、0、晚于现在、溢出 int64、非整数:拒绝' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Redis['gate:3:draining'] = '1789999000'
    $script:RedisTtl['gate:3:draining'] = '800'
    $mark = Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @(4) -TtlSeconds 1800 -WarningAction SilentlyContinue
    Assert-True -Condition (-not $mark.Created -and $mark.MarkedAt -eq 1789999000) -Because '重跑沿用原标记时间,期限不重新起算'
    Assert-Equal -Expected '1789999000' -Actual $script:Redis['gate:3:draining'] -Because '不能覆盖已有标记'
    Assert-Equal -Expected '800' -Actual $script:RedisTtl['gate:3:draining'] -Because '不能重置 TTL'
    $cases = @(
        @{ Value = '1789999000'; Ttl = $null; Pattern = 'TTL=-1' },
        @{ Value = '0'; Ttl = '800'; Pattern = "不是 \(0, Redis TIME=1790000000\] 内" },
        @{ Value = '1790000001'; Ttl = '800'; Pattern = "不是 \(0, Redis TIME=1790000000\] 内" },
        @{ Value = '9999999999999999999'; Ttl = '800'; Pattern = "不是 \(0, Redis TIME=1790000000\] 内" },
        @{ Value = 'yesterday'; Ttl = '800'; Pattern = "不是 \(0, Redis TIME=1790000000\] 内" }
    )
    foreach ($case in $cases) {
        $script:Redis['gate:3:draining'] = $case.Value
        $script:RedisTtl.Remove('gate:3:draining')
        if ($null -ne $case.Ttl) { $script:RedisTtl['gate:3:draining'] = $case.Ttl }
        $failure = Get-ThrownMessage { Set-GateDrainingMark -Login $context.Login -NodeId 3 -PeerNodeIds @(4) -TtlSeconds 1800 }
        Assert-Match -Text $failure -Pattern $case.Pattern -Because "已有标记 '$($case.Value)' 必须拒绝"
        Assert-Match -Text $failure -Pattern '先人工 DEL gate:3:draining gate:3:drained' -Because '拒绝要给出处理方法'
        Assert-Equal -Expected $case.Value -Actual $script:Redis['gate:3:draining'] -Because '拒绝时不改动已有标记'
    }
    Assert-True -Condition (-not (Test-GateDrainMarkValue '1790000000' -NotAfterUnix 1789999999)) -Because '晚于上限的值不可信'
    Assert-True -Condition (Test-GateDrainMarkValue '9223372036854775807') -Because 'int64 上限本身合法(无上限参数时)'
}

Test-Case '轮询:drained 出现即返回判定理由;两个理由都认' {
    foreach ($reason in @('below_threshold', 'deadline')) {
        Reset-DrainFixture
        $context = Get-FixtureContext
        $script:Redis['gate:3:draining'] = '1790000000'
        $script:ReasonToWrite = $reason
        # 模拟 login 的判定循环:第 3 次查询前才写上 drained。
        $script:OnMget = { if ($script:MgetCount -ge 3) { $script:Redis['gate:3:drained'] = $script:ReasonToWrite } }
        $result = Wait-GateDrained -Login $context.Login -NodeId 3 -Record $context.Record -TimeoutSeconds 30 -PollIntervalSeconds 0.01
        Assert-Equal -Expected $reason -Actual $result.Reason -Because '返回 login 写的判定理由'
        Assert-True -Condition (-not $result.IdentityLost) -Because '正常排空不是身份丢失'
        Assert-Equal -Expected 3 -Actual $script:MgetCount -Because '一出现就返回,不多等'
        Assert-True -Condition ($script:RedisLog -contains 'MGET gate:3:draining gate:3:drained') -Because '每轮一次 MGET 同时看两个键'
    }
}

Test-Case '轮询:draining 消失、值非法、drained 值未知都中止,不当成已排空' {
    $cases = @(
        @{ Setup = { $script:Redis.Remove('gate:3:draining') }; Pattern = '已不存在' },
        @{ Setup = { $script:Redis['gate:3:draining'] = 'abc' }; Pattern = '不是整数 Unix 秒' },
        @{ Setup = { $script:Redis['gate:3:drained'] = 'maybe' }; Pattern = "未知的 gate:3:drained 值 'maybe'" }
    )
    foreach ($case in $cases) {
        Reset-DrainFixture
        $context = Get-FixtureContext
        $script:Redis['gate:3:draining'] = '1790000000'
        & $case.Setup
        $failure = Get-ThrownMessage { Wait-GateDrained -Login $context.Login -NodeId 3 -Record $context.Record -TimeoutSeconds 30 -PollIntervalSeconds 0.01 }
        Assert-Match -Text $failure -Pattern $case.Pattern -Because "必须中止:$($case.Pattern)"
    }
}

Test-Case '轮询:每隔几轮复核进程仍在 etcd;进程没了返回 IdentityLost,查询失败继续等' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $c = Get-GateDrainContract
    $script:Redis['gate:3:draining'] = '1790000000'
    $script:Events.Clear()
    # 第 1 次复核查询失败(未知,继续等);第 2 次看到 node_id 3 换成了另一个进程。
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue('not json')
    $script:EtcdResponses.Enqueue((ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.21' -Uuid 'uuid-c'))))
    $result = Wait-GateDrained -Login $context.Login -NodeId 3 -Record $context.Record -TimeoutSeconds 30 -PollIntervalSeconds 0.01
    Assert-True -Condition $result.IdentityLost -Because '按 uuid 认进程:同 node_id 的新进程不是被排空的那一个'
    Assert-Equal -Expected (2 * $c.IdentityRecheckEveryRounds) -Actual $script:MgetCount -Because '每 IdentityRecheckEveryRounds 轮复核一次,查询失败不中止'
    Assert-Equal -Expected 2 -Actual @($script:Events | Where-Object { $_ -eq 'etcd:get' }).Count -Because '复核有节制,不是每轮都查 etcd'
    Assert-True -Condition (@($script:RedisLog | Where-Object { $_ -notlike 'MGET *' }).Count -eq 0) -Because '等待本身不写 Redis,撤回由调用方做'
}

Test-Case '轮询:查询失败只计数继续等;预算耗尽后报超时且不删任何东西' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Redis['gate:3:draining'] = '1790000000'
    $script:Redis['gate:3:drained'] = 'below_threshold'
    $script:RedisFailures = 2
    $result = Wait-GateDrained -Login $context.Login -NodeId 3 -Record $context.Record -TimeoutSeconds 30 -PollIntervalSeconds 0.01 -WarningAction SilentlyContinue
    Assert-Equal -Expected 'below_threshold' -Actual $result.Reason -Because '两次失败后第三次查到'
    Assert-Equal -Expected 1 -Actual $script:MgetCount -Because '失败的两次没有到达 Redis'

    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Calls.Clear()
    $script:Redis['gate:3:draining'] = '1790000000'
    $failure = Get-ThrownMessage { Wait-GateDrained -Login $context.Login -NodeId 3 -Record $context.Record -TimeoutSeconds 0.05 -PollIntervalSeconds 0.01 }
    Assert-Match -Text $failure -Pattern '超过 0.05s 仍未出现.*DEL gate:3:draining gate:3:drained' -Because '超时要给出取消方法'
    Assert-True -Condition (@($script:Events | Where-Object { $_ -like 'delete:*' -or $_ -eq 'redis:DEL' }).Count -eq 0) -Because '超时不删 Pod、不动标记'
    Assert-True -Condition (@($script:Calls | Where-Object { $_.TimeoutSeconds -gt 0.05 -or $_.TimeoutSeconds -le 0 }).Count -eq 0) -Because '每次查询只分到剩余预算'
}

Test-Case 'redis-cli --no-raw 回复解析:nil / 字符串 / 整数 / 状态可区分,错误与乱序拒绝' {
    Reset-DrainFixture
    Assert-True -Condition ($null -eq (ConvertFrom-RedisCliValue -Line '(nil)')) -Because 'nil 是 $null'
    Assert-Equal -Expected '' -Actual (ConvertFrom-RedisCliValue -Line '""') -Because '空串不是 nil'
    Assert-True -Condition ((ConvertFrom-RedisCliValue -Line '(integer) 2') -is [long]) -Because '整数回复'
    Assert-Equal -Expected 'OK' -Actual (ConvertFrom-RedisCliValue -Line 'OK') -Because '状态回复'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RedisCliValue -Line '(error) NOAUTH Authentication required.' }) -Pattern 'NOAUTH' -Because '错误回复不能当成值'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RedisCliArray -Text "1) `"a`"`n3) (nil)" -ExpectedCount 2 }) -Pattern '无法识别' -Because '序号不连续'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RedisCliArray -Text '1) "a"' -ExpectedCount 2 }) -Pattern '期望 2 条' -Because '条数不对'
}

# ─── 删 Pod ───

Write-Host '=== gate 排空脚本:删 Pod、清标记、等重建 ==='

function Set-ReplacementScenario {
    param([string]$AssertPod, [string]$AssertEtcd, [string[]]$AfterPods, [string[]]$AfterEtcd)
    $script:PodResponses.Clear()
    $script:PodResponses.Enqueue($AssertPod)
    foreach ($item in $AfterPods) { $script:PodResponses.Enqueue($item) }
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue($AssertEtcd)
    foreach ($item in $AfterEtcd) { $script:EtcdResponses.Enqueue($item) }
    $script:Redis['gate:3:draining'] = '1790000000'
    $script:Redis['gate:3:drained'] = 'below_threshold'
    $script:Events.Clear()
}

$script:BeforeEtcd = { ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'uuid-a' -Players 5), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'uuid-b')) }
# 新 gate 复用了 node_id 3(常见情形),换了 uuid 与 podIP。
$script:AfterEtcd = { ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.21' -Uuid 'uuid-c'), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'uuid-b')) }

Test-Case '删 Pod:复核通过 → 删除 → 旧记录消失后清两个标记 → 等新 Pod 与 STS 就绪' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    Set-ReplacementScenario -AssertPod (New-MockGatePod) -AssertEtcd (& $script:BeforeEtcd) `
        -AfterPods @((New-MockGatePod -Uid 'uid-new' -PodIp '10.0.0.21')) -AfterEtcd @((& $script:AfterEtcd))
    $result = Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 30
    $events = @($script:Events)
    $delete = [Array]::IndexOf($events, 'delete:pod:gate-1')
    Assert-True -Condition ($delete -ge 0) -Because '必须真正删除 Pod'
    Assert-True -Condition ($delete -lt [Array]::IndexOf($events, 'redis:DEL')) -Because '标记只能在删 Pod、旧记录消失之后清'
    Assert-True -Condition ([Array]::IndexOf($events, 'redis:DEL') -lt [Array]::IndexOf($events, 'get:sts')) -Because '清标记不等新 Pod 就绪:复用 node_id 的新 gate 要尽早能接客'
    $deleteCall = @($script:Calls | Where-Object { $_.Args[0] -eq 'delete' })[0]
    Assert-Equal -Expected 'delete pod gate-1 -n mmorpg-zone-e2e --wait=true --timeout=90s' -Actual ($deleteCall.Args -join ' ') -Because '删除等待 = Pod 宽限期 30s + 60s 余量'
    Assert-True -Condition ($deleteCall.TimeoutSeconds -gt 90) -Because '进程截止要长于 kubectl 自己的等待'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining') -and -not $script:Redis.ContainsKey('gate:3:drained')) -Because '两个标记都清掉'
    Assert-True -Condition $result.MarksCleared -Because '报告已清'
}

Test-Case '删 Pod:排空期间 Pod 被替换或 gate 进程重启过,拒绝删除' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    Set-ReplacementScenario -AssertPod (New-MockGatePod -Uid 'uid-other') -AssertEtcd (& $script:BeforeEtcd)
    $failure = Get-ThrownMessage { Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 30 }
    Assert-Match -Text $failure -Pattern '被替换' -Because '新 Pod 没排空过'
    Assert-True -Condition (@($script:Events | Where-Object { $_ -like 'delete:*' }).Count -eq 0) -Because '不得删除'

    Reset-DrainFixture
    $context = Get-FixtureContext
    Set-ReplacementScenario -AssertPod (New-MockGatePod) -AssertEtcd (ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'uuid-z')))
    $failure = Get-ThrownMessage { Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 30 }
    Assert-Match -Text $failure -Pattern '重启过' -Because '同一 Pod 里的进程换了,标记打在旧身份上'
    Assert-True -Condition (@($script:Events | Where-Object { $_ -like 'delete:*' -or $_ -eq 'redis:DEL' }).Count -eq 0) -Because '不得删除、不得清标记'
}

Test-Case '删 Pod:旧记录迟迟不消失时不清标记,如实报告' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:OriginalContract = $script:ProductionFunctions['Get-GateDrainContract']
    Set-Item Function:script:Get-GateDrainContract {
        $c = & $script:OriginalContract
        $c.StaleRecordWaitSeconds = 0.05
        $c.PollIntervalSeconds = 0.01
        return $c
    }
    Set-ReplacementScenario -AssertPod (New-MockGatePod) -AssertEtcd (& $script:BeforeEtcd) `
        -AfterPods @((New-MockGatePod -Uid 'uid-new' -PodIp '10.0.0.21'))
    $result = Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 30 -WarningAction SilentlyContinue
    Assert-True -Condition (-not $result.MarksCleared) -Because '清早了 login 会把玩家分到已删 Pod 的陈旧记录上'
    Assert-Equal -Expected '1790000000' -Actual $script:Redis['gate:3:draining'] -Because '标记保留到 TTL'
    Assert-True -Condition ($script:Events -contains 'get:sts') -Because '仍然等新 Pod 就绪'
    Assert-True -Condition ($null -eq $result.ReadyError) -Because '新 Pod 已就绪'
}

Test-Case '删 Pod:清标记抛错只记 MarksCleared=false;就绪超时带在 ReadyError 里返回,不盖掉标记状态' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    Set-ReplacementScenario -AssertPod (New-MockGatePod) -AssertEtcd (& $script:BeforeEtcd) `
        -AfterPods @((New-MockGatePod -Uid 'uid-new' -PodIp '10.0.0.21')) -AfterEtcd @((& $script:AfterEtcd))
    $script:RedisFailures = 1   # 删 Pod 流程里唯一的 Redis 调用就是清标记的 DEL
    $result = Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 30 `
        -WarningVariable warned -WarningAction SilentlyContinue 6>$null
    Assert-True -Condition (-not $result.MarksCleared) -Because '清除失败不能报成已清'
    Assert-Match -Text ($warned -join ' ') -Pattern '清除排空标记失败.*Could not connect' -Because '失败原因要可见'
    Assert-Equal -Expected '1790000000' -Actual $script:Redis['gate:3:draining'] -Because '标记仍在'
    Assert-True -Condition ($null -eq $result.ReadyError -and $script:Events -contains 'get:sts') -Because '清除失败不影响等重建'

    Reset-DrainFixture
    $context = Get-FixtureContext
    Set-ReplacementScenario -AssertPod (New-MockGatePod) -AssertEtcd (& $script:BeforeEtcd) -AfterEtcd @((& $script:AfterEtcd))
    $result = Invoke-GatePodReplacement -Login $context.Login -Pod $context.Pod -Record $context.Record -ReadyTimeoutSeconds 0 6>$null
    Assert-True -Condition $result.MarksCleared -Because '旧记录已消失,照常清'
    Assert-Match -Text $result.ReadyError -Pattern '就绪超过 0s' -Because '就绪超时不 throw,带回给主流程排序报告'
}

Test-Case '旧记录是否消失:按 uuid 认进程;缺 uuid 时退回 node_id + ip;查询失败是未知' {
    Reset-DrainFixture
    $login = ConvertFrom-LoginDrainConfig -Text $script:LoginYaml -Source 'audit'
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue((& $script:AfterEtcd))   # node_id 3 已换成 uuid-c @ 10.0.0.21
    $byUuid = [pscustomobject]@{ NodeId = [uint32]3; Ip = '10.0.0.11'; Uuid = 'uuid-a' }
    Assert-True -Condition ((Test-GateRecordGone -Login $login -Record $byUuid) -eq $true) -Because '同 node_id 的新进程不是旧进程'
    $noUuid = [pscustomobject]@{ NodeId = [uint32]3; Ip = '10.0.0.11'; Uuid = '' }
    Assert-True -Condition ((Test-GateRecordGone -Login $login -Record $noUuid) -eq $true) -Because '缺 uuid:node_id 相同但 ip 变了,算消失'
    $noUuidSameIp = [pscustomobject]@{ NodeId = [uint32]3; Ip = '10.0.0.21'; Uuid = '' }
    Assert-True -Condition ((Test-GateRecordGone -Login $login -Record $noUuidSameIp) -eq $false) -Because '缺 uuid:node_id 与 ip 都对上,仍在'
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue('not json')
    Assert-True -Condition ($null -eq (Test-GateRecordGone -Login $login -Record $byUuid)) -Because '查询失败不能当成消失'
}

Test-Case '撤回标记:只删本次写入的值(连同 drained),别人改写过的不动' {
    Reset-DrainFixture
    $login = ConvertFrom-LoginDrainConfig -Text $script:LoginYaml -Source 'audit'
    $script:Redis['gate:3:draining'] = '1790000000'
    $script:Redis['gate:3:drained'] = 'below_threshold'
    Assert-True -Condition (Remove-OwnGateDrainMark -Login $login -NodeId 3 -MarkValue '1790000000') -Because '值相同:撤回'
    Assert-Equal -Expected 'EVAL release 2 gate:3:draining gate:3:drained 1790000000' -Actual $script:RedisLog[0] -Because '一次原子比对删除'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining') -and -not $script:Redis.ContainsKey('gate:3:drained')) -Because '两个键都删'
    $script:Redis['gate:3:draining'] = '1790000500'
    Assert-True -Condition (-not (Remove-OwnGateDrainMark -Login $login -NodeId 3 -MarkValue '1790000000')) -Because '值已被改写:不是本次的'
    Assert-Equal -Expected '1790000500' -Actual $script:Redis['gate:3:draining'] -Because '别人的标记不动'
}

Test-Case '等重建:旧 Pod 仍在或 STS 未满都要等到超时报错,不当成就绪' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $failure = Get-ThrownMessage { Wait-GateStatefulSetReady -Pod $context.Pod -TimeoutSeconds 0.05 -PollIntervalSeconds 0.01 }
    Assert-Match -Text $failure -Pattern '旧 Pod 仍在' -Because '同 UID 的 Pod 不是重建出来的'
    $script:PodResponses.Clear()
    $script:PodResponses.Enqueue((New-MockGatePod -Uid 'uid-new'))
    $script:StsJson = @{ spec = @{ replicas = 2 }; status = @{ readyReplicas = 1 } } | ConvertTo-Json -Compress
    $failure = Get-ThrownMessage { Wait-GateStatefulSetReady -Pod $context.Pod -TimeoutSeconds 0.05 -PollIntervalSeconds 0.01 }
    Assert-Match -Text $failure -Pattern 'readyReplicas=1/2' -Because 'STS 未满不算就绪'
}

# ─── 告警与演练 ───

Write-Host '=== gate 排空脚本:deadline 告警、DryRun 与进程截止 ==='

Test-Case 'reason=deadline:醒目告警带上剩余在线数;读不到在线数也照样告警' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    Write-GateDeadlineWarning -Login $context.Login -Pod $context.Pod -Record $context.Record -WarningVariable warned -WarningAction SilentlyContinue 6>$null
    Assert-Match -Text ($warned -join ' ') -Pattern 'drained=deadline.*仍有玩家在线\(5\).*断线' -Because '运维必须看到删 Pod 会断掉多少人'
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue('not json')
    Write-GateDeadlineWarning -Login $context.Login -Pod $context.Pod -Record $context.Record -WarningVariable warnedAgain -WarningAction SilentlyContinue 6>$null
    Assert-Match -Text ($warnedAgain -join ' ') -Pattern 'drained=deadline.*未知' -Because '在线数只是补充信息,读取失败不能吞掉告警'
}

Test-Case 'DryRun 计划:只打印写操作,不碰 Redis、不删 Pod' {
    Reset-DrainFixture
    $context = Get-FixtureContext
    $script:Events.Clear()
    $text = (Write-GateDrainDryRunPlan -Login $context.Login -Pod $context.Pod -Record $context.Record -PeerNodeIds @(4) -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -WillDeletePod 6>&1) -join "`n"
    Assert-Match -Text $text -Pattern 'SET gate:3:draining <Redis TIME 秒> NX EX 1800' -Because '计划里要写清标记的形状'
    Assert-Match -Text $text -Pattern 'kubectl delete pod gate-1 -n mmorpg-zone-e2e' -Because '带 -DeletePod 时计划里要有删除'
    Assert-Equal -Expected 0 -Actual $script:Events.Count -Because 'DryRun 计划不发出任何调用'
}

# ─── 主流程(Invoke-GateDrain,脚本入口只调用它)───

Write-Host '=== gate 排空脚本:主流程顺序与退出码 ==='

# 完整流程的场景:定位、标记后复核、删前复核都看到同一进程;第一次 MGET 就等到 drained;
# 删 Pod 后 node_id 3 换成新进程(旧 uuid 消失),新 Pod 就绪。
function Set-FullFlowScenario {
    $script:PodResponses.Clear()
    foreach ($i in 1..3) { $script:PodResponses.Enqueue((New-MockGatePod)) }
    $script:PodResponses.Enqueue((New-MockGatePod -Uid 'uid-new' -PodIp '10.0.0.21'))
    $script:EtcdResponses.Clear()
    foreach ($i in 1..3) { $script:EtcdResponses.Enqueue((& $script:BeforeEtcd)) }
    $script:EtcdResponses.Enqueue((& $script:AfterEtcd))
    $script:OnMget = { $script:Redis['gate:3:drained'] = 'below_threshold' }
}

function Invoke-DrainForTest {
    param([switch]$DeletePod, [switch]$DryRun, [int]$ReadyTimeoutSeconds = 30)
    return Invoke-GateDrain -ZoneName 'e2e' -Ordinal 1 -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -ReadyTimeoutSeconds $ReadyTimeoutSeconds `
        -NamespacePrefix 'mmorpg-zone' -DeletePod:$DeletePod -DryRun:$DryRun -WarningAction SilentlyContinue 6>$null
}

function Get-DeleteEvents { return @($script:Events | Where-Object { $_ -like 'delete:*' }) }

Test-Case '主流程 -DryRun:只读定位后返回 0,不访问 Redis、不删 Pod' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $code = Invoke-DrainForTest -DryRun -DeletePod
    Assert-Equal -Expected 0 -Actual $code -Because 'DryRun 成功'
    Assert-Equal -Expected 0 -Actual $script:RedisLog.Count -Because 'DryRun 不发任何 Redis 命令'
    Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because 'DryRun 不删 Pod'
    Assert-Equal -Expected 'get:configmap|get:pod|etcd:get' -Actual ($script:Events -join '|') -Because '只做只读定位'
}

Test-Case '主流程:预算不合格在读 Pod / etcd 与任何 Redis 调用之前拒绝' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:LoginYaml = New-LoginYaml -GateDrainBlock "GateDrain:`n  Deadline: 1h"
    Assert-Match -Text (Get-ThrownMessage { Invoke-DrainForTest -DeletePod }) -Pattern '排空预算不合格' -Because 'TTL 1800 < 期限 3600'
    Assert-Equal -Expected 'get:configmap' -Actual ($script:Events -join '|') -Because '校验失败后什么都不做'
    Assert-Equal -Expected 0 -Actual $script:RedisLog.Count -Because '不写 Redis'
}

Test-Case '主流程:单副本 zone 在任何 Redis 调用之前拒绝(-DryRun 也拒绝)' {
    foreach ($dry in @($false, $true)) {
        Reset-DrainFixture
        Set-FullFlowScenario
        $script:EtcdResponses.Clear()
        $script:EtcdResponses.Enqueue((ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'uuid-a'))))
        $failure = Get-ThrownMessage { Invoke-DrainForTest -DeletePod -DryRun:$dry }
        Assert-Match -Text $failure -Pattern '除 node_id=3 外没有别的可分配 gate' -Because "login 会忽略唯一候选的标记(dryRun=$dry)"
        Assert-Equal -Expected 0 -Actual $script:RedisLog.Count -Because '不访问 Redis'
        Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because '不删 Pod'
    }
}

Test-Case '主流程:其它候选全在排空时原子拒绝,本台不被标记' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:Redis['gate:4:draining'] = '1789990000'
    Assert-Match -Text (Get-ThrownMessage { Invoke-DrainForTest -DeletePod }) -Pattern '全部在排空' -Because '并发排空把 zone 标满时 login 会忽略标记'
    Assert-Equal -Expected 'TIME|EVAL mark 3 gate:3:draining gate:3:drained gate:4:draining 1790000000 1800' -Actual ($script:RedisLog -join '|') -Because '只有只读的 TIME 与拒绝时不写的 EVAL'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining')) -Because '本台没被标记'
    Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because '不删 Pod'
}

Test-Case '主流程 -DeletePod:标记 → 复核 → 等 drained → 删 Pod → 清标记 → 等就绪,返回 0' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $code = Invoke-DrainForTest -DeletePod
    Assert-Equal -Expected 0 -Actual $code -Because '全部完成'
    $events = @($script:Events)
    $order = @('redis:EVAL', 'redis:MGET', 'delete:pod:gate-1', 'redis:DEL', 'get:sts') | ForEach-Object { [Array]::IndexOf($events, $_) }
    Assert-True -Condition ((@($order | Where-Object { $_ -lt 0 }).Count -eq 0) -and ($order -join ',') -eq (($order | Sort-Object) -join ',')) -Because "顺序不对:$($events -join ' ')"
    $recheck = [Array]::IndexOf($events, 'get:pod', [Array]::IndexOf($events, 'redis:EVAL'))
    Assert-True -Condition ($recheck -gt 0 -and $recheck -lt [Array]::IndexOf($events, 'redis:MGET')) -Because '标记后立即复核 Pod 与进程身份'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining') -and -not $script:Redis.ContainsKey('gate:3:drained')) -Because '标记已清'
}

Test-Case '主流程未带 -DeletePod:drained 后返回 0,标记保留,不删 Pod' {
    Reset-DrainFixture
    Set-FullFlowScenario
    Assert-Equal -Expected 0 -Actual (Invoke-DrainForTest) -Because '排空完成即成功'
    Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because '不删 Pod'
    Assert-Equal -Expected '1790000000' -Actual $script:Redis['gate:3:draining'] -Because '标记保留,供加 -DeletePod 重跑沿用'
}

# 定位时是 uuid-a;标记后复核看到同一 podIP 上已是另一个进程(定位与 SET 之间 gate 重启过)。
$script:RestartedEtcd = { ConvertTo-EtcdJson @((New-EtcdGate -NodeId 3 -Ip '10.0.0.11' -Uuid 'uuid-z'), (New-EtcdGate -NodeId 4 -Ip '10.0.0.12' -Uuid 'uuid-b')) }

Test-Case '主流程:标记后复核不通过,撤回本次写入的标记并中止,不等 drained、不删 Pod' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue((& $script:BeforeEtcd))
    $script:EtcdResponses.Enqueue((& $script:RestartedEtcd))
    $failure = Get-ThrownMessage { Invoke-DrainForTest -DeletePod }
    Assert-Match -Text $failure -Pattern '标记后复核不通过.*重启过' -Because '标记落在旧 node_id 上'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining')) -Because '本次写入的标记已撤回'
    Assert-True -Condition ($script:RedisLog -contains 'EVAL release 2 gate:3:draining gate:3:drained 1790000000') -Because '按本次写入值比对撤回'
    Assert-Equal -Expected 0 -Actual $script:MgetCount -Because '不再等 drained'
    Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because '不删 Pod'
}

Test-Case '主流程:复核不通过而标记是沿用的,不删它,返回 3 并给出人工清理指引' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue((& $script:BeforeEtcd))
    $script:EtcdResponses.Enqueue((& $script:RestartedEtcd))
    $script:Redis['gate:3:draining'] = '1789999000'
    $script:RedisTtl['gate:3:draining'] = '800'
    $code = Invoke-GateDrain -ZoneName 'e2e' -Ordinal 1 -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -ReadyTimeoutSeconds 30 `
        -NamespacePrefix 'mmorpg-zone' -DeletePod -WarningVariable warned -WarningAction SilentlyContinue 6>$null
    Assert-Equal -Expected 3 -Actual $code -Because '留下了需要人工清理的标记'
    Assert-Equal -Expected '1789999000' -Actual $script:Redis['gate:3:draining'] -Because '不是本次写入的不删'
    Assert-Match -Text ($warned -join ' ') -Pattern '手工 DEL gate:3:draining gate:3:drained' -Because '要给出人工清理方法'
}

Test-Case '主流程:等 drained 期间进程没了,撤回标记并中止(不空等到超时)' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:OnMget = $null   # login 不会再为不在快照里的进程写 drained
    $script:EtcdResponses.Clear()
    foreach ($i in 1..2) { $script:EtcdResponses.Enqueue((& $script:BeforeEtcd)) }   # 定位、标记后复核
    $script:EtcdResponses.Enqueue((& $script:AfterEtcd))
    $failure = Get-ThrownMessage { Invoke-DrainForTest -DeletePod }
    Assert-Match -Text $failure -Pattern 'uuid=uuid-a\)已从 etcd 消失.*中止' -Because '进程没了,drained 永远不会来'
    Assert-True -Condition (-not $script:Redis.ContainsKey('gate:3:draining')) -Because '撤回本次标记,免得挡住之后复用 node_id 3 的 gate'
    Assert-Equal -Expected (Get-GateDrainContract).IdentityRecheckEveryRounds -Actual $script:MgetCount -Because '第一次复核就发现'
    Assert-Equal -Expected 0 -Actual (Get-DeleteEvents).Count -Because '不删 Pod'
}

Test-Case '主流程:删 Pod 后标记没清,先报残留指引;新 Pod 就绪超时也返回 3' {
    Reset-DrainFixture
    Set-FullFlowScenario
    $script:OriginalContract = $script:ProductionFunctions['Get-GateDrainContract']
    Set-Item Function:script:Get-GateDrainContract { $c = & $script:OriginalContract; $c.StaleRecordWaitSeconds = 0.05; return $c }
    $script:EtcdResponses.Clear()
    $script:EtcdResponses.Enqueue((& $script:BeforeEtcd))   # 旧进程的记录一直不消失
    $code = Invoke-GateDrain -ZoneName 'e2e' -Ordinal 1 -DrainTtlSeconds 1800 -WaitTimeoutSeconds 1800 -ReadyTimeoutSeconds 0 `
        -NamespacePrefix 'mmorpg-zone' -DeletePod -WarningVariable warned -WarningAction SilentlyContinue 6>$null
    Assert-Equal -Expected 3 -Actual $code -Because '标记没清优先于就绪超时'
    $text = $warned -join ' | '
    $guidance = $text.IndexOf('手工 DEL gate:3:draining gate:3:drained')
    Assert-True -Condition ($guidance -ge 0 -and $guidance -lt $text.IndexOf('新 Pod 未就绪')) -Because "先报残留指引再报就绪超时:$text"
    Assert-Equal -Expected '1790000000' -Actual $script:Redis['gate:3:draining'] -Because '标记仍在'

    Reset-DrainFixture
    Set-FullFlowScenario
    $failure = Get-ThrownMessage { Invoke-DrainForTest -DeletePod -ReadyTimeoutSeconds 0 }
    Assert-Match -Text $failure -Pattern '排空标记已清除,但等新 Pod .*就绪超过 0s' -Because '标记已清时就绪超时按普通错误中止(退出码 1)'
}

# 最后一项只运行临时 pwsh 夹具来模拟阻塞的外部进程,不查找或执行真实 kubectl。
Test-Case '外部进程:参数逐项传递,阻塞时按截止硬杀' {
    Reset-DrainFixture
    $pwsh = (Get-Process -Id $PID).Path
    $fixture = Join-Path ([IO.Path]::GetTempPath()) ('gate-drain-fixture-' + [guid]::NewGuid().ToString('N') + '.ps1')
    [IO.File]::WriteAllText($fixture, @'
if ($args[0] -eq 'sleep') { Start-Sleep -Seconds 30 }
ConvertTo-Json -InputObject @($args) -Compress
[Console]::Error.WriteLine('fixture-stderr')
exit 7
'@, [Text.UTF8Encoding]::new($false))
    try {
        $result = Invoke-GateDrainProcess -FilePath $pwsh -ArgumentList @('-NoProfile', '-File', $fixture, 'value with spaces', 'quote"inside', 'h=$1; shift 3') -TimeoutSeconds 30
        Assert-Equal -Expected 7 -Actual $result.ExitCode -Because '保留进程自身退出码'
        $seen = $result.Output | ConvertFrom-Json
        Assert-Equal -Expected 'value with spaces' -Actual $seen[0] -Because '带空格参数不能被拆开'
        Assert-Equal -Expected 'quote"inside' -Actual $seen[1] -Because '参数内引号逐字传递'
        Assert-Equal -Expected 'h=$1; shift 3' -Actual $seen[2] -Because 'sh 脚本文本原样到达,不经本机 shell 展开'
        Assert-Match -Text $result.ErrorOutput -Pattern 'fixture-stderr' -Because 'stderr 独立捕获'

        $elapsed = [Diagnostics.Stopwatch]::StartNew()
        $result = Invoke-GateDrainProcess -FilePath $pwsh -ArgumentList @('-NoProfile', '-File', $fixture, 'sleep') -TimeoutSeconds 0.5
        Assert-True -Condition $result.TimedOut -Because '阻塞必须被截止打断'
        Assert-True -Condition ($result.ExitCode -ne 0) -Because '超时不能伪装成功'
        Assert-True -Condition ($elapsed.Elapsed.TotalSeconds -lt 10) -Because '30s 阻塞夹具不得突破有界清理时间'
    } finally {
        Remove-Item -LiteralPath $fixture -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'k8s_gate_drain')
