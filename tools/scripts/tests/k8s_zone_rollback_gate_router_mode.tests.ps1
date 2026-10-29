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
      - go/db 存盘消费组按 zone 拆分(2026-10-15,docs/design/db-consumer-group-per-zone.md §6):组名基名与分区数现读
        go/db/etc/db.yaml(读不到即拒);回档一律落到 zone 组 —— Step 5 带 Group=<基名>_z<id>、RetireGroup=<基名>、Partitions,
        Step 6 带 -DbConsumerGroup zone;Step 2 排空等待已删除;第 0 步 c 的 LAG 快照只告警、只计本 topic;第 0 步 d 在
        zone 组没有位点时以 dry-run 预演 Step 5,预演失败或 describe 判不出都在停服之前拒绝;-KafkaGroup 与 legacy 组相同时入口即拒。
      - R7 复核(2026-10-17):topic 按 db.yaml 的 TopicGeneration 推导,显式 -KafkaTopic 与推导值不同即入口拒绝;第 0 步 e 按 Step 6 的路径
        (kubectl exec 进 kafka-0,同一 -KubeContext、dev_tools.ps1 默认 infra namespace)预读两组位点,读不到或与本机 CLI 不一致都在停服之前拒绝。
      - 回档丢弃语义(负责人 2026-10-17 裁决「丢弃 TargetTime 之后的写」,2026-10-18 落码):Step 5 与第 0 步 d 一律 -ToLatest、不再带 ToDatetime;
        -Apply 的调用顺序固定为 第 0 步 f 工具预检 → Step 1(及 namespace 已删检查)→ 1b(工具 stopped、--state、两次日志末端)→ PITR →
        4a 盘点 → Step 5(记切点)→ 4b 丢弃 → 5b(切点复核 + 工具 verify)→ Step 6;各步的拒绝分别落在停服前 / PITR 前 / Step 5 前 / Step 6 前;
        blocked 玩家与 -AcceptBlockedPlayers 待负责人裁决,fail-closed;dev_tools.ps1 的新透传参数给了才转发。
        回档复核修正(同日):1b 内先 Kafka 两项、后工具 stopped(停服基线最后取,[1]);inventory / purge / verify 同一组判定参数([2]);
        盘点之后、Step 5 之前复核 namespace 与 zone 组([8]);中止 / 放弃路径的 zone-up 不带 -DbConsumerGroup([9]);入口决定存档 entry-state.json([10]);
        dev_tools 透传 -RollbackAllowEmptyTarget / -RollbackAcknowledgeNoPostTWrites([11])。
      - dev_tools.ps1 的 k8s-zone-rollback 入口:全部入口参数与 -KubeContext / -KubeConfig 真正转到回滚脚本
        (以前 -KubeContext 被静默吞掉,回滚落到 kubectl 当前 context);-AllowDisruptiveSwitch 同样转给回滚脚本,
        由它只透传到 Step 6、不下传 Step 1;Invoke-K8sImage 拒绝 -LoginDevPasswordAuth(k8s_image.ps1 不部署 login),
        -RequireClientEndpoint 原样透传给 k8s_image.ps1(只参与 k8s_deploy.ps1 的组合预检,不改写 login / scene_manager 的 ConfigMap)。

    子进程用例走 dry-run(不带 -Apply):脚本只打印每一步要执行的命令,不调 kubectl / kafka 工具 / 回档工具。
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
Write-Host "=== k8s_zone_rollback.ps1 -GateRouterMode / external entry argument forwarding and preflight contract tests ==="

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
# 预检那一条 kubectl 的固定前缀(一次取回两种 gate、Ingress 与 scene Deployment,见 k8s_zone_rollback.ps1 Get-ZoneEntryKubectlArgs;
# scene Deployment 供 B0-4 nofile 模式核对,2026-10-22)。
$PreflightKubectlPrefix = "get statefulset/gate deployment/gate ingress/gateway deployment/scene deployment/scene-world deployment/scene-instance -n $SandboxNamespace -o json --ignore-not-found"

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

# 进程内沙箱 Invoke-RollbackInSandbox(与 k8s_zone_rollback_discard.tests.ps1 共用一份):每一步都落在假下游上,
# 不碰任何集群、Redis、Kafka、MySQL;参数与返回值见 lib/k8s_zone_rollback_sandbox.ps1。
. "$PSScriptRoot/lib/k8s_zone_rollback_sandbox.ps1"

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

Test-Case '-Apply 三项(-GateRouterMode / -ClientEntryMode / -GatewayIngressHost)与 -NodeOpenFileLimitMode 都显式:不读集群' {
    # 沙箱缺省显式传 -NodeOpenFileLimitMode enforce;它留空时仍要读集群,见下面 B0-4 一节。
    $extra = @{ ClientEntryMode = 'podip'; GatewayIngressHost = 'gw.example.com'; GatewayTrustedProxies = '10.244.0.0/16' }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $extra
    Assert-True -Condition ($null -eq $run.Error) -Because "四项都显式时不该被预检拦。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.KubectlCalls.Count -Because '四项都显式 = 操作者已决定,不核对集群'
    Assert-Equal -Expected 0 -Actual $run.AgonesCalls.Count -Because 'nofile 模式显式给值,不探测 Agones、不读 Fleet'
    Assert-Match -Text $run.Output -Pattern 'All three entry values and -NodeOpenFileLimitMode were supplied explicitly; skipping cluster reads' -Because '输出要写明本次没有核对集群'
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
    Assert-Match -Text $passed.Output -Pattern "Current cluster state \(for comparison only\): gate 为 StatefulSet;模板地址 env:[^\r\n]*GATE_NODE_PORT_BASE='30100'" -Because '显式给了 -ClientEntryMode 时也要把集群现状打出来,供操作者核对照传的地址参数'
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
    Assert-NotMatch -Text $refused.Output -Pattern 'leaving it unset will preserve Ingress' -Because '判不出时不得打印放行口径的绿字,误导操作者'
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
    Assert-Match -Text $run.Output -Pattern 'Explicit -ConfirmNoGatewayIngress' -Because '输出要写明 Ingress 这一项没有核对、是操作者的决定'
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
# B0-4(2026-10-22):-NodeOpenFileLimitMode 留空时第 0 步 a 的 nofile 模式核对(-Apply,沙箱)
# Step 1 删掉整个 namespace,Step 6 只按本次参数重建;以 NODE_OPEN_FILE_LIMIT_MODE=warn 运行的 zone 漏传会落回 enforce,
# 节点池硬限仍不够时 gate / scene 在 Step 6(已停服、已回档、积压已丢弃)拒启。以前第 0 步不读这个 env,只靠摘要提醒。
# 沙箱缺省显式传 -NodeOpenFileLimitMode enforce(不核对);本节用例一律传 -NodeOpenFileLimitMode ''(留空)。
# 折算口径同进程(open_file_limit.h ParseMode):只有逐字 "warn" 是豁免,未设、"enforce" 与其它值都是 enforce。
# ─────────────────────────────────────────────────────────────────

$OpenFileLimitEnvName = 'NODE_OPEN_FILE_LIMIT_MODE'
# 装了 Agones 时 agones.dev/v1 发现文档的回应(同 k8s_client_entry_contract.tests.ps1 的 CRD 探测用例)。
$AgonesInstalledReply = @{ ExitCode = 0; Stdout = '{"kind":"APIResourceList","groupVersion":"agones.dev/v1","resources":[{"name":"fleets"},{"name":"gameservers"}]}' }
# 只核对 nofile 模式:路由模式由各用例显式给 -GateRouterMode 1,入口形态与 Ingress 在这里显式给,不进问题列表。
$OpenFileLimitQuietEntry = @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }
$OpenFileLimitUnknownHint = '显式传 -NodeOpenFileLimitMode warn 或 -NodeOpenFileLimitMode enforce'

<#
.SYNOPSIS
    C++ 工作负载容器的 env:POD_IP 走 valueFrom(干扰项);-RouterValue 非空才写 GATE_CLIENT_RPC_ROUTER(给 gate);
    -Value 为 $null 时不写 NODE_OPEN_FILE_LIMIT_MODE(k8s_deploy.ps1 留空时的形状),否则原样写成字面值。
#>
function New-OpenFileLimitEnv {
    param(
        [AllowNull()]$Value = $null,
        [string]$RouterValue = ''
    )

    $entries = @(@{ name = 'POD_IP'; valueFrom = @{ fieldRef = @{ fieldPath = 'status.podIP' } } })
    if ($RouterValue -ne '') { $entries += @(@{ name = 'GATE_CLIENT_RPC_ROUTER'; value = $RouterValue }) }
    if ($null -ne $Value) { $entries += @(@{ name = $OpenFileLimitEnvName; value = [string]$Value }) }
    return $entries
}

<#
.SYNOPSIS
    scene 工作负载(只含核对会看的字段),容器名即工作负载名。Deployment 的 Pod 模板在 spec.template.spec;
    Fleet 在 spec.template.spec.template.spec(Fleet → GameServer → Pod,k8s_deploy.ps1 New-SceneFleetYaml)。
#>
function New-SceneWorkload {
    param(
        [ValidateSet('Deployment', 'Fleet')][string]$Kind = 'Deployment',
        [string]$Name = 'scene',
        [AllowNull()][object[]]$PodEnv = $null
    )

    $container = [ordered]@{ name = $Name; image = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab' }
    if ($null -ne $PodEnv) { $container.env = $PodEnv }
    $podSpec = [ordered]@{ containers = @($container) }
    if ($Kind -eq 'Fleet') {
        $apiVersion = 'agones.dev/v1'
        $spec = [ordered]@{ replicas = 1; template = [ordered]@{ spec = [ordered]@{ health = [ordered]@{ disabled = $false }; template = [ordered]@{ spec = $podSpec } } } }
    }
    else {
        $apiVersion = 'apps/v1'
        $spec = [ordered]@{ template = [ordered]@{ spec = $podSpec } }
    }
    return [ordered]@{ apiVersion = $apiVersion; kind = $Kind; metadata = [ordered]@{ name = $Name; namespace = $SandboxNamespace }; spec = $spec }
}

# 假 Fleet 读取的成功回应(多个具名资源 + --ignore-not-found:找到的合成一个 List,一个都没有时无输出)。
function New-SceneFleetReply {
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Fleets)

    if ($Fleets.Count -eq 0) { return @{ ExitCode = 0; Stdout = '' } }
    $list = [ordered]@{ apiVersion = 'v1'; kind = 'List'; items = @($Fleets); metadata = [ordered]@{ resourceVersion = '' } }
    return @{ ExitCode = 0; Stdout = ($list | ConvertTo-Json -Depth 30) }
}

Test-Case 'B0-4 -Apply 留空 -NodeOpenFileLimitMode + gate / scene 任一带 warn(gate Deployment / external StatefulSet / 只有一个 scene Deployment / Agones Fleet):停服之前拒绝,报错给出照传的 -NodeOpenFileLimitMode warn' {
    $plainGate = New-GateWorkload -GateEnv (New-OpenFileLimitEnv -RouterValue '1')
    $externalEntry = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; GateNodePortBase = 30100; ClientPublicHost = '203.0.113.10'; ConfirmNoGatewayIngress = $true }
    $cases = @(
        @{ Label = 'gate Deployment 与 scene 都带 warn(上一次 zone-up 传了 -NodeOpenFileLimitMode warn)'; Extra = $OpenFileLimitQuietEntry
            Items = @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -Value 'warn' -RouterValue '1')), (New-SceneWorkload -PodEnv (New-OpenFileLimitEnv -Value 'warn')))
            Detail = "Deployment gate $OpenFileLimitEnvName=`"warn`"" },
        @{ Label = 'external gate StatefulSet 带 warn'; Extra = $externalEntry
            Items = @((New-GateWorkload -Kind StatefulSet -GateEnv ((New-ExternalGateEnv -RouterValue '1') + @(@{ name = $OpenFileLimitEnvName; value = 'warn' }))))
            Detail = "StatefulSet gate $OpenFileLimitEnvName=`"warn`"" },
        @{ Label = '只有 scene-world 手设了 warn(runbook §3.1 的 kubectl set env)'; Extra = $OpenFileLimitQuietEntry
            Items = @($plainGate, (New-SceneWorkload -Name 'scene-world' -PodEnv (New-OpenFileLimitEnv -Value 'warn')), (New-SceneWorkload -Name 'scene-instance'))
            Detail = "Deployment scene-world $OpenFileLimitEnvName=`"warn`"" }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode '' -ExtraArgs $c.Extra -KubectlReply (New-KubectlReply -Items $c.Items)
        Assert-RefusedBeforeStop -Run $run -Label $c.Label -HintPattern '请照传 -NodeOpenFileLimitMode warn'
        Assert-Match -Text $run.Error -Pattern 'nofile 豁免:[^\r\n]*当前为 "warn"' -Because "$($c.Label):以 warn 运行的 zone 留空会被 Step 6 落回 enforce,节点池硬限不够时在已停服、已回档之后拒启"
        Assert-Match -Text $run.Error -Pattern ([regex]::Escape($c.Detail)) -Because "$($c.Label):错误文本要点名带 warn 的工作负载与原值"
        Assert-Match -Text $run.Error -Pattern '显式传 -NodeOpenFileLimitMode enforce' -Because "$($c.Label):硬限已修好、要回到 enforce 的出路也要给"
        Assert-NotMatch -Text $run.Error -Pattern 'gate 路由模式:|客户端入口形态:|gateway Ingress:' -Because "$($c.Label):入口三项已显式,问题只有 nofile 一项"
        Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because "$($c.Label):gate 与 scene Deployment 跟入口同一次读"
        Assert-Match -Text $run.KubectlCalls[0] -Pattern ('^' + [regex]::Escape($PreflightKubectlPrefix)) -Because "$($c.Label):同一次只读 kubectl 带上三个 scene Deployment"
        Assert-Equal -Expected 1 -Actual $run.AgonesCalls.Count -Because "$($c.Label):只探测一次 Agones;没装(NotFound)就不读 Fleet"
    }

    # Agones 编排的 scene:gate 不带豁免,只有 Fleet scene-instance 带 warn。
    $fleetReply = New-SceneFleetReply -Fleets @((New-SceneWorkload -Kind Fleet -Name 'scene-world'), (New-SceneWorkload -Kind Fleet -Name 'scene-instance' -PodEnv (New-OpenFileLimitEnv -Value 'warn')))
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode '' -ExtraArgs $OpenFileLimitQuietEntry `
        -KubectlReply (New-KubectlReply -Items @($plainGate)) -AgonesDiscoveryReply $AgonesInstalledReply -SceneFleetReply $fleetReply
    Assert-RefusedBeforeStop -Run $run -Label 'Agones Fleet 带 warn' -HintPattern '请照传 -NodeOpenFileLimitMode warn'
    Assert-Match -Text $run.Error -Pattern ([regex]::Escape("Fleet scene-instance $OpenFileLimitEnvName=`"warn`"")) -Because 'Fleet 的 env 在 spec.template.spec.template.spec(Fleet → GameServer → Pod),必须读到'
    Assert-Equal -Expected 2 -Actual $run.AgonesCalls.Count -Because '装了 Agones:先探测一次,再按名读一次 Fleet'
    Assert-Match -Text $run.AgonesCalls[0] -Pattern '^get --raw /apis/agones\.dev/v1$' -Because '只读 agones.dev 一个组的发现文档(lib Test-AgonesFleetCrdPresent)'
    Assert-Match -Text $run.AgonesCalls[1] -Pattern ('^get fleets\.agones\.dev scene scene-world scene-instance -n ' + [regex]::Escape($SandboxNamespace) + ' -o json --ignore-not-found') -Because 'Fleet 只读、读本 zone namespace、按名读'
}

Test-Case 'B0-4 -Apply 显式 -NodeOpenFileLimitMode warn / enforce:不核对(显式即操作者的决定),集群带 warn 也放行;静态预检与 Step 6 收到同值' {
    $items = @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -Value 'warn' -RouterValue '1')), (New-SceneWorkload -PodEnv (New-OpenFileLimitEnv -Value 'warn')))
    foreach ($mode in @('warn', 'enforce')) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -NodeOpenFileLimitMode $mode -KubectlReply (New-KubectlReply -Items $items) `
            -AgonesDiscoveryReply @{ Throw = '显式给值时不该探测 Agones' }
        Assert-True -Condition ($null -eq $run.Error) -Because "显式 -NodeOpenFileLimitMode $mode 不该被预检拦(确认硬限已修好时传 enforce 回到 enforce)。错误: $($run.Error)"
        Assert-Equal -Expected 0 -Actual $run.AgonesCalls.Count -Because "显式 ${mode}:不探测 Agones、不读 Fleet"
        Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because "显式 ${mode}:入口三项仍留空,照旧读一次集群,只核对它们"
        Assert-Match -Text $run.Output -Pattern "Explicit -NodeOpenFileLimitMode ${mode}:" -Because '输出要写明 nofile 模式这一项没有核对、是操作者的决定'
        Assert-NotMatch -Text $run.Output -Pattern 'kubectl get statefulset/gate[^\r\n]*NODE_OPEN_FILE_LIMIT_MODE' -Because '显式给值后,预检那一行不再列 nofile 核对项'
        $up = Get-BoundLine -Run $run -Command 'k8s-zone-up'
        Assert-Match -Text $up -Pattern "(^| |;)NodeOpenFileLimitMode=$mode(;|$)" -Because "Step 6 必须真正把 $mode 传给 dev_tools.ps1,不只是打印出来"
        Assert-Equal -Expected ($up -replace '^\S+ ', '') -Actual ((Get-BoundLine -Run $run -Command 'k8s-zone-up/dry-run') -replace '^\S+ ', '') -Because '静态预检的就是 Step 6 将要执行的那一份'
        Assert-NotMatch -Text (Get-BoundLine -Run $run -Command 'k8s-zone-down') -Pattern 'NodeOpenFileLimitMode' -Because 'Step 1 只删 namespace,不传 nofile 开关'
    }
}

Test-Case 'B0-4 -Apply 留空 + gate / scene 都不是 warn(未设 / "enforce" / 进程认不出的大写值 / 装了 Agones 而 Fleet 未设或没有 Fleet):放行,Step 6 不透传,现状随存档落盘' {
    $plainGate = New-GateWorkload -GateEnv (New-OpenFileLimitEnv -RouterValue '1')
    $cases = @(
        @{ Label = '都未设'; Items = @($plainGate, (New-SceneWorkload)); AgonesCalls = 1 },
        @{ Label = 'gate "enforce"、scene-world 大写 "WARN"(进程按 enforce)'; AgonesCalls = 1
            Items = @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -Value 'enforce' -RouterValue '1')), (New-SceneWorkload -Name 'scene-world' -PodEnv (New-OpenFileLimitEnv -Value 'WARN')))
            Detail = "Deployment scene-world $OpenFileLimitEnvName=`"WARN`"" },
        @{ Label = 'Agones 编排、Fleet 未设'; Items = @($plainGate); AgonesCalls = 2; Agones = $AgonesInstalledReply
            Fleet = (New-SceneFleetReply -Fleets @((New-SceneWorkload -Kind Fleet -Name 'scene-world'), (New-SceneWorkload -Kind Fleet -Name 'scene-instance'))) },
        @{ Label = '装了 Agones 但 namespace 里没有 Fleet'; Items = @($plainGate, (New-SceneWorkload)); AgonesCalls = 2; Agones = $AgonesInstalledReply; Fleet = (New-SceneFleetReply -Fleets @()) }
    )
    foreach ($c in $cases) {
        $sandboxArgs = @{
            Apply = $true; DeployDefault = '1'; GateRouterMode = '1'; NodeOpenFileLimitMode = ''
            ExtraArgs = ($OpenFileLimitQuietEntry + @{ RollbackId = 'z101-nofile-pass' }); KubectlReply = (New-KubectlReply -Items $c.Items)
        }
        if ($c.ContainsKey('Agones')) { $sandboxArgs.AgonesDiscoveryReply = $c.Agones; $sandboxArgs.SceneFleetReply = $c.Fleet }
        $run = Invoke-RollbackInSandbox @sandboxArgs
        Assert-True -Condition ($null -eq $run.Error) -Because "$($c.Label):没有 warn,留空与 Step 6 一致,不该拒。错误: $($run.Error)"
        Assert-Equal -Expected 1 -Actual $run.KubectlCalls.Count -Because "$($c.Label):入口三项都显式而 nofile 留空,仍要读一次集群"
        Assert-Equal -Expected $c.AgonesCalls -Actual $run.AgonesCalls.Count -Because "$($c.Label):没装 Agones 只探测一次,装了再读一次 Fleet"
        Assert-NotMatch -Text $run.Output -Pattern 'All three entry values and -NodeOpenFileLimitMode were supplied explicitly' -Because "$($c.Label):nofile 留空时不能写成没有核对集群"
        Assert-Match -Text $run.Output -Pattern 'Current gate / scene nofile mode "enforce"[^\r\n]*leaving -NodeOpenFileLimitMode unset will preserve the mode' -Because "$($c.Label):放行时写明核对结论"
        if ($c.ContainsKey('Detail')) {
            Assert-Match -Text $run.Output -Pattern ([regex]::Escape($c.Detail)) -Because "$($c.Label):认不出的值也要原样打出来,操作者能看出拼错的开关没有生效"
        }
        Assert-NotMatch -Text (Get-BoundLine -Run $run -Command 'k8s-zone-up') -Pattern 'NodeOpenFileLimitMode=' -Because "$($c.Label):留空不透传(默认值只在 k8s_deploy.ps1 一处)"
        Assert-Match -Text ([string]$run.ReportFiles['entry-state.json']) -Pattern '"OpenFileLimitMode": "enforce"' -Because "$($c.Label):核对过的 nofile 现状随集群现状存档,同一 rid 重跑、namespace 已删时据此核对"
    }

    # 探测与 Fleet 读取必须落在 Step 1 / Step 6 将要改动的同一集群。
    $extra = $OpenFileLimitQuietEntry + @{ KubeContext = 'kind-mmorpg'; KubeConfig = 'C:/tmp/kind-mmorpg.kubeconfig' }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode '' -ExtraArgs $extra -KubectlReply (New-KubectlReply -Items @($plainGate)) `
        -AgonesDiscoveryReply $AgonesInstalledReply -SceneFleetReply (New-SceneFleetReply -Fleets @())
    Assert-True -Condition ($null -eq $run.Error) -Because "带 -KubeContext / -KubeConfig 不该被拦。错误: $($run.Error)"
    Assert-Equal -Expected 2 -Actual $run.AgonesCalls.Count -Because '探测一次、读一次 Fleet'
    foreach ($call in $run.AgonesCalls) {
        Assert-Match -Text $call -Pattern ('^' + [regex]::Escape('--context kind-mmorpg --kubeconfig C:/tmp/kind-mmorpg.kubeconfig ')) -Because '探测与 Fleet 读取不能落到 kubectl 当前 context'
    }
}

Test-Case 'B0-4 -Apply 留空 + 判不出(kubectl 失败 / 不可用、namespace 已删且无存档、只剩 scene、Agones 探测失败、Fleet 读取失败、valueFrom、变量重复):一律停服之前拒绝' {
    $plainGate = New-GateWorkload -GateEnv (New-OpenFileLimitEnv -RouterValue '1')
    $plainReply = New-KubectlReply -Items @($plainGate)
    $cases = @(
        @{ Label = 'kubectl 以非 0 退出'; Reply = @{ ExitCode = 1; Output = 'Error from server (Forbidden): deployments.apps "scene" is forbidden' }; Detail = 'Forbidden' },
        @{ Label = 'kubectl 不可用'; Reply = @{ Throw = "The term 'kubectl' is not recognized" }; Detail = 'kubectl 调用失败' },
        @{ Label = 'namespace 已删、没有存档'; Reply = (New-KubectlReply -Items @()); Detail = 'namespace 里没有 gate' },
        # 只剩 scene 证明不了 gate 回滚前的取值(namespace 多半正在删)。
        @{ Label = '只剩 scene、没有 gate'; Reply = (New-KubectlReply -Items @((New-SceneWorkload))); Detail = 'namespace 里没有 gate' },
        @{ Label = 'Agones 探测 Forbidden(查不到不等于没装)'; Reply = $plainReply; Agones = @{ ExitCode = 1; Stderr = 'Error from server (Forbidden): forbidden' }; Detail = '无法确认集群是否安装了 Agones' },
        @{ Label = 'Fleet 读取失败'; Reply = $plainReply; Agones = $AgonesInstalledReply; Fleet = @{ ExitCode = 1; Stderr = 'Error from server (Forbidden): fleets.agones.dev is forbidden' }; Detail = '读不到 scene Fleet' },
        @{ Label = 'Fleet 输出不是 JSON'; Reply = $plainReply; Agones = $AgonesInstalledReply; Fleet = @{ ExitCode = 0; Stdout = '<html>' }; Detail = '读不到 scene Fleet:kubectl 输出不是合法的 JSON' },
        @{ Label = 'gate 的变量用 valueFrom 注入'; Detail = 'valueFrom'
            Reply = (New-KubectlReply -Items @((New-GateWorkload -GateEnv @(@{ name = 'GATE_CLIENT_RPC_ROUTER'; value = '1' }, @{ name = $OpenFileLimitEnvName; valueFrom = @{ configMapKeyRef = @{ name = 'nofile'; key = 'mode' } } })))) },
        # 第一条恰是 enforce:取第一条就会错放行。
        @{ Label = 'scene 里变量重复'; Detail = '出现 2 次'
            Reply = (New-KubectlReply -Items @($plainGate, (New-SceneWorkload -PodEnv @(@{ name = $OpenFileLimitEnvName; value = 'enforce' }, @{ name = $OpenFileLimitEnvName; value = 'warn' })))) }
    )
    foreach ($c in $cases) {
        $sandboxArgs = @{ Apply = $true; DeployDefault = '1'; GateRouterMode = '1'; NodeOpenFileLimitMode = ''; ExtraArgs = $OpenFileLimitQuietEntry; KubectlReply = $c.Reply }
        if ($c.ContainsKey('Agones')) { $sandboxArgs.AgonesDiscoveryReply = $c.Agones }
        if ($c.ContainsKey('Fleet')) { $sandboxArgs.SceneFleetReply = $c.Fleet }
        $run = Invoke-RollbackInSandbox @sandboxArgs
        Assert-RefusedBeforeStop -Run $run -Label $c.Label -HintPattern $OpenFileLimitUnknownHint
        Assert-Match -Text $run.Error -Pattern 'nofile 豁免:[^\r\n]*当前为 \(读不到\)' -Because "$($c.Label):判不出就无法证明不会翻转(fail-closed)"
        Assert-Match -Text $run.Error -Pattern ([regex]::Escape($c.Detail)) -Because "$($c.Label):错误文本要带上判不出的原因"
        Assert-NotMatch -Text $run.Error -Pattern 'gate 路由模式:|客户端入口形态:|gateway Ingress:' -Because "$($c.Label):入口三项已显式,问题只有 nofile 一项"
    }
}

Test-Case 'B0-4 -Apply 留空:放行条件跟着 k8s_deploy.ps1 的 -NodeOpenFileLimitMode 默认值走(不在回滚脚本里写死「留空 = enforce」),读不到默认值即拒' {
    $items = @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -RouterValue '1')), (New-SceneWorkload))
    $common = @{ Apply = $true; GateRouterMode = '1'; NodeOpenFileLimitMode = ''; ExtraArgs = $OpenFileLimitQuietEntry; KubectlReply = (New-KubectlReply -Items $items) }

    $warnDefault = Invoke-RollbackInSandbox @common -DeployDefault '1' -DeployOpenFileLimitDefault 'warn'
    Assert-RefusedBeforeStop -Run $warnDefault -Label '默认值 warn、集群 enforce' -HintPattern $OpenFileLimitUnknownHint
    Assert-Match -Text $warnDefault.Error -Pattern '默认值 "warn"\(gate / scene 按 warn 运行\)' -Because '默认值必须从 k8s_deploy.ps1 读:那边改成 warn 时,留空同样是一次翻转(enforce → warn)'
    Assert-Match -Text $warnDefault.Error -Pattern '当前为 "enforce"' -Because '要带上集群现状'

    $missing = Invoke-RollbackInSandbox @common -DeployDefault '1' -DeployOpenFileLimitDefault 'missing'
    Assert-RefusedBeforeStop -Run $missing -Label '读不到默认值' -HintPattern $OpenFileLimitUnknownHint
    Assert-Match -Text $missing.Error -Pattern '-NodeOpenFileLimitMode 默认值不是字面常量或文件无法解析' -Because '读不到默认值就不知道 Step 6 会以哪种模式拉起(fail-closed)'

    # 真 k8s_deploy.ps1 的默认值必须能按语法树读成字面常量、且折算为 enforce(负责人 2026-10-17:缺省拒启 + 显式豁免);读不出时 -Apply 留空永远被拒。
    $real = Invoke-RollbackInSandbox @common -DeployDefault 'real'
    Assert-True -Condition ($null -eq $real.Error) -Because "真 k8s_deploy.ps1 的默认值下,集群 enforce + 留空必须放行。错误: $($real.Error)"
}

Test-Case '[10] B0-4 nofile 现状随集群现状存档(Schema 3,不进入口决定):同一 rid 重跑、namespace 已删时按存档核对;首次显式给值或旧存档没有该字段时判为读不到,显式传值放行' {
    $live = New-KubectlReply -Items @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -RouterValue '1')), (New-SceneWorkload))
    $gone = New-KubectlReply -Items @()
    $deleted = @{ ExitCode = 0; Output = '' }

    $first = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode '' -ExtraArgs ($OpenFileLimitQuietEntry + @{ RollbackId = 'z101-nofile' }) -KubectlReply $live
    Assert-True -Condition ($null -eq $first.Error) -Because "首次运行不该失败。错误: $($first.Error)"
    $saved = [string]$first.ReportFiles['entry-state.json']
    Assert-Match -Text $saved -Pattern '"Schema": 3' -Because '集群现状加了 nofile 模式,存档升到 Schema 3'
    Assert-Match -Text $saved -Pattern '"OpenFileLimitMode": "enforce"' -Because '核对过的 nofile 现状要落盘'
    Assert-NotMatch -Text $saved -Pattern '"NodeOpenFileLimitMode"' -Because 'nofile 模式不进入口决定:显式值重跑可以改(硬限没修好时改传 warn 是合法出路)'
    $seed = @{ 'entry-state.json' = $saved; 'deploy-facts.json' = $first.ReportFiles['deploy-facts.json'] }
    $rerunArgs = $OpenFileLimitQuietEntry + @{ RollbackId = 'z101-nofile' }

    $reused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode '' -ExtraArgs $rerunArgs -KubectlReply $gone -SeedReportFiles $seed -SnapshotReply $deleted
    Assert-True -Condition ($null -eq $reused.Error) -Because "namespace 已删时按存档的 nofile 现状核对,不再要操作者凭记忆重传。错误: $($reused.Error)"
    Assert-Match -Text $reused.Output -Pattern 'checking against the cluster state saved by the same -RollbackId' -Because '要写明这次核对用的是存档'
    Assert-Match -Text $reused.Output -Pattern 'Current gate / scene nofile mode "enforce"' -Because '存档里的 nofile 现状参与核对'

    $switched = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -NodeOpenFileLimitMode 'warn' -ExtraArgs $rerunArgs -KubectlReply $gone -SeedReportFiles $seed -SnapshotReply $deleted
    Assert-True -Condition ($null -eq $switched.Error) -Because "显式值不按存档比较,重跑改传 warn 必须放行。错误: $($switched.Error)"
    Assert-Match -Text (Get-BoundLine -Run $switched -Command 'k8s-zone-up') -Pattern '(^| |;)NodeOpenFileLimitMode=warn(;|$)' -Because 'Step 6 收到本次的显式值'

    # 首次显式给值:没读 Fleet,存档里的 nofile 现状记为判不出;重跑留空且 namespace 已删时拒绝。
    $explicitFirst = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -NodeOpenFileLimitMode 'warn' -ExtraArgs @{ RollbackId = 'z101-nofile-explicit' } `
        -KubectlReply (New-KubectlReply -Items @((New-GateWorkload -GateEnv (New-OpenFileLimitEnv -Value 'warn' -RouterValue '1'))))
    Assert-True -Condition ($null -eq $explicitFirst.Error) -Because "首次显式 warn 不该失败。错误: $($explicitFirst.Error)"
    $explicitSaved = [string]$explicitFirst.ReportFiles['entry-state.json']
    Assert-Match -Text $explicitSaved -Pattern '"OpenFileLimitMode": null' -Because '没核对就不能记成某个模式,否则重跑留空会拿没读全的现状放行'
    $explicitSeed = @{ 'entry-state.json' = $explicitSaved; 'deploy-facts.json' = $explicitFirst.ReportFiles['deploy-facts.json'] }
    $explicitRerun = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -NodeOpenFileLimitMode '' -ExtraArgs @{ RollbackId = 'z101-nofile-explicit' } `
        -KubectlReply $gone -SeedReportFiles $explicitSeed -SnapshotReply $deleted
    Assert-RefusedBeforeStop -Run $explicitRerun -Label '首次显式、重跑留空、namespace 已删' -HintPattern $OpenFileLimitUnknownHint
    Assert-Match -Text $explicitRerun.Error -Pattern '当前为 \(读不到\)\(未核对' -Because '要写明存档里的 nofile 现状没核对过'
    Assert-NotMatch -Text $explicitRerun.Error -Pattern 'gate 路由模式:|客户端入口形态:|gateway Ingress:' -Because '其余三项照旧按存档的集群现状核对通过'

    # Schema 3 之前的存档:集群现状没有 OpenFileLimitMode。入口三项按它照常核对,nofile 判为读不到;显式给值放行,存档不改写。
    $oldArchive = @(
        '{',
        '  "Schema": 2,',
        '  "RollbackId": "z101-nofile-old",',
        '  "Namespace": "mmorpg-zone-contract-test",',
        '  "CapturedAtUtc": "2026-10-20T00:00:00.0000000Z",',
        '  "ClusterState": { "RouterMode": "1", "RouterDetail": "old", "EntryMode": "podip", "EntryDetail": "old", "IngressKnown": true, "Ingress": null, "IngressDetail": "old", "NamespaceEmpty": false },',
        '  "Decision": { "GateRouterMode": "1", "ClientEntryMode": "podip", "GatewayIngress": "(none)" },',
        '  "LoginAuthState": null',
        '}'
    ) -join "`n"
    $oldSeed = @{ 'entry-state.json' = $oldArchive }
    $oldRefused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -NodeOpenFileLimitMode '' -ExtraArgs @{ RollbackId = 'z101-nofile-old' } -KubectlReply $gone -SeedReportFiles $oldSeed
    Assert-RefusedBeforeStop -Run $oldRefused -Label '旧存档没有 nofile 现状' -HintPattern $OpenFileLimitUnknownHint
    Assert-Match -Text $oldRefused.Error -Pattern '早于 nofile 模式核对' -Because '要写明是存档太旧,不是集群读不到'
    Assert-NotMatch -Text $oldRefused.Error -Pattern 'gate 路由模式:|客户端入口形态:|gateway Ingress:' -Because '旧存档不能连带把入口三项判成读不到'
    $oldExplicit = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -NodeOpenFileLimitMode 'enforce' -ExtraArgs @{ RollbackId = 'z101-nofile-old' } -KubectlReply $gone -SeedReportFiles $oldSeed
    Assert-True -Condition ($null -eq $oldExplicit.Error) -Because "显式给值即决定,旧存档不挡路(报错里给的出路要走得通)。错误: $($oldExplicit.Error)"
    Assert-Equal -Expected $oldArchive.Trim() -Actual ([string]$oldExplicit.ReportFiles['entry-state.json']).Trim() -Because '同一 rid 已有存档时不覆盖'
}

Test-Case 'B0-4:回滚脚本按名读的 scene 工作负载与 k8s_deploy.ps1 Resolve-SceneDeploymentPlan 生成的名字一致(legacy 单池与拆分两种)' {
    $parseErrors = $null
    $deployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
    Assert-Equal -Expected 0 -Actual $parseErrors.Count -Because 'k8s_deploy.ps1 必须能解析'
    $rollbackAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_zone_rollback.ps1'), [ref]$null, [ref]$parseErrors)
    Assert-Equal -Expected 0 -Actual $parseErrors.Count -Because 'k8s_zone_rollback.ps1 必须能解析'
    $planFunction = $deployAst.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Resolve-SceneDeploymentPlan' }, $true)
    $namesFunction = $rollbackAst.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Get-ZoneSceneWorkloadNames' }, $true)
    Assert-True -Condition ($null -ne $planFunction -and $null -ne $namesFunction) -Because '两边的函数都必须在(契约测试的前提被改掉了)'
    . ([scriptblock]::Create($planFunction.Extent.Text))
    . ([scriptblock]::Create($namesFunction.Extent.Text))
    $legacyPlan = Resolve-SceneDeploymentPlan -LegacySceneReplicas 1 -WorldReplicas -1 -InstanceReplicas -1
    $splitPlan = Resolve-SceneDeploymentPlan -LegacySceneReplicas 1 -WorldReplicas 1 -InstanceReplicas 1
    # 计划函数整体返回数组(return ,@(...)),先展开一层再取名字。
    $deployNames = @(@($legacyPlan) + @($splitPlan) | ForEach-Object { $_ } | ForEach-Object { [string]$_.Name } | Sort-Object -Unique)
    $rollbackNames = @(Get-ZoneSceneWorkloadNames | Sort-Object -Unique)
    Assert-Equal -Expected ($deployNames -join ',') -Actual ($rollbackNames -join ',') -Because 'k8s_deploy.ps1 新增或改名 scene 池而回滚脚本不跟,新池上的 warn 读不到,第 0 步会错放行'
}

# B0-4 dry-run 与入口校验(子进程,不带 -Apply,不读集群)。
Test-Case 'B0-4 dry-run:留空时预告 nofile 核对(同一次 kubectl 带 scene Deployment,另预告 Agones 探测与 Fleet 读取),摘要写明 -Apply 会核对;显式给值原样透传到 Step 6、不再预告' {
    $run = Invoke-RollbackDryRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "留空的 dry-run 不该失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern ('kubectl ' + [regex]::Escape($PreflightKubectlPrefix) + '[^\r\n]*NODE_OPEN_FILE_LIMIT_MODE 须与 k8s_deploy\.ps1 -NodeOpenFileLimitMode 默认值一致') -Because 'dry-run 要预告 -Apply 时同一次 kubectl 核对 gate / scene 的 nofile 模式'
    Assert-Match -Text $run.Output -Pattern ('kubectl get --raw /apis/agones\.dev/v1; only when Agones is installed: kubectl get fleets\.agones\.dev scene scene-world scene-instance -n ' + [regex]::Escape($SandboxNamespace)) -Because 'dry-run 要预告 Agones 探测与 scene Fleet 读取'
    Assert-Match -Text $run.Output -Pattern 'nofile mode\s*:[^\r\n]*-Apply checks gate / scene before shutdown' -Because '摘要区要写明 -Apply 会在停服前核对,不再写「preflight does not check this」'
    Assert-NotMatch -Text (Get-ZoneUpArgsLine -Output $run.Output) -Pattern 'NodeOpenFileLimitMode' -Because '留空不透传(默认值只在 k8s_deploy.ps1 一处)'

    foreach ($mode in @('warn', 'enforce')) {
        $explicit = Invoke-RollbackDryRun -ExtraArguments @('-NodeOpenFileLimitMode', $mode)
        Assert-Equal -Expected 0 -Actual $explicit.ExitCode -Because "-NodeOpenFileLimitMode $mode 的 dry-run 不该失败。输出: $($explicit.Output)"
        $line = Get-ZoneUpArgsLine -Output $explicit.Output
        Assert-Match -Text $line -Pattern "(^|\s)-NodeOpenFileLimitMode $mode(\s|$)" -Because "Step 6 必须原样透传 $mode"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($line, 'NodeOpenFileLimitMode').Count) -Because '只透传一次,重复的具名参数会让 dev_tools.ps1 直接报错'
        Assert-Match -Text $explicit.Output -Pattern "nofile mode\s*:\s*-NodeOpenFileLimitMode $mode" -Because '摘要区写明 Step 6 以哪种模式拉起 gate / scene'
        Assert-Match -Text $explicit.Output -Pattern "Explicit -NodeOpenFileLimitMode ${mode}:" -Because '要写明这一项不核对'
        Assert-NotMatch -Text $explicit.Output -Pattern 'only when Agones is installed' -Because '显式给值不预告 Agones 探测与 Fleet 读取'
    }
}

Test-Case 'B0-4 负向:-NodeOpenFileLimitMode 只收小写 enforce / warn,大写与拼错在回滚脚本入口被拒,早于 Step 1' {
    foreach ($bad in @('WARN', 'Enforce', 'off')) {
        $run = Invoke-RollbackDryRun -ExtraArguments @('-NodeOpenFileLimitMode', $bad)
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "C++ 只认小写 warn;'$bad' 必须在回滚脚本入口被拒,拖到 Step 6 才由下游拒时 zone 已停服、数据已回档。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern 'NodeOpenFileLimitMode' -Because "'$bad':错误文本必须点名 NodeOpenFileLimitMode,不能把其他执行错误误判为参数校验"
        Assert-NotMatch -Text $run.Output -Pattern 'Step 1:' -Because "'$bad':参数校验必须先于任何步骤"
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
    Assert-Match -Text $run.Output -Pattern 'k8s-zone-up -DryRun passed' -Because '放行时写明静态预检的结论'
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
    Assert-Match -Text $run.Output -Pattern 'Static preflight: run dev_tools.ps1 k8s-zone-up with the same arguments as Step 6[^\r\n]*-DryRun' -Because 'dry-run 要预告 -Apply 时会先 -DryRun 一遍 Step 6'
}

# ─────────────────────────────────────────────────────────────────
# go/db 存盘消费组按 zone 拆分(2026-10-15,docs/design/db-consumer-group-per-zone.md §6):
# 回档一律落到 zone 组;Step 2 排空等待删除;第 0 步 c 只读快照、d 预演 Step 5;Step 5 重置 zone 组并退役 legacy 组位点
# ─────────────────────────────────────────────────────────────────

# 组名基名与分区数取仓库 go/db/etc/db.yaml(被测脚本同样现读,测试不抄常量)。
$RepoDbYaml = Join-Path (Get-RepoRoot) 'go' 'db' 'etc' 'db.yaml'
$DbGroupBase = (Get-YamlScalar -Path $RepoDbYaml -KeyPath 'ServerConfig.Kafka.GroupID').Value
$DbPartitionCnt = (Get-YamlScalar -Path $RepoDbYaml -KeyPath 'ServerConfig.Kafka.PartitionCnt').Value
$ZoneGroup = "${DbGroupBase}_z101"
$SandboxTopic = 'db_task_zone_101'
# 三项入口参数都显式、不读集群,只看 Kafka 相关的步骤。
$KafkaCaseArgs = @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }

# 断言一行 "参数=值;…" 里有某个键值对(值按字面匹配)。
function Assert-BoundPair {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Line,
        [Parameter(Mandatory = $true)][string]$Pair,
        [Parameter(Mandatory = $true)][string]$Because
    )
    Assert-Match -Text $Line -Pattern "(^| |;)$([regex]::Escape($Pair))(;|$)" -Because $Because
}

Test-Case '前提:仓库 go/db/etc/db.yaml 的 GroupID 基名与 PartitionCnt 读得到' {
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($DbGroupBase)) -Because '回滚脚本按它推导 zone 组 / legacy 组,读不到就拒绝执行'
    Assert-Match -Text $DbPartitionCnt -Pattern '^[1-9]\d*$' -Because 'Step 5 按它逐分区复核位点,必须是正整数'
}

Test-Case '-Apply 全程通过:Step 5 把 zone 组拨到日志末端(-ToLatest)并退役 legacy 组位点,Step 6 显式 -DbConsumerGroup zone,不再有 Step 2 排空' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs
    Assert-True -Condition ($null -eq $run.Error) -Because "全流程不该失败。错误: $($run.Error)"
    foreach ($command in @('k8s-zone-up/dry-run', 'k8s-zone-up')) {
        Assert-BoundPair -Line (Get-BoundLine -Run $run -Command $command) -Pair 'DbConsumerGroup=zone' -Because "$command 必须显式带 -DbConsumerGroup zone:回档后永远落到 zone 组,静态预检与 Step 6 用同一份参数"
    }
    Assert-NotMatch -Text (Get-BoundLine -Run $run -Command 'k8s-zone-down') -Pattern 'DbConsumerGroup' -Because 'zone-down 与消费组无关,不传'
    Assert-True -Condition ($run.KafkaResetBound.Count -ge 1) -Because "Step 5 必须真正调 kafka_offset_reset.ps1。实际: $($run.KafkaResetBound -join ' | ')"
    $step5 = $run.KafkaResetBound[-1]
    foreach ($pair in @("Group=$ZoneGroup", "RetireGroup=$DbGroupBase", "Partitions=$DbPartitionCnt", 'ToLatest=True', 'Apply=True', "Topic=$SandboxTopic")) {
        Assert-BoundPair -Line $step5 -Pair $pair -Because "Step 5 必须带 ${pair}:重置 zone 组(Empty,重置能生效),逐分区复核后再删 legacy 组在本 topic 上的位点"
    }
    # 负责人 2026-10-17 裁决「丢弃 TargetTime 之后的写」:拨回 T 会让 [T, 停服] 之间从未落库的积压照常写进 PITR 之后的 MySQL。
    Assert-NotMatch -Text $step5 -Pattern '(^|;)ToDatetime=' -Because 'Step 5 不再按 TargetTime 拨位点,只拨到日志末端'
    Assert-NotMatch -Text $run.Output -Pattern 'Step 2: drain|waiting for LAG|proceeding anyway' -Because 'zone-down 之后等 LAG 排空必然超时,Step 2 已删除'
}

Test-Case 'zone 组不存在:第 0 步 d 先以 dry-run 预演 Step 5(不带 -Apply / -RetireGroup),早于 Step 1' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs
    Assert-True -Condition ($null -eq $run.Error) -Because "预演成功时全流程不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 2 -Actual $run.KafkaResetBound.Count -Because "第 0 步 d 预演一次、Step 5 执行一次。实际: $($run.KafkaResetBound -join ' | ')"
    $rehearsal = $run.KafkaResetBound[0]
    foreach ($pair in @("Group=$ZoneGroup", "Partitions=$DbPartitionCnt", 'ToLatest=True')) {
        Assert-BoundPair -Line $rehearsal -Pair $pair -Because "预演必须用 Step 5 的同一组重置参数($pair)"
    }
    Assert-NotMatch -Text $rehearsal -Pattern '(^|;)ToDatetime=' -Because '预演与 Step 5 一样拨到日志末端,不再带 TargetTime'
    Assert-NotMatch -Text $rehearsal -Pattern '(^|;)(Apply|RetireGroup)=' -Because '预演只能是 dry-run,且不能删任何位点'
    Assert-True -Condition (@($run.KafkaCliCalls | Where-Object { $_ -match "--describe --group $([regex]::Escape($ZoneGroup))(\s|$)" }).Count -ge 1) -Because '预演前要先 describe zone 组,判断它在本 topic 上有没有位点'
}

Test-Case '第 0 步 d 预演失败(Kafka 生成不出完整计划):停服之前拒绝,Step 5 不执行' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaResetDryRunExit 3
    Assert-RefusedBeforeStop -Run $run -Label '预演失败' -HintPattern 'Step 5 预演失败'
    Assert-Match -Text $run.Error -Pattern '退出码 3' -Because '要带上预演失败的原因'
    Assert-Equal -Expected 1 -Actual $run.KafkaResetBound.Count -Because "只执行过预演,Step 5 一次都不该被调。实际: $($run.KafkaResetBound -join ' | ')"
}

Test-Case 'zone 组已有位点:跳过预演;LAG 快照只告警、合计只含本 topic,照常走到 Step 6' {
    $header = 'GROUP  TOPIC  PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID'
    $lines = @(
        "Consumer group '$ZoneGroup' has no active members.",
        $header,
        "$ZoneGroup  $SandboxTopic  0  10  15  5  -  -  -",
        "$ZoneGroup  $SandboxTopic  1  20  27  7  -  -  -",
        "$ZoneGroup  db_task_zone_102  0  0  100  100  -  -  -"
    )
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaCliOutput @{ Lines = $lines; ExitCode = 0 }
    Assert-True -Condition ($null -eq $run.Error) -Because "LAG > 0 只是快照,不该拦住回档。错误: $($run.Error)"
    Assert-Match -Text $run.Output -Pattern 'Step 5 rehearsal skipped' -Because 'zone 组已有位点 = 组已存在,不必预演'
    Assert-Equal -Expected 1 -Actual $run.KafkaResetBound.Count -Because "只有 Step 5 调了 kafka_offset_reset.ps1。实际: $($run.KafkaResetBound -join ' | ')"
    Assert-Match -Text $run.Output -Pattern "total lag=12(\s|$)" -Because '合计只含本 topic 的 5 + 7'
    Assert-NotMatch -Text $run.Output -Pattern 'total lag=112' -Because '别的 topic(db_task_zone_102)的 LAG 不得计入'
    Assert-True -Condition (@($run.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -eq 1) -Because 'Step 6 照常执行'
}

Test-Case 'Kafka CLI 不可用:第 0 步 c 只告警,第 0 步 d 判不出即停服之前拒绝' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs `
        -KafkaCliOutput @{ Throw = "The term 'kafka-consumer-groups.sh' is not recognized" }
    Assert-Match -Text $run.Output -Pattern 'Kafka LAG snapshot unavailable' -Because '快照失败只告警,不能自己拦住回档'
    Assert-RefusedBeforeStop -Run $run -Label 'CLI 不可用' -HintPattern 'Step 5 预演读不到'
    Assert-Equal -Expected 0 -Actual $run.KafkaResetBound.Count -Because 'describe 都读不到,预演与 Step 5 都不该执行'
}

Test-Case '-KafkaGroup 显式等于 legacy 组:入口即拒,不读集群、不调任何下游' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -ExtraArgs @{ KafkaGroup = $DbGroupBase }
    Assert-True -Condition ($null -ne $run.Error) -Because '重置并退役同一个组会让它在本 topic 上一个位点都不剩,必须拒绝'
    Assert-Match -Text $run.Error -Pattern '-KafkaGroup 与 -KafkaLegacyGroup 都是' -Because '错误文本要点明两组相同'
    Assert-Equal -Expected 0 -Actual $run.DevToolsCalls.Count -Because '入口校验早于任何步骤'
    Assert-Equal -Expected 0 -Actual $run.KubectlCalls.Count -Because '入口校验早于预检读集群'
    Assert-Equal -Expected 0 -Actual $run.KafkaCliCalls.Count -Because '入口校验早于读 Kafka'
}

Test-Case '组名与分区数取自 go/db/etc/db.yaml,不在回滚脚本里写死;读不到即拒绝' {
    $custom = @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group" # 测试用基名', '    TopicGeneration: 1', '    PartitionCnt: 4')
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DbYamlContent $custom
    Assert-True -Condition ($null -eq $run.Error) -Because "自定义 db.yaml 下全流程不该失败。错误: $($run.Error)"
    $step5 = $run.KafkaResetBound[-1]
    foreach ($pair in @('Group=custom_db_group_z101', 'RetireGroup=custom_db_group', 'Partitions=4')) {
        Assert-BoundPair -Line $step5 -Pair $pair -Because "组名基名与分区数必须现读 db.yaml($pair)"
    }

    $missing = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -OmitDbYaml
    Assert-True -Condition ($null -ne $missing.Error) -Because '读不到 db.yaml 就无法推导组名,必须拒绝(fail-closed)'
    Assert-Match -Text $missing.Error -Pattern 'ServerConfig\.Kafka\.GroupID' -Because '错误文本要点明读不到哪个键'
    Assert-Equal -Expected 0 -Actual $missing.DevToolsCalls.Count -Because '入口校验早于任何步骤'

    $badCount = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs `
        -DbYamlContent @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group"', '    PartitionCnt: 0')
    Assert-Match -Text $badCount.Error -Pattern 'PartitionCnt' -Because '分区数不是正整数时无法逐分区复核,必须拒绝'
}

Test-Case 'dry-run:不调 Kafka CLI 与 kafka_offset_reset,只预告快照与 Step 5 预演' {
    $run = Invoke-RollbackInSandbox -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs
    Assert-True -Condition ($null -eq $run.Error) -Because "dry-run 不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.KafkaCliCalls.Count -Because 'dry-run 不读 Kafka'
    Assert-Equal -Expected 0 -Actual $run.KafkaResetBound.Count -Because 'dry-run 连预演也不执行'
    foreach ($group in @($ZoneGroup, $DbGroupBase)) {
        Assert-Match -Text $run.Output -Pattern ("kafka-consumer-groups\.sh --bootstrap-server kafka:9092 --describe --group " + [regex]::Escape($group) + '(\r?\n|$)') -Because "dry-run 要预告快照会 describe $group"
    }
    Assert-Match -Text $run.Output -Pattern "Step 5 rehearsal: describe $([regex]::Escape($ZoneGroup))" -Because 'dry-run 要预告 -Apply 时的 Step 5 预演'
}

Test-Case '子进程 dry-run:Step 5 打印 zone 组与 RetireGroup,Step 6 带 -DbConsumerGroup zone,摘要列出两组' {
    $run = Invoke-RollbackDryRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dry-run 不该失败。输出: $($run.Output)"
    $step5 = @([regex]::Matches($run.Output, '(?m)^[ \t]*kafka_offset_reset\.ps1 [^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $step5.Count -Because "应当恰有一行 Step 5 参数。输出: $($run.Output)"
    Assert-Match -Text $step5[0] -Pattern ('(^|\s)-Group ' + [regex]::Escape($ZoneGroup) + '(\s|$)') -Because 'Step 5 重置的是 zone 组'
    Assert-Match -Text $step5[0] -Pattern ('(^|\s)-RetireGroup ' + [regex]::Escape($DbGroupBase) + '(\s|$)') -Because 'Step 5 退役 legacy 组在本 topic 上的位点'
    Assert-Match -Text $step5[0] -Pattern "(^|\s)-Partitions $DbPartitionCnt(\s|$)" -Because 'Step 5 按 db.yaml 的分区数逐分区复核'
    Assert-Match -Text $step5[0] -Pattern '(^|\s)-ToLatest(\s|$)' -Because 'Step 5 把 zone 组拨到日志末端(负责人 2026-10-17 裁决:丢弃 TargetTime 之后的写)'
    Assert-NotMatch -Text $step5[0] -Pattern '-ToDatetime' -Because 'Step 5 不再拨回 TargetTime'
    Assert-Match -Text (Get-ZoneUpArgsLine -Output $run.Output) -Pattern '(^|\s)-DbConsumerGroup zone(\s|$)' -Because 'Step 6 显式落到 zone 组'
    Assert-Match -Text $run.Output -Pattern ("group=" + [regex]::Escape($ZoneGroup) + " retire=" + [regex]::Escape($DbGroupBase) + '(\s|$)') -Because '摘要要写明重置哪个组、退役哪个组'
    Assert-NotMatch -Text $run.Output -Pattern 'Step 2: drain' -Because 'Step 2 已删除'
}

# ─────────────────────────────────────────────────────────────────
# R7 复核(2026-10-17):[6] topic 世代号从 db.yaml 推导;[2] 第 0 步 e 按 Step 6 的访问路径预读 Kafka 位点
# ─────────────────────────────────────────────────────────────────

Test-Case '[6] TopicGeneration 2:第 0 步 d 与 Step 5 都作用在 <topic>_g2;显式 -KafkaTopic 与推导值不同即入口拒绝;世代号缺失 / 非法同样拒绝' {
    $gen2 = @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group"', '    TopicGeneration: 2', '    PartitionCnt: 4')
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DbYamlContent $gen2
    Assert-True -Condition ($null -eq $run.Error) -Because "TopicGeneration 2 下全流程不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 2 -Actual $run.KafkaResetBound.Count -Because "第 0 步 d 预演一次、Step 5 执行一次。实际: $($run.KafkaResetBound -join ' | ')"
    foreach ($line in $run.KafkaResetBound) {
        Assert-BoundPair -Line $line -Pair 'Topic=db_task_zone_101_g2' -Because '世代号 > 1 时真实 topic 带 _g<gen>,与 Step 6 的 k8s_deploy.ps1 读的是同一个'
    }
    Assert-Match -Text $run.Output -Pattern 'topic=db_task_zone_101_g2(\s|$)' -Because '摘要要写明推导出的 topic'

    $same = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ KafkaTopic = 'db_task_zone_101_g2' } + $KafkaCaseArgs) -DbYamlContent $gen2
    Assert-True -Condition ($null -eq $same.Error) -Because "显式 -KafkaTopic 等于推导值时放行。错误: $($same.Error)"

    $stale = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ KafkaTopic = 'db_task_zone_101' } + $KafkaCaseArgs) -DbYamlContent $gen2
    Assert-True -Condition ($null -ne $stale.Error) -Because '沿用旧命令传了上一代 topic,必须拒绝:Step 5 会重置旧 topic,真实 topic 上的位点不动'
    Assert-Match -Text $stale.Error -Pattern '-KafkaTopic db_task_zone_101 与按 go/db/etc/db\.yaml\(TopicGeneration=2\)推导的 db_task_zone_101_g2 不一致' -Because '错误文本要点明两个 topic'
    Assert-Equal -Expected 0 -Actual ($stale.DevToolsCalls.Count + $stale.KafkaCliCalls.Count + $stale.KafkaExecCalls.Count + $stale.KafkaResetBound.Count) -Because '入口校验早于任何步骤与读 Kafka'

    foreach ($case in @(
            @{ Label = '缺 TopicGeneration'; Lines = @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group"', '    PartitionCnt: 4') }
            @{ Label = 'TopicGeneration 为负'; Lines = @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group"', '    TopicGeneration: -1', '    PartitionCnt: 4') }
            @{ Label = 'TopicGeneration 不是整数'; Lines = @('ServerConfig:', '  Kafka:', '    GroupID: "custom_db_group"', '    TopicGeneration: abc', '    PartitionCnt: 4') }
        )) {
        $bad = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DbYamlContent $case.Lines
        Assert-True -Condition ($null -ne $bad.Error) -Because "$($case.Label):拼不出 topic 名必须拒绝(fail-closed)"
        Assert-Match -Text $bad.Error -Pattern 'ServerConfig\.Kafka\.TopicGeneration' -Because "$($case.Label):错误文本要点明读不到哪个键"
        Assert-Equal -Expected 0 -Actual $bad.DevToolsCalls.Count -Because "$($case.Label):入口校验早于任何步骤"
    }
}

Test-Case '[2] 第 0 步 e:停服之前按 Step 6 的路径(kubectl exec 进 kafka-0,同一 context、dev_tools 默认 infra namespace)读两组位点' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ KubeContext = 'kind-mmorpg' } + $KafkaCaseArgs)
    Assert-True -Condition ($null -eq $run.Error) -Because "两条路径一致时不该被拦。错误: $($run.Error)"
    Assert-Equal -Expected 2 -Actual $run.KafkaExecCalls.Count -Because "zone 组与 legacy 组各 exec 一次。实际: $($run.KafkaExecCalls -join ' | ')"
    foreach ($pair in @(@{ Index = 0; Group = $ZoneGroup }, @{ Index = 1; Group = $DbGroupBase })) {
        $want = "--context kind-mmorpg exec -n contract-infra kafka-0 -c kafka -- /bin/sh -c KAFKA_HEAP_OPTS='-Xms16m -Xmx64m' /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group $($pair.Group)"
        Assert-Equal -Expected $want -Actual $run.KafkaExecCalls[$pair.Index] -Because '与 k8s_deploy.ps1 Read-DbGroupTopicFacts 同一条命令(lib Get-DbGroupDescribeExecArgs)、同一 -KubeContext、dev_tools.ps1 默认的 infra namespace'
    }
    $factsAt = $run.Output.IndexOf('Step 6 Kafka facts readable')
    $stopAt = $run.Output.IndexOf('Step 1: k8s-zone-down')
    Assert-True -Condition ($factsAt -ge 0 -and $stopAt -gt $factsAt) -Because '第 0 步 e 必须在 Step 1 停服之前'
}

Test-Case '[2] 第 0 步 e:exec 读不到(缺 pods/exec、kafka-0 未就绪)或与本机 CLI 不一致,都在停服之前拒绝,Step 5 不执行' {
    $forbidden = @{ Lines = @('Error from server (Forbidden): pods "kafka-0" is forbidden: User "deployer" cannot create resource "pods/exec" in API group ""'); ExitCode = 1 }
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaExecOutput $forbidden
    Assert-RefusedBeforeStop -Run $run -Label 'exec Forbidden' -HintPattern 'pods/exec'
    Assert-Match -Text $run.Error -Pattern 'Step 6' -Because '错误文本要说明这是 Step 6 将走的路径'
    Assert-Equal -Expected 0 -Actual @($run.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because 'Step 5 不能执行'

    # exec 里 zone 组在本 topic 上有位点,本机 CLI(缺省)说组不存在:两条路径指向的不是同一个 Kafka。
    $header = 'GROUP  TOPIC  PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID'
    $other = @{ Lines = @($header, "$ZoneGroup  $SandboxTopic  0  10  15  5  -  -  -"); ExitCode = 0 }
    $mismatch = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaExecOutput $other
    Assert-RefusedBeforeStop -Run $mismatch -Label '两条路径不一致' -HintPattern '两条访问路径看到的 Kafka 不一致'
    Assert-Match -Text $mismatch.Error -Pattern ([regex]::Escape("消费组 $ZoneGroup 在 $SandboxTopic 上:kafka-0 里读到 offsets,本机 CLI 读到 missing")) -Because '要逐组列出两边的结论'
    Assert-Equal -Expected 0 -Actual @($mismatch.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because 'Step 5 不能执行'

    $unreadable = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DevToolsInfraNamespaceDefault ''
    Assert-RefusedBeforeStop -Run $unreadable -Label 'dev_tools 默认 infra namespace 读不出' -HintPattern '-InfraNamespace 默认值'
    Assert-Equal -Expected 0 -Actual $unreadable.KafkaExecCalls.Count -Because '不知道去哪个 namespace 就不 exec'
}

Test-Case '[2] 第 0 步 e:dry-run 只预告 exec 命令,不执行;子进程 dry-run 读得出真 dev_tools.ps1 的默认 infra namespace' {
    $run = Invoke-RollbackInSandbox -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs
    Assert-True -Condition ($null -eq $run.Error) -Because "dry-run 不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 0 -Actual $run.KafkaExecCalls.Count -Because 'dry-run 不读集群'
    foreach ($group in @($ZoneGroup, $DbGroupBase)) {
        Assert-Match -Text $run.Output -Pattern ("kubectl exec -n contract-infra kafka-0 -c kafka -- /bin/sh -c '[^\r\n]*--describe --group " + [regex]::Escape($group) + "'(\r?\n|$)") -Because "dry-run 要预告 -Apply 时对 $group 的 exec，完整 shell 参数必须带引号且组名精确到行尾"
    }
    $sub = Invoke-RollbackDryRun
    Assert-Equal -Expected 0 -Actual $sub.ExitCode -Because "子进程 dry-run 不该失败。输出: $($sub.Output)"
    Assert-Match -Text $sub.Output -Pattern 'kubectl exec -n [A-Za-z0-9.-]+ kafka-0 -c kafka -- ' -Because '真 dev_tools.ps1 的 -InfraNamespace 默认值必须能按语法树读成字面常量,否则 -Apply 永远在第 0 步 e 被拒'
    Assert-NotMatch -Text $sub.Output -Pattern '<unreadable>' -Because '同上'
}

# ─────────────────────────────────────────────────────────────────
# 回档丢弃语义(负责人 2026-10-17 裁决:丢弃 TargetTime 之后的写;2026-10-18 落码,回档丢弃语义对抗复核修正版):
# 第 0 步 f 回档工具预检 → Step 1 → 1b 停服后核对 → Step 3 → 4a 盘点 → Step 5 -ToLatest(记切点)→ 4b 丢弃 → 5b 复核 → Step 6
# ─────────────────────────────────────────────────────────────────

# 回档工具各次调用的模式序列(DiscardCalls 每行 "<模式> <argv>")。
function Get-DiscardModes {
    param([Parameter(Mandatory = $true)]$Run)
    return @($Run.DiscardCalls | ForEach-Object { ($_ -split ' ', 2)[0] })
}

# 某个模式那一次调用的 argv(恰好一次)。
function Get-DiscardArgv {
    param([Parameter(Mandatory = $true)]$Run, [Parameter(Mandatory = $true)][string]$Mode)
    $lines = @($Run.DiscardCalls | Where-Object { $_ -like "$Mode *" })
    Assert-Equal -Expected 1 -Actual $lines.Count -Because "回档工具 $Mode 应当恰被调一次(实际: $($Run.DiscardCalls -join ' | '))"
    return $lines[0]
}

<#
.SYNOPSIS
    断言这次运行在 Step 1b 被拒:zone 已停服(Step 1 执行过),PITR 提示没出现,4a / 4b / 5b、Step 5 -Apply、Step 6 一个都没执行。
#>
function Assert-RefusedBeforePitr {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)][string]$HintPattern
    )
    Assert-True -Condition ($null -ne $Run.Error) -Because "${Label}:必须拒绝。输出: $($Run.Output)"
    Assert-Match -Text $Run.Error -Pattern 'zone 已停服,尚未 PITR,尚未改动 Redis / Kafka' -Because "${Label}:错误文本要写明此刻的状态"
    Assert-Match -Text $Run.Error -Pattern $HintPattern -Because "${Label}:错误文本要带上拒绝的原因"
    Assert-Match -Text $Run.Error -Pattern '-RollbackId z101-' -Because "${Label}:要给出用同一 rid 整脚本重跑的做法"
    Assert-Match -Text $Run.Error -Pattern '放弃回档' -Because "${Label}:要给出放弃回档(直接 zone-up)的做法"
    Assert-NotMatch -Text $Run.Output -Pattern 'Step 3: MySQL Point-in-Time Recovery' -Because "${Label}:拒绝必须在 PITR 提示之前"
    Assert-Equal -Expected 'k8s-zone-up/dry-run GateRouterMode=1|k8s-zone-down GateRouterMode=<unset>' -Actual ($Run.DevToolsCalls -join '|') -Because "${Label}:停服已发生,Step 6 不该执行"
    $late = @(Get-DiscardModes -Run $Run | Where-Object { $_ -in @('inventory', 'purge', 'verify') })
    Assert-Equal -Expected 0 -Actual $late.Count -Because "${Label}:盘点、丢弃、复核都不该执行(实际: $($Run.DiscardCalls -join ' | '))"
    Assert-Equal -Expected 0 -Actual @($Run.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because "${Label}:Step 5 不能执行"
}

$CutHeader = 'GROUP  TOPIC  PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID'

Test-Case '(f) -Apply 全程:调用顺序固定为 预检 → 停服 → 1b → PITR → 盘点 → 停服复核 → Step 5 → 丢弃 → 复核 → Step 6,不碰 redis-cli、不 FLUSHDB' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs
    Assert-True -Condition ($null -eq $run.Error) -Because "全流程不该失败。错误: $($run.Error)"
    Assert-Equal -Expected 'preflight|stopped|inventory|purge|verify' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because '回档工具的五个模式各一次,顺序固定'
    # 回档复核 [1]:Step 1b 先证明 Kafka 上没有消费者、日志末端不动(sleep 在两次读之间),工具 stopped 才写停服基线;
    # 回档复核 [4][5]:第 0 步 f 先读 go/db Redis 的 run_id、核对工具构建提交,再跑 preflight。
    $order = @('kubectl:snapshot', 'kubectl:redis', 'tool:version', 'tool:preflight', 'dev_tools:k8s-zone-down', 'kubectl:namespace', ('kafka-cg:' + $ZoneGroup + ':state'),
        'sleep', 'tool:stopped', 'tool:inventory', 'reset:apply', 'tool:purge', 'tool:verify', 'dev_tools:k8s-zone-up')
    Assert-True -Condition ($order -ccontains ('kafka-cg:' + $ZoneGroup + ':state') -and -not ($order -ccontains 'kafka-cg:')) -Because '时间线判据必须包含完整消费组状态事件，不能被数组逗号拆成三个元素'
    $positions = @($order | ForEach-Object { Get-TimelineIndex -Timeline $run.Timeline -Event $_ })
    for ($i = 0; $i -lt $order.Count; $i++) {
        Assert-True -Condition ($positions[$i] -ge 0) -Because "时间线里必须有 $($order[$i])(实际: $($run.Timeline -join ' > '))"
        if ($i -gt 0) {
            Assert-True -Condition ($positions[$i - 1] -lt $positions[$i]) -Because "$($order[$i - 1]) 必须早于 $($order[$i]):Step 5 记下切点之后才丢弃 Redis(对抗复核 [4]),停服之前完成全部只读预检,停服基线在确认没有写者之后才取(实际: $($run.Timeline -join ' > '))"
        }
    }
    # 回档复核 [8]:盘点之后、Step 5 之前再读一次 namespace 与 zone 组 --state。
    $inventoryAt = Get-TimelineIndex -Timeline $run.Timeline -Event 'tool:inventory'
    $resetAt = Get-TimelineIndex -Timeline $run.Timeline -Event 'reset:apply'
    foreach ($event in @('kubectl:namespace', ('kafka-cg:' + $ZoneGroup + ':state'))) {
        $last = [array]::LastIndexOf([string[]]$run.Timeline, $event)
        Assert-True -Condition ($last -gt $inventoryAt -and $last -lt $resetAt) -Because "$event 必须在盘点之后、Step 5 之前再复核一次(实际: $($run.Timeline -join ' > '))"
    }
    Assert-Equal -Expected 2 -Actual $run.NamespaceCalls.Count -Because 'Step 1 之后一次、Step 5 之前一次'
    $pitrAt = $run.Output.IndexOf('Step 3: MySQL Point-in-Time Recovery')
    $inventoryAt = $run.Output.IndexOf('fake zone_rollback_discard: mode=inventory')
    Assert-True -Condition ($pitrAt -ge 0 -and $inventoryAt -gt $pitrAt) -Because '盘点在 PITR 之后(按 PITR 之后的库归类)'
    Assert-Equal -Expected 2 -Actual $run.LeoCalls.Count -Because 'Step 1b 读两次日志末端'
    Assert-Equal -Expected 1 -Actual $run.SleepCalls.Count -Because '两次读之间只等一次'
    Assert-Match -Text $run.SleepCalls[0] -Pattern '(^|\s)300(\s|$)' -Because '静默窗口缺省 300s(不低于 librdkafka message.timeout.ms)'
    Assert-Equal -Expected 2 -Actual $run.PostResetDescribes -Because 'Step 5 之后读一次切点、5b 再读一次'
    $expectedCut = (@(0..([int]$DbPartitionCnt - 1)) | ForEach-Object { "$_=$(1000 + $_)" }) -join ','
    Assert-Match -Text (Get-DiscardArgv -Run $run -Mode 'verify') -Pattern ("-kafka-cut " + [regex]::Escape($expectedCut) + '(\s|$)') -Because '5b 把 Step 5 的切点交给工具 verify'
    Assert-Match -Text $run.ReportFiles['kafka-cut.json'] -Pattern ([regex]::Escape($expectedCut)) -Because '切点(丢弃水位)要落盘,以后的重置不得越过它'
    Assert-Equal -Expected 0 -Actual $run.RedisCliCalls.Count -Because '回档不再直接调 redis-cli'
    Assert-NotMatch -Text $run.Output -Pattern 'FLUSHDB' -Because '回档里不再有 FLUSHDB'
    Assert-Match -Text $run.Output -Pattern 'Rollback complete' -Because '全流程走完'
}

Test-Case '(f) 回档工具的公共参数:每次调用都带 zone / 世代 / rid / 屏障协议 / Redis / DSN 变量名,工具进程看得到 DSN 变量' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ RollbackId = 'z101-contract' } + $KafkaCaseArgs)
    Assert-True -Condition ($null -eq $run.Error) -Because "全流程不该失败。错误: $($run.Error)"
    foreach ($line in $run.DiscardCalls) {
        foreach ($pattern in @('-zone 101(\s|$)', '-topic-generation \d+(\s|$)', '-rollback-id z101-contract(\s|$)', '-barrier-protocol legacy(\s|$)',
                '-redis 127\.0\.0\.1:6379(\s|$)', '-redis-db 0(\s|$)', '-mysql-dsn-env MMORPG_CONTRACT_ROLLBACK_DSN(\s|$)', '-deploy-facts [^\r\n]*deploy-facts\.json(\s|$)',
                "-expect-run-id $([regex]::Escape($script:RollbackSandboxRedisRunId))(\s|$)")) {
            Assert-Match -Text $line -Pattern $pattern -Because "回档工具的每次调用都要带公共参数($pattern):$line"
        }
    }
    $judge = '-target-time 2026-05-15T14:23:00Z -fingerprint-baseline [^\r\n]*fingerprint-baseline\.json'
    Assert-Match -Text (Get-DiscardArgv -Run $run -Mode 'stopped') -Pattern '-lease-wait 90s -handoff-wait 300s -fingerprint-baseline [^\r\n]*fingerprint-baseline\.json(\s|$)' -Because '租约与 handoff 分别计时,handoff 不低于 HandoffTTL 300s;停服基线由 stopped 写(回档复核 [1])'
    Assert-NotMatch -Text (Get-DiscardArgv -Run $run -Mode 'preflight') -Pattern 'fingerprint-baseline' -Because 'preflight 在停服之前,go/db 仍在落库,那时的基线没有意义,不再写基线(回档复核 [1])'
    Assert-Match -Text (Get-DiscardArgv -Run $run -Mode 'inventory') -Pattern "-mode inventory $judge -report [^\r\n]*inventory\.json" -Because '盘点要知道 TargetTime、与停服基线比较并写报告'
    Assert-Match -Text (Get-DiscardArgv -Run $run -Mode 'purge') -Pattern "-mode purge $judge -confirm -report [^\r\n]*purge\.json" -Because '丢弃与盘点用同一组判定参数(回档复核 [2]),且必须显式 -confirm'
    Assert-Match -Text (Get-DiscardArgv -Run $run -Mode 'verify') -Pattern "-mode verify $judge -kafka-cut \S+ -purge-report [^\r\n]*purge\.json -report [^\r\n]*verify\.json" -Because 'verify 同一组判定参数,并逐个复核 purge.json 里处理过的玩家(回档复核 [6])'
    Assert-NotMatch -Text ($run.DiscardCalls -join "`n") -Pattern 'acknowledge-no-post-t-writes' -Because '没给 -AcknowledgeNoPostTWrites 就不带'
    Assert-Equal -Expected 1 -Actual $run.VersionCalls.Count -Because '第 0 步 f 核对一次工具的构建提交(回档复核 [5])'
    Assert-Equal -Expected 1 -Actual $run.RedisExecCalls.Count -Because '第 0 步 f 读一次 go/db Redis 的 run_id(回档复核 [4])'
    Assert-True -Condition (@($run.DiscardEnv | Where-Object { $_ -match 'MMORPG_CONTRACT_ROLLBACK_DSN=set$' }).Count -eq 5) -Because "DSN 只经环境变量交给工具,五次调用都看得到(实际: $($run.DiscardEnv -join ' | '))"
    Assert-NotMatch -Text ($run.DiscardCalls -join "`n") -Pattern 'contract:dsn@tcp' -Because 'DSN 本身不上命令行'
}

Test-Case '(a) 第 0 步 f:回档工具 preflight 返回 1 / 4 → 停服之前拒绝,dev_tools 只跑过静态预检' {
    foreach ($code in @(1, 4)) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DiscardExit @{ preflight = $code }
        Assert-RefusedBeforeStop -Run $run -Label "preflight 退出码 $code" -HintPattern "preflight 以退出码 $code"
        Assert-Equal -Expected 'preflight' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because "preflight 退出码 ${code}:回档工具只跑过 preflight"
        Assert-Equal -Expected 0 -Actual $run.NamespaceCalls.Count -Because "preflight 退出码 ${code}:没有停服,也就没有 namespace 检查"
        if ($code -eq 4) {
            Assert-Match -Text $run.Error -Pattern '-AllowEmptyTarget' -Because '目标看起来没在用时要说明怎么处理'
        }
    }
    $allowed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ AllowEmptyTarget = $true } + $KafkaCaseArgs)
    Assert-True -Condition ($null -eq $allowed.Error) -Because "带 -AllowEmptyTarget 不该失败。错误: $($allowed.Error)"
    Assert-Match -Text (Get-DiscardArgv -Run $allowed -Mode 'preflight') -Pattern '-mode preflight -allow-empty-target(\s|$)' -Because '-AllowEmptyTarget 只交给 preflight'
    Assert-NotMatch -Text (Get-DiscardArgv -Run $allowed -Mode 'stopped') -Pattern 'allow-empty-target' -Because '其它模式不带它'
}

Test-Case '(b) Step 1b:stopped 返回 3、zone 组 Stable、legacy 组本 topic 仍有成员、两次日志末端不同,都在 PITR 提示之前拒绝' {
    $stopped = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DiscardExit @{ stopped = 3 }
    Assert-RefusedBeforePitr -Run $stopped -Label 'stopped 退出码 3' -HintPattern 'stopped 以退出码 3'
    # 回档复核 [1]:stopped 最后一步写停服基线,必须在 Kafka 两项(消费者、两次日志末端)证明没有写者之后才调。
    Assert-Equal -Expected 2 -Actual $stopped.LeoCalls.Count -Because 'stopped 排在静默窗口之后:两次日志末端先读完'
    Assert-True -Condition ((Get-TimelineIndex -Timeline $stopped.Timeline -Event 'sleep') -lt (Get-TimelineIndex -Timeline $stopped.Timeline -Event 'tool:stopped')) -Because "静默窗口早于工具 stopped(实际: $($stopped.Timeline -join ' > '))"
    # 回档复核 [9]:放弃回档的 zone-up 不带 -DbConsumerGroup(keep),不用 Step 6 的显式 zone。
    Assert-Match -Text $stopped.Error -Pattern '放弃回档[^\r\n]*pwsh -File tools/scripts/dev_tools\.ps1 [^\r\n]*-Command k8s-zone-up' -Because '要给出放弃回档时的 zone-up 命令'
    Assert-NotMatch -Text $stopped.Error -Pattern '-DbConsumerGroup zone' -Because '放弃回档时显式 zone 会把仍在 legacy 组上的 zone 渲染成交接形状,没有交接闸的镜像起不来'

    $stableLines = @('GROUP  COORDINATOR (ID)  ASSIGNMENT-STRATEGY  STATE  #MEMBERS', "$ZoneGroup  kafka-0.kafka:9092 (0)  range  Stable  2")
    $stable = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaStateOutput @{ $ZoneGroup = @{ Lines = $stableLines; ExitCode = 0 } }
    Assert-RefusedBeforePitr -Run $stable -Label 'zone 组 Stable' -HintPattern '状态是 Stable'

    $badState = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaStateOutput @{ $ZoneGroup = @{ Lines = @('WARN something unexpected'); ExitCode = 0 } }
    Assert-RefusedBeforePitr -Run $badState -Label '--state 判不出' -HintPattern '判不出'

    # legacy 组在本 topic 上仍有成员(别的 zone 的旧组 db 实例订阅了本 topic)。同一份行也回给第 0 步,zone 组因此被判为已有位点。
    $assigned = @("Consumer group '$DbGroupBase' has members.", $CutHeader, "$DbGroupBase  $SandboxTopic  0  10  12  2  consumer-db-1-abc  /10.0.0.7  consumer-db-1")
    $legacy = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -KafkaCliOutput @{ Lines = $assigned; ExitCode = 0 }
    Assert-RefusedBeforePitr -Run $legacy -Label 'legacy 组仍有成员' -HintPattern 'consumer-db-1-abc'

    $moving = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -LeoChanges
    Assert-RefusedBeforePitr -Run $moving -Label '日志末端仍在前进' -HintPattern '分区 0:日志末端从 1000 变为 1001'
    Assert-Equal -Expected 2 -Actual $moving.LeoCalls.Count -Because '读两次才判得出'
}

Test-Case '(b) Step 1 之后 namespace 仍在(Remove-Zone 吞掉了删除失败):在 1b 之前拒绝' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs `
        -NamespaceReply @{ ExitCode = 0; Output = "namespace/$SandboxNamespace" }
    Assert-True -Condition ($null -ne $run.Error) -Because '写者可能还活着,必须拒绝'
    Assert-Match -Text $run.Error -Pattern 'namespace mmorpg-zone-contract-test 仍在' -Because '错误文本要点明 namespace 还在'
    Assert-Equal -Expected 'preflight' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because 'Step 1b 的 stopped 不该执行'
    Assert-Equal -Expected 0 -Actual $run.LeoCalls.Count -Because 'Step 1b 不该执行'
}

Test-Case '(c) 4a 盘点出 blocked 玩家(退出码 6):停在 Step 5 之前,不丢弃、不拨位点、不拉起;-AcceptBlockedPlayers 待裁决,入口即拒' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DiscardExit @{ inventory = 6 }
    Assert-True -Condition ($null -ne $run.Error) -Because 'blocked 玩家的处置待负责人裁决,必须拒绝'
    Assert-Match -Text $run.Error -Pattern '在 Step 4a 拒绝继续:zone 已停服、MySQL 已 PITR,Redis 与 Kafka 尚未改动' -Because '错误文本要写明此刻的状态'
    Assert-Match -Text $run.Error -Pattern '裁决\(#4\)' -Because '要点明这是待裁决事项'
    Assert-Match -Text $run.Error -Pattern '中止路径' -Because '要给出换回原库、直接 zone-up 的中止路径'
    # 回档复核 [9]:中止路径的 zone-up 与 Step 6 同一份参数,只去掉 -DbConsumerGroup(keep)。
    Assert-Match -Text $run.Error -Pattern '中止路径[^\r\n]*pwsh -File tools/scripts/dev_tools\.ps1 [^\r\n]*-Command k8s-zone-up' -Because '中止路径要给出可直接执行的 zone-up 命令'
    Assert-NotMatch -Text $run.Error -Pattern '-DbConsumerGroup zone' -Because '中止时显式 zone 会把回档前还在 legacy 组上的 zone 渲染成交接形状,不是安全退路'
    Assert-Match -Text $run.Output -Pattern 'run zone-up WITHOUT -DbConsumerGroup \(keep\)' -Because 'Step 3 的中止提示同一口径'
    Assert-Equal -Expected 'preflight|stopped|inventory' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because 'purge / verify 不该执行'
    Assert-Equal -Expected 0 -Actual @($run.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because 'Step 5 不能执行:Kafka 一动就没有中止路径了'
    Assert-Equal -Expected 0 -Actual @($run.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -Because 'Step 6 不该执行'

    $other = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DiscardExit @{ inventory = 3 }
    Assert-Match -Text $other.Error -Pattern 'inventory 以退出码 3' -Because '其它非 0 同样拒绝,并带上退出码'
    Assert-Equal -Expected 0 -Actual @($other.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because 'Step 5 不能执行'

    $accept = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ AcceptBlockedPlayers = 2 } + $KafkaCaseArgs)
    Assert-Match -Text $accept.Error -Pattern '-AcceptBlockedPlayers 待负责人裁决\(#4\)' -Because '带 blocked 玩家继续是待裁决事项,fail-closed'
    Assert-Equal -Expected 0 -Actual ($accept.DevToolsCalls.Count + $accept.DiscardCalls.Count + $accept.SnapshotCalls.Count + $accept.KafkaCliCalls.Count) -Because '入口即拒,没有任何下游调用'
}

Test-Case '(d) 4b purge 返回 1:Step 5 已执行、5b 与 Step 6 不执行,错误里给出同一 rid 的单步重跑命令' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ RollbackId = 'z101-contract' } + $KafkaCaseArgs) -DiscardExit @{ purge = 1 }
    Assert-True -Condition ($null -ne $run.Error) -Because 'purge 失败必须拒绝继续'
    Assert-Match -Text $run.Error -Pattern '在 Step 4b 失败' -Because '错误文本要点明失败的步骤'
    Assert-Match -Text $run.Error -Pattern '只能向前' -Because 'Step 5 已丢弃积压,不能再换回原库'
    Assert-Match -Text $run.Error -Pattern 'Step 4b:[^\r\n]*-rollback-id z101-contract[^\r\n]*-mode purge -target-time [^\r\n]*-confirm' -Because '要给出同一 rid 的 4b 重跑命令(与 4a 同一组判定参数)'
    Assert-Match -Text $run.Error -Pattern 'Step 6:pwsh -File tools/scripts/dev_tools\.ps1 [^\r\n]*-Command k8s-zone-up' -Because '要给出 Step 6 的单步命令'
    Assert-Equal -Expected 1 -Actual @($run.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because '新顺序下 Step 5 先于 4b(对抗复核 [4])'
    Assert-Equal -Expected 'preflight|stopped|inventory|purge' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because 'verify 不该执行'
    Assert-Equal -Expected 0 -Actual @($run.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -Because 'Step 6 不该执行'
}

Test-Case '(e) Step 5 之后又有消息写入:切点读到 LAG > 0 时不丢弃;5b 读到 LAG > 0 时不拉起' {
    $atCut = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -PostResetLag @(2)
    Assert-Match -Text $atCut.Error -Pattern 'Step 5 之后又有消息写入' -Because '刚拨完位点就有 LAG,说明还有生产者'
    Assert-Equal -Expected 'preflight|stopped|inventory' -Actual ((Get-DiscardModes -Run $atCut) -join '|') -Because '切点都记不下,purge 不该执行'

    $at5b = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -PostResetLag @(0, 3)
    Assert-Match -Text $at5b.Error -Pattern '在 Step 5b 拒绝继续\(Step 6 之前\)' -Because '切点之后有写入,Step 6 之前拒绝'
    Assert-Match -Text $at5b.Error -Pattern '分区 0:CURRENT-OFFSET 1000 不等于 LOG-END-OFFSET 1003' -Because '要列出越过切点的分区'
    Assert-Equal -Expected 'preflight|stopped|inventory|purge' -Actual ((Get-DiscardModes -Run $at5b) -join '|') -Because 'verify 不该执行'
    Assert-Equal -Expected 0 -Actual @($at5b.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -Because 'Step 6 不该执行'

    $verify = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $KafkaCaseArgs -DiscardExit @{ verify = 1 }
    Assert-Match -Text $verify.Error -Pattern 'verify 以退出码 1' -Because '工具 verify 失败同样在 Step 6 之前拒绝'
    Assert-Equal -Expected 0 -Actual @($verify.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -Because 'Step 6 不该执行'
}

# ─────────────────────────────────────────────────────────────────
# 回档复核修正(2026-10-18):[2] 判定参数共用、[8] Step 5 之前复核停服状态、[10] 入口决定存档
# ([0][4][5] 的部署事实、Redis 实例身份与工具版本见 k8s_zone_rollback_discard.tests.ps1)
# ─────────────────────────────────────────────────────────────────

Test-Case '[2] -AcknowledgeNoPostTWrites:inventory / purge / verify 与打印出来的 4b 单步命令都带 -acknowledge-no-post-t-writes,preflight / stopped 不带' {
    $failed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ AcknowledgeNoPostTWrites = $true; RollbackId = 'z101-ack' } + $KafkaCaseArgs) -DiscardExit @{ purge = 1 }
    Assert-Match -Text $failed.Error -Pattern 'Step 4b:[^\r\n]*-mode purge [^\r\n]*-acknowledge-no-post-t-writes[^\r\n]*-confirm' -Because '打印出来的 4b 单步命令必须与 4a 同一组判定参数,否则 Step 5 之后照它重跑必然退出 3'
    foreach ($mode in @('inventory', 'purge')) {
        Assert-Match -Text (Get-DiscardArgv -Run $failed -Mode $mode) -Pattern '-acknowledge-no-post-t-writes(\s|$)' -Because "$mode 必须带上开关:purge 原样重跑 inventory 的判定"
    }
    foreach ($mode in @('preflight', 'stopped')) {
        Assert-NotMatch -Text (Get-DiscardArgv -Run $failed -Mode $mode) -Pattern 'acknowledge-no-post-t-writes' -Because "$mode 不做行指纹判定,不带它"
    }
    $full = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs (@{ AcknowledgeNoPostTWrites = $true } + $KafkaCaseArgs)
    Assert-True -Condition ($null -eq $full.Error) -Because "带 -AcknowledgeNoPostTWrites 的全流程不该失败。错误: $($full.Error)"
    Assert-Match -Text (Get-DiscardArgv -Run $full -Mode 'verify') -Pattern '-acknowledge-no-post-t-writes(\s|$)' -Because 'verify 重跑盘点归类,同一组判定参数'
    Assert-Match -Text $full.ReportFiles['run-log.jsonl'] -Pattern '"acknowledgeNoPostTWrites":true' -Because '运行记录要写明本次放过了「没有任何玩家变化」'
}

Test-Case '[8] Step 5 之前复核停服状态:PITR 期间 namespace 被重建 / 读不到、zone 组又活跃、legacy 组又有成员,都在 reset:apply 之前拒绝' {
    $stableLines = @('GROUP  COORDINATOR (ID)  ASSIGNMENT-STRATEGY  STATE  #MEMBERS', "$ZoneGroup  kafka-0.kafka:9092 (0)  range  Stable  1")
    $cases = @(
        @{ Label = 'namespace 被重建'; Extra = @{ NamespaceReplyAfterInventory = @{ ExitCode = 0; Output = "namespace/$SandboxNamespace" } }; Pattern = 'namespace mmorpg-zone-contract-test 仍在' },
        @{ Label = 'namespace 读不到'; Extra = @{ NamespaceReplyAfterInventory = @{ ExitCode = 1; Output = 'Error from server (Forbidden): namespaces is forbidden' } }; Pattern = '读不到 namespace mmorpg-zone-contract-test 是否已删除' },
        @{ Label = 'zone 组又活跃'; Extra = @{ KafkaStateOutputAfterInventory = @{ $ZoneGroup = @{ Lines = $stableLines; ExitCode = 0 } } }; Pattern = '状态是 Stable' }
    )
    foreach ($c in $cases) {
        $sandboxArgs = @{ Apply = $true; DeployDefault = '1'; GateRouterMode = '1'; ExtraArgs = $KafkaCaseArgs } + $c.Extra
        $run = Invoke-RollbackInSandbox @sandboxArgs
        Assert-True -Condition ($null -ne $run.Error) -Because "$($c.Label):必须在 Step 5 之前拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Error -Pattern '在 Step 5 之前拒绝继续:zone 已停服、MySQL 已 PITR,Redis 与 Kafka 尚未改动' -Because "$($c.Label):错误文本要写明此刻的状态"
        Assert-Match -Text $run.Error -Pattern $c.Pattern -Because "$($c.Label):错误文本要带上原因"
        Assert-Match -Text $run.Error -Pattern '中止路径' -Because "$($c.Label):Kafka 与 Redis 都没动,给出中止路径"
        Assert-Equal -Expected 0 -Actual @($run.KafkaResetBound | Where-Object { $_ -match '(^|;)Apply=True(;|$)' }).Count -Because "$($c.Label):Step 5 不能执行"
        Assert-Equal -Expected 'preflight|stopped|inventory' -Actual ((Get-DiscardModes -Run $run) -join '|') -Because "$($c.Label):purge / verify 不该执行"
        Assert-Equal -Expected 0 -Actual @($run.DevToolsCalls | Where-Object { $_ -like 'k8s-zone-up GateRouterMode=*' }).Count -Because "$($c.Label):Step 6 不该执行"
    }
    # legacy 组在本 topic 上又有成员走的是同一个 Get-KafkaTopicConsumerProblems(CONSUMER-ID 列),判定已在 (b) 覆盖;这里只钉复核的时机。
}

Test-Case '[10] 入口决定存档:首次 -Apply 在 Step 1 之前写 entry-state.json;同一 rid 重跑、namespace 已删时沿用存档核对留空项,显式值与存档不同即停服之前拒绝' {
    $live = New-KubectlReply -GateEnv (New-RouterModeEnv -Value '1')
    $first = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $live -ExtraArgs @{ RollbackId = 'z101-entry' }
    Assert-True -Condition ($null -eq $first.Error) -Because "首次运行不该失败。错误: $($first.Error)"
    $saved = $first.ReportFiles['entry-state.json']
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($saved)) -Because '首次 -Apply 要把入口决定落盘'
    foreach ($pair in @('"GateRouterMode": "1"', '"ClientEntryMode": "podip"', '"GatewayIngress": "(none)"')) {
        Assert-Match -Text $saved -Pattern ([regex]::Escape($pair)) -Because "入口决定要记下 $pair(留空项取核对过的集群现状)"
    }
    $entryAt = $first.Output.IndexOf('entry decision saved to')
    Assert-True -Condition ($entryAt -ge 0 -and $entryAt -lt $first.Output.IndexOf('Step 1: k8s-zone-down')) -Because '存档必须在 Step 1 之前写(第 0 步全部通过之后)'

    $gone = New-KubectlReply -Items @()
    $seed = @{ 'entry-state.json' = $saved; 'deploy-facts.json' = $first.ReportFiles['deploy-facts.json'] }
    $reused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $gone -ExtraArgs @{ RollbackId = 'z101-entry' } -SeedReportFiles $seed -SnapshotReply @{ ExitCode = 0; Output = '' }
    Assert-True -Condition ($null -eq $reused.Error) -Because "namespace 已删时留空项按存档的集群现状核对,不再要操作者凭记忆重传。错误: $($reused.Error)"
    Assert-Match -Text $reused.Output -Pattern 'checking against the cluster state saved by the same -RollbackId' -Because '要写明这次核对用的是存档'

    $explicitSame = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -KubectlReply $gone -SeedReportFiles $seed -SnapshotReply @{ ExitCode = 0; Output = '' } `
        -ExtraArgs @{ RollbackId = 'z101-entry'; ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }
    Assert-True -Condition ($null -eq $explicitSame.Error) -Because "首次留空、重跑显式传同值,判为相同。错误: $($explicitSame.Error)"

    $changed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '0' -KubectlReply $gone -ExtraArgs @{ RollbackId = 'z101-entry' } -SeedReportFiles $seed -SnapshotReply @{ ExitCode = 0; Output = '' }
    Assert-RefusedBeforeStop -Run $changed -Label '重跑换了路由模式' -HintPattern '与同一 rollback-id 的存档 [^\r\n]*entry-state\.json 不同'
    Assert-Match -Text $changed.Error -Pattern "GateRouterMode:存档 '1',本次 '0'" -Because '要逐项列出差异'
    Assert-Match -Text $changed.Error -Pattern '-GateRouterMode 1 -ClientEntryMode podip -ConfirmNoGatewayIngress' -Because '要打印存档里的入口参数,照传即可'

    $noSaved = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -KubectlReply $gone -ExtraArgs @{ RollbackId = 'z101-entry-none' }
    Assert-RefusedBeforeStop -Run $noSaved -Label '没有存档、namespace 已删'
    Assert-Match -Text $noSaved.Error -Pattern '当前模式为 \(读不到\)' -Because '没有存档时照旧判为读不到(fail-closed)'
}

# ─────────────────────────────────────────────────────────────────
# 第 0 步 a:login 第三方登录(微信 / QQ)的防误关(-Apply,沙箱)
# Step 1 连 login ConfigMap 与 Secret login-oauth 一起删,k8s_deploy.ps1 自己的防误关闸在 Step 6 面对空 namespace,
# 所以回滚脚本在停服之前按同一份判据(lib Get-LoginAuthProviderDrift)核对。入口三项一律显式,不读 gate / Ingress,
# 只看第三方登录这一道。线上的 AuthProviders 段按 k8s_deploy.ps1 New-LoginAuthProvidersYaml 的形状手写
# (生成器与解析器口径一致由 k8s_login_auth_provider_guard.tests.ps1 的往返用例钉住)。
# ─────────────────────────────────────────────────────────────────

$LoginAuthQuietEntry = @{ ClientEntryMode = 'podip'; ConfirmNoGatewayIngress = $true }
$LoginAuthLiveYaml = @(
    'AuthProviders:',
    '  WeChat:',
    '    AppId: "wx0123456789abcdef"',
    '    AppSecretEnv: "LOGIN_WECHAT_APP_SECRET"',
    '    RequireUnionId: false',
    '    Endpoint: "https://wechat-mock.contract.test:18090"',
    '  QQ:',
    '    AppId: "100000001"'
) -join "`n"
$LoginAuthLiveConfigMap = New-RollbackLoginConfigMapReply -AuthProvidersYaml $LoginAuthLiveYaml
$LoginAuthLiveSecret = @{ ExitCode = 0; Output = 'secret/login-oauth' }
# 照传线上现状的那组参数(与 $LoginAuthLiveYaml 一一对应)。
$LoginAuthKeepArgs = @{ LoginWeChatAppId = 'wx0123456789abcdef'; LoginWeChatEndpoint = 'https://wechat-mock.contract.test:18090'; LoginWeChatAllowOpenId = $true; LoginQQAppId = '100000001' }
$LoginAuthKeepText = '-LoginWeChatAppId wx0123456789abcdef -LoginWeChatEndpoint https://wechat-mock.contract.test:18090 -LoginWeChatAllowOpenId -LoginQQAppId 100000001'

Test-Case '-Apply 第三方登录防误关:线上开着微信 + QQ,本次漏传 → 停服之前、静态预检之前拒绝并给出照传参数串;照传后放行,静态预检与 Step 6 收到同一组 -Login*' {
    $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $LoginAuthQuietEntry `
        -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-RefusedBeforeStop -Run $run -Label '漏传第三方登录' -HintPattern ('要保持现状请照传 ' + [regex]::Escape($LoginAuthKeepText))
    Assert-Match -Text $run.Error -Pattern '线上已开 AuthProviders\.WeChat,本次没给 -LoginWeChatAppId[^\r\n]*并删除 Secret login-oauth' -Because '要写明漏传会关掉微信并删掉 Secret'
    Assert-Match -Text $run.Error -Pattern '线上已开 AuthProviders\.QQ,本次没给 -LoginQQAppId' -Because 'QQ 同样逐项列出'
    Assert-Match -Text $run.Error -Pattern '确认要关闭或改动,加 -ConfirmLoginAuthProviderChange' -Because '要告诉操作者确要关闭时怎么继续'
    Assert-Equal -Expected 0 -Actual $run.DevToolsCalls.Count -Because '防误关只读两次 kubectl,排在要完整渲染 zone-up 的静态预检之前(便宜的先拒)'
    Assert-Equal -Expected 0 -Actual $run.KubectlCalls.Count -Because '入口三项都显式,gate / Ingress 那次 kubectl 不该发生'
    Assert-Equal -Expected 2 -Actual $run.LoginAuthCalls.Count -Because '只读 login ConfigMap 与 Secret 是否存在各一次'
    $configMapCall = @($run.LoginAuthCalls | Where-Object { $_ -match '(^| )go-svc-login-config( |$)' })
    $secretCall = @($run.LoginAuthCalls | Where-Object { $_ -match '(^| )login-oauth( |$)' })
    Assert-Equal -Expected 1 -Actual $configMapCall.Count -Because '读一次 login ConfigMap'
    Assert-Match -Text $configMapCall[0] -Pattern ('^get configmap go-svc-login-config -n ' + [regex]::Escape($SandboxNamespace) + ' --ignore-not-found -o json$') -Because 'ConfigMap 只读、读本 zone namespace'
    Assert-Equal -Expected 1 -Actual $secretCall.Count -Because '读一次 Secret 是否存在'
    Assert-Match -Text $secretCall[0] -Pattern ('^get secret login-oauth -n ' + [regex]::Escape($SandboxNamespace) + ' --ignore-not-found -o name$') -Because 'Secret 只用 -o name 探测存在性,绝不 -o json / yaml 读出密钥值'
    Assert-NotMatch -Text ($run.Output + $run.Error) -Pattern 'contract-login-secret-never-read' -Because 'login ConfigMap 里别的值不进输出'

    $extra = $LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-keep' }
    $passed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $extra -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-True -Condition ($null -eq $passed.Error) -Because "照传线上现状必须放行。错误: $($passed.Error)"
    Assert-Match -Text $passed.Output -Pattern ('login WeChat / QQ login: live state matches this run''s -Login\* arguments \(' + [regex]::Escape($LoginAuthKeepText) + '\)') -Because '放行时要写明核对通过'
    $up = Get-BoundLine -Run $passed -Command 'k8s-zone-up'
    $dry = Get-BoundLine -Run $passed -Command 'k8s-zone-up/dry-run'
    foreach ($pair in @('LoginWeChatAppId=wx0123456789abcdef', 'LoginWeChatEndpoint=https://wechat-mock.contract.test:18090', 'LoginWeChatAllowOpenId=True', 'LoginQQAppId=100000001')) {
        Assert-Match -Text $up -Pattern ('(^| |;)' + [regex]::Escape($pair) + '(;|$)') -Because "Step 6 必须收到 $pair(不粘滞,漏一个就关掉或改掉第三方登录)"
    }
    Assert-NotMatch -Text $up -Pattern 'LoginQQEndpoint|LoginQQAllowOpenId|ConfirmLoginAuthProviderChange' -Because '没给的不转发:默认值只在 k8s_deploy.ps1 一处'
    Assert-Equal -Expected ($up -replace '^\S+ ', '') -Actual ($dry -replace '^\S+ ', '') -Because '静态预检的就是 Step 6 将要执行的那一份(给了 -LoginWeChatAppId 时它会要求 MMORPG_LOGIN_WECHAT_APP_SECRET)'
    $down = Get-BoundLine -Run $passed -Command 'k8s-zone-down'
    Assert-NotMatch -Text $down -Pattern 'Login(WeChat|QQ)|ConfirmLogin' -Because 'Step 1 只删 namespace,不传第三方登录参数'
    $saved = $passed.ReportFiles['entry-state.json']
    foreach ($pair in @('"LoginWeChatAppId": "wx0123456789abcdef"', '"LoginWeChatAllowOpenId": "true"', '"LoginQQAppId": "100000001"', '"ConfirmLoginAuthProviderChange": ""', '"OAuthSecretPresent": true')) {
        Assert-Match -Text $saved -Pattern ([regex]::Escape($pair)) -Because "入口决定存档要记下 $pair(同一 rid 重跑以它为准)"
    }
    Assert-Match -Text $saved -Pattern '"LoginAuthState"' -Because '核对过的 login 第三方登录现状随存档落盘'
    Assert-NotMatch -Text $saved -Pattern 'contract-login-secret-never-read' -Because '存档只记 AuthProviders 的公开值与 Secret 在不在,不记 ConfigMap 里别的值'
}

Test-Case '-Apply 第三方登录防误关:AppId 照传而漏传 Endpoint / AllowOpenId 同样拒绝;-ConfirmLoginAuthProviderChange 不读线上、放行并只透传到 Step 6' {
    $partial = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' `
        -ExtraArgs ($LoginAuthQuietEntry + @{ LoginWeChatAppId = 'wx0123456789abcdef'; LoginQQAppId = '100000001' }) `
        -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-RefusedBeforeStop -Run $partial -Label '漏传 Endpoint / AllowOpenId' -HintPattern '-ConfirmLoginAuthProviderChange'
    Assert-Match -Text $partial.Error -Pattern 'AuthProviders\.WeChat\.Endpoint:线上指向自定义 Endpoint\(沙箱\),本次没给 -LoginWeChatEndpoint 会改连生产地址' -Because '漏传 Endpoint 会把沙箱改连生产'
    Assert-Match -Text $partial.Error -Pattern 'AuthProviders\.WeChat\.RequireUnionId:线上 RequireUnionId: false[^\r\n]*没给 -LoginWeChatAllowOpenId 会翻回 true' -Because '漏传 AllowOpenId 会让拿不到 unionid 的玩家登录失败'
    Assert-NotMatch -Text $partial.Error -Pattern 'AuthProviders\.QQ' -Because 'QQ 与线上一致,不该列为问题'

    $confirmed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + @{ ConfirmLoginAuthProviderChange = $true }) `
        -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-True -Condition ($null -eq $confirmed.Error) -Because "显式确认按本次参数重建,必须放行。错误: $($confirmed.Error)"
    Assert-Equal -Expected 0 -Actual $confirmed.LoginAuthCalls.Count -Because '显式确认后不读线上 login ConfigMap / Secret(地位同 -ConfirmNoGatewayIngress)'
    Assert-Match -Text $confirmed.Output -Pattern 'Explicit -ConfirmLoginAuthProviderChange' -Because '输出要写明本次没有核对线上第三方登录'
    $up = Get-BoundLine -Run $confirmed -Command 'k8s-zone-up'
    Assert-Match -Text $up -Pattern '(^| |;)ConfirmLoginAuthProviderChange=True(;|$)' -Because 'namespace 没删净时 k8s_deploy.ps1 的防误关闸同样要放行,确认开关透传到 Step 6'
    Assert-NotMatch -Text $up -Pattern 'LoginWeChat|LoginQQ' -Because '没给的不转发'
    Assert-NotMatch -Text (Get-BoundLine -Run $confirmed -Command 'k8s-zone-down') -Pattern 'ConfirmLogin' -Because 'Step 1 不传确认开关'
}

Test-Case '-Apply 第三方登录防误关:读不到(kubectl 失败 / 不可用)、namespace 里没有 login ConfigMap 且无存档、AuthProviders 认不出,都判不出 → 停服之前拒绝' {
    $cases = @(
        @{ Label = 'kubectl 以非 0 退出'; Reply = @{ ExitCode = 1; Output = 'Error from server (Forbidden): configmaps "go-svc-login-config" is forbidden' }; Hint = '读不到 namespace [^\r\n]* 里 login 第三方登录的线上现状' },
        @{ Label = 'kubectl 不可用'; Reply = @{ Throw = "The term 'kubectl' is not recognized" }; Hint = '读不到 namespace [^\r\n]* 里 login 第三方登录的线上现状' },
        @{ Label = 'login ConfigMap 不在、没有存档'; Reply = (New-RollbackLoginConfigMapReply -Absent); Hint = '同一 rid 的存档里也没有 login 的线上现状,判不出回滚前有没有开微信 / QQ 登录' },
        @{ Label = 'AuthProviders 是 flow 写法'; Reply = (New-RollbackLoginConfigMapReply -AuthProvidersYaml 'AuthProviders: { WeChat: { AppId: "wx1" } }'); Hint = 'AuthProviders 段认不出' }
    )
    foreach ($c in $cases) {
        $run = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs $LoginAuthQuietEntry -LoginConfigMapReply $c.Reply
        Assert-RefusedBeforeStop -Run $run -Label $c.Label -HintPattern $c.Hint
        Assert-Match -Text $run.Error -Pattern '-ConfirmLoginAuthProviderChange' -Because "$($c.Label):判不出时要告诉操作者确认后怎么继续"
        Assert-Equal -Expected 0 -Actual $run.DevToolsCalls.Count -Because "$($c.Label):拒绝早于静态预检"
    }
}

Test-Case '[10] 第三方登录现状随入口决定存档:同一 rid 重跑、namespace 里已没有 login ConfigMap 时按存档核对;照传放行,漏传按存档差异拒绝' {
    $first = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-rerun' }) `
        -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-True -Condition ($null -eq $first.Error) -Because "首次运行不该失败。错误: $($first.Error)"
    $seed = @{ 'entry-state.json' = $first.ReportFiles['entry-state.json']; 'deploy-facts.json' = $first.ReportFiles['deploy-facts.json'] }
    $goneConfigMap = New-RollbackLoginConfigMapReply -Absent

    $rerun = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-rerun' }) `
        -SeedReportFiles $seed -SnapshotReply @{ ExitCode = 0; Output = '' } -LoginConfigMapReply $goneConfigMap
    Assert-True -Condition ($null -eq $rerun.Error) -Because "namespace 已删时按存档的现状核对,照传即放行。错误: $($rerun.Error)"
    Assert-Match -Text $rerun.Output -Pattern 'checking against the login state saved by the same -RollbackId' -Because '要写明这次核对用的是存档'

    $missing = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + @{ RollbackId = 'z101-login-rerun' }) `
        -SeedReportFiles $seed -SnapshotReply @{ ExitCode = 0; Output = '' } -LoginConfigMapReply $goneConfigMap
    Assert-RefusedBeforeStop -Run $missing -Label '重跑漏传第三方登录' -HintPattern '与同一 rollback-id 的存档 [^\r\n]*entry-state\.json 不同'
    Assert-Match -Text $missing.Error -Pattern "LoginWeChatAppId:存档 'wx0123456789abcdef',本次 ''" -Because '要逐项列出差异'
    Assert-Match -Text $missing.Error -Pattern ([regex]::Escape($LoginAuthKeepText)) -Because '要打印存档里的第三方登录参数,照传即可'
    Assert-Match -Text $first.ReportFiles['entry-state.json'] -Pattern '"Schema": 3' -Because '带第三方登录现状的存档从 Schema 2 起;2026-10-22 集群现状加了 nofile 模式,当前写 Schema 3'
}

Test-Case '[10] 第三方登录接入之前写的存档(Schema 1,没有 LoginAuthState 与 -Login* 键):入口决定不按存档比第三方登录,由防误关核对决定,不会互相卡死' {
    # 手写一份接入之前的存档:入口三项与本用例的显式值一致,没有任何第三方登录字段。
    $oldArchive = @(
        '{',
        '  "Schema": 1,',
        '  "RollbackId": "z101-login-old",',
        '  "Namespace": "mmorpg-zone-contract-test",',
        '  "CapturedAtUtc": "2026-10-18T00:00:00.0000000Z",',
        '  "ClusterState": null,',
        '  "Decision": { "GateRouterMode": "1", "ClientEntryMode": "podip", "GatewayIngress": "(none)" }',
        '}'
    ) -join "`n"
    $seed = @{ 'entry-state.json' = $oldArchive }
    $goneConfigMap = New-RollbackLoginConfigMapReply -Absent

    $refused = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-old' }) `
        -SeedReportFiles $seed -LoginConfigMapReply $goneConfigMap
    Assert-RefusedBeforeStop -Run $refused -Label '旧存档、login ConfigMap 已删、没确认' -HintPattern '同一 rid 的存档里也没有 login 的线上现状'
    Assert-NotMatch -Text $refused.Error -Pattern '与同一 rollback-id 的存档 [^\r\n]*不同' -Because '旧存档没记过第三方登录,照传的 -Login* 不能被判为与存档不同'
    Assert-Match -Text $refused.Output -Pattern 'predates the login WeChat / QQ check' -Because '要写明这份存档不比第三方登录'

    $confirmed = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' `
        -ExtraArgs ($LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-old'; ConfirmLoginAuthProviderChange = $true }) `
        -SeedReportFiles $seed -LoginConfigMapReply $goneConfigMap
    Assert-True -Condition ($null -eq $confirmed.Error) -Because "照传并确认后必须放行(报错里给的出路要走得通)。错误: $($confirmed.Error)"
    $up = Get-BoundLine -Run $confirmed -Command 'k8s-zone-up'
    Assert-Match -Text $up -Pattern '(^| |;)LoginWeChatAppId=wx0123456789abcdef(;|$)' -Because 'Step 6 照传第三方登录参数'
    Assert-Match -Text $up -Pattern '(^| |;)ConfirmLoginAuthProviderChange=True(;|$)' -Because '确认开关透传到 Step 6'
    Assert-Equal -Expected $oldArchive.Trim() -Actual ([string]$confirmed.ReportFiles['entry-state.json']).Trim() -Because '同一 rid 已有存档时不覆盖'

    $liveKept = Invoke-RollbackInSandbox -Apply -DeployDefault '1' -GateRouterMode '1' -ExtraArgs ($LoginAuthQuietEntry + $LoginAuthKeepArgs + @{ RollbackId = 'z101-login-old' }) `
        -SeedReportFiles $seed -LoginConfigMapReply $LoginAuthLiveConfigMap -LoginOAuthSecretReply $LoginAuthLiveSecret
    Assert-True -Condition ($null -eq $liveKept.Error) -Because "namespace 还在、照传线上现状时,旧存档不能让照传被判为不同。错误: $($liveKept.Error)"
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
    Assert-Match -Text $unset.Output -Pattern 'release\s*:[^\r\n]*staging / prod zones must explicitly pass -ReleaseProfile' -Because '留空时摘要区要提醒 staging / prod 必须显式传'

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

Test-Case '第三方登录 -Login* / -ConfirmLoginAuthProviderChange:给了只透传到 Step 6(不进 zone-down),不给不透传;dry-run 预告 -Apply 会读 login ConfigMap 与 Secret 是否存在' {
    $run = Invoke-RollbackDryRun -ExtraArguments @('-LoginWeChatAppId', 'wx0123456789abcdef', '-LoginWeChatEndpoint', 'https://wechat-mock.contract.test:18090',
        '-LoginWeChatAllowOpenId', '-LoginQQAppId', '100000001', '-LoginQQEndpoint', 'https://qq-mock.contract.test', '-LoginQQAllowOpenId')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "显式第三方登录参数的 dry-run 不该失败。输出: $($run.Output)"
    $line = Get-ZoneUpArgsLine -Output $run.Output
    foreach ($pattern in @('-LoginWeChatAppId wx0123456789abcdef', '-LoginWeChatEndpoint https://wechat-mock\.contract\.test:18090', '-LoginWeChatAllowOpenId True',
            '-LoginQQAppId 100000001', '-LoginQQEndpoint https://qq-mock\.contract\.test', '-LoginQQAllowOpenId True')) {
        Assert-Match -Text $line -Pattern "(^|\s)$pattern(\s|$)" -Because '第三方登录不粘滞,回滚漏掉任何一个 Step 6 就关掉或改掉它'
    }
    Assert-NotMatch -Text $line -Pattern 'ConfirmLoginAuthProviderChange' -Because '没给的确认开关不能出现在 Step 6'
    $downLine = @([regex]::Matches($run.Output, '(?m)^[ \t]*dev_tools\.ps1 [^\r\n]*k8s-zone-down[^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $downLine.Count -Because "dry-run 输出里应当恰有一行 Step 1 参数。输出: $($run.Output)"
    Assert-NotMatch -Text $downLine[0] -Pattern 'Login(WeChat|QQ)|ConfirmLogin' -Because 'zone-down 只删 namespace,不传第三方登录参数'
    Assert-Match -Text $run.Output -Pattern 'kubectl get configmap go-svc-login-config -n mmorpg-zone-contract-test --ignore-not-found -o json; kubectl get secret login-oauth -n mmorpg-zone-contract-test --ignore-not-found -o name' -Because 'dry-run 要预告 -Apply 时读的 login ConfigMap 与 Secret(只用 -o name 探测存在性)'
    Assert-Match -Text $run.Output -Pattern 'login 3rd\s*:\s*WeChat -LoginWeChatAppId wx0123456789abcdef' -Because '摘要区要写明 Step 6 以哪组第三方登录参数重建 login'

    $unset = Invoke-RollbackDryRun
    Assert-NotMatch -Text (Get-ZoneUpArgsLine -Output $unset.Output) -Pattern 'LoginWeChat|LoginQQ|ConfirmLogin' -Because '留空不透传:默认值只在 k8s_deploy.ps1 一处'
    Assert-Match -Text $unset.Output -Pattern 'login 3rd\s*:[^\r\n]*-Apply checks the live login ConfigMap' -Because '留空时摘要区要提醒 -Apply 会先核对线上第三方登录'

    $confirm = Invoke-RollbackDryRun -ExtraArguments @('-ConfirmLoginAuthProviderChange')
    Assert-Equal -Expected 0 -Actual $confirm.ExitCode -Because "显式确认的 dry-run 不该失败。输出: $($confirm.Output)"
    Assert-Match -Text (Get-ZoneUpArgsLine -Output $confirm.Output) -Pattern '(^|\s)-ConfirmLoginAuthProviderChange True(\s|$)' -Because '确认开关透传到 Step 6'
    Assert-Match -Text $confirm.Output -Pattern 'Explicit -ConfirmLoginAuthProviderChange' -Because '显式确认时要写明不核对线上'
    Assert-NotMatch -Text $confirm.Output -Pattern 'kubectl get configmap go-svc-login-config' -Because '显式确认后不预告、也不读 login ConfigMap'
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

Test-Case 'dev_tools k8s-zone-rollback:第三方登录 -Login* 与 -ConfirmLoginAuthProviderChange 非空 / 被指定才转给回滚脚本,由它透传到 Step 6' {
    $run = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-LoginWeChatAppId', 'wx0123456789abcdef', '-LoginWeChatEndpoint', 'https://wechat-mock.contract.test:18090',
        '-LoginWeChatAllowOpenId', '-LoginQQAppId', '100000001', '-LoginQQEndpoint', 'https://qq-mock.contract.test', '-LoginQQAllowOpenId', '-ConfirmLoginAuthProviderChange')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "经 dev_tools.ps1 的回滚 dry-run 不该失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'k8s-zone-rollback \(DRY-RUN\)' -Because '不带 -RollbackApply 必须是 dry-run'
    $line = Get-ZoneUpArgsLine -Output $run.Output
    foreach ($pattern in @('-LoginWeChatAppId wx0123456789abcdef', '-LoginWeChatEndpoint https://wechat-mock\.contract\.test:18090', '-LoginWeChatAllowOpenId True',
            '-LoginQQAppId 100000001', '-LoginQQEndpoint https://qq-mock\.contract\.test', '-LoginQQAllowOpenId True', '-ConfirmLoginAuthProviderChange True')) {
        Assert-Match -Text $line -Pattern "(^|\s)$pattern(\s|$)" -Because 'dev_tools.ps1 必须把第三方登录参数原样转给回滚脚本,不能静默吞掉(漏一个,回滚后的 login 就关掉或改掉它)'
    }
    $unset = Invoke-DevToolsRollbackDryRun
    Assert-Equal -Expected 0 -Actual $unset.ExitCode -Because "不给第三方登录参数的回滚 dry-run 不该失败。输出: $($unset.Output)"
    Assert-NotMatch -Text (Get-ZoneUpArgsLine -Output $unset.Output) -Pattern 'LoginWeChat|LoginQQ|ConfirmLogin' -Because '留空不转发:dev_tools.ps1 的空串 / 未指定开关不能冒充显式值'
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

Test-Case 'dev_tools k8s-zone-rollback:消费组留空由回滚脚本按 db.yaml 推导;-RollbackKafkaGroup / -RollbackKafkaLegacyGroup 非空才转发' {
    # 以前 dev_tools.ps1 缺省转发 legacy 共享组 db_rpc_consumer_group 与 -KafkaDrainTimeoutSec;回滚脚本删掉了 Step 2 与该参数,
    # 再转发就会以「找不到参数」失败。
    $derived = Invoke-DevToolsRollbackDryRun
    Assert-Equal -Expected 0 -Actual $derived.ExitCode -Because "经 dev_tools.ps1 的回滚 dry-run 不该失败。输出: $($derived.Output)"
    Assert-Match -Text $derived.Output -Pattern ("group=" + [regex]::Escape($ZoneGroup) + " retire=" + [regex]::Escape($DbGroupBase) + '(\s|$)') -Because '留空时由回滚脚本推导 zone 组与 legacy 组'

    $explicit = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-RollbackKafkaGroup', 'zone_override', '-RollbackKafkaLegacyGroup', 'legacy_override')
    Assert-Equal -Expected 0 -Actual $explicit.ExitCode -Because "显式组名的 dry-run 不该失败。输出: $($explicit.Output)"
    Assert-Match -Text $explicit.Output -Pattern 'group=zone_override retire=legacy_override(\s|$)' -Because '显式值必须原样转给回滚脚本'
}

Test-Case '(g) dev_tools k8s-zone-rollback:回档丢弃语义的新参数给了才转发、各一次;没给就不转发' {
    $forwarded = Invoke-DevToolsRollbackDryRun -ExtraArguments @(
        '-RollbackMySqlDsnEnv', 'CONTRACT_DSN_ENV', '-RollbackRedisPasswordEnv', 'CONTRACT_REDIS_PW_ENV',
        '-RollbackDiscardTool', 'C:/tools/zone_rollback_discard.exe', '-RollbackBarrierProtocol', 'legacy',
        '-RollbackId', 'z101-forwarded', '-RollbackQuiesceSeconds', '420', '-RollbackPitrRestoredTo', '2026-05-15T14:23:00Z',
        '-RollbackAllowEmptyTarget', '-RollbackAcknowledgeNoPostTWrites')
    Assert-Equal -Expected 0 -Actual $forwarded.ExitCode -Because "带新参数的 dry-run 不该失败。输出: $($forwarded.Output)"
    # 回档复核 [11]:以前经本入口走不到这两条路径。-AllowEmptyTarget 只交给 preflight,-AcknowledgeNoPostTWrites 进判定参数(4a / 4b / 5b)。
    Assert-Match -Text $forwarded.Output -Pattern '-mode preflight -allow-empty-target(\s|$)' -Because '-RollbackAllowEmptyTarget 必须转给回滚脚本的 -AllowEmptyTarget'
    foreach ($mode in @('inventory', 'purge', 'verify')) {
        Assert-Match -Text $forwarded.Output -Pattern "-mode $mode [^\r\n]*-acknowledge-no-post-t-writes(\s|$)" -Because "-RollbackAcknowledgeNoPostTWrites 必须转给回滚脚本,$mode 一并带上"
    }
    $preflightLine = @([regex]::Matches($forwarded.Output, '(?m)^[ \t]*Discard tool preflight: [^\r\n]*') | ForEach-Object { $_.Value })
    Assert-Equal -Expected 1 -Actual $preflightLine.Count -Because "dry-run 要预告第 0 步 f 的工具命令。输出: $($forwarded.Output)"
    foreach ($pattern in @('Discard tool preflight: C:/tools/zone_rollback_discard\.exe ', '-mysql-dsn-env CONTRACT_DSN_ENV(\s|$)', '-redis-password-env CONTRACT_REDIS_PW_ENV(\s|$)',
            '-rollback-id z101-forwarded(\s|$)', '-barrier-protocol legacy(\s|$)')) {
        Assert-Match -Text $preflightLine[0] -Pattern $pattern -Because "dev_tools.ps1 必须把对应参数转给回滚脚本($pattern)"
    }
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($preflightLine[0], '-rollback-id ').Count) -Because '只转发一次'
    Assert-Match -Text $forwarded.Output -Pattern 'quiesce=420s(\s|$)' -Because '-RollbackQuiesceSeconds 必须转给回滚脚本'
    Assert-Match -Text $forwarded.Output -Pattern 'pitr\s*:\s*-PitrRestoredTo 2026-05-15T14:23:00Z' -Because '-RollbackPitrRestoredTo 必须转给回滚脚本'

    $plain = Invoke-DevToolsRollbackDryRun
    Assert-Equal -Expected 0 -Actual $plain.ExitCode -Because "不带新参数的 dry-run 不该失败。输出: $($plain.Output)"
    Assert-Match -Text $plain.Output -Pattern 'Discard tool preflight: <zone_rollback_discard> ' -Because '没给工具就打印占位符'
    Assert-NotMatch -Text $plain.Output -Pattern '-redis-password-env' -Because '没给口令变量名就不转发(Redis 没开认证)'
    Assert-NotMatch -Text $plain.Output -Pattern 'allow-empty-target|acknowledge-no-post-t-writes' -Because '两个开关没给就不转发'
    Assert-Match -Text $plain.Output -Pattern 'quiesce=300s(\s|$)' -Because '没给就用回滚脚本自己的缺省值(默认值只在一处)'
    Assert-Match -Text $plain.Output -Pattern 'rollback id : z101-20260515T142300Z-\d{8}T\d{6}Z ' -Because '没给 -RollbackId 时由回滚脚本生成'

    $blocked = Invoke-DevToolsRollbackDryRun -ExtraArguments @('-RollbackAcceptBlockedPlayers', '3')
    Assert-True -Condition ($blocked.ExitCode -ne 0) -Because "-RollbackAcceptBlockedPlayers 转给回滚脚本后在入口被拒(待负责人裁决 #4)。输出: $($blocked.Output)"
    Assert-Match -Text $blocked.Output -Pattern 'AcceptBlockedPlayers 待负责人裁决' -Because '拒绝来自回滚脚本自己的说明,证明参数确实转发到了'
    Assert-NotMatch -Text $blocked.Output -Pattern 'Step 1:' -Because '入口即拒'
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

Test-Case 'dev_tools Invoke-K8sImage:第三方登录 -Login* 与 -ConfirmLoginAuthProviderChange 在调 k8s_image.ps1 之前逐个拒绝(k8s_image.ps1 不部署 login)' {
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'LoginWeChat|LoginQQ|ConfirmLogin' -Because '没给就不出现在下游参数里'
    $cases = @(
        @{ LoginWeChatAppId = 'wx0123456789abcdef' }, @{ LoginWeChatEndpoint = 'https://wechat-mock.contract.test' }, @{ LoginWeChatAllowOpenId = $true },
        @{ LoginQQAppId = '100000001' }, @{ LoginQQEndpoint = 'https://qq-mock.contract.test' }, @{ LoginQQAllowOpenId = $true },
        @{ ConfirmLoginAuthProviderChange = $true }
    )
    foreach ($variables in $cases) {
        $name = @($variables.Keys)[0]
        $reject = Invoke-DevToolsK8sImageWith -Variables $variables
        Assert-True -Condition ($null -ne $reject.Error) -Because "-$name 经 k8s-release-* 不生效(k8s_image.ps1 不部署 login),显式给了必须拒绝而不是静默吞掉"
        Assert-Match -Text $reject.Error -Pattern "k8s-image-\* / k8s-release-\* 不支持 [^:]*-$name(,|:)" -Because "错误文本必须点名 -$name"
        Assert-True -Condition (-not $reject.DownstreamCalled) -Because "-${name}:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)"
    }
}

exit (Complete-TestRun -SuiteName 'k8s_zone_rollback 透传与停服前预检')
