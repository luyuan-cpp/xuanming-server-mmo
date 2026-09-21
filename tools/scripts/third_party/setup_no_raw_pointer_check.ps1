#requires -Version 7
<#
.SYNOPSIS
下载 LLVM/Clang 开发库并编译仓库的裸指针成员检查器。
.DESCRIPTION
固定官方 Windows x64 开发包及 SHA256，缓存到 build/deps，不安装系统软件或修改全局 PATH。
重跑复用已验证的开发库，检查器仍执行增量构建。下载中断保留 .part，可继续下载。
#>
[CmdletBinding()]
param(
    [string]$LlvmRoot = '',
    [switch]$DownloadOnly,
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$version = '23.1.1'
$archiveName = "clang+llvm-$version-x86_64-pc-windows-msvc.tar.xz"
$archiveSha256 = 'c54ac8146b420fe72e11e6fdd56498d6818011ad23267196b6ab37b5ac9264c3'
$downloadUrl = "https://github.com/llvm/llvm-project/releases/download/llvmorg-$version/$archiveName"
$cacheRoot = Join-Path $repoRoot 'build\deps'
$archivePath = Join-Path $cacheRoot $archiveName
$buildRoot = Join-Path $repoRoot 'cpp\plugin\build'
$requiredFiles = @(
    'lib/cmake/llvm/LLVMConfig.cmake', 'lib/cmake/clang/ClangConfig.cmake',
    'include/clang/Tooling/Tooling.h', 'lib/clangTooling.lib'
)

function Test-LlvmSdk([string]$Root) {
    foreach ($relative in $requiredFiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $Root $relative) -PathType Leaf)) { return $false }
    }
    return $true
}

# 显式目录优先；自动发现已有开发库后不下载另一份。
if (-not $LlvmRoot) {
    $candidates = @($env:LLVM_ROOT, (Join-Path $env:ProgramFiles 'LLVM'), 'D:/game/llvm')
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-LlvmSdk $candidate)) { $LlvmRoot = $candidate; break }
    }
}
$sdkRoot = if ($LlvmRoot) { [IO.Path]::GetFullPath($LlvmRoot) } else { Join-Path $cacheRoot "llvm-$version" }

function Invoke-Checked([string]$Exe, [string[]]$Arguments) {
    & $Exe @Arguments | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { throw "命令失败(退出码 $LASTEXITCODE): $Exe $($Arguments -join ' ')" }
}

function Get-VerifiedArchive([string]$Url, [string]$Destination, [string]$Sha256) {
    $curl = (Get-Command curl.exe -ErrorAction Stop).Source
    if (Test-Path -LiteralPath $Destination) {
        if ((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ne $Sha256) {
            throw "缓存包 SHA256 不匹配，请移走后重下: $Destination"
        }
        return
    }
    $partial = "$Destination.part"
    Invoke-Checked $curl @('--fail', '--location', '--retry', '3', '--retry-all-errors', '--connect-timeout', '30',
        '--continue-at', '-', '--output', $partial, $Url)
    if ((Get-FileHash -LiteralPath $partial -Algorithm SHA256).Hash -ne $Sha256) {
        throw "下载包 SHA256 不匹配，请移走后重下: $partial"
    }
    Move-Item -LiteralPath $partial -Destination $Destination
}

function Initialize-LlvmSupport([string]$CMake) {
    # 官方 LLVM 归档导出的静态库还引用这三项，版本/校验值与其发行构建脚本一致。
    $supportRoot = Join-Path $cacheRoot "llvm-$version-support"
    $prefix = Join-Path $supportRoot 'install'
    $dependencies = @(
        @{ Name='zlib-1.3.2'; Url='https://github.com/madler/zlib/releases/download/v1.3.2/zlib-1.3.2.tar.gz';
           Sha='bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16'; Subdir='';
           Required=@('include/zlib.h','lib/zs.lib'); Options=@('-DZLIB_BUILD_TESTING=OFF') },
        @{ Name='zstd-1.5.7'; Url='https://github.com/facebook/zstd/releases/download/v1.5.7/zstd-1.5.7.tar.gz';
           Sha='eb33e51f49a15e023950cd7825ca74a4a2b43db8354825ac24fc1b7ee09e6fa3'; Subdir='build/cmake';
           Required=@('include/zstd.h','lib/zstd_static.lib'); Options=@('-DZSTD_BUILD_SHARED=OFF','-DZSTD_BUILD_PROGRAMS=OFF','-DZSTD_BUILD_TESTS=OFF') },
        @{ Name='libxml2-2.9.12'; Url='https://gitlab.gnome.org/GNOME/libxml2/-/archive/v2.9.12/libxml2-v2.9.12.tar.gz';
           Sha='98bfa7a9a5e2a75638422050740448ee9f02bf4dc2075c9822d7747d5ff9e617'; Subdir='';
           Required=@('include/libxml2/libxml/parser.h','lib/libxml2s.lib');
           Options=@('-DLIBXML2_WITH_PYTHON=OFF','-DLIBXML2_WITH_PROGRAMS=OFF','-DLIBXML2_WITH_TESTS=OFF',
               '-DLIBXML2_WITH_ICONV=OFF','-DLIBXML2_WITH_ICU=OFF','-DLIBXML2_WITH_LZMA=OFF','-DLIBXML2_WITH_ZLIB=OFF') }
    )
    New-Item -ItemType Directory -Path $supportRoot -Force | Out-Null
    foreach ($dep in $dependencies) {
        $stamp = Join-Path $supportRoot "$($dep.Name).sha256"
        $ready = (Test-Path -LiteralPath $stamp) -and ((Get-Content -LiteralPath $stamp -Raw).Trim() -eq $dep.Sha)
        foreach ($file in $dep.Required) { $ready = $ready -and (Test-Path -LiteralPath (Join-Path $prefix $file)) }
        if ($ready) { Write-Host "复用 LLVM 依赖: $($dep.Name)"; continue }
        $archive = Join-Path $cacheRoot "$($dep.Name).tar.gz"
        Get-VerifiedArchive $dep.Url $archive $dep.Sha
        $source = Join-Path $supportRoot "$($dep.Name)-src"
        if (-not (Test-Path -LiteralPath $source)) {
            $stage = Join-Path $supportRoot ("$($dep.Name).extract-" + [guid]::NewGuid().ToString('N'))
            New-Item -ItemType Directory -Path $stage | Out-Null
            Invoke-Checked (Get-Command tar.exe).Source @('-xf', $archive, '-C', $stage, '--strip-components=1')
            if ((Split-Path $stage -Parent) -ne $supportRoot -or (Split-Path $source -Parent) -ne $supportRoot) {
                throw '依赖解压目录超出受管理目录。'
            }
            Move-Item -LiteralPath $stage -Destination $source
        }
        $build = Join-Path $supportRoot "$($dep.Name)-build"
        $sourceDir = if ($dep.Subdir) { Join-Path $source $dep.Subdir } else { $source }
        Invoke-Checked $CMake (@('-S', $sourceDir, '-B', $build, '-A', 'x64',
            "-DCMAKE_INSTALL_PREFIX=$prefix", '-DBUILD_SHARED_LIBS=OFF', '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded',
            '-DCMAKE_POLICY_DEFAULT_CMP0091=NEW', '-DCMAKE_POLICY_VERSION_MINIMUM=3.5') + $dep.Options)
        Invoke-Checked $CMake @('--build', $build, '--config', 'Release', '--target', 'install',
            '--', '/m:1', '/nr:false', '/p:ImportDirectoryBuildTargets=false')
        foreach ($file in $dep.Required) {
            if (-not (Test-Path -LiteralPath (Join-Path $prefix $file))) { throw "依赖安装不完整: $($dep.Name)/$file" }
        }
        [IO.File]::WriteAllText($stamp, $dep.Sha)
    }
    return $prefix
}

function Resolve-CMake {
    $onPath = Get-Command cmake -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    if (Test-Path -LiteralPath $vswhere) {
        $vsRoot = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
        if ($LASTEXITCODE -eq 0 -and $vsRoot) {
            $candidate = Join-Path $vsRoot 'Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe'
            if (Test-Path -LiteralPath $candidate) { return $candidate }
        }
    }
    throw '未找到 CMake。请在 Visual Studio Installer 中添加 C++ CMake 工具。'
}

if ($DryRun) {
    Write-Host "官方开发包: $downloadUrl"
    Write-Host "SHA256: $archiveSha256"
    Write-Host "开发库目录: $sdkRoot"
    if (-not $DownloadOnly) { Write-Host "Release x64 串行构建: $buildRoot\Release\no_raw_ptr_check.exe" }
    return
}
if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
    throw '此入口仅支持 Windows x64。'
}
# 下载前先核实本机构建工具，避免下载完成后才发现不能编译。
$cmake = if (-not $DownloadOnly) { Resolve-CMake } else { $null }

if ($LlvmRoot) {
    if (-not (Test-LlvmSdk $sdkRoot)) { throw "指定目录不是完整 LLVM/Clang 开发库: $sdkRoot" }
    Write-Host "复用已有 LLVM/Clang 开发库: $sdkRoot"
} else {
    $stampPath = Join-Path $sdkRoot '.archive-sha256'
    $ready = (Test-LlvmSdk $sdkRoot) -and (Test-Path -LiteralPath $stampPath) -and
        ((Get-Content -LiteralPath $stampPath -Raw).Trim() -eq $archiveSha256)
    if (-not $ready) {
        # 不覆盖未知目录或半成品；解压失败保留独立 staging 供诊断。
        if (Test-Path -LiteralPath $sdkRoot) { throw "开发库目录不完整或版本标记不符，请检查后移走再运行: $sdkRoot" }
        $tar = (Get-Command tar.exe -ErrorAction Stop).Source
        New-Item -ItemType Directory -Path $cacheRoot -Force | Out-Null
        Write-Host "准备 LLVM $version 开发包(约 860 MiB)，存在则复用，缺少则下载。"
        Get-VerifiedArchive $downloadUrl $archivePath $archiveSha256
        $stage = Join-Path $cacheRoot ("llvm-$version.extract-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $stage | Out-Null
        Write-Host "校验通过，解压到 $stage"
        Invoke-Checked $tar @('-xf', $archivePath, '-C', $stage, '--strip-components=1')
        if (-not (Test-LlvmSdk $stage)) { throw "归档缺少 LLVM/Clang 开发库，解压内容保留在: $stage" }
        [IO.File]::WriteAllText((Join-Path $stage '.archive-sha256'), $archiveSha256)
        # stage 和目标均固定在仓库 build/deps 内，禁止覆盖已有目录。
        if ((Split-Path $stage -Parent) -ne $cacheRoot -or (Split-Path $sdkRoot -Parent) -ne $cacheRoot) {
            throw '解压目录超出 build/deps。'
        }
        Move-Item -LiteralPath $stage -Destination $sdkRoot
    } else { Write-Host "复用 LLVM $version 开发库: $sdkRoot" }
}
if ($DownloadOnly) { Write-Host "开发库已就绪: $sdkRoot"; return }

$supportPrefix = Initialize-LlvmSupport $cmake
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
$vsRoot = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vsRoot) { throw '未找到 Visual Studio C++ 安装。' }
Invoke-Checked $cmake @('-S', (Join-Path $repoRoot 'cpp\plugin'), '-B', $buildRoot,
    '-A', 'x64', "-DLLVM_ROOT=$sdkRoot", "-DLLVM_DIR=$sdkRoot/lib/cmake/llvm",
    "-DClang_DIR=$sdkRoot/lib/cmake/clang", "-DCMAKE_PREFIX_PATH=$supportPrefix",
    "-DZLIB_LIBRARY_RELEASE=$supportPrefix/lib/zs.lib", "-DZLIB_INCLUDE_DIR=$supportPrefix/include",
    "-DMSVC_DIA_SDK_DIR=$vsRoot/DIA SDK")
# 不继承 game.sln 的构建钩子，防止检查器在编译自己时调用旧版自身。
Invoke-Checked $cmake @('--build', $buildRoot, '--config', 'Release', '--target', 'no_raw_ptr_check',
    '--', '/m:1', '/nr:false', '/p:ImportDirectoryBuildTargets=false')
$checker = Join-Path $buildRoot 'Release\no_raw_ptr_check.exe'
if (-not (Test-Path -LiteralPath $checker)) { throw "构建结束但没有生成检查器: $checker" }
Invoke-Checked $checker @('--help')
& (Join-Path $PSScriptRoot '../tests/no_raw_pointer_check.tests.ps1') -CheckerPath $checker
Write-Host "检查器已生成: $checker"
Write-Host '现有 MSBuild 检查脚本会自动发现此文件。'
