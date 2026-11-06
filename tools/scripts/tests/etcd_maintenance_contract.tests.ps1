#requires -Version 7
<#
.SYNOPSIS
    etcd 镜像、MVCC 压缩与空间维护(P0-C)的静态契约:三套 etcd 清单与两份 compose 的镜像、启动方式、数据目录、压缩 / 配额取值,
    退役的压缩 CronJob 不再下发,告警规则与「官方镜像没有 shell」的使用约束。

.DESCRIPTION
    只读仓库里的静态文件:不调用 k8s_deploy.ps1,不访问集群、容器或网络。
    「infra-up 的 DryRun 产物不再有 etcd-compact、会删残留」要跑部署脚本,放在 k8s_deploy_contract.tests.ps1。

    守住的东西(2026-11-05 起,P0-C 阶段 2 收尾):
      - 镜像:主 etcd、发号器两套、两份 compose 是同一个官方镜像引用,按 quay.io/coreos/etcd:v<版本>@sha256:<64 位> 钉死;
        任何生效内容里不再有 bitnami 或 :latest。换版本只能五处同批改;
      - 启动:官方镜像是 distroless,StatefulSet 显式 command: ["/usr/local/bin/etcd"],ETCD_* 由 etcd 自己解析;不用 bitnami 专用的
        ALLOW_NONE_AUTHENTICATION;数据目录在挂载点之下;非 root 运行;
      - 取值:主 etcd 与两份 compose 的压缩模式、保留量、配额相同;三套 StatefulSet 都满足「配额 × 2 + 1Gi ≤ PVC」;
      - 主 etcd 的抓取注解与排除 NOSPACE 的探针在;发号器两套也带抓取注解(告警覆盖);
      - 压缩 CronJob etcd-compact 退役:清单删除,Apply-Infra 不再 apply 它并删掉集群里的残留;
      - deploy/k8s/etcd-alerts.yaml 有无主与空间告警;
      - 官方镜像没有 shell:脚本与运维手册里 exec 进 etcd 的命令一律直接跑 etcdctl。

    设计与取值:docs/handoff/global-singletons-p0-findings-20261014.md「## P0-C」、交接 handoff-global-singletons-p0-20261015.md §8;
    运维:docs/ops/etcd-maintenance-runbook.md。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

# 路径一律正斜杠 + Join-Path:CI 在 ubuntu 上跑(.github/workflows/deploy-config-tests.yml)。
$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:InfraDir = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra'
$script:EtcdPath = Join-Path $script:InfraDir 'etcd.yaml'
$script:StatefulSetPaths = [ordered]@{
    'etcd'              = $script:EtcdPath
    'etcd-id-secondary' = (Join-Path $script:InfraDir 'etcd-id-secondary.yaml')
    'etcd-id-registry'  = (Join-Path $script:InfraDir 'etcd-id-registry.yaml')
}
$script:MaintenancePath = Join-Path $script:InfraDir 'etcd-maintenance.yaml'
$script:ComposePath = Join-Path $script:RepoRoot 'deploy/docker-compose.yml'
$script:DebugComposePath = Join-Path $script:RepoRoot 'deploy/debug/compose.game.yml'
$script:AlertsPath = Join-Path $script:RepoRoot 'deploy/k8s/etcd-alerts.yaml'
$script:DeployPath = Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'
$script:RunbookPath = Join-Path $script:RepoRoot 'docs/ops/etcd-maintenance-runbook.md'
# 官方镜像引用的形状:quay.io/coreos/etcd:v<主>.<次>.<补丁>@sha256:<64 位小写十六进制>。
$script:PinnedImagePattern = '^quay\.io/coreos/etcd:v3\.\d+\.\d+@sha256:[0-9a-f]{64}$'

# 读文件放在各用例里调用:文件缺失时每条用例各自报红,而不是整个脚本中断。
function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { throw "找不到 $Path" }
    return ([IO.File]::ReadAllText($Path) -replace "`r`n", "`n")
}

# 去掉整行注释(去掉缩进后以 # 开头),只对真正生效的内容断言。
function Get-NonCommentText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return ((($Text -split "`n") | Where-Object { -not $_.TrimStart().StartsWith('#') }) -join "`n")
}

# 取 compose 里某个服务的块:从 `  <服务名>:` 到下一个缩进 ≤ 2 的非注释行(下一个服务或顶层的 volumes:)。
function Get-ComposeServiceBlock {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Service
    )
    $lines = $Text -split "`n"
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match ('^  ' + [regex]::Escape($Service) + ':\s*$')) { $start = $i; break }
    }
    if ($start -lt 0) { throw "compose 里找不到服务 $Service" }

    $body = [System.Collections.Generic.List[string]]::new()
    $body.Add($lines[$start])
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        $l = $lines[$i]
        $isKeyLine = $l.Trim().Length -gt 0 -and -not $l.TrimStart().StartsWith('#')
        if ($isKeyLine -and ($l.Length - $l.TrimStart().Length) -le 2) { break }
        $body.Add($l)
    }
    return ($body -join "`n")
}

# 取文本里某个正则第 1 组的唯一值:出现 0 次或多于 1 次都按契约破坏处理(fail-closed,不取「第一个」糊过去)。
function Get-SingleCapture {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Pattern,
        [Parameter(Mandatory = $true)][string]$What
    )
    $ms = [regex]::Matches($Text, $Pattern)
    if ($ms.Count -ne 1) { throw "$What 应当恰好出现 1 次,实际 $($ms.Count) 次(模式 /$Pattern/)" }
    return $ms[0].Groups[1].Value
}

# StatefulSet 里某个 env 的值(`- name: X` 下一行 `value: "..."`)。
function Get-StatefulSetEnv {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Label)
    return Get-SingleCapture -Text $Text -Pattern ('name:\s*' + [regex]::Escape($Name) + '\s*\n\s*value:\s*"?([^"\s]*)"?') -What "$Label 的 $Name"
}

# compose 服务块里某个 env 的值,两种写法都认:列表 `- NAME=value` 与映射 `NAME: value`。
function Get-ComposeEnv {
    param([Parameter(Mandatory = $true)][string]$Block, [Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Label)
    $escaped = [regex]::Escape($Name)
    $pattern = '(?m)^\s*(?:-\s*' + $escaped + '=(\S*)|' + $escaped + ':\s*"?([^"\s#]*)"?)\s*(?:#.*)?$'
    $ms = [regex]::Matches($Block, $pattern)
    if ($ms.Count -ne 1) { throw "$Label 的 $Name 应当恰好出现 1 次,实际 $($ms.Count) 次" }
    $m = $ms[0]
    if ($m.Groups[1].Success) { return $m.Groups[1].Value }
    return $m.Groups[2].Value
}

function Get-ComposeEtcdBlock {
    param([Parameter(Mandatory = $true)][string]$Path)
    return Get-NonCommentText (Get-ComposeServiceBlock -Text (Read-RepoText $Path) -Service 'etcd')
}

Write-Host "== etcd 镜像与压缩契约(P0-C)=="

Test-Case '镜像:三套 etcd 清单与两份 compose 是同一个官方镜像引用,按 tag@摘要钉死;生效内容里不再有 bitnami 与 :latest' {
    $images = [ordered]@{}
    foreach ($name in $script:StatefulSetPaths.Keys) {
        $images[$name] = Get-SingleCapture -Text (Get-NonCommentText (Read-RepoText $script:StatefulSetPaths[$name])) -Pattern '(?m)^\s*image:\s*(\S+)\s*$' -What "$name 的 image"
    }
    $images['compose'] = Get-SingleCapture -Text (Get-ComposeEtcdBlock -Path $script:ComposePath) -Pattern '(?m)^\s*image:\s*(\S+)\s*$' -What 'compose etcd 服务的 image'
    # 调试栈允许用 ETCD_IMAGE 覆盖,缺省值必须是同一个引用。
    $images['debug-compose'] = Get-SingleCapture -Text (Get-ComposeEtcdBlock -Path $script:DebugComposePath) -Pattern '(?m)^\s*image:\s*\$\{ETCD_IMAGE:-([^}\s]+)\}\s*$' -What '调试 compose etcd 服务的 image 缺省值'
    $reference = $images['etcd']
    Assert-Match -Text $reference -Pattern $script:PinnedImagePattern -Because "必须是官方镜像并按 tag@摘要钉死(quay.io/coreos/etcd:v<版本>@sha256:<64 位>),实际 '$reference'"
    foreach ($where in $images.Keys) {
        Assert-Equal -Expected $reference -Actual $images[$where] -Because "$where 必须与主 etcd 同一个镜像引用:换版本只能五处同批改,两个仓库(quay / gcr)同一 tag 的摘要也不同,不能混用"
    }
    foreach ($path in @($script:StatefulSetPaths.Values) + @($script:ComposePath, $script:DebugComposePath)) {
        $code = if ($path -like '*compose*') { Get-ComposeEtcdBlock -Path $path } else { Get-NonCommentText (Read-RepoText $path) }
        Assert-NotMatch -Text $code -Pattern 'bitnami' -Because "$([IO.Path]::GetFileName($path)):bitnamilegacy 是停更的归档镜像,路径与开关也不该再出现在生效内容里"
        Assert-NotMatch -Text $code -Pattern ':latest\b' -Because "$([IO.Path]::GetFileName($path)):浮动 tag 不可复现,也拿不到摘要"
    }
}

Test-Case '启动:三套 StatefulSet 显式以 etcd 二进制启动、不用 bitnami 专用开关、数据目录在挂载点之下、非 root 运行' {
    foreach ($name in $script:StatefulSetPaths.Keys) {
        $code = Get-NonCommentText (Read-RepoText $script:StatefulSetPaths[$name])
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($code, '(?m)^\s*command:\s*\["/usr/local/bin/etcd"\]\s*$').Count) -Because "${name}:官方镜像是 distroless,显式写出启动的二进制,ETCD_* 由 etcd 自己按同名参数解析"
        Assert-NotMatch -Text $code -Pattern 'ALLOW_NONE_AUTHENTICATION' -Because "${name}:那是 bitnami 启动脚本的开关,etcd 本身不认,留着只会让人以为它有作用"
        $mount = Get-SingleCapture -Text $code -Pattern '(?m)^\s*mountPath:\s*(\S+)\s*$' -What "$name 的数据卷挂载点"
        Assert-Equal -Expected '/var/lib/etcd' -Actual $mount -Because "${name}:etcd 的惯用位置,也是官方镜像的工作目录"
        $dataDir = Get-StatefulSetEnv -Text $code -Name 'ETCD_DATA_DIR' -Label $name
        Assert-Equal -Expected '/var/lib/etcd/data' -Actual $dataDir -Because "${name}:数据目录是 PVC 下的 data/ 子目录(与 bitnami 时代的布局相同,已有 PVC 里的数据照用)"
        Assert-Match -Text $code -Pattern '(?m)^\s*runAsNonRoot:\s*true\s*$' -Because "${name}:etcd 不需要 root"
        Assert-Match -Text $code -Pattern '(?m)^\s*fsGroup:\s*1001\s*$' -Because "${name}:云盘 / 本地盘挂上来是 root:root 0755,不给 fsGroup 进程建不了数据目录"
    }
}

Test-Case 'compose:数据目录就是命名卷的挂载点;调试栈的健康检查直接跑 etcdctl' {
    foreach ($case in @(@{ Label = 'compose'; Path = $script:ComposePath }, @{ Label = '调试 compose'; Path = $script:DebugComposePath })) {
        $block = Get-ComposeEtcdBlock -Path $case.Path
        $mount = Get-SingleCapture -Text $block -Pattern '(?m)^\s*-\s*etcd-data:(\S+?)\s*(?:#.*)?$' -What "$($case.Label) 的 etcd-data 挂载点"
        Assert-Equal -Expected $mount -Actual (Get-ComposeEnv -Block $block -Name 'ETCD_DATA_DIR' -Label $case.Label) -Because "$($case.Label):命名卷的根就是数据目录(bitnami 时代的布局),换挂载点不换布局,已有数据照用"
    }
    $debug = Get-ComposeEtcdBlock -Path $script:DebugComposePath
    Assert-Match -Text $debug -Pattern '(?m)^\s*test:\s*\[CMD,\s*/usr/local/bin/etcdctl,' -Because '官方镜像没有 shell,健康检查只能用 exec 形式直接跑 etcdctl'
}

Test-Case '取值:主 etcd 与两份 compose 的压缩模式、保留量、配额相同;保留量是不小于 100000 的整数;三套 StatefulSet 都满足「配额 × 2 + 1Gi ≤ PVC」' {
    $etcd = Get-NonCommentText (Read-RepoText $script:EtcdPath)
    foreach ($name in 'ETCD_AUTO_COMPACTION_MODE', 'ETCD_AUTO_COMPACTION_RETENTION', 'ETCD_QUOTA_BACKEND_BYTES') {
        $expected = Get-StatefulSetEnv -Text $etcd -Name $name -Label 'etcd.yaml'
        foreach ($case in @(@{ Label = 'compose'; Path = $script:ComposePath }, @{ Label = '调试 compose'; Path = $script:DebugComposePath })) {
            Assert-Equal -Expected $expected -Actual (Get-ComposeEnv -Block (Get-ComposeEtcdBlock -Path $case.Path) -Name $name -Label $case.Label) -Because "$($case.Label) 与 K8s 主 etcd 用同一组压缩和配额取值($name)"
        }
    }
    Assert-Equal -Expected 'revision' -Actual (Get-StatefulSetEnv -Text $etcd -Name 'ETCD_AUTO_COMPACTION_MODE' -Label 'etcd.yaml') -Because '按 revision 保留(periodic 模式的单位是时间,数字含义完全不同)'
    $keep = Get-StatefulSetEnv -Text $etcd -Name 'ETCD_AUTO_COMPACTION_RETENTION' -Label 'etcd.yaml'
    Assert-Match -Text $keep -Pattern '^[1-9][0-9]*$' -Because "保留量必须是不带前导 0 的正整数(实际 '$keep')"
    Assert-True -Condition ([long]$keep -ge 100000) -Because "保留量太小时 watch 的历史窗口只剩几秒到几分钟(实际 $keep;取值与时间窗见 runbook §3.1)"

    $gi = [long]1024 * 1024 * 1024
    foreach ($name in $script:StatefulSetPaths.Keys) {
        $code = Get-NonCommentText (Read-RepoText $script:StatefulSetPaths[$name])
        Assert-Equal -Expected 'revision' -Actual (Get-StatefulSetEnv -Text $code -Name 'ETCD_AUTO_COMPACTION_MODE' -Label $name) -Because "${name}:自动压缩是唯一的压缩手段(没有 CronJob 了),必须开"
        $quotaText = Get-StatefulSetEnv -Text $code -Name 'ETCD_QUOTA_BACKEND_BYTES' -Label $name
        Assert-Match -Text $quotaText -Pattern '^[1-9][0-9]*$' -Because "${name}:ETCD_QUOTA_BACKEND_BYTES 必须是字节数的正整数(实际 '$quotaText')"
        $pvcGi = [long](Get-SingleCapture -Text $code -Pattern '(?m)^\s*storage:\s*(\d+)Gi\s*$' -What "$name 的 PVC storage")
        Assert-True -Condition (([long]$quotaText * 2 + $gi) -le ($pvcGi * $gi)) -Because "${name}:defrag 期间需要临时空间:配额 $quotaText × 2 + 1Gi 必须 ≤ PVC ${pvcGi}Gi(volumeClaimTemplates 不能在线改)"
    }
}

Test-Case '可观测:三套都带抓取注解;主 etcd 的探针排除 NOSPACE' {
    foreach ($name in $script:StatefulSetPaths.Keys) {
        $code = Get-NonCommentText (Read-RepoText $script:StatefulSetPaths[$name])
        Assert-Match -Text $code -Pattern 'prometheus\.io/scrape:\s*"true"' -Because "${name}:空间与无主告警要靠 etcd 自己的 /metrics(deploy/k8s/etcd-alerts.yaml)"
        Assert-Match -Text $code -Pattern 'prometheus\.io/port:\s*"2379"' -Because "${name}:etcd 的指标在 client 端口上"
    }
    $code = Get-NonCommentText (Read-RepoText $script:EtcdPath)
    $readinessPath = Get-SingleCapture -Text $code -Pattern 'readinessProbe:\s*\n\s*httpGet:\s*\n\s*path:\s*(\S+)' -What 'etcd.yaml 的 readinessProbe path'
    $livenessPath = Get-SingleCapture -Text $code -Pattern 'livenessProbe:\s*\n\s*httpGet:\s*\n\s*path:\s*(\S+)' -What 'etcd.yaml 的 livenessProbe path'
    Assert-Equal -Expected '/health?exclude=NOSPACE' -Actual $readinessPath -Because 'NOSPACE 告警时不把成员全摘掉(runbook §2 / §7)'
    Assert-Equal -Expected '/health?exclude=NOSPACE&serializable=true' -Actual $livenessPath -Because 'NOSPACE 告警时不把三个成员一起反复重启(runbook §2 / §7)'
}

Test-Case '压缩 CronJob 已退役:清单删除;Apply-Infra 不再 apply 它,在 etcd.yaml 那一支删掉集群里的残留' {
    Assert-True -Condition (-not (Test-Path -LiteralPath $script:MaintenancePath)) -Because 'etcd-maintenance.yaml 已退役(官方镜像没有 shell,内置压缩做同一件事),不能再出现'
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($script:DeployPath, [ref]$null, [ref]$null)
    $applyInfra = $ast.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    Assert-True -Condition ($null -ne $applyInfra) -Because '夹具前提:k8s_deploy.ps1 里有 Apply-Infra'
    $listAssign = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$infraManifests' -and $a.Operator -eq 'Equals' }, $true))
    Assert-Equal -Expected 1 -Actual $listAssign.Count -Because '夹具前提:$infraManifests 恰好初始化一次'
    Assert-NotMatch -Text $listAssign[0].Right.Extent.Text -Pattern 'etcd-maintenance' -Because '$infraManifests 不能再带退役的清单'
    Assert-Match -Text $listAssign[0].Right.Extent.Text -Pattern '"etcd\.yaml"' -Because '主 etcd 照常下发'

    $etcdBranch = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.IfStatementAst] -and $a.Clauses[0].Item1.Extent.Text -eq '$manifest -eq "etcd.yaml"' }, $true))
    Assert-Equal -Expected 1 -Actual $etcdBranch.Count -Because '夹具前提:循环里有 etcd.yaml 那一支'
    $branchText = $etcdBranch[0].Clauses[0].Item2.Extent.Text
    foreach ($target in @('"cronjob", "etcd-compact"', '"configmap", "etcd-maintenance-scripts"')) {
        Assert-Match -Text $branchText -Pattern ('Invoke-Kubectl -Args @\("delete", ' + [regex]::Escape($target) + ', "-n", \$InfraNamespace, "--ignore-not-found"\)') -Because "以前 apply 过的 $target 要删掉(--ignore-not-found:没有就是空操作),否则旧 CronJob 每 5 分钟拉一次不再钉版本的旧镜像"
    }
}

Test-Case 'etcd-alerts.yaml:PrometheusRule,有无主、空间(80% / 95%)、增长过快与碎片告警,只用低基数 label' {
    $code = Get-NonCommentText (Read-RepoText $script:AlertsPath)
    Assert-Match -Text $code -Pattern '(?m)^kind:\s*PrometheusRule\s*$' -Because '与仓库里其他告警文件同一种安装方式'
    foreach ($rule in @(
        @{ Alert = 'EtcdNoLeader'; Expr = 'etcd_server_has_leader\s*==\s*0'; Severity = 'critical' }
        @{ Alert = 'EtcdDatabaseQuotaHighUsage'; Expr = 'etcd_mvcc_db_total_size_in_bytes[^\n]*etcd_server_quota_backend_bytes[^\n]*>\s*80'; Severity = 'warning' }
        @{ Alert = 'EtcdDatabaseQuotaLowSpace'; Expr = 'etcd_mvcc_db_total_size_in_bytes[^\n]*etcd_server_quota_backend_bytes[^\n]*>\s*95'; Severity = 'critical' }
        @{ Alert = 'EtcdExcessiveDatabaseGrowth'; Expr = 'predict_linear\(etcd_mvcc_db_total_size_in_bytes'; Severity = 'warning' }
        @{ Alert = 'EtcdDatabaseHighFragmentationRatio'; Expr = 'etcd_mvcc_db_total_size_in_use_in_bytes'; Severity = 'warning' }
    )) {
        $m = [regex]::Match($code, '(?s)- alert:\s*' + [regex]::Escape($rule.Alert) + '\s*\n(.*?)(?=\n\s*- alert:|\z)')
        Assert-True -Condition $m.Success -Because "要有告警 $($rule.Alert)(没有 CronJob 之后,空间问题只能从 etcd 自己的指标看出来)"
        Assert-Match -Text $m.Groups[1].Value -Pattern $rule.Expr -Because "$($rule.Alert) 的表达式"
        Assert-Match -Text $m.Groups[1].Value -Pattern ('severity:\s*' + $rule.Severity) -Because "$($rule.Alert) 的级别"
    }
    Assert-NotMatch -Text $code -Pattern 'player_id' -Because '告警不得用玩家维度做 label(高基数)'
}

Test-Case '官方镜像没有 shell:脚本、部署说明与运维手册里 exec 进 etcd 的命令一律直接跑 etcdctl,不开 sh / bash' {
    $files = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'tools/scripts') -Filter '*.ps1' -File | ForEach-Object { $_.FullName })
    $files += @($script:RunbookPath, (Join-Path $script:RepoRoot 'deploy/k8s/README.md'))
    foreach ($path in $files) {
        $lines = (Read-RepoText $path) -split "`n"
        for ($i = 0; $i -lt $lines.Count; $i++) {
            $line = $lines[$i]
            if ($line.TrimStart().StartsWith('#')) { continue }
            # 只看「exec 进 etcd」的那种行:kubectl / docker 的 exec,目标是 etcd 的 Pod、Service 或 compose 容器。
            if ($line -notmatch '\bexec\b' -or $line -notmatch "(?:\betcd(?:-id-(?:secondary|registry))?-\d\b|svc/etcd\b|['""]etcd['""]|\bexec\s+(?:-\S+\s+)*etcd\b)") { continue }
            # 三种写法都算开 shell:kubectl 的 `-- sh`、docker 的 `exec [选项] etcd sh`、参数数组里的 `'etcd','sh'`。
            $shellPattern = "(?:--\s+|['""],\s*|\bexec\s+(?:-\S+\s+)*etcd(?:-\S+)?\s+)['""]?(?:/bin/)?(?:sh|bash)['""]?(?=\s|,|\)|$)"
            Assert-NotMatch -Text $line -Pattern $shellPattern -Because "$([IO.Path]::GetFileName($path)):$($i + 1) exec 进 etcd 开了 shell,官方镜像(distroless)里没有:$($line.Trim())"
        }
    }
}

exit (Complete-TestRun -SuiteName 'etcd_maintenance_contract')
