<#
.SYNOPSIS
    Foreground process entrypoint for mprocs local development dashboard.

.DESCRIPTION
    Runs one Go service or one C++ node in the foreground so mprocs can capture
    live logs and restart processes in a single terminal UI.

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/dev_mprocs_proc.ps1 -Command go-login

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/dev_mprocs_proc.ps1 -Command cpp-scene
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet(
        "go-db",
        "go-data-service",
        "go-player-locator",
        "go-login",
        "go-scene-manager",
        "java-gateway",
        "cpp-gate",
        "cpp-scene"
    )]
    [string]$Command
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
$GoRoot = Join-Path $RepoRoot "go"
$BinRoot = Join-Path $RepoRoot "bin"
$GoBinRoot = Join-Path $RepoRoot "bin\go_services"

function Invoke-GoService {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Dir,

        [Parameter(Mandatory = $true)]
        [string]$Entry,

        [Parameter(Mandatory = $true)]
        [string]$ExeName
    )

    $svcDir = Join-Path $GoRoot $Dir
    if (-not (Test-Path $svcDir)) {
        throw "Go service directory not found: $svcDir"
    }

    $preferredExe = Join-Path $GoBinRoot "$ExeName.exe"
    $fallbackExe = Join-Path $svcDir "$ExeName.exe"

    if (Test-Path $preferredExe) {
        Push-Location $svcDir
        try {
            & $preferredExe 2>&1
            $code = if ($null -eq $LASTEXITCODE) { 0 } else { $LASTEXITCODE }
            exit $code
        }
        finally {
            Pop-Location
        }
    }

    if (Test-Path $fallbackExe) {
        Push-Location $svcDir
        try {
            & $fallbackExe 2>&1
            $code = if ($null -eq $LASTEXITCODE) { 0 } else { $LASTEXITCODE }
            exit $code
        }
        finally {
            Pop-Location
        }
    }

    Push-Location $svcDir
    try {
        Write-Host "[warn] Go exe not found for $ExeName, fallback to go run $Entry" -ForegroundColor Yellow
        & go run $Entry 2>&1
        $code = if ($null -eq $LASTEXITCODE) { 0 } else { $LASTEXITCODE }
        exit $code
    }
    finally {
        Pop-Location
    }
}

function Invoke-CppNode {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Exe
    )

    $exePath = Join-Path $BinRoot $Exe
    if (-not (Test-Path $exePath)) {
        throw "C++ executable not found: $exePath. Build with msbuild game.sln first."
    }

    Push-Location $BinRoot
    try {
        & $exePath 2>&1
        $code = if ($null -eq $LASTEXITCODE) { 0 } else { $LASTEXITCODE }
        exit $code
    }
    finally {
        Pop-Location
    }
}

function Invoke-JavaService {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Dir
    )

    $JavaRoot = Join-Path $RepoRoot "java"
    $svcDir = Join-Path $JavaRoot $Dir
    if (-not (Test-Path $svcDir)) {
        throw "Java service directory not found: $svcDir"
    }

    $javaHome = "C:\Program Files\Eclipse Adoptium\jdk-23.0.2.7-hotspot"
    if (Test-Path $javaHome) {
        $env:JAVA_HOME = $javaHome
    }

    Push-Location $svcDir
    try {
        & .\mvnw.cmd spring-boot:run 2>&1
        $code = if ($null -eq $LASTEXITCODE) { 0 } else { $LASTEXITCODE }
        exit $code
    }
    finally {
        Pop-Location
    }
}

switch ($Command) {
    "go-db" { Invoke-GoService -Dir "db" -Entry "." -ExeName "db" }
    "go-data-service" { Invoke-GoService -Dir "data_service" -Entry "data_service.go" -ExeName "data_service" }
    "go-player-locator" { Invoke-GoService -Dir "player_locator" -Entry "player_locator.go" -ExeName "player_locator" }
    "go-login" { Invoke-GoService -Dir "login" -Entry "login.go" -ExeName "login" }
    "go-scene-manager" { Invoke-GoService -Dir "scene_manager" -Entry "scene_manager_service.go" -ExeName "scene_manager" }
    "java-gateway" { Invoke-JavaService -Dir "gateway_node" }
    "cpp-gate" { Invoke-CppNode -Exe "gate.exe" }
    "cpp-scene" { Invoke-CppNode -Exe "scene.exe" }
    default { throw "Unsupported command: $Command" }
}
