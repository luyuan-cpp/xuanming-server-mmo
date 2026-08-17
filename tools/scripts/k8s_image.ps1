param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("preflight", "build-image", "push-image", "release-zone", "release-all")]
    [string]$Command,

    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$DockerfilePath = "deploy/k8s/Dockerfile.runtime",
    [string]$ImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
    # 留空 = git 短 sha(脏树带 -dirty 后缀)。
    # 以前默认 "latest":registry 上被覆盖后,新旧 Deployment revision 指向同一个
    # digest,docs/ops/release-checklist.md §E.2 的三级回滚 `kubectl rollout undo`
    # 很可能什么都没换。
    [string]$ImageTag = "",
    # 发布档位,透传给 k8s_deploy.ps1;staging/prod 会跑发布预检并拒绝可变 tag。
    [ValidateSet("dev", "staging", "prod")]
    [string]$ReleaseProfile = "dev",
    # 明确允许脏工作树发布(只加 -dirty 标记不阻断)。prod 下依然拒绝。
    [switch]$AllowDirty,
    [switch]$SkipPreflight,

    [string]$ZoneName = "yesterday",
    [int]$ZoneId = 101,
    [string]$ZonesConfigPath = "",
    [string]$NamespacePrefix = "mmorpg-zone",
    [ValidateSet("custom", "managed-cloud", "bare-metal")]
    [string]$OpsProfile = "managed-cloud",
    [int]$CentreReplicas = 1,
    [int]$GateReplicas = 2,
    # Scene 角色拆分,-1 = 未指定(走 legacy 单池)。见 k8s_deploy.ps1 Resolve-SceneDeploymentPlan。
    [int]$SceneReplicas = 4,
    [int]$SceneWorldReplicas = -1,
    [int]$SceneInstanceReplicas = -1,
    [ValidateSet("deployment", "agones")]
    [string]$SceneOrchestrator = "deployment",
    [ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "LoadBalancer",
    [int]$GateServicePort = 18000,
    [switch]$SkipInfra,
    [switch]$WaitReady,
    [int]$WaitTimeoutSeconds = 180,
    [string]$KubeContext = "",
    [string]$KubeConfig = "",
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")

. (Join-Path $ScriptDir "lib\release_common.ps1")

# ─────────────────────────────────────────────────────────────────
# 不可变版本戳
# ─────────────────────────────────────────────────────────────────

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

if ([string]::IsNullOrWhiteSpace($ImageTag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -ImageTag。"
    }
    if ($script:ReleaseStamp.Dirty -and -not $AllowDirty) {
        throw "工作树是脏的(git status 非空),拒绝自动生成发布 tag。要么提交/清理工作树,要么显式加 -AllowDirty(tag 会带 -dirty 后缀)或 -ImageTag <tag>。"
    }
    $ImageTag = $script:ReleaseStamp.Tag
}

Write-Host "Image tag resolved: $ImageTag (profile=$ReleaseProfile dirty=$($script:ReleaseStamp.Dirty))"

<#
.SYNOPSIS
    发布路径(release-zone / release-all)的门禁:tag 必须不可变 + 配置预检必须过。

.DESCRIPTION
    "有检查器但没人调用"是最常见的失效模式,所以门禁挂在 release 入口上,
    不是写在 checklist 里靠人勾。
#>
function Invoke-ReleaseGate {
    $chk = Test-ImmutableImageTag -Tag $ImageTag -RejectDirty:($ReleaseProfile -eq 'prod')
    if (-not $chk.Ok) {
        throw "发布被阻断:$($chk.Reason)"
    }

    if ($SkipPreflight) {
        Write-Warning "已通过 -SkipPreflight 跳过发布预检。真实发布不应该走到这里。"
        return
    }

    $preflight = Join-Path $ScriptDir "release_preflight.ps1"
    if (-not (Test-Path $preflight)) {
        throw "release_preflight.ps1 不存在: $preflight(fail-closed:预检脚本缺失不等于预检通过)"
    }

    & $preflight -ReleaseProfile $ReleaseProfile -ImageTag $ImageTag -ImageRef @((Get-ImageRef))
    if ($LASTEXITCODE -ne 0) {
        throw "release preflight 未通过(exit=$LASTEXITCODE),发布被阻断。"
    }
}

function Resolve-WorkspacePath {
    param([Parameter(Mandatory = $true)][string]$Path)

    if ([System.IO.Path]::IsPathRooted($Path)) {
        return $Path
    }

    return [System.IO.Path]::GetFullPath((Join-Path $RepoRoot $Path))
}

function Get-ImageRef {
    return "{0}:{1}" -f $ImageRepository, $ImageTag
}

function Invoke-Docker {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Args
    )

    if ($DryRun) {
        Write-Host "[dry-run] docker $($Args -join ' ')"
        return
    }

    & docker @Args
    if ($LASTEXITCODE -ne 0) {
        throw "docker failed: docker $($Args -join ' ')"
    }
}

function Test-RuntimePrerequisites {
    $runtimeRootPath = Resolve-WorkspacePath -Path $RuntimeRoot
    $dockerfileFullPath = Resolve-WorkspacePath -Path $DockerfilePath

    $requiredPaths = @(
        (Join-Path $runtimeRootPath "bin/gate"),
        (Join-Path $runtimeRootPath "bin/scene"),
        (Join-Path $runtimeRootPath "bin/zoneinfo/Asia/Hong_Kong"),
        (Join-Path $runtimeRootPath "generated/generated_tables")
    )

    $missingPaths = @()
    foreach ($path in $requiredPaths) {
        if (-not (Test-Path $path)) {
            $missingPaths += $path
        }
    }

    if (-not (Test-Path $dockerfileFullPath)) {
        $missingPaths += $dockerfileFullPath
    }

    $windowsBinaryHints = @(
        (Join-Path $runtimeRootPath "bin/gate.exe"),
        (Join-Path $runtimeRootPath "bin/scene.exe")
    ) | Where-Object { Test-Path $_ }

    Write-Host "K8s image preflight:"
    Write-Host "  runtime_root=$runtimeRootPath"
    Write-Host "  dockerfile=$dockerfileFullPath"
    Write-Host "  image=$(Get-ImageRef)"

    if ($windowsBinaryHints.Count -gt 0) {
        Write-Host "  warning=Windows .exe files were found in runtime staging. Kubernetes manifests currently assume Linux containers."
    }

    if ($missingPaths.Count -gt 0) {
        foreach ($missingPath in $missingPaths) {
            Write-Host "  missing=$missingPath"
        }

        throw "K8s runtime preflight failed. Stage Linux runtime files under deploy/k8s/runtime/linux before building the image."
    }
}

function Invoke-BuildImage {
    Test-RuntimePrerequisites

    $dockerfileFullPath = Resolve-WorkspacePath -Path $DockerfilePath
    $imageRef = Get-ImageRef
    Invoke-Docker -Args @(
        "build",
        "-f", $dockerfileFullPath,
        "-t", $imageRef,
        "--build-arg", ("RUNTIME_ROOT={0}" -f $RuntimeRoot),
        $RepoRoot
    )
}

function Invoke-PushImage {
    $imageRef = Get-ImageRef
    Invoke-Docker -Args @("push", $imageRef)
}

function Invoke-K8sDeploy {
    param([Parameter(Mandatory = $true)][string]$DeployCommand)

    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_deploy.ps1 not found: $scriptPath"
    }

    $args = @{
        Command = $DeployCommand
        NodeImage = (Get-ImageRef)
        ReleaseProfile = $ReleaseProfile
        GoSvcTag = $ImageTag
        JavaSvcTag = $ImageTag
        ZoneName = $ZoneName
        ZoneId = $ZoneId
        NamespacePrefix = $NamespacePrefix
        OpsProfile = $OpsProfile
        CentreReplicas = $CentreReplicas
        GateReplicas = $GateReplicas
        SceneReplicas = $SceneReplicas
        SceneWorldReplicas = $SceneWorldReplicas
        SceneInstanceReplicas = $SceneInstanceReplicas
        SceneOrchestrator = $SceneOrchestrator
        GateServiceType = $GateServiceType
        GateServicePort = $GateServicePort
        WaitTimeoutSeconds = $WaitTimeoutSeconds
    }

    if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
        $args.ZonesConfigPath = $ZonesConfigPath
    }
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
        $args.KubeContext = $KubeContext
    }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
        $args.KubeConfig = $KubeConfig
    }
    if ($SkipInfra) {
        $args.SkipInfra = $true
    }
    if ($WaitReady) {
        $args.WaitReady = $true
    }
    if ($DryRun) {
        $args.DryRun = $true
    }
    if ($SkipPreflight) {
        # k8s_deploy 侧还有一道同样的门禁,这里已经跑过就不重复跑
        $args.SkipPreflight = $true
    }

    & $scriptPath @args
}

switch ($Command) {
    "preflight" {
        Test-RuntimePrerequisites
    }
    "build-image" {
        Invoke-BuildImage
    }
    "push-image" {
        Invoke-PushImage
    }
    "release-zone" {
        Invoke-ReleaseGate
        Test-RuntimePrerequisites
        Invoke-BuildImage
        Invoke-PushImage
        # 这里不传 -SkipPreflight:k8s_deploy 会在 apply 前再跑一次预检。
        # 预检是纯读文件的确定性检查,跑两次的代价只是多一段输出;
        # 而"传了 SkipPreflight" 会在 deploy 侧打出"门禁被跳过"的警告,
        # 那个警告必须只在真的被跳过时出现,不能被正常发布路径污染。
        Invoke-K8sDeploy -DeployCommand "zone-up"
    }
    "release-all" {
        Invoke-ReleaseGate
        Test-RuntimePrerequisites
        Invoke-BuildImage
        Invoke-PushImage
        Invoke-K8sDeploy -DeployCommand "all-up"
    }
    default {
        throw "Unsupported command: $Command"
    }
}
