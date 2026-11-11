#requires -Version 7
# k8s_scene_roll.ps1 的纯函数契约(docs/design/scene-node-planned-drain.md §6):选哪些旧构建 GameServer 打排空标签、
# 还剩几台旧构建。只 dot-source 脚本取函数,不调 kubectl。
$ErrorActionPreference = 'Stop'
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
. (Join-Path $RepoRoot 'tools/scripts/k8s_scene_roll.ps1')

function Assert-Roll([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }

function New-FixtureGameServer([string]$Name, [string]$Build, [string]$State, [switch]$Draining) {
	$labels = [ordered]@{ 'mmorpg.io/build' = $Build }
	if ($Draining) { $labels['mmorpg.io/drain'] = 'true' }
	return [pscustomobject]@{
		metadata = [pscustomobject]@{ name = $Name; labels = [pscustomobject]$labels }
		status   = [pscustomobject]@{ state = $State }
	}
}

$fleet = @(
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -State 'Allocated'),
	(New-FixtureGameServer -Name 'scene-b' -Build 'v1' -State 'Ready'),
	(New-FixtureGameServer -Name 'scene-n' -Build 'v2' -State 'Allocated')
)

$first = @(Select-SceneGameServersToDrain -GameServers $fleet -TargetBuild 'v2' -MaxConcurrent 1)
Assert-Roll ($first.Count -eq 1 -and $first[0] -eq 'scene-a') 'must pick exactly one old-build Allocated game server, by name'
Assert-Roll ((Get-SceneOldBuildRemaining -GameServers $fleet -TargetBuild 'v2') -eq 3) 'every old-build game server counts as remaining, Ready ones included'

$two = @(Select-SceneGameServersToDrain -GameServers $fleet -TargetBuild 'v2' -MaxConcurrent 2)
Assert-Roll ($two.Count -eq 2 -and $two[0] -eq 'scene-a' -and $two[1] -eq 'scene-c') 'Ready old-build game servers are left to the Fleet rolling update'

$withDraining = @(
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -State 'Allocated' -Draining),
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -State 'Allocated')
)
Assert-Roll (@(Select-SceneGameServersToDrain -GameServers $withDraining -TargetBuild 'v2' -MaxConcurrent 1).Count -eq 0) 'no new label while one game server is still draining'

$shuttingDown = @(
	(New-FixtureGameServer -Name 'scene-a' -Build 'v1' -State 'Shutdown' -Draining),
	(New-FixtureGameServer -Name 'scene-c' -Build 'v1' -State 'Allocated')
)
$next = @(Select-SceneGameServersToDrain -GameServers $shuttingDown -TargetBuild 'v2' -MaxConcurrent 1)
Assert-Roll ($next.Count -eq 1 -and $next[0] -eq 'scene-c') 'a game server already shutting down frees its slot'
Assert-Roll ((Get-SceneOldBuildRemaining -GameServers $shuttingDown -TargetBuild 'v2') -eq 1) 'game servers in Shutdown are no longer counted'

$done = @((New-FixtureGameServer -Name 'scene-n' -Build 'v2' -State 'Allocated'))
Assert-Roll ((Get-SceneOldBuildRemaining -GameServers $done -TargetBuild 'v2') -eq 0) 'roll is complete when no old-build game server is left'
Assert-Roll (@(Select-SceneGameServersToDrain -GameServers @() -TargetBuild 'v2' -MaxConcurrent 1).Count -eq 0) 'an empty fleet needs no label'

Write-Output 'PASS: scene roll picks old-build Allocated game servers one batch at a time (no cluster operations)'
