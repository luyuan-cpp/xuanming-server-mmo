#requires -Version 7
<#
.SYNOPSIS
    Disaster-recovery rollback for a single zone — orchestrates
    `k8s-zone-down` → MySQL PITR (manual) → Redis FLUSHDB → Kafka offset reset
    → `k8s-zone-up`.

.DESCRIPTION
    This script is the executable form of the SOP in
    docs/design/zone_data_rollback.md §3 "整 Zone 灾难恢复级回档".

    It will NOT do the destructive steps without -Apply. Default is dry-run.

    What it does (in order):
      0. 预检(只读,先于停服;dry-run 只预告,不执行),两部分都过了才进 Step 1:
         a. 集群现状:以下三项凡是留空的,都要与集群现状核对,一次 kubectl 读 zone namespace 里的
            gate(StatefulSet 或 Deployment,两种形态都认)与 Ingress gateway:
              - -GateRouterMode 留空:gate 模板当前的 GATE_CLIENT_RPC_ROUTER 须与 k8s_deploy.ps1 的 -GateRouterMode 默认值一致;
              - -ClientEntryMode 留空:gate 当前形态(StatefulSet = external,Deployment = podip)须与 k8s_deploy.ps1 的
                -ClientEntryMode 默认值一致;
              - -GatewayIngressHost 与 -ConfirmNoGatewayIngress 都没给:namespace 里不能有 Ingress gateway
                (Step 1 会连它一起删,Step 6 留空不重建),且必须读得到 —— 一个 gate 都没有时(如上一次回滚中途放弃、
                namespace 已被 Step 1 删掉)判为读不到,不当成"确定没有"。
            不一致或判不出即在停服之前拒绝,要求显式传参;显式传了的项不核对。
         b. 静态预检:把 Step 6 将要传给 dev_tools.ps1 k8s-zone-up 的同一份参数加 -DryRun 执行一遍
            (k8s_deploy.ps1 -DryRun 只在本地渲染清单、不调 kubectl),非 0 即拒绝。参数组合、档位与密钥、发布门禁的
            规则只在 k8s_deploy.ps1 一处,这里不复制,拼错或矛盾的组合在停服之前就暴露。
      1. `k8s-zone-down -ZoneName <zone>` — stop all writers(删掉整个 zone namespace)
      2. Wait for Kafka consumer LAG to drain (in-flight writes settle)
      3. ⚠️ MySQL PITR is NOT automated — print runbook reference and pause
         for confirmation. Operator runs PITR manually, hits ENTER to continue.
      4. Redis FLUSHDB on the zone's data Redis (cache rebuilds from MySQL)
      5. Kafka offset reset on db_task_zone_<id> (avoids replaying post-target writes)
      6. `k8s-zone-up -ZoneName <zone> -ZoneId <id>` — restart zone
         ⚠️ gate 路由模式不粘滞:k8s_deploy.ps1 每次部署都把 -GateRouterMode 原样重写进 gate env
         (默认 "1",turn-based §22 D75),不从集群读回旧值。该 zone 若以 "0" 回退运行,回滚时
         **必须传 -GateRouterMode 0**,否则本步把它静默切回 "1";回退的原因(如路由服不可用)
         若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target。留空 = 不覆盖,沿用 k8s_deploy.ps1 默认值;
         -Apply 下留空要先过第 0 步预检,预检拦住的正是这种静默翻转。
         ⚠️ 集群外入口参数同样不粘滞(集群外入口 D80 / D88 / D91):以 external 运行的 zone 回滚时,须照传上一次部署的
         -ClientEntryMode external、-GateServiceType、-GateNodePortBase、-ClientPublicHost / -GateClientHostTemplate、
         -GateExternalTrafficPolicy,经 Ingress 对外的再加 -GatewayIngressHost / -GatewayTrustedProxies(及 ClassName / TlsSecret),
         逐个 zone 切 external 的窗口里还有 -RequireClientEndpoint。Step 1 已删掉整个 namespace,k8s_deploy.ps1 自己的
         集群现状预检(换形态 / 地址漂移 / 已有 Ingress)在这一步面对的是空 namespace,一条都触发不了 —— 所以核对前移到第 0 步。
         -AllowDisruptiveSwitch 只透传到本步,仅在 namespace 没删净、上述预检真的触发时起作用,不豁免第 0 步。
         ⚠️ 发布档位与开发口令认证同样不粘滞:staging / prod zone 必须传 -ReleaseProfile(留空按 dev_tools.ps1 默认 dev 档部署,
         login 以 dev 模式、密钥回落占位值拉起);dev zone 依赖开发口令登录时照传 -LoginDevPasswordAuth
         (共享口令仍走环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET),漏传则 login 没有任何认证配置、Step 7 冒烟登录失败。
      7. Print verification checklist

    Why MySQL is manual:
      Picking the right binlog cut point and validating against shadow DB is
      a judgement call no script can safely make. See
      docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure.

.EXAMPLE
    # Preview what would happen
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z"

.EXAMPLE
    # Run the destructive flow
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -RedisHost zone-redis.mmorpg-zone-test1 `
        -KafkaBootstrap kafka:9092 `
        -Apply

.EXAMPLE
    # zone 以 "0"(gate 直连模式)回退运行时的回滚:必须显式传 -GateRouterMode 0。
    # 回退态不粘滞,Step 6 留空会落回 k8s_deploy.ps1 默认的 "1"(口径见 k8s_deploy.ps1 -GateRouterMode 参数注释);
    # -Apply 留空时第 0 步预检读到 gate 当前是 "0",在停服之前拒绝。
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -RedisHost zone-redis.mmorpg-zone-test1 `
        -KafkaBootstrap kafka:9092 `
        -GateRouterMode 0 `
        -Apply

.EXAMPLE
    # 以 external 运行、经 Ingress 对外的 staging / prod zone:照传上一次部署时的档位、入口与地址参数(值以该 zone 的部署记录为准;
    # 第 0 步预检会把集群里 gate StatefulSet 现有的地址 env 与 Ingress host 打出来供核对)。
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -ReleaseProfile prod `
        -KubeContext <目标集群 context> `
        -ClientEntryMode external -GateServiceType NodePort -GateNodePortBase 30100 -ClientPublicHost 203.0.113.10 `
        -GatewayIngressHost gateway.test1.example.com -GatewayTrustedProxies 10.244.0.0/16 `
        -Apply
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$ZoneName,

    [Parameter(Mandatory = $true)]
    [int]$ZoneId,

    [Parameter(Mandatory = $true)]
    [string]$TargetTime,    # ISO 8601 UTC, e.g. "2026-05-15T14:23:00Z"

    # Zone-local Redis (cache that will be FLUSHDB'd).
    [string]$RedisHost = "",
    [string]$RedisPort = "6379",
    [string]$RedisPassword = "",
    [int]$RedisDB = 0,

    # Kafka cluster bootstrap.
    [string]$KafkaBootstrap = "kafka:9092",
    # 留空 = 按 -ZoneId 推导成 db_task_zone_<id>（go/db 与 go/login 两侧都是这么拼的:
    # go/db/internal/config.go DbTaskTopic / go/login/internal/config/config.go）。
    # 旧默认值 "db_task_topic" 是个**全仓不存在的 topic** —— offset reset 会「成功」地
    # 重置一个空 topic，回滚后 go/db 照样重放目标时间点之后的写入。
    # go/db 的 Kafka.TopicGeneration > 1 时真实名字带 _g<gen> 后缀，这里推不出来,
    # 必须显式传 -KafkaTopic。
    [string]$KafkaTopic = "",
    [string]$KafkaGroup = "db_rpc_consumer_group",

    # Zone restart args.
    #
    # 必填且必须是不可变 tag。以前这里默认 "…:latest" —— 灾难回滚脚本自己
    # 默认了一个可变 tag,等于"回档到某个时间点"时把 zone 拉回了**当前**
    # 镜像,而不是那个时间点在跑的版本。回滚必须由操作者显式指定回到哪一版。
    [Parameter(Mandatory = $true)]
    [string]$NodeImage,
    [string]$NamespacePrefix = "mmorpg-zone",
    # 发布档位,经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传给 k8s_deploy.ps1 -ReleaseProfile。
    # 留空 = 不覆盖,落到 dev_tools.ps1 的默认 dev 档;staging / prod zone 回滚必须显式传,否则 Step 6 以 dev 档重建:
    # login 以 Mode dev 拉起、各密钥回落占位值(安全方向的 fail-open)。只影响 Step 6(以及第 0 步静态预检),
    # Step 1 的 zone-down 与档位无关,不传。与 dev_tools.ps1 同一 ValidateSet,拼错在本脚本入口就拒。
    [ValidateSet("", "dev", "staging", "prod")]
    [string]$ReleaseProfile = "",

    # gate 客户端 RPC 路由模式,经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传给 k8s_deploy.ps1 -GateRouterMode。
    # 默认留空 = 不覆盖下游默认值(k8s_deploy.ps1 默认 "1",turn-based §22 D75):默认值只允许存在于
    # k8s_deploy.ps1 一处,写法同 dev_tools.ps1 / k8s_image.ps1 的透传,非空才透传。
    # 回退态不粘滞(口径见 k8s_deploy.ps1 -GateRouterMode 参数注释):那边每次部署都把本值原样重写进 gate env,
    # 不从集群读回旧值。以 "0" 回退运行的 zone,回滚时必须显式传 -GateRouterMode 0,否则 Step 6 静默落回 "1";
    # 回退的原因(如路由服不可用)若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target。
    # 留空时 -Apply 由预检(Assert-ZoneEntryKeptOnRollback)兜底:集群当前模式与默认值不一致或读不到即拒绝,
    # 不再只靠文字提醒;显式给值即视为操作者的决定(包括有意从 "0" 切回 "1"),不核对。
    # 与 dev_tools.ps1 同一 ValidateSet:拼错在本脚本入口就拒 —— 若放到 Step 6 才由下游拒,
    # 那时 zone 已停服、数据已回档,回滚卡在半路。
    [ValidateSet("", "0", "1")]
    [string]$GateRouterMode = "",

    # ── 集群外客户端入口(k8s_deploy.ps1 同名参数,集群外入口 D76–D93),经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传 ──
    # 写法同 -GateRouterMode:一律默认留空 = 不覆盖下游默认值(默认值只留在 k8s_deploy.ps1 一处),非空才透传;
    # 枚举型与 dev_tools.ps1 同一 ValidateSet,拼错在本脚本入口就拒。
    # 全部不粘滞:Step 1 删掉整个 zone namespace(gate StatefulSet、每序号 Service、Ingress 一并删除),Step 6 只按本次参数重建,
    # 漏传就落回默认值 —— gate 被重建成 podip、对外地址换掉或 Ingress 丢失,集群外玩家全部连不上。以 external 运行的 zone
    # 回滚时必须照传上一次部署的同一组值。-Apply 下由第 0 步预检兜底两类漏传:-ClientEntryMode 留空而 gate 当前形态与默认值不同,
    # 以及 -GatewayIngressHost 留空而 namespace 里有 Ingress gateway,都在停服之前拒绝。地址参数(-GateServiceType、
    # -GateNodePortBase、-ClientPublicHost、-GateClientHostTemplate、-GateExternalTrafficPolicy)不与集群逐项核对:显式给了
    # -ClientEntryMode 即视为操作者的决定;预检把 gate StatefulSet 现有的地址 env 打出来供核对。参数自身的组合错误
    # (LoadBalancer 缺模板、nodePort 越界、host 带 scheme …)由第 0 步静态预检交给 k8s_deploy.ps1 判定,同样在停服之前拒绝。
    [ValidateSet("", "podip", "external")]
    [string]$ClientEntryMode = "",
    [string]$ClientPublicHost = "",
    [string]$GateClientHostTemplate = "",
    # -1 = 未指定(不透传,由 k8s_deploy.ps1 默认值决定);其余值原样透传,由下游校验(同 dev_tools.ps1)。
    [int]$GateNodePortBase = -1,
    [ValidateSet("", "Local", "Cluster")]
    [string]$GateExternalTrafficPolicy = "",
    # external 下决定每序号 Service 的类型,同属地址参数:漏传落回 NodePort,LoadBalancer 的 zone 对外地址整体换掉。
    [ValidateSet("", "ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "",
    # login / scene_manager ConfigMap 的 RequireClientEndpoint。逐个 zone 切 external 的窗口里(ingress_final §6 第 2 批)
    # 已切 zone 以 false 运行,回滚时须照传 -RequireClientEndpoint false,否则 auto 按 external 取 true,跨 zone 跳转到未切 zone 失败。
    # 现状在 ConfigMap 里,预检不核对。
    [ValidateSet("", "auto", "true", "false")]
    [string]$RequireClientEndpoint = "",
    # 集群内 login 的开发口令认证(k8s_deploy.ps1 同名开关,只允许 dev 档,由它的 preflight 把关),经 Step 6 透传;
    # switch 只在被指定时透传,共享口令仍走环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET、不经参数。
    # 不粘滞:dev zone 依赖开发口令登录时照传,漏传则 Step 6 生成的 login 没有任何认证配置(fail-closed),登录全部失败。
    [switch]$LoginDevPasswordAuth,
    # gateway Ingress(D91,由 zone-up 随 gateway 生成)。给了 -GatewayIngressHost 就必须同时给 -GatewayTrustedProxies:
    # 与 k8s_deploy.ps1 preflight(lib Test-ClientEntryPreflight)同一规则,在本脚本入口先拒(dry-run 也报),不拖到 Step 6。
    [string]$GatewayIngressHost = "",
    [string]$GatewayIngressClassName = "",
    [string]$GatewayIngressTlsSecret = "",
    # 逗号分隔的 CIDR,原样透传。
    [string]$GatewayTrustedProxies = "",
    # 显式声明本次回滚后该 zone 不要 Ingress gateway:Ingress 这一项的"显式决定",地位同显式给 -GateRouterMode / -ClientEntryMode。
    # 给了就不核对 Ingress(namespace 里现有的 Ingress gateway 随 Step 1 删除,Step 6 不重建);不给且 -GatewayIngressHost 留空时,
    # 第 0 步要读到"确定没有"才放行 —— 集群读不到、或 namespace 里一个 gate 都没有(已被删,无从判断原先有没有)都拒绝。
    # 与 -GatewayIngressHost 互斥,同时给在入口即拒。
    [switch]$ConfirmNoGatewayIngress,
    # 确认本次就是要切换 gate 入口形态或改客户端地址来源(k8s_deploy.ps1 同名开关),经 Step 6 透传;switch 只在被指定时透传。
    # 通常不起作用:Step 1 删掉整个 zone namespace 后,k8s_deploy.ps1 的集群现状闸(换形态 / 地址漂移)在 Step 6 面对的是空 namespace。
    # 它只在 namespace 没删净时起作用(zone-down 删 namespace 允许失败):那时 Step 6 按集群现状判定,形态或地址与本次参数不同
    # 而没给它,就在写操作之前拒绝,回滚停在 Step 6(zone 已停服、数据已回档)—— 本次回滚确要切换形态或地址时预先加上。
    # 不豁免第 0 步:那里拦的是"留空落回默认值"的静默翻转,要换就显式传目标 -GateRouterMode / -ClientEntryMode,
    # Ingress 显式传 -GatewayIngressHost 或 -ConfirmNoGatewayIngress;"已有 Ingress 却缺 trusted proxies"在 k8s_deploy.ps1 那边同样不受它豁免。
    [switch]$AllowDisruptiveSwitch,

    # kubectl 目标集群,语义同 k8s_deploy.ps1 的 -KubeContext / -KubeConfig(非空才追加 --context / --kubeconfig)。
    # 第 0 步预检的只读 kubectl 与 Step 1 / Step 6(经 dev_tools.ps1 透传给 k8s_deploy.ps1)用同一组值,保证核对与改动落在同一集群。
    # 留空 = kubectl 当前 context;本机默认 context 可能属于别的项目,回滚前务必确认或显式指定。
    [string]$KubeContext = "",
    [string]$KubeConfig = "",

    # Skip the manual MySQL prompt (use only when PITR was done OOB earlier).
    [switch]$SkipMySqlPause,

    # Drain budget for Kafka consumer LAG before restart.
    [int]$KafkaDrainTimeoutSec = 300,

    [switch]$Apply
)

$ErrorActionPreference = "Stop"

# topic 名必须与 go/db 实际消费的一致,否则第 5 步会「成功」重置一个不存在的 topic。
if ($KafkaTopic -eq "") { $KafkaTopic = "db_task_zone_$ZoneId" }
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")

. (Join-Path $ScriptDir "lib\release_common.ps1")
# 只为 Get-GateAddressHint 取 Get-ClientEntryContract(gate 生成器内部 env 名与端口模式取值的唯一来源);
# 该库只定义函数、不读脚本级变量、不设 StrictMode,dot-source 无副作用。
. (Join-Path $ScriptDir "lib\k8s_client_entry.ps1")

<#
.SYNOPSIS
    读 k8s_deploy.ps1 参数块里若干参数的默认值,返回 @{ 参数名 = 默认值 | $null };某项不是字符串字面常量或读不到时为 $null。

.DESCRIPTION
    部署层默认值只允许存在于 k8s_deploy.ps1 一处(-GateRouterMode:turn-based §22 D75;-ClientEntryMode:集群外入口 D80):
    这里按语法树读它们,不在本脚本抄一份 "1" / "podip"。那边哪天改了默认值,预检自动跟着变,
    不会出现「按旧默认放行、Step 6 实际翻到新默认」。某项为 $null 时由调用方 fail-closed(要求显式传该参数),不猜。
    文件只解析一次。
#>
function Get-DeployParamDefaults {
    param(
        [Parameter(Mandatory = $true)][string]$DeployScriptPath,
        [Parameter(Mandatory = $true)][string[]]$Names
    )

    $defaults = @{}
    foreach ($name in $Names) { $defaults[$name] = $null }
    if (-not (Test-Path -LiteralPath $DeployScriptPath)) { return $defaults }
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($DeployScriptPath, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0 -or $null -eq $ast.ParamBlock) { return $defaults }
    foreach ($name in $Names) {
        $parameters = @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq $name })
        if ($parameters.Count -eq 1 -and $parameters[0].DefaultValue -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $defaults[$name] = $parameters[0].DefaultValue.Value
        }
    }
    return $defaults
}

<#
.SYNOPSIS
    gate 工作负载(StatefulSet 或 Deployment)Pod 模板里的 GATE_CLIENT_RPC_ROUTER,折算成 gate 实际生效的模式 "1" / "0"。纯函数。

.DESCRIPTION
    返回 [pscustomobject]@{ Mode = "1" | "0" | $null; Detail = 说明 };Mode 为 $null 表示判不出,由调用方 fail-closed。
    折算口径与 gate 进程一致(cpp/nodes/gate/gate_router_mode.h 的 ParseRouterModeFlag / ResolveRouterModeFromEnv):
    去掉首尾 ASCII 空白、转小写后为 "1" / "true" / "on" 才是路由模式,其它值以及**变量未设置**一律是直连 "0"
    —— 早期 k8s_deploy.ps1 不注入这个变量,那样部署的 gate 跑的就是直连。
    以下情况判不出:名为 gate 的容器不是恰好一个、该变量重复出现(不猜哪条生效)、用 valueFrom 注入(读不到字面值)。
    两种形态都由 k8s_deploy.ps1 写同一个字面值 env(external 的 StatefulSet 见 lib New-GateStatefulSetYaml)。
    StatefulSet 是 OnDelete,模板是最近一次部署的意图,个别 Pod 可能还没重建;回滚 Step 1 会删掉全部 Pod,要比的正是模板。
#>
function Get-GateRouterModeFromPodSpec {
    param(
        [AllowNull()]$PodSpec,
        # 只用于报错文本,例如 "StatefulSet"。
        [Parameter(Mandatory = $true)][string]$Kind
    )

    $containers = @($PodSpec.containers | Where-Object { $_.name -eq 'gate' })
    if ($containers.Count -ne 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "gate $Kind 里名为 gate 的容器有 $($containers.Count) 个(k8s_deploy.ps1 只生成一个)" }
    }
    $entries = @($containers[0].env | Where-Object { $_.name -eq 'GATE_CLIENT_RPC_ROUTER' })
    if ($entries.Count -eq 0) {
        return [pscustomobject]@{ Mode = '0'; Detail = 'gate 未设置 GATE_CLIENT_RPC_ROUTER,进程默认即直连' }
    }
    if ($entries.Count -gt 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "gate 的 GATE_CLIENT_RPC_ROUTER 出现 $($entries.Count) 次,不猜哪条生效" }
    }
    if ($null -ne $entries[0].valueFrom) {
        return [pscustomobject]@{ Mode = $null; Detail = 'gate 的 GATE_CLIENT_RPC_ROUTER 用 valueFrom 注入,读不到字面值' }
    }
    $raw = [string]$entries[0].value
    # 只去 ASCII 空白,与 C++ 侧 std::isspace(C locale)同一集合。
    $normalized = $raw.Trim([char[]]@(' ', "`t", "`n", "`v", "`f", "`r")).ToLowerInvariant()
    $mode = if ($normalized -in @('1', 'true', 'on')) { '1' } else { '0' }
    return [pscustomobject]@{ Mode = $mode; Detail = "gate env GATE_CLIENT_RPC_ROUTER=`"$raw`"" }
}

<#
.SYNOPSIS
    gate StatefulSet 模板里的地址 env → 一段"照传哪些参数"的提示文本。纯函数。

.DESCRIPTION
    env 名、端口模式取值与每序号 Service 标签一律取自 lib/k8s_client_entry.ps1 的 Get-ClientEntryContract
    (New-GateStatefulSetYaml 写入、k8s_deploy.ps1 Get-GateAddressDrift 比对的同一份;生成器内部 env,C++ 不读),本脚本不另写字面量:
    GateClientPortModeEnv 为 GateClientPortModeNodePort 对应 -GateServiceType NodePort + -GateNodePortBase <GateNodePortBaseEnv 的值>,
    为 GateClientPortModeService 对应 -GateServiceType LoadBalancer;GateClientHostTemplateEnv 对应 -GateClientHostTemplate
    ({zone} 已在生成期渲染,照传渲染后的值即可);GateClientPublicHostEnv 对应 -ClientPublicHost。
    externalTrafficPolicy 写在每序号 Service 上、不在模板里,只指路(按 GateOrdinalLabelKey 标签列出)。
    只给提示、不做比对:地址计划的推导(OpsProfile 改写、zones 配置覆盖、模板渲染)只在 k8s_deploy.ps1 一处。
#>
function Get-GateAddressHint {
    param([AllowNull()]$PodSpec)

    $contract = Get-ClientEntryContract
    $portModeEnv = $contract.GateClientPortModeEnv
    $container = @($PodSpec.containers | Where-Object { $_.name -eq 'gate' }) | Select-Object -First 1
    $parts = @()
    foreach ($name in @($portModeEnv, $contract.GateNodePortBaseEnv, $contract.GateClientPortEnv,
            $contract.GateClientHostTemplateEnv, $contract.GateClientPublicHostEnv)) {
        $entry = @($container.env | Where-Object { $_.name -ceq $name }) | Select-Object -First 1
        if ($null -ne $entry) { $parts += "$name='$([string]$entry.value)'" }
    }
    $envText = if ($parts.Count -gt 0) { "模板地址 env:" + ($parts -join ' ') } else { '模板里没有地址 env' }
    return ($envText +
        "($portModeEnv=$($contract.GateClientPortModeNodePort) 对应 -GateServiceType NodePort -GateNodePortBase <$($contract.GateNodePortBaseEnv)>," +
        "$portModeEnv=$($contract.GateClientPortModeService) 对应 -GateServiceType LoadBalancer;" +
        "$($contract.GateClientHostTemplateEnv) 对应 -GateClientHostTemplate,$($contract.GateClientPublicHostEnv) 对应 -ClientPublicHost;" +
        "externalTrafficPolicy 在每序号 Service 上,见 kubectl get svc -l $($contract.GateOrdinalLabelKey))")
}

# kubectl 失败或输出不可解析时的现状:三项都判不出,由 Assert-ZoneEntryKeptOnRollback 按需拒绝。
function New-UnknownZoneEntryState {
    param([Parameter(Mandatory = $true)][string]$Detail)

    return [pscustomobject]@{
        RouterMode    = $null
        RouterDetail  = $Detail
        EntryMode     = $null
        EntryDetail   = $Detail
        IngressKnown  = $false
        Ingress       = $null
        IngressDetail = $Detail
    }
}

<#
.SYNOPSIS
    第 0 步预检的 kubectl 参数(只读):一次取回 zone namespace 里的 gate StatefulSet / gate Deployment / Ingress gateway。
    dry-run 打印的就是这一份,-Apply 时原样执行。

.DESCRIPTION
    多个具名资源加 --ignore-not-found:不存在的静默跳过(退出码 0),找到的合成一个 List 输出,一个都没有时无输出。
    namespace 不存在时按名 get 同样是 NotFound、被一并忽略,由调用方把「gate 一个都没有」判为读不到。
    -KubeContext / -KubeConfig 非空才追加 --context / --kubeconfig,与 Step 1 / Step 6 透传给 k8s_deploy.ps1 的是同一组值。
#>
function Get-ZoneEntryKubectlArgs {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = '',
        [int]$RequestTimeoutSec = 15
    )

    $kubectlArgs = @('get', 'statefulset/gate', 'deployment/gate', 'ingress/gateway', '-n', $Namespace, '-o', 'json', '--ignore-not-found', "--request-timeout=${RequestTimeoutSec}s")
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $kubectlArgs += @('--context', $KubeContext) }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $kubectlArgs += @('--kubeconfig', $KubeConfig) }
    return , $kubectlArgs
}

<#
.SYNOPSIS
    执行一次 Get-ZoneEntryKubectlArgs 给出的只读 kubectl,返回 zone 入口现状:
    [pscustomobject]@{ RouterMode; RouterDetail; EntryMode; EntryDetail; IngressKnown; Ingress; IngressDetail }。

.DESCRIPTION
    - RouterMode:"1" | "0" | $null(判不出),口径见 Get-GateRouterModeFromPodSpec;StatefulSet 与 Deployment 两种 gate 都认。
    - EntryMode:gate 形态折算的 -ClientEntryMode(k8s_deploy.ps1 模式矩阵,集群外入口 D80):StatefulSet = "external",
      Deployment = "podip";$null = 判不出 —— 一个 gate 都没有(如 namespace 已被上一次中途放弃的回滚删掉),
      或两种形态同时存在(上一次形态切换没做完,不猜哪个在服务)。external 时 EntryDetail 附带模板里的地址 env(Get-GateAddressHint)。
    - IngressKnown / Ingress:Ingress 为 $null(不存在)或 { Host; ClassName; TlsSecret }。kubectl 读成功且 namespace 里至少有一个 gate,
      或读到了 Ingress gateway,才 IngressKnown = $true;一个 gate 都没有也没有 Ingress 时判为读不到(namespace 多半已被删,
      "现在没有"证明不了"回滚前没有")。
    kubectl 不可用或失败(不可达、Forbidden)、输出不是 JSON 时三项都判不出。只读一次、不重试。
    --request-timeout 只约束 HTTP 请求,约束不了凭证插件等请求之外的等待;此时尚未产生任何副作用,
    卡住时操作者 CTRL+C 即可,不为此引入进程级截止。
#>
function Get-ClusterZoneEntryState {
    param(
        [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
        [Parameter(Mandatory = $true)][string]$Namespace
    )

    try {
        $lines = @(& kubectl @KubectlArgs 2>&1)
    }
    catch {
        return (New-UnknownZoneEntryState -Detail "kubectl 调用失败:$($_.Exception.Message)")
    }
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        $message = (($lines | ForEach-Object { "$_" }) -join ' ').Trim()
        return (New-UnknownZoneEntryState -Detail "kubectl $($KubectlArgs -join ' ') 失败(退出码 $exitCode):$message")
    }
    # stderr 上的告警行(ErrorRecord)不是 JSON 的一部分。
    $json = ($lines | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } | ForEach-Object { "$_" }) -join "`n"
    $items = @()
    if (-not [string]::IsNullOrWhiteSpace($json)) {
        try {
            $document = $json | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            return (New-UnknownZoneEntryState -Detail "kubectl 输出不是合法的 JSON:$($_.Exception.Message)")
        }
        # 多个具名资源时 kubectl 输出 List;单个对象的写法也认,不依赖 kubectl 版本的包装细节。
        $items = if ($document.kind -eq 'List') { @($document.items) } else { @($document) }
    }

    $gates = @($items | Where-Object { $null -ne $_ -and $_.kind -in @('StatefulSet', 'Deployment') -and $_.metadata.name -eq 'gate' })
    $routerMode = $null
    $entryMode = $null
    if ($gates.Count -eq 0) {
        $routerDetail = "namespace $Namespace 里既没有 gate StatefulSet 也没有 gate Deployment(可能已被上一次中途放弃的回滚删掉)"
        $entryDetail = $routerDetail
    }
    elseif ($gates.Count -gt 1) {
        $routerDetail = "namespace $Namespace 里 gate StatefulSet 与 gate Deployment 同时存在(上一次形态切换没做完),不猜哪个在服务"
        $entryDetail = $routerDetail
    }
    else {
        $gate = $gates[0]
        $kind = [string]$gate.kind
        $router = Get-GateRouterModeFromPodSpec -PodSpec $gate.spec.template.spec -Kind $kind
        $routerMode = $router.Mode
        $routerDetail = "$kind 模板:$($router.Detail)"
        if ($kind -eq 'StatefulSet') {
            $entryMode = 'external'
            $entryDetail = "gate 为 StatefulSet;" + (Get-GateAddressHint -PodSpec $gate.spec.template.spec)
        }
        else {
            $entryMode = 'podip'
            $entryDetail = 'gate 为 Deployment'
        }
    }

    $ingresses = @($items | Where-Object { $null -ne $_ -and $_.kind -eq 'Ingress' -and $_.metadata.name -eq 'gateway' })
    $ingress = $null
    $ingressKnown = $true
    $ingressDetail = "namespace $Namespace 里没有 Ingress gateway"
    if ($ingresses.Count -eq 0 -and $gates.Count -eq 0) {
        # 一个 gate 都没有 = namespace 多半已被删(上一次回滚中途放弃),Ingress 当时已随 namespace 一起没了:
        # "现在没有"证明不了"回滚前没有",与路由模式、入口形态同一口径判为读不到(fail-closed)。
        $ingressKnown = $false
        $ingressDetail = "namespace $Namespace 里一个 gate 都没有(namespace 可能已被上一次中途放弃的回滚删掉),无从判断回滚前有没有 Ingress gateway"
    }
    elseif ($ingresses.Count -gt 0) {
        # rules / tls 缺席时 @($null)[0] 取到 $null,不抛"对空数组取下标"。
        $ingress = [pscustomobject]@{
            Host      = [string](@($ingresses[0].spec.rules)[0].host)
            ClassName = [string]$ingresses[0].spec.ingressClassName
            TlsSecret = [string](@($ingresses[0].spec.tls)[0].secretName)
        }
        $ingressDetail = "namespace $Namespace 里有 Ingress gateway(host='$($ingress.Host)' class='$($ingress.ClassName)' tls='$($ingress.TlsSecret)')"
    }

    return [pscustomobject]@{
        RouterMode    = $routerMode
        RouterDetail  = $routerDetail
        EntryMode     = $entryMode
        EntryDetail   = $entryDetail
        IngressKnown  = $ingressKnown
        Ingress       = $ingress
        IngressDetail = $ingressDetail
    }
}

<#
.SYNOPSIS
    第 0 步预检(-Apply):留空的 -GateRouterMode / -ClientEntryMode / -GatewayIngressHost 逐项与集群现状核对,
    任一项不一致或判不出就在停服之前抛出,所有问题一次列全。

.DESCRIPTION
    k8s_deploy.ps1 每次部署都按本次参数重写 gate / Ingress、不从集群读回旧值(不粘滞);而 Step 1 删掉整个 zone namespace 之后,
    k8s_deploy.ps1 自己的集群现状预检(换形态、地址漂移、已有 Ingress)在 Step 6 面对的是空 namespace,一条都触发不了。
    所以留空的项只在「读得到、且与 Step 6 将要生成的一致」时放行;判不出同样拒绝 —— 判不出就无法证明不会翻转(AGENTS §11.3 fail-closed):
      - 路由模式:留空 = 落回默认值,与 gate 当前模式不同就是静默翻转(以 "0" 回退运行的 zone 被切回 "1",
        回退原因若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target);
      - 入口形态:留空 = 落回默认值,external 的 zone 被重建成 podip,集群外玩家全部连不上;
      - Ingress:留空 = Step 6 不生成 Ingress,而 Step 1 已连同 namespace 删掉它,集群外玩家拿不到 gate 分配。
        读得到且确实没有才放行;namespace 里一个 gate 都没有时判不出(见 Get-ClusterZoneEntryState)。
    拒绝发生在 Step 1 之前,此时没有任何改动;操作者确认后显式传参重跑即可(Ingress 的显式决定是 -GatewayIngressHost 或
    -ConfirmNoGatewayIngress)。显式给了的项不传对应开关,不在这里核对。
#>
function Assert-ZoneEntryKeptOnRollback {
    param(
        # Get-ClusterZoneEntryState 的返回值。
        [Parameter(Mandatory = $true)]$State,
        # Get-DeployParamDefaults 的返回值,须含 GateRouterMode / ClientEntryMode。
        [Parameter(Mandatory = $true)][hashtable]$DeployDefaults,
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$DeployScriptPath,
        [switch]$CheckRouterMode,
        [switch]$CheckEntryMode,
        [switch]$CheckIngress
    )

    $problems = @()
    if ($CheckRouterMode) {
        $deployDefault = $DeployDefaults['GateRouterMode']
        if ($null -ne $deployDefault -and $State.RouterMode -eq $deployDefault) {
            Write-Host "  gate 当前模式 `"$($State.RouterMode)`"($($State.RouterDetail))= k8s_deploy.ps1 默认值,留空不会翻转" -ForegroundColor Green
        }
        else {
            $defaultText = if ($null -ne $deployDefault) { "`"$deployDefault`"" } else { "(读不到:$DeployScriptPath 的 -GateRouterMode 默认值不是字面常量或文件无法解析)" }
            $clusterText = if ($null -ne $State.RouterMode) { "`"$($State.RouterMode)`"" } else { '(读不到)' }
            $problems += ("gate 路由模式:未传 -GateRouterMode 时 Step 6 按 k8s_deploy.ps1 默认值 $defaultText 重新部署 gate," +
                "而 gate 当前模式为 $clusterText($($State.RouterDetail))。两者不一致或判不出时留空可能把回退态静默翻转" +
                "(以 `"0`" 回退运行的 zone 被切回 `"1`",回退原因若仍在,登录 / 匹配全部 no_target)。" +
                "请确认该 zone 应以哪种模式拉起,显式传 -GateRouterMode 0 或 -GateRouterMode 1。")
        }
    }
    if ($CheckEntryMode) {
        $deployDefault = $DeployDefaults['ClientEntryMode']
        if ($null -ne $deployDefault -and $State.EntryMode -eq $deployDefault) {
            Write-Host "  gate 当前入口形态 `"$($State.EntryMode)`"($($State.EntryDetail))= k8s_deploy.ps1 默认值,留空不会翻转" -ForegroundColor Green
        }
        else {
            $defaultText = if ($null -ne $deployDefault) { "`"$deployDefault`"" } else { "(读不到:$DeployScriptPath 的 -ClientEntryMode 默认值不是字面常量或文件无法解析)" }
            $clusterText = if ($null -ne $State.EntryMode) { "`"$($State.EntryMode)`"" } else { '(读不到)' }
            $problems += ("客户端入口形态:未传 -ClientEntryMode 时 Step 6 按 k8s_deploy.ps1 默认值 $defaultText 重建 gate," +
                "而 gate 当前入口形态为 $clusterText($($State.EntryDetail))。两者不一致或判不出时留空可能把入口静默翻转" +
                "(external 的 zone 被重建成 podip,集群外玩家全部连不上)。" +
                "请确认后显式传 -ClientEntryMode podip 或 -ClientEntryMode external;external 须同时照传上一次部署的地址参数" +
                "(-GateServiceType、-GateNodePortBase、-ClientPublicHost / -GateClientHostTemplate、-GateExternalTrafficPolicy)。")
        }
    }
    if ($CheckIngress) {
        if (-not $State.IngressKnown) {
            $problems += ("gateway Ingress:判不出回滚前 namespace $Namespace 里有没有 Ingress gateway($($State.IngressDetail))。" +
                "Step 1 删 namespace 会连它一起删,未传 -GatewayIngressHost 时 Step 6 不会重建。请按该 zone 的部署记录照传 " +
                "-GatewayIngressHost / -GatewayTrustedProxies(及 ClassName / TlsSecret);确认该 zone 不经 Ingress 对外时加 -ConfirmNoGatewayIngress。")
        }
        elseif ($null -ne $State.Ingress) {
            $keep = "-GatewayIngressHost $($State.Ingress.Host) -GatewayTrustedProxies <上一次部署的 CIDR>"
            if ($State.Ingress.ClassName) { $keep += " -GatewayIngressClassName $($State.Ingress.ClassName)" }
            if ($State.Ingress.TlsSecret) { $keep += " -GatewayIngressTlsSecret $($State.Ingress.TlsSecret)" }
            $problems += ("gateway Ingress:$($State.IngressDetail)。Step 1 删 namespace 会连它一起删,未传 -GatewayIngressHost 时 Step 6 不会重建," +
                "集群外玩家拿不到 gate 分配。请照传 $keep;确要撤掉 Ingress,加 -ConfirmNoGatewayIngress。")
        }
        else {
            Write-Host "  $($State.IngressDetail),留空不会丢 Ingress" -ForegroundColor Green
        }
    }
    if ($problems.Count -eq 0) { return }
    throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:Step 1 会删掉 namespace $Namespace,Step 6 只按本次参数重建," +
        "留空的参数落回 k8s_deploy.ps1 默认值、不从集群读回旧值。以下 $($problems.Count) 项与集群现状不一致或判不出:`n  - " +
        ($problems -join "`n  - ") +
        "`n经包装入口调用而它不透传所需参数时,直接调用 tools/scripts/k8s_zone_rollback.ps1。")
}

<#
.SYNOPSIS
    第 0 步静态预检(-Apply):把 Step 6 将要传给 dev_tools.ps1 k8s-zone-up 的同一份参数加上 -DryRun 执行一遍,
    失败即在停服之前抛出;成功只回显其中的告警。

.DESCRIPTION
    参数组合的规则(入口形态与服务类型、模板与副本数、nodePort 段、ClientPublicHost / Ingress host / CIDR 的格式、
    -RequireClientEndpoint 与入口形态的矛盾、-LoginDevPasswordAuth 只许 dev 档、档位对应的密钥与发布门禁 …)只在
    k8s_deploy.ps1 一处(lib Test-ClientEntryPreflight 与各 Assert-*)。本脚本不复制规则,而是让那边对 Step 6 的真实参数
    完整渲染一遍 zone-up:拖到 Step 6 才被拒时 zone 已停服、数据已回档,回滚卡在半路。
    k8s_deploy.ps1 -DryRun 不调 kubectl(集群现状预检、服务端预演都跳过),只在本地渲染清单并打印,停服前执行没有副作用;
    渲染路径里的生成器缺陷(如某条分支参数写错)也会在这里暴露,而不是在 Step 6。
    失败有两种形态都认:throw(k8s_deploy.ps1 的惯例)与非 0 退出码。
    输出整段捕获:dry-run 会打印全部清单(上千行),成功时只回显告警,失败时回显末尾 -TailLines 行与原因。

.PARAMETER UpArgs
    Step 6 的参数表,原样使用(本函数只在副本上加 DryRun),保证预检的就是 Step 6 将要执行的那一份。
#>
function Invoke-ZoneUpStaticPreflight {
    param(
        [Parameter(Mandatory = $true)][string]$DevToolsPath,
        [Parameter(Mandatory = $true)][hashtable]$UpArgs,
        [int]$TailLines = 40
    )

    $dryRunArgs = $UpArgs.Clone()
    $dryRunArgs.DryRun = $true
    $captured = [System.Collections.Generic.List[object]]::new()
    $failure = $null
    # 被调脚本不以 exit 结束时 $LASTEXITCODE 保留更早的值,先清零,才能把它的非 0 判成本次失败。
    $global:LASTEXITCODE = 0
    try {
        & $DevToolsPath @dryRunArgs *>&1 | ForEach-Object { $captured.Add($_) }
        if ($LASTEXITCODE -ne 0) { $failure = "dev_tools.ps1 以退出码 $LASTEXITCODE 结束" }
    }
    catch {
        $failure = $_.Exception.Message
    }

    if ($null -eq $failure) {
        foreach ($record in @($captured | Where-Object { $_ -is [System.Management.Automation.WarningRecord] })) {
            Write-Host "  告警(k8s-zone-up -DryRun):$($record.Message)" -ForegroundColor Yellow
        }
        Write-Host "  k8s-zone-up -DryRun 通过:Step 6 的参数过了 k8s_deploy.ps1 的静态预检,清单渲染无误" -ForegroundColor Green
        return
    }

    $lines = @($captured | ForEach-Object { "$_" })
    if ($lines.Count -gt 0) {
        $shown = [Math]::Min($TailLines, $lines.Count)
        Write-Host "  k8s-zone-up -DryRun 输出末尾 $shown 行:" -ForegroundColor DarkGray
        $lines | Select-Object -Last $TailLines | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    }
    throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:Step 6 将要执行的 dev_tools.ps1 k8s-zone-up 没通过 -DryRun 静态预检" +
        "(参数组合、档位与密钥、发布门禁都在 k8s_deploy.ps1 这一步判定;拖到 Step 6 才失败时 zone 已停服、数据已回档)。" +
        "`n原因:$failure`n修正参数后重跑。")
}

# 回滚目标必须是不可变 tag,否则"回到旧版本"根本没发生。
$rollbackTag = Get-ImageTagFromRef -ImageRef $NodeImage
$rollbackChk = Test-ImmutableImageTag -Tag $rollbackTag
if (-not $rollbackChk.Ok) {
    throw "灾难回滚拒绝该 -NodeImage '$NodeImage':$($rollbackChk.Reason)。请传回滚目标版本的 git sha tag 或 repo@sha256:… digest。"
}

# 与 k8s_deploy.ps1 preflight(lib Test-ClientEntryPreflight,D91)同一规则,提前到入口,dry-run 下也能报出来。
# 其余参数组合的校验只在 k8s_deploy.ps1 一处,-Apply 由第 0 步静态预检(Invoke-ZoneUpStaticPreflight)复用,这里不复制。
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost) -and [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:给了 -GatewayIngressHost 却没有 -GatewayTrustedProxies,Step 6 会被 k8s_deploy.ps1 的 preflight 拒绝(gateway 会把 Ingress controller 的地址当成所有玩家的来源 IP,D91)。请照传上一次部署的 -GatewayTrustedProxies。"
}
# 两者是 Ingress 这一项的两种相反的显式决定,同时给说明操作者自己没想清楚,不替他挑一个。
if ($ConfirmNoGatewayIngress -and -not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) {
    throw "k8s-zone-rollback 拒绝执行:-ConfirmNoGatewayIngress 与 -GatewayIngressHost 互斥,尚未产生任何改动(前者 = 回滚后不要 Ingress,后者 = 回滚后按 $GatewayIngressHost 重建 Ingress),请按该 zone 的部署记录二选一。"
}

$verb = if ($Apply) { "APPLY" } else { "DRY-RUN" }
Write-Host "" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " k8s-zone-rollback ($verb)" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "  zone        : $ZoneName (id=$ZoneId)"
Write-Host "  target time : $TargetTime"
Write-Host "  redis       : $(if ($RedisHost) { "$RedisHost`:$RedisPort/db$RedisDB" } else { '(skipped — no -RedisHost)' })"
Write-Host "  kafka       : $KafkaBootstrap topic=$KafkaTopic group=$KafkaGroup"
# 回退态不粘滞,留空时在摘要里就提醒:操作者在 dry-run 阶段就能看出 Step 6 会以哪种路由模式拉起 gate。
Write-Host "  gate router : $(if ($GateRouterMode) { "-GateRouterMode $GateRouterMode" } else { '(未传,沿用 k8s_deploy.ps1 默认值;该 zone 若以 "0" 回退运行,必须传 -GateRouterMode 0;-Apply 时先预检,不一致或读不到即在停服前拒绝)' })"
# 集群外入口参数同样不粘滞,同样在摘要里提醒;地址参数的实际透传值见 Step 6 那一行。
Write-Host "  client entry: $(if ($ClientEntryMode) { "-ClientEntryMode $ClientEntryMode" } else { '(未传,沿用 k8s_deploy.ps1 默认值;以 external 运行的 zone 必须照传 -ClientEntryMode external 及地址参数;-Apply 时先预检)' })"
Write-Host "  ingress     : $(if ($GatewayIngressHost) { "-GatewayIngressHost $GatewayIngressHost" } elseif ($ConfirmNoGatewayIngress) { '-ConfirmNoGatewayIngress(Step 6 不生成 Ingress,namespace 里现有的 Ingress gateway 随 Step 1 删除,不核对)' } else { '(未传,Step 6 不生成 Ingress;namespace 里现有的 Ingress gateway 会随 Step 1 删除;-Apply 时先预检)' })"
Write-Host "  release     : $(if ($ReleaseProfile) { "-ReleaseProfile $ReleaseProfile" } else { '(未传,Step 6 按 dev_tools.ps1 默认 dev 档部署;staging / prod zone 必须显式传 -ReleaseProfile)' })"
Write-Host "  login auth  : $(if ($LoginDevPasswordAuth) { '-LoginDevPasswordAuth(共享口令取自环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET)' } else { '(未传 -LoginDevPasswordAuth;dev zone 依赖开发口令登录时须照传,否则 Step 6 生成的 login 没有任何认证配置)' })"
Write-Host "  switch gate : $(if ($AllowDisruptiveSwitch) { '-AllowDisruptiveSwitch(只转给 Step 6,namespace 没删净时放行换形态 / 改地址;不豁免第 0 步核对)' } else { '(未传;Step 6 通常面对空 namespace,namespace 没删净时由 k8s_deploy.ps1 按集群现状把关)' })"
Write-Host "  kube target : $(if ($KubeContext -or $KubeConfig) { "context='$KubeContext' kubeconfig='$KubeConfig'" } else { '(未指定,用 kubectl 当前 context;预检、Step 1、Step 6 同一集群)' })"
Write-Host ""

if (-not $Apply) {
    Write-Host "DRY-RUN mode. The actions below would be executed in order." -ForegroundColor Yellow
    Write-Host "Re-run with -Apply to actually perform the rollback." -ForegroundColor Yellow
    Write-Host ""
}

# ── Step 6 的参数:先于第 0 步组装,静态预检与 Step 6 用的是同一份 ──
# 必须 hashtable splatting,数组 splatting 会按位置绑定,"-Command" 这个字符串会直接撞上 dev_tools.ps1 的 ValidateSet。
$upArgs = @{
    Command         = "k8s-zone-up"
    ZoneName        = $ZoneName
    ZoneId          = $ZoneId
    NodeImage       = $NodeImage
    NamespacePrefix = $NamespacePrefix
    WaitReady       = $true
}
# 留空不传,由下游默认值接管(k8s_deploy.ps1 / dev_tools.ps1 各自唯一的一处);只有显式给了才覆盖(同 dev_tools.ps1 / k8s_image.ps1 的透传)。
# 字符串非空才传,GateNodePortBase 非 -1 才传,switch 只在被指定时传(同 dev_tools.ps1 Invoke-K8sDeploy)。
# -KubeContext / -KubeConfig 与第 0 步集群现状预检、Step 1 同一组值。
if (-not [string]::IsNullOrWhiteSpace($GateRouterMode)) { $upArgs.GateRouterMode = $GateRouterMode }
if (-not [string]::IsNullOrWhiteSpace($ReleaseProfile)) { $upArgs.ReleaseProfile = $ReleaseProfile }
if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode)) { $upArgs.ClientEntryMode = $ClientEntryMode }
if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) { $upArgs.ClientPublicHost = $ClientPublicHost }
if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate)) { $upArgs.GateClientHostTemplate = $GateClientHostTemplate }
if ($GateNodePortBase -ne -1) { $upArgs.GateNodePortBase = $GateNodePortBase }
if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $upArgs.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
if (-not [string]::IsNullOrWhiteSpace($GateServiceType)) { $upArgs.GateServiceType = $GateServiceType }
if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint)) { $upArgs.RequireClientEndpoint = $RequireClientEndpoint }
if ($LoginDevPasswordAuth) { $upArgs.LoginDevPasswordAuth = $true }
if ($AllowDisruptiveSwitch) { $upArgs.AllowDisruptiveSwitch = $true }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) { $upArgs.GatewayIngressHost = $GatewayIngressHost }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressClassName)) { $upArgs.GatewayIngressClassName = $GatewayIngressClassName }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret)) { $upArgs.GatewayIngressTlsSecret = $GatewayIngressTlsSecret }
if (-not [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) { $upArgs.GatewayTrustedProxies = $GatewayTrustedProxies }
if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $upArgs.KubeContext = $KubeContext }
if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $upArgs.KubeConfig = $KubeConfig }

# ── 预检: gate 路由模式 / 客户端入口形态 / gateway Ingress + Step 6 静态预检(只读,先于 Step 1 停服) ──
# 放在停服之前:拖到 Step 6 才发现,zone 已停服、数据已回档,回滚卡在半路;而 Step 1 删掉 namespace 之后,
# k8s_deploy.ps1 自己的集群现状预检在 Step 6 已无从判断。集群现状只核对留空的项,一次 kubectl 读完(Assert-ZoneEntryKeptOnRollback);
# 静态预检让 k8s_deploy.ps1 对 Step 6 的同一份参数 -DryRun 一遍(Invoke-ZoneUpStaticPreflight)。
# dry-run 两者都不执行(整个 dry-run 只打印命令、不调任何外部工具),只预告 -Apply 时会做什么。
Write-Host "── 预检: gate 路由模式 / 客户端入口形态 / gateway Ingress / Step 6 静态预检(只读,先于 Step 1 停服) ──" -ForegroundColor Cyan
$zoneNamespace = "${NamespacePrefix}-${ZoneName}"
$checkRouterMode = [string]::IsNullOrWhiteSpace($GateRouterMode)
$checkEntryMode = [string]::IsNullOrWhiteSpace($ClientEntryMode)
$checkIngress = [string]::IsNullOrWhiteSpace($GatewayIngressHost) -and -not $ConfirmNoGatewayIngress
if (-not $checkRouterMode) { Write-Host "  显式 -GateRouterMode ${GateRouterMode}:Step 6 以它拉起 gate,不核对集群当前值" }
if (-not $checkEntryMode) { Write-Host "  显式 -ClientEntryMode ${ClientEntryMode}:Step 6 以它重建 gate,不核对集群当前形态,地址参数以本次所传为准" }
if ($ConfirmNoGatewayIngress) { Write-Host "  显式 -ConfirmNoGatewayIngress:Step 6 不生成 Ingress,不核对集群里有没有 Ingress gateway" }
elseif (-not $checkIngress) { Write-Host "  显式 -GatewayIngressHost ${GatewayIngressHost}:Step 6 随 gateway 重建 Ingress gateway" }
if ($checkRouterMode -or $checkEntryMode -or $checkIngress) {
    $checkNames = @()
    if ($checkRouterMode) { $checkNames += 'GATE_CLIENT_RPC_ROUTER 须与 k8s_deploy.ps1 -GateRouterMode 默认值一致' }
    if ($checkEntryMode) { $checkNames += 'gate 形态须与 k8s_deploy.ps1 -ClientEntryMode 默认值一致' }
    if ($checkIngress) { $checkNames += '不能有 Ingress gateway' }
    $preflightKubectlArgs = Get-ZoneEntryKubectlArgs -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  kubectl $($preflightKubectlArgs -join ' ') → $($checkNames -join ';')"
    if ($Apply) {
        $deployScriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
        $zoneEntryState = Get-ClusterZoneEntryState -KubectlArgs $preflightKubectlArgs -Namespace $zoneNamespace
        # 显式给了 -ClientEntryMode 时不核对,但把集群现状打出来,供操作者核对照传的地址参数。
        if (-not $checkEntryMode) { Write-Host "  集群现状(仅供核对):$($zoneEntryState.EntryDetail)" }
        Assert-ZoneEntryKeptOnRollback -State $zoneEntryState `
            -DeployDefaults (Get-DeployParamDefaults -DeployScriptPath $deployScriptPath -Names @('GateRouterMode', 'ClientEntryMode')) `
            -Namespace $zoneNamespace -DeployScriptPath $deployScriptPath `
            -CheckRouterMode:$checkRouterMode -CheckEntryMode:$checkEntryMode -CheckIngress:$checkIngress
    } else {
        Write-Host "  (dry-run 不读集群;-Apply 时不一致或读不到即在停服前拒绝,须显式传对应参数)" -ForegroundColor Yellow
    }
} else {
    Write-Host "  三项均已显式给值,不读集群"
}
# 集群现状过了再做静态预检:前者只读一次 kubectl,后者要完整渲染一遍 zone-up,便宜的先拒。
Write-Host "  静态预检:以 Step 6 同一组参数(见下方 Step 6 那一行)加 -DryRun 执行 dev_tools.ps1 k8s-zone-up,由 k8s_deploy.ps1 判定参数组合、档位与密钥、发布门禁(不调 kubectl)"
if ($Apply) {
    Invoke-ZoneUpStaticPreflight -DevToolsPath (Join-Path $ScriptDir "dev_tools.ps1") -UpArgs $upArgs
} else {
    Write-Host "  (dry-run 不执行;-Apply 时非 0 即在停服前拒绝)" -ForegroundColor Yellow
}
Write-Host ""

# ── Step 1: stop the zone ────────────────────────────────────────
Write-Host "── Step 1: k8s-zone-down ──" -ForegroundColor Cyan
# hashtable splatting(理由同 Step 5);-KubeContext / -KubeConfig 非空才透传,与第 0 步预检、Step 6 落在同一集群。
$downArgs = @{
    Command         = "k8s-zone-down"
    ZoneName        = $ZoneName
    NamespacePrefix = $NamespacePrefix
}
if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $downArgs.KubeContext = $KubeContext }
if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $downArgs.KubeConfig = $KubeConfig }
Write-Host ("  dev_tools.ps1 " + (($downArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @downArgs
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-down failed" }
    Write-Host "  zone $ZoneName stopped" -ForegroundColor Green
}
Write-Host ""

# ── Step 2: drain Kafka consumer LAG ─────────────────────────────
Write-Host "── Step 2: drain Kafka consumer LAG (timeout ${KafkaDrainTimeoutSec}s) ──" -ForegroundColor Cyan
$describeCmd = "kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup"
Write-Host "  $describeCmd"
if ($Apply) {
    $deadline = (Get-Date).AddSeconds($KafkaDrainTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $out = & kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup 2>&1
        # LAG column is field 5 in default output. Sum non-zero values.
        $hasLag = $false
        foreach ($line in $out) {
            if ($line -match "^\S+\s+\S+\s+\d+\s+\d+\s+\d+\s+(\d+)") {
                if ([int]$matches[1] -gt 0) { $hasLag = $true; break }
            }
        }
        if (-not $hasLag) {
            Write-Host "  consumer LAG drained" -ForegroundColor Green
            break
        }
        Write-Host "  ... waiting for LAG to drain"
        Start-Sleep -Seconds 5
    }
    if ((Get-Date) -ge $deadline) {
        Write-Warning "consumer LAG did not drain within ${KafkaDrainTimeoutSec}s — proceeding anyway"
    }
}
Write-Host ""

# ── Step 3: MySQL PITR (manual) ──────────────────────────────────
Write-Host "── Step 3: MySQL Point-in-Time Recovery ──" -ForegroundColor Cyan
Write-Host "  ⚠️ This step is NOT automated." -ForegroundColor Yellow
Write-Host "  See docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure."
Write-Host "  Target time: $TargetTime"
Write-Host ""
Write-Host "  Typical command (verify before running):"
Write-Host "    mysqlbinlog --stop-datetime=`"$($TargetTime.Replace('T',' ').TrimEnd('Z'))`" \\"
Write-Host "      /backup/binlog/mysql-bin.* | mysql -h shadow-host -u root -p game"
Write-Host ""

if ($Apply -and -not $SkipMySqlPause) {
    Write-Host "  When MySQL PITR is complete and verified, press ENTER to continue."
    Write-Host "  Press CTRL+C to abort." -ForegroundColor Yellow
    [void](Read-Host)
}

# ── Step 4: Redis FLUSHDB ────────────────────────────────────────
Write-Host "── Step 4: Redis FLUSHDB (zone cache) ──" -ForegroundColor Cyan
if ($RedisHost -eq "") {
    Write-Host "  skipped — pass -RedisHost to enable" -ForegroundColor Yellow
} else {
    $redisArgs = @("-h", $RedisHost, "-p", $RedisPort, "-n", $RedisDB)
    if ($RedisPassword -ne "") { $redisArgs += @("-a", $RedisPassword) }
    Write-Host "  redis-cli $($redisArgs -join ' ') FLUSHDB"
    if ($Apply) {
        & redis-cli @redisArgs FLUSHDB
        if ($LASTEXITCODE -ne 0) { throw "redis FLUSHDB failed" }
        Write-Host "  Redis db $RedisDB flushed" -ForegroundColor Green
    }
}
Write-Host ""

# ── Step 5: Kafka offset reset ───────────────────────────────────
Write-Host "── Step 5: Kafka offset reset ($KafkaTopic) ──" -ForegroundColor Cyan
# 必须用 hashtable splatting。之前用的是数组 splatting —— PowerShell 会把数组
# 元素当**位置参数**依次绑定,于是 "-BootstrapServer" 这个字符串本身被绑到
# BootstrapServer、"kafka:9092" 绑到 Group、…… 最后 "-Group" 撞上 [int]$Partitions
# 直接类型转换失败。结果是这个灾难恢复脚本连 dry-run 都跑不到第 6 步。
$kArgs = @{
    BootstrapServer = $KafkaBootstrap
    Topic           = $KafkaTopic
    Group           = $KafkaGroup
    ToDatetime      = $TargetTime
}
if ($Apply) { $kArgs.Apply = $true }
Write-Host ("  kafka_offset_reset.ps1 " + (($kArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/kafka_offset_reset.ps1" @kArgs
    if ($LASTEXITCODE -ne 0) { throw "kafka_offset_reset.ps1 failed" }
}
Write-Host ""

# ── Step 6: k8s-zone-up ──────────────────────────────────────────
Write-Host "── Step 6: k8s-zone-up ──" -ForegroundColor Cyan
# $upArgs 在第 0 步之前已组装好,静态预检用的就是这一份;dry-run 打出来的这一行就是 -Apply 时真正传下去的参数。
Write-Host ("  dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @upArgs
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-up failed" }
    Write-Host "  zone $ZoneName up" -ForegroundColor Green
}
Write-Host ""

# ── Step 7: verification checklist ───────────────────────────────
Write-Host "── Step 7: post-rollback verification ──" -ForegroundColor Cyan
Write-Host "  Run these manually:"
$verifyKubeArgs = "$(if ($KubeContext) { " --context $KubeContext" })$(if ($KubeConfig) { " --kubeconfig $KubeConfig" })"
Write-Host "    1. kubectl get pods,sts,svc,ingress -n ${NamespacePrefix}-${ZoneName}$verifyKubeArgs(external zone 核对 gate-<i> 的 nodePort / Ingress host 与回滚前一致)"
Write-Host "    2. Robot smoke login: pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones $ZoneId"
Write-Host "    3. Spot-check player data:"
Write-Host "       redis-cli -h $RedisHost -p $RedisPort -n $RedisDB GET 'player:{<known-pid>}:player_database'"
Write-Host "    4. Tail scene/gate logs for replay errors (first 5 minutes)"
Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " Rollback complete. " -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
