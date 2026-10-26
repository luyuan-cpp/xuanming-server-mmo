<#
.SYNOPSIS
    从独立固定源码串行构建 GoogleTest 1.17.0 的四个静态库。
.DESCRIPTION
    默认 x64 Debug /MDd，安装到 third_party/googletest/install_vs2026_dbg。
    Release 使用 /MD 和 install_vs2026_rel；x86 使用独立 _x86 后缀。
    不运行测试，不安装或升级工具，BuildRoot 可用于隔离安装。
#>
[CmdletBinding()]
param(
    [ValidateSet('Debug', 'Release')][string]$Configuration = 'Debug',
    [ValidateSet('x64', 'x86')][string]$Architecture = 'x64',
    [string]$BuildRoot = '',
    [ValidatePattern('^\d+\.\d+(?:\.\d+)?$')][string]$PreferredMsvcVersion = '14.51',
    [ValidateRange(1, 1)][int]$Jobs = 1,
    [switch]$Clean, [switch]$SkipToolCheck, [switch]$UseVSGenerator
)
. (Join-Path $PSScriptRoot 'sdk_build_common.ps1')
$savedEnvironment = Get-SdkEnvironment
try {
    $context = Initialize-SdkBuild -Name googletest -Configuration $Configuration -Architecture $Architecture `
        -BuildRoot $BuildRoot -PreferredMsvcVersion $PreferredMsvcVersion -UseVSGenerator:$UseVSGenerator -SkipToolCheck:$SkipToolCheck
    Assert-SdkSource $context.SourceRoot '52eb8108c5bdec04579160ae17225d66034bd723'
    Clear-SdkArtifacts $context -Clean:$Clean
    Invoke-SdkCMake $context $context.SourceRoot $context.BuildPath -Options @(
        '-Dgtest_force_shared_crt=ON', '-DBUILD_GMOCK=ON', '-DINSTALL_GTEST=ON', '-DGTEST_HAS_ABSL=OFF',
        '-Dgtest_build_tests=OFF', '-Dgmock_build_tests=OFF', '-DCMAKE_DEBUG_POSTFIX:STRING=')
    Assert-SdkArtifacts $context @('lib/gtest.lib', 'lib/gtest_main.lib', 'lib/gmock.lib', 'lib/gmock_main.lib',
        'include/gtest/gtest.h', 'include/gmock/gmock.h', 'lib/cmake/GTest/GTestConfig.cmake')
} finally { Restore-SdkEnvironment $savedEnvironment }
