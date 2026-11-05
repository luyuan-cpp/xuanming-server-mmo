#requires -Version 7
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/gate_identity_environment.ps1"
function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    throw '期望拒绝，但操作成功'
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-gate-identity-test-' + [guid]::NewGuid().ToString('N'))
$previous = [Environment]::GetEnvironmentVariable('MMORPG_INTERNAL_AUTH_SECRET', 'Process')
try {
    $etc = Join-Path $root 'go/login/etc'
    [IO.Directory]::CreateDirectory($etc) | Out-Null
    [IO.File]::WriteAllText((Join-Path $etc 'login.yaml'), "Mode: dev`nSecrets:`n  InternalAuth:`n    Env: MMORPG_INTERNAL_AUTH_SECRET`n    Value: `"fixture-internal-identity-development-key`"`n")
    Test-Case '本机开发读取已有配置，子进程环境可恢复' {
        $env:MMORPG_INTERNAL_AUTH_SECRET = $null
        $saved = Enter-LocalGateIdentityEnvironment -RepoRoot $root
        Assert-True ($env:MMORPG_INTERNAL_AUTH_SECRET -ceq 'fixture-internal-identity-development-key') '采用本机既有身份密钥'
        Restore-LocalGateIdentityEnvironment $saved
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_INTERNAL_AUTH_SECRET)) '恢复父环境'
    }
    Test-Case '非开发进程不能取开发默认值' {
        $env:MMORPG_INTERNAL_AUTH_SECRET = $null
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root -AllowDevelopmentDefault $false }) '必须注入' '缺失即拒绝'
    }
    Test-Case '显式注入优先，格式错误不能静默回退' {
        $env:MMORPG_INTERNAL_AUTH_SECRET = 'fixture-explicit-internal-identity-key'
        $saved = Enter-LocalGateIdentityEnvironment -RepoRoot $root -AllowDevelopmentDefault $false
        Assert-True ($env:MMORPG_INTERNAL_AUTH_SECRET -ceq $saved.MMORPG_INTERNAL_AUTH_SECRET) '保留显式注入'
        $env:MMORPG_INTERNAL_AUTH_SECRET = 'short'
        Assert-Match (Get-ThrownMessage { Enter-LocalGateIdentityEnvironment -RepoRoot $root }) '格式无效' '拒绝弱密钥'
    }
} finally {
    [Environment]::SetEnvironmentVariable('MMORPG_INTERNAL_AUTH_SECRET', $previous, 'Process')
    $absolute = [IO.Path]::GetFullPath($root)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $absolute.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $absolute) -notlike 'mmorpg-gate-identity-test-*') { throw '拒绝清理测试目录之外的路径' }
    if (Test-Path -LiteralPath $absolute) { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'Gate 内部身份环境')
