# 全球匹配链路的本机开发密钥。只注入调用进程需要的键；部署环境由 Secret 提供，不调用本文件。
# 值在 run/secrets/match-dev.env（run/ 已被 .gitignore 覆盖）随机生成一次并复用，调用方已设置的值优先，全程不回显。
#
# MATCH_MYSQL_PASSWORD 不在这里：它是数据库账号的口令，不是能随机生成的串；常驻 match 只从写者启动目录取
#（maintenance_drain.ps1 的 runtimeEnvironment，明确禁止父进程回退）。本机建表口令见 go_services.ps1 的 -Command migrate。
function Get-MatchDevSecretAllNames {
    @('MATCH_PROBE_SECRET', 'RANK_PRODUCER_SECRET_MATCH', 'MMORPG_MATCH_OUTCOME_SECRET_BATTLE', 'MMORPG_PLAYER_INBOX_SECRET_MATCH', 'MMORPG_MATCH_INTERNAL_SECRET_GUILD')
}
function Get-MatchDevSecretNames {
    param([Parameter(Mandatory)][ValidateSet('match', 'battle', 'player-inbox', 'rank', 'guild')][string]$Service)
    switch ($Service) {
        'match' { Get-MatchDevSecretAllNames }
        # 探测证明和终局证明都由 battle 签、match 验，两端必须同值。
        'battle' { 'MATCH_PROBE_SECRET'; 'MMORPG_MATCH_OUTCOME_SECRET_BATTLE' }
        'player-inbox' { 'MMORPG_PLAYER_INBOX_SECRET_MATCH' }
        'rank' { 'RANK_PRODUCER_SECRET_MATCH' }
        'guild' { 'MMORPG_MATCH_INTERNAL_SECRET_GUILD' }
    }
}
# battle 的全球匹配身份不是密钥，但和探测密钥同生共死：没有机房号 match 就不会把局放到这个节点上。
function Get-MatchDevPlacementDefaults {
    param([Parameter(Mandatory)][string]$Service)
    if ($Service -ceq 'battle') { return [ordered]@{ GLOBAL_MATCH_ENABLED = 'true'; PHYSICAL_REGION = '1' } }
    return [ordered]@{}
}
function Backup-MatchDevSecrets {
    param([Parameter(Mandatory)][string]$Service)
    $saved = @{}
    foreach ($name in @(Get-MatchDevSecretNames -Service $Service) + @((Get-MatchDevPlacementDefaults -Service $Service).Keys)) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    return $saved
}
function Restore-MatchDevSecrets {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($name in $Saved.Keys) { [Environment]::SetEnvironmentVariable($name, $Saved[$name], 'Process') }
}
function Initialize-MatchDevSecrets {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    $needed = @(Get-MatchDevSecretNames -Service $Service)
    $all = @(Get-MatchDevSecretAllNames)
    foreach ($name in $needed) {
        $supplied = [string][Environment]::GetEnvironmentVariable($name, 'Process')
        if ($supplied.Contains("`r") -or $supplied.Contains("`n")) { throw '本机匹配密钥不能包含换行。' }
    }
    $defaults = Get-MatchDevPlacementDefaults -Service $Service
    foreach ($name in $defaults.Keys) {
        if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name, 'Process'))) {
            [Environment]::SetEnvironmentVariable($name, $defaults[$name], 'Process')
        }
    }
    $path = Join-Path $RepoRoot 'run/secrets/match-dev.env'
    $mutex = [Threading.Mutex]::new($false, 'Global\mmorpg-match-dev-secret')
    $held = $false
    try {
        try { $held = $mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $held = $true }
        if (-not $held) { throw '本机匹配密钥文件正在使用，未启动服务。' }
        $values = @{}
        if (Test-Path -LiteralPath $path) {
            foreach ($line in [IO.File]::ReadAllLines($path)) {
                $parts = $line.Split('=', 2)
                if ($parts.Count -eq 2 -and $parts[0] -in $all -and $parts[1].Trim().Length -ge 32) { $values[$parts[0]] = $parts[1].Trim() }
            }
        }
        $changed = $false
        foreach ($name in $all) {
            # 调用方显式给的值写回文件：后起的对端（match 之后才起 battle，或反过来）才能拿到同一把。
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
                [IO.File]::WriteAllLines($temporary, @('# 仅限本机开发；禁止提交、打印或复制到部署环境。') + @($all | ForEach-Object { "$_=$($values[$_])" }), [Text.UTF8Encoding]::new($false))
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
