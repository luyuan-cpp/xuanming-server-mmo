#requires -Version 7
# scene 计划排空的部署契约(docs/design/scene-node-planned-drain.md §4.6、§4.7、§6):
#   - 计划排空预算 = Pod 宽限期 - 25 秒,宽限期不足 26 秒时为 0(进程关闭计划排空);
#   - Deployment 与 Agones Fleet 两种 scene 编排都注入 MMORPG_BUILD_LABEL(与 mmorpg.io/build 同值)与预算,预算与各自的
#     terminationGracePeriodSeconds 出自同一个数;
#   - 非 scene 的节点(gate)不注入。
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
$script:MysqlUser = 'fixture'; $script:MysqlPassword = ''; $script:RedisPassword = ''
$script:GateTokenSecret = ''; $script:InternalAuthSigningKey = ''; $script:InternalAuthPublicKeys = ''
$NodeImage = 'fixture.invalid/game:fixed'; $ImagePullPolicy = 'IfNotPresent'
$SnowflakeCacheDir = '/run/snowflake'; $GrpcServerMaxPollers = 0; $GateRouterMode = '1'
$RequireClientEndpoint = 'auto'; $ClientEntryMode = 'podip'; $NodeOpenFileLimitMode = ''
$script:CppLogSidecarEnabled = $false; $script:CppLogVolumeSizeLimit = '1Gi'; $script:CppLogPrunerCommand = ''
$AgonesHighDensity = $false
$SceneTerminationGracePeriodSeconds = 120

function Assert-Contract([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }

# 预算推导:宽限期 - 25 秒;不够 1 秒就是 0(关闭),不写一个 K8s 必然砍断的预算。
Assert-Contract ((Get-ScenePlannedDrainBudgetSeconds -GraceSeconds 120) -eq 95) 'budget for a 120s grace period must be 95s'
Assert-Contract ((Get-ScenePlannedDrainBudgetSeconds -GraceSeconds 60) -eq 35) 'budget for a 60s grace period must be 35s'
Assert-Contract ((Get-ScenePlannedDrainBudgetSeconds -GraceSeconds 26) -eq 1) 'budget for a 26s grace period must be 1s'
Assert-Contract ((Get-ScenePlannedDrainBudgetSeconds -GraceSeconds 25) -eq 0) 'grace periods of 25s or less must disable the planned drain'
Assert-Contract ((Get-ScenePlannedDrainBudgetSeconds -GraceSeconds 0) -eq 0) 'a zero grace period must disable the planned drain'

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
Assert-Contract (-not $gate.Contains('MMORPG_BUILD_LABEL')) 'gate must not get the scene build label'

Write-Output 'PASS: scene planned drain budget and build label render for both orchestrators (no cluster operations)'
