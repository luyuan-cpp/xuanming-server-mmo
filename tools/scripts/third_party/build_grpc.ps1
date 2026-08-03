<#
.SYNOPSIS
    Build gRPC (Release + Debug) using Ninja or VS generator + MSVC cl.exe,
    then install to grpc/install_vs2026/ (Release) and grpc/install_vs2026_dbg/ (Debug).

.DESCRIPTION
    This script automates the full gRPC build for the MMORPG project.
    It locates grpc source at ./grpc (relative to this script), uses vswhere
    to locate the newest MSVC toolchain, then runs CMake with the Ninja
    generator (default) or Visual Studio generator (-UseVSGenerator).

    If cmake or ninja is missing or outdated, the script will auto-install /
    upgrade them via winget (requires Windows 10 1709+ or Windows 11).
    Visual Studio with C++ Desktop workload must be installed.

.PARAMETER BuildRelease
    Build the Release configuration (default: true).
.PARAMETER BuildDebug
    Build the Debug configuration (default: true).
.PARAMETER SkipToolCheck
    Skip the cmake/ninja version check and auto-install step.
.PARAMETER Jobs
    Parallel build jobs passed to the build system (-j). Default: number of logical CPUs.
.PARAMETER Clean
    设置后，在构建前同时删除已有 build 与 install 目录。
    新版 gRPC/Abseil 的安装清单不会主动删除上游已移除的库，因此 install
    目录也必须清理，避免旧库残留掩盖依赖错误。
.PARAMETER UseVSGenerator
    Use Visual Studio generator instead of Ninja. Useful when CMake + Ninja
    has compatibility issues (e.g. CMake 4.2.0 recompact bug).
.EXAMPLE
    # Build both Release and Debug (default, Ninja)
    .\build_grpc.ps1

    # Build using Visual Studio generator (workaround for CMake 4.2.0 + Ninja bug)
    .\build_grpc.ps1 -UseVSGenerator

    # Release only, 8 parallel jobs
    .\build_grpc.ps1 -BuildDebug:$false -Jobs 8

    # Clean rebuild
    .\build_grpc.ps1 -Clean

    # Skip auto-install of cmake/ninja
    .\build_grpc.ps1 -SkipToolCheck
#>
[CmdletBinding()]
param(
    [bool]$BuildRelease  = $true,
    [bool]$BuildDebug    = $true,
    [int]   $Jobs          = 0,
    [string]$PreferredMsvcVersion = '14.51',
    [switch]$Clean,
    [switch]$SkipToolCheck,
    [switch]$UseVSGenerator
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Resolve repository root from tools/scripts/third_party and locate grpc source.
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$GrpcRoot = Join-Path $RepoRoot 'third_party\grpc'
if (-not (Test-Path (Join-Path $GrpcRoot 'CMakeLists.txt'))) {
    Write-Error "Cannot find grpc CMakeLists.txt at $GrpcRoot."
}

$ExpectedGrpcSha = 'c876f4da50f7da2f331888b88b2a7243514139fe'
$ExpectedBoringSslSha = '2b44a3701a4788e1ef866ddc7f143060a3d196c9'
$BoringSslRoot = Join-Path $GrpcRoot 'third_party\boringssl-with-bazel'
$BoringSslPatch = Join-Path $RepoRoot 'tools\patches\grpc\v1.83.0-boringssl-windows-x509-name.patch'

function Get-GitHead([string]$RepositoryPath) {
    $git = Get-Command git -ErrorAction SilentlyContinue
    if (-not $git) {
        Write-Error "git is not available on PATH."
    }

    $output = & $git.Source -C $RepositoryPath rev-parse HEAD 2>&1
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Cannot resolve git HEAD for ${RepositoryPath}: $($output -join [Environment]::NewLine)"
    }

    return ($output | Select-Object -Last 1).ToString().Trim()
}

function Initialize-BoringSslWindowsPatch {
    if (-not (Test-Path -LiteralPath $BoringSslRoot -PathType Container)) {
        Write-Error "BoringSSL submodule is missing: $BoringSslRoot"
    }
    if (-not (Test-Path -LiteralPath $BoringSslPatch -PathType Leaf)) {
        Write-Error "Required BoringSSL patch is missing: $BoringSslPatch"
    }

    $actualGrpcSha = Get-GitHead $GrpcRoot
    if ($actualGrpcSha -ne $ExpectedGrpcSha) {
        Write-Error "Unexpected gRPC revision. Expected $ExpectedGrpcSha, got $actualGrpcSha."
    }

    $actualBoringSslSha = Get-GitHead $BoringSslRoot
    if ($actualBoringSslSha -ne $ExpectedBoringSslSha) {
        Write-Error "Unexpected BoringSSL revision. Expected $ExpectedBoringSslSha, got $actualBoringSslSha."
    }

    $git = (Get-Command git -ErrorAction Stop).Source
    $null = & $git -C $BoringSslRoot apply --check $BoringSslPatch 2>$null
    $forwardCheckExitCode = $LASTEXITCODE
    if ($forwardCheckExitCode -eq 0) {
        & $git -C $BoringSslRoot apply $BoringSslPatch
        if ($LASTEXITCODE -ne 0) {
            Write-Error "Failed to apply BoringSSL Windows X509_NAME patch: $BoringSslPatch"
        }
        Write-Host "[patch] Applied BoringSSL Windows X509_NAME compatibility patch."
        return
    }

    $null = & $git -C $BoringSslRoot apply --reverse --check $BoringSslPatch 2>$null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "[patch] BoringSSL Windows X509_NAME compatibility patch is already applied."
        return
    }

    Write-Error "BoringSSL patch is neither applicable nor already applied. Refusing to build: $BoringSslPatch"
}

Initialize-BoringSslWindowsPatch

function Ensure-WingetAvailable {
    $wg = Get-Command winget -ErrorAction SilentlyContinue
    if (-not $wg) {
        Write-Error "winget is not available. Install cmake and ninja manually, or install winget (App Installer) from the Microsoft Store."
    }
}

$ToolDefs = @{
    cmake = @{ WingetId = 'Kitware.CMake';      MinVersion = [version]'3.20.0' }
    ninja = @{ WingetId = 'Ninja-build.Ninja';  MinVersion = [version]'1.11.0' }
}

function Get-ToolVersion([string]$Name) {
    $cmd = Get-Command $Name -ErrorAction SilentlyContinue
    if (-not $cmd) { return $null }

    switch ($Name) {
        'cmake' {
            $out = & $cmd.Source --version 2>$null | Select-Object -First 1
            if ($out -match '(\d+\.\d+\.\d+)') { return [version]$Matches[1] }
        }
        'ninja' {
            $out = & $cmd.Source --version 2>$null | Select-Object -First 1
            if ($out -match '(\d+\.\d+\.\d+)') { return [version]$Matches[1] }
        }
    }

    return $null
}

function Ensure-Tool([string]$Name) {
    $def        = $ToolDefs[$Name]
    $currentVer = Get-ToolVersion $Name

    if ($currentVer -and $currentVer -ge $def.MinVersion) {
        Write-Host "[info] $Name $currentVer is OK (>= $($def.MinVersion))"
        return
    }

    Ensure-WingetAvailable

    if (-not $currentVer) {
        Write-Host "[install] $Name not found - installing via winget ..."
        winget install --id $def.WingetId --accept-source-agreements --accept-package-agreements --silent
    } else {
        Write-Host "[upgrade] $Name $currentVer is below minimum $($def.MinVersion) - upgrading via winget ..."
        winget upgrade --id $def.WingetId --accept-source-agreements --accept-package-agreements --silent
    }

    $wingetExit = $LASTEXITCODE

    # Refresh PATH so the new/existing binary is visible in this session.
    $machinePath = [System.Environment]::GetEnvironmentVariable('Path', 'Machine')
    $userPath    = [System.Environment]::GetEnvironmentVariable('Path', 'User')
    $env:Path    = "$machinePath;$userPath"

    $newVer = Get-ToolVersion $Name
    if ($newVer -and $newVer -ge $def.MinVersion) {
        Write-Host "[info] $Name $newVer is OK after winget (>= $($def.MinVersion))"
        return
    }

    if ($wingetExit -ne 0) {
        Write-Error "winget $Name install/upgrade failed (exit code $wingetExit) and $Name $newVer is still not adequate. Install manually."
    }

    if (-not $newVer) {
        Write-Error "$Name still not found on PATH after install. You may need to restart your terminal."
    }

    Write-Host "[info] $Name $newVer installed successfully"
}

if (-not $SkipToolCheck) {
    Ensure-Tool 'cmake'
    if (-not $UseVSGenerator) { Ensure-Tool 'ninja' }
}

function Resolve-Tool([string]$Name) {
    $cmd = Get-Command $Name -ErrorAction SilentlyContinue
    if (-not $cmd) { Write-Error "'$Name' not found on PATH. Please install it." }
    return $cmd.Source
}

function Resolve-OptionalTool([string]$Name) {
    $cmd = Get-Command $Name -ErrorAction SilentlyContinue
    if (-not $cmd) { return $null }
    return $cmd.Source
}

function Convert-ToCMakePath([string]$Path) {
    return $Path -replace '\\', '/'
}

function Remove-DirectoryRobust([string]$Path) {
    if (-not (Test-Path $Path)) {
        return
    }

    try {
        Remove-Item -Recurse -Force $Path -ErrorAction Stop
    } catch {
        $quotedPath = '"' + $Path + '"'
        $quotedChildren = '"' + (Join-Path $Path '*') + '"'
        cmd.exe /d /s /c "attrib -r -s -h /s /d $quotedChildren >nul 2>&1 & rd /s /q $quotedPath"
        if (Test-Path $Path) {
            Start-Sleep -Milliseconds 500
            cmd.exe /d /s /c "rd /s /q $quotedPath"
        }
        if (Test-Path $Path) {
            throw
        }
    }
}

$CMake = Resolve-Tool 'cmake'
if ($UseVSGenerator) {
    $Ninja = $null
    Write-Host "[info] Using Visual Studio generator (Ninja skipped)"
} else {
    $Ninja = Resolve-Tool 'ninja'
    Write-Host "[info] ninja : $Ninja"
}
$Nasm  = Resolve-OptionalTool 'nasm'
Write-Host "[info] cmake : $CMake"
if ($Nasm) {
    Write-Host "[info] nasm  : $Nasm"
} else {
    Write-Host "[warn] nasm not found on PATH. Falling back to OPENSSL_NO_ASM=ON for BoringSSL."
}

function Find-ClExe([string]$PreferredVersionPrefix) {
    $vswhereLocations = @(
        "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe",
        "$env:ProgramFiles\Microsoft Visual Studio\Installer\vswhere.exe"
    )

    $vswhere = $null
    foreach ($p in $vswhereLocations) {
        if (Test-Path $p) { $vswhere = $p; break }
    }
    if (-not $vswhere) { Write-Error "vswhere.exe not found. Install Visual Studio with C++ workload." }

    $vsPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
    if (-not $vsPath) { Write-Error "No VS installation with C++ VC tools found." }

    $msvcRoot = Join-Path $vsPath 'VC\Tools\MSVC'
    if (-not (Test-Path $msvcRoot)) { Write-Error "MSVC tools directory not found: $msvcRoot" }

    $allMsvcVersions = @(Get-ChildItem -Path $msvcRoot -Directory | Select-Object -ExpandProperty Name)
    if ($allMsvcVersions.Count -eq 0) { Write-Error "No MSVC toolset found under $msvcRoot" }

    $msvcVer = $null
    if (-not [string]::IsNullOrWhiteSpace($PreferredVersionPrefix)) {
        $msvcVer = $allMsvcVersions |
            Where-Object { $_ -like "$PreferredVersionPrefix*" } |
            Sort-Object { [version]$_ } -Descending |
            Select-Object -First 1
    }

    if (-not $msvcVer) {
        $msvcVer = $allMsvcVersions |
            Sort-Object { [version]$_ } -Descending |
            Select-Object -First 1
    }

    $clPath = Join-Path $vsPath "VC\Tools\MSVC\$msvcVer\bin\Hostx64\x64\cl.exe"
    if (-not (Test-Path $clPath)) { Write-Error "cl.exe not found at $clPath" }

    return @{
        VsPath   = $vsPath
        MsvcVer  = $msvcVer
        ClExe    = $clPath
        MsvcBase = Join-Path $vsPath "VC\Tools\MSVC\$msvcVer"
    }
}

$msvc = Find-ClExe -PreferredVersionPrefix $PreferredMsvcVersion
Write-Host "[info] VS    : $($msvc.VsPath)"
Write-Host "[info] MSVC  : $($msvc.MsvcVer)"
Write-Host "[info] cl.exe: $($msvc.ClExe)"

$GrpcExtraArgs = @()
if ($Nasm) {
    $nasmCMakePath = Convert-ToCMakePath $Nasm
    $GrpcExtraArgs += "-DCMAKE_ASM_NASM_COMPILER=$nasmCMakePath"
} elseif (-not $UseVSGenerator) {
    # Ninja generator does not auto-disable BoringSSL ASM when NASM is missing.
    $GrpcExtraArgs += "-DOPENSSL_NO_ASM=ON"
}

function Enter-MsvcEnv([hashtable]$Msvc) {
    $vcvars = Join-Path $Msvc.VsPath 'VC\Auxiliary\Build\vcvars64.bat'
    if (-not (Test-Path $vcvars)) { Write-Error "vcvars64.bat not found at $vcvars" }

    $versionParts = $Msvc.MsvcVer.Split('.')
    $vcvarsVer = if ($versionParts.Length -ge 2) { "$($versionParts[0]).$($versionParts[1])" } else { $Msvc.MsvcVer }
    $envBlock = cmd /c "`"$vcvars`" -vcvars_ver=$vcvarsVer >nul 2>&1 && set" 2>&1
    foreach ($line in $envBlock) {
        if ($line -match '^([^=]+)=(.*)$') {
            [System.Environment]::SetEnvironmentVariable($Matches[1], $Matches[2], 'Process')
        }
    }

    Write-Host "[info] MSVC environment loaded from vcvars64.bat (vcvars_ver=$vcvarsVer)"
}

Enter-MsvcEnv $msvc

# After vcvars64 loads, cl.exe is on PATH. Verify it is reachable.
$clOnPath = Get-Command cl.exe -ErrorAction SilentlyContinue
if (-not $clOnPath) {
    Write-Error "cl.exe not on PATH after loading vcvars64. MSVC environment setup may have failed."
}
Write-Host "[info] cl.exe on PATH: $($clOnPath.Source)"

if ($Jobs -le 0) {
    $Jobs = [System.Environment]::ProcessorCount
}
Write-Host "[info] Parallel jobs: $Jobs"

# Use short name 'cl' since vcvars64 has put cl.exe on PATH.
# This avoids CMake path-escaping issues with long paths containing spaces.
if ($UseVSGenerator) {
    $NinjaCMakePath = $null
    $CommonArgs = @(
        "-G", "Visual Studio 18 2026",
        "-A", "x64",
        "-DCMAKE_CXX_FLAGS=/std:c++20 /DGRPC_NO_FILTER_FUSION",
        "-DgRPC_BUILD_TESTS=OFF",
        "-DgRPC_BUILD_GRPCPP_OTEL_PLUGIN=OFF",
        "-Dprotobuf_BUILD_TESTS=OFF",
        "-DABSL_BUILD_TESTING=OFF",
        "-DABSL_BUILD_TEST_HELPERS=OFF",
        "-DABSL_PROPAGATE_CXX_STD=OFF"
    )
} else {
    $NinjaCMakePath = Convert-ToCMakePath $Ninja
    $CommonArgs = @(
        "-G", "Ninja",
        "-DCMAKE_C_COMPILER=cl",
        "-DCMAKE_CXX_COMPILER=cl",
        "-DCMAKE_MAKE_PROGRAM=$NinjaCMakePath",
        "-DCMAKE_CXX_FLAGS=/std:c++20 /DGRPC_NO_FILTER_FUSION",
        "-DgRPC_BUILD_TESTS=OFF",
        "-DgRPC_BUILD_GRPCPP_OTEL_PLUGIN=OFF",
        "-Dprotobuf_BUILD_TESTS=OFF",
        "-DABSL_BUILD_TESTING=OFF",
        "-DABSL_BUILD_TEST_HELPERS=OFF",
        "-DABSL_PROPAGATE_CXX_STD=OFF"
    )
}

function Invoke-GrpcBuild {
    param(
        [string]$BuildType,
        [string]$BuildDir,
        [string]$InstallDir
    )

    $buildPath   = Join-Path $GrpcRoot $BuildDir
    $installPath = Join-Path $GrpcRoot $InstallDir

    Write-Host "`n=============================================="
    Write-Host "  gRPC $BuildType build"
    Write-Host "  Build dir  : $buildPath"
    Write-Host "  Install dir: $installPath"
    Write-Host "==============================================`n"

    if ($Clean) {
        foreach ($pathToRemove in @($buildPath, $installPath)) {
            if (Test-Path $pathToRemove) {
                Write-Host "[clean] Removing $pathToRemove ..."
                Remove-DirectoryRobust $pathToRemove
            }
        }
    }

    $cachePath = Join-Path $buildPath 'CMakeCache.txt'
    $expectedCxxFlags = '/std:c++20 /DGRPC_NO_FILTER_FUSION'
    if (Test-Path $cachePath) {
        $cacheCompilerLine = Select-String -Path $cachePath -Pattern '^CMAKE_C_COMPILER:FILEPATH=' -SimpleMatch:$false | Select-Object -First 1
        $cacheCxxFlagsLine = Select-String -Path $cachePath -Pattern '^CMAKE_CXX_FLAGS:STRING=' -SimpleMatch:$false | Select-Object -First 1
        if ($cacheCompilerLine) {
            $cachedCompiler = $cacheCompilerLine.Line.Split('=', 2)[1]
            $cachedName = [System.IO.Path]::GetFileNameWithoutExtension($cachedCompiler)
            # We now pass 'cl' to CMake; cache stores a resolved full path.
            # Reset only if the cached compiler is a completely different toolchain.
            if ($cachedName -ne 'cl') {
                Write-Host "[clean] Cached compiler is not cl ($cachedCompiler) - resetting $buildPath ..."
                Remove-DirectoryRobust $buildPath
            } elseif (-not $cachedCompiler.StartsWith($msvc.MsvcBase, [System.StringComparison]::OrdinalIgnoreCase)) {
                Write-Host "[clean] Cached compiler is from a different MSVC toolset ($cachedCompiler) - resetting $buildPath ..."
                Remove-DirectoryRobust $buildPath
            }
        }

        if ((Test-Path $buildPath) -and $cacheCxxFlagsLine) {
            $cachedCxxFlags = $cacheCxxFlagsLine.Line.Split('=', 2)[1]
            if ($cachedCxxFlags -ne $expectedCxxFlags) {
                Write-Host "[clean] Cached CMAKE_CXX_FLAGS is '$cachedCxxFlags' instead of '$expectedCxxFlags' - resetting $buildPath ..."
                Remove-DirectoryRobust $buildPath
            }
        }
    }

    if (-not (Test-Path $buildPath)) { New-Item -ItemType Directory -Path $buildPath | Out-Null }

    Write-Host "[1/3] CMake configure ($BuildType) ..."
    $configArgs = $CommonArgs + @(
        "-DCMAKE_BUILD_TYPE=$BuildType",
        "-DCMAKE_INSTALL_PREFIX=$installPath"
    ) + $GrpcExtraArgs + @(
        $GrpcRoot
    )

    $configured = $false
    for ($attempt = 1; $attempt -le 2 -and -not $configured; $attempt++) {
        & $CMake @configArgs -B $buildPath
        if ($LASTEXITCODE -eq 0) {
            $configured = $true
            break
        }

        if ($attempt -eq 1) {
            Write-Host "[retry] CMake configure failed for $BuildType. Resetting $buildPath and retrying once ..."
            if (Test-Path $buildPath) {
                Remove-DirectoryRobust $buildPath
            }
            New-Item -ItemType Directory -Path $buildPath | Out-Null
        }
    }
    if (-not $configured) { Write-Error "CMake configure failed for $BuildType" }

    Write-Host "[2/3] Building ($BuildType) ..."
    & $CMake --build $buildPath --config $BuildType -j $Jobs
    if ($LASTEXITCODE -ne 0) { Write-Error "Build failed for $BuildType" }

    Write-Host "[3/3] Installing ($BuildType) -> $installPath ..."
    & $CMake --install $buildPath --prefix $installPath --config $BuildType
    if ($LASTEXITCODE -ne 0) { Write-Error "Install failed for $BuildType" }

    Write-Host "`n[done] $BuildType build installed to $installPath`n"
}

$sw = [System.Diagnostics.Stopwatch]::StartNew()

$buildDirSuffix = if ($UseVSGenerator) { '_vs' } else { '_ninja' }

if ($BuildRelease) {
    Invoke-GrpcBuild -BuildType 'Release' `
                     -BuildDir  ".build${buildDirSuffix}_release" `
                     -InstallDir 'install_vs2026'
}

if ($BuildDebug) {
    Invoke-GrpcBuild -BuildType 'Debug' `
                     -BuildDir  ".build${buildDirSuffix}_debug" `
                     -InstallDir 'install_vs2026_dbg'
}

$sw.Stop()
$elapsed = $sw.Elapsed
Write-Host "=============================================="
Write-Host "  All done!  Total time: $($elapsed.Hours)h $($elapsed.Minutes)m $($elapsed.Seconds)s"
Write-Host "=============================================="
