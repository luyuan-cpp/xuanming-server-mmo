<#
.SYNOPSIS
    给真库测试设写者资格夹具(go/shared/authoritysql/sqltest)要的环境变量。只给本机一次性测试库用。
.DESCRIPTION
    要 dot-source 运行,变量才留在当前进程:
        . tools/scripts/sql_fixture_env.ps1 -Backend tidb -Endpoint 127.0.0.1:14000 -ReceiptDir D:\path\sql-fixture-receipts

    夹具不猜物理身份和锁资格,这两个摘要在测试里只是这个库的标识。TiDB 和 MySQL 各设一遍。
    ReceiptDir 要求是固定 NTFS 盘上的绝对路径,读写权限只属于当前账号、SYSTEM、Administrators,这里按同样的 ACL 建好。
    生产的摘要来自真实资格报告,不走这个脚本。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('tidb', 'mysql')][string]$Backend,
    [Parameter(Mandatory)][string]$Endpoint,
    [Parameter(Mandatory)][string]$ReceiptDir
)

if (-not [IO.Path]::IsPathRooted($ReceiptDir)) {
    throw "ReceiptDir must be an absolute path: $ReceiptDir"
}
New-Item -ItemType Directory -Force -Path $ReceiptDir | Out-Null
$currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$icacls = Join-Path $env:SystemRoot 'System32/icacls.exe'
& $icacls $ReceiptDir '/inheritance:r' '/grant:r' "*${currentSid}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "icacls failed on $ReceiptDir (exit code $LASTEXITCODE)."
}

function Get-SqlFixtureDigest {
    param([Parameter(Mandatory)][string]$Text)
    $hash = [Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($Text))
    return [Convert]::ToHexString($hash).ToLowerInvariant()
}

$env:MMORPG_SQL_BACKEND = $Backend
$env:MMORPG_SQL_FIXTURE_ADMIN_DSN = "root@tcp($Endpoint)/"
$env:MMORPG_SQL_FIXTURE_PHYSICAL_AUTHORITY_SHA256 = Get-SqlFixtureDigest -Text "LOCAL_TEST_PHYSICAL_V1:${Backend}:${Endpoint}"
$env:MMORPG_SQL_FIXTURE_LOCK_QUALIFICATION_SHA256 = Get-SqlFixtureDigest -Text "LOCAL_TEST_LOCK_QUALIFICATION_V1:${Backend}"
$env:MMORPG_SQL_FIXTURE_LOCK_PRIMITIVE = if ($Backend -ceq 'mysql') { 'LOCK_TOUCH' } else { 'SELECT_ONLY' }
$env:MMORPG_SQL_FIXTURE_RECEIPT_DIR = $ReceiptDir
Write-Host "SQL fixture environment set: backend=$Backend endpoint=$Endpoint receipts=$ReceiptDir"
