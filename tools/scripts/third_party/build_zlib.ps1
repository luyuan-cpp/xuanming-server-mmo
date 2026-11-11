<#
.SYNOPSIS
    用 MSVC / Ninja 串行构建 zlib 1.3.2 Debug（/MDd），执行上游测试并安装。
.PARAMETER BuildRoot
    全新的仓库内产物目录；相对路径以仓库根为基准。
    构建放在 build 子目录，测试通过后安装到 install 子目录。
.PARAMETER PreferredMsvcVersion
    优先选用的 MSVC 版本前缀，默认 14.51，缺少时选已安装的最新版本。
.PARAMETER Jobs
    固定为 1；上游测试中的嵌套构建也串行执行。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/third_party/build_zlib.ps1 -BuildRoot build/third_party/zlib-v1.3.2-dbg
#>
[CmdletBinding()]
param(
    [string]$BuildRoot = 'build/third_party/zlib-v1.3.2-dbg',
    [string]$PreferredMsvcVersion = '14.51',
    [ValidateRange(1, 1)]
    [int]$Jobs = 1
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../..')).Path
$ZlibRoot = Join-Path $RepoRoot 'third_party/zlib'
$OutputNamesHook = Join-Path $PSScriptRoot 'zlib_output_names.cmake'
$ExpectedZlibSha = 'da607da739fa6047df13e66a2af6b8bec7c2a498'

$git = (Get-Command git -ErrorAction Stop).Source
$actualSha = & $git -C $ZlibRoot rev-parse HEAD
if ($LASTEXITCODE -ne 0 -or $actualSha.Trim() -ne $ExpectedZlibSha) {
    throw "zlib 源码不是钉住的 1.3.2 提交，拒绝构建：$actualSha"
}
if (-not (Test-Path -LiteralPath $OutputNamesHook -PathType Leaf)) {
    throw "缺少 Windows 库名适配：$OutputNamesHook"
}

$ArtifactRoot = [IO.Path]::TrimEndingDirectorySeparator([IO.Path]::GetFullPath(
    $(if ([IO.Path]::IsPathRooted($BuildRoot)) { $BuildRoot } else { Join-Path $RepoRoot $BuildRoot })))
$workspacePrefix = $RepoRoot.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
if (-not $ArtifactRoot.StartsWith($workspacePrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "产物目录必须在当前仓库内，且不能等于仓库根目录：$ArtifactRoot"
}
$currentPath = $ArtifactRoot
while (-not $currentPath.Equals($RepoRoot, [StringComparison]::OrdinalIgnoreCase)) {
    if (Test-Path -LiteralPath $currentPath) {
        $item = Get-Item -LiteralPath $currentPath -Force -ErrorAction Stop
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "产物目录不能经过链接或目录联接：$currentPath"
        }
    }
    $currentPath = [IO.Directory]::GetParent($currentPath).FullName
}
if (Test-Path -LiteralPath $ArtifactRoot) {
    throw "产物目录已存在；为保留旧结果，请指定全新的 BuildRoot：$ArtifactRoot"
}
$BuildPath = Join-Path $ArtifactRoot 'build'
$InstallPath = Join-Path $ArtifactRoot 'install'

$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
if (-not (Test-Path -LiteralPath $vswhere -PathType Leaf)) { throw "缺少 vswhere：$vswhere" }
$vsPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if ($LASTEXITCODE -ne 0 -or -not $vsPath) { throw '没有可用的 Visual Studio C++ 工具链。' }
$toolsets = @(Get-ChildItem -LiteralPath (Join-Path $vsPath 'VC/Tools/MSVC') -Directory |
    Sort-Object { [version]$_.Name } -Descending)
$selectedToolset = $toolsets | Where-Object { $_.Name -like "$PreferredMsvcVersion*" } | Select-Object -First 1
if (-not $selectedToolset) { $selectedToolset = $toolsets | Select-Object -First 1 }
if (-not $selectedToolset) { throw '没有已安装的 MSVC 工具集。' }
$versionParts = $selectedToolset.Name.Split('.')
$vcvarsVersion = "$($versionParts[0]).$($versionParts[1])"
$vcvars = Join-Path $vsPath 'VC/Auxiliary/Build/vcvars64.bat'
$envBlock = & cmd.exe /d /c "`"$vcvars`" -vcvars_ver=$vcvarsVersion >nul && set"
if ($LASTEXITCODE -ne 0) { throw "加载 MSVC 环境失败：$vcvars" }
foreach ($line in $envBlock) {
    if ($line -match '^([^=]+)=(.*)$') {
        [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2], 'Process')
    }
}

$CMake = (Get-Command cmake -ErrorAction Stop).Source
$Ninja = (Get-Command ninja -ErrorAction Stop).Source
$Ctest = Join-Path (Split-Path -Parent $CMake) 'ctest.exe'
if (-not (Test-Path -LiteralPath $Ctest -PathType Leaf)) { throw "缺少与 CMake 配套的 ctest：$Ctest" }
$cmakeVersionText = & $CMake --version
if ($LASTEXITCODE -ne 0 -or @($cmakeVersionText)[0] -notmatch '(\d+\.\d+\.\d+)' -or [version]$Matches[1] -lt [version]'3.24') {
    throw '需要已安装的 CMake 3.24 或以上版本；本脚本不会安装或升级工具。'
}

$savedBuildParallelLevel = $env:CMAKE_BUILD_PARALLEL_LEVEL
$savedTestParallelLevel = $env:CTEST_PARALLEL_LEVEL
$env:CMAKE_BUILD_PARALLEL_LEVEL = '1'
$env:CTEST_PARALLEL_LEVEL = '1'
try {
    $configureArgs = @(
        '-S', $ZlibRoot, '-B', $BuildPath, '-G', 'Ninja',
        '-DCMAKE_C_COMPILER=cl',
        "-DCMAKE_MAKE_PROGRAM=$($Ninja.Replace('\', '/'))",
        '-DCMAKE_BUILD_TYPE=Debug',
        '-DCMAKE_POLICY_DEFAULT_CMP0091=NEW',
        '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreadedDebugDLL',
        "-DCMAKE_PROJECT_INCLUDE=$($OutputNamesHook.Replace('\', '/'))",
        "-DCMAKE_INSTALL_PREFIX=$($InstallPath.Replace('\', '/'))",
        '-DCMAKE_INSTALL_LIBDIR=lib', '-DCMAKE_INSTALL_BINDIR=bin',
        '-DZLIB_BUILD_SHARED=ON', '-DZLIB_BUILD_STATIC=ON',
        '-DZLIB_BUILD_TESTING=ON', '-DZLIB_INSTALL=ON'
    )
    & $CMake @configureArgs
    if ($LASTEXITCODE -ne 0) { throw "zlib 配置失败；保留现场：$BuildPath" }
    & $CMake --build $BuildPath --config Debug --parallel $Jobs
    if ($LASTEXITCODE -ne 0) { throw "zlib 构建失败；保留现场：$BuildPath" }
    & $Ctest --test-dir $BuildPath -C Debug --parallel $Jobs --output-on-failure --no-tests=error --timeout 180
    if ($LASTEXITCODE -ne 0) { throw "zlib 上游测试失败；保留现场：$BuildPath" }
    & $CMake --install $BuildPath --config Debug --prefix $InstallPath
    if ($LASTEXITCODE -ne 0) { throw "zlib 安装失败；保留现场：$InstallPath" }
} finally {
    $env:CMAKE_BUILD_PARALLEL_LEVEL = $savedBuildParallelLevel
    $env:CTEST_PARALLEL_LEVEL = $savedTestParallelLevel
}

foreach ($relativePath in @('lib/zlibd.lib', 'lib/zlibstaticd.lib', 'bin/zlibd.dll', 'include/zlib.h', 'include/zconf.h')) {
    $requiredPath = Join-Path $InstallPath $relativePath
    if (-not (Test-Path -LiteralPath $requiredPath -PathType Leaf)) {
        throw "zlib 安装缺少预期产物：$requiredPath"
    }
}
if (-not (Select-String -LiteralPath (Join-Path $InstallPath 'include/zlib.h') -Pattern '^#define ZLIB_VERSION "1\.3\.2"' -Quiet)) {
    throw "zlib 安装头文件版本不符：$InstallPath"
}
Write-Host "[done] zlib 1.3.2 Debug (/MDd) build and upstream tests finished; install dir: $InstallPath"
