#requires -Version 7
<#
.SYNOPSIS
    通用资产通道(docs/design/guild-phase2/04-asset-channel.md §4.32)**本机开发密钥**的唯一来源。

.DESCRIPTION
    资产 RPC 的请求体里带 HMAC 签名:guild / trade 用 MMORPG_ASSET_OP_SECRET_<CALLER> 签名,
    C++ scene 用**同名变量**验签。两边值不一致时每一次资产 RPC 都回 27008(AssetAuthFailed),
    本地冒烟全红,而日志里看不到任何"密钥不匹配"的线索(密钥值不许进日志)。
    所以本机必须有一份**跨进程一致**的值 —— 这正是本文件存在的理由。

    值不写死在脚本里:密钥值不进仓库、不进日志、不进指标、不进错误文本(AGENTS §11.3)。
    值只在本机生成一次,落在 run/secrets/assetop-dev.env;`run/*` 在 .gitignore 里,
    永远不会被提交。删掉那个文件就会重新生成新值 —— 此时 scene 与 guild / trade 必须一起重启,
    否则一半进程还拿着旧值,表现同样是 27008。

    生成的值是 64 个十六进制字符,刻意不含空白:Go 侧 assetop.NewSigner 会 TrimSpace,
    C++ 侧 DefaultSecretLookup 直接用 getenv 的原串**不 trim**。带首尾空白的值两边看到的
    字节数不同,签名必然对不上,表现又是 27008。调用者自己设的值不在这里改动,
    要自带密钥就别带换行。

    **仅限本机 dev 的约定**。预发 / 生产的两把密钥必须由部署侧注入
    (k8s_deploy.ps1 的 Resolve-InjectedSecret -MinLength 32,规格 §4.43 第 29 项),
    部署链从不调用本文件的任何函数。

    调用方(tools/scripts/cpp_nodes.ps1、tools/scripts/start_game.ps1)的用法:
        . (Join-Path $ScriptDir 'lib/assetop_dev_secret.ps1')
        $prev = Backup-AssetOpDevSecrets
        Initialize-AssetOpDevSecrets -RepoRoot $RepoRoot
        try { ...启动子进程... } finally { Restore-AssetOpDevSecrets $prev }
#>

Set-StrictMode -Off

# 两个调用方各一把(不变量 I6:每条流只有一个服务分配 seq,所以密钥也一人一把)。
# 变量名与 §4.32 的 MMORPG_ASSET_OP_SECRET_<CALLER> 契约、go/trade/internal/svc 的
# AssetOpSecretEnv、C++ asset_op_auth 的默认查找逐字一致。
$script:AssetOpDevSecretEnvNames = @(
    'MMORPG_ASSET_OP_SECRET_GUILD',
    'MMORPG_ASSET_OP_SECRET_TRADE'
)

# 与 go/shared/assetop.MinSecretLen、C++ kAssetOpMinSecretLen 同值。去首尾空白后不足这么长
# 视同**未配置**,调用方会当场拒绝签名 —— 所以本机兜底值必须显著长于它。
$script:AssetOpDevSecretMinLength = 32

# 生成 32 字节随机数 → 64 个十六进制字符,远超下限,且不含空白 / 引号,
# 经 yaml、命令行、环境变量传递都不会被改写。
$script:AssetOpDevSecretRandomBytes = 32

# 只在一次脚本运行里播报一次,免得多实例启动时刷屏。播报**只说注入了几把,不回显值**。
$script:AssetOpDevSecretsAnnounced = $false

function Get-AssetOpDevSecretStorePath {
    <#
    .SYNOPSIS
        本机开发密钥文件的路径(run/secrets/assetop-dev.env,已被 .gitignore 的 `run/*` 覆盖)。
    #>
    param([Parameter(Mandatory = $true)][string]$RepoRoot)
    return (Join-Path $RepoRoot 'run/secrets/assetop-dev.env')
}

function Backup-AssetOpDevSecrets {
    <#
    .SYNOPSIS
        记下当前进程里这两个变量的原值(可能是 $null = 没设过),供 Restore 还原。
    .DESCRIPTION
        与 cpp_nodes.ps1 对 ZONE_ID / NODE_IP / *_RUN_MODE 的做法同一条纪律:
        临时改父进程环境只为让子进程继承,用完必须还原,不污染调用者的 shell。
    #>
    $saved = @{}
    foreach ($name in $script:AssetOpDevSecretEnvNames) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name)
    }
    return $saved
}

function Restore-AssetOpDevSecrets {
    <#
    .SYNOPSIS
        还原 Backup-AssetOpDevSecrets 记下的原值,包含"原本没设"这一种情况。
    #>
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($name in $script:AssetOpDevSecretEnvNames) {
        $previous = $Saved[$name]
        if ($null -eq $previous) {
            Remove-Item "Env:$name" -ErrorAction SilentlyContinue
        } else {
            [Environment]::SetEnvironmentVariable($name, $previous)
        }
    }
}

function New-AssetOpDevSecretValue {
    <#
    .SYNOPSIS
        生成一把本机开发密钥(64 个十六进制字符)。用密码学随机源,不是 Get-Random。
    #>
    $bytes = [byte[]]::new($script:AssetOpDevSecretRandomBytes)
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return [System.Convert]::ToHexString($bytes).ToLowerInvariant()
}

function Read-AssetOpDevSecretStore {
    <#
    .SYNOPSIS
        读 run/secrets/assetop-dev.env,返回 名字 -> 值 的哈希表;文件不在或读不动返回空表。
    .DESCRIPTION
        只认 `NAME=VALUE` 行,`#` 开头是注释。**不做任何回显**:返回值里带着密钥,
        调用方除了塞进子进程环境之外不许拿它做别的事。
    #>
    param([Parameter(Mandatory = $true)][string]$Path)
    $values = @{}
    if (-not (Test-Path -LiteralPath $Path)) { return $values }
    try {
        foreach ($line in (Get-Content -LiteralPath $Path -ErrorAction Stop)) {
            $trimmed = $line.Trim()
            if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) { continue }
            $split = $trimmed.IndexOf('=')
            if ($split -le 0) { continue }
            $name = $trimmed.Substring(0, $split).Trim()
            $value = $trimmed.Substring($split + 1).Trim()
            if ($name -in $script:AssetOpDevSecretEnvNames -and $value.Length -ge $script:AssetOpDevSecretMinLength) {
                $values[$name] = $value
            }
        }
    } catch {
        # 读不动就当没有:下面会重新生成并覆盖写回。这里刻意不抛 —— 起服不该被一个
        # 本机缓存文件挡住。异常文本可能含路径,但不会含密钥值。
        return @{}
    }
    return $values
}

function Write-AssetOpDevSecretStore {
    <#
    .SYNOPSIS
        把全部两把密钥写回 run/secrets/assetop-dev.env(整文件重写,自带说明抬头)。
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)]$Values
    )
    $dir = Split-Path -Parent $Path
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

    $lines = New-Object System.Collections.Generic.List[string]
    $lines.Add('# 通用资产通道的**本机开发密钥**,由 tools/scripts/lib/assetop_dev_secret.ps1 首次运行时随机生成。')
    $lines.Add('# 仅限本机 dev:预发 / 生产的密钥必须由部署侧注入(k8s_deploy.ps1 Resolve-InjectedSecret -MinLength 32)。')
    $lines.Add('# run/* 在 .gitignore 里,本文件不会进仓库;不要手工拷贝到别的机器或别的环境。')
    $lines.Add('# 删掉本文件会重新生成新值 —— scene 与 guild / trade 必须一起重启,否则验签不一致(tip 27008)。')
    foreach ($name in $script:AssetOpDevSecretEnvNames) {
        $lines.Add("$name=$($Values[$name])")
    }
    # 一次性整文件写入:半截文件会让下一次读到"只有一把密钥",表现是一个服务能签另一个不能。
    [System.IO.File]::WriteAllLines($Path, $lines)
}

function Initialize-AssetOpDevSecrets {
    <#
    .SYNOPSIS
        保证当前进程环境里有两把 ≥32 字节的资产通道密钥,供随后启动的子进程继承。

    .DESCRIPTION
        取值优先级(与 cpp_nodes.ps1 对 GATE_RUN_MODE 的"只在没设时兜底"同一条):
          1. 调用者已经显式设好且足够长的环境变量 —— 原样保留,一个字节都不改;
          2. run/secrets/assetop-dev.env 里已有的值 —— 复用,保证 scene 与 Go 服务拿到同一把;
          3. 都没有 —— 随机生成并写入该文件。

        跨进程并发(同时起 scene 与 go 服务)用命名互斥量串起来:两个进程各生成一把不同的值
        再互相覆盖,会造出"一半进程拿旧值"的静默不一致,而那正是最难查的一种。

        **返回值是本次从文件 / 新生成中注入的把数,不是密钥值。**

    .PARAMETER RepoRoot
        仓库根目录(密钥文件按它定位)。
    #>
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot
    )

    # 先看调用者是不是已经全都设好了:是的话连密钥文件都不用碰。
    $missing = @($script:AssetOpDevSecretEnvNames | Where-Object {
            $current = [Environment]::GetEnvironmentVariable($_)
            $null -eq $current -or $current.Trim().Length -lt $script:AssetOpDevSecretMinLength
        })
    if ($missing.Count -eq 0) { return 0 }

    $path = Get-AssetOpDevSecretStorePath -RepoRoot $RepoRoot
    # 互斥量名里不含任何密钥信息;Global\ 前缀让不同会话的 pwsh 也能互斥。
    # 建不出来(权限受限的环境)就退化成无锁:并发首次生成有极小概率各写各的,重跑一次即可收敛;
    # 为此拒绝起服是本末倒置。
    $mutex = $null
    try { $mutex = New-Object System.Threading.Mutex($false, 'Global\mmorpg-assetop-dev-secret') } catch { $mutex = $null }
    $held = $false
    try {
        if ($null -ne $mutex) {
            try { $held = $mutex.WaitOne(10000) } catch [System.Threading.AbandonedMutexException] { $held = $true }
        }

        $stored = Read-AssetOpDevSecretStore -Path $path
        $generated = $false
        foreach ($name in $script:AssetOpDevSecretEnvNames) {
            if (-not $stored.ContainsKey($name)) {
                $stored[$name] = New-AssetOpDevSecretValue
                $generated = $true
            }
        }
        if ($generated) { Write-AssetOpDevSecretStore -Path $path -Values $stored }

        foreach ($name in $missing) {
            [Environment]::SetEnvironmentVariable($name, $stored[$name])
        }
    } finally {
        if ($held) { $mutex.ReleaseMutex() }
        if ($null -ne $mutex) { $mutex.Dispose() }
    }

    if (-not $script:AssetOpDevSecretsAnnounced) {
        $script:AssetOpDevSecretsAnnounced = $true
        # 只说数量与出处,**不回显值**;相对路径让人知道去哪删,不至于以为它进了仓库。
        Write-Host "[asset] 已注入 $($missing.Count) 把本机资产通道开发密钥(值不回显,来自 run/secrets/assetop-dev.env;仅限本机 dev)" -ForegroundColor DarkGray
    }
    return $missing.Count
}
