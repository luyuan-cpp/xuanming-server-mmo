#requires -Version 7
<# 复用现有正式 bootstrap，只初始化 mmorpg_mail，独占注册/维护账号和授权卷。 #>
[CmdletBinding()]
param([ValidateSet('prepare', 'register')][string]$Step)
$ErrorActionPreference = 'Stop'
$requestedStep = $Step
. (Join-Path $PSScriptRoot '../../tools/scripts/lib/sql_boot_catalog_docker.ps1')
$paths = Get-DockerSqlBootPaths
if ($requestedStep -ceq 'register') {
    # 本项目拥有这两个卷；原栈的卷只读挂载，绝不清理或改写。
    Invoke-DockerSqlBootRegister -Paths $paths -Service 'mail'
    exit 0
}
$null = [IO.Directory]::CreateDirectory($paths.PrivateRoot)
foreach ($name in @('physical-authority.original', 'lock-qualification.mysql.LOCK_TOUCH.json')) {
    $source = Join-Path '/run/original-sql-boot' $name
    $target = Join-Path $paths.PrivateRoot $name
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) { throw 'Existing database authority evidence is missing.' }
    if (Test-Path -LiteralPath $target) {
        if ((Get-FileHash -LiteralPath $source).Hash -cne (Get-FileHash -LiteralPath $target).Hash) { throw 'Existing database authority evidence changed.' }
    } else { [IO.File]::Copy($source, $target) }
}
$operatorPath = Join-Path $paths.PrivateRoot 'operators.json'
if (-not (Test-Path -LiteralPath $operatorPath)) {
    $suffix = New-LocalSqlBootSecret -Bytes 4
    $operators = [ordered]@{}
    foreach ($kind in @('registrar', 'maintenance')) {
        $user = "gm_mail_$($kind.Substring(0, 3))_$suffix"
        $operators[$kind] = @{ user = $user; principal = "$user@%"; password = (New-LocalSqlBootSecret) }
    }
    Write-LocalSqlBootText -Path $operatorPath -Text ($operators | ConvertTo-Json -Depth 4)
}
$settings = New-DockerSqlBootSettings -Paths $paths
try {
    Set-DockerSqlBootBackend -Settings $settings
    # 限定现有建库实现的输入集合，避免全栈 prepare 的网关/全局库副作用。
    $settings.LogicalDatabases = [string[]]@('mmorpg_mail')
    Initialize-LocalSqlBootDatabases -Settings $settings -RepoRoot $paths.RepoRoot
    Write-LocalSqlBootLog 'prepared isolated mail database; existing game services unchanged'
} finally { Remove-DockerSqlBootClientFile -Settings $settings }
