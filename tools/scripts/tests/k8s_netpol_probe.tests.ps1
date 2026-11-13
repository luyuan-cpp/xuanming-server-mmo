#requires -Version 7
<#
.SYNOPSIS
    集群核验脚本 tools/scripts/k8s_netpol_probe.ps1 与 Pod 间加密核对库 tools/scripts/lib/pod_network_encryption.ps1 的契约测试。
.DESCRIPTION
    docs/design/gate-online-identity.md §10.2、§10.3。不访问任何集群:
      - 纯函数从脚本 AST 抽出来直接调用(不执行入口);
      - 主流程(Invoke-NetpolProbe)在进程内跑,kubectl 出口换成按夹具应答的替身,模拟「网络插件执行策略」「不执行策略」
        「探测 namespace 是别人的」「只读预览」四种集群;
      - 策略解析器认的字段与 k8s_deploy.ps1 的生成器逐项对上(生成器从部署脚本 AST 抽出来渲染一份)。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_netpol_probe.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

$script:ProbeScript = Join-Path (Get-ToolsScriptsDir) 'k8s_netpol_probe.ps1'
$parseErrors = $null
$script:ProbeAst = [Management.Automation.Language.Parser]::ParseFile($script:ProbeScript, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "k8s_netpol_probe.ps1 语法错误: $($parseErrors.Message -join '; ')" }
foreach ($node in $script:ProbeAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock()
}
. (Join-Path (Join-Path (Get-ToolsScriptsDir) 'lib') 'pod_network_encryption.ps1')
foreach ($name in @('Get-NetpolProbeContract', 'ConvertFrom-NetpolAllowListPolicy', 'Select-NetpolProbeTargetPod', 'New-NetpolProbePlan', 'Get-NetpolProbePodName',
        'New-NetpolProbeManifest', 'Get-NetpolProbeCommand', 'Get-NetpolProbeOutcome', 'Resolve-NetpolProbeVerdict', 'Test-NetpolProbeControls', 'Invoke-NetpolProbe',
        'Get-PodNetworkEncryptionProbe', 'Get-PodNetworkEncryptionNodeEvidenceSpec', 'Get-PodNetworkEncryptionNodeGaps', 'Get-PodNetworkEncryptionClusterProblems', 'ConvertFrom-KubeListJson')) {
    if (-not (Test-Path "Function:$name")) { throw "缺少函数 $name(后续断言无意义)" }
}

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

# ─── 夹具 ───

# 与 k8s_deploy.ps1 New-GrpcAllowListNetworkPolicyYaml 生成的形状相同的一份策略(kubectl -o json 的样子)。
function New-PolicyFixture {
    param(
        [string]$Namespace = 'mmorpg-zone-z1',
        [string]$Name = 'login-grpc-allowlist',
        [string[]]$TargetApps = @('login'),
        [int]$GrpcPort = 50001,
        [string[]]$AllowedApps = @('gate', 'client-rpc-router'),
        [int[]]$OpenPorts = @(9101)
    )
    $ingress = @()
    if ($OpenPorts.Count -gt 0) { $ingress += , [ordered]@{ ports = @($OpenPorts | ForEach-Object { [ordered]@{ protocol = 'TCP'; port = $_ } }) } }
    $ingress += , [ordered]@{
        from  = @([ordered]@{ namespaceSelector = @{}; podSelector = [ordered]@{ matchExpressions = @([ordered]@{ key = 'app'; operator = 'In'; values = $AllowedApps }) } })
        ports = @([ordered]@{ protocol = 'TCP'; port = $GrpcPort })
    }
    return [ordered]@{
        apiVersion = 'networking.k8s.io/v1'; kind = 'NetworkPolicy'
        metadata   = [ordered]@{ name = $Name; namespace = $Namespace; labels = [ordered]@{ 'mmorpg.io/managed-by' = 'k8s_deploy.ps1' } }
        spec       = [ordered]@{
            podSelector = [ordered]@{ matchExpressions = @([ordered]@{ key = 'app'; operator = 'In'; values = $TargetApps }) }
            policyTypes = @('Ingress')
            ingress     = $ingress
        }
    }
}

# hashtable → JSON 文本 → kubectl 返回的那种 PSCustomObject。
function ConvertTo-KubeObject {
    param([Parameter(Mandatory = $true)]$Value)
    return (ConvertTo-Json -InputObject $Value -Depth 32) | ConvertFrom-Json -Depth 64
}

function ConvertTo-KubeListText {
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Items)
    return ConvertTo-Json -InputObject ([ordered]@{ apiVersion = 'v1'; kind = 'List'; items = @($Items) }) -Depth 32
}

function New-PodFixture {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$App,
        [string]$Ip = '10.244.1.10',
        [string]$Phase = 'Running',
        [bool]$Ready = $true,
        [switch]$Terminating
    )
    $metadata = [ordered]@{ name = $Name; labels = [ordered]@{ app = $App } }
    if ($Terminating) { $metadata.deletionTimestamp = '2026-11-12T00:00:00Z' }
    return [ordered]@{
        metadata = $metadata
        status   = [ordered]@{ phase = $Phase; podIP = $Ip; conditions = @([ordered]@{ type = 'Ready'; status = $(if ($Ready) { 'True' } else { 'False' }) }) }
    }
}

# ─── 契约:与 k8s_deploy.ps1 的生成器对上 ───

Test-Case '策略选择器与名字后缀就是 k8s_deploy.ps1 打的标签与名字;解析器读的字段生成器都写了' {
    $c = Get-NetpolProbeContract
    $deployPath = Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'
    $deployAst = [Management.Automation.Language.Parser]::ParseFile($deployPath, [ref]$null, [ref]$null)
    $generator = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -ceq 'New-GrpcAllowListNetworkPolicyYaml' }, $true)
    Assert-True -Condition ($null -ne $generator) -Because '部署脚本要有 New-GrpcAllowListNetworkPolicyYaml'
    Set-Item -Path 'Function:script:New-GrpcAllowListNetworkPolicyYaml' -Value $generator.Body.GetScriptBlock()
    $yaml = New-GrpcAllowListNetworkPolicyYaml -PolicyName 'chat-grpc-allowlist' -TargetApps @('chat') -GrpcPort 50020 -OpenTcpPorts @(9120) -AllowedApps @('client-rpc-router')
    $label = $c.PolicySelector -split '=', 2
    Assert-Match -Text $yaml -Pattern ('(?m)^    ' + [regex]::Escape($label[0]) + ': ' + [regex]::Escape($label[1]) + '$') -Because '生成器打的管理标签就是本脚本选策略用的那个'
    foreach ($line in @('  podSelector:', '    matchExpressions:', '      - key: app', '        operator: In', '        values: [chat]',
            '    - from:', '        - namespaceSelector: {}', '          podSelector:', '              - key: app', '                operator: In',
            '                values: [client-rpc-router]', '        - protocol: TCP', '          port: 50020')) {
        Assert-Match -Text $yaml -Pattern ('(?m)^' + [regex]::Escape($line) + '$') -Because "解析器依赖生成器的这一行:'$line'"
    }
    $deployText = [IO.File]::ReadAllText($deployPath)
    $names = @([regex]::Matches($deployText, "PolicyName = '(?<n>[a-z0-9-]+)'") | ForEach-Object { $_.Groups['n'].Value })
    Assert-True -Condition ($names.Count -ge 9) -Because "准入名单表里至少九份策略(实际 $($names.Count))"
    foreach ($n in $names) { Assert-True -Condition $n.EndsWith($c.PolicyNameSuffix) -Because "策略 $n 以 $($c.PolicyNameSuffix) 结尾,本脚本才认得它" }
    $allowed = @([regex]::Matches($deployText, "AllowedApps = @\((?<v>[^)]*)\)") | ForEach-Object { $_.Groups['v'].Value })
    Assert-True -Condition (-not ($allowed -join ',').Contains("'$($c.UnlistedApp)'")) -Because '任何放行名单都不能含本脚本保留给名单之外调用方的标签'
}

Test-Case '-PodNetworkEncryption 的取值与 k8s_deploy.ps1 一致;-KubeContext 必须显式给' {
    $deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$null)
    $setOf = {
        param($ast)
        $p = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -ceq 'PodNetworkEncryption' }
        $attr = $p.Attributes | Where-Object { $_.TypeName.Name -ceq 'ValidateSet' }
        return (@($attr.PositionalArguments | ForEach-Object { $_.Value }) -join ',')
    }
    Assert-Equal -Expected (& $setOf $deployAst) -Actual (& $setOf $script:ProbeAst) -Because '两个脚本认同一组加密声明'
    $context = $script:ProbeAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -ceq 'KubeContext' }
    $mandatory = @($context.Attributes | Where-Object { $_.TypeName.Name -ceq 'Parameter' } | ForEach-Object { $_.NamedArguments } | Where-Object { $_.ArgumentName -ceq 'Mandatory' })
    Assert-Equal -Expected 1 -Actual $mandatory.Count -Because '本脚本会在集群里建 Pod,不能悄悄落到本机默认 context 上'
}

# ─── 策略解析 ───

Test-Case '解析生成器形状的策略:目标、gRPC 端口与放行名单;对所有来源开放的其它端口不当成 gRPC 规则' {
    $p = ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -TargetApps @('scene', 'scene-world') -AllowedApps @('match', 'guild') -GrpcPort 50000 -OpenPorts @(20000, 9200)))
    Assert-Equal -Expected '' -Actual $p.Error -Because '形状合法'
    Assert-Equal -Expected 'mmorpg-zone-z1/login-grpc-allowlist' -Actual "$($p.Namespace)/$($p.Name)" -Because '带上 namespace 与名字'
    Assert-Equal -Expected 'scene,scene-world' -Actual ($p.TargetApps -join ',') -Because '目标工作负载'
    Assert-Equal -Expected '50000' -Actual $p.GrpcPort -Because 'gRPC 端口取带 from 的那条规则'
    Assert-Equal -Expected 'match,guild' -Actual ($p.AllowedApps -join ',') -Because '放行名单'
    $noOpen = ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -OpenPorts @()))
    Assert-Equal -Expected '' -Actual $noOpen.Error -Because '没有其它开放端口也合法'
}

Test-Case '形状对不上生成器的策略判失败,不猜' {
    $cases = [ordered]@{
        '两条带 from 的规则'      = { param($p) $p.spec.ingress += , $p.spec.ingress[-1] }
        '目标用 NotIn'           = { param($p) $p.spec.podSelector.matchExpressions[0].operator = 'NotIn' }
        '目标按别的键选'          = { param($p) $p.spec.podSelector.matchExpressions[0].key = 'component' }
        'gRPC 规则没有 namespace' = { param($p) $p.spec.ingress[-1].from[0].Remove('namespaceSelector') }
        'gRPC 规则开两个端口'      = { param($p) $p.spec.ingress[-1].ports += , [ordered]@{ protocol = 'TCP'; port = 50002 } }
        'gRPC 规则是 UDP'         = { param($p) $p.spec.ingress[-1].ports[0].protocol = 'UDP' }
        '命名端口'                = { param($p) $p.spec.ingress[-1].ports[0].port = 'grpc' }
        '放行名单为空'             = { param($p) $p.spec.ingress[-1].from[0].podSelector.matchExpressions[0].values = @() }
    }
    foreach ($name in $cases.Keys) {
        $fixture = New-PolicyFixture
        & $cases[$name] $fixture
        $parsed = ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject $fixture)
        Assert-True -Condition (-not [string]::IsNullOrEmpty($parsed.Error)) -Because "$name 要判失败"
        Assert-Match -Text $parsed.Error -Pattern 'mmorpg-zone-z1/login-grpc-allowlist' -Because "$name 的报错点名策略"
    }
}

# ─── 靶子与计划 ───

Test-Case '靶子:按名字取第一个 Running、Ready、没在删除、有 IPv4 的 Pod' {
    $pods = @(
        (New-PodFixture -Name 'match-0' -App 'match' -Ready $false),
        (New-PodFixture -Name 'match-1' -App 'match' -Terminating),
        (New-PodFixture -Name 'match-2' -App 'match' -Phase 'Pending'),
        (New-PodFixture -Name 'match-3' -App 'match' -Ip 'fd00::3'),
        (New-PodFixture -Name 'match-5' -App 'match' -Ip '10.244.1.5'),
        (New-PodFixture -Name 'match-4' -App 'match' -Ip '10.244.1.4'),
        (New-PodFixture -Name 'rank-0' -App 'rank')
    ) | ForEach-Object { ConvertTo-KubeObject $_ }
    $t = Select-NetpolProbeTargetPod -Pods $pods -App 'match'
    Assert-Equal -Expected 'match-4 10.244.1.4' -Actual "$($t.Pod) $($t.Ip)" -Because '没就绪、在删除、Pending、只有 IPv6 的都跳过,剩下的按名字取第一个'
    Assert-True -Condition ($null -eq (Select-NetpolProbeTargetPod -Pods $pods -App 'mail')) -Because '没有这种工作负载回 $null'
}

Test-Case '计划:名单里每个调用方连一次、名单之外的连一次;没部署的金丝雀目标只提示;有 Pod 却没就绪、目标全没部署都算失败' {
    $c = Get-NetpolProbeContract
    $policies = @(
        (ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -Name 'match-grpc-allowlist' -Namespace 'mmorpg-infra' -TargetApps @('match', 'match-canary') -GrpcPort 50050 -AllowedApps @('guild', 'battle')))),
        (ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -Name 'rank-grpc-allowlist' -Namespace 'mmorpg-infra' -TargetApps @('rank') -AllowedApps @('match')))),
        (ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -Name 'chat-grpc-allowlist' -Namespace 'mmorpg-infra' -TargetApps @('chat') -AllowedApps @('client-rpc-router'))))
    )
    $pods = @{
        'mmorpg-infra' = @(
            (New-PodFixture -Name 'match-0' -App 'match' -Ip '10.244.2.7'),
            (New-PodFixture -Name 'rank-0' -App 'rank' -Ready $false)
        ) | ForEach-Object { ConvertTo-KubeObject $_ }
    }
    $plan = New-NetpolProbePlan -Policies $policies -PodsByNamespace $pods
    $matchChecks = @($plan.Checks | Where-Object { $_.Policy -ceq 'mmorpg-infra/match-grpc-allowlist' })
    Assert-Equal -Expected 'guild:connect,battle:connect,netpol-probe-unlisted:blocked' -Actual (($matchChecks | ForEach-Object { "$($_.Caller):$($_.Expect)" }) -join ',') -Because 'match 的靶子:两个调用方连得上,名单之外的连不上'
    Assert-True -Condition (@($matchChecks | Where-Object { $_.Ip -cne '10.244.2.7' -or $_.Port -ne 50050 -or $_.TargetPod -cne 'match-0' }).Count -eq 0) -Because '都连同一个靶子的 gRPC 口'
    Assert-Match -Text ($plan.Skipped -join "`n") -Pattern 'app=match-canary .*not deployed' -Because '没起金丝雀副本:只提示,不算失败'
    Assert-Match -Text ($plan.Problems -join "`n") -Pattern 'rank-grpc-allowlist: 1 pod\(s\) with app=rank .* none is running and ready' -Because '有 Pod 却没就绪:判不了,算失败'
    Assert-Match -Text ($plan.Problems -join "`n") -Pattern 'chat-grpc-allowlist: none of its target workloads \(chat\)' -Because '目标全没部署:这份策略一次都没测到,算失败'
    Assert-Equal -Expected 2 -Actual $plan.Problems.Count -Because '只有这两个问题'
    Assert-Equal -Expected 'battle,guild,netpol-probe-unlisted' -Actual ($plan.Callers -join ',') -Because '只为排进计划的调用方起探测 Pod,加上名单之外的那一个'

    $none = New-NetpolProbePlan -Policies @() -PodsByNamespace @{}
    Assert-Match -Text ($none.Problems -join "`n") -Pattern 'no gRPC allowlist policies found' -Because '一份策略都没有:没开 -NetworkPolicies,不能当成通过'
    $broken = New-NetpolProbePlan -Policies @([pscustomobject]@{ Namespace = 'ns'; Name = 'x-grpc-allowlist'; Error = 'policy ns/x-grpc-allowlist is broken' }) -PodsByNamespace @{}
    Assert-Equal -Expected 'policy ns/x-grpc-allowlist is broken' -Actual ($broken.Problems -join ';') -Because '解析失败原样进问题列表'
    $reserved = New-NetpolProbePlan -Policies @((ConvertFrom-NetpolAllowListPolicy -Policy (ConvertTo-KubeObject (New-PolicyFixture -AllowedApps @($c.UnlistedApp))))) -PodsByNamespace @{}
    Assert-Match -Text ($reserved.Problems -join "`n") -Pattern 'reserves for the unlisted caller' -Because '放行了保留标签的策略判失败,否则「名单之外」的对照失去意义'
}

# ─── 探测 Pod 与探测命令 ───

Test-Case '探测 Pod:名字合法;清单按受限 Pod 安全标准写;镜像是 JSON 字符串;不合法的输入拒绝' {
    $c = Get-NetpolProbeContract
    Assert-Equal -Expected 'probe-client-rpc-router-canary' -Actual (Get-NetpolProbePodName -App 'client-rpc-router-canary') -Because '前缀 + app 标签'
    Assert-Match -Text (Get-ThrownMessage { Get-NetpolProbePodName -App 'Gate' }) -Pattern 'DNS-1123' -Because '大写字母不是合法的标签'
    Assert-Match -Text (Get-ThrownMessage { Get-NetpolProbePodName -App ('a' * 60) }) -Pattern 'longer than 63' -Because 'Pod 名不超过 63 个字符'

    $manifest = New-NetpolProbeManifest -Namespace 'mmorpg-netpol-probe' -Image 'busybox:1.36' -Callers @('gate', $c.UnlistedApp)
    $docs = @($manifest -split "(?m)^---$")
    Assert-Equal -Expected 3 -Actual $docs.Count -Because '一个 namespace 加两个 Pod'
    Assert-Match -Text $docs[0] -Pattern "(?m)^kind: Namespace`$" -Because '第一份是 namespace,apply 时先建'
    Assert-Match -Text $docs[0] -Pattern "(?m)^    $([regex]::Escape($c.ManagedByKey)): $([regex]::Escape($c.ManagedByValue))`$" -Because 'namespace 带「本脚本建的」标签,下次才认得出、删得掉'
    foreach ($line in @('    app: gate', '  automountServiceAccountToken: false', '    runAsNonRoot: true', '      type: RuntimeDefault', '      image: "busybox:1.36"',
            '        allowPrivilegeEscalation: false', '        readOnlyRootFilesystem: true', '          drop: ["ALL"]', "    $($c.ProbeLabelKey): `"1`"")) {
        Assert-Match -Text $docs[1] -Pattern ('(?m)^' + [regex]::Escape($line) + '$') -Because "探测 Pod 清单要有:'$line'"
    }
    Assert-Match -Text $docs[2] -Pattern "(?m)^    app: $([regex]::Escape($c.UnlistedApp))`$" -Because '名单之外的探测 Pod'
    Assert-Match -Text (Get-ThrownMessage { New-NetpolProbeManifest -Namespace 'ns' -Image 'busybox:1.36 --privileged' -Callers @('gate') }) -Pattern 'whitespace' -Because '镜像引用不许带空白'
}

Test-Case '探测命令:只做 TCP 握手、限时、把退出码打成标记行;拼进 sh -c 的只能是 IPv4 与端口' {
    $c = Get-NetpolProbeContract
    $cmd = Get-NetpolProbeCommand -TimeoutSeconds 3 -Ip '10.244.1.4' -Port 50050
    Assert-Equal -Expected 'sh|-c' -Actual (($cmd | Select-Object -First 2) -join '|') -Because '经 sh 包一层,才能把 nc 的退出码打出来'
    Assert-Equal -Expected ('nc -z -w 3 10.244.1.4 50050 2>&1; echo "' + $c.ResultMarker + '$?"') -Actual $cmd[2] -Because '命令逐字钉住'
    Assert-Match -Text (Get-NetpolProbeCommand -TimeoutSeconds 2 -Target 'api-server')[2] -Pattern '"\$KUBERNETES_SERVICE_HOST" "\$KUBERNETES_SERVICE_PORT"' -Because '正对照连 API server'
    Assert-Match -Text (Get-NetpolProbeCommand -TimeoutSeconds 2 -Target 'closed-port')[2] -Pattern 'nc -z -w 2 127\.0\.0\.1 1 ' -Because '负对照连本机没人监听的口'
    Assert-Match -Text (Get-ThrownMessage { Get-NetpolProbeCommand -TimeoutSeconds 3 -Ip '10.0.0.1; rm -rf /' -Port 1 }) -Pattern 'not an IPv4' -Because '不接受任意文本'
    Assert-Match -Text (Get-ThrownMessage { Get-NetpolProbeCommand -TimeoutSeconds 3 -Ip '10.0.0.1' -Port 70000 }) -Pattern 'out of range' -Because '端口越界拒绝'
}

Test-Case '探测结果:标记行 0 = 连上,非 0 = 连不上,没有标记行 = exec 本身失败;以最后一个标记为准' {
    $c = Get-NetpolProbeContract
    Assert-Equal -Expected 'connected' -Actual (Get-NetpolProbeOutcome -Output "$($c.ResultMarker)0") -Because '0 = 连上'
    Assert-Equal -Expected 'not-connected' -Actual (Get-NetpolProbeOutcome -Output "nc: 10.0.0.1 (10.0.0.1:1): Connection timed out`n$($c.ResultMarker)1") -Because '非 0 = 连不上'
    Assert-Equal -Expected 'error' -Actual (Get-NetpolProbeOutcome -Output 'error: unable to upgrade connection: pod does not exist') -Because 'kubectl exec 失败没有标记行'
    Assert-Equal -Expected 'error' -Actual (Get-NetpolProbeOutcome -Output '') -Because '空输出是 error'
    Assert-Equal -Expected 'not-connected' -Actual (Get-NetpolProbeOutcome -Output "$($c.ResultMarker)0`n$($c.ResultMarker)1") -Because '以最后一个为准'
}

# ─── 判定 ───

function New-ResultFixture {
    param([string]$Caller, [string]$Expect, [string]$Outcome, [string]$Pod = 'match-0')
    return [pscustomobject]@{ Policy = 'mmorpg-infra/match-grpc-allowlist'; TargetApp = 'match'; TargetPod = $Pod; Ip = '10.244.2.7'; Port = 50050; Caller = $Caller; Expect = $Expect; Outcome = $Outcome; Detail = '' }
}

Test-Case '判定:名单里的连上、名单之外的连不上才通过;名单之外连上了 = 网络插件不执行策略' {
    $pass = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'connected'), (New-ResultFixture 'netpol-probe-unlisted' 'blocked' 'not-connected')) -PlanProblems @()
    Assert-Equal -Expected 0 -Actual $pass.ExitCode -Because '全部符合退出 0'
    Assert-Equal -Expected 'PASS,PASS' -Actual (($pass.Rows | ForEach-Object { $_.Verdict }) -join ',') -Because '两行都通过'

    $leak = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'connected'), (New-ResultFixture 'netpol-probe-unlisted' 'blocked' 'connected')) -PlanProblems @()
    Assert-Equal -Expected 2 -Actual $leak.ExitCode -Because '名单之外连上了必须失败'
    Assert-Match -Text ($leak.Failures -join "`n") -Pattern 'does not enforce NetworkPolicy' -Because '报错说清楚是网络插件没执行策略'

    $blockedListed = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'not-connected'), (New-ResultFixture 'battle' 'connect' 'connected'), (New-ResultFixture 'netpol-probe-unlisted' 'blocked' 'not-connected')) -PlanProblems @()
    Assert-Equal -Expected 2 -Actual $blockedListed.ExitCode -Because '名单里的调用方被拦也是失败(真实调用方会连不上)'
    Assert-Match -Text ($blockedListed.Failures -join "`n") -Pattern 'guild -> match-0 .*listed caller could not connect' -Because '点名被拦的调用方'
}

Test-Case '判定:名单里一个都没连上时,名单之外的「连不上」判不确定;exec 失败、计划阶段的问题都算失败' {
    $nobody = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'not-connected'), (New-ResultFixture 'netpol-probe-unlisted' 'blocked' 'not-connected')) -PlanProblems @()
    Assert-Equal -Expected 2 -Actual $nobody.ExitCode -Because '靶子可能根本没在监听,连不上证明不了策略拦住了'
    Assert-Match -Text ($nobody.Failures -join "`n") -Pattern 'inconclusive' -Because '标成不确定'
    $otherTarget = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'connected' -Pod 'match-0'), (New-ResultFixture 'netpol-probe-unlisted' 'blocked' 'not-connected' -Pod 'match-canary-0')) -PlanProblems @()
    Assert-Equal -Expected 2 -Actual $otherTarget.ExitCode -Because '可达性按靶子算:别的靶子连上了不能替这个靶子作证'
    $execFailed = Resolve-NetpolProbeVerdict -Results @((New-ResultFixture 'guild' 'connect' 'error')) -PlanProblems @()
    Assert-Match -Text ($execFailed.Failures -join "`n") -Pattern 'kubectl exec failed' -Because 'exec 失败不当成连不上'
    $planned = Resolve-NetpolProbeVerdict -Results @() -PlanProblems @('policy x: broken')
    Assert-Equal -Expected 2 -Actual $planned.ExitCode -Because '计划阶段的问题算失败'
    Assert-Equal -Expected 'policy x: broken' -Actual ($planned.Failures -join ';') -Because '原样列进失败'
}

Test-Case '对照:API server 连得上、没人监听的口连不上,探测工具才可靠' {
    Assert-Equal -Expected 0 -Actual @(Test-NetpolProbeControls -ApiServerOutcome 'connected' -ClosedPortOutcome 'not-connected').Count -Because '两个对照都对'
    Assert-Match -Text (@(Test-NetpolProbeControls -ApiServerOutcome 'not-connected' -ClosedPortOutcome 'not-connected') -join "`n") -Pattern 'API server' -Because '连不上 API server:镜像或出站有问题'
    Assert-Match -Text (@(Test-NetpolProbeControls -ApiServerOutcome 'connected' -ClosedPortOutcome 'connected') -join "`n") -Pattern 'cannot tell blocked from allowed' -Because '连没人监听的口也报连上:工具分不清'
    Assert-Equal -Expected 2 -Actual @(Test-NetpolProbeControls -ApiServerOutcome 'error' -ClosedPortOutcome 'error').Count -Because 'exec 失败两个对照都不成立'
}

# ─── Pod 间加密(lib/pod_network_encryption.ps1) ───

function New-NodeFixture {
    param([string]$Name, [hashtable]$Annotations = @{})
    return [ordered]@{ metadata = [ordered]@{ name = $Name; annotations = $Annotations } }
}

Test-Case '逐节点证据:Calico 看 Node 的 projectcalico.org/WireguardPublicKey,Cilium 看 CiliumNode 的 network.cilium.io/wg-pub-key' {
    $calico = Get-PodNetworkEncryptionNodeEvidenceSpec -Mode 'calico-wireguard'
    Assert-Equal -Expected 'get nodes -o json' -Actual ($calico.EvidenceArgs -join ' ') -Because 'Calico 的公钥就在 Node 上'
    Assert-Equal -Expected 'projectcalico.org/WireguardPublicKey' -Actual $calico.Annotation -Because 'Calico 写的注解'
    $cilium = Get-PodNetworkEncryptionNodeEvidenceSpec -Mode 'cilium-wireguard'
    Assert-Equal -Expected 'get ciliumnodes.cilium.io -o json' -Actual ($cilium.EvidenceArgs -join ' ') -Because 'Cilium 的公钥在同名的 CiliumNode 上'
    Assert-Equal -Expected 'network.cilium.io/wg-pub-key' -Actual $cilium.Annotation -Because 'Cilium 写的注解'
    Assert-Equal -Expected 'get nodes -o json' -Actual ($cilium.NodesArgs -join ' ') -Because '节点全集都从 Node 取'
    Assert-Match -Text (Get-ThrownMessage { Get-PodNetworkEncryptionNodeEvidenceSpec -Mode 'ipsec' }) -Pattern 'no node evidence' -Because '不认识的取值拒绝'
}

Test-Case '逐节点证据:缺公钥的节点点名;CiliumNode 缺一个也算缺;节点列表为空或解析不了不能当成通过' {
    $key = 'projectcalico.org/WireguardPublicKey'
    $nodes = ConvertTo-KubeListText -Items @((New-NodeFixture 'worker-b' @{ $key = 'pub-b' }), (New-NodeFixture 'worker-a'), (New-NodeFixture 'cp' @{ $key = '  ' }))
    $gaps = Get-PodNetworkEncryptionNodeGaps -Mode 'calico-wireguard' -NodesJson $nodes -EvidenceJson $nodes
    Assert-Equal -Expected 'cp,worker-a,worker-b' -Actual ($gaps.Nodes -join ',') -Because '节点全集按名字排序'
    Assert-Equal -Expected 'cp,worker-a' -Actual ($gaps.Missing -join ',') -Because '没有注解、注解只有空白都算没有公钥'

    $ciliumNodes = ConvertTo-KubeListText -Items @((New-NodeFixture 'n1'), (New-NodeFixture 'n2'))
    $ciliumEvidence = ConvertTo-KubeListText -Items @((New-NodeFixture 'n1' @{ 'network.cilium.io/wg-pub-key' = 'pub-1' }))
    $cGaps = Get-PodNetworkEncryptionNodeGaps -Mode 'cilium-wireguard' -NodesJson $ciliumNodes -EvidenceJson $ciliumEvidence
    Assert-Equal -Expected 'n2' -Actual ($cGaps.Missing -join ',') -Because '没有对应 CiliumNode 的节点也算缺'

    Assert-Match -Text (Get-ThrownMessage { Get-PodNetworkEncryptionNodeGaps -Mode 'calico-wireguard' -NodesJson (ConvertTo-KubeListText -Items @()) -EvidenceJson (ConvertTo-KubeListText -Items @()) }) -Pattern 'no nodes' -Because '一个节点都没有:判不了'
    Assert-Match -Text (Get-ThrownMessage { Get-PodNetworkEncryptionNodeGaps -Mode 'calico-wireguard' -NodesJson 'not json' -EvidenceJson 'not json' }) -Pattern 'unparsable' -Because '解析不了:判不了'
}

Test-Case '集群核对:开关没开只报开关、不再查节点;开关开了再查每个节点;kubectl 失败算问题;Calico 只取一次节点' {
    $key = 'projectcalico.org/WireguardPublicKey'
    $script:fakeCalls = [System.Collections.Generic.List[string]]::new()
    $script:fakeAnswers = @{}
    $invoke = {
        param([string[]]$KubectlArgs)
        $joined = $KubectlArgs -join ' '
        $script:fakeCalls.Add($joined)
        if ($script:fakeAnswers.ContainsKey($joined)) { return $script:fakeAnswers[$joined] }
        return [pscustomobject]@{ ExitCode = 1; Stdout = '' }
    }
    $switchArgs = (Get-PodNetworkEncryptionProbe -Mode 'calico-wireguard').Args -join ' '

    $script:fakeAnswers[$switchArgs] = [pscustomobject]@{ ExitCode = 0; Stdout = 'false' }
    $off = @(Get-PodNetworkEncryptionClusterProblems -Mode 'calico-wireguard' -InvokeKubectl $invoke)
    Assert-Equal -Expected 1 -Actual $off.Count -Because '开关没开:一个问题'
    Assert-Match -Text $off[0] -Pattern "wireguardEnabled is 'false', expected 'true'" -Because '报出看到的值与期望值'
    Assert-Equal -Expected 1 -Actual $script:fakeCalls.Count -Because '开关没开就不查节点'

    $script:fakeCalls.Clear()
    $script:fakeAnswers[$switchArgs] = [pscustomobject]@{ ExitCode = 0; Stdout = "true`n" }
    $script:fakeAnswers['get nodes -o json'] = [pscustomobject]@{ ExitCode = 0; Stdout = (ConvertTo-KubeListText -Items @((New-NodeFixture 'a' @{ $key = 'k1' }), (New-NodeFixture 'b' @{ $key = 'k2' }))) }
    Assert-Equal -Expected 0 -Actual @(Get-PodNetworkEncryptionClusterProblems -Mode 'calico-wireguard' -InvokeKubectl $invoke).Count -Because '开关开着、每个节点都有公钥:通过'
    Assert-Equal -Expected 2 -Actual $script:fakeCalls.Count -Because 'Calico 的节点全集与证据是同一份,只取一次'

    $script:fakeAnswers['get nodes -o json'] = [pscustomobject]@{ ExitCode = 0; Stdout = (ConvertTo-KubeListText -Items @((New-NodeFixture 'a' @{ $key = 'k1' }), (New-NodeFixture 'b'))) }
    $gap = @(Get-PodNetworkEncryptionClusterProblems -Mode 'calico-wireguard' -InvokeKubectl $invoke)
    Assert-Match -Text ($gap -join "`n") -Pattern '1 of 2 nodes have no .*not encrypted: b' -Because '点名没启用 WireGuard 的节点'

    $script:fakeAnswers['get nodes -o json'] = [pscustomobject]@{ ExitCode = 1; Stdout = '' }
    Assert-Match -Text (@(Get-PodNetworkEncryptionClusterProblems -Mode 'calico-wireguard' -InvokeKubectl $invoke) -join "`n") -Pattern 'could not list the cluster nodes' -Because 'kubectl 失败:判不了,算问题'

    $script:fakeCalls.Clear()
    $ciliumSwitch = (Get-PodNetworkEncryptionProbe -Mode 'cilium-wireguard').Args -join ' '
    $script:fakeAnswers[$ciliumSwitch] = [pscustomobject]@{ ExitCode = 0; Stdout = 'true' }
    $script:fakeAnswers['get nodes -o json'] = [pscustomobject]@{ ExitCode = 0; Stdout = (ConvertTo-KubeListText -Items @((New-NodeFixture 'a'))) }
    $cilium = @(Get-PodNetworkEncryptionClusterProblems -Mode 'cilium-wireguard' -InvokeKubectl $invoke)
    Assert-Match -Text ($cilium -join "`n") -Pattern 'could not read the CiliumNode annotation network\.cilium\.io/wg-pub-key objects' -Because 'CiliumNode 读不到(没装 Cilium)也算问题'
    Assert-Equal -Expected 3 -Actual $script:fakeCalls.Count -Because 'Cilium 取开关、节点全集、CiliumNode 各一次'
}

# ─── 主流程(进程内,kubectl 出口换成替身) ───

# 一个装了 Calico 的集群:infra 里有 match 与 rank,zone 里有 login;match 的金丝雀没起。Enforce = $false 时网络插件不执行策略。
function New-ClusterFixture {
    param([bool]$Enforce = $true, [bool]$ForeignNamespace = $false)
    $policies = ConvertTo-KubeListText -Items @(
        (New-PolicyFixture -Name 'match-grpc-allowlist' -Namespace 'mmorpg-infra' -TargetApps @('match', 'match-canary') -GrpcPort 50050 -AllowedApps @('guild', 'battle')),
        (New-PolicyFixture -Name 'login-grpc-allowlist' -Namespace 'mmorpg-zone-z1' -TargetApps @('login') -GrpcPort 50001 -AllowedApps @('gate')),
        # 不是准入策略(名字不带后缀):不该被探测。
        (New-PolicyFixture -Name 'data-service-egress' -Namespace 'mmorpg-infra' -TargetApps @('data-service') -AllowedApps @('x'))
    )
    return [pscustomobject]@{
        Enforce          = $Enforce
        ForeignNamespace = $ForeignNamespace
        Policies         = $policies
        Pods             = @{
            'mmorpg-infra'   = ConvertTo-KubeListText -Items @((New-PodFixture -Name 'match-0' -App 'match' -Ip '10.244.2.7'))
            'mmorpg-zone-z1' = ConvertTo-KubeListText -Items @((New-PodFixture -Name 'login-0' -App 'login' -Ip '10.244.3.9'))
        }
        # 靶子 → 名单里的调用方(网络插件执行策略时只有它们连得上)。
        Allowed          = @{ '10.244.2.7:50050' = @('guild', 'battle'); '10.244.3.9:50001' = @('gate') }
        Calls            = [System.Collections.Generic.List[string]]::new()
    }
}

# 按集群夹具应答的 kubectl 替身。exec 的结果:对照按工具可靠应答;连靶子时,执行策略 = 只有名单里的调用方连得上,不执行 = 谁都连得上。
function Invoke-FakeClusterKubectl {
    param($Cluster, [string[]]$Arguments)
    $c = Get-NetpolProbeContract
    $joined = $Arguments -join ' '
    $Cluster.Calls.Add($joined)
    $ok = { param($out) [pscustomobject]@{ ExitCode = 0; Stdout = $out; Stderr = ''; TimedOut = $false } }
    switch -Regex ($joined) {
        '^get networkpolicies --all-namespaces ' { return & $ok $Cluster.Policies }
        '^get pods -n (?<ns>\S+) -o json$' { return & $ok $Cluster.Pods[$Matches['ns']] }
        '^get namespace ' {
            if ($Cluster.ForeignNamespace) { return & $ok '{"metadata":{"name":"mmorpg-netpol-probe","labels":{"team":"someone-else"}}}' }
            return & $ok ''
        }
        '^(apply|wait|delete) ' { return & $ok '' }
        '^exec -n \S+ probe-(?<caller>\S+) -c probe -- sh -c (?<cmd>.*)$' {
            $caller = $Matches['caller']
            $cmd = $Matches['cmd']
            $connected = $false
            if ($cmd.Contains('KUBERNETES_SERVICE_HOST')) { $connected = $true }
            elseif ($cmd.Contains('127.0.0.1 1 ')) { $connected = $false }
            elseif ($cmd -match 'nc -z -w \d+ (?<ip>\S+) (?<port>\d+) ') {
                $endpoint = "$($Matches['ip']):$($Matches['port'])"
                $connected = (-not $Cluster.Enforce) -or (@($Cluster.Allowed[$endpoint]) -ccontains $caller)
            }
            $code = if ($connected) { 0 } else { 1 }
            return & $ok "$($c.ResultMarker)$code`n"
        }
    }
    return [pscustomobject]@{ ExitCode = 1; Stdout = ''; Stderr = "fake kubectl: unexpected call: $joined"; TimedOut = $false }
}

# 在进程内跑 Invoke-NetpolProbe:脚本参数与 $RepoRoot 由这里给,kubectl 出口换成替身。回 { Code; Text; Calls }。
function Invoke-ProbeInProcess {
    param($Cluster, [switch]$DryRunMode, [string]$Encryption = 'none')
    $report = Join-Path ([IO.Path]::GetTempPath()) ("netpol-probe-test-" + [guid]::NewGuid().ToString('N'))
    $KubeContext = 'kind-netpol-test'; $KubeConfig = ''; $PodNetworkEncryption = $Encryption; $ProbeNamespace = 'mmorpg-netpol-probe'
    $Image = 'busybox:1.36'; $ConnectTimeoutSeconds = 3; $ReportDir = $report; $KeepProbeNamespace = $false; $DryRun = [bool]$DryRunMode
    $RepoRoot = Get-RepoRoot
    $fakeCluster = $Cluster
    function Invoke-NetpolProbeKubectl { param([string[]]$Arguments, [double]$TimeoutSeconds = 30) return Invoke-FakeClusterKubectl -Cluster $fakeCluster -Arguments $Arguments }
    $code = $null
    $thrown = ''
    $text = try { Invoke-NetpolProbe 6>&1 | ForEach-Object { if ($_ -is [int]) { $code = $_ } else { [string]$_ } } } catch { $thrown = $_.Exception.Message }
    $summary = if (Test-Path -LiteralPath (Join-Path $report 'summary.txt')) { [IO.File]::ReadAllText((Join-Path $report 'summary.txt')) } else { '' }
    $results = if (Test-Path -LiteralPath (Join-Path $report 'results.txt')) { [IO.File]::ReadAllText((Join-Path $report 'results.txt')) } else { '' }
    Remove-Item -LiteralPath $report -Recurse -Force -ErrorAction SilentlyContinue
    return [pscustomobject]@{ Code = $code; Text = (@($text) -join "`n"); Thrown = $thrown; Calls = $Cluster.Calls; Summary = $summary; Results = $results }
}

Test-Case '主流程:执行策略的集群上全部符合,退出 0;只探测准入策略;对照先于连接检查;最后删掉探测 namespace' {
    $cluster = New-ClusterFixture -Enforce $true
    $r = Invoke-ProbeInProcess -Cluster $cluster
    Assert-Equal -Expected '0' -Actual ([string]$r.Code) -Because "全部符合退出 0。输出: $($r.Text) $($r.Thrown)"
    Assert-Match -Text $r.Text -Pattern 'note: policy mmorpg-infra/match-grpc-allowlist: no pod with app=match-canary' -Because '没起的金丝雀只提示'
    Assert-NotMatch -Text $r.Text -Pattern 'data-service-egress' -Because '名字不带 -grpc-allowlist 后缀的策略不探测'
    $execs = @($r.Calls | Where-Object { $_ -like 'exec *' })
    Assert-Equal -Expected 7 -Actual $execs.Count -Because '两个对照 + match 三条(guild、battle、名单之外)+ login 两条(gate、名单之外)'
    Assert-Match -Text $execs[0] -Pattern 'KUBERNETES_SERVICE_HOST' -Because '先做正对照'
    Assert-Match -Text $execs[1] -Pattern '127\.0\.0\.1 1 ' -Because '再做负对照'
    $apply = [array]::IndexOf([string[]]$r.Calls.ToArray(), @($r.Calls | Where-Object { $_ -like 'apply -f *' })[0])
    $firstExec = [array]::IndexOf([string[]]$r.Calls.ToArray(), $execs[0])
    Assert-True -Condition ($apply -ge 0 -and $apply -lt $firstExec) -Because '先建探测 Pod 再连'
    Assert-Match -Text ($r.Calls -join "`n") -Pattern '(?m)^wait --for=condition=Ready pod -l mmorpg\.io/netpol-probe=1 -n mmorpg-netpol-probe ' -Because '等探测 Pod 就绪'
    Assert-Equal -Expected 'delete namespace mmorpg-netpol-probe --wait=false --ignore-not-found' -Actual $r.Calls[$r.Calls.Count - 1] -Because '最后删掉自己建的探测 namespace'
    Assert-Match -Text $r.Summary -Pattern '(?m)^failures=0$' -Because 'summary.txt 记下结论'
    Assert-Match -Text $r.Results -Pattern '(?m)^PASS\tmmorpg-zone-z1/login-grpc-allowlist\tnetpol-probe-unlisted\tblocked\tnot-connected\t' -Because 'results.txt 逐条记下'
}

Test-Case '主流程:不执行策略的集群(例如 kindnet)上名单之外也连得上,退出 2 并说明原因' {
    $r = Invoke-ProbeInProcess -Cluster (New-ClusterFixture -Enforce $false)
    Assert-Equal -Expected '2' -Actual ([string]$r.Code) -Because "策略没生效必须失败。输出: $($r.Text) $($r.Thrown)"
    Assert-Match -Text $r.Text -Pattern 'FAIL: mmorpg-infra/match-grpc-allowlist netpol-probe-unlisted -> match-0 .*does not enforce NetworkPolicy' -Because '点名哪份策略、哪个靶子没拦住'
    Assert-Match -Text $r.Summary -Pattern '(?m)^failures=2$' -Because '两份策略各一条'
}

Test-Case '主流程:探测 namespace 是别人的就拒绝,什么都不建、什么都不删' {
    $r = Invoke-ProbeInProcess -Cluster (New-ClusterFixture -ForeignNamespace $true)
    Assert-Match -Text $r.Thrown -Pattern 'already exists and was not created by this probe' -Because '拒绝运行(入口把它映射成退出 2)'
    Assert-True -Condition (@($r.Calls | Where-Object { $_ -match '^(apply|delete|exec) ' }).Count -eq 0) -Because "别人的 namespace 不建 Pod、不删。调用: $($r.Calls -join ' | ')"
}

Test-Case '主流程:-DryRun 只读集群、打印计划,不建、不连、不删' {
    $r = Invoke-ProbeInProcess -Cluster (New-ClusterFixture -Enforce $false) -DryRunMode
    Assert-Equal -Expected '0' -Actual ([string]$r.Code) -Because "计划没问题时退出 0。输出: $($r.Text) $($r.Thrown)"
    Assert-Match -Text $r.Text -Pattern '\[dry-run\] mmorpg-zone-z1/login-grpc-allowlist: gate -> login-0 10\.244\.3\.9:50001 expect connect' -Because '打印每一条要做的连接'
    Assert-True -Condition (@($r.Calls | Where-Object { $_ -notmatch '^get ' }).Count -eq 0) -Because "只读:除 get 之外一次都不调。调用: $($r.Calls -join ' | ')"
}

exit (Complete-TestRun -SuiteName 'k8s_netpol_probe')
