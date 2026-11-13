#requires -Version 7
<#
.SYNOPSIS
	Agones 形态下把 scene Fleet 里旧一代、已分配(Allocated)的 GameServer 逐台排空,直到全部换成 Fleet 模板的当前这一代
	(docs/design/scene-node-planned-drain.md §4.6、§6)。
.DESCRIPTION
	为什么要这一步:Agones Fleet 的滚动更新与缩容只替换 Ready 的 GameServer;托着场景的 scene GameServer 是 Allocated,
	模板换了它们也会一直留在旧模板上。做法是 Agones 的标准做法 —— 给旧一代的 Allocated GameServer 打排空标签
	mmorpg.io/drain(与 battle 同键,进程只看键是否存在):scene 进程看到标签就计划排空(关建场景准入、发布 admission
	CLOSED,scene_manager 把它的世界频道迁走、玩家按限速疏散到别的节点),疏散完自己调 SDK /shutdown,Agones 删掉它,
	Fleet 按当前模板补一台新一代的。

	「哪一代」由三样决定,任何一样与 Fleet 的 GameServer 模板不同就是旧一代(Get-SceneGenerationKey):
	- 标签 mmorpg.io/build:镜像的构建标识;
	- 标签 mmorpg.io/revision:模板内容的修订(k8s_deploy.ps1 Get-SceneFleetTemplateRevision),只改配置的换版也会变;
	- 注解 mmorpg.io/restarted-at:重启令牌,只有 -Restart 会打。

	-Restart:模板没有任何变化、只想把整个 Fleet 逐台重启一遍时用,相当于 Deployment 的 kubectl rollout restart。
	先给 Fleet 的 GameServer 模板与它的 Pod 模板打上新的重启令牌(模板变了,Agones 自己替换 Ready 的 GameServer),
	再照常逐台排空 Allocated 的。令牌经 downward API 进到新 Pod 的 NodeInfo.build,scene_manager 因此分得出哪些已经重启过,
	疏散出来的玩家优先落到它们上面,不会再被搬一次。这个注解不在 zone-up 渲染的清单里,之后再 zone-up 不会把它抹掉。

	每轮最多让 -MaxConcurrent 台处在排空中(缺省 1 台,与 Deployment 的 maxUnavailable 0、新 Pod 先就绪同一思路:
	疏散出来的人要有地方去),排空中的那台消失后再打下一台。超时退出码 1,已打的标签不撤(排空是单向的)。
	只操作 -Namespace 里 -Fleet 名下的 GameServer;-DryRun 只打印要做的事。不负责更新 Fleet 模板的内容(zone-up 做)。
.EXAMPLE
	pwsh -File tools/scripts/k8s_scene_roll.ps1 -Namespace zone-1 -Fleet scene-world
.EXAMPLE
	pwsh -File tools/scripts/k8s_scene_roll.ps1 -Namespace zone-1 -Fleet scene-world -Restart
#>
[CmdletBinding()]
param(
	[string]$Namespace = '',
	[string]$Fleet = '',
	[ValidateRange(1, 100)][int]$MaxConcurrent = 1,
	[ValidateRange(1, 1440)][int]$TimeoutMinutes = 60,
	[ValidateRange(1, 300)][int]$PollSeconds = 10,
	[switch]$Restart,
	[switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# 与 C++ scene / battle 的 kAgonesDrainLabelKey、k8s_deploy.ps1 New-SceneFleetYaml 写的标签与注解同名;改名要几处同改。
$script:SceneDrainLabelKey = 'mmorpg.io/drain'
$script:SceneBuildLabelKey = 'mmorpg.io/build'
$script:SceneRevisionLabelKey = 'mmorpg.io/revision'
$script:SceneRestartedAtAnnotationKey = 'mmorpg.io/restarted-at'

# labels / annotations(ConvertFrom-Json 得到的对象)里有没有这个键。键名带点和斜杠,不用成员访问语法。
function Test-SceneMetadataKey {
	param($Map, [Parameter(Mandatory = $true)][string]$Key)

	return ($null -ne $Map -and $null -ne $Map.PSObject.Properties[$Key])
}

# 取 labels / annotations 里一个键的值;没有这个键或值为空都返回空串。
function Get-SceneMetadataValue {
	param($Map, [Parameter(Mandatory = $true)][string]$Key)

	if (-not (Test-SceneMetadataKey -Map $Map -Key $Key)) { return '' }
	return [string]$Map.PSObject.Properties[$Key].Value
}

<#
.SYNOPSIS
	纯函数:一份 metadata(GameServer 的,或 Fleet 的 GameServer 模板的)属于哪一代。只用来比相等。
.DESCRIPTION
	-RestartedAt 给了就用它顶替注解里的重启令牌:-Restart 的 dry-run 用它算「打完令牌之后」的目标。
#>
function Get-SceneGenerationKey {
	param(
		$Metadata,
		[AllowEmptyString()][string]$RestartedAt = $null
	)

	$labels = if ($null -ne $Metadata) { $Metadata.labels } else { $null }
	$annotations = if ($null -ne $Metadata) { $Metadata.annotations } else { $null }
	$build = Get-SceneMetadataValue -Map $labels -Key $script:SceneBuildLabelKey
	$revision = Get-SceneMetadataValue -Map $labels -Key $script:SceneRevisionLabelKey
	$token = if ($PSBoundParameters.ContainsKey('RestartedAt')) { $RestartedAt } else { Get-SceneMetadataValue -Map $annotations -Key $script:SceneRestartedAtAnnotationKey }
	return "build=$build revision=$revision restarted_at=$token"
}

<#
.SYNOPSIS
	纯函数:这一轮要给哪些 GameServer 打排空标签。不调 kubectl,单测直接调。
.DESCRIPTION
	GameServers 是 `kubectl get gameservers -o json` 的 items(只读 metadata.name / labels / annotations 与 status.state)。
	- 旧一代:Get-SceneGenerationKey 不等于 TargetGeneration;
	- 排空中:已有 mmorpg.io/drain 标签、还没消失的旧一代 GameServer(Shutdown 状态的不算,它马上就没了);
	- 候选:旧一代、Allocated、还没打标签的。Ready 的旧一代由 Fleet 的滚动更新自己替换,不打。
	本轮名额 = MaxConcurrent - 排空中台数;候选按名字排序取前若干台,结果确定。
#>
function Select-SceneGameServersToDrain {
	param(
		[object[]]$GameServers = @(),
		[Parameter(Mandatory = $true)][string]$TargetGeneration,
		[Parameter(Mandatory = $true)][int]$MaxConcurrent
	)

	$draining = 0
	$candidates = [Collections.Generic.List[string]]::new()
	foreach ($gs in $GameServers) {
		if ((Get-SceneGenerationKey -Metadata $gs.metadata) -ceq $TargetGeneration) { continue }
		$state = [string]$gs.status.state
		if (Test-SceneMetadataKey -Map $gs.metadata.labels -Key $script:SceneDrainLabelKey) {
			if ($state -ne 'Shutdown') { $draining++ }
			continue
		}
		if ($state -eq 'Allocated') { $candidates.Add([string]$gs.metadata.name) }
	}
	$slots = $MaxConcurrent - $draining
	if ($slots -le 0) { return @() }
	$names = $candidates.ToArray()
	[Array]::Sort($names, [StringComparer]::Ordinal)
	return @($names | Select-Object -First $slots)
}

<#
.SYNOPSIS
	纯函数:还剩几台旧一代的 GameServer(任何状态,Shutdown 的除外)。为 0 即滚动完成。
#>
function Get-SceneOldGenerationRemaining {
	param(
		[object[]]$GameServers = @(),
		[Parameter(Mandatory = $true)][string]$TargetGeneration
	)

	$remaining = 0
	foreach ($gs in $GameServers) {
		if ((Get-SceneGenerationKey -Metadata $gs.metadata) -cne $TargetGeneration -and [string]$gs.status.state -ne 'Shutdown') { $remaining++ }
	}
	return $remaining
}

<#
.SYNOPSIS
	纯函数:-Restart 给 Fleet 打重启令牌的合并补丁(JSON)。
.DESCRIPTION
	GameServer 模板与它的 Pod 模板两层都打同一个值:前者让新建的 GameServer 对象带上令牌(本脚本据此认出旧一代),
	后者让 Pod 经 downward API 读到(MMORPG_FLEET_RESTARTED_AT,C++ 拼进 NodeInfo.build)。
#>
function New-SceneFleetRestartPatch {
	param([Parameter(Mandatory = $true)][string]$Token)

	$annotations = @{ $script:SceneRestartedAtAnnotationKey = $Token }
	$patch = @{
		spec = @{
			template = @{
				metadata = @{ annotations = $annotations }
				spec     = @{ template = @{ metadata = @{ annotations = $annotations } } }
			}
		}
	}
	return ($patch | ConvertTo-Json -Depth 10 -Compress)
}

function Read-SceneFleetTemplateMetadata {
	$raw = & kubectl -n $Namespace get fleet $Fleet -o json
	if ($LASTEXITCODE -ne 0) { throw "Cannot read fleet $Namespace/$Fleet." }
	$metadata = ($raw | Out-String | ConvertFrom-Json).spec.template.metadata
	if ([string]::IsNullOrWhiteSpace((Get-SceneMetadataValue -Map $metadata.labels -Key $script:SceneBuildLabelKey))) {
		throw "Fleet $Namespace/$Fleet has no build label (spec.template.metadata.labels['$script:SceneBuildLabelKey'])."
	}
	return $metadata
}

function Invoke-SceneRoll {
	if ([string]::IsNullOrWhiteSpace($Namespace) -or [string]::IsNullOrWhiteSpace($Fleet)) {
		throw 'Both -Namespace and -Fleet are required.'
	}

	$target = $null
	if ($Restart) {
		$token = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
		if ($DryRun) {
			Write-Host "[scene-roll] dry-run: would set annotation $script:SceneRestartedAtAnnotationKey=$token on the game server template and pod template of fleet $Namespace/$Fleet"
			$target = Get-SceneGenerationKey -Metadata (Read-SceneFleetTemplateMetadata) -RestartedAt $token
		}
		else {
			$patchFile = Join-Path ([IO.Path]::GetTempPath()) ('scene-roll-restart-' + [Guid]::NewGuid().ToString('N') + '.json')
			try {
				Set-Content -LiteralPath $patchFile -Value (New-SceneFleetRestartPatch -Token $token) -Encoding utf8NoBOM
				& kubectl -n $Namespace patch fleet $Fleet --type merge --patch-file $patchFile | Out-Null
				if ($LASTEXITCODE -ne 0) { throw "Failed to set the restart token on fleet $Namespace/$Fleet." }
			}
			finally {
				Remove-Item -LiteralPath $patchFile -Force -ErrorAction SilentlyContinue
			}
			Write-Host "[scene-roll] restart token set on fleet $Namespace/${Fleet}: restarted_at=$token; every existing game server is now an old generation"
		}
	}
	if ($null -eq $target) {
		$target = Get-SceneGenerationKey -Metadata (Read-SceneFleetTemplateMetadata)
	}
	Write-Host "[scene-roll] fleet=$Namespace/$Fleet target=[$target] max_concurrent=$MaxConcurrent restart=$($Restart.IsPresent) dry_run=$($DryRun.IsPresent)"

	$deadline = [DateTime]::UtcNow.AddMinutes($TimeoutMinutes)
	while ($true) {
		$raw = & kubectl -n $Namespace get gameservers -l "agones.dev/fleet=$Fleet" -o json
		if ($LASTEXITCODE -ne 0) { throw "Cannot list game servers of fleet $Namespace/$Fleet." }
		$items = @(($raw | Out-String | ConvertFrom-Json).items)

		$remaining = Get-SceneOldGenerationRemaining -GameServers $items -TargetGeneration $target
		if ($remaining -eq 0) {
			Write-Host "[scene-roll] done: every game server of $Namespace/$Fleet is on the current generation [$target]"
			return 0
		}
		$toDrain = @(Select-SceneGameServersToDrain -GameServers $items -TargetGeneration $target -MaxConcurrent $MaxConcurrent)
		foreach ($name in $toDrain) {
			if ($DryRun) {
				Write-Host "[scene-roll] dry-run: would label gameserver $name with $script:SceneDrainLabelKey=true"
				continue
			}
			& kubectl -n $Namespace label gameserver $name "$script:SceneDrainLabelKey=true" --overwrite | Out-Null
			if ($LASTEXITCODE -ne 0) { throw "Failed to label gameserver $Namespace/$name." }
			Write-Host "[scene-roll] draining gameserver $name (old generation)"
		}
		if ($DryRun) { return 0 }
		if ([DateTime]::UtcNow -ge $deadline) {
			Write-Error "[scene-roll] timeout after $TimeoutMinutes minutes: $remaining old-generation game server(s) left in $Namespace/$Fleet; labels already applied stay (draining is one-way)"
			return 1
		}
		Write-Host "[scene-roll] waiting: old_generation_remaining=$remaining"
		Start-Sleep -Seconds $PollSeconds
	}
}

# 被 dot-source(单测)时只定义函数,不执行。
if ($MyInvocation.InvocationName -ne '.') {
	exit (Invoke-SceneRoll)
}
