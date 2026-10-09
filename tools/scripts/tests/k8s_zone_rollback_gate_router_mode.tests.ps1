#requires -Version 7
<#
.SYNOPSIS
    k8s_zone_rollback.ps1 的 -GateRouterMode 与集群外入口参数的透传 / 停服前预检契约测试。

.DESCRIPTION
    背景(turn-based §22 D75):K8s 部署层 -GateRouterMode 默认已翻成 "1",而 k8s_deploy.ps1 每次部署都把它
    原样重写进 gate env、不从集群读回旧值 —— 回退态不粘滞。灾难回滚的 Step 6 会重新 k8s-zone-up,以前
    $upArgs 里没有这个参数,以 "0" 回退运行的 zone 一回滚就被静默切回 "1";回退的原因(如路由服不可用)若仍在,
    gate 卡在依赖门,登录 / 匹配全部 no_target。

    本套钉住:
      - 显式传 "0" / "1" 时,Step 6 打印的 dev_tools.ps1 k8s-zone-up 参数带上同值的 -GateRouterMode;
      - 留空时 Step 6 不带 -GateRouterMode(默认值只允许存在于 k8s_deploy.ps1 一处,写法同 dev_tools.ps1 /
        k8s_image.ps1 的透传);
      - 拼错在脚本入口被 ValidateSet 拒绝,错误文本点名 GateRouterMode,且早于任何步骤 —— 若拖到 Step 6
        才由下游拒,那时 zone 已停服、数据已回档;
      - -Apply 且留空时的预检(Assert-ZoneEntryKeptOnRollback):gate 当前模式与 k8s_deploy.ps1 默认值
        不一致或判不出,一律在 Step 1 停服之前拒绝;一致才放行;显式给值不核对;dry-run 不读集群。
        以前防静默翻转只靠文字提醒,漏传就把 "0" 回退态切回 "1",而且在走到 Step 6 之前没有任何拦截。
      - 集群外入口(集群外入口 D80 / D88 / D91):-ClientEntryMode / 地址参数 / -GateServiceType / -RequireClientEndpoint /
        -Gateway* / -KubeContext / -KubeConfig 留空不透传、显式给值原样透传到 Step 6(Step 1 同样带上目标集群);
        预检一次 kubectl 同时认 StatefulSet 与 Deployment 两种 gate:-ClientEntryMode 留空而 gate 形态与默认值不同、
        -GatewayIngressHost 留空而 namespace 里有 Ingress gateway,都在停服之前拒绝。Step 1 删掉整个 namespace 之后
        k8s_deploy.ps1 自己的集群现状预检无从触发,以前 external zone 一回滚就被静默重建成 podip、Ingress 丢失。
        Ingress 的显式决定是 -GatewayIngressHost 或 -ConfirmNoGatewayIngress(二者互斥);namespace 里一个 gate 都没有时
        判为读不到,不当成"确定没有 Ingress"。
      - 第 0 步静态预检:-Apply 时把 Step 6 的同一份参数加 -DryRun 交给 dev_tools.ps1 k8s-zone-up 执行一遍,
        throw 或非 0 退出码都在停服之前拒绝(参数组合规则只在 k8s_deploy.ps1 一处,回滚脚本不复制)。
      - -ReleaseProfile / -LoginDevPasswordAuth 只透传到 Step 6(以前 Step 6 恒为 dev 档、开发口令认证被拒绝转发)。
      - dev_tools.ps1 的 k8s-zone-rollback 入口:全部入口参数与 -KubeContext / -KubeConfig 真正转到回滚脚本
        (以前 -KubeContext 被静默吞掉,回滚落到 kubectl 当前 context);-AllowDisruptiveSwitch 同样转给回滚脚本,
        由它只透传到 Step 6、不下传 Step 1;Invoke-K8sImage 拒绝 -LoginDevPasswordAuth(k8s_image.ps1 不部署 login),
        -RequireClientEndpoint 原样透传给 k8s_image.ps1(只参与 k8s_deploy.ps1 的组合预检,不改写 login / scene_manager 的 ConfigMap)。

    子进程用例走 dry-run(不带 -Apply):脚本只打印每一步要执行的命令,不调 kubectl / redis-cli / kafka 工具。
    Step 6 在打印前就把透传写进 $upArgs,所以打印出来的那一行就是 -Apply 时真正传下去的参数。
    仍显式带 -SkipMySqlPause:Step 3 的 Read-Host 目前只在 -Apply 下出现,带上它是防止以后 dry-run
    也停在交互提示上,把 `pwsh -File` 子进程挂死。
    预检只在 -Apply 下读集群,只能在进程内跑沙箱(见 Invoke-RollbackInSandbox):每一步都落在假下游上,
    即使预检有 bug 没拦住也不碰任何集群、Redis、Kafka。
    dev_tools.ps1 入口的子进程用例同样不带 -RollbackApply(回滚脚本只打印);Invoke-K8sImage 的拒绝 / 透传用例按 AST 取出函数、
    下游换成假 k8s_image.ps1(只记下收到的参数),即使拒绝失效也不会真的构建、推送或部署。

    负向用例断错误文本,不只断退出码(同 k8s_deploy_contract.tests.ps1 的口径:退出码 1 可能来自任何地方)。
    既有「拒绝可变 tag / 接受不可变 tag」两条回滚用例在 k8s_deploy_contract.tests.ps1 第 5 节,本文件不重复。

.EXAMPLE
    pwsh -File tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== k8s_zone_rollback.ps1 -GateRouterMode / 集群外入口参数 透传与预检契约测试 ==="

# 不可变 tag 才能过脚本入口的回滚目标校验(见 k8s_deploy_contract.tests.ps1 第 5 节)。
$RollbackBaseArgs = @(
    "-ZoneName", "contract-test", "-ZoneId", "101",
    "-TargetTime", "2026-05-15T14:23:00Z",
    "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab",
    "-SkipMySqlPause"
)

function Invoke-RollbackDryRun {
    param([string[]]$ExtraArguments = @())
    return Invoke-ToolScript -ScriptName "k8s_zone_rollback.ps1" -Arguments ($RollbackBaseArgs + $ExtraArguments)
}

<#
.SYNOPSIS
    从 dry-run 输出里只取 Step 6 打印的那一行 dev_tools.ps1 参数。

.DESCRIPTION
    Step 1(k8s-zone-down)和 Step 7(dev-robot-zones 提示)也会出现 dev_tools.ps1,摘要区的 "gate router"
    行在留空时也会提到 -GateRouterMode —— 按行首 dev_tools.ps1 + k8s-zone-up 锚定,不让它们混进断言。
    恰好一行才算数:找不到这一行时,"不含 GateRouterMode" 的断言会空转通过。
#>
function Get-ZoneUpArgsLine {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output)

    $lines = @([regex]::Matches($Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-up[^\r\n]*') | ForEach-Object { $_.Value.Trim() })
    Assert-Equal -Expected 1 -Actual $lines.Count -Because "dry-run 输出里应当恰有一行 Step 6 的 dev_tools.ps1 k8s-zone-up 参数。输出: $Output"
    return $lines[0]
}

Test-Case '显式 -GateRouterMode 0 / 1:Step 6 的 dev_tools.ps1 k8s-zone-up 参数原样带上' {
    foreach ($mode in @('0', '1')) {
        $run = Invoke-RollbackDryRun -ExtraArguments @('-GateRouterMode', $mode)
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "-GateRouterMode $mode 的 dry-run 不该失败。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern 'DRY-RUN' -Because '不带 -Apply 必须是 dry-run,本用例不能真的去停服 / 清 Redis / 重置 offset'
        $line = Get-ZoneUpArgsLine -Output $run.Output
        Assert-Match -Text $line -Pattern "(^|\s)-GateRouterMode $mode(\s|$)" -Because "以 `"$mode`" 运行的 zone 回滚后必须仍以 `"$mode`" 拉起;不透传就落回 k8s_deploy.ps1 默认值,`"0`" 回退态会被静默切回 `"1`""
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($line, 'GateRouterMode').Count) -Because '只透传一次,重复的具名参数会让 dev_tools.ps1 直接报错'
        Assert-Match -Text $run.Output -Pattern "gate router\s*:\s*-GateRouterMode $mode" -Because '摘要区要写明 Step 6 以哪种路由模式拉起 gate,操作者在 dry-run 阶段就能核对'
    }
}

Test-Case '留空:Step 6 不透传 -GateRouterMode(由 k8s_deploy.ps1 默认值接管),摘要区提醒回退态要显式传 0' {
    $run = Invoke-RollbackDryRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "留空的 dry-run 不该失败。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    Assert-NotMatch -Text $line -Pattern 'GateRouterMode' -Because '留空不得透传:默认值只允许存在于 k8s_deploy.ps1 一处,回滚脚本不得自带或转写一份'
    Assert-Match -Text $run.Output -Pattern 'gate router\s*:[^\r\n]*-GateRouterMode 0' -Because '留空时摘要区必须提醒:以 "0" 回退运行的 zone 要显式传 -GateRouterMode 0,否则 Step 6 落回默认值'
}

Test-Case '负向:-GateRouterMode 只收 空 / "0" / "1",拼错在入口被拒,早于 Step 1 停服' {
    $run = Invoke-RollbackDryRun -ExtraArguments @('-GateRouterMode', '2')
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "gate 侧除 1/true/on 以外一律当关;'2' 必须在回滚脚本入口被拒。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'GateRouterMode' -Because '错误文本必须点名 GateRouterMode,不能把其他执行错误误判为参数校验成功'
    Assert-NotMatch -Text $run.Output -Pattern 'Step 1:' -Because '参数校验必须先于任何步骤:拖到 Step 6 才由下游拒时,zone 已停服、数据已回档,回滚卡在半路'
}

# Step 6 透传的集群外入口参数:参数名 → 用例里给的值(值里不含空格,便于按 "-Name value" 断言)。
$EntryPassthrough = [ordered]@{
    ClientEntryMode           = 'external'
    ClientPublicHost          = '203.0.113.10'
    GateClientHostTemplate    = 'gate-{ordinal}.z1.example.com'
    GateNodePortBase          = '30100'
    GateExternalTrafficPolicy = 'Cluster'
    GateServiceType           = 'NodePort'
    RequireClientEndpoint     = 'false'
    GatewayIngressHost        = 'gw.example.com'
    GatewayIngressClassName   = 'nginx-internal'
    GatewayIngressTlsSecret   = 'gw-tls'
    GatewayTrustedProxies     = '10.244.0.0/16,10.96.0.0/12'
    KubeContext               = 'kind-mmorpg'
    KubeConfig                = 'C:/tmp/kind-mmorpg.kubeconfig'
}

function ConvertTo-ArgumentList {
    param([Parameter(Mandatory = $true)][System.Collections.IDictionary]$Map)
    return @($Map.Keys | ForEach-Object { "-$_"; [string]$Map[$_] })
}

Test-Case '集群外入口参数:显式给值时 Step 6 原样透传且各一次,Step 1 与预检的 kubectl 落在同一集群' {
    $run = Invoke-RollbackDryRun -ExtraArguments (ConvertTo-ArgumentList -Map $EntryPassthrough)
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "显式给齐入口参数的 dry-run 不该失败。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    foreach ($name in $EntryPassthrough.Keys) {
        $value = [regex]::Escape($EntryPassthrough[$name])
        Assert-Match -Text $line -Pattern "(^|\s)-$name $value(\s|$)" -Because "Step 6 必须把 -$name 原样透传给 k8s-zone-up:这组参数不粘滞,Step 1 删 namespace 后漏传就落回默认值"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($line, "(^|\s)-$name\s").Count) -Because "-$name 只透传一次,重复的具名参数会让 dev_tools.ps1 直接报错"
    }
    $downLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-down[^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $downLine.Count -Because "dry-run 输出里应当恰有一行 Step 1 的 dev_tools.ps1 k8s-zone-down 参数。输出: $($run.Output)"
    Assert-Match -Text $downLine[0] -Pattern '-KubeContext kind-mmorpg' -Because 'Step 1 删 namespace 必须与预检、Step 6 落在同一集群,不能落到 kubectl 当前 context'
    # 本用例 -GateRouterMode 留空:预检仍要读集群,但只核对路由模式(-ClientEntryMode / -GatewayIngressHost 已显式)。
    $preflightLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*kubectl get statefulset/gate [^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $preflightLine.Count -Because "dry-run 要预告 -Apply 时预检读的那一条 kubectl。输出: $($run.Output)"
    Assert-Match -Text $preflightLine[0] -Pattern ([regex]::Escape('--context kind-mmorpg --kubeconfig C:/tmp/kind-mmorpg.kubeconfig')) -Because '预检读的必须是 Step 1 / Step 6 将要改动的同一集群'
    Assert-Match -Text $preflightLine[0] -Pattern 'GATE_CLIENT_RPC_ROUTER' -Because '-GateRouterMode 留空仍要核对路由模式'
    Assert-NotMatch -Text $preflightLine[0] -Pattern 'gate 形态须与|不能有 Ingress gateway' -Because '显式给了的项不核对(显式即操作者的决定)'
}

Test-Case '集群外入口参数:留空时 Step 6 一个都不透传(默认值只在 k8s_deploy.ps1 一处)' {
    $run = Invoke-RollbackDryRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "留空的 dry-run 不该失败。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    foreach ($name in $EntryPassthrough.Keys) {
        Assert-NotMatch -Text $line -Pattern "(^|\s)-$name\s" -Because "留空的 -$name 不得透传:回滚脚本不得自带或转写一份默认值(GateNodePortBase 的 -1 = 未指定)"
    }
    Assert-Match -Text $run.Output -Pattern 'client entry\s*:[^\r\n]*-ClientEntryMode external' -Because '摘要区要提醒:以 external 运行的 zone 必须照传 -ClientEntryMode external'
    Assert-Match -Text $run.Output -Pattern 'kubectl get statefulset/gate deployment/gate ingress/gateway [^\r\n]*gate 形态须与[^\r\n]*不能有 Ingress gateway' -Because '三项都留空时,dry-run 要预告 -Apply 时一次 kubectl 核对全部三项'
}

Test-Case '负向:入口参数的枚举拼错、给了 Ingress host 却没给 trusted proxies,都在入口被拒,早于 Step 1 停服' {
    $cases = @(
        @{ Args = @('-ClientEntryMode', 'extern'); Pattern = 'ClientEntryMode' },
        @{ Args = @('-RequireClientEndpoint', 'yes'); Pattern = 'RequireClientEndpoint' },
        @{ Args = @('-GateExternalTrafficPolicy', 'Global'); Pattern = 'GateExternalTrafficPolicy' },
        @{ Args = @('-GateServiceType', 'Ingress'); Pattern = 'GateServiceType' },
        @{ Args = @('-GatewayIngressHost', 'gw.example.com'); Pattern = 'GatewayTrustedProxies' }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackDryRun -ExtraArguments $c.Args
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($c.Args -join ' ') 必须在回滚脚本入口被拒。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern $c.Pattern -Because "$($c.Args -join ' '):错误文本必须点名 $($c.Pattern),不能把其他执行错误误判为入口校验"
        Assert-NotMatch -Text $run.Output -Pattern 'Step 1:' -Because "$($c.Args -join ' '):拖到 Step 6 才由 k8s_deploy.ps1 拒时,zone 已停服、数据已回档"
    }
}

# ─────────────────────────────────────────────────────────────────
# -Apply 时的停服前预检:gate 路由模式 / 客户端入口形态 / gateway Ingress
# ─────────────────────────────────────────────────────────────────

$SandboxNamespace = 'mmorpg-zone-contract-test'
# 预检那一条 kubectl 的固定前缀(一次取回两种 gate 与 Ingress,见 k8s_zone_rollback.ps1 Get-ZoneEntryKubectlArgs)。
$PreflightKubectlPrefix = "get statefulset/gate deployment/gate ingress/gateway -n $SandboxNamespace -o json --ignore-not-found"

<#
.SYNOPSIS
    拼一个 gate 工作负载对象(只含预检会看的字段);不给 -GateEnv 就是没有 env 的旧部署。
    -Kind StatefulSet 即 external 形态(lib New-GateStatefulSetYaml),Deployment 即 podip。
#>
function New-GateWorkload {
    param(
        [ValidateSet('Deployment', 'StatefulSet')][string]$Kind = 'Deployment',
        [AllowNull()][object[]]$GateEnv = $null
    )

    $container = [ordered]@{ name = 'gate'; image = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab' }
    if ($null -ne $GateEnv) { $container.env = $GateEnv }
    return [ordered]@{
        apiVersion = 'apps/v1'
        kind       = $Kind
        metadata   = [ordered]@{ name = 'gate'; namespace = $SandboxNamespace }
        spec       = [ordered]@{ template = [ordered]@{ spec = [ordered]@{ containers = @($container) } } }
    }
}

# zone-up 生成的 Ingress gateway(lib New-GatewayIngressYaml)里预检会看的字段。
function New-GatewayIngress {
    param(
        [Parameter(Mandatory = $true)][string]$IngressHost,
        [string]$ClassName = 'nginx',
        [string]$TlsSecret = ''
    )

    $spec = [ordered]@{ ingressClassName = $ClassName; rules = @([ordered]@{ host = $IngressHost }) }
    if ($TlsSecret) { $spec.tls = @([ordered]@{ hosts = @($IngressHost); secretName = $TlsSecret }) }
    return [ordered]@{
        apiVersion = 'networking.k8s.io/v1'
        kind       = 'Ingress'
        metadata   = [ordered]@{ name = 'gateway'; namespace = $SandboxNamespace }
        spec       = $spec
    }
}

<#
.SYNOPSIS
    external gate StatefulSet 模板里的 env 形状(lib New-GateStatefulSetYaml):路由模式字面值 + fieldRef 干扰项 + 生成器内部地址 env。
#>
function New-ExternalGateEnv {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$RouterValue,
        [string]$NodePortBase = '30100',
        [string]$PublicHost = '203.0.113.10'
    )

    return @(
        @{ name = 'POD_IP'; valueFrom = @{ fieldRef = @{ fieldPath = 'status.podIP' } } },
        @{ name = 'GATE_CLIENT_RPC_ROUTER'; value = $RouterValue },
        @{ name = 'POD_NAME'; valueFrom = @{ fieldRef = @{ fieldPath = 'metadata.name' } } },
        @{ name = 'HOST_IP'; valueFrom = @{ fieldRef = @{ fieldPath = 'status.hostIP' } } },
        @{ name = 'CLIENT_ENDPOINT_SOURCE'; value = 'static' },
        @{ name = 'CLIENT_ENDPOINT_REQUIRED'; value = '1' },
        @{ name = 'CLIENT_PUBLIC_HOST'; value = $PublicHost },
        @{ name = 'GATE_CLIENT_PORT_MODE'; value = 'nodeport' },
        @{ name = 'GATE_NODE_PORT_BASE'; value = $NodePortBase }
    )
}

<#
.SYNOPSIS
    k8s_deploy.ps1 实际生成的 gate env 形状:POD_IP 走 valueFrom(干扰项,预检不能把它当成目标变量),
    GATE_CLIENT_RPC_ROUTER 写字面值。
#>
function New-RouterModeEnv {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

    return @(
        @{ name = 'POD_IP'; valueFrom = @{ fieldRef = @{ fieldPath = 'status.podIP' } } },
        @{ name = 'GATE_CLIENT_RPC_ROUTER'; value = $Value }
    )
}

<#
.SYNOPSIS
    假 kubectl 的一次成功回应,形状同真 kubectl:多个具名资源 + --ignore-not-found 时,找到的合成一个 List;
    一个都没有时无输出(退出码 0)。

.PARAMETER Items
    给了就只用它(@() = 一个都没找到,如 namespace 已被删);不给 = 只有一个 -GateKind 形态的 gate,env 为 -GateEnv。
#>
function New-KubectlReply {
    param(
        [AllowNull()][object[]]$GateEnv = $null,
        [ValidateSet('Deployment', 'StatefulSet')][string]$GateKind = 'Deployment',
        [AllowNull()][AllowEmptyCollection()][object[]]$Items = $null
    )

    if ($null -eq $Items) { $Items = @(New-GateWorkload -Kind $GateKind -GateEnv $GateEnv) }
    if ($Items.Count -eq 0) { return @{ ExitCode = 0; Output = '' } }
    $list = [ordered]@{ apiVersion = 'v1'; kind = 'List'; items = @($Items); metadata = [ordered]@{ resourceVersion = '' } }
    # env 条目的 valueFrom.fieldRef.fieldPath 嵌套十余层,深度给足,别让 ConvertTo-Json 把它截成类型名字符串。
    return @{ ExitCode = 0; Output = ($list | ConvertTo-Json -Depth 30) }
}

<#
.SYNOPSIS
    在进程内的沙箱里跑一遍 k8s_zone_rollback.ps1,返回 @{ Error; Output; KubectlCalls; DevToolsCalls }。

.DESCRIPTION
    预检只在 -Apply 下读集群,子进程 dry-run 覆盖不到,所以在进程内跑:把被测脚本复制进临时「脚本目录」,
    旁边放真 lib/release_common.ps1、lib/k8s_client_entry.ps1 与假的 dev_tools.ps1 / kafka_offset_reset.ps1 / k8s_deploy.ps1;
    kubectl 与 kafka-consumer-groups.sh 用本函数作用域里的同名函数顶替(函数先于外部命令解析,被测脚本在
    子作用域里按动态作用域找到它们)。于是即使预检有 bug 没拦住,-Apply 的每一步也只落在假下游上:
      第 0 步静态预检 / Step 1 / Step 6 → 假 dev_tools.ps1(记下命令与收到的 GateRouterMode,另记一行全部具名参数;
      带 -DryRun 的调用记成 "<命令>/dry-run",与 Step 6 的真实调用区分开);
      Step 2 → 假 kafka-consumer-groups.sh(无输出 = 无 LAG);Step 3 → -SkipMySqlPause;
      Step 4 → 不给 -RedisHost 即跳过;Step 5 → 假 kafka_offset_reset.ps1。
    不碰任何集群、Redis、Kafka。DevToolsCalls 按调用顺序;DevToolsBound 每次调用一行 "<命令> 参数=值;…"
    (按参数名排序,不含 Command 与 DryRun)。

.PARAMETER DryRunFailure
    假 dev_tools.ps1 在 -DryRun 调用上的失败方式:none = 成功;throw = 抛出 -DryRunFailMessage(k8s_deploy.ps1 的惯例);
    exit = 打印 -DryRunFailMessage 后以退出码 3 结束。

.PARAMETER DeployDefault
    '0' / '1' = 只含参数块的假 k8s_deploy.ps1(预检按语法树读它的默认值,真被执行就抛);
    'real' = 复制真 k8s_deploy.ps1,同样只供预检读默认值。

.PARAMETER DeployEntryDefault
    假 k8s_deploy.ps1 里 -ClientEntryMode 的默认值('real' 时不用)。

.PARAMETER KubectlReply
    假 kubectl 的回应:@{ ExitCode; Output },或 @{ Throw = <消息> } 模拟 kubectl 不在 PATH。
    默认就是 Throw:不该读集群的用例一旦读了,KubectlCalls 会记下来。

.PARAMETER ExtraArgs
    额外传给被测脚本的具名参数(集群外入口参数、-KubeContext 等)。
#>
function Invoke-RollbackInSandbox {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('0', '1', 'real')][string]$DeployDefault,
        [ValidateSet('podip', 'external')][string]$DeployEntryDefault = 'podip',
        [hashtable]$KubectlReply = @{ Throw = '本用例不该读集群' },
        [AllowEmptyString()][string]$GateRouterMode = '',
        [hashtable]$ExtraArgs = @{},
        [ValidateSet('none', 'throw', 'exit')][string]$DryRunFailure = 'none',
        [string]$DryRunFailMessage = 'k8s_deploy.ps1 静态预检失败(假)',
        [switch]$Apply
    )

    $sandboxDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-rollback-preflight-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path (Join-Path $sandboxDir 'lib') -Force | Out-Null
    try {
        $toolsDir = Get-ToolsScriptsDir
        Copy-Item -LiteralPath (Join-Path $toolsDir 'k8s_zone_rollback.ps1') -Destination $sandboxDir
        Copy-Item -LiteralPath (Join-Path $toolsDir 'lib' 'release_common.ps1') -Destination (Join-Path $sandboxDir 'lib')
        # 被测脚本在 $ErrorActionPreference = "Stop" 下 dot-source 它(取 Get-ClientEntryContract);不拷则启动即终止,沙箱用例全部跑不到被测逻辑。
        Copy-Item -LiteralPath (Join-Path $toolsDir 'lib' 'k8s_client_entry.ps1') -Destination (Join-Path $sandboxDir 'lib')
        if ($DeployDefault -eq 'real') {
            Copy-Item -LiteralPath (Join-Path $toolsDir 'k8s_deploy.ps1') -Destination $sandboxDir
        }
        else {
            Set-Content -LiteralPath (Join-Path $sandboxDir 'k8s_deploy.ps1') -Encoding utf8 -Value @(
                'param(',
                '    [ValidateSet("0", "1")]',
                ('    [string]$GateRouterMode = "{0}",' -f $DeployDefault),
                '    [ValidateSet("podip", "external")]',
                ('    [string]$ClientEntryMode = "{0}"' -f $DeployEntryDefault),
                ')',
                'throw "假 k8s_deploy.ps1 只供预检读默认值,不该被执行"'
            )
        }
        # 假 dev_tools.ps1 声明第 0 步静态预检 / Step 1 / Step 6 可能传的全部参数并关掉位置绑定:简单脚本里未声明的参数名会让
        # 它后面的值按位置落到别的参数上,记账就错了;以后被测脚本多传一个参数,这里直接报「找不到参数」而不是静默错位。
        $devToolsRecord = Join-Path $sandboxDir 'dev_tools.calls.txt'
        $devToolsBound = Join-Path $sandboxDir 'dev_tools.bound.txt'
        $fakeDevTools = @'
[CmdletBinding(PositionalBinding = $false)]
param($Command, $ZoneName, $ZoneId, $NodeImage, $NamespacePrefix, [switch]$WaitReady, $GateRouterMode = "<unset>", $ReleaseProfile,
    $ClientEntryMode, $ClientPublicHost, $GateClientHostTemplate, $GateNodePortBase, $GateExternalTrafficPolicy, $GateServiceType,
    $RequireClientEndpoint, [switch]$LoginDevPasswordAuth, [switch]$AllowDisruptiveSwitch, $GatewayIngressHost, $GatewayIngressClassName,
    $GatewayIngressTlsSecret, $GatewayTrustedProxies, $KubeContext, $KubeConfig, [switch]$DryRun)
$label = if ($DryRun) { "$Command/dry-run" } else { $Command }
Add-Content -LiteralPath '__CALLS__' -Encoding utf8 -Value "$label GateRouterMode=$GateRouterMode"
$boundParameters = $PSBoundParameters
$bound = @($boundParameters.Keys | Where-Object { $_ -notin @('Command', 'DryRun') } | Sort-Object | ForEach-Object { "$_=$($boundParameters[$_])" }) -join ';'
Add-Content -LiteralPath '__BOUND__' -Encoding utf8 -Value "$label $bound"
if ($DryRun) { __DRYRUN_FAIL__ }
exit 0
'@
        $failMessage = $DryRunFailMessage.Replace("'", "''")
        $dryRunFailCode = switch ($DryRunFailure) {
            'throw' { "throw '$failMessage'" }
            'exit' { "Write-Host '$failMessage'; exit 3" }
            default { '' }
        }
        $fakeDevTools = $fakeDevTools.Replace('__CALLS__', $devToolsRecord.Replace("'", "''")).Replace('__BOUND__', $devToolsBound.Replace("'", "''")).Replace('__DRYRUN_FAIL__', $dryRunFailCode)
        Set-Content -LiteralPath (Join-Path $sandboxDir 'dev_tools.ps1') -Encoding utf8 -Value $fakeDevTools
        Set-Content -LiteralPath (Join-Path $sandboxDir 'kafka_offset_reset.ps1') -Encoding utf8 -Value 'exit 0'

        $fakeKubectlCalls = [System.Collections.Generic.List[string]]::new()
        function kubectl {
            $fakeKubectlCalls.Add(($args -join ' '))
            if ($KubectlReply.ContainsKey('Throw')) {
                throw [System.Management.Automation.CommandNotFoundException]::new($KubectlReply.Throw)
            }
            $global:LASTEXITCODE = $KubectlReply.ExitCode
            return $KubectlReply.Output
        }
        Set-Item -Path 'Function:kafka-consumer-groups.sh' -Value { }

        $rollbackArgs = @{
            ZoneName             = 'contract-test'
            ZoneId               = 101
            TargetTime           = '2026-05-15T14:23:00Z'
            NodeImage            = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab'
            SkipMySqlPause       = $true
            KafkaDrainTimeoutSec = 5
        }
        if ($GateRouterMode -ne '') { $rollbackArgs.GateRouterMode = $GateRouterMode }
        foreach ($key in $ExtraArgs.Keys) { $rollbackArgs[$key] = $ExtraArgs[$key] }
        if ($Apply) { $rollbackArgs.Apply = $true }

        $captured = [System.Text.StringBuilder]::new()
        $errorText = $null
        try {
            & (Join-Path $sandboxDir 'k8s_zone_rollback.ps1') @rollbackArgs *>&1 | ForEach-Object { [void]$captured.AppendLine("$_") }
        }
        catch {
            $errorText = $_.Exception.Message
        }
        return [pscustomobject]@{
            Error         = $errorText
            Output        = $captured.ToString()
            KubectlCalls  = @($fakeKubectlCalls)
            DevToolsCalls = @(if (Test-Path -LiteralPath $devToolsRecord) { Get-Content -LiteralPath $devToolsRecord -Encoding utf8 })
            DevToolsBound = @(if (Test-Path -LiteralPath $devToolsBound) { Get-Content -LiteralPath $devToolsBound -Encoding utf8 })
        }
    }
    finally {
        Remove-Item -LiteralPath $sandboxDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

<#
.SYNOPSIS
    断言这次运行被预检拒绝,且拒绝早于 Step 1:dev_tools.ps1 除第 0 步的 -DryRun 静态预检外一次都没被调,输出里没有 Step 1。
    -HintPattern 是错误文本里"怎么继续"的那一句(默认是路由模式那一项)。
#>
function Assert-RefusedBeforeStop {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Label,
        [string]$HintPattern = '-GateRouterMode 0 或 -GateRouterMode 1'
    )

    Assert-True -Condition ($null -ne $Run.Error) -Because "${Label}:必须拒绝。输出: $($Run.Output)"
    Assert-Match -Text $Run.Error -Pattern '停服之前拒绝' -Because "${Label}:必须是预检自己拒的,不能把别的执行错误误判为预检生效"
    Assert-Match -Text $Run.Error -Pattern $HintPattern -Because "${Label}:错误文本要告诉操作者怎么继续 —— 确认后显式传值"
    $realCalls = @($Run.DevToolsCalls | Where-Object { $_ -notlike '*/dry-run *' })
    Assert-Equal -Expected 0 -Actual $realCalls.Count -Because "${Label}:拒绝必须早于 Step 1 停服,dev_tools.ps1 除 -DryRun 外一次都不该被调(实际: $($Run.DevToolsCalls -join '; '))"
    Assert-NotMatch -Text $Run.Output -Pattern 'Step 1:' -Because "${Label}:拒绝必须早于 Step 1 停服"
}

Test-Case '-Apply 留空 + gate 当前 "0" / 未设变量(旧部署):预检在停服之前拒绝,只读集群一次' {
    $cases = @(
        @{ Label = 'gate env="0"'; Reply = (New-KubectlReply -GateEnv (New-RouterModeEnv -Value '0')); Detail = 'GATE_CLIENT_RPC_ROUTER="0"' },
        @{ Label = 'gate 未设 GATE_CLIENT_RPC_ROUTER'; Reply = (New-KubectlReply); Detail = '未设置 GATE_CLIENT_RPC_ROUTER' }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $c.Reply
        Assert-RefusedBeforeStop -Run $run -Label $c.Label
        Assert-Match -Text $run.Error -Pattern '当前模式为 "0"' -Because "$($c.Label):按 gate_router_mode.h 口径这就是直连;留空会被 Step 6 静默切回 `"1`""
        Assert-Match -Text $run.Error -Pattern ([regex]::Escape($c.Detail)) -Because "$($c.Label):错误文本要带上读到的原值,操作者据此决定传 0 还是 1"
        Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because "$($c.Label):预检只读一次集群"
        Assert-Match -Text $run.KubectlCalls[0] -Pattern ('^' + [regex]::Escape($PreflightKubectlPrefix)) -Because "$($c.Label):预检一次读本 zone namespace 里两种形态的 gate 与 Ingress gateway,且只读"
        Assert-NotMatch -Text $run.Error -Pattern '客户端入口形态:|gateway Ingress:' -Because "$($c.Label):gate 是 Deployment(= 默认 podip)且没有 Ingress,这两项不该被列为问题"
    }
}

Test-Case '-Apply 留空 + 判不出 gate 当前模式(namespace 已删 / kubectl 失败 / kubectl 不可用 / valueFrom / 变量重复):一律拒绝' {
    $cases = @(
        # 真 kubectl 带 --ignore-not-found:namespace 不存在时按名 get 同样是 NotFound,被忽略,输出为空、退出码 0。
        @{ Label = 'namespace 已被上一次中途放弃的回滚删掉'; Reply = (New-KubectlReply -Items @()); Detail = '既没有 gate StatefulSet 也没有 gate Deployment' },
        @{ Label = 'kubectl 以非 0 退出'; Reply = @{ ExitCode = 1; Output = "Error from server (Forbidden): statefulsets.apps `"gate`" is forbidden" }; Detail = 'Forbidden' },
        @{ Label = 'kubectl 不可用'; Reply = @{ Throw = "The term 'kubectl' is not recognized" }; Detail = 'kubectl 调用失败' },
        @{ Label = 'valueFrom 注入'; Reply = (New-KubectlReply -GateEnv @(@{ name = 'GATE_CLIENT_RPC_ROUTER'; valueFrom = @{ configMapKeyRef = @{ name = 'gate-mode'; key = 'router' } } })); Detail = 'valueFrom' },
        # 第一条恰是默认值 "1":取第一条就会错放行,所以必须拒。
        @{ Label = '变量重复'; Reply = (New-KubectlReply -GateEnv @(@{ name = 'GATE_CLIENT_RPC_ROUTER'; value = '1' }, @{ name = 'GATE_CLIENT_RPC_ROUTER'; value = '0' })); Detail = '出现 2 次' }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $c.Reply
        Assert-RefusedBeforeStop -Run $run -Label $c.Label
        Assert-Match -Text $run.Error -Pattern '当前模式为 \(读不到\)' -Because "$($c.Label):判不出就无法证明不会翻转(fail-closed)"
        Assert-Match -Text $run.Error -Pattern ([regex]::Escape($c.Detail)) -Because "$($c.Label):错误文本要带上判不出的原因"
    }
}

Test-Case '-Apply 留空 + gate 当前即默认模式:放行,全流程走完,Step 6 不透传(按 gate 同一口径识别 "1" / " TRUE " / "On")' {
    foreach ($value in @('1', ' TRUE ', 'On')) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value $value))
        Assert-True -Condition ($null -eq $run.Error) -Because "gate env=`"$value`" 按 gate_router_mode.h 是路由模式,与默认值一致,不该拒。错误: $($run.Error)"
        Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=<unset>|k8s-zone-down GateRouterMode=<unset>|k8s-zone-up GateRouterMode=<unset>' -Actual ($run.DevToolsCalls -join '|') -Because "gate env=`"$value`":放行后先静态预检、再 Step 1 停服、Step 6 按默认值拉起;留空不得透传(默认值只在 k8s_deploy.ps1 一处)"
    }
}

Test-Case '-Apply 留空:放行条件跟着 k8s_deploy.ps1 的默认值走,不在回滚脚本里写死 "1"' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '0' -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value '1'))
    Assert-RefusedBeforeStop -Run $run -Label '默认值 "0"、gate 当前 "1"'
    Assert-Match -Text $run.Error -Pattern '默认值 "0"' -Because '默认值必须从 k8s_deploy.ps1 读:那边改成 "0" 时,留空同样是一次翻转(1 → 0)'

    # 真 k8s_deploy.ps1 的默认值必须能按语法树读成字面常量:读不出时 -Apply 留空永远被拒,回滚入口等于被堵死。
    $passedModes = @(foreach ($mode in @('0', '1')) {
            $realRun = Invoke-RollbackInSandbox -Apply -DeployDefault 'real' -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value $mode))
            if ($null -eq $realRun.Error) { $mode }
        })
    Assert-Equal -Expected 1 -Actual $passedModes.Count -Because "gate 当前 `"0`" / `"1`" 两种里必须恰有一种与真 k8s_deploy.ps1 的默认值一致而放行(实际放行: $($passedModes -join ','))"
}

Test-Case '-Apply 显式 -GateRouterMode:不核对路由模式(显式即操作者的决定),Step 6 真正收到同值' {
    foreach ($mode in @('0', '1')) {
        # gate 当前模式故意与显式值相反:显式给值时路由模式不核对(包括有意从 "0" 切回 "1")。
        $opposite = if ($mode -eq '0') { '1' } else { '0' }
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode $mode -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value $opposite))
        Assert-True -Condition ($null -eq $run.Error) -Because "显式 -GateRouterMode $mode 不该被预检拦。错误: $($run.Error)"
        # -ClientEntryMode / -GatewayIngressHost 仍留空,所以预检仍读一次集群,核对的只是入口形态与 Ingress。
        Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because "入口形态与 Ingress 留空,仍要读一次集群"
        Assert-Equal -Expected "k8s-zone-up/dry-run GateRouterMode=$mode|k8s-zone-down GateRouterMode=<unset>|k8s-zone-up GateRouterMode=$mode" -Actual ($run.DevToolsCalls -join '|') -Because "-Apply 时静态预检与 Step 6 都必须真正把 $mode 传给 dev_tools.ps1,不只是打印出来"
    }
}

Test-Case '-Apply 三项(-GateRouterMode / -ClientEntryMode / -GatewayIngressHost)都显式:不读集群' {
    $extra = @{ ClientEntryMode = 'podip'; GatewayIngressHost = 'gw.example.com'; GatewayTrustedProxies = '10.244.0.0/16' }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $extra
    Assert-True -Condition ($null -eq $run.Error) -Because "三项都显式时不该被预检拦。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.KubectlCalls.Count -Because '三项都显式 = 操作者已决定,不核对集群'
    Assert-Match -Text $run.Output -Pattern '三项均已显式给值,不读集群' -Because '输出要写明本次没有核对集群'
}

Test-Case 'dry-run 留空:不读集群、不执行任何步骤,只预告 -Apply 时的预检' {
    $run = Invoke-RollbackInSandbox -DeployDefault '1'
    Assert-True -Condition ($null -eq $run.Error) -Because "留空的 dry-run 不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.KubectlCalls.Count -Because 'dry-run 只打印命令,不调任何外部工具'
    Assert-Equal -Expected 0 -Actual $run.DevToolsCalls.Count -Because 'dry-run 不执行任何步骤'
    Assert-Match -Text $run.Output -Pattern ('kubectl ' + [regex]::Escape($PreflightKubectlPrefix)) -Because 'dry-run 要预告 -Apply 时读的是哪个 namespace 的 gate 与 Ingress'
}

# 从 DevToolsBound 里取某条命令唯一的那一行("<命令> 参数=值;…")。
function Get-BoundLine {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Command
    )

    $lines = @($Run.DevToolsBound | Where-Object { $_ -like "$Command *" })
    Assert-Equal -Expected 1 -Actual $lines.Count -Because "$Command 应当恰被调一次(实际: $($Run.DevToolsBound -join ' | '))"
    return $lines[0]
}

Test-Case '-Apply 留空 + gate 是 StatefulSet(external):入口形态与默认 podip 不同,停服之前拒绝并给出照传用的地址 env' {
    $reply = New-KubectlReply -GateKind StatefulSet -GateEnv (New-ExternalGateEnv -RouterValue '1')
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $reply
    Assert-RefusedBeforeStop -Run $run -Label 'StatefulSet gate + 留空' -HintPattern '-ClientEntryMode podip 或 -ClientEntryMode external'
    Assert-Match -Text $run.Error -Pattern '当前入口形态为 "external"' -Because 'StatefulSet 版 gate 就是 external(k8s_deploy.ps1 模式矩阵);留空会被 Step 6 重建成 podip,集群外玩家全部连不上'
    Assert-Match -Text $run.Error -Pattern "GATE_NODE_PORT_BASE='30100'" -Because '错误文本要带上模板里的地址 env,操作者据此照传 -GateNodePortBase'
    Assert-Match -Text $run.Error -Pattern "CLIENT_PUBLIC_HOST='203\.0\.113\.10'" -Because '同上:照传 -ClientPublicHost'
    Assert-NotMatch -Text $run.Error -Pattern 'gate 路由模式:' -Because 'StatefulSet 模板里的路由模式 "1" 与默认一致,不该被列为问题 —— 路由模式核对必须认 StatefulSet 版 gate'
}

Test-Case '-Apply 路由模式核对认 StatefulSet 版 gate:模板为 "0" 且留空时拒绝;照传入口参数后放行,Step 6 真正收到它们' {
    $external = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; GateNodePortBase = 30100; ClientPublicHost = '203.0.113.10' }
    $refused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $external -KubectlReply (New-KubectlReply -GateKind StatefulSet -GateEnv (New-ExternalGateEnv -RouterValue '0'))
    Assert-RefusedBeforeStop -Run $refused -Label 'StatefulSet gate env="0"'
    Assert-Match -Text $refused.Error -Pattern ([regex]::Escape('StatefulSet 模板:gate env GATE_CLIENT_RPC_ROUTER="0"')) -Because '以前只读 deployment gate,external zone 一律判不出;现在必须从 StatefulSet 模板读到原值'
    Assert-NotMatch -Text $refused.Error -Pattern '客户端入口形态:' -Because '显式给了 -ClientEntryMode 即操作者的决定,不核对'

    $passed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $external -KubectlReply (New-KubectlReply -GateKind StatefulSet -GateEnv (New-ExternalGateEnv -RouterValue '1'))
    Assert-True -Condition ($null -eq $passed.Error) -Because "模板路由模式与默认一致、入口参数显式照传,不该被拦。错误: $($passed.Error)"
    Assert-Match -Text $passed.Output -Pattern "集群现状\(仅供核对\):gate 为 StatefulSet;模板地址 env:[^\r\n]*GATE_NODE_PORT_BASE='30100'" -Because '显式给了 -ClientEntryMode 时也要把集群现状打出来,供操作者核对照传的地址参数'
    $up = Get-BoundLine -Run $passed -Command 'k8s-zone-up'
    foreach ($pair in @('ClientEntryMode=external', 'GateServiceType=NodePort', 'GateNodePortBase=30100', 'ClientPublicHost=203.0.113.10')) {
        Assert-Match -Text $up -Pattern "(^| |;)$([regex]::Escape($pair))(;|$)" -Because "-Apply 时 Step 6 必须真正把 $pair 传给 dev_tools.ps1,不只是打印出来"
    }
    Assert-NotMatch -Text $up -Pattern 'GatewayIngressHost=|RequireClientEndpoint=|KubeContext=|GateRouterMode=' -Because '留空的参数不透传(默认值只在 k8s_deploy.ps1 一处)'
}

Test-Case '-Apply 留空 + gate StatefulSet 与 Deployment 同时存在(上一次切换没做完):路由模式与入口形态都判不出,拒绝' {
    $items = @((New-GateWorkload -Kind StatefulSet -GateEnv (New-ExternalGateEnv -RouterValue '1')), (New-GateWorkload -Kind Deployment -GateEnv (New-RouterModeEnv -Value '1')))
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply (New-KubectlReply -Items $items)
    Assert-RefusedBeforeStop -Run $run -Label '两种形态并存'
    Assert-Match -Text $run.Error -Pattern '当前模式为 \(读不到\)' -Because '不猜哪个在服务(fail-closed)'
    Assert-Match -Text $run.Error -Pattern '当前入口形态为 \(读不到\)' -Because '同上'
    Assert-Match -Text $run.Error -Pattern '同时存在' -Because '错误文本要带上判不出的原因'
}

Test-Case '-Apply namespace 已不存在:Ingress 判为读不到(不当成"确定没有"),显式 -ConfirmNoGatewayIngress 或照传 -GatewayIngressHost 才放行' {
    # 上一次回滚中途放弃、Step 1 已删 namespace:Ingress 当时已随 namespace 一起没了,"现在没有"证明不了"回滚前没有"。
    $gone = New-KubectlReply -Items @()
    $refused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs @{ ClientEntryMode = 'podip' } -KubectlReply $gone
    Assert-RefusedBeforeStop -Run $refused -Label 'namespace 已删 + Ingress 未表态' -HintPattern '-ConfirmNoGatewayIngress'
    Assert-Match -Text $refused.Error -Pattern '无从判断回滚前有没有 Ingress gateway' -Because '错误文本要写明判不出的原因,与路由模式、入口形态"读不到即拒"同一口径'
    Assert-NotMatch -Text $refused.Output -Pattern '留空不会丢 Ingress' -Because '判不出时不得打印放行口径的绿字,误导操作者'
    Assert-NotMatch -Text $refused.Error -Pattern 'gate 路由模式:|客户端入口形态:' -Because '路由模式与入口形态已显式给值,不核对'

    $confirmed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true } -KubectlReply $gone
    Assert-True -Condition ($null -eq $confirmed.Error) -Because "三项都已显式决定(含 -ConfirmNoGatewayIngress),namespace 已删也必须能继续。错误: $($confirmed.Error)"
    Assert-Equal -Expected 0 -Actual $confirmed.KubectlCalls.Count -Because '三项都显式 = 操作者已决定,不核对集群'
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1|k8s-zone-down GateRouterMode=<unset>|k8s-zone-up GateRouterMode=1' -Actual ($confirmed.DevToolsCalls -join '|') -Because '放行后照常静态预检、停服、按显式值拉起'
    Assert-NotMatch -Text (Get-BoundLine -Run $confirmed -Command 'k8s-zone-up') -Pattern 'Gateway|Confirm' -Because '-ConfirmNoGatewayIngress 只是回滚脚本自己的核对豁免,不传给 k8s-zone-up;Step 6 不生成 Ingress'

    $kept = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -KubectlReply $gone `
        -ExtraArgs @{ ClientEntryMode = 'podip'; GatewayIngressHost = 'gw.z1.example.com'; GatewayTrustedProxies = '10.244.0.0/16' }
    Assert-True -Condition ($null -eq $kept.Error) -Because "照传 -GatewayIngressHost 同样是显式决定,必须能继续。错误: $($kept.Error)"
    Assert-Match -Text (Get-BoundLine -Run $kept -Command 'k8s-zone-up') -Pattern '(^| |;)GatewayIngressHost=gw\.z1\.example\.com(;|$)' -Because 'Step 6 按部署记录重建 Ingress'
}

Test-Case '-Apply namespace 里有 Ingress gateway + -ConfirmNoGatewayIngress:显式撤掉 Ingress,放行且不再读集群核对 Ingress' {
    $items = @((New-GateWorkload -GateEnv (New-RouterModeEnv -Value '1')), (New-GatewayIngress -IngressHost 'gw.z1.example.com'))
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs @{ ConfirmNoGatewayIngress = $true } -KubectlReply (New-KubectlReply -Items $items)
    Assert-True -Condition ($null -eq $run.Error) -Because "-ConfirmNoGatewayIngress 是 Ingress 这一项的显式决定,不该再因现有 Ingress 被拦。错误: $($run.Error)"
    Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because '路由模式与入口形态仍留空,仍要读一次集群'
    Assert-NotMatch -Text $run.Output -Pattern 'kubectl get statefulset/gate[^\r\n]*不能有 Ingress gateway' -Because '显式表态后,预检那一行不再列 Ingress 核对项'
    Assert-Match -Text $run.Output -Pattern '显式 -ConfirmNoGatewayIngress' -Because '输出要写明 Ingress 这一项没有核对、是操作者的决定'
    Assert-NotMatch -Text (Get-BoundLine -Run $run -Command 'k8s-zone-up') -Pattern 'Gateway|Confirm' -Because 'Step 6 不生成 Ingress,豁免开关也不下传'
}

Test-Case '-Apply 留空 -GatewayIngressHost 而 namespace 里有 Ingress gateway:停服之前拒绝;照传后 Step 6 收到全部 -Gateway* 参数' {
    $items = @((New-GateWorkload -GateEnv (New-RouterModeEnv -Value '1')), (New-GatewayIngress -IngressHost 'gw.z1.example.com' -TlsSecret 'gw-tls'))
    $reply = New-KubectlReply -Items $items
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $reply
    Assert-RefusedBeforeStop -Run $run -Label '有 Ingress + 留空' -HintPattern '-GatewayIngressHost gw\.z1\.example\.com -GatewayTrustedProxies'
    Assert-Match -Text $run.Error -Pattern '-GatewayIngressTlsSecret gw-tls' -Because 'TLS Secret 同样要照传,否则重建的 Ingress 没有 tls 段'
    Assert-NotMatch -Text $run.Error -Pattern 'gate 路由模式:|客户端入口形态:' -Because 'gate 与默认一致,问题只有 Ingress 一项'

    $extra = @{ GatewayIngressHost = 'gw.z1.example.com'; GatewayTrustedProxies = '10.244.0.0/16'; GatewayIngressClassName = 'nginx'; GatewayIngressTlsSecret = 'gw-tls' }
    $passed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $extra -KubectlReply $reply
    Assert-True -Condition ($null -eq $passed.Error) -Because "照传 -Gateway* 后不该被拦。错误: $($passed.Error)"
    $up = Get-BoundLine -Run $passed -Command 'k8s-zone-up'
    foreach ($key in $extra.Keys) {
        Assert-Match -Text $up -Pattern "(^| |;)$key=$([regex]::Escape($extra[$key]))(;|$)" -Because "Step 6 必须把 -$key 传给 k8s-zone-up:Step 1 删 namespace 时 Ingress 一起没了,只能靠它重建"
    }
}

Test-Case '-Apply 留空 -ClientEntryMode:放行条件跟着 k8s_deploy.ps1 的默认值走,不在回滚脚本里写死 "podip"' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -DeployEntryDefault 'external' -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value '1'))
    Assert-RefusedBeforeStop -Run $run -Label '默认值 external、gate 当前 Deployment' -HintPattern '-ClientEntryMode podip 或 -ClientEntryMode external'
    Assert-Match -Text $run.Error -Pattern '默认值 "external"' -Because '默认值必须从 k8s_deploy.ps1 读:那边改了默认值,留空同样是一次翻转'
    Assert-Match -Text $run.Error -Pattern '当前入口形态为 "podip"' -Because 'Deployment 版 gate 就是 podip'
}

Test-Case '-Apply -KubeContext / -KubeConfig:预检的 kubectl、Step 1、Step 6 落在同一集群' {
    $extra = @{ KubeContext = 'kind-mmorpg'; KubeConfig = 'C:/tmp/kind-mmorpg.kubeconfig' }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs $extra -KubectlReply (New-KubectlReply -GateEnv (New-RouterModeEnv -Value '1'))
    Assert-True -Condition ($null -eq $run.Error) -Because "gate 与默认一致、没有 Ingress,不该被拦。错误: $($run.Error)"
    Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because '预检只读一次集群'
    Assert-Match -Text $run.KubectlCalls[0] -Pattern ([regex]::Escape('--context kind-mmorpg --kubeconfig C:/tmp/kind-mmorpg.kubeconfig')) -Because '预检读的必须是将要被改动的那个集群,不能落到 kubectl 当前 context'
    foreach ($command in @('k8s-zone-down', 'k8s-zone-up')) {
        $line = Get-BoundLine -Run $run -Command $command
        Assert-Match -Text $line -Pattern '(^| |;)KubeContext=kind-mmorpg(;|$)' -Because "$command 必须与预检落在同一集群"
        Assert-Match -Text $line -Pattern ([regex]::Escape('KubeConfig=C:/tmp/kind-mmorpg.kubeconfig')) -Because "$command 必须与预检用同一份 kubeconfig"
    }
}

# ─────────────────────────────────────────────────────────────────
# 第 0 步静态预检:Step 6 的同一份参数加 -DryRun,先于 Step 1 交给 k8s_deploy.ps1 判定
# ─────────────────────────────────────────────────────────────────

Test-Case '-Apply 静态预检:先于 Step 1 执行 k8s-zone-up -DryRun,参数与 Step 6 逐项相同' {
    # 三项都显式,不读集群;这里只看静态预检收到的是不是 Step 6 的那一份。
    $extra = @{
        ClientEntryMode = 'external'; GateServiceType = 'NodePort'; GateNodePortBase = 30100; ClientPublicHost = '203.0.113.10'
        GatewayIngressHost = 'gw.example.com'; GatewayTrustedProxies = '10.244.0.0/16'; RequireClientEndpoint = 'false'
        ReleaseProfile = 'prod'; KubeContext = 'kind-mmorpg'
    }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '0' -ExtraArgs $extra
    Assert-True -Condition ($null -eq $run.Error) -Because "假 dev_tools.ps1 的 -DryRun 成功,不该被拦。错误: $($run.Error)"
    Assert-Match -Text ($run.DevToolsCalls -join '|') -Pattern '^k8s-zone-up/dry-run [^|]*\|k8s-zone-down ' -Because "静态预检必须先于 Step 1 停服(实际: $($run.DevToolsCalls -join '; '))"
    $dryRunArgs = (Get-BoundLine -Run $run -Command 'k8s-zone-up/dry-run').Substring('k8s-zone-up/dry-run '.Length)
    $upArgsLine = (Get-BoundLine -Run $run -Command 'k8s-zone-up').Substring('k8s-zone-up '.Length)
    Assert-Equal -Expected $upArgsLine -Actual $dryRunArgs -Because '静态预检必须用 Step 6 的同一份参数(只多 -DryRun),否则预检过了的组合不等于 Step 6 真正执行的组合'
    Assert-Match -Text $upArgsLine -Pattern '(^|;)ReleaseProfile=prod(;|$)' -Because '-ReleaseProfile 必须真正传到 Step 6,否则 prod zone 回滚按 dev 档部署(占位密钥回落)'
    Assert-NotMatch -Text (Get-BoundLine -Run $run -Command 'k8s-zone-down') -Pattern 'ReleaseProfile' -Because 'zone-down 与档位无关,不传'
    Assert-Match -Text $run.Output -Pattern 'k8s-zone-up -DryRun 通过' -Because '放行时写明静态预检的结论'
}

Test-Case '-Apply 静态预检失败(throw / 非 0 退出码):停服之前拒绝,错误或输出带上 k8s_deploy.ps1 的原因' {
    foreach ($mode in @('throw', 'exit')) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true } `
            -DryRunFailure $mode -DryRunFailMessage 'LoadBalancer 却没有 -GateClientHostTemplate(假)'
        Assert-RefusedBeforeStop -Run $run -Label "静态预检 $mode" -HintPattern '没通过 -DryRun 静态预检'
        Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1' -Actual ($run.DevToolsCalls -join '|') -Because "静态预检 ${mode}:只允许执行过 -DryRun,Step 1 / Step 6 一次都不该被调"
        $reason = if ($mode -eq 'throw') { $run.Error } else { $run.Output }
        Assert-Match -Text $reason -Pattern 'LoadBalancer 却没有 -GateClientHostTemplate\(假\)' -Because "静态预检 ${mode}:下游给的原因要回显给操作者"
        if ($mode -eq 'exit') {
            Assert-Match -Text $run.Error -Pattern '退出码 3' -Because '非 0 退出码同样判为失败,不能只认 throw'
        }
    }
}

Test-Case 'dry-run:只预告静态预检,不执行' {
    $run = Invoke-RollbackInSandbox -DeployDefault '1' -GateRouterMode '1' -ExtraArgs @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }
    Assert-True -Condition ($null -eq $run.Error) -Because "dry-run 不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.DevToolsCalls.Count -Because 'dry-run 只打印命令,连 k8s-zone-up -DryRun 也不执行'
    Assert-Match -Text $run.Output -Pattern '静态预检:以 Step 6 同一组参数[^\r\n]*-DryRun' -Because 'dry-run 要预告 -Apply 时会先 -DryRun 一遍 Step 6'
}

# ─────────────────────────────────────────────────────────────────
# 回滚脚本入口:-ReleaseProfile / -LoginDevPasswordAuth / -ConfirmNoGatewayIngress(子进程 dry-run)
# ─────────────────────────────────────────────────────────────────

Test-Case '-ReleaseProfile / -LoginDevPasswordAuth:给了只透传到 Step 6,不给不透传;档位拼错在入口被拒' {
    $run = Invoke-RollbackDryRun -ExtraArguments @('-ReleaseProfile', 'prod', '-LoginDevPasswordAuth')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "显式档位与开关的 dry-run 不该失败。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    Assert-Match -Text $line -Pattern '(^|\s)-ReleaseProfile prod(\s|$)' -Because '以前 Step 6 恒按 dev 档部署,prod zone 回滚得到 dev 模式的 login 与占位密钥'
    Assert-Match -Text $line -Pattern '(^|\s)-LoginDevPasswordAuth True(\s|$)' -Because '开发口令认证不粘滞,dev zone 回滚漏掉它 login 就没有任何认证配置'
    $downLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-down[^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $downLine.Count -Because "dry-run 输出里应当恰有一行 Step 1 参数。输出: $($run.Output)"
    Assert-NotMatch -Text $downLine[0] -Pattern 'ReleaseProfile|LoginDevPasswordAuth' -Because 'zone-down 与档位、login 认证无关,不传'
    Assert-Match -Text $run.Output -Pattern 'release\s*:\s*-ReleaseProfile prod' -Because '摘要区要写明 Step 6 以哪个档位部署'

    $unset = Invoke-RollbackDryRun
    Assert-NotMatch -Text (Get-ZoneUpArgsLine -Output $unset.Output) -Pattern 'ReleaseProfile|LoginDevPasswordAuth' -Because '留空不透传:档位默认值只在 dev_tools.ps1 一处'
    Assert-Match -Text $unset.Output -Pattern 'release\s*:[^\r\n]*staging / prod zone 必须显式传 -ReleaseProfile' -Because '留空时摘要区要提醒 staging / prod 必须显式传'

    $bad = Invoke-RollbackDryRun -ExtraArguments @('-ReleaseProfile', 'production')
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because "档位拼错必须在入口被拒。输出: $($bad.Output)"
    Assert-Match -Text $bad.Output -Pattern 'ReleaseProfile' -Because '错误文本必须点名 ReleaseProfile'
    Assert-NotMatch -Text $bad.Output -Pattern 'Step 1:' -Because '拖到 Step 6 才由下游拒时,zone 已停服、数据已回档'
}

Test-Case '负向:-ConfirmNoGatewayIngress 与 -GatewayIngressHost 互斥,在入口被拒' {
    $run = Invoke-RollbackDryRun -ExtraArguments @('-ConfirmNoGatewayIngress', '-GatewayIngressHost', 'gw.example.com', '-GatewayTrustedProxies', '10.244.0.0/16')
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "两种相反的显式决定必须在入口被拒。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '互斥' -Because '错误文本要写明二者互斥,不能把其他执行错误误判为入口校验'
    Assert-NotMatch -Text $run.Output -Pattern 'Step 1:' -Because '入口校验必须先于任何步骤'
}

# ─────────────────────────────────────────────────────────────────
# dev_tools.ps1 的 k8s-zone-rollback 入口(子进程;不带 -RollbackApply,回滚脚本只打印,不碰任何集群)
# ─────────────────────────────────────────────────────────────────

$DevToolsRollbackBaseArgs = @(
    '-Command', 'k8s-zone-rollback',
    '-ZoneName', 'contract-test', '-ZoneId', '101',
    '-RollbackTargetTime', '2026-05-15T14:23:00Z',
    '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab',
    '-RollbackSkipMySqlPause'
)

function Invoke-DevToolsRollbackDryRun {
    param([string[]]$ExtraArguments = @())
    return Invoke-ToolScript -ScriptName 'dev_tools.ps1' -Arguments ($DevToolsRollbackBaseArgs + $ExtraArguments)
}

Test-Case 'dev_tools k8s-zone-rollback:入口参数与 -KubeContext / -KubeConfig 真正转到回滚脚本,Step 1、预检 kubectl、Step 6 同一集群' {
    # 以前 dev_tools.ps1 不转发 -KubeContext / -KubeConfig:回滚静默落到 kubectl 当前 context,删的是别的集群里的同名 namespace。
    $run = Invoke-DevToolsRollbackDryRun -ExtraArguments (ConvertTo-ArgumentList -Map $EntryPassthrough)
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "经 dev_tools.ps1 的回滚 dry-run 不该失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'k8s-zone-rollback \(DRY-RUN\)' -Because '不带 -RollbackApply 必须是 dry-run'
    $line = Get-ZoneUpArgsLine -Output $run.Output
    foreach ($name in $EntryPassthrough.Keys) {
        $value = [regex]::Escape($EntryPassthrough[$name])
        Assert-Match -Text $line -Pattern "(^|\s)-$name $value(\s|$)" -Because "dev_tools.ps1 必须把 -$name 转给回滚脚本,再由 Step 6 原样透传"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($line, "(^|\s)-$name\s").Count) -Because "-$name 只透传一次"
    }
    Assert-NotMatch -Text $line -Pattern '(^|\s)-(ReleaseProfile|LoginDevPasswordAuth|GateRouterMode|AllowDisruptiveSwitch)\s' -Because '没给的不转发:dev_tools.ps1 的默认 dev 档不能冒充显式值,默认值只在一处;切换确认闸没给就不能出现在 Step 6'
    $downLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-down[^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $downLine.Count -Because "应当恰有一行 Step 1 参数。输出: $($run.Output)"
    Assert-Match -Text $downLine[0] -Pattern '-KubeContext kind-mmorpg' -Because 'Step 1 删 namespace 必须落在指定集群'
    Assert-Match -Text $downLine[0] -Pattern ([regex]::Escape('-KubeConfig C:/tmp/kind-mmorpg.kubeconfig')) -Because 'Step 1 必须用同一份 kubeconfig'
    $preflightLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*kubectl get statefulset/gate [^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $preflightLine.Count -Because "-GateRouterMode 留空,dry-run 要预告预检的 kubectl。输出: $($run.Output)"
    Assert-Match -Text $preflightLine[0] -Pattern ([regex]::Escape('--context kind-mmorpg --kubeconfig C:/tmp/kind-mmorpg.kubeconfig')) -Because '预检读的必须是 Step 1 / Step 6 将要改动的同一集群'
}

Test-Case 'dev_tools k8s-zone-rollback:-LoginDevPasswordAuth、-RollbackConfirmNoGatewayIngress 与显式 -ReleaseProfile 转给回滚脚本' {
    $run = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-LoginDevPasswordAuth', '-RollbackConfirmNoGatewayIngress')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "以前 -LoginDevPasswordAuth 在这里被拒,dev zone 回滚后只能再跑一次 zone-up 补口令认证。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    Assert-Match -Text $line -Pattern '(^|\s)-LoginDevPasswordAuth True(\s|$)' -Because '开发口令认证不粘滞,要随 Step 6 重新生成;dev 档门禁由 k8s_deploy.ps1 把关'
    Assert-NotMatch -Text $line -Pattern 'Confirm' -Because '-ConfirmNoGatewayIngress 是回滚脚本自己的核对豁免,不下传给 k8s-zone-up'
    Assert-Match -Text $run.Output -Pattern 'ingress\s*:\s*-ConfirmNoGatewayIngress' -Because '-RollbackConfirmNoGatewayIngress 必须转成回滚脚本的 -ConfirmNoGatewayIngress'

    # 工作树可能是脏的:非 dev 档下 dev_tools.ps1 拒绝自动生成 tag,带 -AllowDirty 只为让本入口跑到回滚脚本(dry-run 不部署任何东西)。
    $staging = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-ReleaseProfile', 'staging', '-AllowDirty')
    Assert-Equal -Expected 0 -Actual $staging.ExitCode -Because "显式档位的 dry-run 不该失败。输出: $($staging.Output)"
    Assert-Match -Text (Get-ZoneUpArgsLine -Output $staging.Output) -Pattern '(^|\s)-ReleaseProfile staging(\s|$)' -Because '以前这里静默吞掉 -ReleaseProfile,Step 6 恒为 dev 档(安全方向的 fail-open)'
}

Test-Case 'dev_tools k8s-zone-rollback:-AllowDisruptiveSwitch 转给回滚脚本,只透传到 Step 6、不下传 Step 1' {
    # 以前 dev_tools.ps1 在这里拒绝该开关;现在同一口径透传(dev_tools.ps1 的 k8s-zone-rollback 分支),由回滚脚本只转给 Step 6:
    # 它只在 namespace 没删净、k8s_deploy.ps1 的集群现状闸真的触发时起作用,不豁免第 0 步的停服前核对。
    $run = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-AllowDisruptiveSwitch')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "经 dev_tools.ps1 的回滚 dry-run 不该失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'k8s-zone-rollback \(DRY-RUN\)' -Because '不带 -RollbackApply 必须是 dry-run'
    Assert-Match -Text $run.Output -Pattern 'switch gate\s*:\s*-AllowDisruptiveSwitch' -Because 'dev_tools.ps1 必须把开关真正转给回滚脚本,不能静默吞掉'
    $line = Get-ZoneUpArgsLine -Output $run.Output
    Assert-Match -Text $line -Pattern '(^|\s)-AllowDisruptiveSwitch True(\s|$)' -Because '回滚脚本必须把开关原样透传给 Step 6 的 k8s-zone-up'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($line, '(^|\s)-AllowDisruptiveSwitch\s').Count) -Because '-AllowDisruptiveSwitch 只透传一次'
    $downLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-down[^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $downLine.Count -Because "应当恰有一行 Step 1 参数。输出: $($run.Output)"
    Assert-NotMatch -Text $downLine[0] -Pattern 'AllowDisruptiveSwitch' -Because 'Step 1 只删 namespace,切换确认闸不下传给 k8s-zone-down'
}

# dev_tools.ps1 Invoke-K8sImage:按 AST 取出函数,$ScriptDir 指到假 k8s_image.ps1 所在的临时目录(照 k8s_deploy_contract.tests.ps1)。
function Get-DevToolsFunctionText {
    param([Parameter(Mandatory = $true)][string]$FunctionName)
    $path = Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "dev_tools.ps1 解析失败: $($parseErrors | Out-String)" }
    $definition = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $FunctionName }, $true)
    if ($null -eq $definition) { throw "dev_tools.ps1 里找不到函数 $FunctionName(契约测试的前提被改掉了)" }
    return $definition.Extent.Text
}

<#
.SYNOPSIS
    以给定的脚本级参数值调一次 dev_tools.ps1 的 Invoke-K8sImage -ImageCommand release-zone,返回 @{ Error; DownstreamCalled; DownstreamArgs }。
    被测函数按动态作用域读 dev_tools.ps1 的脚本级参数:这里在本函数作用域里设置 -Variables 给的那几个,其余未设即 $null(开关视为未指定)。
    下游是假 k8s_image.ps1,只把收到的参数($args 以空格拼接)写进标记文件:即使拒绝失效,也不会真的构建、推送或部署。
    假脚本不声明参数块,splat 进来的具名参数全部落进 $args,形如 "-Name:" 后跟值;没被调到时 DownstreamArgs 为 $null。
#>
function Invoke-DevToolsK8sImageWith {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-k8s-image-reject-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        $marker = Join-Path $fakeDir 'k8s_image.called'
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_image.ps1') -Encoding utf8 -Value ('Set-Content -LiteralPath ''{0}'' -Value ($args -join '' ''); exit 0' -f $marker.Replace("'", "''"))
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        . ([scriptblock]::Create((Get-DevToolsFunctionText -FunctionName 'Invoke-K8sImage')))
        $errorText = $null
        try { Invoke-K8sImage -ImageCommand 'release-zone' *>&1 | Out-Null } catch { $errorText = $_.Exception.Message }
        $called = Test-Path -LiteralPath $marker
        $downstreamArgs = if ($called) { Get-Content -LiteralPath $marker -Raw } else { $null }
        return [pscustomobject]@{ Error = $errorText; DownstreamCalled = $called; DownstreamArgs = $downstreamArgs }
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'dev_tools Invoke-K8sImage:-LoginDevPasswordAuth 在调 k8s_image.ps1 之前拒绝;-RequireClientEndpoint 原样透传给 k8s_image.ps1' {
    # 对照组:什么都不给时必须真的调到假下游,否则"没调到"的断言在这套替身下会空转通过。
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'RequireClientEndpoint' -Because '留空不透传,由 k8s_deploy.ps1 的默认值接管(默认值只在一处)'

    # -LoginDevPasswordAuth 只落在 login 上,k8s_image.ps1 不部署 login、也不声明它:显式给了必须拒绝而不是静默吞掉。
    $reject = Invoke-DevToolsK8sImageWith -Variables @{ LoginDevPasswordAuth = $true }
    Assert-True -Condition ($null -ne $reject.Error) -Because '-LoginDevPasswordAuth 经 k8s-release-* 不生效,显式给了必须拒绝'
    Assert-Match -Text $reject.Error -Pattern 'k8s-image-\* / k8s-release-\* 不支持 [^:]*-LoginDevPasswordAuth' -Because '错误文本必须点名 -LoginDevPasswordAuth'
    Assert-True -Condition (-not $reject.DownstreamCalled) -Because '-LoginDevPasswordAuth:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)'

    # -RequireClientEndpoint 以前在这里被拒;现在 k8s_image.ps1 声明并透传它(只参与 k8s_deploy.ps1 的组合预检,
    # 不改写 login / scene_manager 的 ConfigMap,给了由 k8s_image.ps1 告警说明)。
    $forward = Invoke-DevToolsK8sImageWith -Variables @{ RequireClientEndpoint = 'true' }
    Assert-True -Condition ($null -eq $forward.Error -and $forward.DownstreamCalled) -Because "-RequireClientEndpoint 不再拒绝,必须调到 k8s_image.ps1。错误: $($forward.Error)"
    Assert-Match -Text $forward.DownstreamArgs -Pattern '(^|\s)-RequireClientEndpoint:?\s*true(\s|$)' -Because 'dev_tools.ps1 必须把 -RequireClientEndpoint 原样透传给 k8s_image.ps1,不能静默吞掉'
}

exit (Complete-TestRun -SuiteName 'k8s_zone_rollback 透传与停服前预检')
