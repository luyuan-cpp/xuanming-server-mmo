# go_test_race.ps1 -- Run `go test -race` inside a Linux container on Windows dev hosts.
# (Ported from xuanming-server/tools/scripts/go_test_race.ps1, adapted to mmo-cpp's go/<module> layout.)
#
# Why this script exists:
#   `-race` requires CGO, while Windows dev hosts typically run with CGO_ENABLED=0 and no gcc.
#   Mounting the repo and host GOMODCACHE into an official Linux golang container lets developers
#   run real race detector checks locally without waiting for CI or installing MinGW.
#
# Usage:
#   pwsh tools/scripts/go_test_race.ps1                                          # go/shared ./...
#   pwsh tools/scripts/go_test_race.ps1 -Module login -Pattern ./internal/kafka/...
#   pwsh tools/scripts/go_test_race.ps1 -Module all -Timeout 25m
[CmdletBinding()]
param(
    # Target subdirectory under go/ (e.g. "shared", "login", "scene_manager", "guild", "chat", "player_inbox"),
    # or "all" to iterate every go/<dir> containing a go.mod.
    [string]$Module = 'shared',
    # Package pattern passed to `go test -race` inside each module directory.
    [string]$Pattern = './...',
    # Per-test-binary timeout (-race runs 2-20x slower than normal).
    [string]$Timeout = '20m',
    # Container image tag aligned with go/shared/go.mod toolchain version.
    [string]$Image = 'golang:1.27.2'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$goRoot = Join-Path $repoRoot 'go'

$modCache = (& go env GOMODCACHE)
if (-not $modCache -or -not (Test-Path $modCache)) {
    throw "Cannot resolve GOMODCACHE ($modCache); ensure Go toolchain is installed on host first."
}

$targets = @()
if ($Module -eq 'all') {
    $targets = Get-ChildItem -Path $goRoot -Directory |
        Where-Object { Test-Path (Join-Path $_.FullName 'go.mod') } |
        Select-Object -ExpandProperty Name |
        Sort-Object
    if ($targets.Count -eq 0) {
        throw "No Go modules with go.mod found under $goRoot."
    }
} else {
    $modDir = Join-Path $goRoot $Module
    if (-not (Test-Path (Join-Path $modDir 'go.mod'))) {
        throw "Module '$Module' does not contain go.mod under $goRoot."
    }
    $targets = @($Module)
}

Write-Host "[race] image=$Image modules=$($targets -join ',') pattern=$Pattern timeout=$Timeout"
Write-Host "[race] repo=$repoRoot"
Write-Host "[race] modcache=$modCache"

$failed = @()
foreach ($mod in $targets) {
    Write-Host "[race] === go/$mod ($Pattern) ==="
    & docker run --rm `
        -v "${repoRoot}:/src" `
        -v "${modCache}:/go/pkg/mod" `
        -w "/src/go/$mod" `
        -e CGO_ENABLED=1 `
        $Image `
        sh -c "go test -race -count=1 -timeout $Timeout $Pattern"
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[race] FAILED in go/$mod (exit=$LASTEXITCODE)" -ForegroundColor Red
        $failed += $mod
    }
}

if ($failed.Count -gt 0) {
    Write-Host "[race] Failed modules: $($failed -join ', '). Preserve any 'WARNING: DATA RACE' stacks in the incident log." -ForegroundColor Red
    exit 1
}

Write-Host "[race] All target modules passed." -ForegroundColor Green
