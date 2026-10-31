# 本机开发跨进程证明。只注入调用进程需要的键；部署环境必须由Secret提供，不调用本文件。
function Get-AccountDirectoryDevSecretAllNames {
    # 末尾是玩家存盘接口的调用密钥:scene 签名、go/db 验签,两端同值。
    @('LOGIN','TRADE','SCENE','SCENE_MANAGER' | ForEach-Object { "MMORPG_ACCOUNT_DIRECTORY_SECRET_$_" }) + 'MMORPG_PLAYER_STORE_SECRET_SCENE'
}
function Get-AccountDirectoryDevSecretNames {
    param([Parameter(Mandatory)][ValidateSet('scene','data-service','login','trade','match','scene-manager','db')][string]$Service)
    $names = switch ($Service) {
        'data-service' { 'LOGIN'; 'TRADE'; 'SCENE'; 'SCENE_MANAGER' }
        'scene' { 'SCENE'; 'TRADE' }
        'login' { 'LOGIN' }
        'trade' { 'TRADE' }
        'match' { 'SCENE' }
        'scene-manager' { 'SCENE_MANAGER' }
    }
    @($names | ForEach-Object { "MMORPG_ACCOUNT_DIRECTORY_SECRET_$_" })
    if ($Service -cin @('scene','db')) { 'MMORPG_PLAYER_STORE_SECRET_SCENE' }
}
function Backup-AccountDirectoryDevSecrets {
    param([Parameter(Mandatory)][string]$Service)
    $saved = @{}
    $names = @(Get-AccountDirectoryDevSecretNames -Service $Service)
    if ($Service -ceq 'scene-manager') { $names = @(Get-AccountDirectoryDevSecretAllNames) }
    foreach ($name in $names) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    return $saved
}
function Restore-AccountDirectoryDevSecrets {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($name in $Saved.Keys) { [Environment]::SetEnvironmentVariable($name, $Saved[$name], 'Process') }
}
function Initialize-AccountDirectoryDevSecrets {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    $needed = @(Get-AccountDirectoryDevSecretNames -Service $Service)
    $all = @(Get-AccountDirectoryDevSecretAllNames)
    foreach ($name in $needed) {
        $supplied = [string][Environment]::GetEnvironmentVariable($name,'Process')
        if ($supplied.Contains("`r") -or $supplied.Contains("`n")) { throw '本机账号目录证明不能包含换行。' }
    }
    if ($Service -ceq 'scene-manager') {
        # 父shell可能已带login/trade/scene能力；SM子进程只得到独立Lookup键，finally按Backup还原父环境。
        foreach ($name in $all) { if ($name -notin $needed) { [Environment]::SetEnvironmentVariable($name,$null,'Process') } }
    }
    if (@($needed | Where-Object { ([string][Environment]::GetEnvironmentVariable($_,'Process')).Trim().Length -lt 32 }).Count -eq 0) { return }
    $path = Join-Path $RepoRoot 'run/secrets/account-directory-dev.env'
    $mutex = [Threading.Mutex]::new($false,'Global\mmorpg-account-directory-dev-secret')
    $held = $false
    try {
        try { $held = $mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $held = $true }
        if (-not $held) { throw '本机账号目录证明文件正在使用，未启动服务。' }
        $values = @{}
        if (Test-Path -LiteralPath $path) {
            foreach ($line in [IO.File]::ReadAllLines($path)) {
                $parts = $line.Split('=',2)
                if ($parts.Count -eq 2 -and $parts[0] -in $all -and $parts[1].Trim().Length -ge 32) { $values[$parts[0]] = $parts[1].Trim() }
            }
        }
        $changed = $false
        foreach ($name in $all) {
            $current = if ($name -in $needed) { ([string][Environment]::GetEnvironmentVariable($name,'Process')).Trim() } else { '' }
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
            $temporary = Join-Path $directory ([Guid]::NewGuid().ToString('N')+'.tmp')
            try {
                [IO.File]::WriteAllLines($temporary, @('# 仅限本机开发；禁止提交、打印或复制到部署环境。') + @($all | ForEach-Object { "$_=$($values[$_])" }), [Text.UTF8Encoding]::new($false))
                [IO.File]::Move($temporary,$path,$true)
            } finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force } }
        }
        foreach ($name in $needed) {
            if (([string][Environment]::GetEnvironmentVariable($name,'Process')).Trim().Length -lt 32) {
                [Environment]::SetEnvironmentVariable($name,$values[$name],'Process')
            }
        }
    } finally { if ($held) { $mutex.ReleaseMutex() }; $mutex.Dispose() }
}
