#requires -Version 7
param(
    # list-refs:每行输出一个完整镜像引用(Write-Output,无其它输出),给 publish_images.ps1 等机器读。
    [Parameter(Mandatory = $true)]
    [ValidateSet("preflight", "build-image", "push-image", "release-zone", "release-all", "list-refs")]
    [string]$Command,

    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$DockerfilePath = "deploy/k8s/Dockerfile.runtime",
    [string]$ImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
    # 留空 = Get-ReleaseImageTag:有发布版本号时 <vX.Y.Z>-<12位sha>,否则 12 位 git sha(脏树带 -dirty 后缀)。
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
    [switch]$DryRun,
    # 新参数一律追加在末尾:本脚本没有显式 Position,改变声明顺序会改变位置参数绑定。
    # 发布版本号 vX.Y.Z(可空 = 快照构建)。为空时回落 $env:MMORPG_RELEASE_VERSION。
    # 非空时:镜像 tag = <版本>-<12位sha>,BUILD_VERSION = 版本号,脏树一律拒绝(-AllowDirty 也不放行)。
    [string]$Version = "",
    # push-image / release-zone / release-all 推送后把 "ref -> repo@sha256:..." 合并写进这个 JSON。
    # tag 只是别名,部署/回滚要按 digest 定位;推完回读不到 digest 视为发布失败。
    [string]$DigestsOut = "",
    # C++ 日志 sidecar 开关,透传给 k8s_deploy.ps1(见 docs/ops/grafana-loki-local-logs.md §6)。
    # 两个字符串默认留空 = 用 k8s_deploy.ps1 自己的默认值,非空才透传;
    # 尤其 CppLogSidecarImage 透传空串会让生成的清单出现空 image:,Pod 建不起来。
    [switch]$NoCppLogSidecar,
    [string]$CppLogSidecarImage = "",
    [string]$LokiPushUrl = ""
)

$ErrorActionPreference = "Stop"

# 逐级 Split-Path 上溯,不拼 "..\..":反斜杠在 Linux pwsh 里不是路径分隔符。
$ScriptDir = $PSScriptRoot
$RepoRoot = (Resolve-Path (Split-Path -Parent (Split-Path -Parent $ScriptDir))).Path

. (Join-Path $ScriptDir "lib" "release_common.ps1")

# ─────────────────────────────────────────────────────────────────
# 发布版本号 + 不可变版本戳
# ─────────────────────────────────────────────────────────────────

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

# 优先级:-Version > $env:MMORPG_RELEASE_VERSION > 空。两个来源同一套校验,
# 环境变量里的非法值不能因为"不是命令行传的"就被放过。
$script:ReleaseVersion = ''
if (-not [string]::IsNullOrEmpty($Version)) {
    $script:ReleaseVersion = $Version
}
elseif (-not [string]::IsNullOrEmpty($env:MMORPG_RELEASE_VERSION)) {
    $script:ReleaseVersion = $env:MMORPG_RELEASE_VERSION
}
if ($script:ReleaseVersion) {
    $versionCheck = Test-ReleaseVersion -Version $script:ReleaseVersion
    if (-not $versionCheck.Ok) {
        throw "发布版本号校验失败:$($versionCheck.Reason)"
    }
    if (-not $script:ReleaseStamp.Ok) {
        throw "指定了发布版本 $($script:ReleaseVersion),但取不到 git commit:$($script:ReleaseStamp.Reason)。发布镜像必须能追溯到 commit。"
    }
    # 显式 -ImageTag 也不放行:版本号会被写进镜像 LABEL / BUILD_INFO,挂在脏树产物上就无法从 commit 还原。
    if ($script:ReleaseStamp.Dirty) {
        throw "发布版本 $($script:ReleaseVersion) 不接受脏工作树(git status 非空),-AllowDirty / -ImageTag 都不能绕过:脏树产物无法从 commit $($script:ReleaseStamp.Commit) 还原。请先提交或清理工作树。"
    }
}

if ([string]::IsNullOrWhiteSpace($ImageTag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -ImageTag。"
    }
    if ($script:ReleaseStamp.Dirty -and -not $AllowDirty) {
        throw "工作树是脏的(git status 非空),拒绝自动生成发布 tag。要么提交/清理工作树,要么显式加 -AllowDirty(tag 会带 -dirty 后缀)或 -ImageTag <tag>。"
    }
    $ImageTag = Get-ReleaseImageTag -Version $script:ReleaseVersion -Commit $script:ReleaseStamp.Commit -Dirty:$script:ReleaseStamp.Dirty
}

# 注入镜像的版本三元组(接口 D)。快照构建没有版本号,用镜像 tag 充当,至少能和 registry 对上。
$script:BuildVersion = if ($script:ReleaseVersion) { $script:ReleaseVersion } else { $ImageTag }
$script:BuildCommit = if ($script:ReleaseStamp.Ok) { $script:ReleaseStamp.Commit } else { 'unknown' }
# 显式 InvariantCulture + 引号包住分隔符:自定义格式里的 ':' 是"区域时间分隔符",不是字面冒号。
$script:BuildTime = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)

# list-refs 的 stdout 只能有镜像引用:pwsh -File 子进程里 Write-Host 也会写进 stdout。
if ($Command -ne 'list-refs') {
    Write-Host "Image tag resolved: $ImageTag (profile=$ReleaseProfile dirty=$($script:ReleaseStamp.Dirty) version=$($script:BuildVersion) commit=$($script:BuildCommit))"
}

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

    # $env:MMORPG_DOCKER_COMMAND 可换成测试桩(release_common.ps1 同一口径),契约测试不依赖真实 docker。
    $docker = Resolve-ReleaseDockerCommand
    & $docker @Args
    if ($LASTEXITCODE -ne 0) {
        throw "docker failed: docker $($Args -join ' ')"
    }
}

<#
.SYNOPSIS
    需要把 commit 编进 C++ 二进制时,给 Dockerfile.cpp 的 builder 阶段传的 build-arg。

.DESCRIPTION
    只在"发布版本(-Version / MMORPG_RELEASE_VERSION)或显式 $env:MMORPG_STAMP_BUILD=1"且
    Dockerfile 声明了 ARG MMORPG_STAMP_BUILD 时才传。默认的 Dockerfile.runtime 打包预编译二进制,
    没有 builder 阶段,传了只会得到 docker 的"未使用 build-arg"告警。
    快照构建默认不传:这个参数一变,builder 层就要全量重编 C++。
#>
function Get-BinaryStampBuildArgs {
    param([Parameter(Mandatory = $true)][string]$DockerfileFullPath)

    $wantStamp = [bool]$script:ReleaseVersion -or ($env:MMORPG_STAMP_BUILD -eq '1')
    if (-not $wantStamp) {
        return @()
    }
    if (-not (Select-String -LiteralPath $DockerfileFullPath -Pattern '^\s*ARG\s+MMORPG_STAMP_BUILD\b' -Quiet)) {
        return @()
    }
    if ($script:BuildCommit -cnotmatch '^[0-9a-f]{12}$') {
        throw "要把 commit 编进二进制(MMORPG_STAMP_BUILD),但取不到 12 位 git commit:$($script:ReleaseStamp.Reason)"
    }
    # 与 build_linux.sh 在宿主机上的口径一致:脏树加 -dirty,别让脏产物冒充干净 commit。
    $stampCommit = if ($script:ReleaseStamp.Dirty) { "$($script:BuildCommit)-dirty" } else { $script:BuildCommit }
    return @(
        "--build-arg", "MMORPG_STAMP_BUILD=1",
        "--build-arg", ("MMORPG_BUILD_COMMIT={0}" -f $stampCommit)
    )
}

<#
.SYNOPSIS
    -DigestsOut 非空时,回读刚推送镜像的 digest 并合并写进记录文件。
#>
function Save-PushedImageDigest {
    param([Parameter(Mandatory = $true)][string]$ImageRef)

    if ([string]::IsNullOrWhiteSpace($DigestsOut)) {
        return
    }
    if ($DryRun) {
        Write-Host "[dry-run] record digest of $ImageRef -> $DigestsOut"
        return
    }

    $pushed = Get-PushedImageDigest -ImageRef $ImageRef
    if (-not $pushed.Ok) {
        throw "推送后回读 digest 失败,发布记录不完整:$($pushed.Reason)"
    }
    Write-ImageDigestRecord -Path $DigestsOut -ImageRef $ImageRef -Digest $pushed.Digest
    Write-Host "  digest=$($pushed.Digest) -> $DigestsOut"
}

function Test-RuntimePrerequisites {
    $runtimeRootPath = Resolve-WorkspacePath -Path $RuntimeRoot
    $dockerfileFullPath = Resolve-WorkspacePath -Path $DockerfilePath

    $requiredPaths = @(
        (Join-Path $runtimeRootPath "bin/gate"),
        (Join-Path $runtimeRootPath "bin/scene"),
        (Join-Path $runtimeRootPath "bin/battle"),
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
        (Join-Path $runtimeRootPath "bin/scene.exe"),
        (Join-Path $runtimeRootPath "bin/battle.exe")
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
    # revision label 在命令行再给一次(Dockerfile 里也有):publish_images.ps1 按它核对镜像 == 当前 commit,
    # 换一个没声明 LABEL 的 Dockerfile 时这条也不会丢。
    Invoke-Docker -Args (@(
        "build",
        "-f", $dockerfileFullPath,
        "-t", $imageRef,
        "--build-arg", ("RUNTIME_ROOT={0}" -f $RuntimeRoot),
        "--build-arg", ("BUILD_VERSION={0}" -f $script:BuildVersion),
        "--build-arg", ("BUILD_COMMIT={0}" -f $script:BuildCommit),
        "--build-arg", ("BUILD_TIME={0}" -f $script:BuildTime),
        "--label", ("org.opencontainers.image.revision={0}" -f $script:BuildCommit)
    ) + @(Get-BinaryStampBuildArgs -DockerfileFullPath $dockerfileFullPath) + @($RepoRoot))
}

function Invoke-PushImage {
    $imageRef = Get-ImageRef
    Invoke-Docker -Args @("push", $imageRef)
    Save-PushedImageDigest -ImageRef $imageRef
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
    # switch 只在被指定时传下去,字符串非空才传:无条件传 $false / 空串会覆盖下游默认值。
    if ($NoCppLogSidecar) {
        $args.NoCppLogSidecar = $true
    }
    if (-not [string]::IsNullOrWhiteSpace($CppLogSidecarImage)) {
        $args.CppLogSidecarImage = $CppLogSidecarImage
    }
    if (-not [string]::IsNullOrWhiteSpace($LokiPushUrl)) {
        $args.LokiPushUrl = $LokiPushUrl
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
    "list-refs" {
        Write-Output (Get-ImageRef)
    }
    default {
        throw "Unsupported command: $Command"
    }
}
