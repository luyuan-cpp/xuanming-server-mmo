param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("preflight", "build-image", "push-image", "release-zone", "release-all")]
    [string]$Command,

    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$DockerfilePath = "deploy/k8s/Dockerfile.runtime",
    [string]$ImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
    [string]$ImageTag = "latest",

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
        Test-RuntimePrerequisites
        Invoke-BuildImage
        Invoke-PushImage
        Invoke-K8sDeploy -DeployCommand "zone-up"
    }
    "release-all" {
        Test-RuntimePrerequisites
        Invoke-BuildImage
        Invoke-PushImage
        Invoke-K8sDeploy -DeployCommand "all-up"
    }
    default {
        throw "Unsupported command: $Command"
    }
}
