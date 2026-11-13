#requires -Version 7
# scene 计划排空的部署契约(docs/design/scene-node-planned-drain.md §4.6、§4.7、§6):
#   - 计划排空预算 = Pod 宽限期 - 25 秒,宽限期不足 26 秒时为 0(进程关闭计划排空);
#   - Deployment 与 Agones Fleet 两种 scene 编排都注入 MMORPG_BUILD_LABEL(与 mmorpg.io/build 同值)与预算,预算与各自的
#     terminationGracePeriodSeconds 出自同一个数;
#   - gate 与 battle 也注入 MMORPG_BUILD_LABEL(选落点的一方据此优先新构建);计划排空预算只给 scene 与 battle,gate 不注入
#     (gate 的排空由 k8s_gate_drain.ps1 驱动,不走 SIGTERM 计划排空)。
# 只导入函数定义并渲染本地字符串;不执行部署入口、不调用 kubectl、不使用真实密钥。夹具同 lobby_authority_render.tests.ps1。
$ErrorActionPreference = 'Stop'
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
. (Join-Path $RepoRoot 'tools/scripts/lib/release_common.ps1')
. (Join-Path $RepoRoot 'tools/scripts/lib/k8s_client_entry.ps1')
$tokens = $null; $parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $RepoRoot 'tools/scripts/k8s_deploy.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw 'k8s_deploy.ps1 has syntax errors' }
foreach ($definition in $ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.FunctionDefinitionAst] }) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
$assetCallers = @($ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.AssignmentStatementAst] -and $_.Left.Extent.Text -eq '$script:AssetOpCallers' })
if ($assetCallers.Count -ne 1) { throw 'fixture expects exactly one asset caller declaration' }
$script:AssetOpCallers = & ([scriptblock]::Create($assetCallers[0].Right.Extent.Text))
$script:AssetOpsOn = $false; $script:AssetOpSecrets = @{}
$ReleaseProfile = 'dev'; $InfraNamespace = 'fixture-infra'
$PlayerRedisMasters = 0; $PlayerRedisReplicasPerMaster = 1
$script:MysqlUser = 'fixture'; $script:RedisPassword = ''
$script:GateTokenSecret = ''; $script:InternalAuthSigningKey = ''; $script:InternalAuthPublicKeys = ''
$NodeImage = 'fixture.invalid/game:fixed'; $ImagePullPolicy = 'IfNotPresent'
$SnowflakeCacheDir = '/run/snowflake'; $GrpcServerMaxPollers = 0; $GateRouterMode = '1'
$RequireClientEndpoint = 'auto'; $ClientEntryMode = 'podip'; $NodeOpenFileLimitMode = ''
$script:CppLogSidecarEnabled = $false; $script:CppLogVolumeSizeLimit = '1Gi'; $script:CppLogPrunerCommand = ''
$AgonesHighDensity = $false
$SceneTerminationGracePeriodSeconds = 120

function Assert-Contract([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }

# 预算推导:宽限期 - 25 秒;不够 1 秒就是 0(关闭),不写一个 K8s 必然砍断的预算。
Assert-Contract ((Get-PlannedDrainBudgetSeconds -GraceSeconds 120) -eq 95) 'budget for a 120s grace period must be 95s'
Assert-Contract ((Get-PlannedDrainBudgetSeconds -GraceSeconds 60) -eq 35) 'budget for a 60s grace period must be 35s'
Assert-Contract ((Get-PlannedDrainBudgetSeconds -GraceSeconds 26) -eq 1) 'budget for a 26s grace period must be 1s'
Assert-Contract ((Get-PlannedDrainBudgetSeconds -GraceSeconds 25) -eq 0) 'grace periods of 25s or less must disable the planned drain'
Assert-Contract ((Get-PlannedDrainBudgetSeconds -GraceSeconds 0) -eq 0) 'a zero grace period must disable the planned drain'

# 镜像为空时构建标识写 unknown,不让 Get-ImageBuildLabel 的必填参数报错。
$noImage = @(Get-ScenePlannedDrainEnvLines -Indent '' -GraceSeconds 60 -Image '')
Assert-Contract ($noImage.Count -eq 4 -and $noImage[1] -eq '  value: "unknown"' -and $noImage[3] -eq '  value: "35"') 'env lines without an image must carry build "unknown" and the derived budget'

$scene = New-NodeDeploymentYaml -NodeName 'scene' -Replicas 1 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -SceneNodeType 0 -TerminationGracePeriodSeconds $SceneTerminationGracePeriodSeconds
$fleet = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 1 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
$gate = New-NodeDeploymentYaml -NodeName 'gate' -Replicas 1 -RpcPort 18000 -StartCommand './gate' -ConfigMapName 'fixture-gate'

foreach ($pair in @(@{ Name = 'deployment'; Yaml = $scene }, @{ Name = 'agones fleet'; Yaml = $fleet })) {
    $yaml = $pair.Yaml
    Assert-Contract ($yaml -match 'terminationGracePeriodSeconds:\s*120') "scene $($pair.Name) must use the scene grace period"
    Assert-Contract ($yaml -match 'name: MMORPG_BUILD_LABEL\s+value: "fixed"') "scene $($pair.Name) must inject MMORPG_BUILD_LABEL equal to the mmorpg.io/build value"
    Assert-Contract ($yaml -match 'name: MMORPG_PLANNED_DRAIN_BUDGET_SECONDS\s+value: "95"') "scene $($pair.Name) must inject the planned drain budget derived from its grace period"
    Assert-Contract (([regex]::Matches($yaml, 'name: MMORPG_PLANNED_DRAIN_BUDGET_SECONDS')).Count -eq 1) "scene $($pair.Name) must inject the budget exactly once"
}
Assert-Contract ($fleet -match 'mmorpg.io/build: fixed') 'the Fleet build label and MMORPG_BUILD_LABEL must come from the same image tag'
Assert-Contract (-not $gate.Contains('MMORPG_PLANNED_DRAIN_BUDGET_SECONDS')) 'gate must not get the scene planned drain budget'
Assert-Contract ($gate -match 'name: MMORPG_BUILD_LABEL\s+value: "fixed"') 'gate must report its build label so that drain migration can prefer gates off the outgoing build'
Assert-Contract (([regex]::Matches($gate, 'name: MMORPG_BUILD_LABEL')).Count -eq 1) 'gate must inject the build label exactly once'
Assert-Contract (([regex]::Matches($scene, 'name: MMORPG_BUILD_LABEL')).Count -eq 1) 'scene deployment must inject the build label exactly once'

# 工作负载修订:Deployment 形态经 downward API 取 Pod 的 pod-template-hash 标签,C++ 把它拼进 NodeInfo.build,
# 同一个镜像滚动重启时也分得出新旧两代 Pod。Agones Fleet 的 Pod 没有这种标签,scene Fleet 由部署按模板内容自己算(见下)。
$deploymentRevisionEnv = 'name: MMORPG_POD_REVISION\s+valueFrom: \{fieldRef: \{fieldPath: "metadata\.labels\[''pod-template-hash''\]"\}\}'
foreach ($pair in @(@{ Name = 'scene deployment'; Yaml = $scene }, @{ Name = 'gate deployment'; Yaml = $gate })) {
    Assert-Contract ($pair.Yaml -match $deploymentRevisionEnv) "$($pair.Name) must inject MMORPG_POD_REVISION from the pod-template-hash label"
    Assert-Contract (([regex]::Matches($pair.Yaml, 'name: MMORPG_POD_REVISION')).Count -eq 1) "$($pair.Name) must inject the pod revision exactly once"
}

# scene Fleet:模板修订是 GameServer 模板内容的哈希,写进 GameServer 模板的标签(k8s_scene_roll.ps1 据此认出旧一代)
# 与 Pod 的 MMORPG_POD_REVISION;后者再拼上重启令牌(Pod 模板注解 mmorpg.io/restarted-at,k8s_scene_roll.ps1 -Restart 打的)。
function Get-FixtureFleetRevision([string]$Yaml) {
    $match = [regex]::Match($Yaml, 'mmorpg\.io/revision: "(r[0-9a-f]{10})"')
    Assert-Contract $match.Success 'the scene Fleet must label its game server template with mmorpg.io/revision: "r<10 hex>"'
    return $match.Groups[1].Value
}
$fleetRevision = Get-FixtureFleetRevision $fleet
Assert-Contract (-not $fleet.Contains('__SCENE_FLEET_TEMPLATE_REVISION__')) 'the revision placeholder must not survive into the rendered Fleet'
Assert-Contract (([regex]::Matches($fleet, 'mmorpg\.io/revision:')).Count -eq 1) 'the revision label belongs on the game server template only'
Assert-Contract ($fleet -match "name: MMORPG_FLEET_RESTARTED_AT\s+valueFrom:\s+fieldRef:\s+fieldPath: metadata\.annotations\['mmorpg\.io/restarted-at'\]") 'the scene Fleet must read the restart token from the pod template annotation'
Assert-Contract ($fleet -match ('name: MMORPG_POD_REVISION\s+value: "' + [regex]::Escape($fleetRevision) + '\.\$\(MMORPG_FLEET_RESTARTED_AT\)"')) 'MMORPG_POD_REVISION must be the template revision joined with the restart token'
Assert-Contract ($fleet.IndexOf('name: MMORPG_FLEET_RESTARTED_AT') -lt $fleet.IndexOf('name: MMORPG_POD_REVISION')) 'the restart token must be declared before the variable that expands it'
Assert-Contract (([regex]::Matches($fleet, 'name: MMORPG_POD_REVISION')).Count -eq 1) 'the scene Fleet must inject the pod revision exactly once'

# 修订只跟 GameServer 模板的内容走:同样的输入算出同一个值,扩缩容不换代,换镜像换代。
$fleetAgain = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 1 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
$fleetScaled = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 7 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
Assert-Contract ((Get-FixtureFleetRevision $fleetAgain) -ceq $fleetRevision) 'rendering the same Fleet twice must give the same revision'
Assert-Contract ((Get-FixtureFleetRevision $fleetScaled) -ceq $fleetRevision) 'scaling the Fleet must not change the template revision'
$stableImage = $NodeImage
$NodeImage = 'fixture.invalid/game:other'
$fleetOtherImage = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 1 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
$NodeImage = $stableImage
Assert-Contract ((Get-FixtureFleetRevision $fleetOtherImage) -cne $fleetRevision) 'a different image must give a different template revision'
$configMapChanged = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 1 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene-v2' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
Assert-Contract ((Get-FixtureFleetRevision $configMapChanged) -cne $fleetRevision) 'a config-only template change (same image) must give a different template revision'

# 哈希与行的先后、换行符无关:同一份模板换成 CRLF、把两行对调,算出的修订不变。
$crlfRevision = Get-SceneFleetTemplateRevision -FleetYaml (($fleet -replace "`r`n", "`n") -replace "`n", "`r`n")
Assert-Contract ($crlfRevision -ceq (Get-SceneFleetTemplateRevision -FleetYaml $fleet)) 'line endings must not change the template revision'
$fleetLines = ($fleet -replace "`r`n", "`n").Split("`n")
$swapAt = [Array]::IndexOf($fleetLines, ($fleetLines | Where-Object { $_ -match '^\s+- name: POD_IP$' } | Select-Object -First 1))
Assert-Contract ($swapAt -gt 0 -and $swapAt + 1 -lt $fleetLines.Count) 'fixture expects a POD_IP env entry inside the Fleet template'
$swapped = [string[]]$fleetLines.Clone()
$swapped[$swapAt], $swapped[$swapAt + 1] = $swapped[$swapAt + 1], $swapped[$swapAt]
Assert-Contract ((Get-SceneFleetTemplateRevision -FleetYaml ($swapped -join "`n")) -ceq (Get-SceneFleetTemplateRevision -FleetYaml $fleet)) 'line order must not change the template revision'

# lib 的 C++ 模板(external gate、battle 的两种 lib 形态)经 PodCommon.BuildLabel 写同一个变量;不带这个成员时不写。
$contract = Get-ClientEntryContract
$withBuild = New-ClientEntryPodCommon -Image $NodeImage -ImagePullPolicy 'IfNotPresent' -LogVolumeSizeLimit '1Gi' -BuildLabel 'fixed'
$withoutBuild = New-ClientEntryPodCommon -Image $NodeImage -ImagePullPolicy 'IfNotPresent' -LogVolumeSizeLimit '1Gi'
# 先赋值再筛:函数用「,$entries」整体返回数组,直接接管道拿到的是整个数组这一个对象。
$envWithBuild = Get-ClientEntryNodeBaseEnv -PodCommon $withBuild -RpcPort 18000
$envWithoutBuild = Get-ClientEntryNodeBaseEnv -PodCommon $withoutBuild -RpcPort 18000
$buildEntries = @($envWithBuild | Where-Object { $_.Name -eq $contract.BuildLabelEnv })
Assert-Contract ($contract.BuildLabelEnv -eq 'MMORPG_BUILD_LABEL') 'the lib must write the same variable name the C++ node reads'
Assert-Contract ($buildEntries.Count -eq 1 -and $buildEntries[0].Value -eq 'fixed') 'PodCommon.BuildLabel must become exactly one MMORPG_BUILD_LABEL entry'
Assert-Contract (@($envWithoutBuild | Where-Object { $_.Name -eq $contract.BuildLabelEnv }).Count -eq 0) 'a PodCommon without a build label must not write the variable'

# 修订跟着构建标识走:生成器给了标签名(gate StatefulSet 是 controller-revision-hash)才写,没有构建标识时不写。
$envGateStatefulSet = Get-ClientEntryNodeBaseEnv -PodCommon $withBuild -RpcPort 18000 -RevisionLabelKey 'controller-revision-hash'
$envRevisionWithoutBuild = Get-ClientEntryNodeBaseEnv -PodCommon $withoutBuild -RpcPort 18000 -RevisionLabelKey 'controller-revision-hash'
$revisionEntries = @($envGateStatefulSet | Where-Object { $_.Name -eq $contract.PodRevisionEnv })
Assert-Contract ($contract.PodRevisionEnv -eq 'MMORPG_POD_REVISION') 'the lib must write the same revision variable name the C++ node reads'
Assert-Contract ($revisionEntries.Count -eq 1 -and $revisionEntries[0].FieldPath -eq "metadata.labels['controller-revision-hash']") 'a revision label key must become exactly one downward API entry'
Assert-Contract (@($envWithBuild | Where-Object { $_.Name -eq $contract.PodRevisionEnv }).Count -eq 0) 'no revision label key (Agones Fleet) must not write the revision variable'
Assert-Contract (@($envRevisionWithoutBuild | Where-Object { $_.Name -eq $contract.PodRevisionEnv }).Count -eq 0) 'the revision is only written together with the build label'

# battle:三种编排的宽限期都取 -BattleTerminationGracePeriodSeconds,并注入同一个数推出的预算(等在打的局打完,§9)。
# 夹具同 lobby_authority_render.tests.ps1 渲染 battle 的那一段。
$PhysicalRegion = 7; $KafkaBrokers = 3; $OpsProfile = 'custom'; $ClientPublicHost = ''
$AgonesHealthInitialDelaySeconds = 5; $AgonesHealthPeriodSeconds = 10; $AgonesHealthFailureThreshold = 3
$BattleReplicas = 1; $CanaryBattleReplicas = 0; $BattleTerminationGracePeriodSeconds = 330
foreach ($plan in @(@{ Orchestrator = 'deployment'; Entry = 'podip' }, @{ Orchestrator = 'deployment'; Entry = 'external' }, @{ Orchestrator = 'agones'; Entry = 'external' })) {
    $BattleOrchestrator = $plan.Orchestrator; $ClientEntryMode = $plan.Entry
    $battle = (New-BattleWorkloadManifest).Yaml
    $form = "$BattleOrchestrator/$ClientEntryMode"
    Assert-Contract ($battle -match 'terminationGracePeriodSeconds:\s*330') "battle $form must use the battle grace period"
    Assert-Contract ($battle -match 'name: "?MMORPG_PLANNED_DRAIN_BUDGET_SECONDS"?\s+value: [''"]?305[''"]?') "battle $form must inject the planned drain budget derived from its grace period"
    Assert-Contract ($battle -match 'name: "?MMORPG_BUILD_LABEL"?\s+value: [''"]?fixed[''"]?') "battle $form must report its build label"
    Assert-Contract (([regex]::Matches($battle, 'name: "?MMORPG_BUILD_LABEL"?')).Count -eq 1) "battle $form must inject the build label exactly once"
    # 两种 Deployment 形态带工作负载修订(本脚本的模板写成流式、lib 的模板写成块式,所以只核对变量名与标签名);Fleet 不带。
    $revisionCount = ([regex]::Matches($battle, 'name: "?MMORPG_POD_REVISION"?')).Count
    if ($BattleOrchestrator -eq 'agones') {
        Assert-Contract ($revisionCount -eq 0) "battle $form runs as an Agones Fleet, whose pods carry no workload revision label"
    }
    else {
        Assert-Contract ($revisionCount -eq 1) "battle $form must inject the pod revision exactly once"
        Assert-Contract ($battle -match 'name: "?MMORPG_POD_REVISION"?[\s\S]{0,240}?fieldPath: "?metadata\.labels\[''pod-template-hash''\]') "battle $form must take the pod revision from the pod-template-hash label"
    }
}
$ClientEntryMode = 'podip'

Write-Output 'PASS: build label renders for gate, scene and battle; planned drain budget for both scene orchestrators and all three battle forms (no cluster operations)'
