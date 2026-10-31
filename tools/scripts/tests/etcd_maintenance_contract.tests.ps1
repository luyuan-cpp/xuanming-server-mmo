#requires -Version 7
<#
.SYNOPSIS
    etcd MVCC 历史压缩(P0-C)的静态契约:CronJob 清单、compose 与 StatefulSet 的压缩 / 配额 env、探针和抓取注解。

.DESCRIPTION
    只读仓库里的静态文件:不调用 k8s_deploy.ps1,不访问集群、容器或网络。
    「infra-up 的 DryRun 产物里有 etcd-compact」(阶段 1b)要跑部署脚本,放在 k8s_deploy_contract.tests.ps1 的 Kafka 清单用例之后。

    守住的东西:
      - CronJob 的形态:名字、调度只用 */N 分钟、不叠跑、不重试、失败留痕、Job 有小于调度间隔的 activeDeadlineSeconds(防卡死的 Job 让 Forbid
        永久跳过后续调度)、标签不能是 app: etcd、占位只用 __INFRA_NAMESPACE__;
      - 压缩脚本:目标来自线性一致读,不做 defrag、不解除告警,查告警不经管道(否则查询失败等于「没有告警」);
      - 取值:KEEP_REVISIONS 与 compose 的 ETCD_AUTO_COMPACTION_RETENTION 同值,配额满足「配额 × 2 + 1Gi ≤ PVC」;
      - 镜像:清单、etcd.yaml、compose 三处逐字相同,钉版本只能三处同批改;
      - 阶段 2:StatefulSet 的 env 与 compose 相等,抓取注解在,探针排除 NOSPACE。镜像的 tag@摘要待 Codex 实测 M0 后三处同批钉。

    设计与取值:docs/handoff/global-singletons-p0-findings-20261014.md「## P0-C」;运维:docs/ops/etcd-maintenance-runbook.md。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

# 路径一律正斜杠 + Join-Path:CI 在 ubuntu 上跑(.github/workflows/deploy-config-tests.yml)。
$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:MaintenancePath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/etcd-maintenance.yaml'
$script:EtcdPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/etcd.yaml'
$script:ComposePath = Join-Path $script:RepoRoot 'deploy/docker-compose.yml'

# 读文件放在各用例里调用:文件缺失时每条用例各自报红(修复前跑这份测试应当逐条点名缺什么),而不是整个脚本中断。
function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { throw "找不到 $Path" }
    return ([IO.File]::ReadAllText($Path) -replace "`r`n", "`n")
}

# 去掉整行注释(YAML 注释与脚本注释都是去掉缩进后以 # 开头),只对真正生效的内容断言。
function Get-NonCommentText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return ((($Text -split "`n") | Where-Object { -not $_.TrimStart().StartsWith('#') }) -join "`n")
}

# 取 `<Key>: |` 字面块的正文(去掉公共缩进)。runbook 与验证步骤从清单里取脚本原文也是这个写法。
function Get-BlockScalar {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Key
    )
    $lines = $Text -split "`n"
    $start = -1
    $indent = 0
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $m = [regex]::Match($lines[$i], '^(\s*)' + [regex]::Escape($Key) + ':\s*\|\s*$')
        if ($m.Success) {
            $start = $i + 1
            $indent = $m.Groups[1].Value.Length
            break
        }
    }
    if ($start -lt 0) { throw "找不到字面块 ${Key}: |" }

    $body = [System.Collections.Generic.List[string]]::new()
    for ($i = $start; $i -lt $lines.Count; $i++) {
        $l = $lines[$i]
        if ($l.Trim().Length -eq 0) { $body.Add(''); continue }
        if (($l.Length - $l.TrimStart().Length) -le $indent) { break }
        $body.Add($l)
    }
    $nonEmpty = @($body | Where-Object { $_.Length -gt 0 })
    if ($nonEmpty.Count -eq 0) { throw "字面块 $Key 是空的" }
    $min = ($nonEmpty | ForEach-Object { $_.Length - $_.TrimStart().Length } | Measure-Object -Minimum).Minimum
    return ((($body | ForEach-Object { if ($_.Length -ge $min) { $_.Substring($min) } else { '' } }) -join "`n").TrimEnd() + "`n")
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

function Get-ComposeEtcdEnv {
    param([Parameter(Mandatory = $true)][string]$Name)
    $block = Get-NonCommentText (Get-ComposeServiceBlock -Text (Read-RepoText $script:ComposePath) -Service 'etcd')
    return Get-SingleCapture -Text $block -Pattern ('(?m)^\s*-\s*' + [regex]::Escape($Name) + '=(\S*)\s*(?:#.*)?$') -What "compose etcd 服务的 $Name"
}

function Get-KeepRevisions {
    $text = Get-NonCommentText (Read-RepoText $script:MaintenancePath)
    return Get-SingleCapture -Text $text -Pattern 'name:\s*KEEP_REVISIONS\s*\n\s*value:\s*"?([^"\s]*)"?' -What 'etcd-maintenance.yaml 的 KEEP_REVISIONS'
}

Write-Host "== etcd MVCC 压缩契约(P0-C)=="

Test-Case 'etcd-maintenance.yaml 只含 ConfigMap 与 CronJob,不会改到 StatefulSet etcd 的模板' {
    $text = Get-NonCommentText (Read-RepoText $script:MaintenancePath)
    $kinds = @([regex]::Matches($text, '(?m)^kind:\s*(\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
    Assert-Equal -Expected 'ConfigMap,CronJob' -Actual ($kinds -join ',') -Because '维护清单只有这两种对象,etcd 本身的改动都在 etcd.yaml'
}

Test-Case 'CronJob etcd-compact:不叠跑、同一轮不重试、Pod 不重启、失败记录保留' {
    $text = Get-NonCommentText (Read-RepoText $script:MaintenancePath)
    Assert-Match -Text $text -Pattern 'kind:\s*CronJob\s*\nmetadata:\s*\n\s+name:\s*etcd-compact\s*\n' -Because 'CronJob 名固定为 etcd-compact(runbook 的手工触发命令、infra-status 与契约测试都按这个名字找)'
    # 调度只认「每 N 分钟」(*/N * * * *)这一种写法:下面 activeDeadlineSeconds 的上界由 N 推出,别的写法算不出间隔,一律判红。
    $everyMinutes = [int](Get-SingleCapture -Text $text -Pattern '(?m)^\s*schedule:\s*"\*/([1-9][0-9]*) \* \* \* \*"\s*$' -What 'CronJob 的 schedule(*/N * * * *)')
    Assert-Match -Text $text -Pattern '(?m)^\s*concurrencyPolicy:\s*Forbid\s*$' -Because '首次在涨大的库上压缩可能跑得久,不能两轮叠在一起'
    Assert-Match -Text $text -Pattern '(?m)^\s*backoffLimit:\s*0\s*$' -Because '失败要可见:同一轮不重试,下一轮调度就是重试'
    Assert-Match -Text $text -Pattern '(?m)^\s*restartPolicy:\s*Never\s*$' -Because 'Pod 原地重启会把失败藏进 RESTARTS 计数'
    $failedHistory = [int](Get-SingleCapture -Text $text -Pattern '(?m)^\s*failedJobsHistoryLimit:\s*(\d+)\s*$' -What 'failedJobsHistoryLimit')
    Assert-True -Condition ($failedHistory -ge 1) -Because "failedJobsHistoryLimit 至少 1,失败的 Job 及其日志才留得下来(实际 $failedHistory)"
    # activeDeadlineSeconds 是 Forbid 下唯一防「一个卡住的 Job 永久挡住后续调度」的东西:Pod 卡在拉镜像(latest + IfNotPresent,
    # 调度到没有镜像的节点)时 Job 一直 Active,Forbid 跳过之后每一轮,也不产生 Failed 记录,压缩就此停摆。
    # 它必须小于调度间隔(上面 schedule 的 N 分钟),否则一个跑满时限的 Job 仍会吞掉下一轮。
    $intervalSeconds = $everyMinutes * 60
    $activeDeadline = [int](Get-SingleCapture -Text $text -Pattern '(?m)^\s*activeDeadlineSeconds:\s*(\d+)\s*$' -What 'Job 的 activeDeadlineSeconds')
    Assert-True -Condition ($activeDeadline -gt 0 -and $activeDeadline -lt $intervalSeconds) -Because "activeDeadlineSeconds 必须在 (0, 调度间隔 ${intervalSeconds}s) 内,让卡住的 Job 在下一轮调度之前以 Failed 结束(实际 $activeDeadline)"
    # startingDeadlineSeconds 管的是控制器停摆时错过的调度点(不补跑),不是上面那种卡死的兜底;缺了它控制器恢复后可能报错过太多调度而停调度。
    Assert-Match -Text $text -Pattern '(?m)^\s*startingDeadlineSeconds:\s*\d+\s*$' -Because '控制器停摆后错过的调度点直接跳过,不补跑'
}

Test-Case '标签一律 app: etcd-maintenance,绝不能是 app: etcd' {
    $text = Get-NonCommentText (Read-RepoText $script:MaintenancePath)
    $values = @([regex]::Matches($text, '(?m)^\s*app:\s*"?([^"\s#]+)"?') | ForEach-Object { $_.Groups[1].Value })
    Assert-True -Condition ($values.Count -ge 3) -Because "ConfigMap、CronJob、Pod 模板都要打标签(实际找到 $($values.Count) 个 app: 标签)"
    foreach ($v in $values) {
        Assert-Equal -Expected 'etcd-maintenance' -Actual $v -Because 'app: etcd 会被 etcd 的 Service / PDB / StatefulSet 的 selector 选中(etcd.yaml),本 Pod 不监听 2379,进了 Service 就是黑洞'
    }
    # (?-i):断言助手默认不分大小写,不关掉的话 jobTemplate: 里的 Template: 也会被当成 Pod 模板。
    Assert-Match -Text $text -Pattern '(?m-i)^\s+template:\s*\n\s*metadata:\s*\n\s*labels:\s*\n\s*app:\s*etcd-maintenance\s*$' -Because 'Pod 模板本身的标签才是 selector 会看的那一个'
}

Test-Case '占位只用 __INFRA_NAMESPACE__,端点逐个列出 etcd.yaml 的全部成员' {
    $text = Read-RepoText $script:MaintenancePath
    # Matches 默认区分大小写:占位是全大写,注释里也算(Apply-Infra 的 fail-closed 检查扫的是整份文本)。
    $placeholders = @([regex]::Matches($text, '__[A-Z][A-Z0-9_]*__') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected '__INFRA_NAMESPACE__' -Actual ($placeholders -join ',') -Because 'Apply-Infra 对通用清单只替换 __INFRA_NAMESPACE__,别的占位会被它的 fail-closed 检查拦下,infra-up 整体失败'

    $replicas = [int](Get-SingleCapture -Text (Get-NonCommentText (Read-RepoText $script:EtcdPath)) -Pattern '(?m)^  replicas:\s*(\d+)\s*$' -What 'etcd.yaml 的 StatefulSet replicas')
    $endpoints = Get-SingleCapture -Text (Get-NonCommentText $text) -Pattern 'name:\s*ETCDCTL_ENDPOINTS\s*\n\s*value:\s*"([^"]*)"' -What 'ETCDCTL_ENDPOINTS'
    $expected = @(0..($replicas - 1) | ForEach-Object { "http://etcd-$_.etcd-headless.__INFRA_NAMESPACE__.svc.cluster.local:2379" }) -join ','
    Assert-Equal -Expected $expected -Actual $endpoints -Because 'endpoint status 要覆盖全部成员;成员地址与 etcd.yaml 的广播地址同形(headless DNS + 同一个 namespace 占位)'
}

Test-Case '压缩脚本:目标来自线性一致读;不做 defrag、不解除告警;查告警不经管道' {
    $scriptText = Get-BlockScalar -Text (Read-RepoText $script:MaintenancePath) -Key 'compact.sh'
    $code = Get-NonCommentText $scriptText
    Assert-Match -Text $code -Pattern '\$\{KEEP_REVISIONS:\?' -Because '没给保留量就失败,不能按 0 处理把历史全压掉'
    Assert-Match -Text $code -Pattern 'etcdctl get [^\n]*--consistency=l' -Because '压缩目标只能来自线性一致读'
    Assert-Match -Text $code -Pattern 'etcdctl compact ' -Because '脚本的本职是压缩'
    Assert-Match -Text $code -Pattern 'etcdctl alarm list' -Because '有活动告警(多半是 NOSPACE)时要以非 0 退出,让 Job 失败可见'
    Assert-NotMatch -Text $code -Pattern '(?m)alarm list[^\n]*(?<!\|)\|(?!\|)' -Because '告警查询接管道时,查询失败会被当成「没有告警」(fail-open)'
    Assert-NotMatch -Text $code -Pattern 'defrag' -Because 'defrag 会阻塞所在成员,只走 runbook 的手工逐成员流程'
    Assert-NotMatch -Text $code -Pattern 'alarm\s+disarm' -Because '解除告警必须由人确认空间已腾出之后再做,自动解除会让告警反复触发而无人知道'
    # 整份清单(含 CronJob 的 command)也不得出现 defrag。
    Assert-NotMatch -Text (Get-NonCommentText (Read-RepoText $script:MaintenancePath)) -Pattern 'defrag' -Because '清单里任何生效内容都不得做 defrag'
}

Test-Case 'KEEP_REVISIONS 是不小于 100000 的整数,且与 compose 的 ETCD_AUTO_COMPACTION_RETENTION 同值;compose 按 revision 模式' {
    $keep = Get-KeepRevisions
    Assert-Match -Text $keep -Pattern '^[1-9][0-9]*$' -Because "KEEP_REVISIONS 必须是不带前导 0 的正整数(实际 '$keep')"
    Assert-True -Condition ([long]$keep -ge 100000) -Because "保留量太小时 watch 的历史窗口只剩几秒到几分钟(实际 $keep;取值与时间窗见 runbook)"
    Assert-Equal -Expected 'revision' -Actual (Get-ComposeEtcdEnv -Name 'ETCD_AUTO_COMPACTION_MODE') -Because 'compose 的自动压缩必须按 revision 保留,与 CronJob 的语义一致(periodic 模式的单位是时间,数字含义完全不同)'
    Assert-Equal -Expected $keep -Actual (Get-ComposeEtcdEnv -Name 'ETCD_AUTO_COMPACTION_RETENTION') -Because 'compose 与 K8s CronJob 用同一个保留量'
}

Test-Case 'compose 的配额是正整数,且满足「配额 × 2 + 1Gi ≤ etcd.yaml 的 PVC」' {
    $quotaText = Get-ComposeEtcdEnv -Name 'ETCD_QUOTA_BACKEND_BYTES'
    Assert-Match -Text $quotaText -Pattern '^[1-9][0-9]*$' -Because "ETCD_QUOTA_BACKEND_BYTES 必须是字节数的正整数(实际 '$quotaText')"
    $quota = [long]$quotaText
    $pvcGi = [long](Get-SingleCapture -Text (Get-NonCommentText (Read-RepoText $script:EtcdPath)) -Pattern '(?m)^\s*storage:\s*(\d+)Gi\s*$' -What 'etcd.yaml 的 PVC storage')
    $gi = [long]1024 * 1024 * 1024
    Assert-True -Condition (($quota * 2 + $gi) -le ($pvcGi * $gi)) -Because "defrag 期间需要临时空间:配额 $quota × 2 + 1Gi 必须 ≤ PVC ${pvcGi}Gi(PVC 的 volumeClaimTemplates 不能在线改)"
}

Test-Case 'etcd 镜像:etcd-maintenance.yaml、etcd.yaml、compose 三处逐字相同' {
    $maintenanceImage = Get-SingleCapture -Text (Get-NonCommentText (Read-RepoText $script:MaintenancePath)) -Pattern '(?m)^\s*image:\s*(\S+)\s*$' -What 'etcd-maintenance.yaml 的 image'
    $etcdImage = Get-SingleCapture -Text (Get-NonCommentText (Read-RepoText $script:EtcdPath)) -Pattern '(?m)^\s*image:\s*(\S+)\s*$' -What 'etcd.yaml 的 image'
    $composeImage = Get-SingleCapture -Text (Get-NonCommentText (Get-ComposeServiceBlock -Text (Read-RepoText $script:ComposePath) -Service 'etcd')) -Pattern '(?m)^\s*image:\s*(\S+)\s*$' -What 'compose etcd 服务的 image'
    Assert-Equal -Expected $etcdImage -Actual $maintenanceImage -Because 'CronJob 的 etcdctl 与服务端同一个镜像;钉 tag@摘要只能与 etcd.yaml 同批改(待 Codex 实测 M0)'
    Assert-Equal -Expected $etcdImage -Actual $composeImage -Because '本机 / 预发与 K8s 跑同一个 etcd 镜像;钉版本三处同批改'
}

Test-Case '阶段 2:StatefulSet 的压缩与配额 env 与 compose 相等,抓取注解在,探针排除 NOSPACE' {
    $code = Get-NonCommentText (Read-RepoText $script:EtcdPath)
    foreach ($name in 'ETCD_AUTO_COMPACTION_MODE', 'ETCD_AUTO_COMPACTION_RETENTION', 'ETCD_QUOTA_BACKEND_BYTES') {
        $value = Get-SingleCapture -Text $code -Pattern ('name:\s*' + [regex]::Escape($name) + '\s*\n\s*value:\s*"?([^"\s]*)"?') -What "etcd.yaml 的 $name"
        Assert-Equal -Expected (Get-ComposeEtcdEnv -Name $name) -Actual $value -Because 'K8s 与 compose 用同一组压缩和配额取值'
    }
    Assert-Match -Text $code -Pattern 'prometheus\.io/scrape:\s*"true"' -Because '告警要靠 etcd 自己的 /metrics(runbook §8 步骤 1)'
    Assert-Match -Text $code -Pattern 'prometheus\.io/port:\s*"2379"' -Because 'etcd 的指标在 client 端口上'
    $readinessPath = Get-SingleCapture -Text $code -Pattern 'readinessProbe:\s*\n\s*httpGet:\s*\n\s*path:\s*(\S+)' -What 'etcd.yaml 的 readinessProbe path'
    $livenessPath = Get-SingleCapture -Text $code -Pattern 'livenessProbe:\s*\n\s*httpGet:\s*\n\s*path:\s*(\S+)' -What 'etcd.yaml 的 livenessProbe path'
    Assert-Equal -Expected '/health?exclude=NOSPACE' -Actual $readinessPath -Because 'NOSPACE 告警时不把成员全摘掉(runbook §2 / §7)'
    Assert-Equal -Expected '/health?exclude=NOSPACE&serializable=true' -Actual $livenessPath -Because 'NOSPACE 告警时不把三个成员一起反复重启(runbook §2 / §7)'
}

exit (Complete-TestRun -SuiteName 'etcd_maintenance_contract')
