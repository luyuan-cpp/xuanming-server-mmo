# 角色护栏的本机开发密钥(docs/design/horizontal-scale-logical-db.md §3.3)。只注入调用进程需要的键；部署环境由 Secret 提供，不调用本文件。
# 值在 run/secrets/role-fence-dev.env（run/ 已被 .gitignore 覆盖）随机生成一次并复用，调用方已设置的值优先并写回文件，
# 这样后起的对端（data_service 和各参与者）拿到的是同一把。全程不回显。
#
# 名字与 go/shared/rolefence 的 SecretEnv、CoordinatorSecretEnv 对应，MMORPG_ROLE_FENCE_SECRET_DB 是 go/db 调 GetPlayerRoute 用的。
function Get-RoleFenceDevSecretAllNames {
    @('COORDINATOR', 'GUILD', 'TRADE', 'MAIL', 'MATCH', 'PLAYER_INBOX', 'FRIEND', 'CHAT', 'DB' | ForEach-Object { "MMORPG_ROLE_FENCE_SECRET_$_" })
}
# data_service 验全部调用方、调全部参与者，九把都要。参与者是自己那把加协调者那把；chat、go/db 只读，只要自己那把。
function Get-RoleFenceDevSecretNames {
    param([Parameter(Mandatory)][ValidateSet('data-service', 'guild', 'trade', 'mail', 'match', 'player-inbox', 'friend', 'chat', 'db')][string]$Service)
    switch ($Service) {
        'data-service' { Get-RoleFenceDevSecretAllNames }
        'chat' { 'MMORPG_ROLE_FENCE_SECRET_CHAT' }
        'db' { 'MMORPG_ROLE_FENCE_SECRET_DB' }
        default { "MMORPG_ROLE_FENCE_SECRET_$($Service.Replace('-', '_').ToUpperInvariant())"; 'MMORPG_ROLE_FENCE_SECRET_COORDINATOR' }
    }
}
function Backup-RoleFenceDevSecrets {
    param([Parameter(Mandatory)][string]$Service)
    $saved = @{}
    foreach ($name in @(Get-RoleFenceDevSecretNames -Service $Service)) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    return $saved
}
function Restore-RoleFenceDevSecrets {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($name in $Saved.Keys) { [Environment]::SetEnvironmentVariable($name, $Saved[$name], 'Process') }
}
function Initialize-RoleFenceDevSecrets {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    $needed = @(Get-RoleFenceDevSecretNames -Service $Service)
    $all = @(Get-RoleFenceDevSecretAllNames)
    foreach ($name in $needed) {
        $supplied = [string][Environment]::GetEnvironmentVariable($name, 'Process')
        if ($supplied.Contains("`r") -or $supplied.Contains("`n")) { throw 'Local role fence secrets must not contain line breaks.' }
    }
    $path = Join-Path $RepoRoot 'run/secrets/role-fence-dev.env'
    $mutex = [Threading.Mutex]::new($false, 'Global\mmorpg-role-fence-dev-secret')
    $held = $false
    try {
        try { $held = $mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $held = $true }
        if (-not $held) { throw 'The local role fence secret file is in use; service not started.' }
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
