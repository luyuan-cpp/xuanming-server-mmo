<#
.SYNOPSIS
    按固定提交和已登记 Windows 补丁，串行构建独立 BoringSSL 静态 SDK。
.DESCRIPTION
    默认 x64 Debug /MDd；Release 使用 /MD。缺少 NASM 时使用上游无汇编配置。
    仅构建 ssl、crypto 和安装所需的 bssl，不运行测试，不安装或升级工具。
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
    $context = Initialize-SdkBuild -Name boringssl -Configuration $Configuration -Architecture $Architecture `
        -BuildRoot $BuildRoot -PreferredMsvcVersion $PreferredMsvcVersion -UseVSGenerator:$UseVSGenerator -SkipToolCheck:$SkipToolCheck
    Assert-SdkSource $context.SourceRoot '2b44a3701a4788e1ef866ddc7f143060a3d196c9' -AllowChanges
    $git = (Get-Command git -ErrorAction Stop).Source
    $patch = Join-Path $context.RepoRoot 'tools/patches/boringssl/windows-x509-name.patch'
    if (-not (Test-Path -LiteralPath $patch -PathType Leaf)) { throw "缺少登记的 BoringSSL Windows 补丁：$patch" }
    $changes = @(& $git -C $context.SourceRoot diff --name-only HEAD)
    if ($LASTEXITCODE -ne 0 -or @($changes | Where-Object { $_ -ne 'include/openssl/base.h' }).Count) {
        throw "BoringSSL 有登记补丁之外的源码修改：$changes"
    }
    $null = & $git -C $context.SourceRoot apply --reverse --check $patch 2>$null
    if ($LASTEXITCODE -ne 0) {
        if ($changes.Count) { throw 'BoringSSL 已修改的 base.h 不匹配登记补丁，拒绝覆盖。' }
        & $git -C $context.SourceRoot apply --check $patch
        if ($LASTEXITCODE -ne 0) { throw 'BoringSSL Windows 补丁不能应用。' }
        & $git -C $context.SourceRoot apply $patch
        if ($LASTEXITCODE -ne 0) { throw '应用 BoringSSL Windows 补丁失败。' }
    }
    # 反向可应用不代表没有夹带其他改动；忽略 index 摘要长度后逐行核对完整补丁。
    $actualDiff = @(& $git -C $context.SourceRoot diff --no-ext-diff --binary HEAD -- include/openssl/base.h)
    if ($LASTEXITCODE -ne 0) { throw '读取 BoringSSL 补丁差异失败。' }
    $actualLines = @($actualDiff | Where-Object { $_ -notmatch '^index ' })
    $expectedLines = @(Get-Content -LiteralPath $patch | Where-Object { $_ -notmatch '^index ' })
    if (($actualLines -join "`n") -cne ($expectedLines -join "`n")) {
        throw 'BoringSSL 源码差异不等于登记的 Windows 补丁，拒绝混入产物。'
    }
    Clear-SdkArtifacts $context -Clean:$Clean
    $nasm = Get-Command nasm -ErrorAction SilentlyContinue
    $options = if ($nasm) { @("-DCMAKE_ASM_NASM_COMPILER=$($nasm.Source.Replace('\', '/'))") } else { @('-DOPENSSL_NO_ASM=ON') }
    Invoke-SdkCMake $context $context.SourceRoot $context.BuildPath -Options $options -Targets @('ssl', 'crypto', 'bssl')
    Assert-SdkArtifacts $context @('lib/ssl.lib', 'lib/crypto.lib', 'include/openssl/base.h', 'include/openssl/ssl.h',
        'lib/cmake/OpenSSL/OpenSSLConfig.cmake')
} finally { Restore-SdkEnvironment $savedEnvironment }
