param(
    [Parameter(Mandatory = $true)]
    [string]$BinarySourceRoot,

    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$ZoneInfoSource = "bin/zoneinfo",
    [string]$TableSource = "generated/tables"
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

function Copy-DirectoryContent {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Target
    )

    if (-not (Test-Path $Source)) {
        throw "source directory not found: $Source"
    }

    New-Item -ItemType Directory -Path $Target -Force | Out-Null
    Copy-Item -Path (Join-Path $Source "*") -Destination $Target -Recurse -Force
}

$binarySourceRootPath = Resolve-WorkspacePath -Path $BinarySourceRoot
$runtimeRootPath = Resolve-WorkspacePath -Path $RuntimeRoot
$zoneInfoSourcePath = Resolve-WorkspacePath -Path $ZoneInfoSource
$tableSourcePath = Resolve-WorkspacePath -Path $TableSource

$requiredBinaryNames = @("gate", "scene", "battle")
foreach ($binaryName in $requiredBinaryNames) {
    $binaryPath = Join-Path $binarySourceRootPath $binaryName
    if (-not (Test-Path $binaryPath)) {
        $windowsBinaryPath = Join-Path $binarySourceRootPath ("{0}.exe" -f $binaryName)
        if (Test-Path $windowsBinaryPath) {
            throw "Windows binary detected: $windowsBinaryPath. Stage Linux executables instead."
        }

        throw "required Linux binary not found: $binaryPath"
    }
}

$runtimeBinPath = Join-Path $runtimeRootPath "bin"
$runtimeGeneratedPath = Join-Path $runtimeRootPath "generated\generated_tables"
$runtimeZoneInfoPath = Join-Path $runtimeBinPath "zoneinfo"

New-Item -ItemType Directory -Path $runtimeBinPath -Force | Out-Null
foreach ($binaryName in $requiredBinaryNames) {
    Copy-Item -Path (Join-Path $binarySourceRootPath $binaryName) -Destination (Join-Path $runtimeBinPath $binaryName) -Force
}

Copy-DirectoryContent -Source $zoneInfoSourcePath -Target $runtimeZoneInfoPath
Copy-DirectoryContent -Source $tableSourcePath -Target $runtimeGeneratedPath

Write-Host "K8s runtime staged successfully:"
Write-Host "  binaries=$binarySourceRootPath"
Write-Host "  zoneinfo=$zoneInfoSourcePath"
Write-Host "  tables=$tableSourcePath"
Write-Host "  runtime_root=$runtimeRootPath"
