#requires -Version 7
# k8s_scene_roll.ps1 的纯函数契约(docs/design/scene-node-planned-drain.md §4.6、§6):一台 GameServer 属于哪一代、
# 选哪些旧一代的打排空标签、还剩几台旧一代、-Restart 的补丁长什么样。只 dot-source 脚本取函数,不调 kubectl。
$ErrorActionPreference = 'Stop'
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
. (Join-Path $RepoRoot 'tools/scripts/k8s_scene_roll.ps1')

function Assert-Roll([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }

# 一份 metadata:构建标识必有;修订与重启令牌给了才写(老模板渲染出来的 GameServer 两样都没有)。
function New-FixtureMetadata([string]$Name, [string]$Build, [string]$Revision = '', [string]$RestartedAt = '', [switch]$Draining) {
	$labels = [ordered]@{ 'mmorpg.io/build' = $Build }
	if ($Revision -ne '') { $labels['mmorpg.io/revision'] = $Revision }
	if ($Draining) { $labels['mmorpg.io/drain'] = 'true' }
	$metadata = [ordered]@{ name = $Name; labels = [pscustomobject]$labels }
	if ($RestartedAt -ne '') { $metadata['annotations'] = [pscustomobject]@{ 'mmorpg.io/restarted-at' = $RestartedAt } }
	return [pscustomobject]$metadata
}

function New-FixtureGameServer([string]$Name, [string]$Build, [string]$State, [string]$Revision = '', [string]$RestartedAt = '', [switch]$Draining) {
	return [pscustomobject]@{
		metadata = (New-FixtureMetadata -Name $Name -Build $Build -Revision $Revision -RestartedAt $RestartedAt -Draining:$Draining)
		status   = [pscustomobject]@{ state = $State }
	}
}

# ── 换镜像:构建标识不同就是旧一代 ───────────────────────────────────────────────────────────────
$target = Get-SceneGenerationKey -Metadata (New-FixtureMetadata -Name 'template' -Build 'v2' -Revision 'r2222222222')
$fleet = @(
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -Revision 'r1111111111' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -Revision 'r1111111111' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-b' -Build 'v1' -Revision 'r1111111111' -State 'Ready'),
	(New-FixtureGameServer -Name 'scene-n' -Build 'v2' -Revision 'r2222222222' -State 'Allocated')
)

$first = @(Select-SceneGameServersToDrain -GameServers $fleet -TargetGeneration $target -MaxConcurrent 1)
Assert-Roll ($first.Count -eq 1 -and $first[0] -eq 'scene-a') 'must pick exactly one old-generation Allocated game server, by name'
Assert-Roll ((Get-SceneOldGenerationRemaining -GameServers $fleet -TargetGeneration $target) -eq 3) 'every old-generation game server counts as remaining, Ready ones included'

$two = @(Select-SceneGameServersToDrain -GameServers $fleet -TargetGeneration $target -MaxConcurrent 2)
Assert-Roll ($two.Count -eq 2 -and $two[0] -eq 'scene-a' -and $two[1] -eq 'scene-c') 'Ready old-generation game servers are left to the Fleet rolling update'

$withDraining = @(
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -Revision 'r1111111111' -State 'Allocated' -Draining),
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -Revision 'r1111111111' -State 'Allocated')
)
Assert-Roll (@(Select-SceneGameServersToDrain -GameServers $withDraining -TargetGeneration $target -MaxConcurrent 1).Count -eq 0) 'no new label while one game server is still draining'

$shuttingDown = @(
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -Revision 'r1111111111' -State 'Shutdown' -Draining),
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -Revision 'r1111111111' -State 'Allocated')
)
$next = @(Select-SceneGameServersToDrain -GameServers $shuttingDown -TargetGeneration $target -MaxConcurrent 1)
Assert-Roll ($next.Count -eq 1 -and $next[0] -eq 'scene-c') 'a game server already shutting down frees its slot'
Assert-Roll ((Get-SceneOldGenerationRemaining -GameServers $shuttingDown -TargetGeneration $target) -eq 1) 'game servers in Shutdown are no longer counted'

$done = @((New-FixtureGameServer -Name 'scene-n' -Build 'v2' -Revision 'r2222222222' -State 'Allocated'))
Assert-Roll ((Get-SceneOldGenerationRemaining -GameServers $done -TargetGeneration $target) -eq 0) 'roll is complete when no old-generation game server is left'
Assert-Roll (@(Select-SceneGameServersToDrain -GameServers @() -TargetGeneration $target -MaxConcurrent 1).Count -eq 0) 'an empty fleet needs no label'

# ── 同一个镜像只改配置:构建标识相同,模板修订不同,照样是旧一代 ─────────────────────────────────────
$sameImage = @(
	(New-FixtureGameServer -Name 'scene-old' -Build 'v2' -Revision 'r1111111111' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-new' -Build 'v2' -Revision 'r2222222222' -State 'Allocated')
)
$configOnly = @(Select-SceneGameServersToDrain -GameServers $sameImage -TargetGeneration $target -MaxConcurrent 5)
Assert-Roll ($configOnly.Count -eq 1 -and $configOnly[0] -eq 'scene-old') 'a config-only rollout keeps the build label; the template revision alone must mark the old generation'

# 引入修订标签之前渲染出来的 GameServer 没有这个标签:模板已经不同,算旧一代。
$preRevision = @((New-FixtureGameServer -Name 'scene-legacy' -Build 'v2' -State 'Allocated'))
Assert-Roll ((Get-SceneOldGenerationRemaining -GameServers $preRevision -TargetGeneration $target) -eq 1) 'a game server rendered before the revision label existed is an old generation'

# ── -Restart:模板内容没变,只有重启令牌不同 ─────────────────────────────────────────────────────
$templateMetadata = New-FixtureMetadata -Name 'template' -Build 'v2' -Revision 'r2222222222'
$restartTarget = Get-SceneGenerationKey -Metadata $templateMetadata -RestartedAt '20261112T000000Z'
Assert-Roll ($restartTarget -cne $target) 'a restart token must change the target generation even when build and revision stay the same'
$restarting = @(
	(New-FixtureGameServer -Name 'scene-before' -Build 'v2' -Revision 'r2222222222' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-earlier-restart' -Build 'v2' -Revision 'r2222222222' -RestartedAt '20261101T000000Z' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-after' -Build 'v2' -Revision 'r2222222222' -RestartedAt '20261112T000000Z' -State 'Allocated')
)
$restartPick = @(Select-SceneGameServersToDrain -GameServers $restarting -TargetGeneration $restartTarget -MaxConcurrent 5)
Assert-Roll ($restartPick.Count -eq 2 -and $restartPick[0] -eq 'scene-before' -and $restartPick[1] -eq 'scene-earlier-restart') 'after a restart token is set, every game server without that exact token is an old generation'
Assert-Roll ((Get-SceneGenerationKey -Metadata $restarting[2].metadata) -ceq $restartTarget) 'a game server created from the patched template is on the target generation'

# 补丁:GameServer 模板与它的 Pod 模板两层都带同一个令牌。
$patch = (New-SceneFleetRestartPatch -Token '20261112T000000Z') | ConvertFrom-Json
Assert-Roll ((Get-SceneMetadataValue -Map $patch.spec.template.metadata.annotations -Key 'mmorpg.io/restarted-at') -eq '20261112T000000Z') 'the restart patch must annotate the game server template (the roll script reads it from game server objects)'
Assert-Roll ((Get-SceneMetadataValue -Map $patch.spec.template.spec.template.metadata.annotations -Key 'mmorpg.io/restarted-at') -eq '20261112T000000Z') 'the restart patch must annotate the pod template (the pod reads it through the downward API)'

# 排空标签只看键在不在:值为空串也算已打。
$emptyDrainValue = [pscustomobject]@{
	metadata = [pscustomobject]@{ name = 'scene-x'; labels = [pscustomobject]([ordered]@{ 'mmorpg.io/build' = 'v1'; 'mmorpg.io/drain' = '' }) }
	status   = [pscustomobject]@{ state = 'Allocated' }
}
Assert-Roll (@(Select-SceneGameServersToDrain -GameServers @($emptyDrainValue) -TargetGeneration $target -MaxConcurrent 1).Count -eq 0) 'a drain label with an empty value still means the game server is draining'

Write-Output 'PASS: scene roll tells generations apart by build, template revision and restart token, and drains old-generation Allocated game servers one batch at a time (no cluster operations)'
