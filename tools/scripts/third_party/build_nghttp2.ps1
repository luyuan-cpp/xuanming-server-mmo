<#
.SYNOPSIS
    用上游 CMake 编 nghttp2 静态核心库(Debug、/MDd),安装到 third_party/nghttp2/install_vs2026_dbg/。

.DESCRIPTION
    muduo_grpc(cpp/libs/engine/muduo_grpc)与它的单测只需要 nghttp2 的 HTTP/2 编解码核心库,
    见 docs/design/grpc-muduo-transport.md §2 / §9。本脚本:

      1. 核对 third_party/nghttp2 的 HEAD 等于钉住的 v1.70.0 提交,不对就拒绝构建;
      2. 用 vswhere 找最新的 MSVC,加载 vcvars64;
      3. CMake 配置(默认 Ninja;-UseVSGenerator 或找不到 ninja 时用 VS 生成器),
         只编静态核心库:不编 nghttp / nghttpx / h2load / 示例 / 测试 / 文档,不碰嵌套子模块;
      4. 构建并安装到 install_vs2026_dbg,最后校验 lib/nghttp2.lib 与 include/nghttp2/*.h。

    产物命名:只编静态库(BUILD_SHARED_LIBS=OFF)时 lib/CMakeLists.txt 不加 STATIC_LIB_SUFFIX,
    ARCHIVE_OUTPUT_NAME 就是 nghttp2,Debug 也没有 POSTFIX,所以是 nghttp2.lib。
    使用 nghttp2.h 的翻译单元必须定义 NGHTTP2_STATICLIB(muduo_grpc.vcxproj 已加)。

    构建目录 build/third_party/nghttp2-vs2026-dbg 在仓库根的 build/ 下(已被 .gitignore);
    安装目录在子模块内,.gitmodules 对该子模块配了 ignore = dirty。

.PARAMETER Jobs
    并行编译数,传给 cmake --build -j。默认取逻辑 CPU 数。
.PARAMETER PreferredMsvcVersion
    优先选用的 MSVC 工具集版本前缀;找不到就用最新的。
.PARAMETER Clean
    构建前删除构建目录与安装目录,从零开始。
.PARAMETER UseVSGenerator
    改用 Visual Studio 生成器。仓库根的 Directory.Build.targets 会给所有 .vcxproj 注入
    stdcpplatest、额外包含目录与禁用告警,所以 VS 生成器路径下本脚本在进程内关闭
    Directory.Build.props / Directory.Build.targets 的导入;能用 Ninja 时优先 Ninja。
.EXAMPLE
    pwsh -NoProfile -ExecutionPolicy Bypass -File tools/scripts/third_party/build_nghttp2.ps1
.EXAMPLE
    pwsh -NoProfile -ExecutionPolicy Bypass -File tools/scripts/third_party/build_nghttp2.ps1 -Clean
#>
[CmdletBinding()]
param(
    [int]$Jobs = 0,
    [string]$PreferredMsvcVersion = '14.51',
    [switch]$Clean,
    [switch]$UseVSGenerator
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# 钉住的源码版本:v1.70.0(annotated tag 对象 b69da4c2656bb66311c2a84bb2f4a3a1da2da393 指向的提交)。
# 升级 nghttp2 时这里、tools/archived/setup_dependencies.sh 与设计文档 §2 一起改。
$ExpectedNghttp2Sha     = '85e300c79fb6dbcfa9c1013215c8710c1c2cd3d2'
$ExpectedNghttp2Version = '1.70.0'

$RepoRoot     = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$Nghttp2Root  = Join-Path $RepoRoot 'third_party\nghttp2'
$BuildPath    = Join-Path $RepoRoot 'build\third_party\nghttp2-vs2026-dbg'
$InstallPath  = Join-Path $Nghttp2Root 'install_vs2026_dbg'
$BuildType    = 'Debug'

if (-not (Test-Path -LiteralPath (Join-Path $Nghttp2Root 'CMakeLists.txt') -PathType Leaf)) {
    Write-Error "Cannot find nghttp2 CMakeLists.txt at $Nghttp2Root. Run: git submodule update --init third_party/nghttp2"
}

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

# 先核对源码版本再做任何事:版本不对时连构建目录都不碰。
$actualSha = Get-GitHead $Nghttp2Root
if ($actualSha -ne $ExpectedNghttp2Sha) {
    Write-Error "Unexpected nghttp2 revision. Expected $ExpectedNghttp2Sha (v$ExpectedNghttp2Version), got $actualSha."
}
Write-Host "[info] nghttp2 revision OK: $actualSha (v$ExpectedNghttp2Version)"

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

function Find-Msvc([string]$PreferredVersionPrefix) {
    $vswhereLocations = @(
        "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe",
        "$env:ProgramFiles\Microsoft Visual Studio\Installer\vswhere.exe"
    )

    $vswhere = $null
    foreach ($p in $vswhereLocations) {
        if (Test-Path $p) { $vswhere = $p; break }
    }
    if (-not $vswhere) { Write-Error "vswhere.exe not found. Install Visual Studio with the C++ workload." }

    $vsPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
    if (-not $vsPath) { Write-Error "No Visual Studio installation with C++ VC tools found." }

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

function Get-ToolVersion([string]$Source) {
    $out = & $Source --version 2>$null | Select-Object -First 1
    if ($out -match '(\d+\.\d+(\.\d+)?)') { return [version]$Matches[1] }
    return $null
}

# 返回 CMakeCache.txt 里某个缓存项的值;没有缓存或没有该项时返回 $null。
function Get-CMakeCacheValue([string]$CachePath, [string]$Name) {
    if (-not (Test-Path -LiteralPath $CachePath -PathType Leaf)) { return $null }
    $line = Select-String -LiteralPath $CachePath -Pattern ('^' + [regex]::Escape($Name) + ':[A-Z]+=') |
        Select-Object -First 1
    if (-not $line) { return $null }
    return $line.Line.Split('=', 2)[1]
}

$msvc = Find-Msvc -PreferredVersionPrefix $PreferredMsvcVersion
Write-Host "[info] VS    : $($msvc.VsPath)"
Write-Host "[info] MSVC  : $($msvc.MsvcVer)"
Write-Host "[info] cl.exe: $($msvc.ClExe)"

# vcvars64 会把 VS 自带的 CMake / Ninja 也放进 PATH,所以工具在加载环境之后再解析。
Enter-MsvcEnv $msvc

$clOnPath = Get-Command cl.exe -ErrorAction SilentlyContinue
if (-not $clOnPath) {
    Write-Error "cl.exe not on PATH after loading vcvars64. MSVC environment setup may have failed."
}
Write-Host "[info] cl.exe on PATH: $($clOnPath.Source)"

$cmakeCmd = Get-Command cmake -ErrorAction SilentlyContinue
if (-not $cmakeCmd) { Write-Error "cmake not found on PATH. Install CMake 3.15+ or the VS 'C++ CMake tools' component." }
$CMake = $cmakeCmd.Source
$cmakeVersion = Get-ToolVersion $CMake
# CMAKE_MSVC_RUNTIME_LIBRARY(决定 /MDd)从 CMake 3.15 起才有。
if (-not $cmakeVersion -or $cmakeVersion -lt [version]'3.15') {
    Write-Error "cmake $cmakeVersion is too old; 3.15+ is required for CMAKE_MSVC_RUNTIME_LIBRARY."
}
Write-Host "[info] cmake : $CMake ($cmakeVersion)"

$Ninja = $null
if (-not $UseVSGenerator) {
    $ninjaCmd = Get-Command ninja -ErrorAction SilentlyContinue
    if ($ninjaCmd) {
        $Ninja = $ninjaCmd.Source
        Write-Host "[info] ninja : $Ninja ($(Get-ToolVersion $Ninja))"
    } else {
        Write-Host "[warn] ninja not found on PATH; falling back to the Visual Studio generator."
        $UseVSGenerator = $true
    }
}

if ($Jobs -le 0) {
    $Jobs = [System.Environment]::ProcessorCount
}
Write-Host "[info] Parallel jobs: $Jobs"

if ($UseVSGenerator) {
    $GeneratorName = 'Visual Studio 18 2026'
    $GeneratorArgs = @('-G', $GeneratorName, '-A', 'x64')
} else {
    $GeneratorName = 'Ninja'
    # vcvars64 已把 cl 放进 PATH,用短名避免 CMake 对带空格长路径的转义问题。
    $GeneratorArgs = @(
        '-G', $GeneratorName,
        '-DCMAKE_C_COMPILER=cl',
        "-DCMAKE_MAKE_PROGRAM=$(Convert-ToCMakePath $Ninja)"
    )
}

Write-Host ""
Write-Host "=============================================="
Write-Host "  nghttp2 $BuildType build (static core library only)"
Write-Host "  Generator  : $GeneratorName"
Write-Host "  Build dir  : $BuildPath"
Write-Host "  Install dir: $InstallPath"
Write-Host "=============================================="
Write-Host ""

if ($Clean) {
    foreach ($pathToRemove in @($BuildPath, $InstallPath)) {
        if (Test-Path $pathToRemove) {
            Write-Host "[clean] Removing $pathToRemove ..."
            Remove-DirectoryRobust $pathToRemove
        }
    }
}

# 构建目录复用前先核对缓存:换了生成器 CMake 会直接拒绝配置;换了 MSVC 工具集则会静默混用两套编译器的产物。
$cachePath = Join-Path $BuildPath 'CMakeCache.txt'
$cachedGenerator = Get-CMakeCacheValue $cachePath 'CMAKE_GENERATOR'
if ($cachedGenerator -and $cachedGenerator -ne $GeneratorName) {
    Write-Host "[clean] Cached generator is '$cachedGenerator' instead of '$GeneratorName' - resetting $BuildPath ..."
    Remove-DirectoryRobust $BuildPath
}
# 只有 Ninja 路径的编译器来自本脚本加载的 vcvars;VS 生成器自己选工具集,不在这里比。
# CMake 缓存里的路径是正斜杠,两边统一成正斜杠再比前缀。
if (-not $UseVSGenerator) {
    $cachedCompiler = Get-CMakeCacheValue $cachePath 'CMAKE_C_COMPILER'
    $expectedCompilerPrefix = Convert-ToCMakePath $msvc.MsvcBase
    if ($cachedCompiler -and
        -not (Convert-ToCMakePath $cachedCompiler).StartsWith($expectedCompilerPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        Write-Host "[clean] Cached C compiler is from a different toolset ($cachedCompiler) - resetting $BuildPath ..."
        Remove-DirectoryRobust $BuildPath
    }
}

if (-not (Test-Path $BuildPath)) { New-Item -ItemType Directory -Path $BuildPath -Force | Out-Null }

$ConfigureArgs = $GeneratorArgs + @(
    '-DENABLE_LIB_ONLY=ON',
    '-DBUILD_SHARED_LIBS=OFF',
    '-DBUILD_STATIC_LIBS=ON',
    '-DENABLE_DOC=OFF',
    '-DBUILD_TESTING=OFF',
    '-DENABLE_FAILMALLOC=OFF',
    '-DENABLE_STATIC_CRT=OFF',
    "-DCMAKE_BUILD_TYPE=$BuildType",
    # nghttp2 的 cmake_minimum_required 是 3.14,CMP0091 默认 OLD,不打开的话 CMAKE_MSVC_RUNTIME_LIBRARY 被忽略。
    '-DCMAKE_POLICY_DEFAULT_CMP0091=NEW',
    '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreadedDebugDLL',
    "-DCMAKE_INSTALL_PREFIX=$(Convert-ToCMakePath $InstallPath)",
    '-S', $Nghttp2Root,
    '-B', $BuildPath
)

# VS 生成器下 MSBuild 会从构建目录向上找到仓库根的 Directory.Build.targets(构建目录在仓库内),
# 把 stdcpplatest、额外包含目录、4996 禁用等注入 nghttp2 的工程。只在本进程内关闭这两个导入,结束后还原。
$savedImportProps   = $env:ImportDirectoryBuildProps
$savedImportTargets = $env:ImportDirectoryBuildTargets
if ($UseVSGenerator) {
    $env:ImportDirectoryBuildProps   = 'false'
    $env:ImportDirectoryBuildTargets = 'false'
}

$sw = [System.Diagnostics.Stopwatch]::StartNew()
try {
    Write-Host "[1/3] CMake configure ($BuildType) ..."
    & $CMake @ConfigureArgs
    if ($LASTEXITCODE -ne 0) { Write-Error "CMake configure failed for nghttp2 ($BuildType)" }

    Write-Host "[2/3] Building ($BuildType) ..."
    & $CMake --build $BuildPath --config $BuildType -j $Jobs
    if ($LASTEXITCODE -ne 0) { Write-Error "Build failed for nghttp2 ($BuildType)" }

    # 安装目录只装本次构建的产物:先清空再装,避免旧版本残留的头文件或库掩盖问题。
    Write-Host "[3/3] Installing ($BuildType) -> $InstallPath ..."
    if (Test-Path $InstallPath) {
        Remove-DirectoryRobust $InstallPath
    }
    & $CMake --install $BuildPath --config $BuildType --prefix $InstallPath
    if ($LASTEXITCODE -ne 0) { Write-Error "Install failed for nghttp2 ($BuildType)" }
} finally {
    $env:ImportDirectoryBuildProps   = $savedImportProps
    $env:ImportDirectoryBuildTargets = $savedImportTargets
}

# 校验 muduo_grpc.vcxproj / gate.vcxproj 实际引用的产物都在,且头文件确实是钉住的版本。
$requiredArtifacts = @(
    (Join-Path $InstallPath 'lib\nghttp2.lib'),
    (Join-Path $InstallPath 'include\nghttp2\nghttp2.h'),
    (Join-Path $InstallPath 'include\nghttp2\nghttp2ver.h')
)
$missing = @($requiredArtifacts | Where-Object { -not (Test-Path -LiteralPath $_ -PathType Leaf) })
if ($missing.Count -gt 0) {
    Write-Error "nghttp2 install is incomplete. Missing: $($missing -join ', ')"
}

$versionHeader = Join-Path $InstallPath 'include\nghttp2\nghttp2ver.h'
$versionPattern = '#define NGHTTP2_VERSION "' + $ExpectedNghttp2Version + '"'
if (-not (Select-String -LiteralPath $versionHeader -SimpleMatch -Pattern $versionPattern -Quiet)) {
    Write-Error "Installed nghttp2ver.h does not declare version $ExpectedNghttp2Version ($versionHeader)."
}

$sw.Stop()
$elapsed = $sw.Elapsed
Write-Host ""
Write-Host "=============================================="
Write-Host "  nghttp2 v$ExpectedNghttp2Version installed to $InstallPath"
foreach ($artifact in $requiredArtifacts) {
    Write-Host "    $artifact"
}
Write-Host "  Total time: $($elapsed.Hours)h $($elapsed.Minutes)m $($elapsed.Seconds)s"
Write-Host "=============================================="
