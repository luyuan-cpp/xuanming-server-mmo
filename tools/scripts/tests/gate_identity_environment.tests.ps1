#requires -Version 7
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/gate_identity_environment.ps1"
function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    throw '期望拒绝，但操作成功'
}
$signingName = 'MMORPG_INTERNAL_AUTH_SIGNING_KEY'
$publicName = 'MMORPG_INTERNAL_AUTH_PUBLIC_KEYS'
# RFC 8032 §7.1 TEST 1(与 lib 里的开发密钥对相同);另一把公钥用来模拟运维显式注入的列表。
$devSeed = '9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60'
$devPublic = 'd75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'
$otherPublic = 'a' * 64
function Clear-IdentityEnvironment {
    [Environment]::SetEnvironmentVariable($signingName, $null, 'Process')
    [Environment]::SetEnvironmentVariable($publicName, $null, 'Process')
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-gate-identity-test-' + [guid]::NewGuid().ToString('N'))
$previousSigning = [Environment]::GetEnvironmentVariable($signingName, 'Process')
$previousPublic = [Environment]::GetEnvironmentVariable($publicName, 'Process')
try {
    $etc = Join-Path $root 'go/login/etc'
    [IO.Directory]::CreateDirectory($etc) | Out-Null
    [IO.File]::WriteAllText((Join-Path $etc 'login.yaml'), "Mode: dev`n")
    Test-Case 'gate 在本机开发拿到开发密钥对，子进程环境可恢复' {
        Clear-IdentityEnvironment
        $saved = Enter-LocalGateIdentityEnvironment -RepoRoot $root -IncludeSigningKey
        Assert-True ($env:MMORPG_INTERNAL_AUTH_SIGNING_KEY -ceq $devSeed) 'gate 拿到开发私钥'
        Assert-True ($env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS -ceq $devPublic) 'gate 拿到配对的开发公钥'
        Restore-LocalGateIdentityEnvironment $saved
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_INTERNAL_AUTH_SIGNING_KEY)) '恢复父环境(私钥)'
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS)) '恢复父环境(公钥)'
    }
    Test-Case 'Go 服务只拿公钥，拿不到签名私钥' {
        Clear-IdentityEnvironment
        $saved = Enter-LocalGateIdentityEnvironment -RepoRoot $root
        Assert-True ($env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS -ceq $devPublic) '验证方拿到开发公钥'
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_INTERNAL_AUTH_SIGNING_KEY)) '验证方不带私钥'
        Restore-LocalGateIdentityEnvironment $saved
    }
    Test-Case '非开发进程不能取开发默认值' {
        Clear-IdentityEnvironment
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root -AllowDevelopmentDefault $false -IncludeSigningKey }) 'must inject' '缺失即拒绝'
        [IO.File]::WriteAllText((Join-Path $etc 'login.yaml'), "Mode: pro`n")
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root }) 'dev or test mode' 'login 不是 dev/test 时不给开发密钥对'
        [IO.File]::WriteAllText((Join-Path $etc 'login.yaml'), "Mode: dev`n")
    }
    Test-Case '显式注入优先，只注入一半或格式错误都拒绝' {
        Clear-IdentityEnvironment
        $env:MMORPG_INTERNAL_AUTH_SIGNING_KEY = $devSeed
        $env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = "$otherPublic,$devPublic"
        $saved = Enter-LocalGateIdentityEnvironment -RepoRoot $root -AllowDevelopmentDefault $false -IncludeSigningKey
        Assert-True ($env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS -ceq "$otherPublic,$devPublic") '保留显式注入的轮换期公钥列表'
        Restore-LocalGateIdentityEnvironment $saved

        Clear-IdentityEnvironment
        $env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = $otherPublic
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root -IncludeSigningKey }) 'together' 'gate 只注入公钥不能配开发私钥'

        Clear-IdentityEnvironment
        $env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = 'short'
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root }) 'malformed' '拒绝格式不对的公钥列表'

        Clear-IdentityEnvironment
        $env:MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = "$devPublic,$devPublic"
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root }) 'malformed' '拒绝重复的公钥'
    }
} finally {
    [Environment]::SetEnvironmentVariable($signingName, $previousSigning, 'Process')
    [Environment]::SetEnvironmentVariable($publicName, $previousPublic, 'Process')
    $absolute = [IO.Path]::GetFullPath($root)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $absolute.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $absolute) -notlike 'mmorpg-gate-identity-test-*') { throw '拒绝清理测试目录之外的路径' }
    if (Test-Path -LiteralPath $absolute) { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'Gate 内部身份环境')
