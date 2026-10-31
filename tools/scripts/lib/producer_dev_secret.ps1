# 邮件、排行榜生产者签名的本机开发密钥。只注入调用进程需要的键；部署环境由 Secret 提供，不调用本文件。
# 值在 run/secrets/producer-dev.env（run/ 已被 .gitignore 覆盖）随机生成一次并复用，调用方已设置的值优先并写回文件，
# 这样后起的对端（rank 签、mail 验）拿到的是同一把。
#
# MAIL_PRODUCER_SECRET_ADMIN 不在这里：运维 CLI 和 robot 也要它，由 mail_admin_dev_token.ps1 单独管。
function Get-ProducerDevSecretAllNames {
    @('MAIL_PRODUCER_SECRET_RANK', 'MAIL_PRODUCER_SECRET_GUILD', 'RANK_PRODUCER_SECRET_ADMIN')
}
function Get-ProducerDevSecretNames {
    param([Parameter(Mandatory)][ValidateSet('mail', 'rank')][string]$Service)
    switch ($Service) {
        'mail' { 'MAIL_PRODUCER_SECRET_RANK'; 'MAIL_PRODUCER_SECRET_GUILD' }
        'rank' { 'MAIL_PRODUCER_SECRET_RANK'; 'RANK_PRODUCER_SECRET_ADMIN' }
    }
}
function Backup-ProducerDevSecrets {
    param([Parameter(Mandatory)][string]$Service)
    $saved = @{}
    foreach ($name in @(Get-ProducerDevSecretNames -Service $Service)) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    return $saved
}
function Restore-ProducerDevSecrets {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($name in $Saved.Keys) { [Environment]::SetEnvironmentVariable($name, $Saved[$name], 'Process') }
}
function Initialize-ProducerDevSecrets {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    $needed = @(Get-ProducerDevSecretNames -Service $Service)
    $all = @(Get-ProducerDevSecretAllNames)
    foreach ($name in $needed) {
        $supplied = [string][Environment]::GetEnvironmentVariable($name, 'Process')
        if ($supplied.Contains("`r") -or $supplied.Contains("`n")) { throw 'Local producer secrets must not contain line breaks.' }
    }
    $path = Join-Path $RepoRoot 'run/secrets/producer-dev.env'
    $mutex = [Threading.Mutex]::new($false, 'Global\mmorpg-producer-dev-secret')
    $held = $false
    try {
        try { $held = $mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $held = $true }
        if (-not $held) { throw 'The local producer secret file is in use; service not started.' }
        $values = @{}
        if (Test-Path -LiteralPath $path) {
            foreach ($line in [IO.File]::ReadAllLines($path)) {
                $parts = $line.Split('=', 2)
                if ($parts.Count -eq 2 -and $parts[0] -in $all -and $parts[1].Trim().Length -ge 32) { $values[$parts[0]] = $parts[1].Trim() }
            }
        }
        $changed = $false
        foreach ($name in $all) {
            $current = if ($name -in $needed) { ([string][Environment]::GetEnvironmentVariable($name, 'Process')).Trim() } else { '' }
            if ($current.Length -ge 32) {
                if ($values[$name] -cne $current) { $values[$name] = $current; $changed = $true }
            } elseif (-not $values.ContainsKey($name)) {
                $bytes = [byte[]]::new(32)
                [Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
                $values[$name] = [Convert]::ToHexString($bytes).ToLowerInvariant()
                $changed = $true
            }
        }
        if ($changed) {
            $directory = Split-Path -Parent $path
            [IO.Directory]::CreateDirectory($directory) | Out-Null
            $temporary = Join-Path $directory ([Guid]::NewGuid().ToString('N') + '.tmp')
            try {
                [IO.File]::WriteAllLines($temporary, @('# 仅限本机开发；禁止提交、打印或复制到部署环境。') + @($all | ForEach-Object { "$_=$($values[$_])" }))
                [IO.File]::Move($temporary, $path, $true)
            } finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force } }
        }
        foreach ($name in $needed) {
            if (([string][Environment]::GetEnvironmentVariable($name, 'Process')).Trim().Length -lt 32) {
                [Environment]::SetEnvironmentVariable($name, $values[$name], 'Process')
            }
        }
    } finally { if ($held) { $mutex.ReleaseMutex() }; $mutex.Dispose() }
}
