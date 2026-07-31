param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("help", "pbgen-build", "pbgen-run", "proto-gen-build", "proto-gen-run", "tree", "naming-audit", "naming-apply", "third-party-grpc-build", "iwyu-run", "k8s-infra-up", "k8s-infra-down", "k8s-infra-status", "k8s-zone-up", "k8s-zone-down", "k8s-zone-status", "k8s-zone-rollback", "k8s-all-up", "k8s-all-down", "k8s-all-status", "k8s-build-all", "k8s-exposure-preflight", "k8s-stage-runtime", "k8s-image-preflight", "k8s-build-image", "k8s-push-image", "k8s-release-zone", "k8s-release-all", "go-svc-start", "go-svc-start-exe", "go-svc-stop", "go-svc-status", "go-svc-list", "go-svc-build", "go-svc-build-images", "go-svc-push-images", "java-svc-build-image", "java-svc-push-image", "cpp-node-start", "cpp-node-stop", "cpp-node-status", "cpp-node-list", "dev-start", "dev-start-exe", "dev-start-zones", "dev-stop", "dev-status", "dev-robot-zones", "merge-zone", "merge-zone-audit", "kafka-offset-reset")]
    [string]$Command,

    [string]$ConfigPath = "",

    [switch]$EnablePprof,
    [switch]$UseBinary,
    [switch]$UseGoRun,

    [ValidateSet("snake", "kebab")]
    [string]$Style = "snake",

    [int]$MaxChanges = 0,
    [int]$Jobs = 0,

    [string]$ZoneName = "yesterday",
    [int]$ZoneId = 101,
    [string]$NamespacePrefix = "mmorpg-zone",
    [string]$InfraNamespace = "mmorpg-infra",
    [string]$ZonesConfigPath = "",
    [string]$NodeImage = "ghcr.io/luyuancpp/mmorpg-node:latest",
    [ValidateSet("custom", "managed-cloud", "bare-metal")]
    [string]$OpsProfile = "custom",
    [string]$ImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
    [string]$ImageTag = "latest",
    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$DockerfilePath = "deploy/k8s/Dockerfile.runtime",
    [string]$BinarySourceRoot = "",
    [string]$ZoneInfoSource = "bin/zoneinfo",
    [string]$TableSource = "generated/tables",
    [int]$CentreReplicas = 1,
    [int]$GateReplicas = 2,
    # Scene 角色拆分,-1 = 未指定(走 legacy 单池)。见 k8s_deploy.ps1 Resolve-SceneDeploymentPlan。
    [int]$SceneReplicas = 4,
    [int]$SceneWorldReplicas = -1,
    [int]$SceneInstanceReplicas = -1,
    # Scene Node 编排方式:deployment(默认) | agones。见 docs/design/agones-scene-node-high-density.md。
    [ValidateSet("deployment", "agones")]
    [string]$SceneOrchestrator = "deployment",
    # Agones 高密度 / FleetAutoscaler 开关。
    # 这些以前只存在于 k8s_deploy.ps1,dev_tools.ps1 没透传 —— 而 dev_tools.ps1
    # 才是文档里的入口,等于这些功能从文档路径**完全不可达**。
    [switch]$AgonesHighDensity,
    [int]$AgonesRoomCapacity = 0,
    [switch]$AgonesAutoscale,
    [int]$AgonesBufferRooms = 5,
    [int]$AgonesMinReplicas = 1,
    [int]$AgonesMaxReplicas = 0,
    [ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "NodePort",
    [int]$GateServicePort = 18000,
    [switch]$SkipInfra,
    [switch]$SkipGoSvc,
    [string]$GoSvcRegistry = "ghcr.io/luyuancpp",
    [string]$GoSvcTag = "latest",
    [switch]$SkipJavaSvc,
    [string]$JavaSvcRegistry = "ghcr.io/luyuancpp",
    [string]$JavaSvcTag = "latest",
    [bool]$BuildRelease = $true,
    [bool]$BuildDebug = $true,
    # iwyu-run
    [string[]]$NodePath = @("cpp/nodes/scene"),
    [ValidateSet("auto", "iwyu", "clang-tidy")]
    [string]$IwyuTool = "auto",
    [switch]$FixIncludes,
    [switch]$ChangedOnly,
    [string]$DiffBase = "",
    [switch]$Clean,
    [switch]$SkipToolCheck,
    [switch]$DryRun,
    [switch]$WaitReady,
    [int]$WaitTimeoutSeconds = 180,
    [string]$KubeContext = "",
    [string]$KubeConfig = "",
    # go-svc-* commands
    [string[]]$GoServices = @(),
    # Per-service instance counts for local multi-open of Go services. Accepts:
    #   * hashtable (in-session use): -GoCounts @{ login = 2; scene_manager = 2 }
    #   * string list (pwsh -File):   -GoCounts login=2,scene_manager=2
    # Cap = 32 per service. 'db' is single-instance only.
    [object]$GoCounts = @{},
    [int]$GoPortStride = 1,
    # Disable tier-staged Go service startup (parallel launch like before).
    [switch]$NoTier,
    # Per-tier readiness budget (seconds). Soft TCP LISTEN probe per instance.
    [int]$TierReadySeconds = 10,
    # cpp-node-* / dev-* commands
    [string[]]$CppNodes = @(),
    [int]$GateCount = 1,
    [int]$SceneCount = 1,
    [switch]$UseVSGenerator,

    # Local multi-zone stress launch (dev-start-zones).
    # Accepts comma-separated ints, e.g. -Zones "1,2" or -Zones 1,2.
    # For dev-start-zones, defaults to 1,2 when empty. Forwarded to
    # go_services.ps1 / cpp_nodes.ps1 as -Zone <N> for each zone.
    [int[]]$Zones = @(),
    # Single-zone override forwarded to underlying scripts when callers want
    # to launch one specific zone (e.g. dev_tools.ps1 ... -Command go-svc-start -Zone 2).
    [int]$Zone = 0,
    # Port offset between zones (forwarded to go_services.ps1).
    [int]$ZonePortShift = 1000,
    # merge-zone command
    [int]$MergeSourceZone = 0,
    [int]$MergeTargetZone = 0,
    [string]$MergeMySqlDsn = "root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
    [string]$MergeRedisAddr = "127.0.0.1:6379",
    [string]$MergeRedisPassword = "",
    [int]$MergeRedisDB = 2,
    # merge-zone-audit only — switches to post-merge verification mode that
    # asserts source-zone state has been emptied. See merge-zone-runbook.md §5.5.
    [switch]$VerifyMerged,

    # ── kafka-offset-reset ────────────────────────────────────────
    # See tools/scripts/kafka_offset_reset.ps1 for full docs.
    # Used by zone rollback (docs/design/zone_data_rollback.md §3 step 4).
    [string]$KafkaBootstrapServer = "kafka:9092",
    [string]$KafkaGroup = "",
    [string]$KafkaTopic = "",
    [string]$KafkaToDatetime = "",      # ISO 8601 UTC, e.g. "2026-05-15T03:17:00Z"
    [switch]$KafkaToEarliest,
    [switch]$KafkaToLatest,
    [switch]$KafkaDeleteAndRecreate,
    [int]$KafkaPartitions = 0,
    [int]$KafkaReplicationFactor = 1,
    [string]$KafkaBin = "",
    [switch]$KafkaApply,

    # ── k8s-zone-rollback ─────────────────────────────────────────
    # See tools/scripts/k8s_zone_rollback.ps1 for full docs.
    # Wraps docs/design/zone_data_rollback.md §3 5-step disaster recovery.
    [string]$RollbackTargetTime = "",       # ISO 8601 UTC
    [string]$RollbackRedisHost = "",
    [string]$RollbackRedisPort = "6379",
    [string]$RollbackRedisPassword = "",
    [int]$RollbackRedisDB = 0,
    [string]$RollbackKafkaTopic = "db_task_topic",
    [string]$RollbackKafkaGroup = "db_rpc_consumer_group",
    [switch]$RollbackSkipMySqlPause,
    [int]$RollbackKafkaDrainTimeoutSec = 300,
    [switch]$RollbackApply
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
$ProtoGenDir = Join-Path $RepoRoot "tools\proto_generator\protogen"
$ProtoGenEnablePprofEnvVar = "PROTOGEN_ENABLE_PPROF"
$LegacyProtoGenEnablePprofEnvVar = "PBGEN_ENABLE_PPROF"

function Invoke-ProtoGenBuild {
    Push-Location $ProtoGenDir
    try {
        $legacyProtoGenExePath = Join-Path $ProtoGenDir "pbgen.exe"
        $primaryProtoGenExePath = Join-Path $ProtoGenDir "proto-gen.exe"
        $staleCmdExePath = Join-Path $ProtoGenDir "cmd.exe"

        go build -v -o $legacyProtoGenExePath ./cmd
        Copy-Item -Path $legacyProtoGenExePath -Destination $primaryProtoGenExePath -Force

        if (Test-Path $staleCmdExePath) {
            Remove-Item $staleCmdExePath -Force -ErrorAction SilentlyContinue
        }
    }
    finally {
        Pop-Location
    }
}

function Invoke-ProtoGenRun {
    Push-Location $ProtoGenDir
    $previousConfigPath = [Environment]::GetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", "Process")
    $previousEnablePprof = [Environment]::GetEnvironmentVariable($ProtoGenEnablePprofEnvVar, "Process")
    $previousLegacyEnablePprof = [Environment]::GetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, "Process")
    try {
        if ($UseBinary -and $UseGoRun) {
            throw "UseBinary and UseGoRun cannot be enabled at the same time."
        }

        if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", (Join-Path $ProtoGenDir "etc\proto_gen.yaml"), "Process")
        }
        else {
            $resolvedConfigPath = $ConfigPath
            if (-not [System.IO.Path]::IsPathRooted($resolvedConfigPath)) {
                $resolvedConfigPath = Join-Path $RepoRoot $resolvedConfigPath
            }

            $resolvedConfigPath = (Resolve-Path $resolvedConfigPath).Path
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $resolvedConfigPath, "Process")
        }

        if ($EnablePprof) {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, "1", "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $null, "Process")
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $null, "Process")
        }

        $primaryProtoGenExePath = Join-Path $ProtoGenDir "proto-gen.exe"
        $legacyProtoGenExePath = Join-Path $ProtoGenDir "pbgen.exe"

        if ($UseGoRun) {
            go run ./cmd
            return
        }

        if ($UseBinary) {
            if (Test-Path $primaryProtoGenExePath) {
                & $primaryProtoGenExePath
                return
            }

            if (Test-Path $legacyProtoGenExePath) {
                & $legacyProtoGenExePath
                return
            }

            throw "proto-gen binary not found. Expected $primaryProtoGenExePath or $legacyProtoGenExePath. Please run proto-gen-build first."
        }

        # Default strategy: prefer prebuilt proto-gen binary for faster startup, fall back to the legacy name, then go run.
        if (Test-Path $primaryProtoGenExePath) {
            & $primaryProtoGenExePath
        }
        elseif (Test-Path $legacyProtoGenExePath) {
            & $legacyProtoGenExePath
        }
        else {
            go run ./cmd
        }
    }
    finally {
        if ($null -ne $previousConfigPath) {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $previousConfigPath, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $null, "Process")
        }

        if ($null -ne $previousEnablePprof) {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $previousEnablePprof, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $null, "Process")
        }

        if ($null -ne $previousLegacyEnablePprof) {
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $previousLegacyEnablePprof, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $null, "Process")
        }

        Pop-Location
    }
}

function Invoke-Tree {
    $TreeScript = Join-Path $ScriptDir "tree.ps1"
    if (-not (Test-Path $TreeScript)) {
        throw "tree.ps1 not found: $TreeScript"
    }

    & $TreeScript
}

function Invoke-NamingAudit {
    $scriptPath = Join-Path $ScriptDir "normalize_names.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "normalize_names.ps1 not found: $scriptPath"
    }

    & $scriptPath -Mode audit -Style $Style -MaxChanges $MaxChanges
}

function Invoke-NamingApply {
    $scriptPath = Join-Path $ScriptDir "normalize_names.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "normalize_names.ps1 not found: $scriptPath"
    }

    & $scriptPath -Mode apply -Style $Style -MaxChanges $MaxChanges -Confirm:$false
}

function Invoke-ThirdPartyGrpcBuild {
    $scriptPath = Join-Path $ScriptDir "third_party\build_grpc.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "third_party/build_grpc.ps1 not found: $scriptPath"
    }

    $bound = $script:PSBoundParameters
    $args = @{}
    if ($bound.ContainsKey('BuildRelease')) {
        $args.BuildRelease = $BuildRelease
    }
    if ($bound.ContainsKey('BuildDebug')) {
        $args.BuildDebug = $BuildDebug
    }
    if ($bound.ContainsKey('Jobs')) {
        $args.Jobs = $Jobs
    }
    if ($Clean) {
        $args.Clean = $true
    }
    if ($SkipToolCheck) {
        $args.SkipToolCheck = $true
    }
    if ($UseVSGenerator) {
        $args.UseVSGenerator = $true
    }

    & $scriptPath @args
}

function Invoke-IwyuRun {
    $scriptPath = Join-Path $ScriptDir "iwyu_run.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "iwyu_run.ps1 not found: $scriptPath"
    }

    $args = @{
        NodePath = $NodePath
        Tool     = $IwyuTool
    }
    if ($FixIncludes) { $args.Fix = $true }
    if ($ChangedOnly) { $args.ChangedOnly = $true }
    if (-not [string]::IsNullOrWhiteSpace($DiffBase)) { $args.DiffBase = $DiffBase }

    & $scriptPath @args
}

function Invoke-K8sDeploy {
    param(
        [Parameter(Mandatory = $true)]
        [string]$K8sCommand
    )

    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_deploy.ps1 not found: $scriptPath"
    }

    $args = @{
        Command = $K8sCommand
        ZoneName = $ZoneName
        ZoneId = $ZoneId
        NamespacePrefix = $NamespacePrefix
        InfraNamespace = $InfraNamespace
        NodeImage = $NodeImage
        OpsProfile = $OpsProfile
        CentreReplicas = $CentreReplicas
        GateReplicas = $GateReplicas
        SceneReplicas = $SceneReplicas
        SceneWorldReplicas = $SceneWorldReplicas
        SceneInstanceReplicas = $SceneInstanceReplicas
        SceneOrchestrator = $SceneOrchestrator
        AgonesRoomCapacity = $AgonesRoomCapacity
        AgonesBufferRooms = $AgonesBufferRooms
        AgonesMinReplicas = $AgonesMinReplicas
        AgonesMaxReplicas = $AgonesMaxReplicas
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

    if ($SkipGoSvc) {
        $args.SkipGoSvc = $true
    }

    if (-not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
        $args.GoSvcRegistry = $GoSvcRegistry
        $args.GoSvcTag = $GoSvcTag
    }

    if ($SkipJavaSvc) {
        $args.SkipJavaSvc = $true
    }

    if (-not [string]::IsNullOrWhiteSpace($JavaSvcRegistry)) {
        $args.JavaSvcRegistry = $JavaSvcRegistry
        $args.JavaSvcTag = $JavaSvcTag
    }

    # switch 只在被指定时传下去。无条件传 $false 会让 k8s_deploy.ps1 里
    # "开了高密度却没给容量就报错" 那些判定失去意义。
    if ($AgonesHighDensity) {
        $args.AgonesHighDensity = $true
    }

    if ($AgonesAutoscale) {
        $args.AgonesAutoscale = $true
    }

    if ($DryRun) {
        $args.DryRun = $true
    }

    if ($WaitReady) {
        $args.WaitReady = $true
    }

    & $scriptPath @args
}

function Invoke-K8sBuildAll {
    Write-Host "=== Building C++ node image ===" -ForegroundColor Cyan
    $cppDockerfile = Join-Path $RepoRoot "deploy\k8s\Dockerfile.cpp"
    if ($DryRun) {
        Write-Host "[dry-run] docker build -f $cppDockerfile -t $NodeImage $RepoRoot"
    } else {
        & docker build -f $cppDockerfile -t $NodeImage $RepoRoot
        if ($LASTEXITCODE -ne 0) { throw "C++ node image build failed" }
    }

    Write-Host "`n=== Building Go service images ===" -ForegroundColor Cyan
    & (Join-Path $ScriptDir "go_svc_image.ps1") -Command build-all -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun

    Write-Host "`n=== Building Java service image ===" -ForegroundColor Cyan
    & (Join-Path $ScriptDir "java_svc_image.ps1") -Command build -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun

    Write-Host "`nAll images built successfully." -ForegroundColor Green
}

function Invoke-K8sExposurePreflight {
    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_deploy.ps1 not found: $scriptPath"
    }

    function Invoke-PreflightCase {
        param(
            [Parameter(Mandatory = $true)][string]$Title,
            [Parameter(Mandatory = $true)][string]$CaseZoneName,
            [Parameter(Mandatory = $true)][int]$CaseZoneId,
            [Parameter(Mandatory = $true)][string]$CaseOpsProfile,
            [Parameter(Mandatory = $true)][string]$ExpectedServiceType,
            [bool]$ExpectWarning = $false,
            [string]$CaseGateServiceType = ""
        )

        $caseArgs = @{
            Command = "zone-up"
            ZoneName = $CaseZoneName
            ZoneId = $CaseZoneId
            NamespacePrefix = $NamespacePrefix
            InfraNamespace = $InfraNamespace
            NodeImage = $NodeImage
            OpsProfile = $CaseOpsProfile
            CentreReplicas = $CentreReplicas
            GateReplicas = $GateReplicas
            SceneReplicas = $SceneReplicas
            GateServicePort = $GateServicePort
            WaitTimeoutSeconds = $WaitTimeoutSeconds
            DryRun = $true
        }

        if (-not [string]::IsNullOrWhiteSpace($CaseGateServiceType)) {
            $caseArgs.GateServiceType = $CaseGateServiceType
        }
        if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
            $caseArgs.KubeContext = $KubeContext
        }
        if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
            $caseArgs.KubeConfig = $KubeConfig
        }

        Write-Host "--- $Title ---" -ForegroundColor Cyan
        $output = & $scriptPath @caseArgs 2>&1
        $text = ($output | Out-String)
        $output | ForEach-Object { Write-Host $_ }

        $expectedMarker = "Ops profile resolved: profile=$CaseOpsProfile gate_service_type=$ExpectedServiceType"
        if ($text -notmatch [regex]::Escape($expectedMarker)) {
            throw "Preflight case '$Title' failed: expected marker not found -> $expectedMarker"
        }

        $warningMarker = "Using OpsProfile=custom with GateServiceType=LoadBalancer"
        if ($ExpectWarning) {
            if ($text -notmatch [regex]::Escape($warningMarker)) {
                throw "Preflight case '$Title' failed: expected warning not found."
            }
        }
        else {
            if ($text -match [regex]::Escape($warningMarker)) {
                throw "Preflight case '$Title' failed: unexpected warning found."
            }
        }

        Write-Host "PASS: $Title" -ForegroundColor Green
    }

    Invoke-PreflightCase -Title "CASE1 custom default" -CaseZoneName "preflight-a" -CaseZoneId 201 -CaseOpsProfile "custom" -ExpectedServiceType "NodePort"
    Invoke-PreflightCase -Title "CASE2 managed-cloud" -CaseZoneName "preflight-b" -CaseZoneId 202 -CaseOpsProfile "managed-cloud" -ExpectedServiceType "LoadBalancer"
    Invoke-PreflightCase -Title "CASE3 custom + LoadBalancer" -CaseZoneName "preflight-c" -CaseZoneId 203 -CaseOpsProfile "custom" -ExpectedServiceType "LoadBalancer" -CaseGateServiceType "LoadBalancer" -ExpectWarning $true

    Write-Host "k8s exposure preflight passed." -ForegroundColor Green
}

function Invoke-K8sImage {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ImageCommand
    )

    $scriptPath = Join-Path $ScriptDir "k8s_image.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_image.ps1 not found: $scriptPath"
    }

    $args = @{
        Command = $ImageCommand
        RuntimeRoot = $RuntimeRoot
        DockerfilePath = $DockerfilePath
        ImageRepository = $ImageRepository
        ImageTag = $ImageTag
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
        AgonesRoomCapacity = $AgonesRoomCapacity
        AgonesBufferRooms = $AgonesBufferRooms
        AgonesMinReplicas = $AgonesMinReplicas
        AgonesMaxReplicas = $AgonesMaxReplicas
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

function Invoke-K8sStageRuntime {
    $scriptPath = Join-Path $ScriptDir "k8s_stage_runtime.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_stage_runtime.ps1 not found: $scriptPath"
    }

    if ([string]::IsNullOrWhiteSpace($BinarySourceRoot)) {
        throw "-BinarySourceRoot is required for k8s-stage-runtime"
    }

    & $scriptPath `
        -BinarySourceRoot $BinarySourceRoot `
        -RuntimeRoot $RuntimeRoot `
        -ZoneInfoSource $ZoneInfoSource `
        -TableSource $TableSource
}

function Invoke-Help {
    @"
dev_tools.ps1 command help

Primary proto generator commands:
    -Command proto-gen-build
    -Command proto-gen-run

Compatibility aliases (legacy):
    -Command pbgen-build
    -Command pbgen-run

Useful proto-gen flags:
    -EnablePprof   Enable pprof wait mode (PROTOGEN_ENABLE_PPROF=1)
    -UseBinary     Force running proto-gen binary (proto-gen.exe, fallback: pbgen.exe)
    -UseGoRun      Force running go run ./cmd

Go micro-service commands (local dev):
    -Command go-svc-start [-GoServices login,db,...] [-GoCounts @{login=2;scene_manager=2}] [-GoPortStride N]
    -Command go-svc-start-exe [-GoServices login,db,...] [-GoCounts @{login=2}] [-GoPortStride N]
    -Command go-svc-stop  [-GoServices login,...]
    -Command go-svc-status
    -Command go-svc-list

Go micro-service build commands (local binary):
    -Command go-svc-build [-GoServices login,db,...]
        Build native binaries for selected (or all) Go services -> bin\go_services\

Go micro-service Docker image commands:
    -Command go-svc-build-images [-GoSvcRegistry <registry> -GoSvcTag <tag>]
    -Command go-svc-push-images  [-GoSvcRegistry <registry> -GoSvcTag <tag>]

Java service Docker image commands:
    -Command java-svc-build-image [-JavaSvcRegistry <registry> -JavaSvcTag <tag>]
    -Command java-svc-push-image  [-JavaSvcRegistry <registry> -JavaSvcTag <tag>]

C++ node commands (local dev):
    -Command cpp-node-start [-CppNodes gate,scene] [-GateCount N] [-SceneCount N]
    -Command cpp-node-stop  [-CppNodes gate,...]
    -Command cpp-node-status
    -Command cpp-node-list

Unified dev commands (C++ nodes + Go services):
    -Command dev-start  [-GateCount N] [-SceneCount N] [-GoCounts @{login=2}]
    -Command dev-start-exe  [-GateCount N] [-SceneCount N] [-GoCounts @{login=2}]
    -Command dev-stop
    -Command dev-status

Other common commands:
    -Command tree
    -Command naming-audit
    -Command naming-apply
    -Command third-party-grpc-build
    -Command iwyu-run
    -Command k8s-build-all
        Build all Docker images (C++ node + Go services + Java auth) before deployment.
    -Command k8s-infra-up | k8s-infra-down | k8s-infra-status
        Manage shared infrastructure (etcd/redis/kafka/mysql) in the mmorpg-infra namespace.
    -Command k8s-zone-up | k8s-zone-down | k8s-zone-status
    -Command k8s-all-up | k8s-all-down | k8s-all-status
    -Command k8s-exposure-preflight
    -Command k8s-stage-runtime
    -Command k8s-image-preflight | k8s-build-image | k8s-push-image | k8s-release-zone | k8s-release-all

Zone merge (合服) commands:
    -Command merge-zone -MergeSourceZone <id> -MergeTargetZone <id> [-DryRun]
        Migrates guild data (MySQL zone_id + Redis ranking ZSET) from source zone into target zone.
        Use -DryRun first to preview changes.

Proto-gen naming docs:
    tools/docs/proto_gen_naming_audit.md
    tools/docs/proto_gen_naming_migration.md
"@ | Write-Output
}

switch ($Command) {
    "help" { Invoke-Help }
    "pbgen-build" { Invoke-ProtoGenBuild }
    "pbgen-run" { Invoke-ProtoGenRun }
    "proto-gen-build" { Invoke-ProtoGenBuild }
    "proto-gen-run" { Invoke-ProtoGenRun }
    "tree" { Invoke-Tree }
    "naming-audit" { Invoke-NamingAudit }
    "naming-apply" { Invoke-NamingApply }
    "third-party-grpc-build" { Invoke-ThirdPartyGrpcBuild }
    "iwyu-run"              { Invoke-IwyuRun }
    "k8s-infra-up" { Invoke-K8sDeploy -K8sCommand "infra-up" }
    "k8s-infra-down" { Invoke-K8sDeploy -K8sCommand "infra-down" }
    "k8s-infra-status" { Invoke-K8sDeploy -K8sCommand "infra-status" }
    "k8s-zone-up" { Invoke-K8sDeploy -K8sCommand "zone-up" }
    "k8s-zone-down" { Invoke-K8sDeploy -K8sCommand "zone-down" }
    "k8s-zone-status" { Invoke-K8sDeploy -K8sCommand "zone-status" }
    "k8s-all-up" { Invoke-K8sDeploy -K8sCommand "all-up" }
    "k8s-all-down" { Invoke-K8sDeploy -K8sCommand "all-down" }
    "k8s-all-status" { Invoke-K8sDeploy -K8sCommand "all-status" }
    "k8s-build-all" { Invoke-K8sBuildAll }
    "k8s-exposure-preflight" { Invoke-K8sExposurePreflight }
    "k8s-stage-runtime" { Invoke-K8sStageRuntime }
    "k8s-image-preflight" { Invoke-K8sImage -ImageCommand "preflight" }
    "k8s-build-image" { Invoke-K8sImage -ImageCommand "build-image" }
    "k8s-push-image" { Invoke-K8sImage -ImageCommand "push-image" }
    "k8s-release-zone" { Invoke-K8sImage -ImageCommand "release-zone" }
    "k8s-release-all" { Invoke-K8sImage -ImageCommand "release-all" }
    "go-svc-start"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift }
    "go-svc-start-exe"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift }
    "go-svc-stop"   { & (Join-Path $ScriptDir "go_services.ps1") -Command stop   -Services $GoServices }
    "go-svc-status" { & (Join-Path $ScriptDir "go_services.ps1") -Command status }
    "go-svc-list"   { & (Join-Path $ScriptDir "go_services.ps1") -Command list   }
    "go-svc-build"        { & (Join-Path $ScriptDir "go_services.ps1") -Command build  -Services $GoServices }
    "go-svc-build-images" { & (Join-Path $ScriptDir "go_svc_image.ps1") -Command build-all -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun }
    "go-svc-push-images"  { & (Join-Path $ScriptDir "go_svc_image.ps1") -Command push-all  -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun }
    "java-svc-build-image" { & (Join-Path $ScriptDir "java_svc_image.ps1") -Command build -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun }
    "java-svc-push-image"  { & (Join-Path $ScriptDir "java_svc_image.ps1") -Command push  -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun }
    "cpp-node-start"  { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start  -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -Zone $Zone }
    "cpp-node-stop"   { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command stop   -Nodes $CppNodes }
    "cpp-node-status" { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command status }
    "cpp-node-list"   { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command list   }
    "dev-start" {
        Write-Host "=== Starting Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command start -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds
        Write-Host "`n=== Starting C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount
    }
    "dev-start-exe" {
        Write-Host "=== Starting Go services (exe) ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds
        Write-Host "`n=== Starting C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount
    }
    "dev-start-zones" {
        # Multi-zone local stress launch. Each zone gets its own go services
        # (zone-prefixed instance keys + derived yaml with ZoneId/port rewritten)
        # and its own gate/scene processes (ZONE_ID env var override).
        # Shared infra (etcd/Redis/Kafka/MySQL) is reused; per-zone DB schemas
        # (zone_<N>_db) must be pre-created via deploy/mysql-init/00_init_zone_dbs.sql.
        $zoneList = if ($Zones.Count -gt 0) { $Zones } else { @(1, 2) }
        Write-Host "=== Multi-zone launch: zones [$($zoneList -join ',')] ===" -ForegroundColor Cyan
        foreach ($z in $zoneList) {
            Write-Host "`n>>> Zone ${z}: starting Go services (exe) ..." -ForegroundColor Cyan
            & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $z -ZonePortShift $ZonePortShift
            Write-Host "`n>>> Zone ${z}: starting C++ nodes ..." -ForegroundColor Cyan
            & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -Zone $z
        }
        Write-Host "`nAll zones launched. Use 'dev status' to inspect." -ForegroundColor Green
    }
    "dev-stop" {
        Write-Host "=== Stopping Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command stop -Services $GoServices
        Write-Host "`n=== Stopping C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command stop -Nodes $CppNodes
    }
    "dev-status" {
        Write-Host "=== C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command status
        Write-Host "`n=== Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command status
    }
    "merge-zone" {
        # Drives tools/merge_zone/main.go directly via `go run`. The earlier
        # implementation invoked a `merge_zone.ps1` wrapper that never
        # actually existed in the repo — we route to `go run` to remove the
        # phantom dependency. See docs/ops/merge-zone-runbook.md §5.
        if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
            throw "merge-zone requires -MergeSourceZone <id> and -MergeTargetZone <id>"
        }
        $repoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
        $mergeGoArgs = @(
            "run", "./tools/merge_zone",
            "-source-zone", $MergeSourceZone,
            "-target-zone", $MergeTargetZone,
            "-mysql-dsn", $MergeMySqlDsn,
            "-redis-addr", $MergeRedisAddr,
            "-redis-db", $MergeRedisDB
        )
        if ($MergeRedisPassword -ne "") { $mergeGoArgs += @("-redis-password", $MergeRedisPassword) }
        if ($DryRun) { $mergeGoArgs += "-dry-run" } else { $mergeGoArgs += "-apply" }
        Push-Location $repoRoot
        try { & go @mergeGoArgs }
        finally { Pop-Location }
    }
    "merge-zone-audit" {
        # Read-only inspection — see tools/merge_zone/audit_resources.go and
        # docs/ops/merge-zone-runbook.md §4.2. Reports per-resource counts +
        # conflicts; never writes. Safe to run any time, even on a live zone.
        # T-1 day uses default mode; post-merge verification uses -VerifyMerged.
        if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
            throw "merge-zone-audit requires -MergeSourceZone <id> and -MergeTargetZone <id>"
        }
        $repoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
        $auditArgs = @(
            "run", "./tools/merge_zone",
            "-mode", "audit",
            "-source-zone", $MergeSourceZone,
            "-target-zone", $MergeTargetZone,
            "-mysql-dsn", $MergeMySqlDsn,
            "-redis-addr", $MergeRedisAddr,
            "-redis-db", $MergeRedisDB
        )
        if ($MergeRedisPassword -ne "") { $auditArgs += @("-redis-password", $MergeRedisPassword) }
        if ($VerifyMerged) { $auditArgs += "-verify-merged" }
        Push-Location $repoRoot
        try { & go @auditArgs }
        finally { Pop-Location }
    }
    "kafka-offset-reset" {
        # Wraps kafka_offset_reset.ps1 — see docs/design/zone_data_rollback.md §3 step 4.
        # Used during zone rollback to reset db_task_topic offsets before restarting a zone.
        $kArgs = @("-BootstrapServer", $KafkaBootstrapServer, "-Topic", $KafkaTopic)
        if ($KafkaGroup -ne "")               { $kArgs += @("-Group", $KafkaGroup) }
        if ($KafkaToDatetime -ne "")          { $kArgs += @("-ToDatetime", $KafkaToDatetime) }
        if ($KafkaToEarliest)                 { $kArgs += "-ToEarliest" }
        if ($KafkaToLatest)                   { $kArgs += "-ToLatest" }
        if ($KafkaDeleteAndRecreate)          { $kArgs += "-DeleteAndRecreateTopic" }
        if ($KafkaPartitions -gt 0)           { $kArgs += @("-Partitions", $KafkaPartitions) }
        if ($KafkaReplicationFactor -ne 1)    { $kArgs += @("-ReplicationFactor", $KafkaReplicationFactor) }
        if ($KafkaBin -ne "")                 { $kArgs += @("-KafkaBin", $KafkaBin) }
        if ($KafkaApply)                      { $kArgs += "-Apply" }
        & (Join-Path $ScriptDir "kafka_offset_reset.ps1") @kArgs
    }
    "k8s-zone-rollback" {
        # Wraps k8s_zone_rollback.ps1 — see docs/design/zone_data_rollback.md §3.
        # Orchestrates: zone-down → Kafka drain → MySQL PITR (manual) → Redis FLUSHDB
        #             → Kafka offset reset → zone-up → verification checklist.
        if ($RollbackTargetTime -eq "") {
            throw "k8s-zone-rollback requires -RollbackTargetTime (ISO 8601 UTC, e.g. '2026-05-15T14:23:00Z')"
        }
        $rbArgs = @(
            "-ZoneName", $ZoneName,
            "-ZoneId", $ZoneId,
            "-TargetTime", $RollbackTargetTime,
            "-KafkaBootstrap", $KafkaBootstrapServer,
            "-KafkaTopic", $RollbackKafkaTopic,
            "-KafkaGroup", $RollbackKafkaGroup,
            "-NodeImage", $NodeImage,
            "-NamespacePrefix", $NamespacePrefix,
            "-KafkaDrainTimeoutSec", $RollbackKafkaDrainTimeoutSec
        )
        if ($RollbackRedisHost -ne "")     { $rbArgs += @("-RedisHost", $RollbackRedisHost) }
        if ($RollbackRedisPort -ne "6379") { $rbArgs += @("-RedisPort", $RollbackRedisPort) }
        if ($RollbackRedisPassword -ne "") { $rbArgs += @("-RedisPassword", $RollbackRedisPassword) }
        if ($RollbackRedisDB -ne 0)        { $rbArgs += @("-RedisDB", $RollbackRedisDB) }
        if ($RollbackSkipMySqlPause)       { $rbArgs += "-SkipMySqlPause" }
        if ($RollbackApply)                { $rbArgs += "-Apply" }
        & (Join-Path $ScriptDir "k8s_zone_rollback.ps1") @rbArgs
    }
    "dev-robot-zones" {
        # Per-zone parallel robot launch. Builds robot once, then for each
        # requested zone derives a yaml at run/etc/robot/robot.z<N>.yaml with
        # zone_id rewritten and account_fmt prefixed by 'z<N>_' to avoid
        # account-name collisions across zones. Each robot is launched in its
        # own console window so stress output stays separable.
        $zoneList = if ($Zones.Count -gt 0) { $Zones } else { @(1, 2) }
        $robotDir = Join-Path $RepoRoot "robot"
        $robotExe = Join-Path $robotDir "robot.exe"
        $baseYaml = Join-Path $robotDir "etc\robot.yaml"
        if (-not (Test-Path $baseYaml)) { throw "Base robot yaml not found: $baseYaml" }

        Write-Host "=== Building robot ===" -ForegroundColor Cyan
        Push-Location $robotDir
        try {
            go build -o robot.exe .
            if ($LASTEXITCODE -ne 0) { throw "robot build failed" }
        } finally { Pop-Location }

        $derivedDir = Join-Path $RepoRoot "run\etc\robot"
        if (-not (Test-Path $derivedDir)) { New-Item -ItemType Directory -Path $derivedDir -Force | Out-Null }

        $baseContent = Get-Content $baseYaml -Raw
        $zoneRegex    = [regex]::new('(?m)^(?<lead>zone_id:\s*)\d+\s*(?<tail>(#.*)?)$')
        $accountRegex = [regex]::new('(?m)^(?<lead>account_fmt:\s*")(?<fmt>[^"\r\n]+)(?<tail>".*)$')

        foreach ($z in $zoneList) {
            $content = $baseContent
            if ($zoneRegex.IsMatch($content)) {
                $content = $zoneRegex.Replace(
                    $content,
                    { param($m) "$($m.Groups['lead'].Value)$z $($m.Groups['tail'].Value)".TrimEnd() },
                    1
                )
            } else {
                Write-Warning "No 'zone_id:' line found in $baseYaml; zone $z derived yaml may not target the right zone."
            }
            if ($accountRegex.IsMatch($content)) {
                $content = $accountRegex.Replace(
                    $content,
                    { param($m) "$($m.Groups['lead'].Value)z${z}_$($m.Groups['fmt'].Value)$($m.Groups['tail'].Value)" },
                    1
                )
            }
            $derivedYaml = Join-Path $derivedDir "robot.z${z}.yaml"
            Set-Content -Path $derivedYaml -Value $content -Encoding UTF8 -NoNewline
            Write-Host "[zone $z] cfg -> $derivedYaml" -ForegroundColor DarkGray

            # Launch in a new console window so each zone's stress output is
            # visible side-by-side. -WindowStyle Normal so it actually pops up.
            $title = "robot-z$z"
            Start-Process -FilePath "cmd.exe" `
                -ArgumentList @("/k", "title $title && `"$robotExe`" -c `"$derivedYaml`"") `
                -WorkingDirectory $robotDir `
                -WindowStyle Normal | Out-Null
            Write-Host "[zone $z] robot launched (window title: $title)" -ForegroundColor Green
        }
    }
    default { throw "Unsupported command: $Command" }
}
