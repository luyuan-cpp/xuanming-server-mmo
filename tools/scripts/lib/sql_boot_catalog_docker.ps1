#requires -Version 7
<#
.SYNOPSIS
    docker 调试栈(deploy/debug/compose.game.yml)的写者注册步骤,在 compose 的一次性容器里运行。

.DESCRIPTION
    与 K8s 的 registrar init 同一个形状:每次启动前注册,单次启动授权写进这个服务的私有卷,常驻容器按容器内路径读。
    授权清单、账号、角色模板和单条目 roster 全部复用 sql_boot_catalog_local.ps1,这里只管容器特有的三件事:
      - 端点固定 compose 的 mysql,root 口令来自容器环境。本机入口的回环校验没有放宽,这里不走它;
      - 命名卷里的上一轮产物要先清(K8s 新 Pod 的 emptyDir 天然是空的);
      - 常驻容器以 10001 运行,产物要交给它:Unix 上的私有文件校验要求本账号可读、他人不可读写。

    -Step prepare                     每次 up 跑一次:补库,生成注册步骤共用的口令和原件,判定全局库要不要跑 data_service -migrate。
    -Step register -WriterService X   每个写者启动前跑一次:建这一 boot 的账号,调 writer_authority register。

    生产门禁一条没动:注册走正式 writer_authority,常驻进程走正式 LoadWriterConfig,没有 dev 旁路。不打印口令。
    用法见 tools/scripts/README.md 的 prepare_docker_debug.py 一节。

    dot-source 只定义函数;直接运行才真正执行。
#>
[CmdletBinding()]
param(
    [ValidateSet('prepare', 'register')][string]$Step = 'prepare',
    [string]$WriterService = ''
)

. (Join-Path $PSScriptRoot 'sql_boot_catalog_local.ps1')

# 容器内路径,和 compose.game.yml 的挂载一一对应。脚本和模板是 prepare_docker_debug.py 按仓库布局复制的只读副本,所以 RepoRoot 从脚本位置推。
function Get-DockerSqlBootPaths {
    return [pscustomobject]@{
        RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
        ConfigRoot = '/app/sql-boot-config'
        PrivateRoot = '/run/sql-boot/private'
        StateRoot = '/run/sql-boot/state'
        BootRoot = '/run/sql-boot/boot'
        ClientRoot = [IO.Path]::GetTempPath()
        Tool = '/app/service'
    }
}

function New-DockerSqlBootSettings {
    param([Parameter(Mandatory)]$Paths)
    $rootPassword = [string][Environment]::GetEnvironmentVariable('MYSQL_ROOT_PASSWORD', 'Process')
    if ([string]::IsNullOrEmpty($rootPassword)) { throw 'MYSQL_ROOT_PASSWORD is required; run tools/scripts/prepare_docker_debug.py and start through its .env.' }
    $settings = New-LocalSqlBootSettings -Endpoint 'mysql:3306' -EndpointName 'mysql' -Port 3306 -RootUser 'root' -RootPassword $rootPassword -RootPrincipal 'root@%' `
        -DbConfigPath (Join-Path $Paths.ConfigRoot 'db.yaml') -DataServiceConfigPath (Join-Path $Paths.ConfigRoot 'data_service.yaml') -LoginConfigPath (Join-Path $Paths.ConfigRoot 'login.yaml')
    $settings.Executor = New-LocalSqlBootClientExecutor -Settings $settings -ClientPath 'mysql' -PrivateRoot $Paths.ClientRoot
    return $settings
}

# compose 的 mysql 固定是 MySQL 档;换成别的服务器时这里直接报错,不猜。
function Set-DockerSqlBootBackend {
    param([Parameter(Mandatory)]$Settings)
    $Settings.Backend = Resolve-LocalSqlBootBackend -Settings $Settings -Requested 'mysql'
    $Settings.LockPrimitive = 'LOCK_TOUCH'
}

function Remove-DockerSqlBootClientFile {
    param($Settings)
    if ($null -ne $Settings -and $Settings.Executor.DefaultsFile -and (Test-Path -LiteralPath $Settings.Executor.DefaultsFile)) { Remove-Item -LiteralPath $Settings.Executor.DefaultsFile -Force }
}

function Invoke-DockerSqlBootPrepare {
    param([Parameter(Mandatory)]$Paths)
    $settings = New-DockerSqlBootSettings -Paths $Paths
    try {
        Set-DockerSqlBootBackend -Settings $settings
        # 已有数据卷不会重跑 mysql-init,这里幂等补齐库和网关的表。
        Initialize-LocalSqlBootDatabases -Settings $settings -RepoRoot $Paths.RepoRoot -Requested @('gateway')
        # 这三份只在这一步生成:注册步骤是并行跑的,放那边会互相覆盖口令。
        $null = Get-LocalSqlBootPhysicalAuthority -Settings $settings -PrivateRoot $Paths.PrivateRoot
        $null = Get-LocalSqlBootLockQualification -Settings $settings -PrivateRoot $Paths.PrivateRoot
        $null = Get-LocalSqlBootOperators -PrivateRoot $Paths.PrivateRoot
        # data_service -migrate 只接受空库,而每次 up 都会重跑一次性容器:权威表齐全时留标记,data-migrate 见到就跳过。
        $marker = Join-Path $Paths.StateRoot 'global-schema-ready'
        $inspection = Get-LocalSqlBootGlobalSchemaState -Settings $settings
        switch -CaseSensitive ($inspection.State) {
            'fresh' { if (Test-Path -LiteralPath $marker) { Remove-Item -LiteralPath $marker -Force } }
            'ready' { Write-LocalSqlBootText -Path $marker -Text "$($settings.GlobalSchema)`n" }
            default { throw "$($settings.GlobalSchema) has tables but lacks the authority set (missing: $($inspection.Missing -join ', ')). data_service -migrate only runs on a fresh schema and this script never drops data; recreate the debug volumes with docker compose down -v, then start again." }
        }
        Write-LocalSqlBootLog "prepared: backend=$($settings.Backend) global-schema=$($inspection.State)"
    } finally {
        Remove-DockerSqlBootClientFile -Settings $settings
    }
}

# 上一轮的授权、配置和消费标记不带进新一 boot。consume 是单独的卷,挂载点删不掉,只清里面。
function Clear-DockerSqlBootInstance {
    param([Parameter(Mandatory)][string]$InstanceRoot)
    $null = [IO.Directory]::CreateDirectory($InstanceRoot)
    foreach ($child in @(Get-ChildItem -LiteralPath $InstanceRoot -Force)) {
        if ($child.Name -cne 'consume') { Remove-Item -LiteralPath $child.FullName -Recurse -Force; continue }
        foreach ($marker in @(Get-ChildItem -LiteralPath $child.FullName -Force)) { Remove-Item -LiteralPath $marker.FullName -Recurse -Force }
    }
}

# 重复 up 也会重跑注册步骤,而常驻容器原样不动。上一轮的账号还有连接,就是服务还在跑,它的连接池还要重连,不能删。
function Test-DockerSqlBootAccountsLive {
    param([Parameter(Mandatory)]$Settings, [object[]]$Accounts = @())
    $names = @($Accounts | ForEach-Object { ([string]$_.principal).Split('@')[0] } | Where-Object { $_ -cmatch '^[a-z][a-z0-9_]{0,31}$' })
    if ($names.Count -eq 0) { return $false }
    $list = ($names | ForEach-Object { "'$_'" }) -join ','
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT COUNT(*) FROM performance_schema.threads WHERE PROCESSLIST_USER IN ($list);" -Label 'probe live writer sessions')
    return ($rows.Count -eq 1 -and $rows[0] -cne '0')
}

# 常驻镜像的运行身份(deploy/k8s/Dockerfile.go-svc、Dockerfile.java-svc 的 USER 10001:10001)。
function Set-DockerSqlBootOwner {
    param([Parameter(Mandatory)][string]$Path)
    $result = Invoke-LocalSqlBootProcess -FilePath 'chown' -Arguments @('-R', '10001:10001', $Path) -TimeoutSeconds 60
    if ($result.Code -ne 0) { throw "chown failed on $Path (exit code $($result.Code))." }
}

function Invoke-DockerSqlBootRegister {
    param([Parameter(Mandatory)]$Paths, [Parameter(Mandatory)][string]$Service)
    if ($Service -cnotin @(Get-LocalSqlBootWriterServices)) { throw "Unknown writer service '$Service'." }
    $instanceRoot = Join-Path $Paths.BootRoot $Service
    Clear-DockerSqlBootInstance -InstanceRoot $instanceRoot
    $settings = New-DockerSqlBootSettings -Paths $Paths
    try {
        Set-DockerSqlBootBackend -Settings $settings
        $meta = Get-LocalSqlBootRouteMeta -Settings $settings
        $physical = Get-LocalSqlBootPhysicalAuthority -Settings $settings -PrivateRoot $Paths.PrivateRoot
        $qualification = Get-LocalSqlBootLockQualification -Settings $settings -PrivateRoot $Paths.PrivateRoot
        $operators = Get-LocalSqlBootOperators -PrivateRoot $Paths.PrivateRoot
        $bootId = New-LocalSqlBootSecret -Bytes 4
        $instance = [pscustomobject]@{ InstanceKey = $Service; Service = $Service; ConfigPath = (Join-Path $Paths.ConfigRoot "$Service.yaml") }
        $item = New-LocalSqlBootInstanceFiles -Settings $settings -RepoRoot $Paths.RepoRoot -Instance $instance -BootRoot $Paths.BootRoot -BootId $bootId -Meta $meta -PhysicalSha256 $physical -QualificationFile $qualification

        # 账号记录每服务一份:注册步骤并行跑,共用一个文件会互相覆盖。先记账再建账号,中途失败时下一轮也能按记录清掉。
        $accountsPath = Join-Path $Paths.PrivateRoot "runtime-accounts.$Service.json"
        $known = @(if (Test-Path -LiteralPath $accountsPath) { Get-Content -LiteralPath $accountsPath -Raw | ConvertFrom-Json })
        $live = Test-DockerSqlBootAccountsLive -Settings $settings -Accounts $known
        $accountPlan = Get-LocalSqlBootStaleAccounts -Accounts $known -Instances @([pscustomobject]@{ InstanceKey = $Service; Running = $live })
        $records = @($accountPlan.Keep) + @([pscustomobject]@{ principal = $item.Principal; instanceKey = $Service })
        Write-LocalSqlBootText -Path $accountsPath -Text (ConvertTo-Json -InputObject ([object[]]$records) -Depth 3)
        $dropSql = @($accountPlan.Drop | ForEach-Object { "DROP USER IF EXISTS $(ConvertTo-AuthoritySqlAccount $_.principal);" }) -join "`n"
        $tablesBySchema = Get-LocalSqlBootTablesBySchema -Settings $settings
        $manifest = Get-LocalSqlBootGrantManifest -Settings $settings
        $accountSql = New-LocalSqlBootAccountSql -Settings $settings -RepoRoot $Paths.RepoRoot -Operators $operators -Prepared @($item) -TablesBySchema $tablesBySchema -Manifest $manifest
        $null = Invoke-LocalSqlBootSql -Settings $settings -Sql ($dropSql + "`n" + $accountSql) -Label 'create writer account'

        $control = New-LocalSqlBootControlEnvironment -Settings $settings -Operators $operators -PhysicalSha256 $physical
        $null = Register-LocalSqlBootInstance -Item $item -Tool $Paths.Tool -ControlEnvironment $control -PhysicalSha256 $physical

        # 部署身份和物理 authority 摘要不是秘密,但 K8s 上来自 Pod UID 和清单;这里由入口脚本从卷里读出来设环境变量。
        $grantDirectory = [IO.Path]::GetDirectoryName($item.GrantFile)
        Write-LocalSqlBootText -Path (Join-Path $grantDirectory 'deployment-id') -Text $item.DeploymentId
        Write-LocalSqlBootText -Path (Join-Path $grantDirectory 'physical-authority-sha256') -Text $physical
        Set-DockerSqlBootOwner -Path $instanceRoot
        Write-LocalSqlBootLog "registered $Service as $($item.Principal)"
    } finally {
        Remove-DockerSqlBootClientFile -Settings $settings
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    $ErrorActionPreference = 'Stop'
    $paths = Get-DockerSqlBootPaths
    if ($Step -ceq 'prepare') { Invoke-DockerSqlBootPrepare -Paths $paths }
    else { Invoke-DockerSqlBootRegister -Paths $paths -Service $WriterService }
}
