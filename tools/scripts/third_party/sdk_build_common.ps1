# Windows 静态 SDK 的私有构建装配；只使用已安装工具，不下载或升级工具链。
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
# Git 的补丁探测允许预期非零，所有原生命令均在调用点检查退出码。
$PSNativeCommandUseErrorActionPreference = $false

function Get-SdkEnvironment {
    $saved = @{}
    foreach ($entry in [Environment]::GetEnvironmentVariables('Process').GetEnumerator()) {
        $saved[$entry.Key] = $entry.Value
    }
    return $saved
}

function Restore-SdkEnvironment([hashtable]$Saved) {
    foreach ($name in @([Environment]::GetEnvironmentVariables('Process').Keys)) {
        if (-not $Saved.ContainsKey($name)) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
    }
    foreach ($entry in $Saved.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, 'Process')
    }
}

function Assert-SdkSource([string]$Root, [string]$ExpectedSha, [switch]$AllowChanges) {
    $git = (Get-Command git -ErrorAction Stop).Source
    if (-not (Test-Path -LiteralPath (Join-Path $Root 'CMakeLists.txt') -PathType Leaf)) {
        throw "缺少已初始化的 SDK 源码：$Root"
    }
    $actualRoot = & $git -C $Root rev-parse --show-toplevel
    if ($LASTEXITCODE -ne 0 -or -not $actualRoot -or
        -not [IO.Path]::GetFullPath($actualRoot).Equals($Root, [StringComparison]::OrdinalIgnoreCase)) {
        throw "SDK 源码必须是独立的 Git 根目录：$Root"
    }
    $actualSha = & $git -C $Root rev-parse HEAD
    if ($LASTEXITCODE -ne 0 -or $actualSha -ne $ExpectedSha) {
        throw "SDK 固定提交不符：$Root；期望 $ExpectedSha，实际 $actualSha"
    }
    if (-not $AllowChanges) {
        $changes = & $git -C $Root status --porcelain --untracked-files=no
        if ($LASTEXITCODE -ne 0 -or $changes) { throw "SDK 已跟踪源码存在改动：$Root；$changes" }
    }
}

function Resolve-SdkArtifactPath([string]$RepoRoot, [string]$Path) {
    $full = [IO.Path]::TrimEndingDirectorySeparator([IO.Path]::GetFullPath(
        $(if ([IO.Path]::IsPathRooted($Path)) { $Path } else { Join-Path $RepoRoot $Path })))
    $prefix = $RepoRoot.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "SDK 产物必须位于当前仓库内，且不能等于仓库根目录：$full"
    }
    $ancestor = $full
    while (-not $ancestor.Equals($RepoRoot, [StringComparison]::OrdinalIgnoreCase)) {
        if ((Test-Path -LiteralPath $ancestor) -and
            (((Get-Item -LiteralPath $ancestor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
            throw "SDK 产物目录不能经过链接或联接：$ancestor"
        }
        $ancestor = [IO.Directory]::GetParent($ancestor).FullName
    }
    return $full
}

function Initialize-SdkBuild {
    param([string]$Name, [string]$Configuration, [string]$Architecture,
        [string]$BuildRoot, [string]$PreferredMsvcVersion, [switch]$UseVSGenerator,
        [switch]$SkipToolCheck)
    $repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../..')).Path
    $sourceRoot = Join-Path $repoRoot "third_party/$Name"
    $artifactRoot = if ($BuildRoot) { Resolve-SdkArtifactPath $repoRoot $BuildRoot } else { $sourceRoot }
    if ($BuildRoot) {
        & git -C $repoRoot check-ignore --quiet -- $artifactRoot
        if ($LASTEXITCODE -ne 0) { throw "自定义 SDK 产物根必须被仓库忽略：$artifactRoot" }
    }
    $archSuffix = if ($Architecture -eq 'x86') { '_x86' } else { '' }
    $configSuffix = if ($Configuration -eq 'Debug') { 'dbg' } else { 'rel' }
    $sdkSuffix = "vs2026${archSuffix}_$configSuffix"
    $installPath = Resolve-SdkArtifactPath $repoRoot (Join-Path $artifactRoot "install_$sdkSuffix")
    $buildPath = Resolve-SdkArtifactPath $repoRoot (Join-Path $artifactRoot ".build_$sdkSuffix")
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
    if (-not (Test-Path -LiteralPath $vswhere -PathType Leaf)) { throw "缺少已安装的 vswhere：$vswhere" }
    $vsPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
    if ($LASTEXITCODE -ne 0 -or -not $vsPath) { throw '缺少已安装的 Visual Studio C++ 工具链。' }
    $toolset = Get-ChildItem -LiteralPath (Join-Path $vsPath 'VC/Tools/MSVC') -Directory |
        Where-Object { $_.Name -eq $PreferredMsvcVersion -or $_.Name.StartsWith($PreferredMsvcVersion + '.') } |
        Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1
    if (-not $toolset) { throw "缺少已安装的 MSVC $PreferredMsvcVersion；不切换工具集。" }
    $vcvars = Join-Path $vsPath 'VC/Auxiliary/Build/vcvarsall.bat'
    $vcvarsArch = if ($Architecture -eq 'x86') { 'x64_x86' } else { 'x64' }
    $versionParts = $toolset.Name.Split('.')
    $vcvarsVersion = "$($versionParts[0]).$($versionParts[1])"
    $environmentLines = & cmd.exe /d /c "`"$vcvars`" $vcvarsArch -vcvars_ver=$vcvarsVersion >nul && set"
    if ($LASTEXITCODE -ne 0) { throw "加载 MSVC $vcvarsVersion/$Architecture 环境失败。" }
    foreach ($line in $environmentLines) {
        if ($line -match '^([^=]+)=(.*)$') { [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2], 'Process') }
    }
    if ($env:VCToolsVersion.TrimEnd('\', '/') -ne $toolset.Name -or $env:VSCMD_ARG_TGT_ARCH -ne $Architecture) {
        throw 'MSVC 实际工具集或目标架构与所选配置不符。'
    }
    $cmake = (Get-Command cmake -ErrorAction Stop).Source
    $null = Get-Command cl -ErrorAction Stop
    $generatorArgs = if ($UseVSGenerator) {
        @('-G', 'Visual Studio 18 2026', '-A', $(if ($Architecture -eq 'x86') { 'Win32' } else { 'x64' }),
            '-T', "v145,version=$($toolset.Name)")
    } else {
        $ninja = (Get-Command ninja -ErrorAction Stop).Source
        if (-not $SkipToolCheck) {
            $version = & $ninja --version
            if ($LASTEXITCODE -ne 0 -or $version -notmatch '^\d+\.\d+(?:\.\d+)?$' -or [version]$version -lt [version]'1.11') {
                throw '需要已安装的 Ninja 1.11 或以上版本。'
            }
        }
        @('-G', 'Ninja', '-DCMAKE_C_COMPILER=cl', '-DCMAKE_CXX_COMPILER=cl',
            "-DCMAKE_MAKE_PROGRAM=$($ninja.Replace('\', '/'))")
    }
    if (-not $SkipToolCheck) {
        $version = & $cmake --version
        if ($LASTEXITCODE -ne 0 -or @($version)[0] -notmatch '(\d+\.\d+\.\d+)' -or [version]$Matches[1] -lt [version]'3.24') {
            throw '需要已安装的 CMake 3.24 或以上版本。'
        }
    }
    $env:CMAKE_BUILD_PARALLEL_LEVEL = '1'
    $env:ImportDirectoryBuildProps = 'false'
    $env:ImportDirectoryBuildTargets = 'false'
    $runtime = if ($Configuration -eq 'Debug') { 'MultiThreadedDebugDLL' } else { 'MultiThreadedDLL' }
    return @{
        RepoRoot = $repoRoot; SourceRoot = $sourceRoot; BuildPath = $buildPath; InstallPath = $installPath
        CMake = $cmake; Configuration = $Configuration; Architecture = $Architecture; UseVSGenerator = [bool]$UseVSGenerator
        ConfigureArgs = $generatorArgs + @("-DCMAKE_BUILD_TYPE=$Configuration", '-DCMAKE_CXX_STANDARD=20',
            '-DCMAKE_CXX_STANDARD_REQUIRED=ON', '-DCMAKE_C_FLAGS=/utf-8', '-DCMAKE_CXX_FLAGS=/utf-8',
            "-DCMAKE_MSVC_RUNTIME_LIBRARY=$runtime", '-DBUILD_SHARED_LIBS=OFF',
            "-DCMAKE_INSTALL_PREFIX=$($installPath.Replace('\', '/'))", '-DCMAKE_INSTALL_LIBDIR=lib', '-DCMAKE_INSTALL_BINDIR=bin')
    }
}

function Clear-SdkArtifacts([hashtable]$Context, [switch]$Clean) {
    if (-not $Clean) { return }
    foreach ($path in @($Context.BuildPath, $Context.InstallPath)) {
        $checked = Resolve-SdkArtifactPath $Context.RepoRoot $path
        if (Test-Path -LiteralPath $checked) { Remove-Item -LiteralPath $checked -Recurse -Force }
    }
}

function Invoke-SdkCMake {
    param([hashtable]$Context, [string]$Source, [string]$Build,
        [string[]]$Options = @(), [string[]]$Targets = @())
    $cmake = $Context.CMake
    Write-Host "[SDK] $Source；$($Context.Configuration)/$($Context.Architecture)；$Build"
    & $cmake -S $Source -B $Build @($Context.ConfigureArgs) @Options
    if ($LASTEXITCODE -ne 0) { throw "SDK 配置失败，退出码 $LASTEXITCODE；保留现场：$Build" }
    $buildArgs = @('--build', $Build, '--config', $Context.Configuration, '--parallel', '1')
    if ($Targets.Count) { $buildArgs += @('--target') + $Targets }
    if ($Context.UseVSGenerator) { $buildArgs += @('--', '/m:1', '/p:BuildInParallel=false', '/p:CL_MPCount=1') }
    & $cmake @buildArgs
    if ($LASTEXITCODE -ne 0) { throw "SDK 构建失败，退出码 $LASTEXITCODE；保留现场：$Build" }
    & $cmake --install $Build --config $Context.Configuration --prefix $Context.InstallPath
    if ($LASTEXITCODE -ne 0) { throw "SDK 安装失败，退出码 $LASTEXITCODE；保留现场：$Build" }
}

function Assert-SdkArtifacts([hashtable]$Context, [string[]]$Files) {
    foreach ($relative in $Files) {
        $path = Join-Path $Context.InstallPath $relative
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "SDK 安装缺少预期产物：$path" }
    }
    Write-Host "[完成] $($Context.Configuration)/$($Context.Architecture) SDK 已安装：$($Context.InstallPath)"
}
