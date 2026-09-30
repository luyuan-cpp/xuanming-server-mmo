#requires -Version 7
<#
.SYNOPSIS
    集群外客户端入口(D76–D93)部署生成器契约测试:lib/k8s_client_entry.ps1 与它在 k8s_deploy.ps1 里的接入。

.DESCRIPTION
    钉住 ingress_final WP9「契约用例」:
      - podip 默认输出与现状一致(无 CLIENT_ENDPOINT_*、无 hostPort);单一 gate-entry 只在 podip 且 gate 副本数为 1 时生成(D90);
      - external + NodePort 2 副本的完整形态(gate StatefulSet + 每序号 Service + headless + PDB,及 apply 顺序);
      - preflight 的每条报错与警告;battle 的 Agones Fleet 与 hostPort Deployment 两种形态;
      - login / scene_manager 的 RequireClientEndpoint;-LoginDevPasswordAuth 只许 dev 档、口令明文不进输出;
      - 全部输出里 gRPC 口(50000 / 48000)从不进 hostPort / nodePort / Agones ports;新生成器不出现 D92 禁用的两个身份 env;
      - Fleet 滚动就绪判据(旧 GameServerSet 的 Ready 不计);会踢人的删除未确认时的报错原文;Agones CRD 探测的三条路径;
      - gateway 管理面口令非 dev 档缺失 / 占位 / 过短在写操作之前拒绝;zones 样例(JSON / YAML)逐 zone 显式写 gateNodePortBase 且段不重叠;
      - 包装入口(dev_tools / k8s_image)splat 的每个键都是下游脚本声明过的参数(AST 契约)。

    三种跑法:
      1. 库函数 dot-source 进本进程直接测(纯函数;碰集群的函数由同名 function kubectl 顶替,函数先于外部命令解析);
      2. k8s_deploy.ps1 -DryRun 子进程,切 `--- BEGIN MANIFEST ---` 块断言(同 k8s_deploy_contract.tests.ps1);
      3. 集群现状预检只在非 DryRun 下跑:子进程里由一次性驱动脚本定义全局 function kubectl,按规则表回应并记账,
         断言"拒绝时零写操作"。安全垫:同时传一个不存在的 -KubeContext 和一份没有任何 context 的 kubeconfig,
         假 kubectl 万一没接管,真 kubectl 也只会因找不到 context 失败,碰不到本机默认集群;驱动启动前还核对
         kubectl 解析到的是函数,否则直接退出。

    负向用例一律断错误文本,不只断退出码(退出码 1 可能来自任何地方,同 k8s_deploy_contract.tests.ps1 的口径)。
    错误 / 警告文本的权威在 lib Test-ClientEntryPreflight 与 k8s_deploy.ps1,改措辞要同步改这里。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) "lib" "k8s_client_entry.ps1")

Write-Host ""
Write-Host "=== 集群外客户端入口(k8s_client_entry)部署生成器契约测试 ==="

$NodeImageRef = 'registry.invalid/test/mmorpg-node:0123456789ab'
$TestZone = 'ce'
$TestZoneNs = 'mmorpg-zone-ce'
$TestInfraNs = 'contract-ce-infra'
# battle 全局池要两把互不相同、够长的密钥(k8s_deploy_contract.tests.ps1 同口径);测试不回显它们。
$BattleTestEnv = @{
    MMORPG_BATTLE_TOKEN_SECRET = 'contract-ce-battle-ticket-0123456789abcdef'
    MMORPG_GATE_TOKEN_SECRET   = 'contract-ce-gate-ticket-0123456789abcdef01'
}
$ZoneUpArgs = @('-Command', 'zone-up', '-ZoneName', $TestZone, '-ZoneId', '101', '-NodeImage', $NodeImageRef)
$InfraUpArgs = @('-Command', 'infra-up', '-SkipGoSvc', '-SkipJavaSvc', '-InfraNamespace', $TestInfraNs, '-NodeImage', $NodeImageRef)
$GoJavaArgs = @('-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test')
# D92:整词匹配。GATE_NODE_PORT_BASE 是合法的生成器内部 env,子串匹配会误伤。
$D92Pattern = '(?<![A-Za-z0-9_])NODE_(?:IP|PORT)(?![A-Za-z0-9_])'

# ─────────────────────────────────────────────────────────────────
# 0. 采集与断言小工具
# ─────────────────────────────────────────────────────────────────

# DryRun 输出 → 逐份 YAML 文档:一个 MANIFEST 块里可能用 --- 连着多份(gate-0 / gate-1、RBAC 的 SA + RoleBinding)。
function Get-ManifestDocs {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output)
    $docs = New-Object System.Collections.Generic.List[string]
    foreach ($block in (Get-ManifestBlocks -Output $Output)) {
        foreach ($doc in ($block -split '(?m)^---[ \t]*$')) {
            if (-not [string]::IsNullOrWhiteSpace($doc)) { $docs.Add($doc.Trim([char[]]"`r`n")) }
        }
    }
    return ,$docs.ToArray()
}

# 按 kind(+ 可选 metadata.name,第 2 列)取全部文档;env / 容器里的 "- name:" 不会误中。
function Find-ManifestDocs {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [AllowEmptyString()][string]$Name = ''
    )
    # 先落到变量再逐份过滤:Get-ManifestDocs 以 ,$array 整体返回,直接接管道时 Where-Object 拿到的是整个数组。
    $docs = Get-ManifestDocs -Output $Output
    return @($docs | Where-Object {
        $_ -cmatch "(?m)^kind:[ \t]*$([regex]::Escape($Kind))[ \t]*$" -and
            ($Name -eq '' -or $_ -cmatch "(?m)^  name:[ \t]*$([regex]::Escape($Name))[ \t]*$")
    })
}

# 同一次部署里 kind/name 至多一份;找不到返回 $null,多份直接判失败(以哪份为准不该交给 apply 顺序)。
function Select-ManifestDoc {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $hits = @(Find-ManifestDocs -Output $Output -Kind $Kind -Name $Name)
    if ($hits.Count -gt 1) { throw "DryRun 输出里 $Kind/$Name 出现了 $($hits.Count) 份,同一次部署不该 apply 两遍" }
    if ($hits.Count -eq 0) { return $null }
    return $hits[0]
}

# 文档在 apply 序列里的位置(-1 = 没有),用来断言 apply 顺序。
function Get-ManifestDocIndex {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $docs = Get-ManifestDocs -Output $Output
    for ($i = 0; $i -lt $docs.Count; $i++) {
        if ($docs[$i] -cmatch "(?m)^kind:[ \t]*$([regex]::Escape($Kind))[ \t]*$" -and $docs[$i] -cmatch "(?m)^  name:[ \t]*$([regex]::Escape($Name))[ \t]*$") { return $i }
    }
    return -1
}

# Pod 模板里业务容器那一段(containers: 到 Pod 级 volumes:)。日志 sidecar 在 initContainers 里且自带 POD_NAME,
# 不能让它混进"业务容器有没有某个 env"的判断。
function Get-MainContainerSection {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Doc)
    $m = [regex]::Match([string]$Doc, '(?ms)^[ \t]*containers:[ \t]*\n(?<body>.*?)^[ \t]*volumes:[ \t]*$')
    if (-not $m.Success) { throw "文档里找不到 containers: … volumes: 段(工作负载缺席,或契约前提被改掉了)" }
    return $m.Groups['body'].Value
}

# env 字面值("- name: X" 紧跟 value: "...")。缺席返回 $null;同名多条直接判失败(以哪条为准不该交给 kubelet)。
function Get-EnvLiteral {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $pattern = "(?m)^[ \t]*- name: $([regex]::Escape($Name))[ \t]*\n[ \t]*value: `"(?<v>(?:[^`"\\]|\\.)*)`"[ \t]*$"
    $found = [regex]::Matches($Text, $pattern)
    if ($found.Count -gt 1) { throw "env $Name 出现了 $($found.Count) 次" }
    if ($found.Count -eq 0) { return $null }
    return $found[0].Groups['v'].Value
}

function Test-EnvFieldRef {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$FieldPath
    )
    $pattern = "(?m)^[ \t]*- name: $([regex]::Escape($Name))[ \t]*\n[ \t]*valueFrom:[ \t]*\n[ \t]*fieldRef:[ \t]*\n[ \t]*fieldPath: $([regex]::Escape($FieldPath))[ \t]*$"
    return ([regex]::Matches($Text, $pattern).Count -eq 1)
}

function Test-EnvDeclared {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name
    )
    return ($Text -cmatch "(?m)^[ \t]*- name: $([regex]::Escape($Name))[ \t]*$")
}

# Fleet 的 GameServer 级 ports 段(spec.template.spec.ports,第 6 列),不含 Pod 模板里容器自己的 ports。
function Get-FleetGameServerPorts {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Doc)
    $m = [regex]::Match($Doc, '(?ms)^      ports:[ \t]*\n(?<body>.*?)^      (?:health|eviction|template|counters|lists):')
    if (-not $m.Success) { return $null }
    return $m.Groups['body'].Value
}

# gRPC 口(非 gate:RpcPort + 30000 = 50000;gate 的 48000 本来就没有监听)永不进 hostPort / nodePort / Agones ports,
# 也不开 hostNetwork(D81)。扫一次 DryRun 输出里的全部清单。
function Assert-GrpcNeverExposed {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $docs = Get-ManifestDocs -Output $Output
    Assert-True -Condition ($docs.Count -gt 0) -Because "${Label}:输出里一份清单都没有,暴露检查会空转通过"
    $all = $docs -join "`n---`n"
    foreach ($m in [regex]::Matches($all, '(?m)^[ \t]*(?:- )?(?<k>hostPort|nodePort):[ \t]*(?<p>\d+)')) {
        Assert-True -Condition ($m.Groups['p'].Value -notin @('50000', '48000')) -Because "${Label}:$($m.Groups['k'].Value): $($m.Groups['p'].Value) 是 gRPC 口,永不对集群外暴露(D81)"
    }
    Assert-NotMatch -Text $all -Pattern 'hostNetwork' -Because "${Label}:禁止 hostNetwork(D81)"
    foreach ($fleet in @(Find-ManifestDocs -Output $Output -Kind 'Fleet')) {
        $ports = Get-FleetGameServerPorts -Doc $fleet
        if ($null -eq $ports) { continue }
        Assert-NotMatch -Text $ports -Pattern '\b(?:50000|48000)\b' -Because "${Label}:Agones ports 只列客户端口,gRPC 不进 Agones 的端口分配(D81)"
    }
}

# DryRun 的参数组合校验必须在任何 kubectl(含删除旧形态)之前拒绝,且错误文本点出原因。
function Assert-RefusedBeforeAnyKubectl {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string[]]$Patterns,
        [Parameter(Mandatory = $true)][string]$Label
    )
    Assert-True -Condition ($Run.ExitCode -ne 0) -Because "${Label}:必须拒绝。输出: $($Run.Output)"
    foreach ($pattern in $Patterns) {
        Assert-Match -Text $Run.Output -Pattern $pattern -Because "${Label}:错误文本要点出原因,不能把别的执行错误误判为校验生效"
    }
    Assert-NotMatch -Text $Run.Output -Pattern '\[dry-run\] kubectl' -Because "${Label}:校验必须早于任何 kubectl,不留半截部署"
}

# lib 内部调用的前提:本库函数的必填参数全部具名传入,且没有被解析成裸字符串的 `-Xxx'…'`(少一个空格就是位置参数)。
# 违反时 podip 的 zone-up / all-up 会在删旧形态那一步停在"缺必填参数"的交互提示上,把整套测试挂死 —— 所以先按语法树判,
# 不满足就不启动会走到那里的子进程,直接记失败。返回缺陷描述数组,空 = 通过。
function Get-LibCallDefects {
    $path = Join-Path (Get-ToolsScriptsDir) "lib" "k8s_client_entry.ps1"
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { return ,@("lib 解析失败:$($parseErrors[0].Message)") }
    $mandatoryByFunction = @{}
    foreach ($fn in $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)) {
        $mandatory = @()
        if ($null -ne $fn.Body.ParamBlock) {
            foreach ($p in $fn.Body.ParamBlock.Parameters) {
                foreach ($attr in $p.Attributes) {
                    if ($attr -isnot [System.Management.Automation.Language.AttributeAst] -or $attr.TypeName.Name -ne 'Parameter') { continue }
                    foreach ($na in $attr.NamedArguments) {
                        if ($na.ArgumentName -eq 'Mandatory' -and $na.Argument.Extent.Text -eq '$true') { $mandatory += $p.Name.VariablePath.UserPath }
                    }
                }
            }
        }
        $mandatoryByFunction[$fn.Name] = $mandatory
    }
    $defects = @()
    foreach ($cmd in $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true)) {
        $name = $cmd.GetCommandName()
        if (-not $name -or -not $mandatoryByFunction.ContainsKey($name)) { continue }
        $elements = @($cmd.CommandElements | Select-Object -Skip 1)
        $bareDash = @($elements | Where-Object { $_ -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $_.StringConstantType -eq 'BareWord' -and $_.Value.StartsWith('-') })
        $splatted = @($elements | Where-Object { $_ -is [System.Management.Automation.Language.VariableExpressionAst] -and $_.Splatted }).Count -gt 0
        $named = @($elements | Where-Object { $_ -is [System.Management.Automation.Language.CommandParameterAst] } | ForEach-Object { $_.ParameterName })
        $missing = if ($splatted) { @() } else { @($mandatoryByFunction[$name] | Where-Object { $_ -notin $named }) }
        if ($bareDash.Count -gt 0 -or $missing.Count -gt 0) {
            $defects += "k8s_client_entry.ps1:$($cmd.Extent.StartLineNumber) 调 $name 缺具名必填参数 [$($missing -join ', ')],裸 '-' 实参 $($bareDash.Count) 个"
        }
    }
    return ,$defects
}
$LibCallDefects = Get-LibCallDefects

# 以 -DryRun 跑 k8s_deploy.ps1。能走到 Apply-Zone 的 podip zone-up / all-up 会进 Get-GateObsoleteResources 的 podip 分支:
# lib 前提不满足时不启动子进程(见 Get-LibCallDefects),返回一个必然被断言打红的结果。
# -RefusedAtEntry:调用方声明这组参数在写路径入口就被拒(参数绑定 ValidateSet、Assert-ClientEntryDeployPreflight、
# Initialize-InjectedSecrets,都在 k8s_deploy.ps1 的 switch ($Command) 之前),到不了 Apply-Zone,照常起子进程 ——
# 负向用例的红绿只由它自己针对的那道闸决定,不陪 lib 前提一起红。只给断言"在任何 kubectl 之前拒绝"的负向用例用。
# lib 前提未修时入口闸若再回归失守,子进程也不会挂住:lib/deploy_capture.ps1 的 Invoke-CapturedPwsh 以 -NonInteractive
# 启动子进程,缺必填参数直接报错退出,该用例带着缺参报错变红。
function Invoke-EntryDryRun {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{},
        [switch]$RefusedAtEntry
    )
    $allArgs = @($Arguments) + @('-DryRun')
    $reachesPodipGate = (($allArgs -contains 'zone-up') -or ($allArgs -contains 'all-up')) -and -not ($allArgs -contains 'external')
    if ($reachesPodipGate -and -not $RefusedAtEntry -and $LibCallDefects.Count -gt 0) {
        return @{ ExitCode = -1; Output = "未启动子进程(会挂在缺必填参数的交互提示上):$($LibCallDefects -join '; ')" }
    }
    return Invoke-DeployDryRun -Arguments $allArgs -Env $Env
}

# 库生成器的公共容器参数(关 sidecar:生成器单测只看本库渲染的部分)。
function New-TestPodCommon {
    return New-ClientEntryPodCommon -Image $NodeImageRef -ImagePullPolicy 'IfNotPresent' -SnowflakeCacheDir '/app/snowflake-cache' -LogVolumeSizeLimit '2Gi'
}

# 进程内调用的 lib 生成器,返回值一律先经这里再断言。lib 按 .gitattributes「*.ps1 eol=crlf」签出(Linux CI 同样),
# here-string 拼的 YAML 行尾因此是 CRLF,又与 -join "`n" 拼的片段混在一起;(?m)^…$ 与 [ \t]*\n 都认不得 \r,
# 不归一就会整批误红,甚至让"找不到就放过"的扫描空转。换行形态不是契约(YAML 解析器本来就把 CRLF 归一成 LF)。
# DryRun 路径已由 Get-ManifestBlocks 按行重拼成 LF,不经这里。
function ConvertTo-LfText {
    param([AllowEmptyString()][AllowNull()][string]$Text)
    return ([string]$Text) -replace "`r`n", "`n"
}

# ─────────────────────────────────────────────────────────────────
# 1. 库:前提、规则与地址计划(纯函数,进程内)
# ─────────────────────────────────────────────────────────────────

Test-Case "前提:lib 内部调用的必填参数全部具名传入,没有被解析成位置参数的 -Xxx'…' 写法(否则 podip 路径挂在交互提示上)" {
    # 历史缺陷:Get-GateObsoleteResources 里 `-Reason'gate 从 StatefulSet…'` 少一个空格,整段被当成位置参数绑到 -Name,
    # 必填的 -Reason 缺失 —— 默认 podip 的 zone-up 在删旧形态那一步报错或停在交互提示上。
    Assert-Equal -Expected '' -Actual ($LibCallDefects -join '; ') -Because '本库自己调用自己的函数时,必填参数必须全部具名传入'
}

Test-Case '库:D90 单一 gate-entry 只在 podip 且 gate 副本数恰为 1 时需要;RequireClientEndpoint 的 auto 取值跟随模式' {
    Assert-True -Condition (Test-GateEntryServiceWanted -ClientEntryMode podip -GateReplicas 1) -Because 'podip 单副本:gate-entry 是唯一入口'
    foreach ($case in @(@{ Mode = 'podip'; Replicas = 2 }, @{ Mode = 'podip'; Replicas = 0 }, @{ Mode = 'external'; Replicas = 1 }, @{ Mode = 'external'; Replicas = 2 })) {
        Assert-True -Condition (-not (Test-GateEntryServiceWanted -ClientEntryMode $case.Mode -GateReplicas $case.Replicas)) -Because "$($case.Mode) / $($case.Replicas) 副本不生成 gate-entry:≥2 副本时约一半票据被 token_gate_node_mismatch 拒绝;external 由每序号 Service 取代"
    }
    Assert-Equal -Expected 'true' -Actual (Get-ClientEntryRequireClientEndpoint -ClientEntryMode external) -Because 'external:缺自报地址的 gate 不下发'
    Assert-Equal -Expected 'false' -Actual (Get-ClientEntryRequireClientEndpoint -ClientEntryMode podip) -Because 'podip 的 gate 从不自报地址,取 true 会跳过全部 gate'
}

Test-Case '库:NodePort 地址计划 = base + 序号,gate 自报端口与 Service 暴露端口同源;枚举取值规范成 K8s 大小写' {
    $plan = Resolve-GateClientEndpointPlan -ServiceType 'nodeport' -Replicas 2 -ServicePort 18000 -NodePortBase 30000 -ClientPublicHost '127.0.0.1' -ZoneName 'z1'
    Assert-True -Condition ($plan.ServiceType -ceq 'NodePort') -Because "ValidateSet 不分大小写,原样写进 type: 会被 API server 拒收(实际 '$($plan.ServiceType)')"
    Assert-Equal -Expected 'nodeport' -Actual $plan.PortMode -Because 'NodePort 形态由 shell 按序号算端口'
    Assert-Equal -Expected 2 -Actual @($plan.Ordinals).Count -Because '序号个数 = 副本数'
    foreach ($i in 0, 1) {
        $o = $plan.Ordinals[$i]
        Assert-Equal -Expected (30000 + $i) -Actual $o.NodePort -Because "gate-$i 的 nodePort = base + 序号(D88)"
        Assert-Equal -Expected $o.NodePort -Actual $o.ClientPort -Because '自报端口必须就是 Service 的 nodePort,否则票据指向别的 gate'
        Assert-Equal -Expected "gate-$i" -Actual $o.ServiceName -Because '每序号 Service 与 Pod 同名(StatefulSet gate 的 Pod 名 gate-<i>)'
        Assert-Equal -Expected '127.0.0.1' -Actual $o.ClientHost -Because '没有模板时取 -ClientPublicHost'
        Assert-Equal -Expected '' -Actual $o.ExternalDnsHostname -Because 'NodePort 不需要 external-dns 注解'
    }
    Assert-Equal -Expected 0 -Actual $plan.ClientPort -Because 'nodeport 模式下端口由 shell 按序号算,计划级 ClientPort 不用'
}

Test-Case '库:LoadBalancer 地址计划:模板 {zone} 生成期渲染、{ordinal} 逐序号渲染,端口 = Service 端口,模板优先于 -ClientPublicHost' {
    $plan = Resolve-GateClientEndpointPlan -ServiceType LoadBalancer -Replicas 2 -ServicePort 18000 -ClientHostTemplate 'gate-{ordinal}.{zone}.example.com' -ClientPublicHost '1.2.3.4' -ZoneName 'z1'
    Assert-Equal -Expected 'service' -Actual $plan.PortMode -Because 'LB 形态端口固定为 Service 端口'
    Assert-Equal -Expected 18000 -Actual $plan.ClientPort -Because 'LB 端口 = 18000(D88)'
    Assert-Equal -Expected 'gate-{ordinal}.z1.example.com' -Actual $plan.HostTemplate -Because '{zone} 在生成期渲染,{ordinal} 留给启动 shell'
    Assert-Equal -Expected 'gate-1.z1.example.com' -Actual $plan.Ordinals[1].ClientHost -Because '主机优先级:模板 > -ClientPublicHost(D88)'
    Assert-Equal -Expected 'gate-1.z1.example.com' -Actual $plan.Ordinals[1].ExternalDnsHostname -Because 'LB 地址靠 external-dns 把渲染后的名字指过去'
    Assert-Equal -Expected 0 -Actual $plan.Ordinals[1].NodePort -Because 'LB 不写 nodePort'
}

Test-Case '负向 库:地址计划拒绝 {zone} 缺 zone 名、NodePort 缺 base' {
    $cases = @(
        @{ Pattern = '含 \{zone\}'; Block = { Resolve-GateClientEndpointPlan -ServiceType LoadBalancer -Replicas 1 -ServicePort 18000 -ClientHostTemplate 'gate-{ordinal}.{zone}.example.com' } },
        @{ Pattern = '必须给 -NodePortBase'; Block = { Resolve-GateClientEndpointPlan -ServiceType NodePort -Replicas 1 -ServicePort 18000 } }
    )
    foreach ($case in $cases) {
        $err = $null
        try { & $case.Block | Out-Null } catch { $err = $_.Exception.Message }
        Assert-Match -Text $err -Pattern $case.Pattern -Because '算不出正确地址时必须 throw,不能带着错地址渲染清单'
    }
}

Test-Case '库:battle Fleet health 抬到能覆盖启动最坏耗时的下限(30/10/3 → 105),够大的值原样保留,心跳间隔以下直接拒绝' {
    $raised = Resolve-BattleFleetHealth -InitialDelaySeconds 30 -PeriodSeconds 10 -FailureThreshold 3
    Assert-Equal -Expected 135 -Actual $raised.StartupWorstSeconds -Because '推导:Agones Fetch 60 + 初始化 20 + Ready 重试 53 + 首个 healthInterval 2'
    Assert-Equal -Expected 105 -Actual $raised.InitialDelaySeconds -Because 'initialDelaySeconds ≥ 135 − 10×3'
    Assert-True -Condition $raised.Raised -Because '抬高必须让调用方知道(k8s_deploy 打 NOTE),不静默改写运维给的值'
    $kept = Resolve-BattleFleetHealth -InitialDelaySeconds 200 -PeriodSeconds 10 -FailureThreshold 3
    Assert-Equal -Expected 200 -Actual $kept.InitialDelaySeconds -Because '传入值已够,原样使用'
    Assert-True -Condition (-not $kept.Raised) -Because '没抬就不该报抬高'
    foreach ($bad in @(@{ Period = 2; Failure = 3; Pattern = '必须大于' }, @{ Period = 10; Failure = 0; Pattern = 'failureThreshold=0' })) {
        $err = $null
        try { Resolve-BattleFleetHealth -InitialDelaySeconds 30 -PeriodSeconds $bad.Period -FailureThreshold $bad.Failure | Out-Null } catch { $err = $_.Exception.Message }
        Assert-Match -Text $err -Pattern $bad.Pattern -Because 'periodSeconds ≤ C++ healthInterval(2s)时正常心跳也会被判失败,必须拒绝'
    }
}

Test-Case '库:D92 —— 本库源码里不出现两个集群内身份 env 的名字(整词),任何新生成器都不可能写出它们' {
    $source = Get-Content -LiteralPath (Join-Path (Get-ToolsScriptsDir) "lib" "k8s_client_entry.ps1") -Raw -Encoding utf8
    Assert-NotMatch -Text $source -Pattern $D92Pattern -Because '它们是 ResolveNodeIp / TryResolveNodePortFromEnv 的内部身份输入,客户端地址只许用 CLIENT_ENDPOINT_* / HOST_IP / POD_NAME(D92)'
}

Test-Case '库:模式切换清单 —— 会踢人的条目带 CurrentShape / KeepHint,不踢人的残留清理不受闸约束' {
    # 这几个库函数以 ,$items 整体返回:直接赋值拿到数组;外面再包 @() 会变成"只有一个元素(整个数组)"。
    $external = Get-GateObsoleteResources -ClientEntryMode external -GateReplicas 2
    Assert-Equal -Expected 'deployment gate|service gate-entry' -Actual (($external | ForEach-Object { "$($_.Kind) $($_.Name)" }) -join '|') -Because 'external:删 Deployment 形态的 gate 与 D90 不再生成的 gate-entry'
    Assert-True -Condition ($external[0].KicksPlayers -and $external[0].KeepHint -eq '-ClientEntryMode podip') -Because '删 gate Deployment = 整台踢人,报错要告诉操作者保持现状传什么'
    Assert-True -Condition (-not $external[1].KicksPlayers) -Because 'gate-entry 残留删了不踢人'
    $toFleet = Get-BattleObsoleteResources -BattleOrchestrator agones
    Assert-True -Condition ($toFleet.Count -eq 1 -and $toFleet[0].Kind -eq 'deployment' -and $toFleet[0].KicksPlayers) -Because 'battle 切 Fleet:删 Deployment 形态,在打的局作废'
    $toDeployment = Get-BattleObsoleteResources -BattleOrchestrator deployment
    Assert-True -Condition ($toDeployment.Count -eq 1 -and $toDeployment[0].Kind -eq 'fleets.agones.dev' -and $toDeployment[0].ApiGroup -eq 'agones.dev') -Because '切回 Deployment 删 Fleet;集群没装 Agones 时按 ApiGroup 跳过'
    if ($LibCallDefects.Count -gt 0) { throw "podip 分支未执行:$($LibCallDefects -join '; ')" }
    foreach ($case in @(@{ Replicas = 2; Count = 5; Entry = $true }, @{ Replicas = 1; Count = 4; Entry = $false })) {
        $podip = Get-GateObsoleteResources -ClientEntryMode podip -GateReplicas $case.Replicas
        Assert-Equal -Expected $case.Count -Actual $podip.Count -Because "podip / $($case.Replicas) 副本:StatefulSet gate、每序号 Service、headless、PDB$(if ($case.Entry) { '、gate-entry' })"
        Assert-True -Condition ($podip[0].Kind -eq 'statefulset' -and $podip[0].KicksPlayers -and $podip[0].KeepHint -eq '-ClientEntryMode external') -Because '切回 podip 删 StatefulSet gate = 整台踢人'
        Assert-True -Condition (@($podip | Where-Object { [string]::IsNullOrWhiteSpace($_.Reason) -or $_.Name -like '-*' }).Count -eq 0) -Because '每条都要有删除原因,名字不能是被错绑的参数文本'
        Assert-Equal -Expected $case.Entry -Actual (@($podip | Where-Object { $_.Name -eq 'gate-entry' }).Count -eq 1) -Because 'D90:只有副本数 ≠1 时才删 gate-entry'
    }
}

# ─────────────────────────────────────────────────────────────────
# 1b. 库:gate(external)生成器 —— StatefulSet + 每序号 Service + headless + PDB(D87 / D88 / D89)
# ─────────────────────────────────────────────────────────────────

$LibPodCommon = New-TestPodCommon
$LibNodePortPlan = Resolve-GateClientEndpointPlan -ServiceType NodePort -Replicas 2 -ServicePort 18000 -NodePortBase 30000 -ClientPublicHost '127.0.0.1' -ZoneName 'z1'
$LibLbPlan = Resolve-GateClientEndpointPlan -ServiceType LoadBalancer -Replicas 2 -ServicePort 18000 -ClientHostTemplate 'gate-{ordinal}.{zone}.example.com' -ZoneName 'z1'
$LibGateSts = ConvertTo-LfText (New-GateStatefulSetYaml -PodCommon $LibPodCommon -Plan $LibNodePortPlan -RpcPort 18000 -StartCommand './gate' -ConfigMapName 'node-config' -GateRouterMode '1')

Test-Case '库:gate StatefulSet 的形态 —— Parallel + OnDelete + governing headless,副本数取自地址计划' {
    Assert-Match -Text $LibGateSts -Pattern '(?m)^kind: StatefulSet$' -Because 'external 的 gate 是 StatefulSet(D87)'
    Assert-Match -Text $LibGateSts -Pattern '(?m)^  serviceName: gate-headless$' -Because 'StatefulSet 必须指向 governing headless Service'
    Assert-Match -Text $LibGateSts -Pattern '(?m)^  replicas: 2$' -Because '副本数与每序号 Service 同源'
    Assert-Match -Text $LibGateSts -Pattern '(?m)^  podManagementPolicy: Parallel$' -Because '序号只用来算地址,不需要按序启动'
    Assert-Match -Text $LibGateSts -Pattern 'updateStrategy:\s+type: OnDelete' -Because 'gate 有长连接,滚动一律走 k8s_gate_drain.ps1 排空后删 Pod'
    Assert-Match -Text $LibGateSts -Pattern 'readinessProbe:\s+tcpSocket:\s+port: 18000' -Because '就绪探针同 Deployment 版:只探唯一的监听口 18000'
    $main = Get-MainContainerSection -Doc $LibGateSts
    Assert-Equal -Expected '18000' -Actual (([regex]::Matches($main, 'containerPort:\s*(\d+)') | ForEach-Object { $_.Groups[1].Value }) -join ',') -Because 'gate 容器只声明 18000,gRPC 口 48000 上本来就没有监听'
    Assert-NotMatch -Text $LibGateSts -Pattern 'hostPort|hostNetwork' -Because 'gate 经每序号 Service 暴露,不占节点端口'
}

Test-Case '库:gate StatefulSet 的 env —— static 来源 + REQUIRED=1 + 生成器内部 env;HOST / PORT 只由启动 shell 算出,不写成 env' {
    $main = Get-MainContainerSection -Doc $LibGateSts
    $literals = [ordered]@{
        RPC_PORT = '18000'; GATE_CLIENT_RPC_ROUTER = '1'; CLIENT_ENDPOINT_SOURCE = 'static'; CLIENT_ENDPOINT_REQUIRED = '1'
        GATE_CLIENT_HOST_TEMPLATE = ''; CLIENT_PUBLIC_HOST = '127.0.0.1'; GATE_CLIENT_PORT_MODE = 'nodeport'; GATE_NODE_PORT_BASE = '30000'
    }
    foreach ($name in $literals.Keys) {
        Assert-Equal -Expected $literals[$name] -Actual (Get-EnvLiteral -Text $main -Name $name) -Because "gate 容器 env $name(ingress_final WP9 生成物形态)"
    }
    foreach ($ref in @(@('POD_IP', 'status.podIP'), @('POD_NAME', 'metadata.name'), @('HOST_IP', 'status.hostIP'))) {
        Assert-True -Condition (Test-EnvFieldRef -Text $main -Name $ref[0] -FieldPath $ref[1]) -Because "$($ref[0]) 必须是 downward API $($ref[1])"
    }
    foreach ($name in @('CLIENT_ENDPOINT_HOST', 'CLIENT_ENDPOINT_PORT', 'GATE_CLIENT_PORT')) {
        Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name $name)) -Because "$name 不写成 env:HOST / PORT 由启动 shell 按序号算出并 export;nodeport 模式不用 GATE_CLIENT_PORT"
    }
    Assert-NotMatch -Text $LibGateSts -Pattern $D92Pattern -Because 'D92:新生成器不复用集群内身份 env'
}

Test-Case '库:gate 启动 shell 先按序号算出客户端地址再启动进程,且不触发 kubelet 的 $(VAR) 展开' {
    $prefixLines = @((Get-GateClientEndpointShellPrefix).TrimEnd() -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    Assert-True -Condition ($prefixLines.Count -ge 5) -Because '地址片段至少包含序号、主机、端口、export 几步'
    foreach ($line in $prefixLines) {
        Assert-True -Condition ($LibGateSts.Contains($line)) -Because "启动 args 里必须原样带上地址片段:$line"
    }
    Assert-Match -Text $LibGateSts -Pattern ([regex]::Escape('CLIENT_ENDPOINT_PORT=$((GATE_NODE_PORT_BASE + ORD))')) -Because 'NodePort 端口 = base + 序号,与每序号 Service 的 nodePort 同一公式'
    $exportAt = $LibGateSts.IndexOf('export CLIENT_ENDPOINT_HOST CLIENT_ENDPOINT_PORT')
    $startAt = $LibGateSts.IndexOf('mkdir -p /app/bin/logs/cpp_nodes && ./gate')
    Assert-True -Condition ($exportAt -ge 0 -and $startAt -gt $exportAt) -Because '先 export 地址,再建日志目录并启动 gate(C++ 在构造期读 CLIENT_ENDPOINT_*)'
    Assert-NotMatch -Text $LibGateSts -Pattern '\$\$' -Because 'kubelet 会把 $$ 折成 $,片段里不得出现'
    $declared = @([regex]::Matches($LibGateSts, '(?m)^[ \t]*- name: ([A-Z_][A-Z0-9_]*)[ \t]*$') | ForEach-Object { $_.Groups[1].Value })
    foreach ($m in [regex]::Matches($LibGateSts, '\$\(([^)]*)\)')) {
        Assert-True -Condition ($m.Groups[1].Value -cnotin $declared) -Because "`$($($m.Groups[1].Value)) 恰好是容器 env 名,kubelet 会先展开它,shell 里的命令替换就被改写了"
    }
}

Test-Case '库:每序号 Service gate-<i>(NodePort)—— 只选同序号 Pod、nodePort = base + i、externalTrafficPolicy 默认 Local' {
    $docs = @((ConvertTo-LfText (New-GateOrdinalServicesYaml -Plan $LibNodePortPlan)) -split '(?m)^---[ \t]*$' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    Assert-Equal -Expected 2 -Actual $docs.Count -Because '一个序号一份 Service'
    for ($i = 0; $i -lt 2; $i++) {
        $doc = $docs[$i]
        Assert-Match -Text $doc -Pattern "(?m)^  name: gate-$i$" -Because 'Service 与 Pod 同名'
        Assert-Match -Text $doc -Pattern "mmorpg\.io/gate-ordinal: `"$i`"" -Because '序号标签值加引号(纯数字不加引号会被 API server 拒)'
        Assert-Match -Text $doc -Pattern '(?m)^  type: NodePort$' -Because 'external + NodePort'
        Assert-Match -Text $doc -Pattern '(?m)^  externalTrafficPolicy: Local$' -Because 'Local 保留玩家真实源 IP(D89)'
        Assert-Match -Text $doc -Pattern "selector:\s+app: gate\s+statefulset\.kubernetes\.io/pod-name: gate-$i\s" -Because '只命中同序号的 Pod:自报的 nodePort 一定落到自己身上,票据才不会被 token_gate_node_mismatch 拒绝'
        Assert-Match -Text $doc -Pattern "port: 18000\s+targetPort: rpc\s+nodePort: $(30000 + $i)\s*$" -Because 'nodePort = base + 序号(D88)'
        Assert-NotMatch -Text $doc -Pattern 'annotations:' -Because 'NodePort 形态没有 external-dns 注解'
    }
    $cluster = ConvertTo-LfText (New-GateOrdinalServicesYaml -Plan $LibNodePortPlan -ExternalTrafficPolicy 'cluster')
    Assert-Match -Text $cluster -Pattern '(?m)^  externalTrafficPolicy: Cluster$' -Because '写进 YAML 的枚举必须是 K8s 标准大小写'
    $none = Resolve-GateClientEndpointPlan -ServiceType NodePort -Replicas 0 -ServicePort 18000 -NodePortBase 30000
    Assert-Equal -Expected '' -Actual (New-GateOrdinalServicesYaml -Plan $none) -Because '0 副本不生成 Service(调用方跳过 apply)'
}

Test-Case '库:每序号 Service(LoadBalancer)不写 nodePort,配模板时带 external-dns 注解;headless 与 PDB 的形态' {
    $docs = @((ConvertTo-LfText (New-GateOrdinalServicesYaml -Plan $LibLbPlan)) -split '(?m)^---[ \t]*$' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    Assert-Equal -Expected 2 -Actual $docs.Count -Because '一个序号一份 Service'
    Assert-Match -Text $docs[1] -Pattern '(?m)^  type: LoadBalancer$' -Because 'LB 形态'
    Assert-Match -Text $docs[1] -Pattern 'external-dns\.alpha\.kubernetes\.io/hostname: "gate-1\.z1\.example\.com"' -Because 'LB 地址由云厂商分配,只能靠 DNS 模板 + external-dns'
    Assert-NotMatch -Text ($docs -join "`n") -Pattern 'nodePort:' -Because 'LB 的 nodePort 由集群分配,客户端不用它'
    $headless = ConvertTo-LfText (New-GateHeadlessServiceYaml -ServicePort 18000)
    Assert-Match -Text $headless -Pattern '(?m)^  name: gate-headless$' -Because 'headless 名 gate-headless(ingress_final §3)'
    Assert-Match -Text $headless -Pattern '(?m)^  clusterIP: None$' -Because 'governing Service 必须是 headless'
    Assert-NotMatch -Text $headless -Pattern 'type:|nodePort' -Because 'headless 不对外暴露'
    $pdb = ConvertTo-LfText (New-GatePdbYaml)
    Assert-Match -Text $pdb -Pattern '(?m)^apiVersion: policy/v1$' -Because 'PDB 用 policy/v1'
    Assert-Match -Text $pdb -Pattern '(?m)^  name: gate$' -Because 'PDB 名 gate'
    Assert-Match -Text $pdb -Pattern '(?m)^  maxUnavailable: 0$' -Because '驱逐 = 整台 gate 踢人,维护前先排空(D87)'
}

# ─────────────────────────────────────────────────────────────────
# 1c. 库:battle 两种形态(D81–D86)、gateway Ingress 与限流片段(D91)、login 开发口令片段(2b §7)
# ─────────────────────────────────────────────────────────────────

$LibFleetHealth = Resolve-BattleFleetHealth -InitialDelaySeconds 30 -PeriodSeconds 10 -FailureThreshold 3
function New-LibBattleFleet {
    param([Parameter(Mandatory = $true)][string]$Mode, [AllowEmptyString()][string]$PublicHost = '')
    return ConvertTo-LfText (New-BattleFleetYaml -PodCommon $LibPodCommon -Replicas 2 -RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config' `
        -ClientEntryMode $Mode -Health $LibFleetHealth -BuildLabel '0123456789ab' -ClientPublicHost $PublicHost)
}

Test-Case '库:battle Fleet(external)—— 只有 client 一个 Dynamic 端口,gRPC 只在容器 ports,agones 来源 + REQUIRED,永不写 PORT' {
    $fleet = New-LibBattleFleet -Mode external -PublicHost '127.0.0.1'
    Assert-Match -Text $fleet -Pattern '(?m)^apiVersion: agones\.dev/v1$' -Because 'battle 用 Agones Fleet(D81)'
    Assert-Match -Text $fleet -Pattern '(?m)^  scheduling: Packed$' -Because '装箱调度,缩容时先腾空的节点可回收'
    Assert-Match -Text $fleet -Pattern 'allocationOverflow:\s+labels:\s+mmorpg\.io/drain: "true"' -Because '滚动 / 缩容时给旧的 Allocated 实例打排空标签(D83),键名是跨语言契约'
    Assert-Match -Text $fleet -Pattern 'eviction:\s+safe: Never' -Because '在打的房间不被驱逐(D83)'
    Assert-Match -Text $fleet -Pattern 'health:\s+disabled: false\s+initialDelaySeconds: 105\s+periodSeconds: 10\s+failureThreshold: 3' -Because 'health 取 Resolve-BattleFleetHealth 的结果(2c 补充第 1 条)'
    $ports = Get-FleetGameServerPorts -Doc $fleet
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($ports, '(?m)^\s*- name:').Count) -Because 'Agones ports 只列客户端口一项'
    Assert-Match -Text $ports -Pattern '- name: client\s+portPolicy: Dynamic\s+containerPort: 20000\s+protocol: TCP' -Because '端口名 client 是 C++ agones::kClientPortName 的契约;Dynamic 由 Agones 分配 hostPort(D81)'
    Assert-Match -Text $fleet -Pattern '(?m)^          serviceAccountName: agones-sdk$' -Because 'SDK sidecar 需要 agones-sdk 的 RBAC'
    $main = Get-MainContainerSection -Doc $fleet
    Assert-Equal -Expected '50000' -Actual (([regex]::Matches($main, 'containerPort:\s*(\d+)') | ForEach-Object { $_.Groups[1].Value }) -join ',') -Because '容器只声明 gRPC:20000 由 Agones 注入,重复声明会触发端口重复校验'
    foreach ($pair in @(@('AGONES_ENABLED', '1'), @('CLIENT_ENDPOINT_SOURCE', 'agones'), @('CLIENT_ENDPOINT_HOST', '127.0.0.1'), @('CLIENT_ENDPOINT_REQUIRED', '1'), @('RPC_PORT', '20000'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-EnvLiteral -Text $main -Name $pair[0]) -Because "battle Fleet env $($pair[0])"
    }
    Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name 'CLIENT_ENDPOINT_PORT')) -Because 'agones 来源下设置 CLIENT_ENDPOINT_PORT 即致命(端口来自 Agones status)'
    Assert-NotMatch -Text $fleet -Pattern 'livenessProbe|readinessProbe|startupProbe|hostPort|hostNetwork' -Because 'GameServer Pod 是 restartPolicy: Never,就绪交给 SDK;端口交给 Agones'
    Assert-NotMatch -Text $fleet -Pattern $D92Pattern -Because 'D92'
    $noHost = Get-MainContainerSection -Doc (New-LibBattleFleet -Mode external)
    Assert-True -Condition (-not (Test-EnvDeclared -Text $noHost -Name 'CLIENT_ENDPOINT_HOST')) -Because '没给 -ClientPublicHost 就取 Agones status.address,不写空的覆盖'
}

Test-Case '库:battle Fleet(podip)只写 CLIENT_ENDPOINT_SOURCE=none,HOST / PORT / REQUIRED 一个都不写' {
    $main = Get-MainContainerSection -Doc (New-LibBattleFleet -Mode podip -PublicHost '127.0.0.1')
    Assert-Equal -Expected 'none' -Actual (Get-EnvLiteral -Text $main -Name 'CLIENT_ENDPOINT_SOURCE') -Because 'podip 客户端直接拿 PodIP'
    foreach ($name in @('CLIENT_ENDPOINT_HOST', 'CLIENT_ENDPOINT_PORT', 'CLIENT_ENDPOINT_REQUIRED')) {
        Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name $name)) -Because "SOURCE=none 时带 $name 会打常态 WARN(2b §3);none + REQUIRED=1 是致命矛盾"
    }
}

Test-Case '库:battle hostPort Deployment —— 只有客户端口 20000 占 hostPort,maxSurge 0,HOST 依赖 HOST_IP 展开' {
    $hp = ConvertTo-LfText (New-BattleHostPortDeploymentYaml -PodCommon $LibPodCommon -Replicas 1 -RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config')
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($hp, 'hostPort:').Count) -Because '每个节点只占一个 hostPort'
    Assert-Match -Text $hp -Pattern 'containerPort: 20000\s+hostPort: 20000\s+name: rpc' -Because '客户端口 20000 直接占节点端口(D86)'
    Assert-Match -Text $hp -Pattern 'containerPort: 50000\s+name: grpc' -Because 'gRPC 只写 containerPort'
    Assert-Match -Text $hp -Pattern 'maxSurge: 0\s+maxUnavailable: 1' -Because '新旧 Pod 不能抢同一节点的 hostPort 而卡 Pending'
    Assert-Match -Text $hp -Pattern 'startupProbe:\s+tcpSocket:\s+port: 20000' -Because '探针沿用 Deployment 版 battle'
    Assert-Match -Text $hp -Pattern 'readinessProbe:\s+tcpSocket:\s+port: 50000' -Because '探针沿用 Deployment 版 battle'
    $main = Get-MainContainerSection -Doc $hp
    foreach ($pair in @(@('CLIENT_ENDPOINT_SOURCE', 'static'), @('CLIENT_ENDPOINT_HOST', '$(HOST_IP)'), @('CLIENT_ENDPOINT_PORT', '20000'), @('CLIENT_ENDPOINT_REQUIRED', '1'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-EnvLiteral -Text $main -Name $pair[0]) -Because "battle hostPort env $($pair[0])"
    }
    Assert-True -Condition (Test-EnvFieldRef -Text $main -Name 'HOST_IP' -FieldPath 'status.hostIP') -Because 'HOST_IP 走 downward API'
    Assert-True -Condition ($main.IndexOf('- name: HOST_IP') -lt $main.IndexOf('- name: CLIENT_ENDPOINT_HOST')) -Because 'kubelet 的 $(VAR) 只展开排在前面的 env'
    Assert-NotMatch -Text $hp -Pattern $D92Pattern -Because 'D92:新生成器只写 RPC_PORT'
    $withHost = Get-MainContainerSection -Doc (ConvertTo-LfText (New-BattleHostPortDeploymentYaml -PodCommon $LibPodCommon -Replicas 1 -RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config' -ClientPublicHost '127.0.0.1'))
    Assert-Equal -Expected '127.0.0.1' -Actual (Get-EnvLiteral -Text $withHost -Name 'CLIENT_ENDPOINT_HOST') -Because '给了 -ClientPublicHost 就用它(kind 填 127.0.0.1)'
}

Test-Case '库:gateway Ingress 只路由 /api(Prefix),{zone} 生成期渲染,TLS 可选;/admin 与 /actuator 不出集群' {
    $ing = ConvertTo-LfText (New-GatewayIngressYaml -IngressHost 'play.{zone}.example.com' -ZoneName 'z1' -TlsSecret 'gw-tls')
    Assert-Match -Text $ing -Pattern '(?m)^apiVersion: networking\.k8s\.io/v1$' -Because 'Ingress v1'
    Assert-Match -Text $ing -Pattern '(?m)^  name: gateway$' -Because 'Ingress 名 gateway'
    Assert-Match -Text $ing -Pattern '(?m)^  ingressClassName: nginx$' -Because '默认 class nginx'
    Assert-Match -Text $ing -Pattern 'tls:\s+- hosts:\s+- play\.z1\.example\.com\s+secretName: gw-tls' -Because '给了 TLS Secret 才生成 tls 段'
    Assert-Match -Text $ing -Pattern '- host: play\.z1\.example\.com' -Because '{zone} 在生成期渲染'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($ing, '- path:').Count) -Because '只放一条玩家面路由'
    Assert-Match -Text $ing -Pattern '- path: /api\s+pathType: Prefix\s+backend:\s+service:\s+name: gateway\s+port:\s+number: 8081' -Because '只路由 /api,后端 gateway:8081(D91)'
    Assert-Equal -Expected '/api' -Actual (Get-ClientEntryContract).GatewayIngressPathPrefix -Because '路由前缀的唯一真相在库契约里,与 gateway controller 的 @RequestMapping("/api") 一致'
    Assert-NotMatch -Text $ing -Pattern '/admin|/actuator|(?m)path: /\s*$' -Because '管理面与探针只在集群内访问;也不许用根路径 / 把它们一并放出集群(D91)'
    $bare = ConvertTo-LfText (New-GatewayIngressYaml -IngressHost 'play.example.com' -IngressClassName '')
    Assert-NotMatch -Text $bare -Pattern 'ingressClassName|tls:' -Because 'class 留空不写;不给 TLS Secret 不生成 tls 段'
    $err = $null
    try { New-GatewayIngressYaml -IngressHost 'play.{zone}.example.com' | Out-Null } catch { $err = $_.Exception.Message }
    Assert-Match -Text $err -Pattern '含 \{zone\}' -Because '渲染不了的 host 必须 throw'
}

Test-Case '库:gate.rate-limit.trusted-proxies 片段接在 gate: 下,空列表不写,非法 CIDR 直接拒' {
    $expected = @('  rate-limit:', '    trusted-proxies:', '      - "10.0.0.0/8"', '      - "192.168.1.1"') -join "`n"
    Assert-Equal -Expected $expected -Actual (ConvertTo-LfText (New-GatewayRateLimitYaml -TrustedProxies ' 10.0.0.0/8 , 192.168.1.1 ,' -Indent '  ')) -Because '去空白、去空项;缩进是 gate: 子键的缩进'
    Assert-Equal -Expected '' -Actual (New-GatewayRateLimitYaml -TrustedProxies '') -Because '空 = 只信 socket 对端(fail-closed),不写键'
    foreach ($bad in @('not-a-cidr', '10.0.0.0/33', '10.0.0/8')) {
        $err = $null
        try { New-GatewayRateLimitYaml -TrustedProxies $bad | Out-Null } catch { $err = $_.Exception.Message }
        Assert-Match -Text $err -Pattern '含非法 CIDR' -Because "gateway 会静默跳过非法项 '$bad',漏掉的网段让全体玩家共用一个限流桶"
    }
}

Test-Case '库:DevPasswordAuth 片段的键与值 == go/login/etc/login.yaml(跨文件一致)' {
    $devPasswordYaml = ConvertTo-LfText (New-LoginDevPasswordAuthYaml)
    $flat = (ConvertFrom-YamlToFlatMap -Text $devPasswordYaml).Scalars
    foreach ($key in @('DevPasswordAuth.Enabled', 'DevPasswordAuth.SharedSecretEnv', 'DevPasswordAuth.AllowedAccountPrefixes[0]', 'DevPasswordAuth.AllowedAccountPrefixes[1]')) {
        Assert-True -Condition $flat.Contains($key) -Because "生成的片段里应当有 $key"
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath $key) -Actual $flat[$key] -Because "$key 必须与 login 自己的配置同名同值(键名由 config.go DevPasswordAuthConf 决定)"
    }
    Assert-NotMatch -Text $devPasswordYaml -Pattern '(?m)^PasswordAuth:|SharedSecret:' -Because '与 PasswordAuth 互斥(auth_init.go);共享口令本身绝不进 ConfigMap,只写环境变量名'
}

# ─────────────────────────────────────────────────────────────────
# 1d. 库:preflight 的每条报错与警告(纯函数;Errors / Warnings 恰好是期望的那几条,不多不少)
# ─────────────────────────────────────────────────────────────────

function New-PreflightZone {
    param([string]$Name = 'z1', [int]$Replicas = 2, [int]$Base = 30000, [bool]$Explicit = $true)
    return [pscustomobject]@{ Name = $Name; GateReplicas = $Replicas; GateNodePortBase = $Base; GateNodePortBaseExplicit = $Explicit }
}

function Assert-PreflightExactly {
    param(
        [Parameter(Mandatory = $true)]$Result,
        [string[]]$Errors = @(),
        [string[]]$Warnings = @(),
        [Parameter(Mandatory = $true)][string]$Label
    )
    foreach ($kind in @(@{ Name = '错误'; Actual = @($Result.Errors); Expected = @($Errors) }, @{ Name = '警告'; Actual = @($Result.Warnings); Expected = @($Warnings) })) {
        $actualText = $kind.Actual -join ' | '
        Assert-Equal -Expected $kind.Expected.Count -Actual $kind.Actual.Count -Because "${Label}:$($kind.Name)条数(实际:$actualText)"
        foreach ($pattern in $kind.Expected) {
            Assert-Equal -Expected 1 -Actual @($kind.Actual | Where-Object { $_ -match $pattern }).Count -Because "${Label}:应当恰有一条$($kind.Name)匹配 /$pattern/(实际:$actualText)"
        }
    }
}

# 每项:@{ Label; Args(传给 Test-ClientEntryPreflight 的 splat);Errors;Warnings }。
$PreflightCases = @(
    @{ Label = '干净的 external + NodePort 2 副本,配齐 Ingress 与 trusted proxies'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); ClientPublicHost = '127.0.0.1'; DeploysGateway = $true; GatewayIngressHost = 'play.example.com'; GatewayTrustedProxies = '10.0.0.0/8' } },
    @{ Label = '干净的 podip 默认(含 battle 与 gateway)'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); DeploysBattle = $true; DeploysGateway = $true } },
    @{ Label = 'external + agones 且 CRD 已装'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; DeploysBattle = $true; BattleOrchestrator = 'agones'; AgonesFleetCrdPresent = $true } },
    @{ Label = 'external 配 ClusterIP'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'ClusterIP'; Zones = @(New-PreflightZone) }; Errors = @('要求 -GateServiceType 为 NodePort 或 LoadBalancer') },
    @{ Label = 'LoadBalancer 没有模板'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'LoadBalancer'; Zones = @(New-PreflightZone) }; Errors = @('LoadBalancer 需要 -GateClientHostTemplate') },
    @{ Label = 'LoadBalancer 配带 {ordinal} 的模板'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'LoadBalancer'; Zones = @(New-PreflightZone); GateClientHostTemplate = 'gate-{ordinal}.{zone}.example.com' } },
    @{ Label = '副本数 >1 而模板不含 {ordinal}'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); GateClientHostTemplate = 'gate.example.com' }; Errors = @('zone z1:gate 副本数 2 > 1,-GateClientHostTemplate 必须含 \{ordinal\}') },
    @{ Label = '单副本的模板可以不含 {ordinal}'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Replicas 1); GateClientHostTemplate = 'gate.example.com' } },
    @{ Label = '模板带端口'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); GateClientHostTemplate = 'gate-{ordinal}.example.com:30000' }; Errors = @('-GateClientHostTemplate 只能含字母') },
    @{ Label = 'GateServiceType 大小写不标准(两种模式都拦)'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'nodeport'; Zones = @(New-PreflightZone) }; Errors = @('必须按 K8s 的大小写写成 NodePort') },
    @{ Label = 'nodePort 段低于 30000'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Base 29999) }; Errors = @('\[29999-30000\] 越界') },
    @{ Label = 'nodePort 段高于 32767'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Base 32767) }; Errors = @('\[32767-32768\] 越界') },
    @{ Label = 'nodePort 段顶到 32767(合法但出推荐子段)'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Base 32766) }; Warnings = @('\[32766-32767\] 超出推荐静态子段 30000-30085') },
    @{ Label = 'nodePort 段恰好收在 30085'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Base 30084) } },
    @{ Label = 'nodePort 段越过 30085'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Base 30085) }; Warnings = @('\[30085-30086\] 超出推荐静态子段') },
    @{ Label = '两个 zone 的 nodePort 段重叠'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @((New-PreflightZone -Name 'a' -Base 30000), (New-PreflightZone -Name 'b' -Base 30001)) }; Errors = @('zone a 与 zone b 的 gate nodePort 段重叠:\[30000-30001\] 与 \[30001-30002\]') },
    @{ Label = '两个 zone 的 nodePort 段相邻不重叠'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @((New-PreflightZone -Name 'a' -Base 30000), (New-PreflightZone -Name 'b' -Base 30002)) } },
    @{ Label = '多 zone 有 zone 没显式写 base'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @((New-PreflightZone -Name 'a' -Base 30000), (New-PreflightZone -Name 'b' -Base 30010 -Explicit $false)) }; Errors = @('每个 zone 必须在 zones 配置里显式写 gateNodePortBase\(缺:b\)') },
    @{ Label = '单 zone 不要求显式 base'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone -Explicit $false) } },
    @{ Label = 'externalTrafficPolicy Cluster'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); GateExternalTrafficPolicy = 'Cluster' }; Warnings = @('Cluster 会对入站连接做 SNAT') },
    @{ Label = 'agones 但集群没装 Agones'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; DeploysBattle = $true; BattleOrchestrator = 'agones'; AgonesFleetCrdPresent = $false }; Errors = @('需要集群已安装 Agones:找不到 CRD fleets\.agones\.dev') },
    @{ Label = 'agones 未探测(DryRun 跳过)'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; DeploysBattle = $true; BattleOrchestrator = 'agones' } },
    @{ Label = 'external 配 deployment 形态的 battle'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; DeploysBattle = $true }; Warnings = @('battle 以 hostPort 运行') },
    @{ Label = '配了 Ingress 却没配 trusted proxies'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; DeploysGateway = $true; GatewayIngressHost = 'play.example.com' }; Errors = @('配置了 -GatewayIngressHost 却没有 -GatewayTrustedProxies') },
    @{ Label = 'trusted proxies 含非法 CIDR'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; GatewayTrustedProxies = '10.0.0.0/8,bogus' }; Errors = @('-GatewayTrustedProxies 含非法 CIDR:bogus') },
    @{ Label = 'Ingress host 是 IP'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; DeploysGateway = $true; GatewayIngressHost = '10.0.0.1'; GatewayTrustedProxies = '10.0.0.0/8' }; Errors = @('-GatewayIngressHost 必须是 DNS 名') },
    @{ Label = 'Ingress host 给了但本次不部署 gateway'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; GatewayIngressHost = 'play.example.com'; GatewayTrustedProxies = '10.0.0.0/8' }; Warnings = @('本次不部署 gateway') },
    @{ Label = '多 zone 共用一个不带 {zone} 的 Ingress host'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; Zones = @((New-PreflightZone -Name 'a'), (New-PreflightZone -Name 'b')); DeploysGateway = $true; GatewayIngressHost = 'play.example.com'; GatewayTrustedProxies = '10.0.0.0/8' }; Warnings = @('host 相同的 gateway Ingress') },
    @{ Label = 'external 部署 gateway 却没有 Ingress'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; Zones = @(New-PreflightZone); DeploysGateway = $true }; Warnings = @('external 但未配置 -GatewayIngressHost') },
    @{ Label = 'TLS Secret 没有 Ingress host'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; GatewayIngressTlsSecret = 'gw-tls' }; Warnings = @('TLS 设置被忽略') },
    @{ Label = 'ClientPublicHost 带 scheme'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; ClientPublicHost = 'http://1.2.3.4' }; Errors = @('-ClientPublicHost 只能是裸主机名或 IPv4 字面量') },
    @{ Label = 'ClientPublicHost 带端口'; Args = @{ ClientEntryMode = 'external'; GateServiceType = 'NodePort'; ClientPublicHost = '1.2.3.4:30000' }; Errors = @('-ClientPublicHost 只能是裸主机名或 IPv4 字面量') },
    @{ Label = 'podip 下给了 ClientPublicHost'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; ClientPublicHost = '127.0.0.1' }; Warnings = @('podip 下 -ClientPublicHost / -GateClientHostTemplate 不生效') },
    @{ Label = '非 dev 档开 -LoginDevPasswordAuth'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; LoginDevPasswordAuth = $true; ReleaseProfile = 'staging' }; Errors = @('-LoginDevPasswordAuth 只允许 -ReleaseProfile dev\(当前 staging\)') },
    @{ Label = 'dev 档开 -LoginDevPasswordAuth'; Args = @{ ClientEntryMode = 'podip'; GateServiceType = 'NodePort'; LoginDevPasswordAuth = $true; ReleaseProfile = 'dev' } }
)

foreach ($case in $PreflightCases) {
    Test-Case "预检:$($case.Label)" {
        $caseArgs = $case.Args
        $result = Test-ClientEntryPreflight @caseArgs
        Assert-PreflightExactly -Result $result -Errors @($case.Errors | Where-Object { $_ }) -Warnings @($case.Warnings | Where-Object { $_ }) -Label $case.Label
    }
}

Test-Case '预检:Assert-ClientEntryPreflightResult 一次列全全部错误再拒绝,警告逐条带 [client-entry] 前缀' {
    $err = $null
    try { Assert-ClientEntryPreflightResult -Result ([pscustomobject]@{ Errors = @('第一条', '第二条'); Warnings = @() }) } catch { $err = $_.Exception.Message }
    Assert-Match -Text $err -Pattern '集群外入口预检失败\(2 项\)' -Because '一次报全,不修一条跑一次'
    Assert-Match -Text $err -Pattern '(?s)第一条.*第二条' -Because '每条错误都要列出来'
    $captured = $null
    Assert-ClientEntryPreflightResult -Result ([pscustomobject]@{ Errors = @(); Warnings = @('只是提醒') }) -WarningVariable captured -WarningAction SilentlyContinue
    Assert-Equal -Expected '[client-entry] 只是提醒' -Actual (@($captured) -join '|') -Because '没有错误时只打警告、不拒绝'
}

# ─────────────────────────────────────────────────────────────────
# 1e. 库:碰集群的函数(进程内,同名 function kubectl 顶替;函数先于外部命令解析,被测函数按动态作用域找到它)
# ─────────────────────────────────────────────────────────────────

Test-Case '库:Agones CRD 探测 —— 有 fleets 为真、组里没有 fleets 或 NotFound 为假,其余失败一律 throw(查不到不等于没装)' {
    $script:CrdReply = $null
    $calls = [System.Collections.Generic.List[string]]::new()
    function kubectl {
        $calls.Add((@($args | ForEach-Object { [string]$_ }) -join ' '))
        if ($script:CrdReply.Stderr) { Write-Error -Message $script:CrdReply.Stderr -ErrorAction Continue }
        $global:LASTEXITCODE = $script:CrdReply.ExitCode
        if ($script:CrdReply.Stdout) { return $script:CrdReply.Stdout }
    }
    # 三条路径:装了 → $true;确定没装(组里没有 fleets / 组未注册 NotFound)→ $false;其余一切失败 → throw。
    # 第三条是 fail-closed 的要害:Forbidden、连不上、组发现本身失败(ServiceUnavailable)、空返回都不是"没装",
    # 当成没装会让 agones 编排的预检放行错误组合,或让删 Fleet 残留的清理静默跳过。
    $cases = @(
        @{ Label = '已装'; Reply = @{ ExitCode = 0; Stdout = '{"kind":"APIResourceList","resources":[{"name":"fleets"},{"name":"gameservers"}]}' }; Expected = $true },
        @{ Label = '组里没有 fleets'; Reply = @{ ExitCode = 0; Stdout = '{"kind":"APIResourceList","resources":[{"name":"gameservers"}]}' }; Expected = $false },
        @{ Label = '组未注册'; Reply = @{ ExitCode = 1; Stderr = 'Error from server (NotFound): the server could not find the requested resource' }; Expected = $false },
        @{ Label = '无权限'; Reply = @{ ExitCode = 1; Stderr = 'Error from server (Forbidden): forbidden' }; Throws = '无法确认集群是否安装了 Agones\(kubectl get --raw /apis/agones\.dev/v1 失败,exit 1\)' },
        @{ Label = '组发现失败(部分发现失败,不是 NotFound)'; Reply = @{ ExitCode = 1; Stderr = 'Error from server (ServiceUnavailable): the server is currently unable to handle the request' }; Throws = '无法确认集群是否安装了 Agones\(kubectl get --raw' },
        @{ Label = '集群不可达'; Reply = @{ ExitCode = 1; Stderr = 'Unable to connect to the server: dial tcp 127.0.0.1:6443: connectex: No connection could be made' }; Throws = '无法确认集群是否安装了 Agones\(kubectl get --raw' },
        @{ Label = '返回不是 JSON'; Reply = @{ ExitCode = 0; Stdout = '<html>' }; Throws = '返回不是 JSON' },
        @{ Label = '成功但返回为空'; Reply = @{ ExitCode = 0 }; Throws = '无法确认集群是否安装了 Agones' }
    )
    foreach ($case in $cases) {
        $script:CrdReply = $case.Reply
        $calls.Clear()
        $actual = $null
        $err = $null
        try { $actual = Test-AgonesFleetCrdPresent -KubeContext 'kind-mmorpg' -KubeConfig 'C:/kube/kind.yaml' } catch { $err = $_.Exception.Message }
        if ($case.ContainsKey('Throws')) {
            Assert-Match -Text $err -Pattern $case.Throws -Because "$($case.Label):fail-closed,既不能当装了也不能当没装"
        }
        else {
            Assert-Equal -Expected $case.Expected -Actual $actual -Because "$($case.Label)(错误:$err)"
        }
        Assert-Equal -Expected '--context kind-mmorpg --kubeconfig C:/kube/kind.yaml get --raw /apis/agones.dev/v1' -Actual ($calls -join ';') -Because "$($case.Label):只读 agones.dev 一个组的发现文档,且显式带上调用方给的集群(不落到默认 context)"
    }
}

Test-Case '库:删除另一种形态 —— 会踢人的条目存在且未确认就整条拒绝、一个都不删;确认后才删;DryRun 只打意图' {
    $calls = [System.Collections.Generic.List[string]]::new()
    function kubectl {
        $line = (@($args | ForEach-Object { [string]$_ }) -join ' ')
        $calls.Add($line)
        $global:LASTEXITCODE = 0
        if ($line -match '^get deployment gate ') { return 'deployment.apps/gate' }
        if ($line -match '^get service gate-entry ') { return 'service/gate-entry' }
    }
    $items = Get-GateObsoleteResources -ClientEntryMode external -GateReplicas 2
    $err = $null
    try { Remove-ClientEntryObsoleteResources -Namespace 'ns1' -Resources $items -KubeContext '' -KubeConfig '' 3>$null } catch { $err = $_.Exception.Message }
    Assert-Match -Text $err -Pattern '拒绝删除正在服务的工作负载' -Because '模式参数不粘滞:漏传 -ClientEntryMode 不能把正在服务的 gate 删掉'
    Assert-Match -Text $err -Pattern '当前集群是 deployment gate\(deployment\.apps/gate,namespace=ns1\)' -Because '报错要写明集群现状'
    Assert-Match -Text $err -Pattern '要保持现状请显式传 -ClientEntryMode podip;确认要切换请加 -AllowDisruptiveSwitch' -Because '报错要告诉操作者怎么继续'
    # 报错原文整句钉住(Find-ClientEntryObsoleteResources 的拒绝文本 + Remove 的抬头),改措辞必须同步改这里与运维文档。
    $refusalLine = '当前集群是 deployment gate(deployment.apps/gate,namespace=ns1),本次参数会删除它:' +
        'gate 从 Deployment 切到 StatefulSet(-ClientEntryMode external):旧 Deployment 上的玩家全部断线,需要重新登录。' +
        ' 要保持现状请显式传 -ClientEntryMode podip;确认要切换请加 -AllowDisruptiveSwitch。'
    $expectedRefusal = "拒绝删除正在服务的工作负载(模式参数不粘滞,漏传会落回默认值 podip / deployment),未删除任何资源:`n  - $refusalLine"
    Assert-Equal -Expected $expectedRefusal -Actual $err -Because 'KicksPlayers 未确认时的报错原文(一条会踢人的条目)'
    Assert-Equal -Expected 0 -Actual @($calls | Where-Object { $_ -notmatch '^get ' }).Count -Because "拒绝时一个都不删(不留删了 gate-entry、没删 gate 的半切换状态)。调用: $($calls -join '; ')"

    # 只探测不删:预检(Assert-ClientEntryClusterState)与删除前的闸用的是同一份判据与拒绝文本。
    $calls.Clear()
    $probe = Find-ClientEntryObsoleteResources -Namespace 'ns1' -Resources $items -KubeContext '' -KubeConfig '' -KicksPlayersOnly
    Assert-Equal -Expected 'get deployment gate -n ns1 --ignore-not-found -o name' -Actual ($calls -join '|') -Because '-KicksPlayersOnly 只探测会踢人的条目,不为残留清理多打 get,也绝不删除'
    Assert-Equal -Expected $refusalLine -Actual (@($probe.Refusals) -join '|') -Because '拒绝文本与 Remove-ClientEntryObsoleteResources 报错里的条目逐字相同'
    Assert-Equal -Expected 'deployment.apps/gate' -Actual (@($probe.Present | ForEach-Object { $_.Names -join ',' }) -join '|') -Because 'Present 只含探测到的会踢人条目'
    $calls.Clear()
    $full = Find-ClientEntryObsoleteResources -Namespace 'ns1' -Resources $items -KubeContext '' -KubeConfig ''
    Assert-Equal -Expected 2 -Actual @($full.Present).Count -Because '不加 -KicksPlayersOnly 时残留清理条目(gate-entry)同样探测'
    Assert-Equal -Expected 1 -Actual @($full.Refusals).Count -Because '不踢人的残留不产生拒绝文本'
    Assert-Equal -Expected 0 -Actual @($calls | Where-Object { $_ -notmatch '^get ' }).Count -Because "Find 只读,不删任何东西。调用: $($calls -join '; ')"

    $calls.Clear()
    Remove-ClientEntryObsoleteResources -Namespace 'ns1' -Resources $items -KubeContext '' -KubeConfig '' -AllowDisruptiveSwitch 3>$null
    $deletes = @($calls | Where-Object { $_ -match '^delete ' })
    Assert-Equal -Expected 'delete deployment.apps/gate -n ns1 --ignore-not-found|delete service/gate-entry -n ns1 --ignore-not-found' -Actual ($deletes -join '|') -Because '确认切换后按探测到的名字逐条删除'

    $calls.Clear()
    $dry = Remove-ClientEntryObsoleteResources -Namespace 'ns1' -Resources $items -KubeContext '' -KubeConfig '' -DryRun 6>&1 | Out-String
    Assert-Equal -Expected 0 -Actual $calls.Count -Because 'DryRun 不探测集群'
    Assert-Match -Text $dry -Pattern '\[dry-run\] kubectl delete deployment gate -n ns1 --ignore-not-found .*未给 -AllowDisruptiveSwitch,真实执行会拒绝并中止' -Because 'DryRun 要注明会踢人的删除在真实执行时需要确认'
    Assert-Match -Text $dry -Pattern '\[dry-run\] kubectl delete service gate-entry -n ns1 --ignore-not-found' -Because '不踢人的残留清理同样打出意图'
}

Test-Case '库:gate 缩容后只删序号 ≥ 副本数的每序号 Service,认不出的标签值不删' {
    $calls = [System.Collections.Generic.List[string]]::new()
    function kubectl {
        $line = (@($args | ForEach-Object { [string]$_ }) -join ' ')
        $calls.Add($line)
        $global:LASTEXITCODE = 0
        if ($line -match '^get service ') {
            return '{"items":[' +
                '{"metadata":{"name":"gate-0","labels":{"mmorpg.io/gate-ordinal":"0"}}},' +
                '{"metadata":{"name":"gate-1","labels":{"mmorpg.io/gate-ordinal":"1"}}},' +
                '{"metadata":{"name":"gate-2","labels":{"mmorpg.io/gate-ordinal":"2"}}},' +
                '{"metadata":{"name":"gate-x","labels":{"mmorpg.io/gate-ordinal":"x"}}}]}'
        }
    }
    Remove-StaleGateOrdinalServices -Namespace 'ns1' -Replicas 2 -KubeContext '' -KubeConfig '' 3>$null
    Assert-Equal -Expected 'delete service/gate-2 -n ns1 --ignore-not-found' -Actual (@($calls | Where-Object { $_ -match '^delete ' }) -join '|') -Because '多出来的 gate-2 继续占着 nodePort;gate-x 认不出序号,只告警不删'
}

# Fleet 滚动就绪判据的夹具:经 JSON 往返,得到与 kubectl -o json 同形的对象(creationTimestamp 会被转成 DateTime)。
# 模板 labels / annotations 与本库 Fleet 生成器同口径:mmorpg.io/build 随镜像变,sidecar 配置哈希随日志配置变。
function New-TestFleetObject {
    $fleet = [ordered]@{
        metadata = [ordered]@{ name = 'battle' }
        spec     = [ordered]@{ replicas = 2; template = [ordered]@{ metadata = [ordered]@{
                    labels      = [ordered]@{ app = 'battle'; 'mmorpg.io/build' = 'b2' }
                    annotations = [ordered]@{ 'mmorpg.io/cpp-log-sidecar-config-hash' = 'h1' } } } }
        # 合计刻意"已达标":判据不得看它(滚动时旧 GameServerSet 的实例会让合计提前达标)。
        status   = [ordered]@{ replicas = 4; readyReplicas = 2; allocatedReplicas = 0 }
    }
    return ($fleet | ConvertTo-Json -Depth 10 -Compress | ConvertFrom-Json)
}

function New-TestGameServerSet {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Created,
        [string]$Build = 'b2',
        [string]$SidecarHash = 'h1',
        [int]$Replicas = 2,
        [int]$Ready = 0,
        [int]$Allocated = 0
    )
    $set = [ordered]@{
        metadata = [ordered]@{ name = $Name; creationTimestamp = $Created; labels = [ordered]@{ 'agones.dev/fleet' = 'battle' } }
        spec     = [ordered]@{ replicas = $Replicas; template = [ordered]@{ metadata = [ordered]@{
                    labels      = [ordered]@{ app = 'battle'; 'mmorpg.io/build' = $Build }
                    annotations = [ordered]@{ 'mmorpg.io/cpp-log-sidecar-config-hash' = $SidecarHash } } } }
        status   = [ordered]@{ replicas = $Replicas; readyReplicas = $Ready; allocatedReplicas = $Allocated }
    }
    return ($set | ConvertTo-Json -Depth 10 -Compress | ConvertFrom-Json)
}

$FleetOldCreated = '2026-09-29T00:00:00Z'
$FleetNewCreated = '2026-09-29T00:05:00Z'

Test-Case '库:Fleet 滚动就绪判据 —— 只认当前模板的 GameServerSet;旧 GameServerSet 的 Ready 不计、在打的 Allocated 计入;Fleet.status 合计不作数' {
    $cases = @(
        @{ Label = '还没有 GameServerSet'; Sets = @(); Done = $false; State = 'no GameServerSet yet' },
        @{ Label = '换镜像后控制器还没建新 GameServerSet(最新的仍是旧模板)'
            Sets = @(New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Ready 2); Done = $false; State = 'does not carry the current Fleet template' },
        @{ Label = '只改了日志 sidecar 配置(模板注解不同)'
            Sets = @(New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -SidecarHash 'h0' -Ready 2); Done = $false; State = 'does not carry the current Fleet template' },
        @{ Label = '滚动刚开始:旧 GameServerSet 2 个 Ready,新的一个都没就绪'
            Sets = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Ready 2), (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated))
            Done = $false; State = 'current=battle-new ready=0 allocated=0 target=2 old_sets=1 old_allocated=0 expected=2' },
        @{ Label = '旧 GameServerSet 的 Ready 不计:新 1 Ready + 旧 1 Ready'
            Sets = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Replicas 1 -Ready 1), (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated -Replicas 1 -Ready 1)); Done = $false },
        @{ Label = '排空中:新 1 Ready + 旧 1 Allocated(在打的局打完即回收)'
            Sets = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Replicas 0 -Allocated 1), (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated -Replicas 1 -Ready 1)); Done = $true },
        @{ Label = '新 GameServerSet 自己没扩容到位(旧 Allocated 凑够总数也不行)'
            Sets = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Replicas 0 -Allocated 1), (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated -Replicas 2 -Ready 1)); Done = $false },
        @{ Label = '滚动完成(新 GameServerSet 的 Allocated 同样计入)'
            Sets = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Replicas 0), (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated -Ready 1 -Allocated 1)); Done = $true },
        @{ Label = '最新的两个时间戳并列,无法判定哪个是当前'
            Sets = @((New-TestGameServerSet -Name 'battle-a' -Created $FleetNewCreated -Ready 2), (New-TestGameServerSet -Name 'battle-b' -Created $FleetNewCreated -Ready 2)); Done = $false; State = 'ambiguous current GameServerSet' }
    )
    $fleet = New-TestFleetObject
    foreach ($case in $cases) {
        $verdict = Measure-FleetRolloutReadiness -Fleet $fleet -GameServerSets @($case.Sets) -ExpectedReplicas 2
        Assert-Equal -Expected $case.Done -Actual $verdict.Done -Because "$($case.Label)(State: $($verdict.State))"
        if ($case.ContainsKey('State')) {
            Assert-Match -Text $verdict.State -Pattern ([regex]::Escape($case.State)) -Because "$($case.Label):State 要写明卡在哪,超时报错原样带出"
        }
    }
}

Test-Case '库:Wait-ForFleetReady 按 agones.dev/fleet 标签取 GameServerSet 逐个判定,只读;Fleet 合计达标而新模板没就绪时截止后 throw' {
    $calls = [System.Collections.Generic.List[string]]::new()
    $fleetJson = New-TestFleetObject | ConvertTo-Json -Depth 10 -Compress
    $setsQuery = 'get gameserversets.agones.dev -n ns1 -l agones.dev/fleet=battle -o json'
    function kubectl {
        $line = (@($args | ForEach-Object { [string]$_ }) -join ' ')
        $calls.Add($line)
        $global:LASTEXITCODE = 0
        if ($line -ceq 'get fleet battle -n ns1 -o json') { return $fleetJson }
        if ($line -ceq $setsQuery) { return $script:FleetSetsJson }
    }
    $script:FleetSetsJson = @{ items = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Ready 2),
            (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated)) } | ConvertTo-Json -Depth 12 -Compress
    $err = $null
    try { Wait-ForFleetReady -Namespace 'ns1' -FleetName 'battle' -ExpectedReplicas 2 -KubeContext '' -KubeConfig '' -TimeoutSeconds 1 -PollIntervalSeconds 1 6>$null }
    catch { $err = $_.Exception.Message }
    Assert-Equal -Expected 'fleet not ready after 1s: namespace=ns1 name=battle (current=battle-new ready=0 allocated=0 target=2 old_sets=1 old_allocated=0 expected=2)' -Actual $err -Because '旧 GameServerSet 的 Ready 让 Fleet.status 合计达标,也不能让 -WaitReady 在新版本一个都没就绪时交差'
    Assert-True -Condition ($calls -ccontains $setsQuery) -Because "按 Agones 写在 GameServerSet 上的所属 Fleet 标签取集合。调用: $($calls -join '; ')"
    Assert-Equal -Expected 0 -Actual @($calls | Where-Object { $_ -notmatch '^(get|describe) ' }).Count -Because "等待与超时诊断都只读。调用: $($calls -join '; ')"

    $calls.Clear()
    $script:FleetSetsJson = @{ items = @((New-TestGameServerSet -Name 'battle-old' -Created $FleetOldCreated -Build 'b1' -Replicas 0),
            (New-TestGameServerSet -Name 'battle-new' -Created $FleetNewCreated -Ready 2)) } | ConvertTo-Json -Depth 12 -Compress
    $err = $null
    try { Wait-ForFleetReady -Namespace 'ns1' -FleetName 'battle' -ExpectedReplicas 2 -KubeContext '' -KubeConfig '' -TimeoutSeconds 5 -PollIntervalSeconds 1 6>$null }
    catch { $err = $_.Exception.Message }
    Assert-Equal -Expected $null -Actual $err -Because '新模板的 GameServerSet 全部就绪即返回'
    Assert-Equal -Expected "get fleet battle -n ns1 -o json|$setsQuery" -Actual ($calls -join '|') -Because '首轮就绪:一次 get fleet + 一次 get GameServerSet,不打诊断'
}

# ─────────────────────────────────────────────────────────────────
# 2. k8s_deploy.ps1 -DryRun 端到端:接入把库的形态原样送到 apply,podip 默认路径保持现状
# ─────────────────────────────────────────────────────────────────

# 默认 GateReplicas = 2(k8s_deploy.ps1 参数默认值)。
$PodipRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + $GoJavaArgs)
$PodipSingleRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-GateReplicas', '1', '-SkipGoSvc', '-SkipJavaSvc'))
$PodipInfraRun = Invoke-EntryDryRun -Arguments $InfraUpArgs -Env $BattleTestEnv

# gate / battle 业务容器里只属于集群外入口的 env(sidecar 的 POD_NAME 在 initContainers 里,不在此列)。
$ClientEntryOnlyEnv = @('CLIENT_ENDPOINT_SOURCE', 'CLIENT_ENDPOINT_HOST', 'CLIENT_ENDPOINT_PORT', 'CLIENT_ENDPOINT_REQUIRED', 'HOST_IP', 'POD_NAME',
    'GATE_CLIENT_HOST_TEMPLATE', 'CLIENT_PUBLIC_HOST', 'GATE_CLIENT_PORT_MODE', 'GATE_NODE_PORT_BASE', 'GATE_CLIENT_PORT')

Test-Case '端到端 podip 默认(gate 2 副本):gate 仍是 Deployment,不带任何集群外入口 env、不占 hostPort,也没有 StatefulSet 一族' {
    Assert-Equal -Expected 0 -Actual $PodipRun.ExitCode -Because "podip 默认 zone-up 的 DryRun 必须成功。输出: $($PodipRun.Output)"
    $gate = Select-ManifestDoc -Output $PodipRun.Output -Kind 'Deployment' -Name 'gate'
    Assert-True -Condition ($null -ne $gate) -Because 'podip 的 gate 是现状 Deployment(D80)'
    $main = Get-MainContainerSection -Doc $gate
    foreach ($name in $ClientEntryOnlyEnv) {
        Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name $name)) -Because "podip 与现状一致:gate 容器不带 $name"
    }
    $workloads = @('Deployment', 'StatefulSet', 'Fleet') | ForEach-Object { Find-ManifestDocs -Output $PodipRun.Output -Kind $_ }
    Assert-NotMatch -Text ($workloads -join "`n") -Pattern 'CLIENT_ENDPOINT_' -Because 'podip 下任何工作负载都不写 CLIENT_ENDPOINT_*(SOURCE=none 时带了会打常态 WARN)'
    Assert-NotMatch -Text ((Get-ManifestDocs -Output $PodipRun.Output) -join "`n") -Pattern 'hostPort' -Because 'podip 不占节点端口'
    foreach ($absent in @(@('StatefulSet', 'gate'), @('Service', 'gate-0'), @('Service', 'gate-headless'), @('PodDisruptionBudget', 'gate'), @('Ingress', 'gateway'))) {
        Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $PodipRun.Output -Kind $absent[0] -Name $absent[1])) -Because "podip 不生成 $($absent[0])/$($absent[1])"
    }
    Assert-Match -Text $PodipRun.Output -Pattern 'Client entry: mode=podip battle_orchestrator=deployment' -Because '写路径入口要打出本次的入口形态摘要'
}

Test-Case '端到端 D90:podip 且 gate 副本数 ≠1 不生成 gate-entry,并清理残留;副本数为 1 才生成' {
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $PodipRun.Output -Kind 'Service' -Name 'gate-entry')) -Because '2 副本时单一 gate-entry 会让约一半票据被 token_gate_node_mismatch 拒绝'
    Assert-Match -Text $PodipRun.Output -Pattern "\[dry-run\] kubectl delete service gate-entry -n $TestZoneNs --ignore-not-found" -Because '以前生成过的 gate-entry 残留要删掉'
    Assert-Equal -Expected 0 -Actual $PodipSingleRun.ExitCode -Because "单副本 zone-up 必须成功。输出: $($PodipSingleRun.Output)"
    $entry = Select-ManifestDoc -Output $PodipSingleRun.Output -Kind 'Service' -Name 'gate-entry'
    Assert-True -Condition ($null -ne $entry) -Because 'podip 单副本:gate-entry 是唯一入口'
    Assert-Match -Text $entry -Pattern '(?m)^  type: NodePort$' -Because 'podip 下 -GateServiceType(默认 NodePort)决定 gate-entry 的类型'
    Assert-Match -Text $entry -Pattern 'selector:\s+app: gate\s+ports:\s+- name: tcp-gate\s+protocol: TCP\s+port: 18000\s+targetPort: rpc' -Because 'gate-entry 形态与改造前一致'
    Assert-NotMatch -Text $PodipSingleRun.Output -Pattern 'kubectl delete service gate-entry' -Because '要生成的东西不能同时出现在删除清单里'
}

Test-Case '端到端 podip:login / scene_manager 的 RequireClientEndpoint = false,键名与服务自己的 etc 一致;不写 DevPasswordAuth,清理口令 Secret' {
    foreach ($cm in @(@{ Name = 'go-svc-login-config'; File = 'login.yaml'; Etc = 'go/login/etc/login.yaml' }, @{ Name = 'go-svc-scene-manager-config'; File = 'scene_manager_service.yaml'; Etc = 'go/scene_manager/etc/scene_manager_service.yaml' })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestDoc -Output $PodipRun.Output -Kind 'ConfigMap' -Name $cm.Name)
        Assert-Equal -Expected 'false' -Actual (Get-FlatValue -Flat $flat -KeyPath "data.$($cm.File).RequireClientEndpoint") -Because "$($cm.Name):podip 的 gate 从不自报地址"
        Assert-Equal -Expected 'false' -Actual (Get-EtcValue -RelativePath $cm.Etc -KeyPath 'RequireClientEndpoint') -Because "$($cm.Etc) 里必须有同名键(go-zero 按键名读,拼错即静默取默认值)"
    }
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestDoc -Output $PodipRun.Output -Kind 'ConfigMap' -Name 'go-svc-login-config')
    Assert-True -Condition (-not $loginFlat.Scalars.Contains('data.login.yaml.DevPasswordAuth.Enabled')) -Because '不传 -LoginDevPasswordAuth = login 没有口令认证配置(fail-closed)'
    Assert-Match -Text $PodipRun.Output -Pattern "\[dry-run\] kubectl delete secret login-dev-password -n $TestZoneNs --ignore-not-found" -Because '开关关闭时删掉残留的口令 Secret'
    $gatewayCm = Select-ManifestDoc -Output $PodipRun.Output -Kind 'ConfigMap' -Name 'java-svc-gateway-config'
    Assert-True -Condition ($null -ne $gatewayCm) -Because '给了 -JavaSvcRegistry 就部署 gateway'
    Assert-NotMatch -Text $gatewayCm -Pattern 'rate-limit|trusted-proxies' -Because '不给 -GatewayTrustedProxies = 只信 socket 对端,不写空键'
}

Test-Case '端到端 infra-up 默认(podip + deployment):battle 仍是现状 Deployment,不带集群外入口 env、不占 hostPort;清理 Fleet 形态只打意图' {
    Assert-Equal -Expected 0 -Actual $PodipInfraRun.ExitCode -Because "infra-up 默认 DryRun 必须成功。输出: $($PodipInfraRun.Output)"
    $battle = Select-ManifestDoc -Output $PodipInfraRun.Output -Kind 'Deployment' -Name 'battle'
    Assert-True -Condition ($null -ne $battle) -Because 'podip + deployment:battle 是现状 Deployment'
    $main = Get-MainContainerSection -Doc $battle
    foreach ($name in $ClientEntryOnlyEnv) {
        Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name $name)) -Because "podip 与现状一致:battle 容器不带 $name"
    }
    Assert-NotMatch -Text $battle -Pattern 'hostPort' -Because 'podip 的 battle 客户端拿 POD_IP:20000'
    Assert-True -Condition ((Find-ManifestDocs -Output $PodipInfraRun.Output -Kind 'Fleet').Count -eq 0) -Because 'deployment 编排不生成 Fleet'
    Assert-Match -Text $PodipInfraRun.Output -Pattern "\[dry-run\] kubectl delete fleets\.agones\.dev battle -n $TestInfraNs --ignore-not-found .*未给 -AllowDisruptiveSwitch" -Because '另一种形态的 Fleet 在删除前要过会踢人的闸(真实执行时集群没装 Agones 会按 ApiGroup 跳过)'
}

$ExternalArgs = @('-ClientEntryMode', 'external', '-GateServiceType', 'NodePort', '-GateNodePortBase', '30000', '-ClientPublicHost', '127.0.0.1')
$ExternalRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + $GoJavaArgs + $ExternalArgs + @(
    '-GatewayIngressHost', 'play.{zone}.example.com', '-GatewayIngressTlsSecret', 'gateway-tls', '-GatewayTrustedProxies', '10.0.0.0/8,192.168.0.0/16', '-WaitReady'))

Test-Case '端到端 external + NodePort 2 副本:gate StatefulSet + gate-0 / gate-1 + headless + PDB,不生成 Deployment gate 与 gate-entry' {
    Assert-Equal -Expected 0 -Actual $ExternalRun.ExitCode -Because "external zone-up 的 DryRun 必须成功。输出: $($ExternalRun.Output)"
    $sts = Select-ManifestDoc -Output $ExternalRun.Output -Kind 'StatefulSet' -Name 'gate'
    Assert-True -Condition ($null -ne $sts) -Because 'external 的 gate 是 StatefulSet(D87)'
    foreach ($pattern in @('(?m)^  replicas: 2$', '(?m)^  serviceName: gate-headless$', '(?m)^  podManagementPolicy: Parallel$', 'updateStrategy:\s+type: OnDelete')) {
        Assert-Match -Text $sts -Pattern $pattern -Because 'StatefulSet 形态(D87)'
    }
    $main = Get-MainContainerSection -Doc $sts
    $literals = [ordered]@{ CLIENT_ENDPOINT_SOURCE = 'static'; CLIENT_ENDPOINT_REQUIRED = '1'; CLIENT_PUBLIC_HOST = '127.0.0.1'; GATE_CLIENT_PORT_MODE = 'nodeport'; GATE_NODE_PORT_BASE = '30000'; GATE_CLIENT_RPC_ROUTER = '1'; RPC_PORT = '18000' }
    foreach ($name in $literals.Keys) {
        Assert-Equal -Expected $literals[$name] -Actual (Get-EnvLiteral -Text $main -Name $name) -Because "gate 容器 env $name 必须原样来自本次参数"
    }
    Assert-True -Condition ((Test-EnvFieldRef -Text $main -Name 'POD_NAME' -FieldPath 'metadata.name') -and (Test-EnvFieldRef -Text $main -Name 'HOST_IP' -FieldPath 'status.hostIP')) -Because '启动 shell 由 POD_NAME 算序号,主机缺省取 HOST_IP'
    Assert-Match -Text $sts -Pattern '(?s)initContainers:\s+- name: log-sidecar' -Because '日志 sidecar 与 Deployment 版同样挂上(sidecar 自带的 POD_NAME 不能混进业务容器)'
    Assert-NotMatch -Text $sts -Pattern $D92Pattern -Because 'D92:新生成器不复用集群内身份 env'
    for ($i = 0; $i -lt 2; $i++) {
        $svc = Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Service' -Name "gate-$i"
        Assert-True -Condition ($null -ne $svc) -Because "每序号 Service gate-$i"
        Assert-Match -Text $svc -Pattern '(?m)^  type: NodePort$' -Because '-GateServiceType NodePort'
        Assert-Match -Text $svc -Pattern '(?m)^  externalTrafficPolicy: Local$' -Because '默认 Local 保留源 IP(D89)'
        Assert-Match -Text $svc -Pattern "statefulset\.kubernetes\.io/pod-name: gate-$i\s" -Because '只选同序号 Pod'
        Assert-Match -Text $svc -Pattern "targetPort: rpc\s+nodePort: $(30000 + $i)\s*$" -Because 'nodePort = base + 序号,与 gate 自报端口同源'
    }
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Service' -Name 'gate-2')) -Because '只生成副本数个每序号 Service'
    Assert-Match -Text (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Service' -Name 'gate-headless') -Pattern 'clusterIP: None' -Because 'governing headless Service'
    Assert-Match -Text (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'PodDisruptionBudget' -Name 'gate') -Pattern 'maxUnavailable: 0' -Because 'PDB maxUnavailable: 0(D87)'
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Deployment' -Name 'gate')) -Because 'Deployment 与 StatefulSet 同名同时在线 = 两套 gate 同时收玩家'
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Service' -Name 'gate-entry')) -Because 'external 由每序号 Service 取代 gate-entry(D90)'
}

Test-Case '端到端 external:先删 Deployment 形态(带确认提示)与 gate-entry,再按 headless → 每序号 Service → StatefulSet → PDB 的顺序 apply,就绪等 StatefulSet' {
    $out = $ExternalRun.Output
    Assert-Match -Text $out -Pattern "\[dry-run\] kubectl delete deployment gate -n $TestZoneNs --ignore-not-found .*未给 -AllowDisruptiveSwitch,真实执行会拒绝并中止" -Because '删 Deployment gate 会整台踢人,真实执行要显式确认'
    Assert-Match -Text $out -Pattern "\[dry-run\] kubectl delete service gate-entry -n $TestZoneNs --ignore-not-found" -Because 'external 删掉单一 gate-entry 残留(D90)'
    $deleteAt = $out.IndexOf('kubectl delete deployment gate')
    $stsAt = $out.IndexOf('kind: StatefulSet')
    Assert-True -Condition ($deleteAt -ge 0 -and $stsAt -gt $deleteAt) -Because '先删旧形态再 apply 新形态'
    $order = @(
        (Get-ManifestDocIndex -Output $out -Kind 'ConfigMap' -Name 'node-config'),
        (Get-ManifestDocIndex -Output $out -Kind 'Service' -Name 'gate-headless'),
        (Get-ManifestDocIndex -Output $out -Kind 'Service' -Name 'gate-0'),
        (Get-ManifestDocIndex -Output $out -Kind 'StatefulSet' -Name 'gate'),
        (Get-ManifestDocIndex -Output $out -Kind 'PodDisruptionBudget' -Name 'gate'))
    Assert-True -Condition (($order -notcontains -1) -and (($order | Sort-Object) -join ',') -eq ($order -join ',')) -Because "Service 先于 StatefulSet:Service 被集群拒绝时 gate 还没按冲突端口发布进 etcd(实际顺序 $($order -join ','))"
    Assert-Match -Text $out -Pattern "\[dry-run\] kubectl delete service -l mmorpg\.io/gate-ordinal -n $TestZoneNs .*序号 >= 2" -Because '缩容后多出来的每序号 Service 要回收(否则继续占着 nodePort)'
    Assert-Match -Text $out -Pattern "\[dry-run\] wait statefulset/gate -n ${TestZoneNs}: poll \.status\.readyReplicas >= 2" -Because 'OnDelete 的 StatefulSet 不能用 rollout status 等'
    Assert-NotMatch -Text $out -Pattern 'rollout status deployment/gate' -Because 'external 下没有 gate Deployment'
    Assert-Match -Text $out -Pattern 'Gate \(external\): StatefulSet replicas=2 service_type=NodePort etp=Local host=127\.0\.0\.1 nodePort=30000\+ordinal' -Because '写路径要打出 gate 地址来源摘要'
}

Test-Case '端到端 external:login / scene_manager 的 RequireClientEndpoint = true;gateway 带 Ingress 与 trusted proxies' {
    foreach ($cm in @(@{ Name = 'go-svc-login-config'; File = 'login.yaml' }, @{ Name = 'go-svc-scene-manager-config'; File = 'scene_manager_service.yaml' })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'ConfigMap' -Name $cm.Name)
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath "data.$($cm.File).RequireClientEndpoint") -Because "$($cm.Name):external 下缺自报地址的 gate 不下发(D78)"
    }
    Assert-Match -Text $ExternalRun.Output -Pattern 'require_client_endpoint=true\(auto\)' -Because '摘要要写明取值与来源'
    $ingress = Select-ManifestDoc -Output $ExternalRun.Output -Kind 'Ingress' -Name 'gateway'
    Assert-True -Condition ($null -ne $ingress) -Because 'Ingress 随 zone-up 的 gateway 生成(D91)'
    Assert-Match -Text $ingress -Pattern "- host: play\.$TestZone\.example\.com" -Because '{zone} 按本 zone 渲染'
    Assert-Match -Text $ingress -Pattern '- path: /api\s+pathType: Prefix' -Because '只路由 /api'
    Assert-Equal -Expected '/api' -Actual (@([regex]::Matches($ingress, '(?m)^[ \t]*- path:[ \t]*(\S+)[ \t]*$') | ForEach-Object { $_.Groups[1].Value }) -join ',') -Because '接入 k8s_deploy 后仍只有 /api 一条路由:/admin/** 与 /actuator/** 不出集群(D91)'
    Assert-Match -Text $ingress -Pattern 'secretName: gateway-tls' -Because '给了 TLS Secret 就生成 tls 段'
    $gatewaySvcAt = Get-ManifestDocIndex -Output $ExternalRun.Output -Kind 'Service' -Name 'gateway'
    Assert-True -Condition ($gatewaySvcAt -ge 0 -and (Get-ManifestDocIndex -Output $ExternalRun.Output -Kind 'Ingress' -Name 'gateway') -gt $gatewaySvcAt) -Because 'Ingress 与 gateway 同处(zone namespace),排在它引用的 gateway Service 之后'
    $gatewayCm = Select-ManifestDoc -Output $ExternalRun.Output -Kind 'ConfigMap' -Name 'java-svc-gateway-config'
    $flat = ConvertTo-FlatManifest -Block $gatewayCm
    Assert-Equal -Expected '10.0.0.0/8' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.gate.rate-limit.trusted-proxies[0]') -Because 'Spring 属性 gate.rate-limit.trusted-proxies'
    Assert-Equal -Expected '192.168.0.0/16' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.gate.rate-limit.trusted-proxies[1]') -Because '逗号分隔的每一项都要写进去'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($gatewayCm, '(?m)^\s*gate:\s*$').Count) -Because '片段必须接在已有的 gate: 下面,不能再起一个 gate:(重复键后者覆盖前者)'
}

$ExternalLowerRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-ClientEntryMode', 'external', '-GateServiceType', 'nodeport',
    '-GateExternalTrafficPolicy', 'cluster', '-ClientPublicHost', '127.0.0.1', '-AllowDisruptiveSwitch'))
$ExternalWarnRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-SkipGoSvc', '-JavaSvcRegistry', 'registry.invalid/test', '-ClientEntryMode', 'external',
    '-GateNodePortBase', '30100', '-ClientPublicHost', '127.0.0.1'))
$RequireFalseRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc') + $ExternalArgs + @('-RequireClientEndpoint', 'false'))

Test-Case '端到端:枚举参数在入口规范成 K8s 大小写(nodeport → NodePort、cluster → Cluster);给了 -AllowDisruptiveSwitch 就不再打确认提示' {
    Assert-Equal -Expected 0 -Actual $ExternalLowerRun.ExitCode -Because "小写取值能过 ValidateSet,必须在入口规范化而不是被 API server 拒收。输出: $($ExternalLowerRun.Output)"
    foreach ($i in 0, 1) {
        $svc = Select-ManifestDoc -Output $ExternalLowerRun.Output -Kind 'Service' -Name "gate-$i"
        Assert-Match -Text $svc -Pattern '(?m)^  type: NodePort$' -Because 'Service.spec.type 区分大小写'
        Assert-Match -Text $svc -Pattern '(?m)^  externalTrafficPolicy: Cluster$' -Because 'externalTrafficPolicy 区分大小写'
    }
    Assert-Match -Text $ExternalLowerRun.Output -Pattern 'Cluster 会对入站连接做 SNAT' -Because 'ETP=Cluster 要警告(D89)'
    Assert-Match -Text $ExternalLowerRun.Output -Pattern "\[dry-run\] kubectl delete deployment gate -n $TestZoneNs --ignore-not-found" -Because '切换仍要删旧形态'
    Assert-NotMatch -Text $ExternalLowerRun.Output -Pattern '真实执行会拒绝并中止' -Because '已确认切换,不该再提示需要 -AllowDisruptiveSwitch'
    Assert-Match -Text $ExternalLowerRun.Output -Pattern 'allow_disruptive_switch=True' -Because '摘要要写明本次确认了会踢人的切换'
}

Test-Case '端到端 external 警告:nodePort 段出推荐子段、部署 gateway 却没有 Ingress —— 只警告不拒绝,且端口原样生效' {
    Assert-Equal -Expected 0 -Actual $ExternalWarnRun.ExitCode -Because "警告不阻断部署。输出: $($ExternalWarnRun.Output)"
    Assert-Match -Text $ExternalWarnRun.Output -Pattern "zone ${TestZone}:gate nodePort 段 \[30100-30101\] 超出推荐静态子段 30000-30085" -Because 'D88 推荐静态子段'
    Assert-Match -Text $ExternalWarnRun.Output -Pattern 'external 但未配置 -GatewayIngressHost' -Because 'gateway 的 HTTP 入口没有集群外入口(D91)'
    Assert-Match -Text (Select-ManifestDoc -Output $ExternalWarnRun.Output -Kind 'Service' -Name 'gate-1') -Pattern 'nodePort: 30101' -Because '-GateNodePortBase 原样生效'
    Assert-Equal -Expected '30100' -Actual (Get-EnvLiteral -Text (Get-MainContainerSection -Doc (Select-ManifestDoc -Output $ExternalWarnRun.Output -Kind 'StatefulSet' -Name 'gate')) -Name 'GATE_NODE_PORT_BASE') -Because 'gate 自报端口与 Service 同源'
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $ExternalWarnRun.Output -Kind 'Ingress' -Name 'gateway')) -Because '没给 -GatewayIngressHost 不生成 Ingress'
}

Test-Case '端到端 -RequireClientEndpoint:external 显式 false 只警告并写 false(逐个 zone 切换的窗口);podip 显式 true 在任何 kubectl 之前拒绝' {
    Assert-Equal -Expected 0 -Actual $RequireFalseRun.ExitCode -Because "窗口期的显式覆盖不阻断部署。输出: $($RequireFalseRun.Output)"
    Assert-Match -Text $RequireFalseRun.Output -Pattern '-ClientEntryMode external 配 -RequireClientEndpoint false' -Because '回落下发集群内 PodIP 要警告'
    foreach ($cm in @(@{ Name = 'go-svc-login-config'; File = 'login.yaml' }, @{ Name = 'go-svc-scene-manager-config'; File = 'scene_manager_service.yaml' })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestDoc -Output $RequireFalseRun.Output -Kind 'ConfigMap' -Name $cm.Name)
        Assert-Equal -Expected 'false' -Actual (Get-FlatValue -Flat $flat -KeyPath "data.$($cm.File).RequireClientEndpoint") -Because "$($cm.Name) 与另一个服务取同一个值"
    }
    $refused = Invoke-EntryDryRun -RefusedAtEntry -Arguments ($ZoneUpArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-RequireClientEndpoint', 'true'))
    Assert-RefusedBeforeAnyKubectl -Run $refused -Patterns @('集群外入口预检失败', '-RequireClientEndpoint true 与 -ClientEntryMode podip 矛盾') -Label 'podip + RequireClientEndpoint true'
    $typo = Invoke-EntryDryRun -RefusedAtEntry -Arguments ($ZoneUpArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-RequireClientEndpoint', 'yes'))
    Assert-RefusedBeforeAnyKubectl -Run $typo -Patterns @('RequireClientEndpoint') -Label '-RequireClientEndpoint 拼错'
}

Test-Case 'zones.sample.json:每个 zone 显式写 gateNodePortBase 且各段不重叠;all-up external 按它生成各 zone 的 nodePort' {
    $samplePath = Join-Path (Get-RepoRoot) 'deploy' 'k8s' 'zones.sample.json'
    $zones = @((Get-Content -LiteralPath $samplePath -Raw -Encoding utf8 | ConvertFrom-Json).zones)
    Assert-True -Condition ($zones.Count -ge 2) -Because '样例要演示多 zone(多 zone + external + NodePort 时每个 zone 必须显式写 base)'
    $records = foreach ($zone in $zones) {
        Assert-True -Condition ($null -ne $zone.gateNodePortBase) -Because "样例 zone $($zone.name) 必须写 zone 级 gateNodePortBase"
        New-PreflightZone -Name $zone.name -Replicas ([int]$zone.replicas.gate) -Base ([int]$zone.gateNodePortBase)
    }
    $check = Test-ClientEntryPreflight -ClientEntryMode external -GateServiceType NodePort -Zones @($records) -ClientPublicHost '127.0.0.1'
    Assert-PreflightExactly -Result $check -Label 'zones.sample.json 的 external + NodePort 预检'

    $run = Invoke-EntryDryRun -Arguments @('-Command', 'all-up', '-SkipInfra', '-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $samplePath,
        '-NodeImage', $NodeImageRef, '-ClientEntryMode', 'external', '-ClientPublicHost', '127.0.0.1')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "样例 all-up external 必须能生成。输出: $($run.Output)"
    $expected = @($records | ForEach-Object { $r = $_; 0..($r.GateReplicas - 1) | ForEach-Object { $r.GateNodePortBase + $_ } } | Sort-Object)
    $actual = @(Find-ManifestDocs -Output $run.Output -Kind 'Service' | ForEach-Object { [regex]::Match($_, '(?m)^\s*nodePort: (\d+)\s*$') } | Where-Object { $_.Success } | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    Assert-Equal -Expected ($expected -join ',') -Actual ($actual -join ',') -Because '每个 zone 的 gate-<i> nodePort = 该 zone 的 gateNodePortBase + i'
    Assert-Equal -Expected $zones.Count -Actual (Find-ManifestDocs -Output $run.Output -Kind 'StatefulSet' -Name 'gate').Count -Because '每个 zone 一个 gate StatefulSet'
}

# YAML 样例与 JSON 样例同一口径。本套测试不带 YAML 解析器:键的写法按 Get-ZonesFromJson 的 YAML 回退解析钉住
# (键独占一行、不带行尾注释,否则回退解析读不到,静默落回命令行 -GateNodePortBase),段的合法性交给 all-up 的预检本身判。
foreach ($sampleName in @('zones.sample.yaml', 'zones.ops-recommended.yaml', 'zones.10zones.yaml')) {
    Test-Case "${sampleName}:每个 zone 显式写 gateNodePortBase;all-up external 下各段不重叠、不越界、不出推荐静态子段" {
        $samplePath = Join-Path (Get-RepoRoot) 'deploy' 'k8s' $sampleName
        $raw = Get-Content -LiteralPath $samplePath -Raw -Encoding utf8
        $zoneCount = [regex]::Matches($raw, '(?m)^[ \t]*-[ \t]+name:').Count
        Assert-True -Condition ($zoneCount -ge 2) -Because "$sampleName 是多 zone 样例(多 zone + external + NodePort 时每个 zone 必须显式写 base)"
        Assert-Equal -Expected $zoneCount -Actual ([regex]::Matches($raw, '(?m)^[ \t]+gateNodePortBase:[ \t]*\d+[ \t]*\r?$').Count) -Because '每个 zone 恰好一行 zone 级 gateNodePortBase: <整数>(与 zoneId 同级,行尾不带注释)'

        $run = Invoke-EntryDryRun -Arguments @('-Command', 'all-up', '-SkipInfra', '-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $samplePath,
            '-NodeImage', $NodeImageRef, '-ClientEntryMode', 'external', '-ClientPublicHost', '127.0.0.1')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "样例 all-up external 必须通过预检(显式、不重叠、不越界)并能生成。输出: $($run.Output)"
        Assert-NotMatch -Text $run.Output -Pattern '超出推荐静态子段' -Because '样例各段都应收在推荐静态子段 30000-30085 内(D88)'
        $gateSets = @(Find-ManifestDocs -Output $run.Output -Kind 'StatefulSet' -Name 'gate')
        Assert-Equal -Expected $zoneCount -Actual $gateSets.Count -Because '每个 zone 一个 gate StatefulSet'
        $gateTotal = 0
        foreach ($sts in $gateSets) { $gateTotal += [int][regex]::Match($sts, '(?m)^  replicas: (\d+)$').Groups[1].Value }
        $nodePorts = @(Find-ManifestDocs -Output $run.Output -Kind 'Service' | ForEach-Object { [regex]::Match($_, '(?m)^\s*nodePort: (\d+)\s*$') } | Where-Object { $_.Success } | ForEach-Object { [int]$_.Groups[1].Value })
        Assert-Equal -Expected $gateTotal -Actual $nodePorts.Count -Because '每个 gate 副本一个每序号 Service'
        Assert-Equal -Expected $nodePorts.Count -Actual @($nodePorts | Sort-Object -Unique).Count -Because "各 zone 的 nodePort 不得重复(实际: $(($nodePorts | Sort-Object) -join ','))"
    }
}

$FleetExternalRun = Invoke-EntryDryRun -Arguments ($InfraUpArgs + @('-BattleOrchestrator', 'agones', '-ClientEntryMode', 'external', '-ClientPublicHost', '127.0.0.1', '-WaitReady')) -Env $BattleTestEnv
$FleetPodipRun = Invoke-EntryDryRun -Arguments ($InfraUpArgs + @('-BattleOrchestrator', 'agones')) -Env $BattleTestEnv
$HostPortRun = Invoke-EntryDryRun -Arguments ($InfraUpArgs + @('-ClientEntryMode', 'external', '-WaitReady')) -Env $BattleTestEnv

Test-Case '端到端 infra-up agones + external:battle 是 Fleet(client 端口 Dynamic、health 抬到 105、agones 来源、不写 PORT),RBAC 先于 Fleet,等 Fleet 就绪' {
    Assert-Equal -Expected 0 -Actual $FleetExternalRun.ExitCode -Because "agones + external 的 DryRun 必须成功(DryRun 跳过 CRD 探测)。输出: $($FleetExternalRun.Output)"
    $out = $FleetExternalRun.Output
    $fleet = Select-ManifestDoc -Output $out -Kind 'Fleet' -Name 'battle'
    Assert-True -Condition ($null -ne $fleet) -Because '-BattleOrchestrator agones 生成 battle Fleet'
    Assert-Match -Text (Get-FleetGameServerPorts -Doc $fleet) -Pattern '^\s*- name: client\s+portPolicy: Dynamic\s+containerPort: 20000\s+protocol: TCP\s*$' -Because 'Agones ports 只有 client 一项(D81)'
    Assert-Match -Text $fleet -Pattern 'initialDelaySeconds: 105\s+periodSeconds: 10\s+failureThreshold: 3' -Because '复用 -AgonesHealth* 默认 30 / 10 / 3,initialDelaySeconds 按推导抬到 105'
    Assert-Match -Text $out -Pattern 'initialDelaySeconds 由 -AgonesHealthInitialDelaySeconds=30 抬到 105' -Because '改写运维给的值必须打出来'
    Assert-Match -Text $fleet -Pattern 'allocationOverflow:\s+labels:\s+mmorpg\.io/drain: "true"' -Because '排空标签(D83)'
    $main = Get-MainContainerSection -Doc $fleet
    foreach ($pair in @(@('CLIENT_ENDPOINT_SOURCE', 'agones'), @('CLIENT_ENDPOINT_HOST', '127.0.0.1'), @('CLIENT_ENDPOINT_REQUIRED', '1'), @('AGONES_ENABLED', '1'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-EnvLiteral -Text $main -Name $pair[0]) -Because "battle Fleet env $($pair[0])"
    }
    Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name 'CLIENT_ENDPOINT_PORT')) -Because 'agones 来源下设置 PORT 即致命'
    Assert-Match -Text $fleet -Pattern 'serviceAccountName: agones-sdk' -Because 'Fleet Pod 用 agones-sdk'
    $saAt = Get-ManifestDocIndex -Output $out -Kind 'ServiceAccount' -Name 'agones-sdk'
    Assert-True -Condition ($saAt -ge 0 -and $saAt -lt (Get-ManifestDocIndex -Output $out -Kind 'Fleet' -Name 'battle')) -Because 'RBAC 必须先于 Fleet,否则第一批 GameServer 建 Pod 被拒'
    Assert-Match -Text (Select-ManifestDoc -Output $out -Kind 'ServiceAccount' -Name 'agones-sdk') -Pattern "namespace: $TestInfraNs" -Because 'battle 在 infra namespace,RBAC 建在同一个 namespace'
    Assert-True -Condition ($null -eq (Select-ManifestDoc -Output $out -Kind 'Deployment' -Name 'battle')) -Because 'Fleet 与 Deployment 同名同时在线 = 两套 battle 同时注册'
    Assert-Match -Text $out -Pattern "\[dry-run\] kubectl delete deployment battle -n $TestInfraNs --ignore-not-found .*未给 -AllowDisruptiveSwitch" -Because '删 Deployment 形态会作废在打的局,真实执行要显式确认'
    Assert-Match -Text $out -Pattern "\[dry-run\] wait fleet/battle -n ${TestInfraNs}: .* >= 1, timeout 300s" -Because 'Fleet 就绪等待截止至少 300s(ingress_final WP9)'
    Assert-NotMatch -Text $out -Pattern 'rollout status deployment/battle' -Because 'Fleet 不是 Deployment'
}

Test-Case '端到端 infra-up agones + podip:Fleet 只写 CLIENT_ENDPOINT_SOURCE=none' {
    Assert-Equal -Expected 0 -Actual $FleetPodipRun.ExitCode -Because "agones + podip 的 DryRun 必须成功。输出: $($FleetPodipRun.Output)"
    $main = Get-MainContainerSection -Doc (Select-ManifestDoc -Output $FleetPodipRun.Output -Kind 'Fleet' -Name 'battle')
    Assert-Equal -Expected 'none' -Actual (Get-EnvLiteral -Text $main -Name 'CLIENT_ENDPOINT_SOURCE') -Because 'podip + agones(D80 矩阵第 2 行)'
    foreach ($name in @('CLIENT_ENDPOINT_HOST', 'CLIENT_ENDPOINT_PORT', 'CLIENT_ENDPOINT_REQUIRED')) {
        Assert-True -Condition (-not (Test-EnvDeclared -Text $main -Name $name)) -Because "podip 不写 $name"
    }
}

Test-Case '端到端 infra-up external + deployment:battle 是 hostPort Deployment(只有 20000 占 hostPort),打 D86 警告,清理 Fleet 形态' {
    Assert-Equal -Expected 0 -Actual $HostPortRun.ExitCode -Because "external + deployment 的 DryRun 必须成功。输出: $($HostPortRun.Output)"
    $out = $HostPortRun.Output
    $battle = Select-ManifestDoc -Output $out -Kind 'Deployment' -Name 'battle'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($battle, 'hostPort:').Count) -Because '只有客户端口占 hostPort'
    Assert-Match -Text $battle -Pattern 'containerPort: 20000\s+hostPort: 20000\s+name: rpc' -Because 'D86'
    Assert-Match -Text $battle -Pattern 'maxSurge: 0\s+maxUnavailable: 1' -Because '同一节点新旧 Pod 抢 hostPort 会卡 Pending'
    $main = Get-MainContainerSection -Doc $battle
    foreach ($pair in @(@('CLIENT_ENDPOINT_SOURCE', 'static'), @('CLIENT_ENDPOINT_HOST', '$(HOST_IP)'), @('CLIENT_ENDPOINT_PORT', '20000'), @('CLIENT_ENDPOINT_REQUIRED', '1'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-EnvLiteral -Text $main -Name $pair[0]) -Because "battle hostPort env $($pair[0])"
    }
    Assert-Match -Text $out -Pattern 'battle 以 hostPort 运行,每个节点只能跑 1 个副本' -Because 'external + deployment 只用于验证与回退,要警告(D86)'
    Assert-Match -Text $out -Pattern "\[dry-run\] kubectl delete fleets\.agones\.dev battle -n $TestInfraNs --ignore-not-found" -Because '切回 Deployment 要删 Fleet 形态'
    Assert-Match -Text $out -Pattern "rollout status deployment/battle -n $TestInfraNs" -Because 'Deployment 形态照常等 rollout'
    Assert-NotMatch -Text $battle -Pattern $D92Pattern -Because 'D92:新生成器只写 RPC_PORT'
}

Test-Case '全部 DryRun 输出:gRPC 口从不进 hostPort / nodePort / Agones ports,不开 hostNetwork;新生成器的工作负载不出现 D92 身份 env' {
    $runs = [ordered]@{
        'podip zone-up' = $PodipRun; 'podip 单副本' = $PodipSingleRun; 'podip infra-up' = $PodipInfraRun; 'external zone-up' = $ExternalRun
        'external 小写参数' = $ExternalLowerRun; 'external 警告' = $ExternalWarnRun; 'RequireClientEndpoint false' = $RequireFalseRun
        'agones + external' = $FleetExternalRun; 'agones + podip' = $FleetPodipRun; 'hostPort' = $HostPortRun
    }
    foreach ($label in $runs.Keys) {
        Assert-Equal -Expected 0 -Actual $runs[$label].ExitCode -Because "$label 的 DryRun 必须成功,否则下面的扫描是空转"
        Assert-GrpcNeverExposed -Output $runs[$label].Output -Label $label
    }
    $newGenerated = @(
        (Select-ManifestDoc -Output $ExternalRun.Output -Kind 'StatefulSet' -Name 'gate'),
        (Select-ManifestDoc -Output $FleetExternalRun.Output -Kind 'Fleet' -Name 'battle'),
        (Select-ManifestDoc -Output $FleetPodipRun.Output -Kind 'Fleet' -Name 'battle'),
        (Select-ManifestDoc -Output $HostPortRun.Output -Kind 'Deployment' -Name 'battle')) + @(Find-ManifestDocs -Output $ExternalRun.Output -Kind 'Service' | Where-Object { $_ -match 'gate-ordinal|gate-headless' })
    Assert-True -Condition (@($newGenerated | Where-Object { $_ }).Count -ge 7) -Because '新生成器的清单都要在输出里(StatefulSet、2 份 Fleet、hostPort Deployment、headless、gate-0 / gate-1)'
    foreach ($doc in $newGenerated) {
        Assert-NotMatch -Text $doc -Pattern $D92Pattern -Because 'D92:新变量只用 CLIENT_ENDPOINT_* / HOST_IP / POD_NAME'
    }
}

Test-Case '端到端负向:preflight 一次报全,并在任何 kubectl(含删除旧形态)之前拒绝' {
    $zoneUpExternal = $ZoneUpArgs + @('-SkipGoSvc', '-ClientEntryMode', 'external')
    $cases = @(
        @{ Label = 'ClusterIP + 带 scheme 的主机 + Ingress 没有 trusted proxies'
            Args = $zoneUpExternal + @('-GateServiceType', 'ClusterIP', '-ClientPublicHost', 'http://1.2.3.4', '-JavaSvcRegistry', 'registry.invalid/test', '-GatewayIngressHost', 'play.example.com')
            Patterns = @('集群外入口预检失败\(3 项\)', '要求 -GateServiceType 为 NodePort 或 LoadBalancer', '-ClientPublicHost 只能是裸主机名或 IPv4 字面量', '配置了 -GatewayIngressHost 却没有 -GatewayTrustedProxies') },
        @{ Label = 'LoadBalancer 没有模板'
            Args = $zoneUpExternal + @('-SkipJavaSvc', '-GateServiceType', 'LoadBalancer')
            Patterns = @('集群外入口预检失败\(1 项\)', 'LoadBalancer 需要 -GateClientHostTemplate') },
        @{ Label = '2 副本模板不含 {ordinal} + nodePort 越界'
            Args = $zoneUpExternal + @('-SkipJavaSvc', '-GateClientHostTemplate', 'gate.example.com', '-GateNodePortBase', '32767')
            Patterns = @('集群外入口预检失败\(2 项\)', "zone ${TestZone}:gate 副本数 2 > 1,-GateClientHostTemplate 必须含 \{ordinal\}", '\[32767-32768\] 越界') }
    )
    foreach ($case in $cases) {
        $run = Invoke-EntryDryRun -Arguments $case.Args
        Assert-RefusedBeforeAnyKubectl -Run $run -Patterns $case.Patterns -Label $case.Label
    }
}

Test-Case '端到端负向:all-up 多 zone —— 有 zone 没显式写 gateNodePortBase、各 zone 段重叠,一次报全并在任何 kubectl 之前拒绝' {
    $dir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-client-entry-zones-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $dir | Out-Null
    try {
        $zonesPath = Join-Path $dir 'zones.json'
        $zonesJson = [ordered]@{ zones = @(
            [ordered]@{ name = 'ce-a'; zoneId = 201; gateNodePortBase = 30000; replicas = [ordered]@{ gate = 2 } },
            [ordered]@{ name = 'ce-b'; zoneId = 202; gateNodePortBase = 30001; replicas = [ordered]@{ gate = 2 } },
            [ordered]@{ name = 'ce-c'; zoneId = 203; replicas = [ordered]@{ gate = 2 } }) }
        Set-Content -LiteralPath $zonesPath -Encoding utf8 -Value ($zonesJson | ConvertTo-Json -Depth 5)
        $run = Invoke-EntryDryRun -Arguments @('-Command', 'all-up', '-SkipInfra', '-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $zonesPath,
            '-NodeImage', $NodeImageRef, '-ClientEntryMode', 'external', '-ClientPublicHost', '127.0.0.1')
        # ce-c 没写 base,落回 -GateNodePortBase 默认 30000:与 ce-a、ce-b 都重叠。
        Assert-RefusedBeforeAnyKubectl -Run $run -Label 'all-up 多 zone nodePort 段' -Patterns @(
            '集群外入口预检失败\(4 项\)', '显式写 gateNodePortBase\(缺:ce-c\)',
            'zone ce-a 与 zone ce-b 的 gate nodePort 段重叠', 'zone ce-a 与 zone ce-c 的 gate nodePort 段重叠', 'zone ce-b 与 zone ce-c 的 gate nodePort 段重叠')
    }
    finally {
        Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

$DevPassword = 'contract-dev-pw-Zq7tXk2w'
$DevPasswordRun = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc', '-LoginDevPasswordAuth')) -Env @{ MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = $DevPassword }

Test-Case '端到端 -LoginDevPasswordAuth(dev):login.yaml 写 DevPasswordAuth,口令经 Secret 的 secretKeyRef 注入,输出里没有口令明文' {
    Assert-Equal -Expected 0 -Actual $DevPasswordRun.ExitCode -Because "dev 档开开关并给了口令必须成功。输出: $($DevPasswordRun.Output)"
    $out = $DevPasswordRun.Output
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestDoc -Output $out -Kind 'ConfigMap' -Name 'go-svc-login-config')
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.DevPasswordAuth.Enabled') -Because '开关打开'
    Assert-Equal -Expected 'LOGIN_DEV_PASSWORD_SHARED_SECRET' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.DevPasswordAuth.SharedSecretEnv') -Because 'ConfigMap 只写环境变量名'
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Mode') -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Mode') -Because 'DevPasswordAuth 只在 go-zero Mode 为 dev / test 时生效,否则 login 启动即 panic'
    $login = Select-ManifestDoc -Output $out -Kind 'Deployment' -Name 'login'
    Assert-Match -Text $login -Pattern '- name: LOGIN_DEV_PASSWORD_SHARED_SECRET\s+valueFrom:\s+secretKeyRef:\s+name: login-dev-password\s+key: shared-secret\s' -Because '口令只经 Secret 进容器'
    Assert-NotMatch -Text $login -Pattern 'optional:\s*true' -Because 'Secret 缺失时要卡在 CreateContainerConfigError,不能带着空口令起来'
    $secretLine = "[dry-run] kubectl apply -n $TestZoneNs -f -   # Secret login-dev-password(key shared-secret,值已隐去)"
    $secretAt = $out.IndexOf($secretLine)
    Assert-True -Condition ($secretAt -ge 0) -Because "DryRun 只打一行隐去值的 Secret 意图。输出: $out"
    Assert-True -Condition ($secretAt -lt $out.IndexOf('name: go-svc-login-config')) -Because 'Secret 必须先于引用它的 login 落地'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($DevPassword)) -Because '口令明文绝不进 DryRun 输出 / ConfigMap / 日志'
    Assert-NotMatch -Text $out -Pattern 'kubectl delete secret login-dev-password' -Because '开关打开时不删自己要用的 Secret'
}

Test-Case '端到端负向 -LoginDevPasswordAuth:非 dev 档在任何 kubectl 之前拒绝(给了口令也拒);dev 档没给口令同样在写操作之前拒绝' {
    $prod = Invoke-EntryDryRun -RefusedAtEntry -Arguments ($ZoneUpArgs + @('-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc', '-ReleaseProfile', 'prod', '-SkipPreflight', '-LoginDevPasswordAuth')) -Env @{ MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = $DevPassword }
    Assert-RefusedBeforeAnyKubectl -Run $prod -Patterns @('-LoginDevPasswordAuth 只允许 -ReleaseProfile dev\(当前 prod\)') -Label 'prod + -LoginDevPasswordAuth'
    Assert-NotMatch -Text $prod.Output -Pattern ([regex]::Escape($DevPassword)) -Because '拒绝路径同样不回显口令'
    $noSecret = Invoke-EntryDryRun -RefusedAtEntry -Arguments ($ZoneUpArgs + @('-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc', '-LoginDevPasswordAuth')) -Env @{ MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = '' }
    Assert-RefusedBeforeAnyKubectl -Run $noSecret -Patterns @('-LoginDevPasswordAuth 需要环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET') -Label 'dev 档缺口令'
}

# ─────────────────────────────────────────────────────────────────
# 3. 非 DryRun 的集群现状预检(假 kubectl):会踢人 / 会静默改写入口的情形在任何写操作之前拒绝
# ─────────────────────────────────────────────────────────────────

# 一次性驱动:子进程里先定义全局 function kubectl,再以非 DryRun 调 k8s_deploy.ps1。每次调用记一行 JSON
# (剥掉 --context / --kubeconfig 单独记;-f <临时文件> 记成 <file> 并记下文件里各文档的 kind),按规则表第一条匹配回应。
$FakeKubectlDriverSource = @'
#requires -Version 7
param(
    [Parameter(Mandatory = $true)][string]$DeployScript,
    [Parameter(Mandatory = $true)][string]$ArgsPath,
    [Parameter(Mandatory = $true)][string]$RulesPath,
    [Parameter(Mandatory = $true)][string]$CallLogPath
)
$ErrorActionPreference = 'Stop'
$global:FakeKubectlRules = @(Get-Content -LiteralPath $RulesPath -Raw -Encoding utf8 | ConvertFrom-Json)
$global:FakeKubectlCallLog = $CallLogPath

function global:kubectl {
    $raw = @($args | ForEach-Object { [string]$_ })
    $rest = [System.Collections.Generic.List[string]]::new()
    $context = ''
    $kubeConfig = ''
    $kinds = @()
    for ($i = 0; $i -lt $raw.Count; $i++) {
        $arg = $raw[$i]
        if ($arg -ceq '--context' -and $i + 1 -lt $raw.Count) { $context = $raw[$i + 1]; $i++; continue }
        if ($arg -ceq '--kubeconfig' -and $i + 1 -lt $raw.Count) { $kubeConfig = $raw[$i + 1]; $i++; continue }
        if ($arg -ceq '-f' -and $i + 1 -lt $raw.Count -and $raw[$i + 1] -ne '-') {
            if (Test-Path -LiteralPath $raw[$i + 1]) {
                $kinds = @([regex]::Matches((Get-Content -LiteralPath $raw[$i + 1] -Raw), '(?m)^kind:[ \t]*(\S+)') | ForEach-Object { $_.Groups[1].Value })
            }
            $rest.Add('-f'); $rest.Add('<file>'); $i++
            continue
        }
        $rest.Add($arg)
    }
    $line = $rest -join ' '
    $record = [ordered]@{ Args = $line; Context = $context; KubeConfig = $kubeConfig; Kinds = @($kinds) }
    Add-Content -LiteralPath $global:FakeKubectlCallLog -Encoding utf8 -Value ($record | ConvertTo-Json -Compress -Depth 5)
    foreach ($rule in $global:FakeKubectlRules) {
        if ($line -match $rule.Pattern) {
            $global:LASTEXITCODE = [int]$rule.ExitCode
            if (-not [string]::IsNullOrEmpty([string]$rule.Stdout)) { return [string]$rule.Stdout }
            return
        }
    }
    $global:LASTEXITCODE = 0
}

# 安全垫:kubectl 没解析到假函数就绝不以非 DryRun 运行部署脚本。
$resolved = Get-Command kubectl -ErrorAction SilentlyContinue
if ($null -eq $resolved -or $resolved.CommandType -ne 'Function') {
    Write-Host 'FAKE-KUBECTL-NOT-ACTIVE: kubectl did not resolve to the fake function; refusing to run k8s_deploy.ps1 without -DryRun'
    exit 99
}
$deployArgs = Get-Content -LiteralPath $ArgsPath -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
try {
    & $DeployScript @deployArgs
}
catch {
    Write-Host "DEPLOY-FAILED: $($_.Exception.Message)"
    exit 1
}
exit 0
'@

$FakeKubectlDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-client-entry-fake-kubectl-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $FakeKubectlDir | Out-Null
$FakeKubectlDriver = Join-Path $FakeKubectlDir 'deploy_with_fake_kubectl.ps1'
Set-Content -LiteralPath $FakeKubectlDriver -Encoding utf8 -Value $FakeKubectlDriverSource
# 第二道安全垫:不存在的 context + 没有任何 context 的 kubeconfig。假函数万一没接管,真 kubectl 也只会报找不到 context。
$FakeKubeContext = 'mmorpg-contract-fake-context-never-exists'
$FakeKubeConfig = Join-Path $FakeKubectlDir 'kubeconfig-without-contexts.yaml'
Set-Content -LiteralPath $FakeKubeConfig -Encoding utf8 -Value @('apiVersion: v1', 'kind: Config', 'clusters: []', 'contexts: []', 'users: []', 'current-context: ""')
$script:FakeRunSeq = 0

<#
.SYNOPSIS
    以非 DryRun 跑 k8s_deploy.ps1,kubectl 由规则表回应。返回 { ExitCode; Output; Calls }。
    Rules:@(@{ Pattern = <对剥掉集群参数后的调用串的正则>; ExitCode = <int>; Stdout = <string> })。没命中的调用:退出码 0、无输出
    (get --ignore-not-found 即"不存在")。-KubeContext / -KubeConfig / -NodeImage 由本函数补上。
#>
function Invoke-DeployWithFakeKubectl {
    param(
        [Parameter(Mandatory = $true)][hashtable]$DeployArgs,
        [object[]]$Rules = @(),
        [hashtable]$Env = @{}
    )
    $command = [string]$DeployArgs.Command
    # 非 DryRun 的 podip zone-up / all-up 在集群现状预检(Assert-ClientEntryClusterState)里就会调 Get-GateObsoleteResources 的
    # podip 分支,早于 Apply-Zone;本节用例都要走到那道闸,没有"入口就被拒"的例外,所以这里不设 -RefusedAtEntry。
    if ($command -in @('zone-up', 'all-up') -and $DeployArgs.ClientEntryMode -ne 'external' -and $LibCallDefects.Count -gt 0) {
        return [pscustomobject]@{ ExitCode = -1; Output = "未启动子进程(会挂在缺必填参数的交互提示上):$($LibCallDefects -join '; ')"; Calls = @() }
    }
    $script:FakeRunSeq++
    $prefix = Join-Path $FakeKubectlDir ('run{0}' -f $script:FakeRunSeq)
    $all = @{} + $DeployArgs
    $all.NodeImage = $NodeImageRef
    $all.KubeContext = $FakeKubeContext
    $all.KubeConfig = $FakeKubeConfig
    Set-Content -LiteralPath "$prefix.args.json" -Encoding utf8 -Value ($all | ConvertTo-Json -Depth 5)
    Set-Content -LiteralPath "$prefix.rules.json" -Encoding utf8 -Value (ConvertTo-Json -InputObject @($Rules) -Depth 5)
    $run = Invoke-CapturedPwsh -ScriptPath $FakeKubectlDriver -Env $Env -Arguments @(
        '-DeployScript', (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), '-ArgsPath', "$prefix.args.json",
        '-RulesPath', "$prefix.rules.json", '-CallLogPath', "$prefix.calls.jsonl")
    $calls = @()
    if (Test-Path -LiteralPath "$prefix.calls.jsonl") {
        $calls = @(Get-Content -LiteralPath "$prefix.calls.jsonl" -Encoding utf8 | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    }
    return [pscustomobject]@{ ExitCode = $run.ExitCode; Output = $run.Output; Calls = $calls }
}

# 拒绝时零写操作:只允许只读调用(get)与服务端预演(--dry-run=server);-AllowedApplyKinds 放行指定 kind 的 apply
# (例如 zone-up 在服务端预演之前先 apply Namespace)。每次调用都必须显式带上本次给的 context 与 kubeconfig。
function Assert-NoClusterWrites {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Label,
        [string[]]$AllowedApplyKinds = @()
    )
    Assert-True -Condition (@($Run.Calls).Count -gt 0) -Because "${Label}:假 kubectl 一次都没被调(集群现状预检没跑,或 kubectl 没被顶替)。输出: $($Run.Output)"
    foreach ($call in @($Run.Calls)) {
        Assert-Equal -Expected $FakeKubeContext -Actual $call.Context -Because "${Label}:每次 kubectl 都必须显式带 --context,不落到本机默认 context($($call.Args))"
        Assert-Equal -Expected $FakeKubeConfig -Actual $call.KubeConfig -Because "${Label}:每次 kubectl 都必须显式带 --kubeconfig($($call.Args))"
        $verb = ($call.Args -split ' ')[0]
        if ($verb -eq 'get' -or $call.Args -match '(^| )--dry-run=server( |$)') { continue }
        $kinds = @($call.Kinds | Where-Object { $_ })
        $allowed = ($verb -eq 'apply') -and $kinds.Count -gt 0 -and @($kinds | Where-Object { $_ -notin $AllowedApplyKinds }).Count -eq 0
        Assert-True -Condition $allowed -Because "${Label}:拒绝时不得有任何写操作,实际调用了 kubectl $($call.Args)(kinds: $($kinds -join ','))"
    }
}

# 集群里现有工作负载的 kubectl -o json(只含预检会读的字段)。值为空的 env 由 API server 省略 value 字段。
$LiveGateStsJson = '{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"gate"},"spec":{"template":{"spec":{"containers":[{"name":"gate","env":[' +
    '{"name":"POD_NAME","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}},{"name":"GATE_CLIENT_HOST_TEMPLATE"},' +
    '{"name":"CLIENT_PUBLIC_HOST","value":"127.0.0.1"},{"name":"GATE_CLIENT_PORT_MODE","value":"nodeport"},{"name":"GATE_NODE_PORT_BASE","value":"30000"}]}]}}}}'
$LiveBattleStaticJson = '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"battle"},"spec":{"template":{"spec":{"containers":[{"name":"battle","env":[' +
    '{"name":"CLIENT_ENDPOINT_SOURCE","value":"static"},{"name":"CLIENT_ENDPOINT_HOST","value":"$(HOST_IP)"}]}]}}}}'
$AgonesPresentJson = '{"kind":"APIResourceList","groupVersion":"agones.dev/v1","resources":[{"name":"fleets"},{"name":"gameservers"}]}'
$AgonesAbsentJson = '{"kind":"APIResourceList","groupVersion":"agones.dev/v1","resources":[]}'

# 复制基础参数再覆盖(哈希表相加遇到重复键会直接报错)。
function New-FakeDeployArgs {
    param([Parameter(Mandatory = $true)][hashtable]$Base, [hashtable]$Extra = @{})
    $merged = @{} + $Base
    foreach ($key in $Extra.Keys) { $merged[$key] = $Extra[$key] }
    return $merged
}
$ZoneUpFake = @{ Command = 'zone-up'; ZoneName = $TestZone; ZoneId = 101; SkipGoSvc = $true; SkipJavaSvc = $true }
$InfraUpFake = @{ Command = 'infra-up'; SkipGoSvc = $true; SkipJavaSvc = $true; InfraNamespace = $TestInfraNs }
$ExternalZoneFake = New-FakeDeployArgs -Base $ZoneUpFake -Extra @{ ClientEntryMode = 'external'; ClientPublicHost = '127.0.0.1' }
$IngressZoneFake = New-FakeDeployArgs -Base $ExternalZoneFake -Extra @{ SkipJavaSvc = $false; JavaSvcRegistry = 'registry.invalid/test' }
$IngressRules = @(@{ Pattern = '^get ingress gateway '; ExitCode = 0; Stdout = 'ingress.networking.k8s.io/gateway' })
$IngressPatterns = @('集群现状预检失败\(1 项\)', '已有 Ingress gateway,本次部署 gateway 却没给 -GatewayTrustedProxies', "kubectl -n $TestZoneNs delete ingress gateway")

$ClusterStateCases = @(
    @{ Label = 'battle 换 kind:集群里是 Deployment,本次 -BattleOrchestrator agones 却没确认'
        DeployArgs = (New-FakeDeployArgs -Base $InfraUpFake -Extra @{ BattleOrchestrator = 'agones' }); Env = $BattleTestEnv
        Rules = @(@{ Pattern = '^get --raw /apis/agones\.dev/v1$'; ExitCode = 0; Stdout = $AgonesPresentJson }, @{ Pattern = '^get deployment battle .*-o name$'; ExitCode = 0; Stdout = 'deployment.apps/battle' })
        Patterns = @('集群现状预检失败\(1 项\),未做任何写操作', "当前集群是 deployment battle\(deployment\.apps/battle,namespace=$TestInfraNs\)", '要保持现状请显式传 -BattleOrchestrator deployment;确认要切换请加 -AllowDisruptiveSwitch') },
    @{ Label = 'agones 但集群没装 Agones(发现文档里没有 fleets)'
        DeployArgs = (New-FakeDeployArgs -Base $InfraUpFake -Extra @{ BattleOrchestrator = 'agones' }); Env = $BattleTestEnv
        Rules = @(@{ Pattern = '^get --raw /apis/agones\.dev/v1$'; ExitCode = 0; Stdout = $AgonesAbsentJson })
        Patterns = @('集群外入口预检失败\(1 项\)', '需要集群已安装 Agones:找不到 CRD fleets\.agones\.dev') },
    @{ Label = 'battle 同 kind 换入口:现有 Deployment 以 external 运行,本次漏传 -ClientEntryMode'
        DeployArgs = $InfraUpFake; Env = $BattleTestEnv
        Rules = @(@{ Pattern = '^get --raw /apis/agones\.dev/v1$'; ExitCode = 0; Stdout = $AgonesAbsentJson }, @{ Pattern = '^get deployment battle .*-o json$'; ExitCode = 0; Stdout = $LiveBattleStaticJson })
        Patterns = @('集群现状预检失败\(1 项\)', "当前 deployment battle 以 -ClientEntryMode external 运行\(CLIENT_ENDPOINT_SOURCE='static'\),本次参数会改成 podip", '要保持现状请显式传 -ClientEntryMode external;') },
    @{ Label = 'gate 换 kind:集群里是 Deployment,本次 external 却没确认'
        DeployArgs = $ExternalZoneFake
        Rules = @(@{ Pattern = '^get deployment gate .*-o name$'; ExitCode = 0; Stdout = 'deployment.apps/gate' })
        Patterns = @('集群现状预检失败\(1 项\)', "当前集群是 deployment gate\(deployment\.apps/gate,namespace=$TestZoneNs\)", '要保持现状请显式传 -ClientEntryMode podip;') },
    @{ Label = 'gate 换 kind:集群里是 StatefulSet,本次漏传 -ClientEntryMode(落回 podip)'
        DeployArgs = $ZoneUpFake
        Rules = @(@{ Pattern = '^get statefulset gate .*-o name$'; ExitCode = 0; Stdout = 'statefulset.apps/gate' })
        Patterns = @('集群现状预检失败\(1 项\)', "当前集群是 statefulset gate\(statefulset\.apps/gate,namespace=$TestZoneNs\)", '要保持现状请显式传 -ClientEntryMode external;') },
    @{ Label = 'external gate 地址来源漂移:换了 nodePort 起点与主机(StatefulSet 是 OnDelete,现有 Pod 仍报旧地址)'
        DeployArgs = (New-FakeDeployArgs -Base $ExternalZoneFake -Extra @{ ClientPublicHost = '10.1.2.3'; GateNodePortBase = 30010 })
        Rules = @(@{ Pattern = '^get statefulset gate .*-o json$'; ExitCode = 0; Stdout = $LiveGateStsJson })
        Patterns = @('集群现状预检失败\(2 项\)', 'gate 自报端口将从 nodePort 30000\+序号\(NodePort\) 改为 nodePort 30010\+序号\(NodePort\)',
            '要保持现状请显式传 -GateServiceType NodePort -GateNodePortBase 30000;', "gate 自报主机将从 主机 '127\.0\.0\.1' 改为 主机 '10\.1\.2\.3'",
            '要保持现状请显式传 -ClientPublicHost 127\.0\.0\.1 且不传 -GateClientHostTemplate;') },
    @{ Label = '地址来源与集群一致,但 nodePort 已被别处占用:服务端预演拒绝,停在删除旧形态之前'
        DeployArgs = (New-FakeDeployArgs -Base $ExternalZoneFake -Extra @{ GateNodePortBase = 30000 })
        Rules = @(@{ Pattern = '^get statefulset gate .*-o json$'; ExitCode = 0; Stdout = $LiveGateStsJson },
            @{ Pattern = '(^| )--dry-run=server( |$)'; ExitCode = 1; Stdout = 'The Service "gate-0" is invalid: spec.ports[0].nodePort: Invalid value: 30000: provided port is already allocated' })
        Patterns = @('服务端预演\(--dry-run=server\)被集群拒绝,未删除、未改动任何资源', '换一个不重叠的 gateNodePortBase')
        NotPatterns = @('集群现状预检失败'); AllowedApplyKinds = @('Namespace'); ServerDryRunKinds = @('Service', 'StatefulSet', 'PodDisruptionBudget') },
    @{ Label = '已有 Ingress gateway,本次部署 gateway 却没给 trusted proxies'
        DeployArgs = $IngressZoneFake; Rules = $IngressRules; Patterns = $IngressPatterns },
    @{ Label = '同上且给了 -AllowDisruptiveSwitch(限流桶问题与切换无关,不受它豁免)'
        DeployArgs = (New-FakeDeployArgs -Base $IngressZoneFake -Extra @{ AllowDisruptiveSwitch = $true }); Rules = $IngressRules; Patterns = $IngressPatterns }
)

foreach ($case in $ClusterStateCases) {
    Test-Case "非 DryRun 负向:$($case.Label)" {
        $caseEnv = if ($case.ContainsKey('Env')) { $case.Env } else { @{} }
        $run = Invoke-DeployWithFakeKubectl -DeployArgs $case.DeployArgs -Rules $case.Rules -Env $caseEnv
        Assert-NotMatch -Text $run.Output -Pattern 'FAKE-KUBECTL-NOT-ACTIVE' -Because '假 kubectl 必须接管,否则本用例不该继续'
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "必须拒绝。输出: $($run.Output)"
        foreach ($pattern in $case.Patterns) {
            Assert-Match -Text $run.Output -Pattern $pattern -Because '错误文本要写明集群现状与保持现状 / 确认切换各要传什么'
        }
        foreach ($pattern in @($case.NotPatterns | Where-Object { $_ })) {
            Assert-NotMatch -Text $run.Output -Pattern $pattern -Because '失败必须来自本用例针对的那一道闸'
        }
        Assert-NoClusterWrites -Run $run -Label $case.Label -AllowedApplyKinds @($case.AllowedApplyKinds | Where-Object { $_ })
        if ($case.ContainsKey('ServerDryRunKinds')) {
            $probe = @($run.Calls | Where-Object { $_.Args -match '(^| )--dry-run=server( |$)' })
            Assert-Equal -Expected 1 -Actual $probe.Count -Because '删除旧形态之前恰好做一次服务端预演'
            foreach ($kind in $case.ServerDryRunKinds) {
                Assert-True -Condition (@($probe[0].Kinds) -contains $kind) -Because "服务端预演要带上整套 external gate 清单(缺 $kind)"
            }
        }
    }
}

# gateway 管理面口令(MMORPG_GATEWAY_ADMIN_API_KEY → Secret gateway-admin-api-key → 容器 env ADMIN_APIKEY):
# 非 dev 档缺失 / 占位 / 过短,在任何集群写操作之前拒绝(k8s_deploy.ps1 Initialize-InjectedSecrets → Resolve-InjectedSecret)。
# 走假 kubectl 的非 DryRun:拒绝落在集群现状预检(只读)之后、任何写操作之前;驱动用 Write-Host 打出完整异常原文,
# 不经错误视图折行,可以整句断言。其余生产密钥给齐合法值,保证拒绝只能来自这一把。
$ProdSecretEnv = @{
    MMORPG_GATE_TOKEN_SECRET    = 'contract-ce-gate-token-0123456789abcdef0123'
    MMORPG_INTERNAL_AUTH_SECRET = 'contract-ce-internal-auth-0123456789abcdef'
    MMORPG_MYSQL_USER           = 'contract_ce_app'
    MMORPG_MYSQL_PASSWORD       = 'contract-ce-mysql-pw-01'
    MMORPG_REDIS_PASSWORD       = 'contract-ce-redis-pw-01'
    MMORPG_GATEWAY_DB_USER      = 'contract_ce_gateway'
    MMORPG_GATEWAY_DB_PASSWORD  = 'contract-ce-gwdb-pw-01'
}
$AdminKeyPurpose = 'Java Gateway 管理面口令(X-Admin-Key / admin.api-key)'
$AdminKeyCases = @(
    @{ Label = 'prod 未设置'; Profile = 'prod'; Value = ''
        Expected = "ReleaseProfile=prod 要求从环境变量 MMORPG_GATEWAY_ADMIN_API_KEY 注入 $AdminKeyPurpose,当前未设置。生成器不会再把占位常量写进生产 ConfigMap。" },
    @{ Label = 'prod 仍是 application.yaml 里公开的默认值'; Profile = 'prod'; Value = 'change-me-in-production'
        Expected = "环境变量 MMORPG_GATEWAY_ADMIN_API_KEY($AdminKeyPurpose)的值仍是占位串,拒绝用于 ReleaseProfile=prod。" },
    @{ Label = 'staging 短于 32 位'; Profile = 'staging'; Value = 'contract-ce-16ch'; NoEcho = $true
        Expected = "环境变量 MMORPG_GATEWAY_ADMIN_API_KEY($AdminKeyPurpose)长度 16 < 要求的 32,拒绝用于 ReleaseProfile=staging。" }
)
foreach ($case in $AdminKeyCases) {
    Test-Case "非 DryRun 负向 gateway 管理面口令:$($case.Label) —— 在任何写操作之前拒绝,报错点名变量" {
        $caseEnv = @{} + $ProdSecretEnv
        $caseEnv['MMORPG_GATEWAY_ADMIN_API_KEY'] = $case.Value
        $deployArgs = New-FakeDeployArgs -Base $ZoneUpFake -Extra @{
            SkipJavaSvc = $false; JavaSvcRegistry = 'registry.invalid/test'; ReleaseProfile = $case.Profile; SkipPreflight = $true }
        $run = Invoke-DeployWithFakeKubectl -DeployArgs $deployArgs -Env $caseEnv
        Assert-NotMatch -Text $run.Output -Pattern 'FAKE-KUBECTL-NOT-ACTIVE' -Because '假 kubectl 必须接管,否则本用例不该继续'
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "必须拒绝。输出: $($run.Output)"
        Assert-True -Condition (([string]$run.Output).Contains("DEPLOY-FAILED: $($case.Expected)")) -Because "报错原文要点名变量与档位(Resolve-InjectedSecret)。输出: $($run.Output)"
        Assert-NoClusterWrites -Run $run -Label $case.Label
        if ($case.NoEcho) {
            Assert-NotMatch -Text $run.Output -Pattern ([regex]::Escape($case.Value)) -Because '拒绝路径不回显口令'
        }
    }
}

Test-Case '端到端 gateway 管理面口令只在本次部署 gateway 时解析:prod 档不带 Java 服务的 zone-up 不因缺它被拒' {
    $env2 = @{} + $ProdSecretEnv
    $env2['MMORPG_GATEWAY_ADMIN_API_KEY'] = ''
    $run = Invoke-EntryDryRun -Arguments ($ZoneUpArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-ReleaseProfile', 'prod', '-SkipPreflight')) -Env $env2
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "infra-up 与不带 Java 服务的 zone-up 不该被管理面口令阻断。输出: $($run.Output)"
    Assert-NotMatch -Text $run.Output -Pattern 'MMORPG_GATEWAY_ADMIN_API_KEY|gateway-admin-api-key' -Because '不部署 gateway 就不解析、不生成管理面口令 Secret'
}

# ─────────────────────────────────────────────────────────────────
# 4. 包装入口 splat 契约(AST):dev_tools / k8s_image 转给下游的每个键都必须是下游 param 块里声明过的参数
# ─────────────────────────────────────────────────────────────────

# 下游是 advanced script(有 [Parameter()] / CmdletBinding)时,splat 进一个未声明的具名参数会在参数绑定阶段直接报错,
# 整条命令失败 —— k8s_image 曾因此让 k8s-image-* / k8s-release-* 全部起不来(dev_tools.ps1 Invoke-K8sImage 注释)。
function Get-ScriptParamContract {
    param([Parameter(Mandatory = $true)][string]$Path)
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($Path, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "$Path 解析失败:$($parseErrors[0].Message)" }
    if ($null -eq $ast.ParamBlock) { throw "$Path 没有脚本级 param 块" }
    $types = [ordered]@{}
    foreach ($p in $ast.ParamBlock.Parameters) { $types[$p.Name.VariablePath.UserPath] = $p.StaticType.Name }
    $advanced = (@($ast.ParamBlock.Attributes | Where-Object { $_.TypeName.Name -eq 'CmdletBinding' }).Count -gt 0) -or
        (@($ast.ParamBlock.Parameters | ForEach-Object { $_.Attributes } | Where-Object { $_ -is [System.Management.Automation.Language.AttributeAst] -and $_.TypeName.Name -eq 'Parameter' }).Count -gt 0)
    return [pscustomobject]@{ Names = @($types.Keys); Types = $types; Advanced = $advanced }
}

# 包装函数转给下游的键:函数体内哈希字面量的键 + `$args.X = …` / `$args['X'] = …` 的键(同 scratch 审计脚本口径);
# 以及它 Join-Path 拼出的下游脚本名、是否以 `& $scriptPath @args` 调用。
function Get-WrapperSplatContract {
    param(
        [Parameter(Mandatory = $true)][System.Management.Automation.Language.Ast]$ScriptAst,
        [Parameter(Mandatory = $true)][string]$FunctionName
    )
    $fn = $ScriptAst.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $FunctionName }, $true)
    if ($null -eq $fn) { throw "找不到函数 $FunctionName(契约测试的前提被改掉了)" }
    $keys = New-Object System.Collections.Generic.List[string]
    foreach ($ht in $fn.Body.FindAll({ param($n) $n -is [System.Management.Automation.Language.HashtableAst] }, $true)) {
        foreach ($pair in $ht.KeyValuePairs) {
            $keys.Add($(if ($pair.Item1 -is [System.Management.Automation.Language.StringConstantExpressionAst]) { $pair.Item1.Value } else { $pair.Item1.Extent.Text }))
        }
    }
    foreach ($assign in $fn.Body.FindAll({ param($n) $n -is [System.Management.Automation.Language.AssignmentStatementAst] }, $true)) {
        $left = $assign.Left
        if ($left -is [System.Management.Automation.Language.MemberExpressionAst] -and $left.Expression -is [System.Management.Automation.Language.VariableExpressionAst] -and
            $left.Expression.VariablePath.UserPath -eq 'args' -and $left.Member -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $keys.Add($left.Member.Value)
        }
        elseif ($left -is [System.Management.Automation.Language.IndexExpressionAst] -and $left.Target -is [System.Management.Automation.Language.VariableExpressionAst] -and
            $left.Target.VariablePath.UserPath -eq 'args' -and $left.Index -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $keys.Add($left.Index.Value)
        }
    }
    $text = $fn.Extent.Text
    $downstream = [regex]::Match($text, 'Join-Path\s+\$ScriptDir\s+"(?<f>[^"]+\.ps1)"')
    return [pscustomobject]@{
        Keys       = @($keys | Sort-Object -Unique)
        Downstream = $(if ($downstream.Success) { $downstream.Groups['f'].Value } else { '' })
        SplatsArgs = ($text -match '&\s*\$scriptPath\s+@args\b')
    }
}

Test-Case 'AST 契约自检:哈希字面量键、$args.X / $args[''X''] 赋值键都能取到,未声明的键会被点名(守卫不得空转)' {
    $synthetic = @'
function Invoke-Wrapper {
    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    $args = @{ Command = 'zone-up'; 'ZoneName' = 'z' }
    if ($x) { $args.NoSuchParam = $true }
    $args['AnotherBogus'] = 1
    & $scriptPath @args
}
'@
    $ast = [System.Management.Automation.Language.Parser]::ParseInput($synthetic, [ref]$null, [ref]$null)
    $contract = Get-WrapperSplatContract -ScriptAst $ast -FunctionName 'Invoke-Wrapper'
    Assert-Equal -Expected 'AnotherBogus,Command,NoSuchParam,ZoneName' -Actual ($contract.Keys -join ',') -Because '四种写法的键都要取到'
    Assert-Equal -Expected 'k8s_deploy.ps1' -Actual $contract.Downstream -Because '下游脚本名取自 Join-Path'
    Assert-True -Condition $contract.SplatsArgs -Because '识别 & $scriptPath @args'
    $deployNames = (Get-ScriptParamContract -Path (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1')).Names
    Assert-Equal -Expected 'AnotherBogus,NoSuchParam' -Actual (@($contract.Keys | Where-Object { $_ -notin $deployNames }) -join ',') -Because '未声明的键必须被点名'
}

# 三条包装链都必须透传的键。RequireClientEndpoint:k8s_image 链路只参与 k8s_deploy 的组合预检(k8s_image.ps1 对此告警);
# AllowDisruptiveSwitch:会踢人的切换只能由调用方显式确认,包装层漏传 = 维护窗口里切不过去。钉进清单,删掉任一处透传即红。
$ClientEntryWrapperKeys = @('ClientEntryMode', 'ClientPublicHost', 'GateClientHostTemplate', 'GateNodePortBase', 'GateExternalTrafficPolicy', 'BattleOrchestrator',
    'RequireClientEndpoint', 'AllowDisruptiveSwitch')
$WrapperPairs = @(
    @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sDeploy'; Downstream = 'k8s_deploy.ps1'
        Forwarded = $ClientEntryWrapperKeys + @('GatewayIngressHost', 'GatewayIngressClassName', 'GatewayIngressTlsSecret', 'GatewayTrustedProxies', 'LoginDevPasswordAuth', 'GateRouterMode') },
    @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sImage'; Downstream = 'k8s_image.ps1'; Forwarded = $ClientEntryWrapperKeys + @('GateRouterMode') },
    @{ Source = 'k8s_image.ps1'; Function = 'Invoke-K8sDeploy'; Downstream = 'k8s_deploy.ps1'; Forwarded = $ClientEntryWrapperKeys + @('GateRouterMode') }
)
foreach ($pair in $WrapperPairs) {
    Test-Case "AST 契约:$($pair.Source)::$($pair.Function) → $($pair.Downstream) 的每个 splat 键都是下游 param 块声明过的参数" {
        $parseErrors = $null
        $sourceAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) $pair.Source), [ref]$null, [ref]$parseErrors)
        Assert-Equal -Expected 0 -Actual $parseErrors.Count -Because "$($pair.Source) 必须能解析"
        $contract = Get-WrapperSplatContract -ScriptAst $sourceAst -FunctionName $pair.Function
        Assert-Equal -Expected $pair.Downstream -Actual $contract.Downstream -Because '被测的包装函数确实转给这个下游脚本'
        Assert-True -Condition $contract.SplatsArgs -Because '包装函数以 & $scriptPath @args 转发'
        $downstream = Get-ScriptParamContract -Path (Join-Path (Get-ToolsScriptsDir) $pair.Downstream)
        Assert-True -Condition $downstream.Advanced -Because '下游是 advanced script,未声明的具名参数会直接报错(这正是本契约要拦的)'
        Assert-True -Condition ($contract.Keys -contains 'Command') -Because "取到的键必须包含 Command,否则取键逻辑空转(实际: $($contract.Keys -join ','))"
        $missing = @($contract.Keys | Where-Object { $_ -notin $downstream.Names })
        Assert-Equal -Expected '' -Actual ($missing -join ',') -Because "这些键 $($pair.Downstream) 没有声明,调用时参数绑定直接失败"
        $notForwarded = @($pair.Forwarded | Where-Object { $_ -notin $contract.Keys })
        Assert-Equal -Expected '' -Actual ($notForwarded -join ',') -Because '集群外入口参数按「留空不透传、给了才透传」接到下游(2c 补充:参数名以 ingress_final §3 为准)'
    }
}

Test-Case 'k8s_deploy.ps1 的集群外入口参数名与类型(包装层与文档按这些名字对齐)' {
    $types = (Get-ScriptParamContract -Path (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1')).Types
    $expected = [ordered]@{
        ClientEntryMode = 'String'; RequireClientEndpoint = 'String'; ClientPublicHost = 'String'; GateClientHostTemplate = 'String'; GateNodePortBase = 'Int32'
        GateExternalTrafficPolicy = 'String'; BattleOrchestrator = 'String'; GatewayIngressHost = 'String'; GatewayIngressClassName = 'String'
        GatewayIngressTlsSecret = 'String'; GatewayTrustedProxies = 'String'; LoginDevPasswordAuth = 'SwitchParameter'; AllowDisruptiveSwitch = 'SwitchParameter'
    }
    foreach ($name in $expected.Keys) {
        Assert-Equal -Expected $expected[$name] -Actual $types[$name] -Because "k8s_deploy.ps1 -$name 的类型(GatewayTrustedProxies 为逗号分隔字符串,开关类为 switch)"
    }
}

Remove-Item -LiteralPath $FakeKubectlDir -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s client entry contract")
