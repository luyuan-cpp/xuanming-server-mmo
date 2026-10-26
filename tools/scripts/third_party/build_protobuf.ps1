<#
.SYNOPSIS
    按固定提交先构建 Abseil，再构建 Protobuf 35.1、protoc 与 utf8 静态库。
.DESCRIPTION
    默认 x64 Debug /MDd；Release 使用 /MD。源码独立，不下载依赖、不运行测试。
    默认 SDK 安装到 third_party/protobuf/install_vs2026_dbg 或 install_vs2026_rel；
    x86 加 _x86 后缀。BuildRoot 可将构建和 SDK 安装隔离到仓库已忽略目录。
.PARAMETER ZlibInstallRoot
    已验证的同配置、同架构 zlib 安装目录；保留压缩支持，缺少时明确失败。
#>
[CmdletBinding()]
param(
    [ValidateSet('Debug', 'Release')][string]$Configuration = 'Debug',
    [ValidateSet('x64', 'x86')][string]$Architecture = 'x64',
    [string]$BuildRoot = '',
    [string]$ZlibInstallRoot = '',
    [ValidatePattern('^\d+\.\d+(?:\.\d+)?$')][string]$PreferredMsvcVersion = '14.51',
    [ValidateRange(1, 1)][int]$Jobs = 1,
    [switch]$Clean, [switch]$SkipToolCheck, [switch]$UseVSGenerator
)
. (Join-Path $PSScriptRoot 'sdk_build_common.ps1')
$savedEnvironment = Get-SdkEnvironment
try {
    $context = Initialize-SdkBuild -Name protobuf -Configuration $Configuration -Architecture $Architecture `
        -BuildRoot $BuildRoot -PreferredMsvcVersion $PreferredMsvcVersion -UseVSGenerator:$UseVSGenerator -SkipToolCheck:$SkipToolCheck
    $abseilRoot = Join-Path $context.RepoRoot 'third_party/abseil-cpp'
    Assert-SdkSource $context.SourceRoot '35cd01f9fe9afbeea38cc7b979a3b6bfcde82c03'
    Assert-SdkSource $abseilRoot '76bb24329e8bf5f39704eb10d21b9a80befa7c81'
    if (-not $ZlibInstallRoot) {
        $archSuffix = if ($Architecture -eq 'x86') { '_x86' } else { '' }
        $configSuffix = if ($Configuration -eq 'Debug') { 'dbg' } else { 'rel' }
        $ZlibInstallRoot = Join-Path $context.RepoRoot "third_party/zlib/install_vs2026${archSuffix}_$configSuffix"
    }
    $zlibRoot = (Resolve-Path -LiteralPath $ZlibInstallRoot).Path.Replace('\', '/')
    $zlibName = if ($Configuration -eq 'Debug') { 'zlibstaticd.lib' } else { 'zlibstatic.lib' }
    foreach ($relative in @('include/zlib.h', "lib/$zlibName")) {
        if (-not (Test-Path -LiteralPath (Join-Path $zlibRoot $relative) -PathType Leaf)) {
            throw "缺少同配置、同架构的 zlib；请通过 -ZlibInstallRoot 指定已验证安装：$zlibRoot/$relative"
        }
    }
    Clear-SdkArtifacts $context -Clean:$Clean
    Invoke-SdkCMake $context $abseilRoot (Join-Path $context.BuildPath 'abseil') -Options @(
        '-DABSL_ENABLE_INSTALL=ON', '-DABSL_BUILD_TESTING=OFF', '-DBUILD_TESTING=OFF',
        '-DABSL_BUILD_TEST_HELPERS=OFF', '-DABSL_MSVC_STATIC_RUNTIME=OFF', '-DABSL_PROPAGATE_CXX_STD=ON')
    $prefix = $context.InstallPath.Replace('\', '/')
    Invoke-SdkCMake $context $context.SourceRoot (Join-Path $context.BuildPath 'protobuf') -Options @(
        '-Dprotobuf_BUILD_TESTS=OFF', '-Dprotobuf_BUILD_CONFORMANCE=OFF', '-Dprotobuf_BUILD_EXAMPLES=OFF',
        '-Dprotobuf_BUILD_SHARED_LIBS=OFF', '-Dprotobuf_MSVC_STATIC_RUNTIME=OFF', '-Dprotobuf_BUILD_LIBUPB=OFF',
        '-Dprotobuf_BUILD_PROTOC_BINARIES=ON', '-Dprotobuf_LOCAL_DEPENDENCIES_ONLY=ON',
        '-Dprotobuf_FORCE_FETCH_DEPENDENCIES=OFF', "-Dabsl_DIR=$prefix/lib/cmake/absl", "-DCMAKE_PREFIX_PATH=$prefix",
        '-Dprotobuf_WITH_ZLIB=ON', '-DCMAKE_FIND_PACKAGE_PREFER_CONFIG=OFF', '-DZLIB_USE_STATIC_LIBS=ON',
        "-DZLIB_INCLUDE_DIR=$zlibRoot/include", "-DZLIB_LIBRARY=$zlibRoot/lib/$zlibName",
        "-DZLIB_LIBRARY_DEBUG=$zlibRoot/lib/$zlibName", "-DZLIB_LIBRARY_RELEASE=$zlibRoot/lib/$zlibName")
    $postfix = if ($Configuration -eq 'Debug') { 'd' } else { '' }
    Assert-SdkArtifacts $context @("lib/libprotobuf$postfix.lib", "lib/libprotoc$postfix.lib", 'bin/protoc.exe',
        'lib/absl_base.lib', 'lib/libutf8_range.lib', 'lib/libutf8_validity.lib',
        'include/google/protobuf/message.h', 'include/utf8_validity.h', 'lib/cmake/protobuf/protobuf-config.cmake')
} finally { Restore-SdkEnvironment $savedEnvironment }
