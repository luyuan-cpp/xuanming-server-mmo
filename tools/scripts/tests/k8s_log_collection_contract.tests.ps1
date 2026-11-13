#requires -Version 7
<#
.SYNOPSIS
    K8s 日志采集的契约测试(docs/design/distributed-tracing.md §9.2、§10.2,docs/ops/grafana-loki-local-logs.md §6.11)。

.DESCRIPTION
    钉住:
      1. 规则一致:C++ 日志 sidecar 取 trace / 玩家的两条正则,容器日志 DaemonSet 的 Go / Java 每段 stage.match 与 Java 的多行合并,
         以及 C++ 容器 stdout / stderr 的 gRPC / librdkafka 两段,都与本机 deploy/observability/alloy/config.alloy 逐段相同。
         集群一侧只允许去掉取时间的几步(时间用 CRI 时间戳);本机只取时间的 go-redis 那段、不进 stdout 的 muduo 行与
         Windows 才有的断言行不搬;集群另有一段未捕获异常;DaemonSet 认 C++ 节点用的 app 名与部署脚本里的工作负载名对得上;
      2. trace_id / span_id / player_id 只进结构化元数据,三份 Alloy 配置里都不做标签;
      3. DaemonSet 清单的渲染(纯函数):占位全部换掉,指纹确定、对输入敏感、与行尾无关;命名空间名不合法、缺指纹占位、剩下未知占位都拒绝;
      4. 推送地址的校验:带凭据、引号、空白、非 http(s) 都拒绝,报错不回显地址;
      5. 部署判定:只有 infra-up 与不带 -SkipInfra 的 all-up 部署,-NoCppLogSidecar 两路一起关;只有 Apply-Infra 渲染与部署它;
      6. DryRun:单副本与 3 副本 Loki 下 DaemonSet 都推 loki.<infra>:3100;给了 -LokiPushUrl 推那个地址;关了 sidecar 不渲染;
         带凭据的地址在任何清单之前拒绝;infra-down 按名字删 ClusterRoleBinding。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

    设了环境变量 MMORPG_DUMP_ALLOY_CONFIG_DIR 时,另按部署脚本的缺省命名空间(mmorpg-infra / mmorpg-zone)渲染一份,写进那个目录:
    pod-log-collector.yaml(整份清单)、pod-log-collector.alloy、cpp-log-sidecar.alloy。给 alloy fmt 与 kind 上的端到端核对用
    (docs/PROGRESS.md 2026-11-12「分布式日志阶段二」条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_log_collection_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== k8s log collection contract tests ==='

$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors[0].Message)" }
$deployFunctions = @('Get-CppLogSidecarConfigAlloy', 'New-PodLogCollectorManifestText', 'Assert-LokiPushUrlSafe', 'Test-PodLogCollectorDeployed')
foreach ($node in $deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($deployFunctions -contains $node.Name) { Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock() }
}
foreach ($name in $deployFunctions) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 $name" }
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Assert-SameText {
    param([AllowEmptyString()][string]$Expected, [AllowEmptyString()][string]$Actual, [Parameter(Mandatory = $true)][string]$Because)
    if ($Expected -cne $Actual) { throw "断言失败: $Because`n  期望: $Expected`n  实际: $Actual" }
}

function Read-RepoFileLf([string]$RelativePath) {
    return [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath)).Replace("`r`n", "`n")
}

# ──────────────────────────────────────────────
# 0. Alloy 配置的小解析
# ──────────────────────────────────────────────
# 去掉 // 行注释。反引号原样串与双引号串(含 \ 转义)里的内容原样保留:正则里有 //,注释里也有引号。
function Remove-AlloyComments {
    param([Parameter(Mandatory = $true)][string]$Text)
    $sb = [System.Text.StringBuilder]::new()
    $i = 0
    while ($i -lt $Text.Length) {
        $c = $Text[$i]
        if ($c -eq [char]'`') {
            $close = $Text.IndexOf([char]'`', $i + 1)
            if ($close -lt 0) { throw "Alloy 配置里有没闭合的反引号串(位置 $i)" }
            [void]$sb.Append($Text, $i, $close - $i + 1)
            $i = $close + 1
            continue
        }
        if ($c -eq [char]'"') {
            $j = $i + 1
            while ($j -lt $Text.Length -and $Text[$j] -ne [char]'"') {
                if ($Text[$j] -eq [char]'\') { $j++ }
                $j++
            }
            if ($j -ge $Text.Length) { throw "Alloy 配置里有没闭合的双引号串(位置 $i)" }
            [void]$sb.Append($Text, $i, $j - $i + 1)
            $i = $j + 1
            continue
        }
        if ($c -eq [char]'/' -and ($i + 1) -lt $Text.Length -and $Text[$i + 1] -eq [char]'/') {
            $newline = $Text.IndexOf([char]"`n", $i)
            if ($newline -lt 0) { break }
            $i = $newline
            continue
        }
        [void]$sb.Append($c)
        $i++
    }
    return $sb.ToString()
}

# 从 $OpenIndex(一个 '{')起找配对的 '}'。$Text 要先去掉注释;串里的花括号(正则的 \d{8}、LogQL 的 {job=...})不算。
function Find-AlloyBlockEnd {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][int]$OpenIndex)
    $depth = 0
    $i = $OpenIndex
    while ($i -lt $Text.Length) {
        $c = $Text[$i]
        if ($c -eq [char]'`') {
            $i = $Text.IndexOf([char]'`', $i + 1) + 1
            if ($i -le 0) { break }
            continue
        }
        if ($c -eq [char]'"') {
            $j = $i + 1
            while ($j -lt $Text.Length -and $Text[$j] -ne [char]'"') {
                if ($Text[$j] -eq [char]'\') { $j++ }
                $j++
            }
            $i = $j + 1
            continue
        }
        if ($c -eq [char]'{') { $depth++ }
        elseif ($c -eq [char]'}') {
            $depth--
            if ($depth -eq 0) { return $i }
        }
        $i++
    }
    throw "Alloy 配置的花括号不配对(从位置 $OpenIndex 起)"
}

# 顶层组件的正文(从组件名到配对的 '}'),例如 -Header 'loki.process "go_services"'。不是恰好一个就 throw。
function Get-AlloyComponent {
    param([Parameter(Mandatory = $true)][string]$Config, [Parameter(Mandatory = $true)][string]$Header)
    $found = @([regex]::Matches($Config, '(?m)^[ \t]*' + [regex]::Escape($Header) + '[ \t]*\{'))
    if ($found.Count -ne 1) { throw "Alloy 配置里 $Header 应当恰好 1 个,实际 $($found.Count) 个" }
    $open = $found[0].Index + $found[0].Length - 1
    return $Config.Substring($found[0].Index, (Find-AlloyBlockEnd -Text $Config -OpenIndex $open) - $found[0].Index + 1)
}

# 组件里依次出现的某种子块(例如 stage.match),按出现顺序逐块输出全文。调用方一律用 @(...) 收,一块时也是数组。
function Get-AlloySubBlocks {
    param([Parameter(Mandatory = $true)][string]$Block, [Parameter(Mandatory = $true)][string]$Name)
    $result = New-Object System.Collections.Generic.List[string]
    $pattern = [regex]::new([regex]::Escape($Name) + '\s*\{')
    $at = 0
    while ($true) {
        $m = $pattern.Match($Block, $at)
        if (-not $m.Success) { break }
        $end = Find-AlloyBlockEnd -Text $Block -OpenIndex ($m.Index + $m.Length - 1)
        $result.Add($Block.Substring($m.Index, $end - $m.Index + 1))
        $at = $end + 1
    }
    return $result.ToArray()
}

function Get-StageSelector {
    param([Parameter(Mandatory = $true)][string]$Stage)
    $m = [regex]::Match($Stage, '(?m)^\s*selector\s*=\s*(?<v>`[^`]*`|"(?:[^"\\]|\\.)*")\s*$')
    if (-not $m.Success) { throw "stage.match 里没有 selector: $Stage" }
    return $m.Groups['v'].Value
}

# 一段 stage.match 去掉取时间的部分后压成一行,用来逐段比较本机与集群。取时间的部分是:stage.timestamp 子块、stage.json 里的 ts 取值、
# 为拼时间而设的 stage.template(source 是 ts 或 *_ts,例如 gRPC 行补年份的 glog_ts)。
function ConvertTo-ComparableStage {
    param([Parameter(Mandatory = $true)][string]$Stage)
    $text = $Stage
    foreach ($name in @('stage.timestamp', 'stage.template')) {
        $at = 0
        while ($true) {
            $m = [regex]::new([regex]::Escape($name) + '\s*\{').Match($text, $at)
            if (-not $m.Success) { break }
            $end = Find-AlloyBlockEnd -Text $text -OpenIndex ($m.Index + $m.Length - 1)
            $block = $text.Substring($m.Index, $end - $m.Index + 1)
            if ($name -eq 'stage.timestamp' -or $block -match 'source\s*=\s*"(?:ts|[a-z_]+_ts)"') {
                $text = $text.Remove($m.Index, $block.Length)
                $at = $m.Index
            }
            else {
                $at = $end + 1
            }
        }
    }
    $text = [regex]::Replace($text, '(?m)^[ \t]*ts[ \t]*=[^\n]*,[ \t]*$', '')
    return ([regex]::Replace($text, '\s+', ' ')).Trim()
}

# 两份配置里同语言 loki.process 的 stage.match 逐段比较:选择器按出现顺序一一对应,同一段的正文(去掉取时间的部分)相同。
# -SkipSelectorPattern:本机有、集群按设计不搬的段(只取时间的 go-redis 那段;不进 stdout 的 muduo 行;Windows 才有的断言行)。
# -ClusterOnlySelectorPattern:集群有、本机没有的段(C++ 容器 stderr 上的未捕获异常)。
function Assert-StageParity {
    param(
        [Parameter(Mandatory = $true)][string]$Local,
        [Parameter(Mandatory = $true)][string]$Cluster,
        [Parameter(Mandatory = $true)][string]$Label,
        [string]$SkipSelectorPattern = '',
        [string]$ClusterOnlySelectorPattern = '',
        [int]$MinStages = 1
    )
    $localStages = @(Get-AlloySubBlocks -Block $Local -Name 'stage.match' | Where-Object { -not ($SkipSelectorPattern -and (Get-StageSelector -Stage $_) -match $SkipSelectorPattern) })
    $clusterStages = @(Get-AlloySubBlocks -Block $Cluster -Name 'stage.match' | Where-Object { -not ($ClusterOnlySelectorPattern -and (Get-StageSelector -Stage $_) -match $ClusterOnlySelectorPattern) })
    Assert-True -Condition ($localStages.Count -ge $MinStages) -Because "${Label}:本机配置里至少抽得到 $MinStages 段 stage.match(实际 $($localStages.Count));少了说明写法变了,本用例在空集上空转"
    Assert-SameText -Expected (($localStages | ForEach-Object { Get-StageSelector -Stage $_ }) -join "`n") `
        -Actual (($clusterStages | ForEach-Object { Get-StageSelector -Stage $_ }) -join "`n") `
        -Because "${Label}:两边的 stage.match 选择器(含先后)要一一对应"
    for ($i = 0; $i -lt $localStages.Count; $i++) {
        Assert-SameText -Expected (ConvertTo-ComparableStage -Stage $localStages[$i]) -Actual (ConvertTo-ComparableStage -Stage $clusterStages[$i]) `
            -Because "${Label}:第 $($i + 1) 段 stage.match 的正文要与本机相同(只允许去掉取时间的部分)"
    }
}

# 会成为 Loki 流标签的键:external_labels、stage.labels / stage.static_labels 的 values、discovery.relabel 的 target_label。
function Get-AlloyLabelKeys {
    param([Parameter(Mandatory = $true)][string]$Config)
    $keys = New-Object System.Collections.Generic.HashSet[string]
    $blocks = @([regex]::Matches($Config, '(?s)external_labels\s*=\s*\{(?<b>[^}]*)\}')) +
        @([regex]::Matches($Config, '(?s)stage\.(?:static_)?labels\s*\{\s*values\s*=\s*\{(?<b>[^}]*)\}'))
    foreach ($block in $blocks) {
        foreach ($m in [regex]::Matches($block.Groups['b'].Value, '(?<k>[A-Za-z_][A-Za-z0-9_]*)\s*=')) { [void]$keys.Add($m.Groups['k'].Value) }
    }
    foreach ($m in [regex]::Matches($Config, 'target_label\s*=\s*"(?<k>[^"]+)"')) { [void]$keys.Add($m.Groups['k'].Value) }
    return , $keys
}

# 进结构化元数据的键:各 stage.structured_metadata 的 values。
function Get-AlloyStructuredMetadataKeys {
    param([Parameter(Mandatory = $true)][string]$Config)
    $keys = New-Object System.Collections.Generic.HashSet[string]
    foreach ($block in [regex]::Matches($Config, '(?s)stage\.structured_metadata\s*\{\s*values\s*=\s*\{(?<b>[^}]*)\}')) {
        foreach ($m in [regex]::Matches($block.Groups['b'].Value, '(?<k>[A-Za-z_][A-Za-z0-9_]*)\s*=')) { [void]$keys.Add($m.Groups['k'].Value) }
    }
    return , $keys
}

# 取 C++ 行尾上下文后缀的那几条正则(expression 里带 \[ctx 的),按出现顺序。
function Get-CtxSuffixRegexes {
    param([Parameter(Mandatory = $true)][string]$Block)
    return @([regex]::Matches($Block, '(?m)^\s*expression\s*=\s*(?<v>`[^`]*\[ctx[^`]*`)\s*$') | ForEach-Object { $_.Groups['v'].Value })
}

# 渲染结果里 ConfigMap 的 config.alloy(YAML 块标量,缩进 4 格)还原成 Alloy 原文。
function Get-CollectorAlloyConfig {
    param([Parameter(Mandatory = $true)][string]$Manifest)
    $lines = $Manifest.Replace("`r`n", "`n") -split "`n"
    $start = [array]::IndexOf($lines, '  config.alloy: |')
    if ($start -lt 0) { throw '渲染结果里没有 ConfigMap 的 config.alloy: |' }
    $body = New-Object System.Collections.Generic.List[string]
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        $line = $lines[$i]
        if ($line -eq '') { $body.Add(''); continue }
        if (-not $line.StartsWith('    ')) { break }
        $body.Add($line.Substring(4))
    }
    return (($body -join "`n").TrimEnd()) + "`n"
}

# 清单里某个 kind(+ metadata.name)的那一份文档。
function Get-ManifestDocument {
    param([Parameter(Mandatory = $true)][string]$Manifest, [Parameter(Mandatory = $true)][string]$Kind, [Parameter(Mandatory = $true)][string]$Name)
    $docs = @(($Manifest.Replace("`r`n", "`n") -split '(?m)^---[ \t]*$') |
        Where-Object { $_ -cmatch ('(?m)^kind: ' + [regex]::Escape($Kind) + '\s*$') -and $_ -cmatch ('(?m)^  name: ' + [regex]::Escape($Name) + '\s*$') })
    if ($docs.Count -ne 1) { throw "清单里 $Kind $Name 应当恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

# ──────────────────────────────────────────────
# 共用输入
# ──────────────────────────────────────────────
$LocalConfig = Remove-AlloyComments -Text (Read-RepoFileLf 'deploy/observability/alloy/config.alloy')
$CollectorTemplate = Read-RepoFileLf 'deploy/k8s/manifests/infra/pod-log-collector.yaml'

$Infra = 'logs-infra'
$Prefix = 'logs-zone'
$InClusterPushUrl = "http://loki.${Infra}:3100/loki/api/v1/push"
$AlloyImage = 'grafana/alloy:v1.10.0'

$script:CppLogSidecarPushUrl = $InClusterPushUrl
$SidecarConfig = Remove-AlloyComments -Text (Get-CppLogSidecarConfigAlloy).Replace("`r`n", "`n")
$Rendered = New-PodLogCollectorManifestText -Template $CollectorTemplate -InfraNamespace $Infra -ZoneNamespacePrefix $Prefix -PushUrl $InClusterPushUrl -Image $AlloyImage
$CollectorConfig = Remove-AlloyComments -Text (Get-CollectorAlloyConfig -Manifest $Rendered)

$dumpDir = [Environment]::GetEnvironmentVariable('MMORPG_DUMP_ALLOY_CONFIG_DIR')
if (-not [string]::IsNullOrWhiteSpace($dumpDir)) {
    # 按部署脚本的缺省命名空间渲染(-InfraNamespace mmorpg-infra、-NamespacePrefix mmorpg-zone),与 kind 上缺省部署的那一份相同。
    $null = New-Item -ItemType Directory -Force -Path $dumpDir
    $utf8 = [System.Text.UTF8Encoding]::new($false)
    $script:CppLogSidecarPushUrl = 'http://loki.mmorpg-infra:3100/loki/api/v1/push'
    $defaultManifest = New-PodLogCollectorManifestText -Template $CollectorTemplate -InfraNamespace 'mmorpg-infra' -ZoneNamespacePrefix 'mmorpg-zone' `
        -PushUrl $script:CppLogSidecarPushUrl -Image $AlloyImage
    [IO.File]::WriteAllText((Join-Path $dumpDir 'pod-log-collector.yaml'), $defaultManifest, $utf8)
    [IO.File]::WriteAllText((Join-Path $dumpDir 'pod-log-collector.alloy'), (Get-CollectorAlloyConfig -Manifest $defaultManifest), $utf8)
    [IO.File]::WriteAllText((Join-Path $dumpDir 'cpp-log-sidecar.alloy'), (Get-CppLogSidecarConfigAlloy).Replace("`r`n", "`n"), $utf8)
    $script:CppLogSidecarPushUrl = $InClusterPushUrl
    Write-Host "Rendered log collection configs written to $dumpDir"
}

# ──────────────────────────────────────────────
# 1. 规则与本机一致
# ──────────────────────────────────────────────
Test-Case 'C++ sidecar 取 trace 与玩家的两条正则与本机 cpp 段逐字相同,三个键进结构化元数据' {
    $localRegexes = @(Get-CtxSuffixRegexes -Block (Get-AlloyComponent -Config $LocalConfig -Header 'loki.process "cpp_nodes"'))
    $sidecarProcess = Get-AlloyComponent -Config $SidecarConfig -Header 'loki.process "cpp"'
    Assert-Equal -Expected 2 -Actual $localRegexes.Count -Because '本机 cpp 段应当是 trace 与玩家分开取的两条(抽不到说明写法变了)'
    Assert-SameText -Expected ($localRegexes -join "`n") -Actual ((Get-CtxSuffixRegexes -Block $sidecarProcess) -join "`n") -Because 'sidecar 的两条正则要与本机逐字相同'
    $muduo = @(Get-AlloySubBlocks -Block $sidecarProcess -Name 'stage.match' | Where-Object { (Get-StageSelector -Stage $_) -match 'TRACE\|DEBUG' })
    Assert-Equal -Expected 1 -Actual $muduo.Count -Because 'sidecar 里标准 muduo 行那段恰好一个'
    Assert-Equal -Expected 2 -Actual (@(Get-CtxSuffixRegexes -Block $muduo[0]).Count) -Because '两条正则放在标准 muduo 行那段里(只对带行头的行取)'
    $metadata = Get-AlloyStructuredMetadataKeys -Config $muduo[0]
    foreach ($key in @('tid', 'trace_id', 'span_id', 'player_id')) {
        Assert-True -Condition $metadata.Contains($key) -Because "muduo 行那段的结构化元数据要有 $key"
    }
}

Test-Case 'Go 采集管道:每段 stage.match 与本机 go_services 段相同(去掉取时间的部分),go-redis 那段只取时间、不搬' {
    Assert-StageParity -Local (Get-AlloyComponent -Config $LocalConfig -Header 'loki.process "go_services"') `
        -Cluster (Get-AlloyComponent -Config $CollectorConfig -Header 'loki.process "go_services"') `
        -Label 'Go' -SkipSelectorPattern '\^redis: ' -MinStages 4
}

Test-Case 'Java 采集管道:多行合并的行头与每段 stage.match 都与本机 java 段相同(去掉取时间的部分)' {
    $local = Get-AlloyComponent -Config $LocalConfig -Header 'loki.process "java"'
    $cluster = Get-AlloyComponent -Config $CollectorConfig -Header 'loki.process "java"'
    Assert-StageParity -Local $local -Cluster $cluster -Label 'Java' -MinStages 3
    $localMultiline = @(Get-AlloySubBlocks -Block $local -Name 'stage.multiline')
    $clusterMultiline = @(Get-AlloySubBlocks -Block $cluster -Name 'stage.multiline')
    Assert-Equal -Expected 1 -Actual $localMultiline.Count -Because '本机 java 段有一个多行合并'
    Assert-Equal -Expected 1 -Actual $clusterMultiline.Count -Because '集群 java 管道有一个多行合并'
    Assert-SameText -Expected (ConvertTo-ComparableStage -Stage $localMultiline[0]) -Actual (ConvertTo-ComparableStage -Stage $clusterMultiline[0]) -Because '堆栈续行的行头要与本机一致'
    $cri = $cluster.IndexOf('stage.cri')
    Assert-True -Condition ($cri -ge 0 -and $cri -lt $cluster.IndexOf('stage.multiline')) -Because '先剥掉 CRI 行头再按正文合并多行'
}

Test-Case 'C++ 容器 stdout / stderr 的管道:gRPC / abseil 与 librdkafka 两段与本机 cpp 段相同,muduo 行与 Windows 断言行不搬,另有未捕获异常记 fatal' {
    $cluster = Get-AlloyComponent -Config $CollectorConfig -Header 'loki.process "cpp_nodes"'
    Assert-StageParity -Local (Get-AlloyComponent -Config $LocalConfig -Header 'loki.process "cpp_nodes"') -Cluster $cluster -Label 'C++ stdout / stderr' `
        -SkipSelectorPattern 'TRACE\|DEBUG|\^Assertion failed: ' -ClusterOnlySelectorPattern '\^terminate called ' -MinStages 2
    $terminate = @(Get-AlloySubBlocks -Block $cluster -Name 'stage.match' | Where-Object { (Get-StageSelector -Stage $_) -match '\^terminate called ' })
    Assert-Equal -Expected 1 -Actual $terminate.Count -Because '未捕获异常那段恰好一个'
    Assert-Match -Text $terminate[0] -Pattern 'stage\.static_labels\s*\{\s*values\s*=\s*\{\s*level\s*=\s*"fatal"\s*\}' -Because '未捕获异常按 fatal 记'
    Assert-NotMatch -Text $cluster -Pattern 'stage\.multiline' -Because 'C++ 容器输出一行一条,不合并多行(gate 的启动行不能被并进前一条 gRPC 行)'
    Assert-Match -Text $CollectorConfig -Pattern ('(?s)source_labels\s*=\s*\["__meta_kubernetes_pod_label_tier",\s*"__meta_kubernetes_pod_label_app"\]\s*separator\s*=\s*";"\s*regex\s*=\s*' +
        [regex]::Escape('";(gate|scene|battle)(?:-world|-instance)?(?:-canary)?"') + '\s*target_label\s*=\s*"job"\s*replacement\s*=\s*"cpp_nodes"') -Because 'C++ 节点 = 没有 tier、app 是 C++ 节点名的 Pod'
    Assert-Match -Text $CollectorConfig -Pattern ('(?s)regex\s*=\s*' + [regex]::Escape('";(gate|scene|battle)(?:-world|-instance)?(?:-canary)?"') + '\s*target_label\s*=\s*"service"\s*replacement\s*=\s*"\$1"') -Because 'C++ 的 service 取节点角色,与 sidecar 同值'
}

Test-Case 'DaemonSet 认得出的 app 与部署脚本里 C++ 工作负载的名字对得上(gate / scene 三种池 / battle,金丝雀加 -canary)' {
    $cppApp = [regex]::new('\A(gate|scene|battle)(?:-world|-instance)?(?:-canary)?\z')
    $deployText = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    $names = New-Object System.Collections.Generic.HashSet[string]
    foreach ($m in [regex]::Matches($deployText, '(?m)-NodeName\s+["'']([a-z][a-z-]*)["'']')) { [void]$names.Add($m.Groups[1].Value) }
    foreach ($m in [regex]::Matches($deployText, '(?m)^\s*Name\s*=\s*"(scene[a-z-]*)"')) { [void]$names.Add($m.Groups[1].Value) }
    foreach ($m in [regex]::Matches($deployText, "(?m)\`$name\s*=\s*'(battle[a-z-]*)'")) { [void]$names.Add($m.Groups[1].Value) }
    foreach ($expected in @('gate', 'battle', 'scene', 'scene-world', 'scene-instance', 'battle-canary')) {
        Assert-True -Condition $names.Contains($expected) -Because "前提:部署脚本里抽得到 C++ 工作负载名 $expected(实际:$(@($names) -join ', '));抽不到说明写法变了,本用例要跟着改"
    }
    foreach ($name in $names) {
        Assert-True -Condition $cppApp.IsMatch($name) -Because "C++ 工作负载 $name 要被 DaemonSet 认成 C++ 节点,否则它容器 stderr 上的 gRPC / librdkafka 日志与崩溃现场没人采"
        Assert-True -Condition $cppApp.IsMatch("$name-canary") -Because "金丝雀 $name-canary 同样要认得出"
    }
}

Test-Case '集群里的时间一律用 CRI 时间戳:三条管道先过 stage.cri,不再从行内取时间' {
    foreach ($name in @('go_services', 'java', 'cpp_nodes')) {
        $process = Get-AlloyComponent -Config $CollectorConfig -Header "loki.process `"$name`""
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($process, 'stage\.cri\s*\{').Count) -Because "${name}:恰好一个 stage.cri"
        Assert-True -Condition ($process.IndexOf('stage.cri') -lt $process.IndexOf('stage.match')) -Because "${name}:stage.cri 在所有 stage.match 之前"
        Assert-NotMatch -Text $process -Pattern 'stage\.timestamp' -Because "${name}:不从行内取时间(本机 go-redis 那段的开发机时区搬进容器会算出未来时间,Loki 400 拒收整批丢)"
    }
}

Test-Case 'trace_id / span_id / player_id 在三份 Alloy 配置里都只进结构化元数据,不做标签' {
    foreach ($case in @(
            @{ Label = '本机 config.alloy'; Config = $LocalConfig; Expected = @('trace_id', 'span_id', 'player_id') }
            @{ Label = 'C++ sidecar'; Config = $SidecarConfig; Expected = @('trace_id', 'span_id', 'player_id') }
            @{ Label = '容器日志 DaemonSet'; Config = $CollectorConfig; Expected = @('trace_id', 'span_id', 'player_id', 'caller', 'logger', 'thread', 'tid', 'filename') }
        )) {
        $labels = Get-AlloyLabelKeys -Config $case.Config
        foreach ($key in @('trace_id', 'span_id', 'player_id', 'filename', 'caller', 'tid', 'logger', 'thread', 'pid')) {
            Assert-True -Condition (-not $labels.Contains($key)) -Because "$($case.Label):$key 不能做流标签(实际标签:$(@($labels) -join ', '))"
        }
        $metadata = Get-AlloyStructuredMetadataKeys -Config $case.Config
        foreach ($key in $case.Expected) {
            Assert-True -Condition $metadata.Contains($key) -Because "$($case.Label):结构化元数据里要有 $key"
        }
    }
    $collectorLabels = Get-AlloyLabelKeys -Config $CollectorConfig
    foreach ($key in @('env', 'job', 'lang', 'service', 'zone', 'namespace', 'pod', 'container', 'level')) {
        Assert-True -Condition $collectorLabels.Contains($key) -Because "DaemonSet 的流标签要有 $key(与本机 §3 / k8s §6.4 的口径对齐)"
    }
    Assert-Match -Text $CollectorConfig -Pattern 'stage\.label_drop\s*\{\s*values\s*=\s*\["filename"\]' -Because 'filename 每次重启都变,转存结构化元数据后从流标签里删掉'
}

# ──────────────────────────────────────────────
# 2. DaemonSet 清单的渲染(纯函数)
# ──────────────────────────────────────────────
Test-Case '渲染:占位全部替换,采集范围、zone 标签、RBAC 绑定与读取位置目录都带上命名空间' {
    Assert-NotMatch -Text $Rendered -Pattern '__[A-Z][A-Z0-9_]*__' -Because '五个占位都要换掉'
    Assert-Match -Text $CollectorConfig -Pattern ('url\s*=\s*"' + [regex]::Escape($InClusterPushUrl) + '"') -Because '推送地址'
    Assert-Match -Text $CollectorConfig -Pattern ('regex\s*=\s*"' + [regex]::Escape("$Infra|$Prefix-.+") + '"\s*action\s*=\s*"keep"') -Because '只采本部署的 infra 与 zone 命名空间'
    $zoneRule = [regex]::Match($CollectorConfig, 'regex\s*=\s*"' + [regex]::Escape("$Prefix-(.+)") + '"\s*target_label\s*=\s*"zone"')
    $globalRule = [regex]::Match($CollectorConfig, 'regex\s*=\s*"' + [regex]::Escape($Infra) + '"\s*target_label\s*=\s*"zone"\s*replacement\s*=\s*"global"')
    Assert-True -Condition $zoneRule.Success -Because 'zone 名取自 zone 命名空间'
    Assert-True -Condition $globalRule.Success -Because 'infra 命名空间里的全局服务记 zone=global'
    Assert-True -Condition ($zoneRule.Index -lt $globalRule.Index) -Because 'global 那条排在后面,前缀碰巧也匹配 infra 命名空间时以 global 为准'
    foreach ($tier in @(@{ Tier = 'go-svc'; Job = 'go_services'; Lang = 'go' }, @{ Tier = 'java-svc'; Job = 'java'; Lang = 'java' })) {
        foreach ($pair in @(@{ Label = 'job'; Value = $tier.Job }, @{ Label = 'lang'; Value = $tier.Lang })) {
            Assert-Match -Text $CollectorConfig -Pattern ('(?s)__meta_kubernetes_pod_label_tier"\]\s*regex\s*=\s*"' + [regex]::Escape($tier.Tier) + '"\s*target_label\s*=\s*"' + $pair.Label + '"\s*replacement\s*=\s*"' + $pair.Value + '"') `
                -Because "tier=$($tier.Tier) 的 Pod 记 $($pair.Label)=$($pair.Value)"
        }
    }
    $keepJob = [regex]::Match($CollectorConfig, '(?s)source_labels\s*=\s*\["job"\]\s*regex\s*=\s*"\.\+"\s*action\s*=\s*"keep"')
    Assert-True -Condition $keepJob.Success -Because '认不出语言的 Pod(etcd / Kafka / Redis / Loki、本采集器自己)不采'
    $cppJob = [regex]::Match($CollectorConfig, 'replacement\s*=\s*"cpp_nodes"')
    Assert-True -Condition ($cppJob.Success -and $keepJob.Index -gt $cppJob.Index) -Because '先认完三种语言再丢认不出的'
    Assert-Match -Text $CollectorConfig -Pattern '(?s)__meta_kubernetes_pod_container_init"\]\s*regex\s*=\s*"true"\s*action\s*=\s*"drop"' -Because 'init 容器不采(含 C++ 的日志 sidecar)'
    Assert-Match -Text $CollectorConfig -Pattern 'field\s*=\s*"spec\.nodeName="\s*\+\s*sys\.env\("NODE_NAME"\)' -Because '每个采集器只发现本节点的 Pod'
    Assert-Match -Text $CollectorConfig -Pattern 'replacement\s*=\s*"/var/log/pods/\*\$1/\*\.log"' -Because '读 kubelet 落盘的当前文件,轮转下来的旧文件不重读'
    Assert-Match -Text $CollectorConfig -Pattern ('source\s*=\s*"service"\s*template\s*=\s*' + [regex]::Escape('`{{ Replace .Value "-" "_" -1 }}`')) -Because 'service 统一成本机的下划线写法'

    $binding = Get-ManifestDocument -Manifest $Rendered -Kind 'ClusterRoleBinding' -Name "mmorpg-pod-log-reader-$Infra"
    Assert-Match -Text $binding -Pattern ('(?s)subjects:\s*- kind: ServiceAccount\s*name: pod-log-collector\s*namespace: ' + [regex]::Escape($Infra) + '\s*$') -Because '绑定给 infra 命名空间里的专用 ServiceAccount'
    Assert-Match -Text $binding -Pattern '(?s)roleRef:.*?kind: ClusterRole\s*name: mmorpg-pod-log-reader\s' -Because '绑定的是只读角色'
    $role = Get-ManifestDocument -Manifest $Rendered -Kind 'ClusterRole' -Name 'mmorpg-pod-log-reader'
    Assert-Match -Text $role -Pattern 'resources: \["pods"\]\s*verbs: \["list", "watch"\]' -Because '只给 pods 的 list / watch'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($role, '(?m)^\s*- apiGroups:').Count) -Because '只有这一条规则'
    $null = Get-ManifestDocument -Manifest $Rendered -Kind 'ServiceAccount' -Name 'pod-log-collector'

    $ds = Get-ManifestDocument -Manifest $Rendered -Kind 'DaemonSet' -Name 'pod-log-collector'
    Assert-Match -Text $ds -Pattern 'serviceAccountName: pod-log-collector' -Because '用专用 ServiceAccount,不挂到命名空间的 default 上'
    Assert-Match -Text $ds -Pattern ('image: ' + [regex]::Escape($AlloyImage) + '\s') -Because '与 C++ sidecar 同一个镜像'
    Assert-Match -Text $ds -Pattern '(?s)- name: pod-logs\s+mountPath: /var/log/pods\s+readOnly: true' -Because '/var/log/pods 只读挂载'
    Assert-Match -Text $ds -Pattern '(?s)- name: pod-logs\s+hostPath:\s+path: /var/log/pods\s+type: Directory' -Because '读的是宿主机上 kubelet 的落盘目录'
    Assert-Match -Text $ds -Pattern ('(?s)- name: state\s+hostPath:\s+path: /var/lib/mmorpg-pod-log-collector/' + [regex]::Escape($Infra) + '\s+type: DirectoryOrCreate') -Because '读取位置落宿主机,每个 infra 命名空间一个目录'
    Assert-Match -Text $ds -Pattern '(?s)- name: NODE_NAME\s+valueFrom:\s+fieldRef:\s+fieldPath: spec\.nodeName' -Because '节点名经 Downward API 交给字段选择器'
    foreach ($pattern in @('allowPrivilegeEscalation: false', 'readOnlyRootFilesystem: true', '(?s)capabilities:\s+drop:\s+- ALL')) {
        Assert-Match -Text $ds -Pattern $pattern -Because "root 运行时其余能收的都收:$pattern"
    }
    Assert-Match -Text $ds -Pattern 'mmorpg\.io/pod-log-collector-config-hash: "[0-9a-f]{12}"' -Because '配置指纹在 Pod 模板上'
    Assert-NotMatch -Text $Rendered -Pattern 'secretKeyRef|(?m)^kind: Secret\s*$' -Because '这份清单不需要任何密钥'
}

Test-Case '渲染:指纹确定,行尾无关,对推送地址、镜像与命名空间敏感' {
    $hashOf = { param([string]$Text) [regex]::Match($Text, 'mmorpg\.io/pod-log-collector-config-hash: "(?<h>[0-9a-f]{12})"').Groups['h'].Value }
    $again = New-PodLogCollectorManifestText -Template $CollectorTemplate -InfraNamespace $Infra -ZoneNamespacePrefix $Prefix -PushUrl $InClusterPushUrl -Image $AlloyImage
    Assert-SameText -Expected $Rendered -Actual $again -Because '同样的输入,同样的输出'
    $crlf = New-PodLogCollectorManifestText -Template ($CollectorTemplate.Replace("`n", "`r`n")) -InfraNamespace $Infra -ZoneNamespacePrefix $Prefix -PushUrl $InClusterPushUrl -Image $AlloyImage
    Assert-SameText -Expected $Rendered -Actual $crlf -Because 'Windows 检出(CRLF)与 Linux 检出(LF)渲染出逐字相同的清单,换台机器部署不白滚'
    $base = & $hashOf $Rendered
    foreach ($case in @(
            @{ Why = '推送地址'; Args = @{ PushUrl = 'http://loki.observability:3100/loki/api/v1/push' } }
            @{ Why = '镜像'; Args = @{ Image = 'grafana/alloy:v1.10.1' } }
            @{ Why = 'zone 前缀'; Args = @{ ZoneNamespacePrefix = 'other-zone' } }
            @{ Why = 'infra 命名空间'; Args = @{ InfraNamespace = 'other-infra' } }
        )) {
        $params = @{ Template = $CollectorTemplate; InfraNamespace = $Infra; ZoneNamespacePrefix = $Prefix; PushUrl = $InClusterPushUrl; Image = $AlloyImage }
        foreach ($k in $case.Args.Keys) { $params[$k] = $case.Args[$k] }
        Assert-True -Condition ((& $hashOf (New-PodLogCollectorManifestText @params)) -cne $base) -Because "改了$($case.Why),Pod 模板的指纹要变,apply 才会滚动"
    }
}

Test-Case '渲染:命名空间名不合法、镜像带空白、缺指纹占位、剩下未知占位都拒绝' {
    $params = @{ Template = $CollectorTemplate; InfraNamespace = $Infra; ZoneNamespacePrefix = $Prefix; PushUrl = $InClusterPushUrl; Image = $AlloyImage }
    foreach ($case in @(
            @{ Why = 'infra 命名空间带大写与下划线'; Args = @{ InfraNamespace = 'Logs_Infra' }; Pattern = '-InfraNamespace must be a Kubernetes namespace name' }
            @{ Why = 'zone 前缀带正则字符'; Args = @{ ZoneNamespacePrefix = 'zone.*' }; Pattern = '-NamespacePrefix must be a Kubernetes namespace name' }
            @{ Why = 'zone 前缀以连字符结尾'; Args = @{ ZoneNamespacePrefix = 'zone-' }; Pattern = '-NamespacePrefix must be a Kubernetes namespace name' }
            @{ Why = '镜像带空白'; Args = @{ Image = 'grafana/alloy:v1.10.0 --privileged' }; Pattern = 'plain image reference' }
            @{ Why = '清单缺指纹占位'; Args = @{ Template = $CollectorTemplate.Replace('__POD_LOG_COLLECTOR_CONFIG_HASH__', 'fixed') }; Pattern = 'would never roll the DaemonSet' }
            @{ Why = '清单里有未知占位'; Args = @{ Template = $CollectorTemplate + "`n# __UNKNOWN_THING__`n" }; Pattern = 'still has the placeholder __UNKNOWN_THING__' }
        )) {
        $p = @{} + $params
        foreach ($k in $case.Args.Keys) { $p[$k] = $case.Args[$k] }
        Assert-Match -Text (Get-ThrownMessage { New-PodLogCollectorManifestText @p }) -Pattern $case.Pattern -Because "$($case.Why) 要拒绝"
    }
}

# ──────────────────────────────────────────────
# 3. 推送地址的校验与部署判定(纯函数)
# ──────────────────────────────────────────────
Test-Case '-LokiPushUrl:只认不带凭据的 http(s) 绝对地址,报错不回显地址' {
    foreach ($ok in @('', 'http://loki.observability:3100/loki/api/v1/push', 'https://logs.example.com/loki/api/v1/push', 'http://10.0.0.5:3100/loki/api/v1/push')) {
        Assert-Equal -Expected '' -Actual (Get-ThrownMessage { Assert-LokiPushUrlSafe -Url $ok }) -Because "合法地址放行:'$ok'"
    }
    foreach ($case in @(
            @{ Url = 'http://logger:contract-pw-0001@loki.example.invalid:3100/loki/api/v1/push'; Pattern = 'must not carry credentials'; Secret = 'contract-pw-0001' }
            @{ Url = 'http://contract-token-0002@loki.example.invalid/loki/api/v1/push'; Pattern = 'must not carry credentials'; Secret = 'contract-token-0002' }
            @{ Url = 'logs-gateway.internal:3100/loki/api/v1/push'; Pattern = 'absolute http or https URL'; Secret = 'logs-gateway' }
            @{ Url = 'ftp://loki.example.invalid/push'; Pattern = 'absolute http or https URL'; Secret = 'ftp://' }
            @{ Url = 'http://loki.example.invalid/push"injected'; Pattern = 'quotes, backslashes'; Secret = 'injected' }
            @{ Url = 'http://loki.example.invalid/a b'; Pattern = 'quotes, backslashes'; Secret = 'example.invalid' }
            @{ Url = 'http://loki.example.invalid\push'; Pattern = 'quotes, backslashes'; Secret = 'example.invalid' }
            @{ Url = "http://loki.example.invalid/a`nb"; Pattern = 'quotes, backslashes'; Secret = 'example.invalid' }
        )) {
        $message = Get-ThrownMessage { Assert-LokiPushUrlSafe -Url $case.Url }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "'$($case.Url.Replace("`n", '\n'))' 要拒绝"
        Assert-NotMatch -Text $message -Pattern ([regex]::Escape($case.Secret)) -Because '报错不回显地址本身(带凭据时那就是口令)'
    }
}

Test-Case '部署判定:只有 infra-up 与不带 -SkipInfra 的 all-up 部署 DaemonSet,-NoCppLogSidecar 两路一起关' {
    foreach ($case in @(
            @{ Command = 'infra-up'; SkipInfra = $false; Sidecar = $true; Expected = $true }
            @{ Command = 'all-up'; SkipInfra = $false; Sidecar = $true; Expected = $true }
            @{ Command = 'all-up'; SkipInfra = $true; Sidecar = $true; Expected = $false }
            @{ Command = 'zone-up'; SkipInfra = $false; Sidecar = $true; Expected = $false }
            @{ Command = 'infra-up'; SkipInfra = $false; Sidecar = $false; Expected = $false }
            @{ Command = 'infra-down'; SkipInfra = $false; Sidecar = $true; Expected = $false }
        )) {
        $script:Command = $case.Command
        $script:SkipInfra = $case.SkipInfra
        $script:CppLogSidecarEnabled = $case.Sidecar
        Assert-Equal -Expected $case.Expected -Actual (Test-PodLogCollectorDeployed) -Because "$($case.Command) SkipInfra=$($case.SkipInfra) sidecar=$($case.Sidecar)"
    }
}

Test-Case '只有 Apply-Infra 渲染与部署 DaemonSet:渲染在第一个写操作(Ensure-Namespace)之前,推送地址的校验在写路径入口' {
    $applyInfra = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    Assert-True -Condition ($null -ne $applyInfra) -Because '前提:k8s_deploy.ps1 有 Apply-Infra'
    $callsOf = {
        param([string]$CommandName)
        @($deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.CommandAst] -and $a.GetCommandName() -ceq $CommandName }.GetNewClosure(), $true))
    }
    $inApplyInfra = { param($Ast) $Ast.Extent.StartOffset -gt $applyInfra.Extent.StartOffset -and $Ast.Extent.EndOffset -lt $applyInfra.Extent.EndOffset }
    foreach ($command in @('New-PodLogCollectorManifest', 'Apply-PodLogCollector')) {
        $calls = & $callsOf $command
        Assert-Equal -Expected 1 -Actual $calls.Count -Because "$command 只调一处"
        Assert-True -Condition (& $inApplyInfra $calls[0]) -Because "$command 在 Apply-Infra 里调用"
    }
    $render = (& $callsOf 'New-PodLogCollectorManifest')[0].Extent.StartOffset
    $firstWrite = @(& $callsOf 'Ensure-Namespace' | Where-Object { & $inApplyInfra $_ } | ForEach-Object { $_.Extent.StartOffset } | Sort-Object)[0]
    Assert-True -Condition ($null -ne $firstWrite -and $render -lt $firstWrite) -Because '渲染与校验在 Apply-Infra 的第一个写操作之前,渲染失败不留半截部署'
    $guard = @(& $callsOf 'Assert-LokiPushUrlSafe')
    Assert-Equal -Expected 1 -Actual $guard.Count -Because '推送地址只在写路径入口校验一处'
    $initSecrets = @(& $callsOf 'Initialize-InjectedSecrets' | ForEach-Object { $_.Extent.StartOffset } | Sort-Object)[0]
    Assert-True -Condition ($guard[0].Extent.StartOffset -lt $initSecrets -and -not (& $inApplyInfra $guard[0])) -Because '在写路径入口、密钥解析之前拒绝'
}

# ──────────────────────────────────────────────
# 4. DryRun
# ──────────────────────────────────────────────
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-InfraNamespace', $Infra,
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }

function Invoke-LogDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    return Invoke-DeployDryRun -Arguments $Arguments -Env $Env
}

# DryRun 输出里 DaemonSet 那一份 MANIFEST 块(ServiceAccount、RBAC、ConfigMap、DaemonSet 在同一块里)。
function Get-CollectorBlock {
    param([Parameter(Mandatory = $true)][string]$Output)
    $blocks = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: DaemonSet\s*$' })
    if ($blocks.Count -ne 1) { throw "DryRun 输出里应当恰好 1 份 DaemonSet 清单,实际 $($blocks.Count) 份" }
    return $blocks[0]
}

Test-Case 'DryRun:dev 单副本 Loki,DaemonSet 推 loki.<infra>:3100,排在 Loki 之后' {
    $run = Invoke-LogDryRun -Arguments $InfraArgs -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $block = Get-CollectorBlock -Output $run.Output
    Assert-Match -Text $block -Pattern ('url\s*=\s*"' + [regex]::Escape($InClusterPushUrl) + '"') -Because '推集群内 Loki'
    Assert-Match -Text $block -Pattern ([regex]::Escape("`"$Infra|mmorpg-zone-.+`"")) -Because 'zone 前缀取 -NamespacePrefix 的缺省值'
    Assert-NotMatch -Text $block -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    Assert-Match -Text $run.Output -Pattern ([regex]::Escape("Pod log collector: daemonset/pod-log-collector in $Infra ships container stdout/stderr of Go, Java and C++ pods to $InClusterPushUrl")) -Because '部署输出点名推送地址'
    $lokiAt = $run.Output.IndexOf('claimName: loki-data-pvc')
    $collectorAt = $run.Output.IndexOf('kind: DaemonSet')
    Assert-True -Condition ($lokiAt -ge 0 -and $collectorAt -gt $lokiAt) -Because 'DaemonSet 排在 Loki 之后'
}

Test-Case 'DryRun:3 副本 Loki(dev 夹具)下 DaemonSet 的推送地址不变' {
    $run = Invoke-LogDryRun -Arguments ($InfraArgs + @('-LokiReplicas', '3', '-LokiS3DevFixture')) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Loki: replicas=3 manifest=loki-ha\.yaml' -Because '前提:这次是 3 副本形态'
    Assert-Match -Text (Get-CollectorBlock -Output $run.Output) -Pattern ('url\s*=\s*"' + [regex]::Escape($InClusterPushUrl) + '"') -Because '两种形态的 Service 都叫 loki:3100'
}

Test-Case 'DryRun:给了 -LokiPushUrl 时 DaemonSet 推那个地址,集群内不部署 Loki' {
    $external = 'http://loki.observability:3100/loki/api/v1/push'
    $run = Invoke-LogDryRun -Arguments ($InfraArgs + @('-LokiPushUrl', $external)) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-Match -Text (Get-CollectorBlock -Output $run.Output) -Pattern ('url\s*=\s*"' + [regex]::Escape($external) + '"') -Because '与 C++ sidecar 推同一个外部 Loki'
    Assert-NotMatch -Text $run.Output -Pattern 'claimName: loki-data-pvc|serviceName: loki-memberlist' -Because '指向外部 Loki 时不部署集群内那套'
}

Test-Case 'DryRun:-NoCppLogSidecar 两路一起关,不渲染 DaemonSet' {
    $run = Invoke-LogDryRun -Arguments ($InfraArgs + @('-NoCppLogSidecar')) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-NotMatch -Text $run.Output -Pattern 'pod-log-collector|kind: DaemonSet' -Because '关掉后不能留下采集器的任何对象'
}

Test-Case 'DryRun:-LokiPushUrl 带凭据在任何清单之前拒绝,输出里没有口令' {
    $run = Invoke-LogDryRun -Arguments ($InfraArgs + @('-LokiPushUrl', 'http://logger:contract-pw-0001@loki.example.invalid:3100/loki/api/v1/push')) -Env $InfraEnv
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "带凭据的地址要拒绝。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'must not carry credentials' -Because '点名原因'
    Assert-NotMatch -Text $run.Output -Pattern 'contract-pw-0001' -Because '口令不进任何输出'
    Assert-NotMatch -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'
}

Test-Case 'DryRun:infra-down 删完命名空间再按名字删 ClusterRoleBinding,名字与清单里的一致' {
    $run = Invoke-LogDryRun -Arguments @('-Command', 'infra-down', '-DryRun', '-InfraNamespace', $Infra, '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab') -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $namespaceAt = $run.Output.IndexOf("delete namespace $Infra")
    $bindingAt = $run.Output.IndexOf("delete clusterrolebinding mmorpg-pod-log-reader-$Infra --ignore-not-found")
    Assert-True -Condition ($namespaceAt -ge 0 -and $bindingAt -gt $namespaceAt) -Because '集群级的绑定删命名空间带不走,要按名字删'
    Assert-NotMatch -Text $run.Output -Pattern 'delete clusterrole mmorpg-pod-log-reader' -Because '共用的 ClusterRole 留给同一集群里别的部署'
    $null = Get-ManifestDocument -Manifest $Rendered -Kind 'ClusterRoleBinding' -Name "mmorpg-pod-log-reader-$Infra"
}

exit (Complete-TestRun -SuiteName 'k8s log collection contract')
