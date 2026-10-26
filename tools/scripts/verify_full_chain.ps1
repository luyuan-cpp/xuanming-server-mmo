param(
    [string]$Configuration = "Debug",
    [string]$Platform = "x64",
    [switch]$SkipClean,
    [switch]$SkipProtobufBuild,
    [switch]$SkipGrpcClientVerify
)

$ErrorActionPreference = "Stop"

function Resolve-MSBuildPath {
    $pf86 = [Environment]::GetFolderPath("ProgramFilesX86")
    $vswhere = Join-Path $pf86 "Microsoft Visual Studio/Installer/vswhere.exe"
    if (!(Test-Path $vswhere)) {
        throw "vswhere.exe not found: $vswhere"
    }

    $msbuild = & $vswhere -latest -products * -requires Microsoft.Component.MSBuild -find "MSBuild/**/Bin/MSBuild.exe" | Select-Object -First 1
    if (-not $msbuild) {
        throw "MSBuild.exe not found via vswhere"
    }

    return $msbuild
}

function Invoke-Step {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,

        [Parameter(Mandatory = $true)]
        [scriptblock]$Action
    )

    Write-Host ""
    Write-Host "========== $Name ==========" -ForegroundColor Cyan
    & $Action
    if ($LASTEXITCODE -ne 0) {
        throw "$Name failed with exit code $LASTEXITCODE"
    }
    Write-Host "[ok] $Name" -ForegroundColor Green
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Resolve-Path (Join-Path $scriptDir "..\..")
Push-Location $repoRoot
try {
    $msbuild = Resolve-MSBuildPath

    if (-not $SkipProtobufBuild) {
        Invoke-Step -Name "Build standalone Protobuf" -Action {
            & "pwsh" -NoProfile -File "tools/scripts/dev_tools.ps1" -Command "third-party-protobuf-build" -Configuration $Configuration
        }
    }

    if (-not $SkipClean) {
        Invoke-Step -Name "Clean game.sln ($Configuration|$Platform)" -Action {
            & $msbuild "game.sln" /t:Clean /m:1 /p:Configuration=$Configuration /p:Platform=$Platform /nologo /clp:ErrorsOnly
        }
    }

    Invoke-Step -Name "Build game.sln ($Configuration|$Platform)" -Action {
        & $msbuild "game.sln" /m:1 /p:Configuration=$Configuration /p:Platform=$Platform /nologo /clp:ErrorsOnly
    }

    if (-not $SkipGrpcClientVerify) {
        Invoke-Step -Name "Verify grpc_client build" -Action {
            & "pwsh" -NoProfile -File "tools/scripts/verify_grpc_client_build.ps1"
        }
    }

    Write-Host ""
    Write-Host "FULL_CHAIN_VERIFY_OK" -ForegroundColor Green
    exit 0
}
catch {
    Write-Host ""
    Write-Host "FULL_CHAIN_VERIFY_FAIL: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
finally {
    Pop-Location
}
