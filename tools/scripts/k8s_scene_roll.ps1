#requires -Version 7
<#
.SYNOPSIS
	Agones 形态下把 scene Fleet 里旧构建、已分配(Allocated)的 GameServer 逐台排空,直到全部换成 Fleet 模板的当前构建
	(docs/design/scene-node-planned-drain.md §6)。
.DESCRIPTION
	为什么要这一步:Agones Fleet 的滚动更新与缩容只替换 Ready 的 GameServer;托着场景的 scene GameServer 是 Allocated,
	模板换了构建它们也会一直留在旧构建上。做法是 Agones 的标准做法 —— 给旧构建的 Allocated GameServer 打排空标签
	mmorpg.io/drain(与 battle 同键,进程只看键是否存在):scene 进程看到标签就计划排空(关建场景准入、发布 admission
	CLOSED,scene_manager 把它的世界频道迁走、玩家按限速疏散到别的节点),疏散完自己调 SDK /shutdown,Agones 删掉它,
	Fleet 按当前模板补一台新构建的。
	每轮最多让 -MaxConcurrent 台处在排空中(缺省 1 台,与 Deployment 的 maxUnavailable 0、新 Pod 先就绪同一思路:
	疏散出来的人要有地方去),排空中的那台消失后再打下一台。超时退出码 1,已打的标签不撤(排空是单向的)。
	只操作 -Namespace 里 -Fleet 名下的 GameServer;-DryRun 只打印要打标签的名字。不负责更新 Fleet 模板(zone-up 做)。
.EXAMPLE
	pwsh -File tools/scripts/k8s_scene_roll.ps1 -Namespace zone-1 -Fleet scene-world
#>
[CmdletBinding()]
param(
	[string]$Namespace = '',
	[string]$Fleet = '',
	[ValidateRange(1, 100)][int]$MaxConcurrent = 1,
	[ValidateRange(1, 1440)][int]$TimeoutMinutes = 60,
	[ValidateRange(1, 300)][int]$PollSeconds = 10,
	[switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# 与 C++ scene / battle 的 kAgonesDrainLabelKey、Fleet 模板的 mmorpg.io/build 标签同名;改名要几处同改。
$script:SceneDrainLabelKey = 'mmorpg.io/drain'
$script:SceneBuildLabelKey = 'mmorpg.io/build'

<#
.SYNOPSIS
	纯函数:这一轮要给哪些 GameServer 打排空标签。不调 kubectl,单测直接调。
.DESCRIPTION
	GameServers 是 `kubectl get gameservers -o json` 的 items(只读 metadata.name、metadata.labels、status.state)。
	- 旧构建:mmorpg.io/build 不等于 TargetBuild;
	- 排空中:已有 mmorpg.io/drain 标签、还没消失的旧构建 GameServer(Shutdown 状态的不算,它马上就没了);
	- 候选:旧构建、Allocated、还没打标签的。Ready 的旧构建由 Fleet 的滚动更新自己替换,不打。
	本轮名额 = MaxConcurrent - 排空中台数;候选按名字排序取前若干台,结果确定。
#>
function Select-SceneGameServersToDrain {
	param(
		[object[]]$GameServers = @(),
		[Parameter(Mandatory = $true)][string]$TargetBuild,
		[Parameter(Mandatory = $true)][int]$MaxConcurrent
	)

	$draining = 0
	$candidates = [Collections.Generic.List[string]]::new()
	foreach ($gs in $GameServers) {
		$labels = $gs.metadata.labels
		$build = if ($null -ne $labels) { $labels.$script:SceneBuildLabelKey } else { $null }
		if ($build -ceq $TargetBuild) { continue }
		$state = [string]$gs.status.state
		$hasDrainLabel = $null -ne $labels -and $null -ne $labels.$script:SceneDrainLabelKey
		if ($hasDrainLabel) {
			if ($state -ne 'Shutdown') { $draining++ }
			continue
		}
		if ($state -eq 'Allocated') { $candidates.Add([string]$gs.metadata.name) }
	}
	$slots = $MaxConcurrent - $draining
	if ($slots -le 0) { return @() }
	return @($candidates | Sort-Object | Select-Object -First $slots)
}

<#
.SYNOPSIS
	纯函数:还剩几台旧构建的 GameServer(任何状态,Shutdown 的除外)。为 0 即滚动完成。
#>
function Get-SceneOldBuildRemaining {
	param(
		[object[]]$GameServers = @(),
		[Parameter(Mandatory = $true)][string]$TargetBuild
	)

	$remaining = 0
	foreach ($gs in $GameServers) {
		$labels = $gs.metadata.labels
		$build = if ($null -ne $labels) { $labels.$script:SceneBuildLabelKey } else { $null }
		if ($build -cne $TargetBuild -and [string]$gs.status.state -ne 'Shutdown') { $remaining++ }
	}
	return $remaining
}

function Invoke-SceneRoll {
	if ([string]::IsNullOrWhiteSpace($Namespace) -or [string]::IsNullOrWhiteSpace($Fleet)) {
		throw 'Both -Namespace and -Fleet are required.'
	}
	$target = (& kubectl -n $Namespace get fleet $Fleet -o "jsonpath={.spec.template.metadata.labels.mmorpg\.io/build}")
	if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($target)) {
		throw "Cannot read the build label of fleet $Namespace/$Fleet (spec.template.metadata.labels['$script:SceneBuildLabelKey'])."
	}
	$target = $target.Trim()
	Write-Host "[scene-roll] fleet=$Namespace/$Fleet target_build=$target max_concurrent=$MaxConcurrent dry_run=$($DryRun.IsPresent)"

	$deadline = [DateTime]::UtcNow.AddMinutes($TimeoutMinutes)
	while ($true) {
		$raw = & kubectl -n $Namespace get gameservers -l "agones.dev/fleet=$Fleet" -o json
		if ($LASTEXITCODE -ne 0) { throw "Cannot list game servers of fleet $Namespace/$Fleet." }
		$items = @(($raw | Out-String | ConvertFrom-Json).items)

		$remaining = Get-SceneOldBuildRemaining -GameServers $items -TargetBuild $target
		if ($remaining -eq 0) {
			Write-Host "[scene-roll] done: every game server of $Namespace/$Fleet runs build $target"
			return 0
		}
		$toDrain = @(Select-SceneGameServersToDrain -GameServers $items -TargetBuild $target -MaxConcurrent $MaxConcurrent)
		foreach ($name in $toDrain) {
			if ($DryRun) {
				Write-Host "[scene-roll] dry-run: would label gameserver $name with $script:SceneDrainLabelKey=true"
				continue
			}
			& kubectl -n $Namespace label gameserver $name "$script:SceneDrainLabelKey=true" --overwrite | Out-Null
			if ($LASTEXITCODE -ne 0) { throw "Failed to label gameserver $Namespace/$name." }
			Write-Host "[scene-roll] draining gameserver $name (old build)"
		}
		if ($DryRun) { return 0 }
		if ([DateTime]::UtcNow -ge $deadline) {
			Write-Error "[scene-roll] timeout after $TimeoutMinutes minutes: $remaining old-build game server(s) left in $Namespace/$Fleet; labels already applied stay (draining is one-way)"
			return 1
		}
		Write-Host "[scene-roll] waiting: old_build_remaining=$remaining"
		Start-Sleep -Seconds $PollSeconds
	}
}

# 被 dot-source(单测)时只定义函数,不执行。
if ($MyInvocation.InvocationName -ne '.') {
	exit (Invoke-SceneRoll)
}
