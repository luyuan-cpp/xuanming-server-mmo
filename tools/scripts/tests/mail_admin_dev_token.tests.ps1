#requires -Version 7
<#
.SYNOPSIS
    邮件 MailAdmin 本机开发令牌(tools/scripts/lib/mail_admin_dev_token.ps1)的回归。
.DESCRIPTION
    只在临时目录里读写令牌文件,不碰仓库的 run/secrets;每个用例前后备份 / 还原 MAIL_PRODUCER_SECRET_ADMIN,
    不污染跑测试的 shell。CI(deploy-config-tests.yml)在 Linux pwsh 上跑,路径一律 Join-Path。
.EXAMPLE
    pwsh -File tools/scripts/tests/mail_admin_dev_token.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. (Join-Path $PSScriptRoot '../lib/mail_admin_dev_token.ps1')

# 与 go/mail/etc/mail.yaml 的 Producers.admin 同名(生产者分用途签名替代了旧的单一 M1 令牌)。
$envName = 'MAIL_PRODUCER_SECRET_ADMIN'
$outerSaved = [Environment]::GetEnvironmentVariable($envName)

function New-TempRepoRoot {
    param([switch]$WithoutMarker)
    $root = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-mail-admin-token-tests-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $root -Force | Out-Null
    if (-not $WithoutMarker) {
        # 库只认有 go/mail/etc/mail.yaml 的目录为仓库根(Assert-MailAdminDevTokenRepoRoot)。
        $marker = Join-Path (Join-Path (Join-Path (Join-Path $root 'go') 'mail') 'etc') 'mail.yaml'
        New-Item -ItemType Directory -Path (Split-Path -Parent $marker) -Force | Out-Null
        Set-Content -LiteralPath $marker -Value '# test marker' -Encoding utf8
    }
    return $root
}

function Reset-TokenEnv {
    Remove-Item "Env:$envName" -ErrorAction SilentlyContinue
    # 每个用例都要能看到播报(只播报一次的开关是脚本级的)。
    $script:MailAdminDevTokenAnnounced = $false
}

$tempRoots = New-Object System.Collections.Generic.List[string]
try {
    Test-Case '调用方已设足够长的值:原样保留、返回 0、不写文件' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        $preset = 'caller-provided-mail-admin-token-0123456789abcdef'
        [Environment]::SetEnvironmentVariable($envName, $preset)
        $injected = Initialize-MailAdminDevToken -RepoRoot $root 6>$null
        Assert-Equal -Expected 0 -Actual $injected -Because '调用方自带的值不算本次注入'
        Assert-Equal -Expected $preset -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '一个字节都不改'
        Assert-True -Condition (-not (Test-Path -LiteralPath (Get-MailAdminDevTokenStorePath -RepoRoot $root))) -Because '不把调用方自带的令牌悄悄落盘'
    }

    Test-Case '首次运行:生成 64 个十六进制字符(无空白),写进临时 RepoRoot 下的 run/secrets/mail-admin-producer-dev.env,注入环境' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        $injected = Initialize-MailAdminDevToken -RepoRoot $root 6>$null
        Assert-Equal -Expected 1 -Actual $injected -Because '没有任何来源时注入一把'
        $value = [Environment]::GetEnvironmentVariable($envName)
        Assert-Match -Text $value -Pattern '^[0-9a-f]{64}$' -Because '32 字节随机数 → 64 个小写十六进制字符,不含空白'
        $path = Join-Path (Join-Path (Join-Path $root 'run') 'secrets') 'mail-admin-producer-dev.env'
        Assert-True -Condition (Test-Path -LiteralPath $path) -Because '令牌文件在 run/secrets 下(run/* 已被 .gitignore 覆盖)'
        Assert-Equal -Expected $value -Actual (Read-MailAdminDevTokenStore -Path $path) -Because '文件里的值就是注入的值'
    }

    Test-Case '文件里已有值:复用,两次注入拿到同一把(mail 与 robot / CLI 一致的前提)' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        Initialize-MailAdminDevToken -RepoRoot $root 6>$null | Out-Null
        $first = [Environment]::GetEnvironmentVariable($envName)
        Reset-TokenEnv
        $injected = Initialize-MailAdminDevToken -RepoRoot $root 6>$null
        Assert-Equal -Expected 1 -Actual $injected -Because '环境里没有时从文件注入'
        Assert-Equal -Expected $first -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '第二次必须复用文件里的同一把'
    }

    Test-Case '去空白后不足 32 字符的值当作没有:环境里的短值被文件 / 新值替换,文件里的短值被重写' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        $path = Get-MailAdminDevTokenStorePath -RepoRoot $root
        New-Item -ItemType Directory -Path (Split-Path -Parent $path) -Force | Out-Null
        [IO.File]::WriteAllLines($path, @('# test', "$envName=short-value"))
        [Environment]::SetEnvironmentVariable($envName, '   too-short-in-env   ')
        $injected = Initialize-MailAdminDevToken -RepoRoot $root 6>$null
        Assert-Equal -Expected 1 -Actual $injected -Because '环境里的短值视同未配置'
        $value = [Environment]::GetEnvironmentVariable($envName)
        Assert-Match -Text $value -Pattern '^[0-9a-f]{64}$' -Because '文件里的短值也不算数,重新生成'
        Assert-Equal -Expected $value -Actual (Read-MailAdminDevTokenStore -Path $path) -Because '重新生成的值写回文件'
    }

    Test-Case 'Backup 时原本没设:Restore 后变量被删掉(包一层哈希表的理由)' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        $saved = Backup-MailAdminDevToken
        Initialize-MailAdminDevToken -RepoRoot $root 6>$null | Out-Null
        Assert-True -Condition (-not [string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable($envName))) -Because '夹具前提:注入成功'
        Restore-MailAdminDevToken $saved
        Assert-Equal -Expected $null -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '原本没设,还原后必须没有(注入的令牌不能留在父进程)'
    }

    Test-Case 'Backup 时原本有值:Restore 还原成原值' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        [Environment]::SetEnvironmentVariable($envName, 'x')
        $saved = Backup-MailAdminDevToken
        Initialize-MailAdminDevToken -RepoRoot $root 6>$null | Out-Null
        Restore-MailAdminDevToken $saved
        Assert-Equal -Expected 'x' -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '还原成备份时的原值'
        Restore-MailAdminDevToken $null
        Assert-Equal -Expected 'x' -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '传 $null(没备份过)什么都不做'
    }

    Test-Case 'RepoRoot 不像仓库根(没有 go/mail/etc/mail.yaml):报错,不生成、不写文件、不注入' {
        Reset-TokenEnv
        $root = New-TempRepoRoot -WithoutMarker; $tempRoots.Add($root)
        $threw = $false
        try { Initialize-MailAdminDevToken -RepoRoot $root 6>$null | Out-Null } catch { $threw = $true; $message = $_.Exception.Message }
        Assert-True -Condition $threw -Because '在 robot/ 之类的目录里生成令牌,会写进不被 .gitignore 覆盖的路径'
        Assert-Match -Text $message -Pattern 'does not look like the repository root' -Because '报错要说清原因'
        Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $root 'run'))) -Because '一个文件都不写'
        Assert-Equal -Expected $null -Actual ([Environment]::GetEnvironmentVariable($envName)) -Because '不注入'
    }

    Test-Case '不传 RepoRoot:默认取本库所属的仓库根(tools/scripts/lib 往上三级),与当前目录无关' {
        $expected = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
        Assert-Equal -Expected $expected -Actual $script:MailAdminDevTokenDefaultRepoRoot -Because '按库文件自身位置推出,不读当前目录'
        Assert-True -Condition (Test-Path -LiteralPath (Join-Path $expected 'go/mail/etc/mail.yaml')) -Because '默认根就是本仓库,通得过标记检查'
        $param = (Get-Command Initialize-MailAdminDevToken).Parameters['RepoRoot']
        Assert-True -Condition (-not ($param.Attributes | Where-Object { $_ -is [System.Management.Automation.ParameterAttribute] -and $_.Mandatory })) -Because '-RepoRoot 可省略'
    }

    Test-Case '播报只说出处,不回显令牌值' {
        Reset-TokenEnv
        $root = New-TempRepoRoot; $tempRoots.Add($root)
        $output = (Initialize-MailAdminDevToken -RepoRoot $root 6>&1 | Out-String)
        $value = [Environment]::GetEnvironmentVariable($envName)
        Assert-Match -Text $output -Pattern 'Injected local MailAdmin admin producer development secret \(value hidden' -Because '要有一行播报'
        Assert-True -Condition (-not $output.Contains($value)) -Because '播报里不得出现令牌值'
    }
}
finally {
    if ($null -eq $outerSaved) { Remove-Item "Env:$envName" -ErrorAction SilentlyContinue }
    else { [Environment]::SetEnvironmentVariable($envName, $outerSaved) }
    foreach ($root in $tempRoots) { Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue }
}

exit (Complete-TestRun -SuiteName 'mail_admin_dev_token')
