#requires -Version 7
<#
.SYNOPSIS
    缩容收尾 CronJob(sql-writer-slot-retirer-<StatefulSet>)的部署契约。

.DESCRIPTION
    缺口清单第 22 条残余(docs/design/writer-roster-exhaustion-20261106.md §5.8)。写者缩容之后,被缩掉序号的末代由这份 CronJob
    跑 writer_authority -command retire-removed-slots 结束。这里钉的都是「错了不会报错、出事才发现」的东西:
      - 凭据就是这个写者 registrar 的那几样(同一个镜像、名册 Secret、注册账号 Secret、库映射与 MMORPG_SQL_BACKEND),不多给,
        运行配置 Secret 不挂;
      - 集群权限只有这一个 StatefulSet、pods 与 serviceaccounts 的 get,不能列、不能改(serviceaccounts 用来核一代是不是本 namespace 登记的,§5.9「归属」);
      - 命令行就是 Go 侧 retire-removed-slots 收的那几个参数,名册路径与 registrar 读的同一个;
      - 每个写者(Go 的稳定版与金丝雀、Java 的 gateway)都在 apply 之前渲染、在写者 apply 之后落地;金丝雀收尾删 StatefulSet 时不删它,
        它把金丝雀各槽位的末代结束掉。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/sql_writer_slot_retirer_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:DeployPath = Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $Path))) -replace "`r`n", "`n"
}

# 渲染、落地与库映射注入三个函数按 AST 抽出来直接调用(与 audit_pipeline_deploy_contract 同一做法);
# 它们依赖的目录条目、拉取策略与库映射计划换成桩,只核这三个函数自己的拼装。
$tokens = $null
$parseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile($script:DeployPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败: $($parseErrors[0].Message)" }
$script:DeployFunctions = @{}
foreach ($fn in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    $script:DeployFunctions[$fn.Name] = $fn
}
$script:RetirementFunctions = @('Get-SqlWriterLiveReplicas', 'Complete-SqlWriterSlotRetirement', 'Complete-SqlWriterScaleDown', 'Complete-SqlWriterCanaryRemoval')
foreach ($name in @('New-SqlWriterSlotRetirerManifest', 'Apply-SqlWriterSlotRetirer', 'Add-SqlWriterPlacementEnv', 'Apply-OneGoSvc', 'Apply-GlobalGoSvcCanaries', 'Apply-JavaSvcManifests') + $script:RetirementFunctions) {
    if (-not $script:DeployFunctions.ContainsKey($name)) { throw "k8s_deploy.ps1 缺少函数 $name" }
}
foreach ($name in @('New-SqlWriterSlotRetirerManifest', 'Apply-SqlWriterSlotRetirer', 'Add-SqlWriterPlacementEnv')) {
    Set-Item -Path "Function:script:$name" -Value $script:DeployFunctions[$name].Body.GetScriptBlock()
}
# 当场收尾的四个函数按需装上真函数(Use-RealRetirementFunction),用例之间互相替换成桩时不串。
function Use-RealRetirementFunction {
    param([Parameter(Mandatory = $true)][string[]]$Name)
    foreach ($one in $Name) { Set-Item -Path "Function:script:$one" -Value $script:DeployFunctions[$one].Body.GetScriptBlock() }
}

# 部署脚本的全局参数与桩。
$GoSvcManifestsDir = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc'
$DatabaseBackend = 'tidb'
$ReleaseProfile = 'staging'
$GlobalAuthorityTiDBHost = 'tidb.invalid:4000'
$FriendTiDBHost = ''
$TradeTiDBHost = ''
$MatchTiDBHost = ''
$InfraNamespace = 'mmorpg-infra'
$script:CatalogLookups = New-Object System.Collections.Generic.List[string]
$script:KubectlCalls = New-Object System.Collections.Generic.List[object]
$script:FixtureEntry = [ordered]@{
    requestSecret           = 'fixture-sql-writer-request'
    registrarSecret         = 'fixture-sql-writer-registrar'
    runtimeConfigSecret     = 'fixture-sql-writer-runtime'
    registrarImage          = 'registry.invalid/mmorpg-writer-authority@sha256:' + ('a' * 64)
    physicalAuthoritySha256 = ('b' * 64)
    rosterSha256            = ('c' * 64)
    replicas                = 2
}
function Get-SqlWriterDeploymentEntry {
    param([string]$Namespace, [string]$SvcName)
    $script:CatalogLookups.Add("$Namespace/$SvcName")
    return $script:FixtureEntry
}
function Resolve-ImagePullPolicy { param([string]$ImageRef) return 'IfNotPresent' }
function Get-GlobalAuthoritySqlSettings { param($Profile, $CanonicalHost, $FriendHost, $TradeHost, $MatchHost, $DevelopmentHost, $Backend) return @{ Host = $CanonicalHost } }
function Get-DbPlacementPlan { param($SqlHost) return @{ SqlHost = $SqlHost } }
function ConvertTo-DbPlacementJson { param($Plan) return '{"fixture":"placement"}' }
function Invoke-KubectlWithInputFile {
    param([string[]]$Args, [string]$InputContent)
    $script:KubectlCalls.Add([pscustomobject]@{ Args = ($Args -join ' '); InputContent = $InputContent })
}

# 按 --- 切成文档,取 kind 对应的那一份。
function Get-ManifestDocument {
    param([Parameter(Mandatory = $true)][string]$Manifest, [Parameter(Mandatory = $true)][string]$Kind)
    $docs = @(($Manifest -replace "`r`n", "`n") -split '(?m)^---[ \t]*$' | Where-Object { $_ -match "(?m)^kind: $Kind[ \t]*$" })
    if ($docs.Count -ne 1) { throw "清单里 kind: $Kind 应恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

# CronJob 容器的 args(一行 JSON 数组)。
function Get-RetirerArgs {
    param([Parameter(Mandatory = $true)][string]$CronJob)
    $match = [regex]::Match($CronJob, '(?m)^\s+args: (\[[^\r\n]+\])\s*$')
    if (-not $match.Success) { throw 'CronJob 容器没有一行 JSON 形式的 args' }
    return @($match.Groups[1].Value | ConvertFrom-Json)
}

function Get-ArgValue {
    param([Parameter(Mandatory = $true)][object[]]$Arguments, [Parameter(Mandatory = $true)][string]$Flag)
    $at = [Array]::IndexOf($Arguments, $Flag)
    if ($at -lt 0 -or $at + 1 -ge $Arguments.Count) { return $null }
    return [string]$Arguments[$at + 1]
}

Write-Host ""
Write-Host "=== sql-writer-slot-retirer deployment contract tests ==="

Test-Case '渲染出的 CronJob 只带这个写者 registrar 的凭据,集群权限只有 get' {
    $script:CatalogLookups.Clear()
    $manifest = New-SqlWriterSlotRetirerManifest -Namespace 'zone-1' -StatefulSet 'data-service'
    Assert-NotMatch -Text $manifest -Pattern 'PLACEHOLDER_' -Because '占位都换掉'
    Assert-Equal -Expected 'zone-1/data-service' -Actual ($script:CatalogLookups -join ',') -Because '凭据按这个 StatefulSet 自己的目录条目取'
    foreach ($kind in @('ServiceAccount', 'Role', 'RoleBinding', 'CronJob')) {
        $doc = Get-ManifestDocument -Manifest $manifest -Kind $kind
        Assert-Match -Text $doc -Pattern '(?m)^  name: sql-writer-slot-retirer-data-service\s*$' -Because "$kind 按 StatefulSet 起名"
        Assert-Match -Text $doc -Pattern '(?m)^    mmorpg\.io/slot-retirer-for: data-service\s*$' -Because "$kind 标出它管哪个写者"
    }

    $role = Get-ManifestDocument -Manifest $manifest -Kind 'Role'
    Assert-Match -Text $role -Pattern '(?ms)resources: \["statefulsets"\]\s*\n\s*resourceNames: \["data-service"\]\s*\n\s*verbs: \["get"\]' -Because '只读这一个 StatefulSet 的副本数'
    Assert-Match -Text $role -Pattern '(?ms)resources: \["pods"\]\s*\n\s*verbs: \["get"\]' -Because '只查 Pod 在不在'
    Assert-Match -Text $role -Pattern '(?ms)resources: \["serviceaccounts"\]\s*\n\s*verbs: \["get"\]' -Because '核归属:读登记这一代的 ServiceAccount 现在的 UID(设计稿 §5.9「归属」)'
    $verbs = @([regex]::Matches($role, 'verbs: \[([^\]]*)\]') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected '"get","get","get"' -Actual ($verbs -join ',') -Because '没有 list、watch、create、update、patch、delete'
    Assert-NotMatch -Text $role -Pattern 'serviceaccounts/token|secrets' -Because '只读 ServiceAccount 对象本身,不能签发令牌,也不读 Secret'
    Assert-Match -Text (Get-ManifestDocument -Manifest $manifest -Kind 'RoleBinding') -Pattern '(?m)^    namespace: zone-1\s*$' -Because 'RoleBinding 的 subjects 写明 namespace'

    $cronJob = Get-ManifestDocument -Manifest $manifest -Kind 'CronJob'
    Assert-Match -Text $cronJob -Pattern ('(?m)^\s+image: ' + [regex]::Escape($script:FixtureEntry.registrarImage) + '\s*$') -Because '和 registrar 同一个按摘要钉住的镜像'
    Assert-Match -Text $cronJob -Pattern '(?m)^\s+imagePullPolicy: IfNotPresent\s*$' -Because '拉取策略按镜像引用算'
    Assert-Match -Text $cronJob -Pattern '(?ms)- name: MMORPG_SQL_OPERATOR_PRINCIPAL\s*\n\s*valueFrom:\s*\n\s*secretKeyRef: \{name: fixture-sql-writer-registrar, key: principal, optional: false\}' -Because '注册账号的主体来自 registrar Secret'
    Assert-Match -Text $cronJob -Pattern '(?ms)- name: MMORPG_SQL_REGISTRAR_DSN\s*\n\s*valueFrom:\s*\n\s*secretKeyRef: \{name: fixture-sql-writer-registrar, key: dsn, optional: false\}' -Because '注册账号的连接串来自 registrar Secret,不进 ConfigMap'
    Assert-Match -Text $cronJob -Pattern ('(?ms)- name: MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256\s*\n\s*value: "' + $script:FixtureEntry.physicalAuthoritySha256 + '"') -Because '物理 authority 摘要与 registrar 同一个'
    Assert-Match -Text $cronJob -Pattern '(?ms)- name: MMORPG_SQL_BACKEND\s*\n\s*value: "tidb"' -Because '按 -DatabaseBackend 核真实引擎,与 registrar 同值'
    Assert-Match -Text $cronJob -Pattern 'secretName: fixture-sql-writer-request' -Because '只挂名册所在的 Secret'
    Assert-NotMatch -Text $manifest -Pattern 'fixture-sql-writer-runtime' -Because '运行配置 Secret 这条命令不读,不挂'
    $placement = [regex]::Matches($cronJob, '(?m)^\s+- name: MMORPG_DB_PLACEMENT\s*$')
    Assert-Equal -Expected 1 -Actual $placement.Count -Because '库映射注入一次'
    Assert-True -Condition ($cronJob.IndexOf('- name: MMORPG_DB_PLACEMENT') -lt $cronJob.IndexOf('- name: MMORPG_SQL_REGISTRAR_DSN')) -Because '库映射插在注册账号 DSN 前面,与 registrar 同一份'
    Assert-Match -Text $cronJob -Pattern "value: '\{`"fixture`":`"placement`"\}'" -Because '库映射的值来自部署计划'

    Assert-Match -Text $cronJob -Pattern '(?m)^  schedule: "\*/5 \* \* \* \*"\s*$' -Because '缩容之后最多一轮(5 分钟)开始结束'
    Assert-Match -Text $cronJob -Pattern '(?m)^  concurrencyPolicy: Forbid\s*$' -Because '同一时刻只有一轮'
    Assert-Match -Text $cronJob -Pattern '(?m)^      backoffLimit: 0\s*$' -Because '失败不重试,下一轮按现状重判'
    Assert-Match -Text $cronJob -Pattern '(?m)^      ttlSecondsAfterFinished: 86400\s*$' -Because '部署脚本手工起的 Job 不算进历史上限,靠 TTL 回收'
    $deadline = [regex]::Match($cronJob, '(?m)^      activeDeadlineSeconds: (\d+)\s*$')
    Assert-True -Condition ($deadline.Success -and [int]$deadline.Groups[1].Value -gt 60 -and [int]$deadline.Groups[1].Value -lt 300) -Because 'Job 截止长于命令自己的 60 秒、短于调度周期,卡住的一轮不挡下一轮'
    Assert-Match -Text $cronJob -Pattern '(?m)^\s+restartPolicy: Never\s*$' -Because '容器不原地重启'
    Assert-Match -Text $cronJob -Pattern '(?m)^\s+runAsNonRoot: true\s*$' -Because '非 root'
    Assert-Match -Text $cronJob -Pattern '(?m)^\s+readOnlyRootFilesystem: true\s*$' -Because '只读根文件系统'
    Assert-Match -Text $cronJob -Pattern '(?ms)- name: POD_NAMESPACE\s*\n\s*valueFrom: \{fieldRef: \{fieldPath: metadata\.namespace\}\}' -Because '按自己所在的 namespace 读 StatefulSet 与 Pod'
    Assert-Match -Text $cronJob -Pattern '(?ms)- name: MMORPG_SQL_DEPLOYMENT_ID\s*\n\s*valueFrom: \{fieldRef: \{fieldPath: metadata\.uid\}\}' -Because 'RETIRE 的 deployment_id 带本 Job Pod 的 UID'
}

Test-Case '命令行是 Go 侧 retire-removed-slots 收的参数,名册路径与 registrar 读的同一个' {
    $cronJob = Get-ManifestDocument -Manifest (New-SqlWriterSlotRetirerManifest -Namespace 'zone-1' -StatefulSet 'db') -Kind 'CronJob'
    $arguments = Get-RetirerArgs -CronJob $cronJob
    Assert-Equal -Expected 'retire-removed-slots' -Actual (Get-ArgValue -Arguments $arguments -Flag '-command') -Because '命令名'
    Assert-Equal -Expected 'db' -Actual (Get-ArgValue -Arguments $arguments -Flag '-statefulset') -Because '名册所属的 StatefulSet'
    Assert-Equal -Expected $script:FixtureEntry.rosterSha256 -Actual (Get-ArgValue -Arguments $arguments -Flag '-roster-sha256') -Because '名册按已批准的原字节 SHA 读'
    Assert-Equal -Expected '$(MMORPG_SQL_DEPLOYMENT_ID)' -Actual (Get-ArgValue -Arguments $arguments -Flag '-deployment-id') -Because '本 Job Pod 的 UID'
    Assert-Equal -Expected '60s' -Actual (Get-ArgValue -Arguments $arguments -Flag '-timeout') -Because '命令自己的截止,注册器上限 60 秒'
    $roster = Get-ArgValue -Arguments $arguments -Flag '-roster'
    $root = Get-ArgValue -Arguments $arguments -Flag '-input-root'
    Assert-True -Condition ($roster.StartsWith($root + '/', [StringComparison]::Ordinal)) -Because '名册在 -input-root 之下'
    Assert-Match -Text (Read-RepoText 'deploy/k8s/manifests/go-svc/data-service.yaml') -Pattern ('"-roster","' + [regex]::Escape($roster) + '"') -Because '和 registrar init 读同一个名册文件'
    Assert-True -Condition ($cronJob.Contains('mountPath: ' + [IO.Path]::GetDirectoryName($roster).Replace('\', '/'))) -Because '名册所在目录挂着名册 Secret'
    $main = Read-RepoText 'go/data_service/cmd/writer_authority/main.go'
    Assert-Match -Text $main -Pattern 'case "retire-removed-slots":' -Because 'Go 侧有这条命令'
    foreach ($flag in @('-command', '-roster', '-roster-sha256', '-input-root', '-statefulset', '-deployment-id')) {
        Assert-True -Condition ($arguments -ccontains $flag) -Because "args 带 $flag"
        Assert-Match -Text $main -Pattern ('flags\.String\("' + [regex]::Escape($flag.Substring(1)) + '"') -Because "Go 侧认 $flag"
    }
    Assert-Equal -Expected 14 -Actual $arguments.Count -Because '没有 Go 侧不收的参数(7 个参数各带一个值)'
}

Test-Case '金丝雀用自己的目录条目,CronJob 名超过 52 个字符拒绝' {
    $script:CatalogLookups.Clear()
    $manifest = New-SqlWriterSlotRetirerManifest -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary'
    Assert-Equal -Expected 'mmorpg-infra/guild-canary' -Actual ($script:CatalogLookups -join ',') -Because '金丝雀 StatefulSet 有自己的名册与 Secret'
    Assert-Match -Text $manifest -Pattern '(?m)^  name: sql-writer-slot-retirer-guild-canary\s*$' -Because '按金丝雀 StatefulSet 起名'
    $tooLong = 'w' * 29
    $failure = $null
    try { $null = New-SqlWriterSlotRetirerManifest -Namespace 'zone-1' -StatefulSet $tooLong } catch { $failure = $_.Exception.Message }
    Assert-Match -Text $failure -Pattern 'longer than 52 characters' -Because 'Job 名要在 CronJob 名后面再加 11 个字符,超长在任何写操作之前拦下'
}

Test-Case 'Apply-SqlWriterSlotRetirer 只 apply 渲染好的清单' {
    $script:KubectlCalls.Clear()
    Apply-SqlWriterSlotRetirer -Namespace 'zone-1' -StatefulSet 'db' -Manifest 'rendered-manifest'
    Assert-Equal -Expected 1 -Actual $script:KubectlCalls.Count -Because '一次 apply'
    Assert-Equal -Expected 'apply -n zone-1' -Actual $script:KubectlCalls[0].Args -Because '按写者的 namespace apply'
    Assert-Equal -Expected 'rendered-manifest' -Actual $script:KubectlCalls[0].InputContent -Because '原样落地渲染结果'
}

Test-Case '每个写者都在 apply 之前渲染、在写者 apply 之后落地;缩容当场收尾;金丝雀收尾做完才删它' {
    $goSvc = $script:DeployFunctions['Apply-OneGoSvc'].Extent.Text -replace "`r`n", "`n"
    $render = $goSvc.IndexOf('$slotRetirerYaml = New-SqlWriterSlotRetirerManifest -Namespace $Namespace -StatefulSet $sqlWriterCatalogName')
    $firstKubectl = $goSvc.IndexOf('Invoke-Kubectl')
    $firstSecretSync = $goSvc.IndexOf('Sync-WorkloadSecrets')
    Assert-True -Condition ($render -ge 0 -and $firstKubectl -gt $render -and ($firstSecretSync -lt 0 -or $firstSecretSync -gt $render)) -Because 'Go 写者:收尾 CronJob 和写者清单一起在任何集群写操作之前渲染完,目录条目不对在这里就拦下'
    $canaryApply = $goSvc.IndexOf('Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent', $render)
    $canaryRetirer = $goSvc.IndexOf('Apply-SqlWriterSlotRetirer -Namespace $Namespace -StatefulSet "$SvcName-canary" -Manifest $slotRetirerYaml')
    Assert-True -Condition ($canaryApply -ge 0 -and $canaryRetirer -gt $canaryApply) -Because 'Go 金丝雀:写者 apply 之后落地它自己的收尾'
    $stableApply = $goSvc.IndexOf('Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent', $canaryRetirer)
    $stableRetirer = $goSvc.IndexOf('Apply-SqlWriterSlotRetirer -Namespace $Namespace -StatefulSet $SvcName -Manifest $slotRetirerYaml')
    Assert-True -Condition ($stableApply -ge 0 -and $stableRetirer -gt $stableApply) -Because 'Go 稳定版:写者 apply 之后落地'
    # 缩容当场收尾:apply 之前读线上副本数,收尾 CronJob 落地之后比副本数(金丝雀与稳定版各一处)。
    $canaryLive = $goSvc.IndexOf('$liveReplicas = if ($null -ne $slotRetirerYaml) { Get-SqlWriterLiveReplicas -Namespace $Namespace -StatefulSet "$SvcName-canary" }')
    $canaryScaleDown = $goSvc.IndexOf('Complete-SqlWriterScaleDown -Namespace $Namespace -StatefulSet "$SvcName-canary" -LiveReplicas $liveReplicas -ManifestContent $manifestContent')
    Assert-True -Condition ($canaryLive -ge 0 -and $canaryLive -lt $canaryApply -and $canaryScaleDown -gt $canaryRetirer) -Because 'Go 金丝雀:apply 之前读副本数,收尾 CronJob 落地之后判缩容'
    $stableLive = $goSvc.IndexOf('$liveReplicas = if ($null -ne $slotRetirerYaml) { Get-SqlWriterLiveReplicas -Namespace $Namespace -StatefulSet $SvcName }')
    $stableScaleDown = $goSvc.IndexOf('Complete-SqlWriterScaleDown -Namespace $Namespace -StatefulSet $SvcName -LiveReplicas $liveReplicas -ManifestContent $manifestContent')
    Assert-True -Condition ($stableLive -gt $canaryScaleDown -and $stableLive -lt $stableApply -and $stableScaleDown -gt $stableRetirer) -Because 'Go 稳定版:apply 之前读副本数,收尾 CronJob 落地之后判缩容'

    $java = $script:DeployFunctions['Apply-JavaSvcManifests'].Extent.Text -replace "`r`n", "`n"
    $javaRender = $java.IndexOf('$slotRetirerYaml = New-SqlWriterSlotRetirerManifest -Namespace $Namespace -StatefulSet $svcName')
    $javaLive = $java.IndexOf('$liveReplicas = Get-SqlWriterLiveReplicas -Namespace $Namespace -StatefulSet $svcName', $javaRender)
    $javaApply = $java.IndexOf('-InputContent $manifestContent', $javaRender)
    $javaRetirer = $java.IndexOf('Apply-SqlWriterSlotRetirer -Namespace $Namespace -StatefulSet $svcName -Manifest $slotRetirerYaml')
    $javaScaleDown = $java.IndexOf('Complete-SqlWriterScaleDown -Namespace $Namespace -StatefulSet $svcName -LiveReplicas $liveReplicas -ManifestContent $manifestContent')
    Assert-True -Condition ($javaRender -ge 0 -and $javaLive -gt $javaRender -and $javaApply -gt $javaLive -and $javaRetirer -gt $javaApply -and $javaScaleDown -gt $javaRetirer) -Because 'gateway:渲染在 apply 之前,apply 之前读副本数,落地在写者 apply 之后,随后判缩容'

    $canaries = $script:DeployFunctions['Apply-GlobalGoSvcCanaries'].Extent.Text
    $delete = $canaries.IndexOf('"delete", "deployment,statefulset", "$svcName-canary"')
    $removal = $canaries.IndexOf('if ($script:SqlWriterGoSvcs -ccontains $svcName) { Complete-SqlWriterCanaryRemoval -Namespace $InfraNamespace -StatefulSet "$svcName-canary" }')
    Assert-True -Condition ($delete -ge 0 -and $removal -gt $delete) -Because '金丝雀收尾先删 StatefulSet,再只对 SQL 写者收它各槽位的末代'
    Assert-NotMatch -Text $canaries -Pattern '"delete", "[^"]*cronjob' -Because '收尾 CronJob 不在这里直接删:要等当场那一轮做完(Complete-SqlWriterCanaryRemoval)'
}

# 当场收尾的桩:kubectl 调用、Job 状态与日志都按剧本回,sleep 不真睡。
function Reset-RetirementStubs {
    $script:KubectlCalls.Clear()
    $script:JobStates = New-Object System.Collections.Generic.Queue[string]
    $script:JobLogs = New-Object System.Collections.Generic.Queue[string]
    $script:LiveReplicasOutput = [pscustomobject]@{ ExitCode = 0; Output = ''; ErrorOutput = ''; TimedOut = $false }
    $script:CronJobProbe = [pscustomobject]@{ ExitCode = 0; Output = ''; ErrorOutput = ''; TimedOut = $false }
    $script:Sleeps = 0
    $script:DryRun = $false
    $script:WaitReady = $false
    $script:ReleaseProfile = 'prod'
    $script:WaitTimeoutSeconds = 60
    $script:SqlWriterSlotRetirementMinWaitSeconds = 60
    Use-RealRetirementFunction -Name $script:RetirementFunctions
}
function Invoke-Kubectl {
    param([string[]]$Args, [switch]$AllowFailure)
    $script:KubectlCalls.Add([pscustomobject]@{ Args = ($Args -join ' '); InputContent = $null })
    # 真 kubectl 会往标准输出写一行(job.batch/... created),这里照样写:收尾函数的返回值里不能混进它,调用方按真假判断。
    "kubectl-stdout: $($Args -join ' ')"
}
function Get-GoSvcMigrateJobState {
    param([string]$Namespace, [string]$JobName, [double]$TimeoutSeconds, [switch]$RequirePodsTerminal)
    if ($script:JobStates.Count -eq 0) { return 'unknown' }
    return $script:JobStates.Dequeue()
}
function Invoke-GoSvcMigrateKubectl {
    param([string[]]$Args, [double]$TimeoutSeconds = 10)
    $joined = $Args -join ' '
    if ($joined -like 'get statefulset *') { return $script:LiveReplicasOutput }
    if ($joined -like 'get cronjob *') { return $script:CronJobProbe }
    if ($joined -like 'logs job/*') {
        $text = if ($script:JobLogs.Count -gt 0) { $script:JobLogs.Dequeue() } else { '' }
        return [pscustomobject]@{ ExitCode = 0; Output = $text; ErrorOutput = ''; TimedOut = $false }
    }
    throw "unexpected kubectl query: $joined"
}
function Start-Sleep { param([int]$Seconds, [int]$Milliseconds) $script:Sleeps++ }

# 收尾 Job 日志的汇总行,与 Go 侧 retirementReport.lines 同一格式。
function New-RetirerSummary {
    param([string]$StatefulSet = 'db', [int]$Candidates, [int]$Retired, [int]$Pending)
    return "writer_authority command=retire-removed-slots statefulset=$StatefulSet replicas_before=2 replicas_after=2 roster_slots=4 candidates=$Candidates retired=$Retired pending=$Pending"
}

Test-Case '收尾 Job 汇总行的格式与 Go 侧一致(pending 是部署脚本判做完没有的依据)' {
    $go = Read-RepoText 'go/data_service/cmd/writer_authority/retire_removed_slots.go'
    Assert-Match -Text $go -Pattern ([regex]::Escape('"writer_authority command=retire-removed-slots statefulset=%s replicas_before=%d replicas_after=%d roster_slots=%d candidates=%d retired=%d pending=%d"')) -Because 'Go 侧打印的汇总行'
    $goTest = Read-RepoText 'go/data_service/cmd/writer_authority/retire_removed_slots_test.go'
    $sample = [regex]::Match($goTest, 'want := "(?<line>writer_authority command=retire-removed-slots [^"]+)"')
    Assert-True -Condition $sample.Success -Because 'Go 单测里有汇总行的样本'
    # Go 单测的样本每种结局各一个候选:8 种结局,结束 1 个,还没做完 5 个(Pod 还在、冻结、排空 run、失败、别处登记)。
    Assert-Equal -Expected (New-RetirerSummary -Candidates 8 -Retired 1 -Pending 5).Replace('replicas_before=2 replicas_after=2', 'replicas_before=1 replicas_after=1') -Actual $sample.Groups['line'].Value -Because '本套的样本与 Go 单测的样本同一格式'
}

Test-Case '读线上副本数:对象不在回 0,读不到或不是数字就拒绝' {
    Reset-RetirementStubs
    $script:LiveReplicasOutput = [pscustomobject]@{ ExitCode = 0; Output = "3`n"; ErrorOutput = ''; TimedOut = $false }
    Assert-Equal -Expected 3 -Actual (Get-SqlWriterLiveReplicas -Namespace 'zone-1' -StatefulSet 'db') -Because '读到的 spec.replicas'
    $script:LiveReplicasOutput = [pscustomobject]@{ ExitCode = 0; Output = ''; ErrorOutput = ''; TimedOut = $false }
    Assert-Equal -Expected 0 -Actual (Get-SqlWriterLiveReplicas -Namespace 'zone-1' -StatefulSet 'db') -Because 'StatefulSet 还不存在(首次部署)'
    foreach ($bad in @([pscustomobject]@{ ExitCode = 1; Output = ''; ErrorOutput = 'forbidden'; TimedOut = $false }, [pscustomobject]@{ ExitCode = 0; Output = 'two'; ErrorOutput = ''; TimedOut = $false })) {
        $script:LiveReplicasOutput = $bad
        $failure = $null
        try { $null = Get-SqlWriterLiveReplicas -Namespace 'zone-1' -StatefulSet 'db' } catch { $failure = $_.Exception.Message }
        Assert-Match -Text $failure -Pattern 'statefulset zone-1/db' -Because '不知道是不是缩容就不 apply,报出是哪个 StatefulSet'
    }
}

Test-Case '只有缩容才当场收尾;扩容、不变、首次部署不做事' {
    Reset-RetirementStubs
    $script:RetirementCalls = New-Object System.Collections.Generic.List[string]
    Set-Item -Path 'Function:script:Complete-SqlWriterSlotRetirement' -Value { param($Namespace, $StatefulSet, $Reason, [switch]$AlwaysWait) $script:RetirementCalls.Add("$Namespace/$StatefulSet/$Reason"); return $true }
    $manifest = "kind: StatefulSet`nspec:`n  replicas: 2`n  serviceName: db`n"
    foreach ($live in @(0, 1, 2)) { Complete-SqlWriterScaleDown -Namespace 'zone-1' -StatefulSet 'db' -LiveReplicas $live -ManifestContent $manifest }
    Assert-Equal -Expected 0 -Actual $script:RetirementCalls.Count -Because '副本数不变少就不收尾'
    Complete-SqlWriterScaleDown -Namespace 'zone-1' -StatefulSet 'db' -LiveReplicas 3 -ManifestContent $manifest
    Assert-Equal -Expected 'zone-1/db/a scale-down from 3 to 2 replicas' -Actual ($script:RetirementCalls -join ',') -Because '从 3 缩到 2 当场收尾一轮'
    $failure = $null
    try { Complete-SqlWriterScaleDown -Namespace 'zone-1' -StatefulSet 'db' -LiveReplicas 3 -ManifestContent "kind: StatefulSet`n" } catch { $failure = $_.Exception.Message }
    Assert-Match -Text $failure -Pattern 'spec\.replicas exactly once' -Because '清单里读不出副本数就拒绝,不猜'
}

Test-Case '当场收尾:Pod 还在优雅退出时隔 10 秒再起一轮,直到 Job 成功且 pending=0;做完的 Job 删掉' {
    Reset-RetirementStubs
    $script:JobStates.Enqueue('running'); $script:JobStates.Enqueue('complete'); $script:JobStates.Enqueue('complete')
    $script:JobLogs.Enqueue((New-RetirerSummary -Candidates 2 -Retired 0 -Pending 2))
    $script:JobLogs.Enqueue("writer_authority command=retire-removed-slots statefulset=db slot=2 logical_db=zone_1_db boot_seq=3 result=retired request_id=00`n" + (New-RetirerSummary -Candidates 1 -Retired 1 -Pending 0))
    $done = Complete-SqlWriterSlotRetirement -Namespace 'zone-1' -StatefulSet 'db' -Reason 'a scale-down from 3 to 2 replicas'
    Assert-True -Condition ($done -is [bool]) -Because 'kubectl 的输出打到控制台,不混进返回值(混进去的话 if 判断永远为真,没收完也会删金丝雀的 CronJob)'
    Assert-Equal -Expected 'True' -Actual $done -Because '第二轮 pending=0 才算做完'
    $creates = @($script:KubectlCalls | Where-Object { $_.Args -like 'create job *' })
    Assert-Equal -Expected 2 -Actual $creates.Count -Because '第一轮 Pod 还在、第二轮才结束'
    foreach ($create in $creates) {
        Assert-Match -Text $create.Args -Pattern '^create job sql-writer-slot-retirer-db-d\d{9} --from=cronjob/sql-writer-slot-retirer-db -n zone-1$' -Because '按这个写者自己的收尾 CronJob 起 Job,名字不超过 63 个字符'
    }
    Assert-Equal -Expected 2 -Actual @($script:KubectlCalls | Where-Object { $_.Args -like 'delete job sql-writer-slot-retirer-db-d* -n zone-1 --ignore-not-found' }).Count -Because '没做完的那轮与做完的那轮都删,结果已经打进部署日志'
    Assert-True -Condition ($script:Sleeps -ge 2) -Because '两轮之间隔一段时间,不忙等'
}

Test-Case '当场收尾:Job 失败、汇总行属于别的 StatefulSet 都不算做完;预算用完警告后返回,不中断部署' {
    Reset-RetirementStubs
    # 5 秒预算:读得到这一轮 Job 的状态(失败),不够再起下一轮(两轮之间要留 10 秒)。
    $script:WaitTimeoutSeconds = 5
    $script:SqlWriterSlotRetirementMinWaitSeconds = 5
    $script:JobStates.Enqueue('failed')
    $script:JobLogs.Enqueue((New-RetirerSummary -Candidates 0 -Retired 0 -Pending 0))
    $outcome = @(Complete-SqlWriterSlotRetirement -Namespace 'zone-1' -StatefulSet 'db' -Reason 'a scale-down from 3 to 2 replicas' 3>&1)
    $warnings = @($outcome | Where-Object { $_ -is [System.Management.Automation.WarningRecord] })
    $values = @($outcome | Where-Object { $_ -isnot [System.Management.Automation.WarningRecord] })
    Assert-Equal -Expected 'False' -Actual ($values -join ',') -Because 'Job 失败时汇总行写着 pending=0 也不算做完'
    Assert-Equal -Expected 1 -Actual $warnings.Count -Because '没做完要警告'
    Assert-Match -Text $warnings[0].Message -Pattern 'do not start a maintenance drain until a run of cronjob/sql-writer-slot-retirer-db reports pending=0' -Because '警告写清排空之前要等什么'
    Assert-Equal -Expected 0 -Actual @($script:KubectlCalls | Where-Object { $_.Args -like 'delete job *' }).Count -Because '没做完的最后一个 Job 留着查日志'

    Reset-RetirementStubs
    $script:JobStates.Enqueue('complete'); $script:JobStates.Enqueue('complete')
    $script:JobLogs.Enqueue((New-RetirerSummary -StatefulSet 'db-canary' -Candidates 0 -Retired 0 -Pending 0))
    $script:JobLogs.Enqueue((New-RetirerSummary -Candidates 0 -Retired 0 -Pending 0))
    $done = Complete-SqlWriterSlotRetirement -Namespace 'zone-1' -StatefulSet 'db' -Reason 'a scale-down from 3 to 2 replicas'
    Assert-Equal -Expected 'True' -Actual $done -Because '别的 StatefulSet 的汇总行不作数,下一轮读到自己的才算'
    Assert-Equal -Expected 2 -Actual @($script:KubectlCalls | Where-Object { $_.Args -like 'create job *' }).Count -Because '第一轮不作数,再起一轮'
}

Test-Case '当场收尾:门禁不生效(dev 不带 -WaitReady)只起一轮不等;金丝雀收尾不看门禁' {
    Reset-RetirementStubs
    $script:ReleaseProfile = 'dev'
    $done = Complete-SqlWriterSlotRetirement -Namespace 'zone-1' -StatefulSet 'db' -Reason 'a scale-down from 3 to 2 replicas'
    Assert-Equal -Expected 'False' -Actual $done -Because '不等就不能说做完了'
    Assert-Equal -Expected 'create job' -Actual (($script:KubectlCalls | ForEach-Object { ($_.Args -split ' ')[0..1] -join ' ' }) -join ',') -Because '只起一轮,不等、不删'

    Reset-RetirementStubs
    $script:ReleaseProfile = 'dev'
    $script:JobStates.Enqueue('complete')
    $script:JobLogs.Enqueue((New-RetirerSummary -StatefulSet 'guild-canary' -Candidates 2 -Retired 2 -Pending 0))
    $done = Complete-SqlWriterSlotRetirement -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary' -Reason 'the removal of the canary' -AlwaysWait
    Assert-Equal -Expected 'True' -Actual $done -Because '金丝雀收尾要靠结果决定删不删 CronJob,dev 也等'
}

Test-Case '金丝雀收尾:收尾 CronJob 在才跑一轮,做完才删它与它的 RBAC;读不到就不动' {
    Reset-RetirementStubs
    Complete-SqlWriterCanaryRemoval -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary'
    Assert-Equal -Expected 0 -Actual $script:KubectlCalls.Count -Because 'CronJob 不在:从没起过这个金丝雀或者已经收完,什么都不做'

    Reset-RetirementStubs
    $script:CronJobProbe = [pscustomobject]@{ ExitCode = 0; Output = 'cronjob.batch/sql-writer-slot-retirer-guild-canary'; ErrorOutput = ''; TimedOut = $false }
    $script:JobStates.Enqueue('complete')
    $script:JobLogs.Enqueue((New-RetirerSummary -StatefulSet 'guild-canary' -Candidates 2 -Retired 2 -Pending 0))
    Complete-SqlWriterCanaryRemoval -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary'
    Assert-Equal -Expected 1 -Actual @($script:KubectlCalls | Where-Object { $_.Args -ceq 'delete cronjob,rolebinding,role,serviceaccount sql-writer-slot-retirer-guild-canary -n mmorpg-infra --ignore-not-found' }).Count -Because '做完了删 CronJob 与同名的 RBAC,之后删金丝雀的目录条目与 Secret 不会留下起不来的 CronJob'

    Reset-RetirementStubs
    $script:WaitTimeoutSeconds = 0
    $script:SqlWriterSlotRetirementMinWaitSeconds = 0
    $script:CronJobProbe = [pscustomobject]@{ ExitCode = 0; Output = 'cronjob.batch/sql-writer-slot-retirer-guild-canary'; ErrorOutput = ''; TimedOut = $false }
    $script:JobStates.Enqueue('complete')
    $script:JobLogs.Enqueue((New-RetirerSummary -StatefulSet 'guild-canary' -Candidates 2 -Retired 1 -Pending 1))
    $null = Complete-SqlWriterCanaryRemoval -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary' 3>$null
    Assert-Equal -Expected 0 -Actual @($script:KubectlCalls | Where-Object { $_.Args -like 'delete cronjob*' }).Count -Because '没做完就留着,CronJob 接着做,下一次部署再来'

    Reset-RetirementStubs
    $script:CronJobProbe = [pscustomobject]@{ ExitCode = 1; Output = ''; ErrorOutput = 'connection refused'; TimedOut = $false }
    $null = Complete-SqlWriterCanaryRemoval -Namespace 'mmorpg-infra' -StatefulSet 'guild-canary' 3>$null
    Assert-Equal -Expected 0 -Actual $script:KubectlCalls.Count -Because '读不到 CronJob 在不在,既不收尾也不删'
}

Test-Case '告警与手册跟着收尾 CronJob 走' {
    $alerts = Read-RepoText 'deploy/k8s/logical-db-alerts.yaml'
    Assert-Match -Text $alerts -Pattern '- alert: SqlWriterSlotRetirerFailing' -Because '一直不成功要有人知道'
    Assert-Match -Text (Read-RepoText 'deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml') -Pattern 'SqlWriterSlotRetirerFailing' -Because '清单注释里写的告警真的存在'
    $runbook = Read-RepoText 'docs/ops/k8s-open-server-runbook.md'
    foreach ($pattern in @('sql-writer-slot-retirer', 'SqlWriterSlotRetirerFailing')) {
        Assert-Match -Text $runbook -Pattern $pattern -Because "开服手册要写 $pattern"
    }
    Assert-Match -Text (Read-RepoText 'deploy/k8s/README.md') -Pattern 'sql-writer-slot-retirer' -Because 'deploy/k8s/README.md 登记这份 CronJob'
}

# 归属(设计稿 §5.9):收尾只结束本 namespace 的工作负载登记的一代。这几样分散在 Go、清单、告警与手册里,对不上时不会报错:
# 结局名改了而手册、告警没跟上,值班的人就不知道 registered-elsewhere 是什么;写者关掉 ServiceAccount 令牌,注册器就读不到自己的身份。
Test-Case '归属:结局名、身份前缀与令牌挂载在 Go、清单、告警、手册之间一致' {
    $retirer = Read-RepoText 'go/data_service/cmd/writer_authority/retire_removed_slots.go'
    $outcome = [regex]::Match($retirer, '(?m)^\s*slotRegisteredElsewhere\s*=\s*"([^"]+)"')
    Assert-True -Condition $outcome.Success -Because 'Go 侧有「别处登记」这种结局'
    $name = $outcome.Groups[1].Value
    Assert-Equal -Expected 'registered-elsewhere' -Actual $name -Because '结局名是日志里的字面量,手册、告警、部署脚本的警告都引用它'
    Assert-Match -Text $retirer -Pattern 'case slotRetired, slotAlreadyEnded, slotScaledBackUp:\s*\n\s*return false' -Because '别处登记的一代算 pending:部署当场收尾不能把它当成做完了'
    foreach ($file in @('docs/ops/k8s-open-server-runbook.md', 'deploy/k8s/logical-db-alerts.yaml', 'tools/scripts/k8s_deploy.ps1', 'deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml')) {
        Assert-Match -Text (Read-RepoText $file) -Pattern ([regex]::Escape($name)) -Because "$file 要说明 result=$name"
    }
    $identity = Read-RepoText 'go/data_service/cmd/writer_authority/workload_identity.go'
    Assert-Match -Text $identity -Pattern 'workloadIdentityPrefix = "k8s-sa:"' -Because '身份文本的前缀'
    Assert-Match -Text (Read-RepoText 'proto/common/database/rollback_database_table.proto') -Pattern '(?m)^\s*string registered_by = 14;' -Because 'REGISTER 记下登记它的工作负载身份'
    Assert-Match -Text (Read-RepoText 'docs/ops/k8s-open-server-runbook.md') -Pattern 'k8s-sa:<namespace>/<ServiceAccount>/<UID>' -Because '手册写明 registeredBy 的形状,值班的人才看得懂日志'
    # 注册器靠 Pod 里的 ServiceAccount 令牌问 API server「我是谁」(SelfSubjectReview);写者清单关掉令牌挂载,登记就停在 init。
    $writerManifests = @(Get-ChildItem -LiteralPath (Join-Path $RepoRoot 'deploy/k8s/manifests/go-svc') -Filter '*.yaml') + @(Get-Item -LiteralPath (Join-Path $RepoRoot 'deploy/k8s/manifests/java-svc/gateway.yaml'))
    foreach ($file in $writerManifests) {
        $active = (([IO.File]::ReadAllText($file.FullName) -split "`r?`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
        Assert-NotMatch -Text $active -Pattern 'automountServiceAccountToken:\s*false' -Because "$($file.Name) 不能关 ServiceAccount 令牌:写者的注册器要用它读自己的工作负载身份"
    }
}

exit (Complete-TestRun -SuiteName 'sql_writer_slot_retirer_contract')
