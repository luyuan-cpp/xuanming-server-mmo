# 本机启动复用 login.yaml 已有的开发密钥，不另写密钥文件；部署只接受外部注入。
# 返回原环境供 finally 恢复，全程不输出值。解析仅支持仓库开发配置的固定标量形状。
function Enter-LocalGateIdentityEnvironment {
    param([Parameter(Mandatory)][string]$RepoRoot, [bool]$AllowDevelopmentDefault = $true)
    $name = 'MMORPG_INTERNAL_AUTH_SECRET'
    $previous = [Environment]::GetEnvironmentVariable($name, 'Process')
    if (-not [string]::IsNullOrEmpty($previous)) {
        if ([Text.Encoding]::UTF8.GetByteCount($previous) -lt 32 -or $previous -cne $previous.Trim()) { throw '内部身份密钥格式无效。' }
        return @{ $name = $previous }
    }
    if (-not $AllowDevelopmentDefault) { throw '非开发模式必须注入内部身份签名密钥。' }
    $yaml = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/login/etc/login.yaml'))
    if ($yaml -notmatch '(?m)^Mode:\s*(dev|test)\s*$') { throw '仅 dev/test 的本机启动可读取开发密钥。' }
    $match = [regex]::Match($yaml, '(?m)^  InternalAuth:\s*\r?\n(?:    [^\r\n]*\r?\n)*?    Value:\s*"([^"\r\n]+)"')
    if (-not $match.Success -or [Text.Encoding]::UTF8.GetByteCount($match.Groups[1].Value) -lt 32) { throw '本机 login 开发密钥不可用，请显式注入环境变量。' }
    [Environment]::SetEnvironmentVariable($name, $match.Groups[1].Value, 'Process')
    return @{ $name = $previous }
}

function Restore-LocalGateIdentityEnvironment {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($key in $Saved.Keys) { [Environment]::SetEnvironmentVariable($key, $Saved[$key], 'Process') }
}
