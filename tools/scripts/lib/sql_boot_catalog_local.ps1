#requires -Version 7
<#
.SYNOPSIS
    本机 SQL 写者启动目录(MMORPG_SQL_BOOT_CATALOG)生成器,只接受回环数据库。

.DESCRIPTION
    一条命令备齐 start_game.ps1 / go_services.ps1 需要的写者授权:
      1. 用本机 root 建库、建表(各服务自己的 -migrate 与 go/db cmd/migrate;常驻账号没有 DDL)。
      2. 按 deploy/sql 的角色模板(maintenance_drain.ps1 的 ConvertTo-AuthorityRoleSql)给每个实例建独立账号。
      3. 每个实例调一次正式的 writer_authority register,拿到单次启动授权和选中配置。闸门跟着逻辑库走:
         实例要写几个逻辑库,一次注册就在几个库各登记一份,授权文件是这些份的集合。
      4. 写出 Get-SqlWriterBootEnvironment 认的目录,并用它自检一遍。

    脚本自己只做正式链路里本来就归部署者的事:建账号(模板写明「账号创建由独立本地部署者完成」)、
    拼单条目 roster(K8s 的 roster 也是操作者预先提供)。注册、grant、选中配置的写出全部走 writer_authority。

    生产门禁一条没动:Go / C++ / Java 没有 dev 旁路,Get-SqlWriterBootEnvironment 的校验照旧。
    锁资格文件是本机声明,不是真实 I3 报告,文件正文写明了这一点;它只进本机注册原件。
    产物全部在已被 git 忽略的 run/secrets/sql-boot-local/,目录 ACL 只留当前账号、SYSTEM、Administrators;不打印口令。
    用法、前置和重来的办法见 tools/scripts/README.md 的 start_game.ps1 一节。
    docker 调试栈不走这个入口,它在容器里复用这里的函数,见 sql_boot_catalog_docker.ps1。

    dot-source 只定义函数;直接运行或经 dev_tools.ps1 -Command sql-boot-catalog-local 才真正执行。
#>
[CmdletBinding()]
param(
    # 逗号分隔或数组都行;空 = 全部写者服务。不碰 SQL 的 Go 服务名会被忽略,调用方可以把整批名字传进来。
    [string[]]$Services = @(),
    # host:port,只接受回环。空 = MMORPG_GLOBAL_AUTHORITY_TIDB_HOST,再空 = go/db/etc/db.yaml 的 Database.Hosts。
    [string]$Endpoint = '',
    # auto = 问服务器自己是什么。mysql 用 LOCK_TOUCH,tidb 用 SELECT_ONLY(player-data-tidb-scaling-plan.md §9.3)。
    [ValidateSet('auto', 'mysql', 'tidb')][string]$Backend = 'auto',
    # 多区一次生成。空 = 单区。存档库按地域分、和区无关(A17c),多区与单区用同一份存档库清单(go/login 的 Placement)。
    [int[]]$Zones = @(),
    # 'name=N,name=N' 或 hashtable,同 go_services.ps1 -Counts。
    [object]$Counts = @{},
    [int]$PortStride = 1,
    [int]$ZonePortShift = 1000,
    # '990017=0,990018=1000':语义区号代入 (Zone-1)*ZonePortShift 会越界时,显式给每个区的端口偏移。启动时 go_services.ps1 -ZonePortOffset 传同一个值。
    [string[]]$ZonePortOffsets = @(),
    # R商城/网关各两副本的公开实例计划；不含口令，所有角色一次合并发布。
    [string]$JavaInstancePlanPath = '',
    [string]$JavaInstancePlanSha256 = '',
    [string]$PhysicalAuthorityFile = '',
    [string]$PhysicalAuthoritySha256 = '',
    [string]$LockQualificationFile = '',
    [string]$LockQualificationSha256 = '',
    [ValidateSet('', 'create', 'open-boot', 'migrate', 'close')][string]$JavaFixtureAction = '',
    [string]$JavaFixtureRequestFile = '',
    [string]$JavaFixtureRequestSha256 = ''
)

. (Join-Path $PSScriptRoot 'release_common.ps1')
. (Join-Path $PSScriptRoot '../maintenance_drain.ps1')

# 迁移顺序:go/db 与 data_service 在前(data_service.yaml 的首次启动说明),其余互不依赖。
# login 不连 SQL,但 go_services.ps1 同样要它的目录条目;gateway 由 start_game.ps1 单独拉起,同样消费 grant。
function Get-LocalSqlBootWriterServices {
    @('db', 'data_service', 'chat', 'friend', 'guild', 'mail', 'rank', 'match', 'trade', 'player_inbox', 'login', 'gateway')
}

# 与 start_game.ps1 的 $optionalServices 同义:建表失败只跳过它们,不拖垮登录和匹配。
function Get-LocalSqlBootOptionalServices {
    @('chat', 'friend', 'mail', 'rank', 'trade', 'player_inbox')
}

function Get-LocalSqlBootNonWriterServices {
    @('client_rpc_router', 'scene_manager', 'player_locator')
}

# 权限真值表:角色模板已给 runtime 只读,renderer 拒绝它们进业务表清单。
function Get-LocalSqlBootAuthorityTables {
    @('route_meta', 'writer_registry', 'writer_slot', 'writer_lifecycle_receipt', 'sql_writer_drain_run', 'sql_writer_freeze_ticket', 'sql_writer_maintenance_receipt', 'db_move_sentinel')
}

function Get-LocalSqlBootRoot {
    param([Parameter(Mandatory)][string]$RepoRoot)
    return [IO.Path]::GetFullPath((Join-Path $RepoRoot 'run/secrets/sql-boot-local'))
}

function Write-LocalSqlBootLog {
    param([Parameter(Mandatory)][string]$Message)
    Write-Host "[sql-boot] $Message"
}

# 错误文本可能进启动日志:抹掉随机口令形状的长十六进制、IDENTIFIED BY 和 DSN 里的口令。
function ConvertTo-LocalSqlBootSafeText {
    param([AllowEmptyString()][string]$Text = '')
    $safe = $Text -replace '[0-9a-fA-F]{32,}', '***'
    $safe = $safe -replace "(?i)IDENTIFIED BY\s+'[^']*'", "IDENTIFIED BY '***'"
    $safe = $safe -replace ':[^:@\s]*@tcp\(', ':***@tcp('
    $safe = $safe.Trim()
    if ($safe.Length -gt 2000) { $safe = $safe.Substring(0, 2000) }
    return $safe
}

function New-LocalSqlBootSecret {
    param([int]$Bytes = 32)
    $buffer = [byte[]]::new($Bytes)
    [Security.Cryptography.RandomNumberGenerator]::Fill($buffer)
    return [Convert]::ToHexString($buffer).ToLowerInvariant()
}

function New-LocalSqlBootIdentity {
    param([int]$Bytes = 16)
    $buffer = [byte[]]::new($Bytes)
    [Security.Cryptography.RandomNumberGenerator]::Fill($buffer)
    return [Convert]::ToBase64String($buffer)
}

function ConvertTo-LocalSqlBootBase64 {
    param([Parameter(Mandatory)][string]$Hex)
    return [Convert]::ToBase64String([Convert]::FromHexString($Hex))
}

function Write-LocalSqlBootText {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][AllowEmptyString()][string]$Text)
    $temporary = "$Path.$([Guid]::NewGuid().ToString('N')).tmp"
    try {
        [IO.File]::WriteAllText($temporary, $Text, [Text.UTF8Encoding]::new($false))
        [IO.File]::Move($temporary, $Path, $true)
    } finally {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
    }
}

# 所有子进程走这里:参数逐个传、凭据只进子进程环境(不进命令行和父环境),标准输入喂 SQL。测试桩替换本函数,不起真实进程。
function Invoke-LocalSqlBootProcess {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [string[]]$Arguments = @(),
        [Collections.IDictionary]$Environment = @{},
        [string]$WorkingDirectory = '',
        [string]$StandardInput = '',
        [int]$TimeoutSeconds = 120,
        [switch]$RequireCompleteExit,
        [hashtable]$FixtureDiagnostic,
        [ValidateSet('cf3-read-file','cf3-create-file','cf3-validate-directory','sql-surface','prepare-schema','register','retire')]
        [string]$FixtureOperation
    )
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $FilePath
    foreach ($argument in $Arguments) { $start.ArgumentList.Add($argument) }
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $start.StandardOutputEncoding = [Text.UTF8Encoding]::new($false)
    $start.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    $hasInput = -not [string]::IsNullOrEmpty($StandardInput)
    if ($hasInput) {
        $start.RedirectStandardInput = $true
        $start.StandardInputEncoding = [Text.UTF8Encoding]::new($false)
    }
    if ($WorkingDirectory) { $start.WorkingDirectory = $WorkingDirectory }
    foreach ($name in @($Environment.Keys)) { $start.Environment[[string]$name] = [string]$Environment[$name] }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $start
    $event=$null; $started=$false
    if ($null -ne $FixtureDiagnostic) {
        $clock=[Diagnostics.Stopwatch]::StartNew()
        $event=[ordered]@{operation=$FixtureOperation;sourceLine=$MyInvocation.ScriptLineNumber;timeoutSeconds=$TimeoutSeconds
            elapsedMs=0;classification='start-unconfirmed';exitConfirmed=$false;exitCode=$null}
        $FixtureDiagnostic.nativeCalls.Add($event)
    }
    try {
        if (-not $process.Start()) { throw "Failed to start $([IO.Path]::GetFileName($FilePath))." }
        $started=$true
        if ($null -ne $event) { $event.classification='process-io-unconfirmed' }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if ($hasInput) {
            $process.StandardInput.Write($StandardInput)
            $process.StandardInput.Close()
        }
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            if ($null -ne $event) { $event.classification='timeout' }
            try { $process.Kill($true) } catch { $null = $_ }
            if ($RequireCompleteExit -and -not $process.WaitForExit(5000)) { throw '受限控制进程超时且cleanup_pending；保留原件HOLD' }
            throw "$([IO.Path]::GetFileName($FilePath)) did not finish within $TimeoutSeconds seconds."
        }
        # 后台孙进程可能继承管道,读尾部只等有限时间。
        if ($null -ne $event) { $event.classification='output-unconfirmed' }
        $outDone=$stdout.Wait(5000); $errDone=$stderr.Wait(5000)
        if ($RequireCompleteExit -and (-not $outDone -or -not $errDone)) { throw '受限控制进程输出未完整闭合；保留原件HOLD' }
        $out = if ($outDone) { $stdout.Result } else { '' }
        $err = if ($errDone) { $stderr.Result } else { '' }
        if ($null -ne $event) { $event.classification=if ($process.ExitCode -eq 0) { 'confirmed' } else { 'nonzero-exit' } }
        return [pscustomobject]@{ Code = $process.ExitCode; Out = $out; Err = $err }
    } finally {
        if ($null -ne $event) {
            $event.elapsedMs=$clock.ElapsedMilliseconds
            try {
                if ($started -and $process.HasExited) { $event.exitConfirmed=$true; $event.exitCode=$process.ExitCode }
            } catch { } # Inability to query process state leaves exit unconfirmed.
        }
        $process.Dispose()
    }
}

# 只放行 127.0.0.1、localhost、[::1] 和显式端口;校验先于任何连接,远端地址连试探都不发。错误不回显原值:写错时可能带口令。
function Assert-LocalSqlBootEndpoint {
    param([Parameter(Mandatory)][AllowEmptyString()][string]$Endpoint)
    $match = [regex]::Match($Endpoint, '\A(?<name>127\.0\.0\.1|localhost|\[::1\]):(?<port>[0-9]{1,5})\z')
    $port = 0
    if (-not $match.Success -or -not [int]::TryParse($match.Groups['port'].Value, [ref]$port) -or $port -lt 1 -or $port -gt 65535) {
        throw 'sql-boot-catalog-local only accepts a loopback database endpoint: 127.0.0.1, localhost or [::1] with an explicit port.'
    }
    return [pscustomobject]@{ Name = $match.Groups['name'].Value.Trim('[', ']'); Port = $port }
}

function Get-LocalSqlBootYamlRequired {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$KeyPath)
    $value = Get-YamlScalar -Path $Path -KeyPath $KeyPath
    if (-not $value.Found -or [string]::IsNullOrEmpty($value.Value)) { throw "Cannot read $KeyPath from $Path; refusing to guess." }
    return [string]$value.Value
}

function ConvertTo-LocalSqlBootOrdinalSet {
    param([string[]]$Values)
    $unique = [Collections.Generic.SortedSet[string]]::new([StringComparer]::Ordinal)
    foreach ($value in $Values) { $null = $unique.Add($value) }
    return [string[]]@($unique)
}

# 本机账号与库名全部取已有配置的原件,缺了不猜。root 与 go_services.ps1 的本机建表同一来源:
# go/db 的账号(与 compose 的 MYSQL_ROOT_PASSWORD 同值),值只进子进程环境;容器外连入 CURRENT_USER() 是 root@%。
function Resolve-LocalSqlBootSettings {
    param([Parameter(Mandatory)][string]$RepoRoot, [string]$Endpoint = '')
    $dbConfig = Join-Path $RepoRoot 'go/db/etc/db.yaml'
    $resolved = $Endpoint
    if ([string]::IsNullOrWhiteSpace($resolved)) { $resolved = [string][Environment]::GetEnvironmentVariable('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST', 'Process') }
    if ([string]::IsNullOrWhiteSpace($resolved)) { $resolved = Get-LocalSqlBootYamlRequired -Path $dbConfig -KeyPath 'ServerConfig.Database.Hosts' }
    $parsed = Assert-LocalSqlBootEndpoint -Endpoint $resolved

    $rootUser = [string][Environment]::GetEnvironmentVariable('MMORPG_LOCAL_SQL_ROOT_USER', 'Process')
    if ([string]::IsNullOrWhiteSpace($rootUser)) { $rootUser = Get-LocalSqlBootYamlRequired -Path $dbConfig -KeyPath 'ServerConfig.Database.User' }
    $rootPassword = [string][Environment]::GetEnvironmentVariable('MMORPG_LOCAL_SQL_ROOT_PASSWORD', 'Process')
    if ([string]::IsNullOrEmpty($rootPassword)) { $rootPassword = Get-LocalSqlBootYamlRequired -Path $dbConfig -KeyPath 'ServerConfig.Database.Passwd' }
    if ($rootPassword.IndexOfAny([char[]]@([char]0, [char]10, [char]13)) -ge 0) { throw 'The local SQL root password must not contain line breaks.' }
    $rootPrincipal = [string][Environment]::GetEnvironmentVariable('MMORPG_SQL_SCHEMA_PRINCIPAL', 'Process')
    if ([string]::IsNullOrWhiteSpace($rootPrincipal)) { $rootPrincipal = "${rootUser}@%" }

    return New-LocalSqlBootSettings -Endpoint $resolved -EndpointName $parsed.Name -Port $parsed.Port -RootUser $rootUser -RootPassword $rootPassword -RootPrincipal $rootPrincipal `
        -DataServiceConfigPath (Join-Path $RepoRoot 'go/data_service/etc/data_service.yaml') -LoginConfigPath (Join-Path $RepoRoot 'go/login/etc/login.yaml')
}

# 库名清单与保护集合只有这一份。端点和 root 身份由调用方定:本机入口先过回环校验,docker 调试栈固定 compose 的 mysql。
function New-LocalSqlBootSettings {
    param(
        [Parameter(Mandatory)][string]$Endpoint,
        [Parameter(Mandatory)][string]$EndpointName,
        [Parameter(Mandatory)][int]$Port,
        [Parameter(Mandatory)][string]$RootUser,
        [Parameter(Mandatory)][string]$RootPassword,
        [Parameter(Mandatory)][string]$RootPrincipal,
        [Parameter(Mandatory)][string]$DataServiceConfigPath,
        # 新号落哪几个存档库由 login 的 Placement 决定(地域码、每地域存档库数 K),本机建库、迁移、写资格都按它算。
        [Parameter(Mandatory)][string]$LoginConfigPath
    )
    $globalSchema = Get-LocalSqlBootYamlRequired -Path $DataServiceConfigPath -KeyPath 'SnapshotMySQL.DBName'
    # 片数和 data_service 同一来源;没写按 1,和 Directory.Shards() 一样。
    $shardsValue = Get-YamlScalar -Path $DataServiceConfigPath -KeyPath 'Directory.LogicalShards'
    $logicalShards = if ($shardsValue.Found -and -not [string]::IsNullOrEmpty($shardsValue.Value)) { [int]$shardsValue.Value } else { 1 }
    $directoryShards = [string[]]@(Get-DirectoryDbNames -Base $globalSchema -LogicalShards $logicalShards)
    $ledgerSchema = Get-DirectoryLedgerDbName -Base $globalSchema
    # 存档库(docs/design/player-data-tidb-scaling-plan.md §7.2,A17c):本地域固定 K 个 player_store_<id>_db,和区无关,zone 库已退役。
    # 地域码与 K 取 login 的 Placement(新号按 player_id % K 落库),库名公式和部署脚本同一份(release_common.ps1 Get-LoginPlayerStoreDbNames)。
    $playerDatabases = [string[]]@(Get-LoginPlayerStoreDbNames -LoginConfigPath $LoginConfigPath)
    # 调用方给了白名单(DB_ALLOWED_DATABASES 优先,其次 MMORPG_DB_ALLOWED_DATABASES)就必须恰好是这 K 个库:
    # 对不上说明白名单和 login 不是同一份部署,落在缺的那个库上的玩家 go/db 存不了盘。
    $allowList = [string][Environment]::GetEnvironmentVariable('DB_ALLOWED_DATABASES', 'Process')
    if ([string]::IsNullOrWhiteSpace($allowList)) { $allowList = [string][Environment]::GetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES', 'Process') }
    if (-not [string]::IsNullOrWhiteSpace($allowList)) {
        $listed = [string[]]@($allowList -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })
        $retired = @($listed | Where-Object { $_ -cmatch '\Azone_[0-9]+_db\z' })
        if ($retired.Count -gt 0) {
            throw "The player database allow list names $($retired -join ','): zone databases are retired (player-data-tidb-scaling-plan.md §7.2 A17c); list only the player stores player_store_<id>_db."
        }
        if ($listed.Count -ne $playerDatabases.Count -or ((ConvertTo-LocalSqlBootOrdinalSet -Values $listed) -join ',') -cne ((ConvertTo-LocalSqlBootOrdinalSet -Values $playerDatabases) -join ',')) {
            throw "The player database allow list ($($listed -join ',')) must be exactly the player stores from go/login Placement: $($playerDatabases -join ',')."
        }
    }

    # 本机全部逻辑库。每个库自带一份写者闸门,写者连接的 DATABASE() 必须等于它的闸门所在的库(authoritysql pool.go、Java WriterAuthority)。
    $schemas = ConvertTo-LocalSqlBootOrdinalSet -Values (@('mmorpg', $ledgerSchema, 'mmorpg_chat', 'mmorpg_friend', 'mmorpg_guild', 'mmorpg_mail', 'mmorpg_match', 'mmorpg_player_inbox', 'mmorpg_rank', 'mmorpg_trade') + $directoryShards + $playerDatabases)
    foreach ($schema in $schemas) { $null = ConvertTo-AuthoritySqlIdentifier $schema }

    return [pscustomobject]@{
        Endpoint = $Endpoint; EndpointName = $EndpointName; Port = $Port
        RootUser = $RootUser; RootPassword = $RootPassword; RootPrincipal = $RootPrincipal
        GlobalSchema = $globalSchema; DirectoryShards = $directoryShards; DirectorySchema = $directoryShards[0]; LedgerSchema = $ledgerSchema
        PlayerDatabases = $playerDatabases
        LogicalDatabases = [string[]]@($schemas)
        Executor = $null; Backend = ''; LockPrimitive = ''
    }
}

# Go 的 ReadPrivateControlFile 要求固定 NTFS 盘,目录和文件的读写权限只属于当前账号、SYSTEM、Administrators。
# 只设根目录:子目录和文件都从它继承。用 SID 指定,中文系统的内置组名不一样。
function Initialize-LocalSqlBootPrivateDirectory {
    param([Parameter(Mandatory)][string]$Path)
    $full = [IO.Path]::GetFullPath($Path)
    $drive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($full))
    if ($drive.DriveType -ne [IO.DriveType]::Fixed -or $drive.DriveFormat -cne 'NTFS') { throw 'The repository must live on a fixed NTFS drive: writer grants need persistent private ACLs.' }
    $null = [IO.Directory]::CreateDirectory($full)
    if ((Get-Item -LiteralPath $full -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'The private SQL boot directory must not be a reparse point.' }
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $icacls = [IO.Path]::GetFullPath((Join-Path $env:SystemRoot 'System32/icacls.exe'))
    $result = Invoke-LocalSqlBootProcess -FilePath $icacls -Arguments @($full, '/inheritance:r', '/grant:r', "*${sid}:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '/Q') -TimeoutSeconds 60
    if ($result.Code -ne 0) { throw "icacls failed on the private SQL boot directory (exit code $($result.Code))." }
    Assert-LocalSqlBootPrivateDirectory -Path $full
}

function Assert-LocalSqlBootPrivateDirectory {
    param([Parameter(Mandatory)][string]$Path)
    $acl = Get-Acl -LiteralPath $Path
    if (-not $acl.AreAccessRulesProtected) { throw 'The private SQL boot directory still inherits ACL entries.' }
    $trusted = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18', 'S-1-5-32-544')
    foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
        if ($rule.IdentityReference.Value -cnotin $trusted) { throw 'The private SQL boot directory grants access to an untrusted account.' }
    }
}

function Find-LocalSqlBootDocker {
    $candidates = @(
        $(if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA 'Programs/DockerDesktop/resources/bin/docker.exe' })
        $(if ($env:ProgramFiles) { Join-Path $env:ProgramFiles 'Docker/Docker/resources/bin/docker.exe' })
    )
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) { return [IO.Path]::GetFullPath($candidate) }
    }
    $found = Get-Command 'docker.exe' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($found) { return $found.Source }
    return $null
}

# 选 SQL 通路:本次端点就是 compose 的 mysql 容器发布的端口时走容器内客户端(主机不必装 mysql,口令留在容器环境里);
# 否则用主机 mysql.exe,口令放进私有目录的选项文件,不上命令行。--context 固定本机引擎,与 start_game.ps1 同一口径。
function Resolve-LocalSqlBootExecutor {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$PrivateRoot)
    $docker = Find-LocalSqlBootDocker
    if ($docker) {
        $published = $null
        try { $published = Invoke-LocalSqlBootProcess -FilePath $docker -Arguments @('--context', 'desktop-linux', 'port', 'mysql', '3306/tcp') -TimeoutSeconds 30 } catch { $published = $null }
        if ($null -ne $published -and $published.Code -eq 0 -and ([string]$published.Out -match "(?m):$($Settings.Port)\s*$")) {
            return [pscustomobject]@{ Kind = 'docker'; Path = $docker; Container = 'mysql'; DefaultsFile = '' }
        }
    }
    $client = Get-Command 'mysql.exe' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $client) { throw "No SQL client: endpoint $($Settings.Endpoint) is not the published port of the compose mysql container and mysql.exe is not on PATH." }
    return New-LocalSqlBootClientExecutor -Settings $Settings -ClientPath $client.Source -PrivateRoot $PrivateRoot
}

# mysql 客户端通路:口令放进私有目录的选项文件,不上命令行。调用方负责在 finally 里删掉这个文件。
function New-LocalSqlBootClientExecutor {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$ClientPath, [Parameter(Mandatory)][string]$PrivateRoot)
    $defaults = Join-Path $PrivateRoot 'mysql-client.cnf'
    $quoted = $Settings.RootPassword.Replace('\', '\\').Replace('"', '\"')
    Write-LocalSqlBootText -Path $defaults -Text "[client]`nprotocol=tcp`nhost=$($Settings.EndpointName)`nport=$($Settings.Port)`nuser=$($Settings.RootUser)`npassword=`"$quoted`"`n"
    return [pscustomobject]@{ Kind = 'client'; Path = $ClientPath; Container = ''; DefaultsFile = $defaults }
}

function Invoke-LocalSqlBootSql {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Sql, [Parameter(Mandatory)][string]$Label, [int]$TimeoutSeconds = 300)
    $executor = $Settings.Executor
    $arguments = if ($executor.Kind -ceq 'docker') {
        @('--context', 'desktop-linux', 'exec', '-i', $executor.Container, 'sh', '-c', 'exec mysql --default-character-set=utf8mb4 -uroot -p"$MYSQL_ROOT_PASSWORD" -N -B')
    } else {
        @("--defaults-extra-file=$($executor.DefaultsFile)", '--default-character-set=utf8mb4', '-N', '-B')
    }
    $result = Invoke-LocalSqlBootProcess -FilePath $executor.Path -Arguments $arguments -StandardInput $Sql -TimeoutSeconds $TimeoutSeconds
    if ($result.Code -ne 0) { throw "$Label failed (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err))" }
    return @(([string]$result.Out -split '\r?\n') | Where-Object { $_ -ne '' })
}

function Resolve-LocalSqlBootBackend {
    param([Parameter(Mandatory)]$Settings, [string]$Requested = 'auto')
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql 'SELECT VERSION(),@@version_comment;' -Label 'server version probe')
    $actual = if (($rows -join ' ') -match '(?i)tidb') { 'tidb' } else { 'mysql' }
    if ($Requested -cne 'auto' -and $Requested -cne $actual) { throw "The server at $($Settings.Endpoint) is $actual, not $Requested." }
    return $actual
}

function New-LocalSqlBootDsn {
    param([Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password, [Parameter(Mandatory)][string]$Endpoint, [Parameter(Mandatory)][string]$Database, [string]$Parameters = 'charset=utf8mb4&parseTime=true')
    return "${User}:${Password}@tcp(${Endpoint})/${Database}?${Parameters}"
}

# 只有 maintenance_drain.ps1 的 runtimeEnvironment 白名单里的变量:chat、guild、mail、rank 的 DSN 与 match 的口令。其余服务的账号写在配置里。
function Get-LocalSqlBootRuntimeEnvironment {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Service, [Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password)
    switch -CaseSensitive ($Service) {
        'chat' { return @{ CHAT_AUTHORITY_DSN = (New-LocalSqlBootDsn -User $User -Password $Password -Endpoint $Settings.Endpoint -Database 'mmorpg_chat') } }
        'guild' { return @{ MMORPG_GUILD_SQL_DSN = (New-LocalSqlBootDsn -User $User -Password $Password -Endpoint $Settings.Endpoint -Database 'mmorpg_guild' -Parameters 'charset=utf8mb4&parseTime=true&loc=Local') } }
        'mail' { return @{ MMORPG_MAIL_SQL_DSN = (New-LocalSqlBootDsn -User $User -Password $Password -Endpoint $Settings.Endpoint -Database 'mmorpg_mail') } }
        'rank' { return @{ MMORPG_RANK_SQL_DSN = (New-LocalSqlBootDsn -User $User -Password $Password -Endpoint $Settings.Endpoint -Database 'mmorpg_rank') } }
        'match' { return @{ MATCH_MYSQL_PASSWORD = $Password } }
    }
    return @{}
}

function ConvertTo-LocalSqlBootYamlString {
    param([AllowEmptyString()][string]$Value = '')
    return '"' + $Value.Replace('\', '\\').Replace('"', '\"') + '"'
}

function Find-LocalSqlBootYamlKey {
    param($Lines, [int]$First, [int]$Limit, [int]$Indent, [string]$Key)
    $pattern = '^' + (' ' * $Indent) + [regex]::Escape($Key) + ':(?:\s|$)'
    $found = -1
    for ($i = $First; $i -lt $Limit; $i++) {
        if ($Lines[$i] -cmatch $pattern) {
            if ($found -ge 0) { throw "YAML key $Key is duplicated; refusing to guess which one to rewrite." }
            $found = $i
        }
    }
    return $found
}

# 按缩进定位点分路径,只改这一行(叶子不存在就插到所在段的段首),其余字节原样保留。段不存在或不是块式映射就拒绝,不猜。
function Set-LocalSqlBootYamlScalar {
    param([Parameter(Mandatory)][string]$Content, [Parameter(Mandatory)][string]$KeyPath, [AllowEmptyString()][string]$Literal = '', [switch]$Remove)
    $newline = if ($Content.Contains("`r`n")) { "`r`n" } else { "`n" }
    $lines = [Collections.Generic.List[string]]::new([string[]]($Content -split '\r?\n'))
    $segments = $KeyPath.Split('.')
    $first = 0
    $limit = $lines.Count
    $indent = 0
    for ($depth = 0; $depth -lt $segments.Count - 1; $depth++) {
        $header = Find-LocalSqlBootYamlKey -Lines $lines -First $first -Limit $limit -Indent $indent -Key $segments[$depth]
        if ($header -lt 0 -or $lines[$header] -cnotmatch ':\s*(?:#.*)?$') { throw "YAML section $($segments[$depth]) of $KeyPath is missing or not a block mapping." }
        $bodyLimit = $limit
        $childIndent = -1
        for ($i = $header + 1; $i -lt $limit; $i++) {
            $text = $lines[$i]
            if ([string]::IsNullOrWhiteSpace($text) -or $text.TrimStart().StartsWith('#')) { continue }
            $textIndent = $text.Length - $text.TrimStart(' ').Length
            if ($textIndent -le $indent) { $bodyLimit = $i; break }
            if ($childIndent -lt 0) { $childIndent = $textIndent }
        }
        if ($childIndent -lt 0) { $childIndent = $indent + 2 }
        $first = $header + 1
        $limit = $bodyLimit
        $indent = $childIndent
    }
    $leaf = $segments[$segments.Count - 1]
    $existing = Find-LocalSqlBootYamlKey -Lines $lines -First $first -Limit $limit -Indent $indent -Key $leaf
    if ($Remove) {
        if ($existing -ge 0) { $lines.RemoveAt($existing) }
    } elseif ($existing -ge 0) {
        $lines[$existing] = (' ' * $indent) + $leaf + ': ' + $Literal
    } else {
        $lines.Insert($first, (' ' * $indent) + $leaf + ': ' + $Literal)
    }
    return ($lines -join $newline)
}

# 每个服务连本机数据库要改的键。源配置是给 TiDB / K8s 镜像用的,本机的差异全在这张表里,不动源文件。
# 只写实际要变的键,每一条都对应一条会拒启的校验(见各服务 config.Validate 与 main)。
function Get-LocalSqlBootConfigPatch {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Service, [Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password)
    $backend = $Settings.Backend
    $endpoint = ConvertTo-LocalSqlBootYamlString -Value $Settings.Endpoint
    $userLiteral = ConvertTo-LocalSqlBootYamlString -Value $User
    $passwordLiteral = ConvertTo-LocalSqlBootYamlString -Value $Password
    $values = [ordered]@{}
    $remove = @()
    switch -CaseSensitive ($Service) {
        'data_service' {
            $values['SnapshotMySQL.Backend'] = $backend
            $values['SnapshotMySQL.Host'] = $endpoint
            $values['SnapshotMySQL.User'] = $userLiteral
            $values['SnapshotMySQL.Password'] = $passwordLiteral
            # 源配置写 true(dev 自动建表的旧口径),常驻进程见到 true 直接 panic;表已由 -migrate 建好。
            $values['Schema.AutoMigrate'] = 'false'
        }
        'db' {
            $values['ServerConfig.Database.Backend'] = $backend
            $values['ServerConfig.Database.Hosts'] = $endpoint
            $values['ServerConfig.Database.User'] = $userLiteral
            $values['ServerConfig.Database.Passwd'] = $passwordLiteral
        }
        'chat' { $values['Backend'] = $backend }
        'friend' {
            $values['Backend'] = $backend
            $values['MySQL.Host'] = $endpoint
            $values['MySQL.User'] = $userLiteral
            $values['MySQL.Password'] = $passwordLiteral
        }
        'guild' {
            $values['MySQL.Backend'] = $backend
            # 连接串只走环境变量,配置里不留账号。
            $values['MySQL.DataSourceEnv'] = 'MMORPG_GUILD_SQL_DSN'
            $remove += 'MySQL.DataSource'
        }
        'mail' {
            # 源配置缺 Etcd.Key,go-zero 加载就失败;必须显式空串(契约 D-13,同 friend.yaml)。
            $values['Etcd.Key'] = '""'
            $values['Backend'] = $backend
        }
        'rank' {
            $values['Etcd.Key'] = '""'
            $values['Backend'] = $backend
        }
        'match' {
            $values['GlobalMatch.Backend'] = $backend
            $values['GlobalMatch.Host'] = $endpoint
            $values['GlobalMatch.User'] = $userLiteral
        }
        'trade' {
            $values['MySQL.Backend'] = $backend
            $values['MySQL.Host'] = $endpoint
            $values['MySQL.User'] = $userLiteral
            $values['MySQL.Password'] = $passwordLiteral
        }
        'player_inbox' {
            $values['MySQL.Backend'] = $backend
            $values['MySQL.Host'] = $endpoint
            $values['MySQL.User'] = $userLiteral
            $values['MySQL.Password'] = $passwordLiteral
        }
        'login' { }
        default { throw "No local SQL config patch for service $Service." }
    }
    return [pscustomobject]@{ Values = $values; Remove = $remove }
}

function ConvertTo-LocalSqlBootConfig {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Service, [Parameter(Mandatory)][string]$Content, [Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password)
    $patch = Get-LocalSqlBootConfigPatch -Settings $Settings -Service $Service -User $User -Password $Password
    $text = $Content
    foreach ($key in $patch.Remove) { $text = Set-LocalSqlBootYamlScalar -Content $text -KeyPath $key -Remove }
    foreach ($key in @($patch.Values.Keys)) { $text = Set-LocalSqlBootYamlScalar -Content $text -KeyPath $key -Literal ([string]$patch.Values[$key]) }
    return $text
}

# gateway 的每 boot 配置:只覆盖数据源和运行模式,其余沿用包内 application.yaml;连接串参数取包内原件,不留第二份。
function New-LocalSqlBootGatewayConfig {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password)
    $source = Join-Path $RepoRoot 'java/gateway_node/src/main/resources/application.yaml'
    $url = Get-LocalSqlBootYamlRequired -Path $source -KeyPath 'spring.datasource.url'
    if (-not $url.Contains('//localhost:3306/')) { throw 'Unexpected spring.datasource.url shape in the gateway application.yaml.' }
    $url = $url.Replace('//localhost:3306/', "//$($Settings.Endpoint)/")
    $requireTiDb = if ($Settings.Backend -ceq 'tidb') { 'true' } else { 'false' }
    return @(
        'spring:'
        '  datasource:'
        "    url: $(ConvertTo-LocalSqlBootYamlString -Value $url)"
        "    username: $(ConvertTo-LocalSqlBootYamlString -Value $User)"
        "    password: $(ConvertTo-LocalSqlBootYamlString -Value $Password)"
        'mmorpg:'
        '  run-mode: dev'
        '  sql:'
        "    require-tidb: $requireTiDb"
        ''
    ) -join "`n"
}

# 这是R同一业务栈的四个全局副本，不是两个zone各自拥有商城库。
# 计划只承载公开寻址与产物身份；凭据由本次producer生成，写进每boot私有配置。
function Read-LocalSqlBootJavaPlan {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Sha256, [int[]]$Zones = @())
    if ($Sha256 -cnotmatch '^[A-Fa-f0-9]{64}$' -or (Get-MaintenanceSha256 -Path $Path) -ine $Sha256) { throw 'Java实例计划缺固定SHA或已经改变。' }
    $plan = Read-MaintenanceJson -Path $Path -ExpectedSha256 $Sha256.ToLowerInvariant()
    Assert-MaintenanceProperties -Value $plan -Required @('format','javaExecutable','javaSha256','instances')
    if ($plan.format -cne 'LOCAL_R_JAVA_FOUR_INSTANCES_V1' -or @($plan.instances).Count -ne 4) { throw 'Java计划必须是R的Mall/Gateway各两个全局副本。' }
    if (-not [IO.Path]::IsPathFullyQualified($plan.javaExecutable) -or [IO.Path]::GetFileName($plan.javaExecutable) -ine 'java.exe' -or $plan.javaSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $plan.javaExecutable) -cne $plan.javaSha256) { throw 'Java运行程序缺固定绝对路径或SHA。' }
    $keys = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $ports = [Collections.Generic.HashSet[int]]::new()
    $databases = @{}
    $instances = @(foreach ($item in $plan.instances) {
        Assert-MaintenanceProperties -Value $item -Required @('instanceKey','service','port','database','jarSha256') -Optional @('loginGrpcEndpoints','redisEndpoint','etcdEndpoint','mallAdminUrl')
        $service = [string]$item.service; $key = [string]$item.instanceKey
        if ($key -cnotin @('mall','mall-replica','gateway','gateway-replica') -or $service -cnotin @('mall','gateway') -or ($key -replace '-replica$','') -cne $service -or -not $keys.Add($key)) { throw 'Java计划角色或实例键重复/不符。' }
        if ($item.port -isnot [long] -and $item.port -isnot [int]) { throw 'Java监听端口必须是整数。' }
        $port = [int]$item.port
        if ($port -lt 1 -or $port -gt 65535 -or -not $ports.Add($port)) { throw 'Java监听端口越界或冲突。' }
        $database = [string]$item.database; $null = ConvertTo-AuthoritySqlIdentifier $database
        if ($databases.ContainsKey($service) -and $databases[$service] -cne $database) { throw '同一Java服务的两个副本必须连接同一个全局逻辑库。' }
        $databases[$service] = $database
        $module = if ($service -ceq 'mall') { 'mall_game' } else { 'gateway_node' }
        $jarName = if ($service -ceq 'mall') { 'mall-game-0.1.0-SNAPSHOT.jar' } else { 'gateway-node-0.0.1-SNAPSHOT.jar' }
        $jar = [IO.Path]::GetFullPath((Join-Path $RepoRoot "java/$module/target/$jarName"))
        if ($item.jarSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $jar) -cne $item.jarSha256) { throw 'Java运行包不是当前计划固定的R产物。' }
        if ($service -ceq 'gateway') {
            foreach ($required in @('loginGrpcEndpoints','redisEndpoint','etcdEndpoint','mallAdminUrl')) { if (-not $item.Contains($required)) { throw '网关计划缺两区Go login或本轮协调/商城地址。' } }
            $mapped = [Collections.Generic.HashSet[int]]::new()
            foreach ($endpoint in ([string]$item.loginGrpcEndpoints -split ',')) {
                if ($endpoint -cnotmatch '^(?<zone>[1-9][0-9]*)=(?<endpoint>127\.0\.0\.1:[0-9]+)$' -or -not $mapped.Add([int]$Matches.zone)) { throw '网关只能显式按区绑定本机Go login，不能轮询到别区。' }
                $null = Assert-LocalSqlBootEndpoint $Matches.endpoint
            }
            if ((@($mapped | Sort-Object) -join ',') -cne (@($Zones | Sort-Object) -join ',') -or $Zones.Count -ne 2) { throw '网关Go login映射必须恰好覆盖本次两个zone。' }
            $null = Assert-LocalSqlBootEndpoint ([string]$item.redisEndpoint)
            foreach ($name in @('etcdEndpoint','mallAdminUrl')) {
                $uri = $null
                if (-not [uri]::TryCreate([string]$item[$name], [UriKind]::Absolute, [ref]$uri) -or $uri.Scheme -cne 'http' -or $uri.Host -cne '127.0.0.1' -or $uri.Port -lt 1 -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or $uri.AbsolutePath -cne '/') { throw 'Java协调与商城地址必须为显式本机HTTP根地址。' }
            }
        } elseif (@('loginGrpcEndpoints','redisEndpoint','etcdEndpoint','mallAdminUrl') | Where-Object { $item.Contains($_) }) { throw 'Mall计划不得夹带Gateway寻址字段。' }
        [pscustomobject]@{ InstanceKey = $key; Service = $service; Port = $port; Database = $database; Java = $true; Running = $false; ConfigPath = (Join-Path $RepoRoot "java/$module/src/main/resources/application.yaml"); Jar = $jar; JarSha256 = $item.jarSha256; Settings = $item; JavaExecutable = $plan.javaExecutable; JavaSha256 = $plan.javaSha256 }
    })
    if ($keys.Count -ne 4 -or $databases.mall -ceq $databases.gateway) { throw 'Mall和Gateway必须分别拥有独立的全局逻辑库。' }
    foreach ($gateway in @($instances | Where-Object Service -CEQ 'gateway')) {
        $target = ([uri]$gateway.Settings.mallAdminUrl).Port
        if ($target -notin @($instances | Where-Object Service -CEQ 'mall' | ForEach-Object Port)) { throw '网关商城地址没有绑定本计划的Mall端口。' }
    }
    return $instances
}

function New-LocalSqlBootJavaConfig {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)]$Instance, [Parameter(Mandatory)][string]$User, [Parameter(Mandatory)][string]$Password)
    $text = [IO.File]::ReadAllText($Instance.ConfigPath)
    $url = "jdbc:mysql://$($Settings.Endpoint)/$($Instance.Database)?useSSL=false&allowPublicKeyRetrieval=true&serverTimezone=UTC&connectTimeout=3000&socketTimeout=10000"
    $values = [ordered]@{ 'server.address' = '"127.0.0.1"'; 'server.port' = [string]$Instance.Port; 'spring.datasource.url' = (ConvertTo-LocalSqlBootYamlString $url); 'spring.datasource.username' = (ConvertTo-LocalSqlBootYamlString $User); 'spring.datasource.password' = (ConvertTo-LocalSqlBootYamlString $Password); 'mmorpg.run-mode' = 'dev'; 'mmorpg.sql.require-tidb' = $(if ($Settings.Backend -ceq 'tidb') { 'true' } else { 'false' }) }
    if ($Instance.Service -ceq 'mall') {
        $values['commerce.run-mode'] = 'dev'; $values['commerce.require-tidb'] = $values['mmorpg.sql.require-tidb']
    } else {
        $redis = Assert-LocalSqlBootEndpoint $Instance.Settings.redisEndpoint
        $values['spring.data.redis.host'] = (ConvertTo-LocalSqlBootYamlString $redis.Name); $values['spring.data.redis.port'] = [string]$redis.Port
        $values['login.grpc.endpoints'] = (ConvertTo-LocalSqlBootYamlString $Instance.Settings.loginGrpcEndpoints)
        # 本轮仅用固定Go login映射；不让共享etcd中的别区或他人进程改写它。
        $values['login.grpc.discovery-enabled'] = 'false'
        $etcdList = [regex]::new('(?m)(^etcd:\r?\n)  endpoints:\r?\n(?:    - [^\r\n]*\r?\n)+')
        if ($etcdList.Matches($text).Count -ne 1) { throw '网关etcd源配置形状改变，拒绝猜测列表替换。' }
        $literal = ConvertTo-LocalSqlBootYamlString $Instance.Settings.etcdEndpoint
        $text = $etcdList.Replace($text, [Text.RegularExpressions.MatchEvaluator]{ param($m) $m.Groups[1].Value + "  endpoints: [$literal]`n" })
        $values['mall-admin.base-url'] = (ConvertTo-LocalSqlBootYamlString $Instance.Settings.mallAdminUrl)
    }
    foreach ($key in $values.Keys) { $text = Set-LocalSqlBootYamlScalar -Content $text -KeyPath $key -Literal $values[$key] }
    return $text
}

function Initialize-LocalSqlBootMall {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Database)
    # 闸门DDL只有authoritysql一份；Schema进程和NORMAL业务进程分离。
    $tool = Resolve-LocalSqlBootTool -RepoRoot $RepoRoot -Name 'writer_authority'
    $environment = @{ MMORPG_SQL_SCHEMA_DSN = (New-LocalSqlBootDsn -User $Settings.RootUser -Password $Settings.RootPassword -Endpoint $Settings.Endpoint -Database $Database); MMORPG_SQL_SCHEMA_PRINCIPAL = $Settings.RootPrincipal; MMORPG_SQL_BACKEND = $Settings.Backend; MMORPG_SQL_WRITER_GRANT_FILE = ''; MMORPG_SQL_REGISTRAR_DSN = ''; MMORPG_SQL_MAINTENANCE_DSN = '' }
    $result = Invoke-LocalSqlBootProcess -FilePath $tool -Arguments @('-command','prepare-schema','-schema',$Database,'-timeout','60s') -Environment $environment -TimeoutSeconds 70
    if ($result.Code -ne 0) { throw 'Mall本库写者闸门迁移失败；保留现场，不注册NORMAL。' }
    $source = [IO.File]::ReadAllText((Join-Path $RepoRoot 'java/mall_game/src/main/resources/db/schema.sql'))
    $expected = @([regex]::Matches($source, '(?im)^CREATE TABLE IF NOT EXISTS ([a-z0-9_]+)\s*\(') | ForEach-Object { $_.Groups[1].Value })
    $identifier = ConvertTo-AuthoritySqlIdentifier $Database
    $present = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT table_name FROM information_schema.tables WHERE table_schema='$Database';" -Label 'inspect Mall business schema')
    $business = @($present | Where-Object { $_ -cnotin @(Get-LocalSqlBootAuthorityTables) })
    if ($business.Count -eq 0) {
        $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql ("USE $identifier;`n" + $source) -Label 'create Mall business tables'
    } elseif (@($expected | Where-Object { $_ -cnotin $business }).Count -gt 0) { throw 'Mall库是半成品；不自动补写旧布局，不发布目录。' }
}

# 单调时钟是启动预算的唯一来源；测试只替换这个接缝，不让回归等待真实120秒。
function New-LocalSqlBootJavaStartupClock { return [Diagnostics.Stopwatch]::StartNew() }

function Start-LocalSqlBootJavaInstances {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$PlanPath, [Parameter(Mandatory)][string]$PlanSha256, [Parameter(Mandatory)][int[]]$Zones, [Parameter(Mandatory)][string]$SummaryPath)
    $instances = @(Read-LocalSqlBootJavaPlan -RepoRoot $RepoRoot -Path $PlanPath -Sha256 $PlanSha256 -Zones $Zones)
    $summary = Read-MaintenanceJson -Path $SummaryPath
    if (-not $summary.Contains('javaInstancePlan') -or $summary.javaInstancePlan.sha256 -cne $PlanSha256.ToLowerInvariant() -or [IO.Path]::GetFullPath($summary.javaInstancePlan.path) -ine [IO.Path]::GetFullPath($PlanPath) -or $summary.bootId -cnotmatch '^[a-f0-9]{8}$') { throw 'Java启动必须对应本次一次发布的同一计划/目录。' }
    if ((@($summary.javaInstances | Sort-Object) -join ',') -cne (@($instances | ForEach-Object InstanceKey | Sort-Object) -join ',')) { throw '启动摘要没有完整四Java角色。' }
    $helper = $summary.privateFileHelper
    if ($helper.sha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $helper.path) -cne $helper.sha256) { throw 'Java私密文件helper缺固定原件。' }
    $root = [IO.Path]::GetFullPath($RepoRoot)
    $run = Join-Path $root ("run/logs/java-boot/" + $summary.bootId)
    if (Test-Path -LiteralPath $run) { throw '此Java boot已有启动尝试；保留原件，不自动重试。' }
    $previousCatalog = $env:MMORPG_SQL_BOOT_CATALOG
    $prepared = @(); $processes = [Collections.Generic.List[object]]::new(); $originalFailure = $null
    try {
        $env:MMORPG_SQL_BOOT_CATALOG = $summary.catalog
        foreach ($item in $instances) {
            if (@(Get-NetTCPConnection -State Listen -LocalPort $item.Port -ErrorAction SilentlyContinue).Count -gt 0) { throw 'Java计划端口已被占用；不接管已有进程。' }
            $selected = ''
            $environment = Get-SqlWriterBootEnvironment -InstanceKey $item.InstanceKey -RepoRoot $root -ServiceName $item.Service -SelectedRuntimeConfigFile ([ref]$selected)
            if ([string](Get-YamlScalar -Path $selected -KeyPath 'server.port').Value -cne [string]$item.Port) { throw 'Java选中配置与本轮端口计划不符。' }
            $jdbc = [string](Get-YamlScalar -Path $selected -KeyPath 'spring.datasource.url').Value
            if ($jdbc -notmatch ('^jdbc:mysql://(?:127[.]0[.]0[.]1|localhost|\[::1\]):[0-9]+/' + [regex]::Escape($item.Database) + '[?]')) { throw 'Java选中配置没有连接本角色的全局逻辑库。' }
            if ($item.Service -ceq 'gateway' -and [string](Get-YamlScalar -Path $selected -KeyPath 'login.grpc.endpoints').Value -cne [string]$item.Settings.loginGrpcEndpoints) { throw 'Java选中配置不是本轮两区Go login映射。' }
            $environment.MMORPG_SQL_PRIVATE_FILE_HELPER = $helper.path
            $environment.MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256 = $helper.sha256
            $prepared += [pscustomobject]@{ Instance = $item; Config = $selected; Environment = $environment }
        }
        $null = [IO.Directory]::CreateDirectory($run)
        $null = Write-MaintenanceImmutableJson -Path (Join-Path $run 'started.json') -Value @{ format = 'LOCAL_R_JAVA_START_ATTEMPT_V1'; bootId = $summary.bootId; planSha256 = $PlanSha256.ToLowerInvariant(); instances = @($summary.javaInstances) }
        foreach ($row in $prepared | Sort-Object -Stable { if ($_.Instance.Service -ceq 'mall') { 0 } else { 1 } }) {
            $item = $row.Instance
            if ((Get-MaintenanceSha256 $item.Jar) -cne $item.JarSha256 -or (Get-MaintenanceSha256 $item.JavaExecutable) -cne $item.JavaSha256) { throw 'Java实际启动前的运行程序或包发生漂移。' }
            # 每次只把同一个boot的资格注入当前子进程；finally立刻恢复父环境。
            $saved = Enter-SqlWriterBootEnvironment -Environment $row.Environment
            try {
                $arguments = @('-Xmx512m','-XX:ActiveProcessorCount=2','-jar',('"' + $item.Jar + '"'),('"--spring.config.location=file:' + $row.Config.Replace('\','/') + '"'))
                $process = Start-Process -FilePath $item.JavaExecutable -ArgumentList $arguments -WorkingDirectory ([IO.Path]::GetDirectoryName($item.ConfigPath) + '/../../..') -WindowStyle Hidden -RedirectStandardOutput (Join-Path $run ($item.InstanceKey + '.stdout.log')) -RedirectStandardError (Join-Path $run ($item.InstanceKey + '.stderr.log')) -PassThru
                # 保持原Process句柄直到全批结束；失败清理不重新按PID找进程。
                $null = $process.Handle
                $clock = New-LocalSqlBootJavaStartupClock
                $birthPath = Join-Path $run ($item.InstanceKey + '.process.birth.actual.json')
                $owned = [pscustomobject]@{ Process = $process; InstanceKey = $item.InstanceKey; BirthPath = $birthPath; Birth = $null; BirthPersisted = $false }
                $processes.Add($owned)
                $birth = [ordered]@{ format = 'LOCAL_R_JAVA_PROCESS_BIRTH_V1'; bootId = $summary.bootId; instanceKey = $item.InstanceKey; service = $item.Service; pid = $process.Id; creationFiletimeUtc = [string]$process.StartTime.ToUniversalTime().ToFileTimeUtc(); javaExecutable = $item.JavaExecutable; javaSha256 = $item.JavaSha256; jar = $item.Jar; jarSha256 = $item.JarSha256; port = $item.Port; runtimeConfigSha256 = (Get-MaintenanceSha256 $row.Config); planSha256 = $PlanSha256.ToLowerInvariant(); listening_confirmed = $false; businessAccepted = $false }
                $owned.Birth = $birth
                # 启动尝试目录是本boot新建的。身份先于任何监听等待CreateNew落盘；
                # 时钟已开始，写身份耗时同样计入原120秒预算。
                $null = Write-MaintenanceImmutableJson -Path $birthPath -Value $birth
                $owned.BirthPersisted = $true
            } finally { Restore-SqlWriterBootEnvironment -Saved $saved }
            $listening = $false
            while ($clock.Elapsed.TotalSeconds -lt 120) {
                if ($process.HasExited) { throw 'Java新boot启动期退出；保留日志和消费原件。' }
                if (@(Get-NetTCPConnection -State Listen -LocalPort $item.Port -ErrorAction SilentlyContinue | Where-Object OwningProcess -EQ $process.Id).Count -gt 0) { $listening = $true; break }
                Start-Sleep -Milliseconds 200
            }
            if (-not $listening) { throw 'Java新boot未在120秒内拥有计划监听端口。' }
            $null = Write-MaintenanceImmutableJson -Path (Join-Path $run ($item.InstanceKey + '.process.actual.json')) -Value @{ instanceKey = $item.InstanceKey; service = $item.Service; pid = $process.Id; creationFiletimeUtc = $birth.creationFiletimeUtc; javaSha256 = $item.JavaSha256; jarSha256 = $item.JarSha256; port = $item.Port; runtimeConfigSha256 = $birth.runtimeConfigSha256; birthRecord = @{ path = $birthPath; sha256 = (Get-MaintenanceSha256 $birthPath) }; listening_is_not_business_acceptance = $true }
        }
        return [pscustomobject]@{ bootId = $summary.bootId; logDirectory = $run; started = @($summary.javaInstances); businessAccepted = $false }
    } catch {
        $originalFailure = $_
        $cleanupRows = @(); $cleanupPending = $false
        foreach ($owned in $processes) {
            $process = $owned.Process
            $closed = $false; $killAttempted = $false; $waitConfirmed = $false; $cleanupErrorType = $null
            try {
                $closed = $process.HasExited
                if (-not $closed) {
                    $killAttempted = $true; $process.Kill()
                    $waitConfirmed = [bool]$process.WaitForExit(10000)
                    $closed = $waitConfirmed -and $process.HasExited
                }
            } catch { $cleanupErrorType = $_.Exception.GetType().FullName; $closed = $false }
            if (-not $closed) { $cleanupPending = $true }
            $cleanupRows += [ordered]@{ instanceKey = $owned.InstanceKey; pid = $process.Id; birthRecordPath = $owned.BirthPath; birthRecordPersisted = $owned.BirthPersisted; creationFiletimeUtc = $(if ($null -ne $owned.Birth) { $owned.Birth.creationFiletimeUtc } else { $null }); killAttempted = $killAttempted; waitBudgetMilliseconds = 10000; waitConfirmed = $waitConfirmed; exitConfirmed = $closed; cleanup_pending = (-not $closed); cleanupErrorType = $cleanupErrorType }
        }
        # 原异常继续向外抛；清理或写证据失败不能把它盖掉。只有布尔与公开路径
        # 进入Exception.Data，不序列化Process/环境/凭据。
        $originalFailure.Exception.Data['java_cleanup_pending'] = $cleanupPending
        if ($processes.Count -gt 0) {
            $cleanupPath = Join-Path $run 'cleanup.actual.json'
            $originalFailure.Exception.Data['java_cleanup_evidence'] = $cleanupPath
            try {
                $null = Write-MaintenanceImmutableJson -Path $cleanupPath -Value @{ format = 'LOCAL_R_JAVA_FAILED_START_CLEANUP_V1'; bootId = $summary.bootId; originalErrorType = $originalFailure.Exception.GetType().FullName; cleanup_pending = $cleanupPending; processes = $cleanupRows; birth_and_started_records_preserved = $true; releasing_Process_handle_is_not_exit_proof = $true; automatic_retry = $false }
            } catch { Write-Warning 'Writing the Java failure cleanup evidence was not confirmed; the original start error and identity records are kept, so cleanup cannot be considered complete.' }
        }
        if ($cleanupPending) { Write-Warning 'Some Java child processes in this batch are still cleanup_pending; their birth identities and the original error are kept and nothing is retried automatically.' }
        throw $originalFailure
    } finally {
        $env:MMORPG_SQL_BOOT_CATALOG = $previousCatalog
        # Dispose只释放本地监督句柄。未确认退出的状态已明确记录为pending，
        # 后续只能按保留的PID/精确birth身份继续核实，不能把Dispose当成退出。
        $disposeFailed = $false
        foreach ($owned in $processes) {
            try { $owned.Process.Dispose() }
            catch { $disposeFailed = $true; Write-Warning 'Releasing the Java supervision handle was not confirmed; the original start error is kept and this is not treated as a process exit.' }
        }
        if ($disposeFailed) {
            if ($null -ne $originalFailure) { $originalFailure.Exception.Data['java_process_handle_cleanup_pending'] = $true }
            else { throw 'Java已启动，但监督句柄释放未确认；保留出生身份待核实。' }
        }
    }
}

function Get-LocalSqlBootSourceConfigPath {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    $file = if ($Service -ceq 'match') { 'match_service.yaml' } else { "$Service.yaml" }
    return [IO.Path]::GetFullPath((Join-Path $RepoRoot "go/$Service/etc/$file"))
}

# 与 go_services.ps1 的 Resolve-GoExecutablePath 同一查找顺序。
function Resolve-LocalSqlBootServiceExecutable {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service)
    foreach ($candidate in @((Join-Path $RepoRoot "bin/go_services/$Service.exe"), (Join-Path $RepoRoot "go/$Service/$Service.exe"))) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { return [IO.Path]::GetFullPath($candidate) }
    }
    throw "Missing $Service.exe; run: pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build"
}

function Resolve-LocalSqlBootTool {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Name)
    $path = [IO.Path]::GetFullPath((Join-Path $RepoRoot "bin/go_services/$Name.exe"))
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Missing bin/go_services/$Name.exe; run: pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build (without -GoServices)" }
    return $path
}

# 建库与 deploy/mysql-init 同口径:已有卷不会重跑 initdb,这里幂等补齐。gateway 的表同样补,脚本本身可重复执行。
function Initialize-LocalSqlBootDatabases {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [string[]]$Requested = @())
    $statements = foreach ($schema in $Settings.LogicalDatabases) {
        $identifier = ConvertTo-AuthoritySqlIdentifier $schema
        if ($schema -ceq 'mmorpg' -or $schema -cin $Settings.PlayerDatabases) { "CREATE DATABASE IF NOT EXISTS $identifier;" }
        else { "CREATE DATABASE IF NOT EXISTS $identifier DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" }
    }
    $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql (($statements -join "`n") + "`n") -Label 'create local databases'
    # mmorpg 的闸门表也在这份脚本里:网关和 login 的写资格都登记在这个库,它们没有自己的 -migrate。
    if ('gateway' -cin $Requested -or 'login' -cin $Requested) {
        $gatewayTables = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/mysql-init/gateway_tables.sql'))
        $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql $gatewayTables -Label 'create gateway tables'
        if ('gateway' -cin $Requested -and $Settings.PSObject.Properties['JavaDatabases'] -and $Settings.JavaDatabases.gateway -cne 'mmorpg') {
            if ([regex]::Matches($gatewayTables, '(?m)^USE mmorpg;\r?$').Count -ne 1) { throw 'gateway正式DDL的数据库选择形状改变。' }
            $identifier = ConvertTo-AuthoritySqlIdentifier $Settings.JavaDatabases.gateway
            $selected = [regex]::Replace($gatewayTables, '(?m)^USE mmorpg;\r?$', "USE $identifier;")
            $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql $selected -Label 'create planned gateway tables'
        }
    }
}

# data_service -migrate 一次建目录各片和流水库。fresh:这些库全是空的,跑 -migrate。ready:每个库的权威表都齐,跳过(-migrate 拒绝已有表)。
# partial:半成品或旧布局,不替人删数据。
function Get-LocalSqlBootGlobalSchemaState {
    param([Parameter(Mandatory)]$Settings)
    $databases = @($Settings.DirectoryShards) + @($Settings.LedgerSchema)
    $empty = 0
    $missing = [Collections.Generic.List[string]]::new()
    foreach ($database in $databases) {
        $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT table_name FROM information_schema.tables WHERE table_schema='$database';" -Label "inspect $database")
        if ($rows.Count -eq 0) {
            $empty++
            $missing.Add("$database (empty)")
            continue
        }
        foreach ($table in @(Get-LocalSqlBootAuthorityTables | Where-Object { $_ -cnotin $rows })) { $missing.Add("$database.$table") }
    }
    if ($empty -eq $databases.Count) { return [pscustomobject]@{ State = 'fresh'; Missing = @() } }
    return [pscustomobject]@{ State = $(if ($missing.Count -eq 0) { 'ready' } else { 'partial' }); Missing = $missing.ToArray() }
}

function Invoke-LocalSqlBootServiceMigration {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$Service, [Parameter(Mandatory)][string]$WorkDirectory)
    $executable = Resolve-LocalSqlBootServiceExecutable -RepoRoot $RepoRoot -Service $Service
    $config = Join-Path $WorkDirectory "$Service.yaml"
    $source = [IO.File]::ReadAllText((Get-LocalSqlBootSourceConfigPath -RepoRoot $RepoRoot -Service $Service))
    Write-LocalSqlBootText -Path $config -Text (ConvertTo-LocalSqlBootConfig -Settings $Settings -Service $Service -Content $source -User $Settings.RootUser -Password $Settings.RootPassword)
    $environment = @{ MMORPG_SQL_SCHEMA_PRINCIPAL = $Settings.RootPrincipal }
    if ($Service -ceq 'match') {
        # match 的迁移账号只走这两个变量,MATCH_MYSQL_PASSWORD 是常驻账号的。
        $environment['MATCH_MIGRATION_USER'] = $Settings.RootUser
        $environment['MATCH_MIGRATION_PASSWORD'] = $Settings.RootPassword
    } else {
        $runtime = Get-LocalSqlBootRuntimeEnvironment -Settings $Settings -Service $Service -User $Settings.RootUser -Password $Settings.RootPassword
        foreach ($name in @($runtime.Keys)) { $environment[$name] = $runtime[$name] }
    }
    $workingDirectory = [IO.Path]::GetFullPath((Join-Path $RepoRoot "go/$Service"))
    $result = Invoke-LocalSqlBootProcess -FilePath $executable -Arguments @('-f', $config, '-migrate') -Environment $environment -WorkingDirectory $workingDirectory -TimeoutSeconds 300
    if ($result.Code -ne 0) { throw "$Service -migrate failed (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err + [string]$result.Out))" }
}

# 存档库名到编号(placement.StorageIDFromDBName):只有地域存档库 player_store_<id>_db,zone 库已退役(A17c)。编号下限由 cmd/migrate 核。
function Get-LocalSqlBootStorageId {
    param([Parameter(Mandatory)][string]$Database)
    $match = [regex]::Match($Database, '\Aplayer_store_(?<id>[1-9][0-9]*)_db\z')
    if (-not $match.Success) { throw "$Database is not a player store database name player_store_<id>_db." }
    return $match.Groups['id'].Value
}

# go/db 的正式入口是 cmd/migrate,不走 authoritysql。退出码 4 = 迁移成功、有待人工过目的漂移(cmd/migrate/main.go),不拦。
# 每个存档库都迁:data_service 的写资格要覆盖全部存档库,没有闸门表的库注册不了。cmd/migrate 一次迁一个库,-storage-id 必填。
function Invoke-LocalSqlBootDbMigration {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$WorkDirectory)
    $tool = Resolve-LocalSqlBootTool -RepoRoot $RepoRoot -Name 'db_migrate'
    $config = Join-Path $WorkDirectory 'db.yaml'
    $source = [IO.File]::ReadAllText((Get-LocalSqlBootSourceConfigPath -RepoRoot $RepoRoot -Service 'db'))
    Write-LocalSqlBootText -Path $config -Text (ConvertTo-LocalSqlBootConfig -Settings $Settings -Service 'db' -Content $source -User $Settings.RootUser -Password $Settings.RootPassword)
    $workingDirectory = [IO.Path]::GetFullPath((Join-Path $RepoRoot 'go/db'))
    foreach ($database in @($Settings.PlayerDatabases)) {
        $arguments = @('-f', $config, '-command', 'up', '-storage-id', (Get-LocalSqlBootStorageId -Database $database))
        $result = Invoke-LocalSqlBootProcess -FilePath $tool -Arguments $arguments -Environment @{ DB_ALLOWED_DATABASES = ($Settings.PlayerDatabases -join ',') } -WorkingDirectory $workingDirectory -TimeoutSeconds 600
        if ($result.Code -eq 4) {
            Write-Warning "go/db migrate of $database reported drift that needs manual review: $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Out))"
        } elseif ($result.Code -ne 0) {
            throw "go/db migrate of $database failed (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err + [string]$result.Out))"
        }
    }
}

function Invoke-LocalSqlBootGlobalSchema {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$WorkDirectory)
    $inspection = Get-LocalSqlBootGlobalSchemaState -Settings $Settings
    switch -CaseSensitive ($inspection.State) {
        'fresh' { Invoke-LocalSqlBootServiceMigration -Settings $Settings -RepoRoot $RepoRoot -Service 'data_service' -WorkDirectory $WorkDirectory }
        'ready' { Write-LocalSqlBootLog "$($Settings.GlobalSchema) already has the authority tables; data_service -migrate skipped (it only runs on a fresh schema)." }
        default {
            throw "$($Settings.GlobalSchema) has tables but lacks the authority set (missing: $($inspection.Missing -join ', ')). data_service -migrate only runs on a fresh schema and this script never drops data; follow the reset steps in tools/scripts/README.md (start_game.ps1), then rerun."
        }
    }
}

# 建表逐服务做。请求启动的可选服务失败:告警并跳过;必需服务失败:中止;没请求启动的服务(mail / rank 常因缺签名密钥不起)
# 失败只告警,它们的表能建就建,data_service 的跨域探针才看得到。返回被跳过的服务名。
function Invoke-LocalSqlBootMigrations {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [string[]]$Requested = @(), [Parameter(Mandatory)][string]$WorkDirectory)
    $optional = @(Get-LocalSqlBootOptionalServices)
    $skipped = [Collections.Generic.List[string]]::new()
    foreach ($service in @(Get-LocalSqlBootWriterServices)) {
        if ($service -cin @('login', 'gateway')) { continue }
        try {
            if ($service -ceq 'db') { Invoke-LocalSqlBootDbMigration -Settings $Settings -RepoRoot $RepoRoot -WorkDirectory $WorkDirectory }
            elseif ($service -ceq 'data_service') { Invoke-LocalSqlBootGlobalSchema -Settings $Settings -RepoRoot $RepoRoot -WorkDirectory $WorkDirectory }
            else { Invoke-LocalSqlBootServiceMigration -Settings $Settings -RepoRoot $RepoRoot -Service $service -WorkDirectory $WorkDirectory }
            Write-LocalSqlBootLog "schema ready: $service"
        } catch {
            $wanted = $service -cin $Requested
            if ($wanted -and $service -cnotin $optional) { throw }
            if ($wanted) { $skipped.Add($service) }
            Write-Warning "Schema step for $service failed; it will not be started by this catalog: $($_.Exception.Message)"
        }
    }
    return $skipped.ToArray()
}

# 每个写者服务要写的逻辑库:闸门跟着库走,按库各注册一份。
# login 没有自己的库,它的目录条目只为领每 boot 配置,挂在网关那个库上。go/db 写本地域全部存档库,每个都必须已经由 cmd/migrate 建好闸门表。
# data_service 恰好是目录各片、流水库和全部存档库(Config.WriterSchemas),多一份少一份都拒绝启动。
function Get-LocalSqlBootWriterDatabases {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Service, [Parameter(Mandatory)][hashtable]$TablesBySchema)
    switch -CaseSensitive ($Service) {
        'data_service' {
            foreach ($database in @($Settings.PlayerDatabases)) {
                if (@($TablesBySchema[$database]) -cnotcontains 'route_meta') { throw "$database has no writer gate tables; go/db cmd/migrate has not completed." }
            }
            return @($Settings.DirectoryShards) + @($Settings.LedgerSchema) + @($Settings.PlayerDatabases)
        }
        'gateway' { return @($(if ($Settings.PSObject.Properties['JavaDatabases']) { $Settings.JavaDatabases.gateway } else { 'mmorpg' })) }
        'mall' { return @($Settings.JavaDatabases.mall) }
        'login' { return @('mmorpg') }
        'db' {
            # 与 data_service 同一份存档库清单;A17c 起没有「本 zone 库」,没迁完的库 go/db 也写不了,一个都不能缺。
            foreach ($database in @($Settings.PlayerDatabases)) {
                if (@($TablesBySchema[$database]) -cnotcontains 'route_meta') { throw "$database has no writer gate tables; go/db cmd/migrate has not completed." }
            }
            return @($Settings.PlayerDatabases)
        }
    }
    return @("mmorpg_$Service")
}

# 读一个逻辑库的闸门元行。schema 账号的 -migrate 种的行,没有就是迁移没跑完。
function Get-LocalSqlBootRouteMeta {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$Database)
    $schema = ConvertTo-AuthoritySqlIdentifier $Database
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT generation,writable,writer_epoch,LENGTH(freeze_run_id) FROM $schema.``route_meta`` WHERE id=1;" -Label "read route_meta of $Database")
    if ($rows.Count -ne 1) { throw "route_meta of $Database has no seed row; its schema migration has not completed." }
    $fields = $rows[0].Split("`t")
    $epoch = [uint64]0
    if ($fields.Count -ne 4 -or $fields[0] -cnotmatch '^[0-9a-fA-F]{1,32}$' -or -not [uint64]::TryParse($fields[2], [ref]$epoch) -or $epoch -eq 0) { throw 'route_meta seed row has an unexpected shape.' }
    # 冻结或未释放的维护 run 里不能发普通授权,Register 也会拒绝;这里先报清楚。
    if ($fields[1] -cne '1' -or $fields[3] -cne '0') { throw "route_meta of $Database is frozen by a maintenance run; refusing to register writers." }
    return [pscustomobject]@{ Generation = $fields[0]; WriterEpoch = $epoch }
}

# 一批实例要写的全部逻辑库的闸门元行。
function Get-LocalSqlBootMetaByDatabase {
    param([Parameter(Mandatory)]$Settings, [string[]]$Databases = @())
    $meta = @{}
    foreach ($database in @($Databases | Select-Object -Unique)) { $meta[$database] = Get-LocalSqlBootRouteMeta -Settings $Settings -Database $database }
    return $meta
}

function Get-LocalSqlBootTablesBySchema {
    param([Parameter(Mandatory)]$Settings)
    $names = ($Settings.LogicalDatabases | ForEach-Object { "'$_'" }) -join ','
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT table_schema,table_name FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema IN ($names);" -Label 'list business tables')
    $tables = @{}
    foreach ($row in $rows) {
        $fields = $row.Split("`t")
        if ($fields.Count -ne 2) { throw 'Unexpected row from information_schema.tables.' }
        if (-not $tables.ContainsKey($fields[0])) { $tables[$fields[0]] = [Collections.Generic.List[string]]::new() }
        $tables[$fields[0]].Add($fields[1])
    }
    return $tables
}

# 每个写者能碰什么:只有自己的库的 DML,不跨库。唯一的例外是 data_service 在存档库的那部分:
# 建角、判死、过户证明写 player_store,空路由启动判据读 player_centre_database 和 user_accounts(route_bootstrap.go)。模板要求逐表 GRANT,没有 schema.*。
function Get-LocalSqlBootGrantManifest {
    param([Parameter(Mandatory)]$Settings)
    $read = @('SELECT')
    $write = @('SELECT', 'INSERT', 'UPDATE', 'DELETE')
    $dataServiceCross = [Collections.Generic.List[object]]::new()
    foreach ($schema in $Settings.PlayerDatabases) {
        $dataServiceCross.Add(@{ Schema = $schema; Tables = @('player_store'); Operations = $write })
        $dataServiceCross.Add(@{ Schema = $schema; Tables = @('player_centre_database', 'user_accounts'); Operations = $read })
    }
    return @{
        data_service = @{ Own = @(@($Settings.DirectoryShards) + @($Settings.LedgerSchema)); Cross = $dataServiceCross.ToArray() }
        db = @{ Own = @($Settings.PlayerDatabases); Cross = @() }
        login = @{ Own = @(); Cross = @() }
        gateway = @{ Own = @($(if ($Settings.PSObject.Properties['JavaDatabases']) { $Settings.JavaDatabases.gateway } else { 'mmorpg' })); Cross = @() }
        mall = @{ Own = @($(if ($Settings.PSObject.Properties['JavaDatabases']) { $Settings.JavaDatabases.mall } else { 'mmorpg_mall' })); Cross = @() }
        chat = @{ Own = @('mmorpg_chat'); Cross = @() }
        friend = @{ Own = @('mmorpg_friend'); Cross = @() }
        guild = @{ Own = @('mmorpg_guild'); Cross = @() }
        mail = @{ Own = @('mmorpg_mail'); Cross = @() }
        rank = @{ Own = @('mmorpg_rank'); Cross = @() }
        match = @{ Own = @('mmorpg_match'); Cross = @() }
        trade = @{ Own = @('mmorpg_trade'); Cross = @() }
        player_inbox = @{ Own = @('mmorpg_player_inbox'); Cross = @() }
    }
}

# 把清单展开成 renderer 要的逐表条目。只授已存在的表(MySQL 拒绝给不存在的表做表级 GRANT);
# schema_migrations 只读:常驻的 Plan 看不见台账就判基线未跑(schemamigrate/runner.go),但不该写它。
function Get-LocalSqlBootInstanceTables {
    param([Parameter(Mandatory)][hashtable]$Grant, [Parameter(Mandatory)][hashtable]$TablesBySchema)
    $authority = @(Get-LocalSqlBootAuthorityTables)
    $merged = [ordered]@{}
    foreach ($schema in @($Grant.Own)) {
        foreach ($table in @($TablesBySchema[$schema])) {
            if (-not $table -or $table -iin $authority) { continue }
            $operations = if ($table -ceq 'schema_migrations') { @('SELECT') } else { @('SELECT', 'INSERT', 'UPDATE', 'DELETE') }
            $merged["$schema.$table"] = @{ schema = $schema; table = $table; operations = @($operations) }
        }
    }
    foreach ($cross in @($Grant.Cross)) {
        $present = @($TablesBySchema[$cross.Schema])
        $wanted = if ($cross.ContainsKey('Tables')) { @($cross.Tables) } else { $present }
        foreach ($table in $wanted) {
            if (-not $table -or $table -iin $authority -or $table -ceq 'schema_migrations' -or $present -cnotcontains $table) { continue }
            $key = "$($cross.Schema).$table"
            if (-not $merged.Contains($key)) { $merged[$key] = @{ schema = $cross.Schema; table = $table; operations = @($cross.Operations) } }
        }
    }
    return @($merged.Values)
}

# 物理 authority 在 K8s 上是独立 PD 身份原件的摘要;本机只有一个回环实例,用一份本机原件固定下来,反复启动不变。
function Get-LocalSqlBootPhysicalAuthority {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$PrivateRoot)
    if ($Settings.PSObject.Properties['QualifiedPhysicalSha256']) { return $Settings.QualifiedPhysicalSha256 }
    $path = Join-Path $PrivateRoot 'physical-authority.original'
    if (-not (Test-Path -LiteralPath $path)) {
        Write-LocalSqlBootText -Path $path -Text "LOCAL_SQL_AUTHORITY_V1`nendpoint=$($Settings.Endpoint)`nnonce=$(New-LocalSqlBootSecret)`n"
    }
    return Get-MaintenanceSha256 -Path $path
}

# 本机声明,不是真实 I3 报告:writer_authority 只核它的 SHA 与 Request 一致。锁原语按档位定,生产的资格仍由操作者提供。
function Get-LocalSqlBootLockQualification {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$PrivateRoot)
    if ($Settings.PSObject.Properties['QualifiedLockFile']) { return $Settings.QualifiedLockFile }
    $path = Join-Path $PrivateRoot "lock-qualification.$($Settings.Backend).$($Settings.LockPrimitive).json"
    if (-not (Test-Path -LiteralPath $path)) {
        $declaration = [ordered]@{ format = 'LOCAL_DEV_LOCK_QUALIFICATION_V1'; backend = $Settings.Backend; lockPrimitive = $Settings.LockPrimitive; scope = 'Loopback development database only. This is not a production I3 qualification report.' }
        Write-LocalSqlBootText -Path $path -Text (ConvertTo-Json -InputObject $declaration -Compress)
    }
    return $path
}

# 注册与维护账号口令本机随机生成一次并复用;schema 身份本机就是 root(与 go_services.ps1 的本机建表一致),不另建。
function Get-LocalSqlBootOperators {
    param([Parameter(Mandatory)][string]$PrivateRoot)
    $path = Join-Path $PrivateRoot 'operators.json'
    if (Test-Path -LiteralPath $path) { return (Get-Content -LiteralPath $path -Raw | ConvertFrom-Json) }
    $operators = [pscustomobject]@{
        registrar = [pscustomobject]@{ user = 'mmorpg_local_registrar'; password = (New-LocalSqlBootSecret); principal = 'mmorpg_local_registrar@%' }
        maintenance = [pscustomobject]@{ user = 'mmorpg_local_maintenance'; password = (New-LocalSqlBootSecret); principal = 'mmorpg_local_maintenance@%' }
    }
    Write-LocalSqlBootText -Path $path -Text (ConvertTo-Json -InputObject $operators -Depth 4)
    return $operators
}

function New-LocalSqlBootAccountName {
    param([Parameter(Mandatory)][string]$BootId, [Parameter(Mandatory)][string]$InstanceKey)
    $name = "lw_${BootId}_$($InstanceKey.ToLowerInvariant() -replace '[^a-z0-9_]', '_')"
    if ($name.Length -gt 32) { $name = $name.Substring(0, 32) }
    return $name
}

# 为一个实例备齐注册原件:私有配置、环境值、REGISTER 模板与单槽位名册(SQL_WRITER_BOOT_ROSTER_V2)。不碰数据库。
# 本机每次启动都建新账号,一个账号就是一个槽位,所以本机的槽位只有第一代(K8s 上同一槽位每次启动换代,见
# docs/design/writer-roster-exhaustion-20261106.md §5)。模板不带身份:writer_key、instance、request_id 由 writer_authority 现场生成。
# 实例要写的每个逻辑库一份模板,账号相同;writer_authority 把这一代注册在各自的库,grant 写成一个集合。
function New-LocalSqlBootInstanceFiles {
    param(
        [Parameter(Mandatory)]$Settings,
        [Parameter(Mandatory)][string]$RepoRoot,
        [Parameter(Mandatory)]$Instance,
        [Parameter(Mandatory)][string]$BootRoot,
        [Parameter(Mandatory)][string]$BootId,
        [Parameter(Mandatory)][string[]]$Databases,
        [Parameter(Mandatory)][hashtable]$MetaByDatabase,
        [Parameter(Mandatory)][string]$PhysicalSha256,
        [Parameter(Mandatory)][string]$QualificationFile,
        $FixtureIo = $null
    )
    $key = $Instance.InstanceKey
    $service = $Instance.Service
    $user = New-LocalSqlBootAccountName -BootId $BootId -InstanceKey $key
    $password = New-LocalSqlBootSecret
    $principal = "${user}@%"
    $instanceRoot = Join-Path $BootRoot $key
    $inputRoot = Join-Path $instanceRoot 'input'
    $grantDirectory = Join-Path $instanceRoot 'grant'
    $runtimeDirectory = Join-Path $instanceRoot 'runtime'
    $environmentDirectory = Join-Path $instanceRoot 'env'
    $consumeDirectory = Join-Path $instanceRoot 'consume'
    foreach ($directory in @($inputRoot, $grantDirectory, $runtimeDirectory, $environmentDirectory, $consumeDirectory)) { $null = [IO.Directory]::CreateDirectory($directory) }

    $configText = if ($Instance.PSObject.Properties['Java'] -and $Instance.Java) {
        New-LocalSqlBootJavaConfig -Settings $Settings -Instance $Instance -User $user -Password $password
    } elseif ($service -ceq 'gateway') {
        New-LocalSqlBootGatewayConfig -Settings $Settings -RepoRoot $RepoRoot -User $user -Password $password
    } else {
        ConvertTo-LocalSqlBootConfig -Settings $Settings -Service $service -Content ([IO.File]::ReadAllText($Instance.ConfigPath)) -User $user -Password $password
    }
    $inputConfig = Join-Path $inputRoot 'runtime.yaml'
    if ($null -ne $FixtureIo) { $null = Write-LocalJavaFixtureFile $FixtureIo $inputConfig $configText }
    else { Write-LocalSqlBootText -Path $inputConfig -Text $configText }

    $environmentEntries = [Collections.Generic.List[object]]::new()
    $environmentNames = [Collections.Generic.List[string]]::new()
    if ($service -cnotin @('gateway','mall')) {
        $runtimeEnvironment = Get-LocalSqlBootRuntimeEnvironment -Settings $Settings -Service $service -User $user -Password $password
        foreach ($name in @($runtimeEnvironment.Keys)) {
            $valueFile = Join-Path $inputRoot "env-$name"
            # writer_authority 按原字节核:不能有换行。
            [IO.File]::WriteAllBytes($valueFile, [Text.UTF8Encoding]::new($false).GetBytes([string]$runtimeEnvironment[$name]))
            $environmentEntries.Add([ordered]@{ name = $name; valuePath = "env-$name"; valueSha256 = (ConvertTo-LocalSqlBootBase64 -Hex (Get-MaintenanceSha256 -Path $valueFile)) })
            $environmentNames.Add($name)
        }
    }

    $qualificationCopy = Join-Path $inputRoot 'lock-qualification.original'
    if ($null -ne $FixtureIo) {
        # 资格原件是公开pin；新副本仍经同一受信私密IO封存，不读取旧DSN/owner/receipt。
        $qualificationText=[Text.UTF8Encoding]::new($false,$true).GetString([IO.File]::ReadAllBytes($QualificationFile))
        $null = Write-LocalJavaFixtureFile $FixtureIo $qualificationCopy $qualificationText
    } else { [IO.File]::Copy($QualificationFile, $qualificationCopy) }
    $qualificationSha = if ($null -ne $FixtureIo) { (Read-LocalJavaFixtureFile $FixtureIo $qualificationCopy).Sha256 } else { Get-MaintenanceSha256 $qualificationCopy }
    $inputConfigSha = if ($null -ne $FixtureIo) { (Read-LocalJavaFixtureFile $FixtureIo $inputConfig).Sha256 } else { Get-MaintenanceSha256 $inputConfig }
    $deploymentId = "local-$BootId-$key"
    $requests = [Collections.Generic.List[object]]::new()
    $registrations = [Collections.Generic.List[object]]::new()
    foreach ($database in $Databases) {
        $meta = $MetaByDatabase[$database]
        $request = [ordered]@{
            kind = 'REGISTER'
            writer = [ordered]@{
                generation = $meta.Generation
                sqlPrincipal = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($principal))
                state = 'ACTIVE'
            }
            authoritySchema = $database
            protectedSchemas = [string[]]@($database)
            deploymentDigest = (ConvertTo-LocalSqlBootBase64 -Hex $PhysicalSha256)
            lockPrimitive = $Settings.LockPrimitive
            lockQualificationSha256 = (ConvertTo-LocalSqlBootBase64 -Hex $qualificationSha)
        }
        $requestFile = Join-Path $inputRoot "request.$database.json"
        $requestSha256 = if ($null -ne $FixtureIo) { Write-LocalJavaFixtureJson $FixtureIo $requestFile $request } else { Write-MaintenanceImmutableJson -Path $requestFile -Value $request }
        $requests.Add($request)
        $registrations.Add([pscustomobject]@{ Database = $database; RequestFile = $requestFile; RequestSha256 = $requestSha256 })
    }
    $entry = [ordered]@{
        requests = [object[]]$requests.ToArray()
        runtimeConfigPath = 'runtime.yaml'
        runtimeConfigSha256 = (ConvertTo-LocalSqlBootBase64 -Hex $inputConfigSha)
    }
    if ($environmentEntries.Count -gt 0) { $entry['runtimeEnvironment'] = [object[]]$environmentEntries.ToArray() }
    $rosterFile = Join-Path $inputRoot 'roster.json'
    $roster = [ordered]@{ format = 'SQL_WRITER_BOOT_ROSTER_V2'; entries = [object[]]@($entry) }
    $rosterSha256 = if ($null -ne $FixtureIo) { Write-LocalJavaFixtureJson $FixtureIo $rosterFile $roster } else { Write-MaintenanceImmutableJson -Path $rosterFile -Value $roster }

    return [pscustomobject]@{
        InstanceKey = $key; Service = $service; User = $user; Password = $password; Principal = $principal
        InputRoot = $inputRoot; RosterFile = $rosterFile; RosterSha256 = $rosterSha256
        Registrations = [object[]]$registrations.ToArray(); QualificationFile = $qualificationCopy
        GrantFile = (Join-Path $grantDirectory 'boot-grant.json'); RuntimeFile = (Join-Path $runtimeDirectory "$key.yaml")
        EnvironmentDirectory = $environmentDirectory; EnvironmentNames = [string[]]$environmentNames.ToArray()
        ConsumeDirectory = $consumeDirectory; DeploymentId = $deploymentId
    }
}

# 角色文本只出自正式 renderer,这里只负责拼 CREATE USER(模板明确不含账号创建)。schema 身份是 root,不再给 root 授权。
function New-LocalSqlBootAccountSql {
    param(
        [Parameter(Mandatory)]$Settings,
        [Parameter(Mandatory)][string]$RepoRoot,
        [Parameter(Mandatory)]$Operators,
        [Parameter(Mandatory)][object[]]$Prepared,
        [Parameter(Mandatory)][hashtable]$TablesBySchema,
        [Parameter(Mandatory)][hashtable]$Manifest,
        [switch]$FreshAccountsOnly,
        [switch]$SkipOperatorCreation
    )
    $templateRoot = Join-Path $RepoRoot 'deploy/sql'
    $rootAccount = ConvertTo-AuthoritySqlAccount $Settings.RootPrincipal
    $builder = [Text.StringBuilder]::new()
    foreach ($operator in @($Operators.registrar, $Operators.maintenance)) {
        if ($SkipOperatorCreation) { continue }
        $account = ConvertTo-AuthoritySqlAccount $operator.principal
        if ($FreshAccountsOnly) { $null = $builder.AppendLine("CREATE USER $account IDENTIFIED BY '$($operator.password)';") }
        else {
            $null = $builder.AppendLine("CREATE USER IF NOT EXISTS $account IDENTIFIED BY '$($operator.password)';")
            $null = $builder.AppendLine("ALTER USER $account IDENTIFIED BY '$($operator.password)';")
        }
    }
    # 注册、维护账号按逻辑库授权,每个库一遍。闸门表的权限也按库:写者只看得见自己要写的库的闸门。
    $operatorDone = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($item in $Prepared) {
        $tables = @(Get-LocalSqlBootInstanceTables -Grant $Manifest[$item.Service] -TablesBySchema $TablesBySchema)
        $ifNew = if ($FreshAccountsOnly) { '' } else { 'IF NOT EXISTS ' }
        $null = $builder.AppendLine("CREATE USER $ifNew$(ConvertTo-AuthoritySqlAccount $item.Principal) IDENTIFIED BY '$($item.Password)';")
        foreach ($registration in @($item.Registrations)) {
            $roles = ConvertTo-AuthorityRoleSql -TemplateRoot $templateRoot -Plan @{
                authoritySchema = $registration.Database
                directorySchema = $Settings.DirectorySchema
                directoryShard = ($registration.Database -cin @($Settings.DirectoryShards))
                runtimePrincipal = $item.Principal
                registrarPrincipal = $Operators.registrar.principal
                maintenancePrincipal = $Operators.maintenance.principal
                schemaPrincipal = $Settings.RootPrincipal
                registrationRequestFile = $registration.RequestFile
                registrationRequestSha256 = $registration.RequestSha256
                lockQualificationFile = $item.QualificationFile
                tables = $tables
                schemaDatabases = @($registration.Database)
            }
            if ($operatorDone.Add($registration.Database)) {
                foreach ($line in ($roles.operator -split '\r?\n')) {
                    if (-not $line.EndsWith(" TO $rootAccount;")) { $null = $builder.AppendLine($line) }
                }
            }
            $null = $builder.AppendLine($roles.runtime)
        }
    }
    return $builder.ToString()
}

# writer_authority register 的控制环境。只给注册子进程,不进常驻进程。
function New-LocalSqlBootControlEnvironment {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)]$Operators, [Parameter(Mandatory)][string]$PhysicalSha256)
    return @{
        MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256 = $PhysicalSha256
        MMORPG_SQL_OPERATOR_PRINCIPAL = $Operators.registrar.principal
        MMORPG_SQL_REGISTRAR_DSN = (New-LocalSqlBootDsn -User $Operators.registrar.user -Password $Operators.registrar.password -Endpoint $Settings.Endpoint -Database $Settings.DirectorySchema)
        MMORPG_SQL_BACKEND = $Settings.Backend
    }
}

# 正式注册:每实例一次,单槽位名册,本实例就是槽位 0(Pod 名按 StatefulSet 的写法 <实例>-0)。
# 控制凭据只在这一个子进程的环境里;常驻进程拿不到(Enter-SqlWriterBootEnvironment 也会清)。
function Register-LocalSqlBootInstance {
    param([Parameter(Mandatory)]$Item, [Parameter(Mandatory)][string]$Tool, [Parameter(Mandatory)][hashtable]$ControlEnvironment, [Parameter(Mandatory)][string]$PhysicalSha256, [switch]$PreserveInputs, $FixtureIo = $null)
    $operationSeconds=if ($null -ne $FixtureIo) { Get-LocalJavaFixtureOperationSeconds $FixtureIo 30 } else { 30 }
    $arguments = @(
        '-command', 'register'
        '-roster', $Item.RosterFile
        '-roster-sha256', $Item.RosterSha256
        '-input-root', $Item.InputRoot
        '-lock-qualification', $Item.QualificationFile
        '-runtime-config-output', $Item.RuntimeFile
        '-boot-output', $Item.GrantFile
        '-deployment-id', $Item.DeploymentId
        '-pod-name', "$($Item.InstanceKey)-0"
        '-timeout', "${operationSeconds}s"
    )
    if ($Item.EnvironmentNames.Count -gt 0) { $arguments += @('-runtime-env-output', $Item.EnvironmentDirectory) }
    $processArguments=@{FilePath=$Tool;Arguments=$arguments;Environment=$ControlEnvironment;TimeoutSeconds=60}
    if ($null -ne $FixtureIo) {
        $processArguments.RequireCompleteExit=$true; $processArguments.TimeoutSeconds=$operationSeconds+2
        $processArguments.FixtureDiagnostic=$FixtureIo.Diagnostic; $processArguments.FixtureOperation='register'
    }
    $result = Invoke-LocalSqlBootProcess @processArguments
    if ($result.Code -ne 0) { throw "writer_authority register failed for $($Item.InstanceKey) (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err))" }
    $grantSha256 = if ($null -ne $FixtureIo) { (Read-LocalJavaFixtureFile $FixtureIo $Item.GrantFile).Sha256 } else { Get-MaintenanceSha256 -Path $Item.GrantFile }
    $sidecar = if ($null -ne $FixtureIo) { (Read-LocalJavaFixtureFile $FixtureIo "$($Item.GrantFile).sha256").Text } else { [IO.File]::ReadAllText("$($Item.GrantFile).sha256") }
    if ($sidecar.Trim() -cne $grantSha256) { throw "Grant digest mismatch for $($Item.InstanceKey)." }
    $environmentEntries = @(foreach ($name in $Item.EnvironmentNames) {
            $valueFile = Join-Path $Item.EnvironmentDirectory $name
            [ordered]@{ name = $name; valueFile = $valueFile; valueSha256 = (Get-MaintenanceSha256 -Path $valueFile) }
        })
    # 输入里有明文口令,选中副本已写出,不再留第二份。
    if (-not $PreserveInputs) { Remove-Item -LiteralPath $Item.InputRoot -Recurse -Force }
    return [ordered]@{
        grantFile = $Item.GrantFile
        grantSha256 = $grantSha256
        consumeDir = $Item.ConsumeDirectory
        deploymentId = $Item.DeploymentId
        physicalAuthoritySha256 = $PhysicalSha256
        runtimeConfigFile = $Item.RuntimeFile
        runtimeConfigSha256 = $(if ($null -ne $FixtureIo) { (Read-LocalJavaFixtureFile $FixtureIo $Item.RuntimeFile).Sha256 } else { Get-MaintenanceSha256 -Path $Item.RuntimeFile })
        runtimeEnvironment = [object[]]$environmentEntries
    }
}

# 产物必须过得了正式闸门:用启动时那个函数把每个条目走一遍,不等到起服才发现。
function Assert-LocalSqlBootCatalog {
    param([Parameter(Mandatory)][string]$CatalogPath, [Parameter(Mandatory)][string]$RepoRoot, [object[]]$Prepared = @())
    $saved = [Environment]::GetEnvironmentVariable('MMORPG_SQL_BOOT_CATALOG', 'Process')
    try {
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_BOOT_CATALOG', $CatalogPath, 'Process')
        foreach ($item in $Prepared) {
            $selected = ''
            $null = Get-SqlWriterBootEnvironment -InstanceKey $item.InstanceKey -RepoRoot $RepoRoot -ServiceName $item.Service -SelectedRuntimeConfigFile ([ref]$selected)
        }
    } finally {
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_BOOT_CATALOG', $saved, 'Process')
    }
}

function Test-LocalSqlBootGatewayRunning {
    param([Parameter(Mandatory)][string]$RepoRoot)
    $jarRoot = [IO.Path]::GetFullPath((Join-Path $RepoRoot 'java/gateway_node/target'))
    $running = @(Get-CimInstance Win32_Process -Filter "Name = 'java.exe'" -ErrorAction Stop | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($jarRoot) })
    return $running.Count -gt 0
}

# 实例清单问 go_services.ps1:端口避让、派生配置和「已在运行」的判定都只有它那一份。结果写文件:子进程 stdout 混着 Write-Host。
function Get-LocalSqlBootInstancePlan {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [string[]]$Requested = @(), [Parameter(Mandatory)][string]$WorkDirectory)
    $instances = [Collections.Generic.List[object]]::new()
    $goServices = @($Requested | Where-Object { $_ -cne 'gateway' })
    if ($goServices.Count -gt 0) {
        $planFile = Join-Path $WorkDirectory 'instance-plan.json'
        $arguments = @('-NoProfile', '-NonInteractive', '-File', ([IO.Path]::GetFullPath((Join-Path $RepoRoot 'tools/scripts/go_services.ps1'))), '-Command', 'plan-instances', '-Services', ($goServices -join ','), '-InstancePlanOutput', $planFile)
        if ($Settings.Backend -ceq 'mysql') { $arguments += '-LocalMySQL' }
        $result = Invoke-LocalSqlBootProcess -FilePath (Join-Path $PSHOME 'pwsh.exe') -Arguments $arguments -TimeoutSeconds 120
        if ($result.Code -ne 0) { throw "go_services plan-instances failed (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err + [string]$result.Out))" }
        foreach ($item in @(Get-Content -LiteralPath $planFile -Raw | ConvertFrom-Json)) { $instances.Add($item) }
    }
    if ('gateway' -cin $Requested) {
        $instances.Add([pscustomobject]@{ InstanceKey = 'gateway'; Service = 'gateway'; Running = (Test-LocalSqlBootGatewayRunning -RepoRoot $RepoRoot); Port = 0; ConfigPath = '' })
    }
    return $instances.ToArray()
}

# Java 的 WriterAuthority 在 Windows 上经 private_control_file 读写私有文件,并要求 exe 的整条路径都不能被非受信主体替换。
# 仓库目录及 LocalAppData 的祖先可能允许其他主体修改。将 helper 放在用户配置目录下的专用私有目录。
function Get-LocalSqlBootPrivateFileHelperDirectory {
    $profileRoot = [Environment]::GetFolderPath('UserProfile')
    return [IO.Path]::GetFullPath((Join-Path $profileRoot '.mmorpg/sql-boot-local'))
}

function Install-LocalSqlBootPrivateFileHelper {
    param([Parameter(Mandatory)][string]$RepoRoot)
    $source = Resolve-LocalSqlBootTool -RepoRoot $RepoRoot -Name 'private_control_file'
    $directory = Get-LocalSqlBootPrivateFileHelperDirectory
    Initialize-LocalSqlBootPrivateDirectory -Path $directory
    $target = Join-Path $directory 'private_control_file.exe'
    $existing = Get-Item -LiteralPath $target -ErrorAction SilentlyContinue
    if ($existing -and ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint))) {
        throw 'Java 私有文件 helper 路径不是普通文件。'
    }
    $sha256 = Get-MaintenanceSha256 -Path $source
    if (-not (Test-Path -LiteralPath $target -PathType Leaf) -or (Get-MaintenanceSha256 -Path $target) -cne $sha256) { Copy-Item -LiteralPath $source -Destination $target -Force }
    if ((Get-MaintenanceSha256 -Path $target) -cne $sha256) { throw 'Java 私有文件 helper 复制后摘要不符。' }
    return [pscustomobject]@{ path = $target; sha256 = $sha256 }
}

# 上一轮的账号:实例已不在跑就删,还在跑的留着(它的连接池还要重连)。没进本次清单的实例不动。
function Get-LocalSqlBootStaleAccounts {
    param([object[]]$Accounts = @(), [object[]]$Instances = @())
    $running = @($Instances | Where-Object { $_.Running } | ForEach-Object { $_.InstanceKey })
    $planned = @($Instances | ForEach-Object { $_.InstanceKey })
    $drop = @($Accounts | Where-Object { $_.instanceKey -cin $planned -and $_.instanceKey -cnotin $running })
    $keep = @($Accounts | Where-Object { -not ($_.instanceKey -cin $planned -and $_.instanceKey -cnotin $running) })
    return [pscustomobject]@{ Drop = $drop; Keep = $keep }
}

# -Counts 的两种形状(会话内 hashtable、pwsh -File 的字符串)统一成 plan-instances 认的 'name=N,name=N'。
function ConvertTo-LocalSqlBootCountsArgument {
    param($Counts)
    if ($null -eq $Counts) { return '' }
    if ($Counts -is [Collections.IDictionary]) { return (@($Counts.Keys | Sort-Object | ForEach-Object { "$_=$([int]$Counts[$_])" }) -join ',') }
    return (@($Counts | ForEach-Object { ([string]$_) -split '[,;]' } | Where-Object { $_ -ne '' }) -join ',')
}

# '990017=0,990018=1000' 解析成区号到偏移。要么都不给,要么每个区都给,不留一半按公式、一半按显式。
function ConvertTo-LocalSqlBootZoneOffsets {
    param([string[]]$Raw = @(), [int[]]$Zones = @())
    $offsets = @{}
    foreach ($item in @($Raw | ForEach-Object { ([string]$_) -split '[,;]' } | Where-Object { $_ -ne '' })) {
        $match = [regex]::Match($item.Trim(), '\A(?<zone>[1-9][0-9]*)=(?<offset>[0-9]+)\z')
        if (-not $match.Success) { throw "Invalid -ZonePortOffsets entry '$item'. Expected 'zone=offset'." }
        $zone = [int]$match.Groups['zone'].Value
        if ($zone -notin $Zones) { throw "-ZonePortOffsets names zone $zone, which is not in -Zones." }
        if ($offsets.ContainsKey($zone)) { throw "-ZonePortOffsets names zone $zone twice." }
        $offsets[$zone] = [int]$match.Groups['offset'].Value
    }
    if ($offsets.Count -gt 0 -and $offsets.Count -ne $Zones.Count) { throw '-ZonePortOffsets must cover every zone in -Zones or none of them.' }
    return $offsets
}

# 多区实例清单:每区问一次 go_services plan-instances,各写各的文件。这里只合并,任何一区失败都整批失败。
function Get-LocalSqlBootMultiZonePlan {
    param(
        [Parameter(Mandatory)]$Settings,
        [Parameter(Mandatory)][string]$RepoRoot,
        [Parameter(Mandatory)][string[]]$Services,
        [Parameter(Mandatory)][int[]]$Zones,
        [string]$Counts = '',
        [int]$PortStride = 1,
        [int]$ZonePortShift = 1000,
        [hashtable]$ZonePortOffsets = @{},
        [Parameter(Mandatory)][string]$WorkDirectory
    )
    $instances = [Collections.Generic.List[object]]::new()
    $launcher = [IO.Path]::GetFullPath((Join-Path $RepoRoot 'tools/scripts/go_services.ps1'))
    foreach ($zone in $Zones) {
        $planFile = Join-Path $WorkDirectory "instance-plan.z$zone.json"
        $arguments = @('-NoProfile', '-NonInteractive', '-File', $launcher, '-Command', 'plan-instances', '-Services', ($Services -join ','), '-InstancePlanOutput', $planFile, '-Zone', [string]$zone, '-ZonePortShift', [string]$ZonePortShift, '-PortStride', [string]$PortStride)
        if ($Counts) { $arguments += @('-Counts', $Counts) }
        if ($ZonePortOffsets.ContainsKey($zone)) { $arguments += @('-ZonePortOffset', [string]$ZonePortOffsets[$zone]) }
        if ($Settings.Backend -ceq 'mysql') { $arguments += '-LocalMySQL' }
        $result = Invoke-LocalSqlBootProcess -FilePath (Join-Path $PSHOME 'pwsh.exe') -Arguments $arguments -TimeoutSeconds 120
        if ($result.Code -ne 0) { throw "go_services plan-instances failed for zone $zone (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err + [string]$result.Out))" }
        foreach ($item in @(Get-Content -LiteralPath $planFile -Raw | ConvertFrom-Json)) {
            $item | Add-Member -NotePropertyName Zone -NotePropertyValue $zone
            $instances.Add($item)
        }
    }
    return $instances.ToArray()
}

# 合并后的计划先过这一关才碰数据库。运行中的实例身份已由 go_services 按 PID、可执行文件和配置核过,这里只核键和端口。
# 新账号名由 boot 号和实例键截断而来,多副本可能截成同一个:同样在建库前拒绝。
function Assert-LocalSqlBootMergedPlan {
    param([Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][object[]]$Instances, [Parameter(Mandatory)][string]$BootId)
    $writers = @(Get-LocalSqlBootWriterServices | Where-Object { $_ -cne 'gateway' })
    $known = $writers + @(Get-LocalSqlBootNonWriterServices) + @('gateway','mall')
    $derivedRoot = [IO.Path]::GetFullPath((Join-Path $RepoRoot 'run/etc/go_services')) + [IO.Path]::DirectorySeparatorChar
    $keys = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $ports = @{}
    $accounts = @{}
    foreach ($instance in $Instances) {
        $key = [string]$instance.InstanceKey
        if (-not $keys.Add($key)) { throw "Instance $key appears twice in the merged plan." }
        if ([string]$instance.Service -cnotin $known) { throw "Instance $key has an unknown service." }
        $claimed = [Collections.Generic.List[int]]::new()
        $claimed.Add([int]$instance.Port)
        if ($instance.PSObject.Properties['Java'] -and $instance.Java) {
            # Java源配置尚未含本boot端口；私有派生由New-LocalSqlBootJavaConfig一次生成。
            # 四角色的来源、产物SHA和本机地址已由Java计划纯校验固定。
            if ($instance.Service -cnotin @('gateway','mall') -or $instance.Running) { throw 'Java计划必须是尚未启动的独立boot。' }
            $account = New-LocalSqlBootAccountName -BootId $BootId -InstanceKey $key
            if ($accounts.ContainsKey($account)) { throw "Instances $($accounts[$account]) and $key map to the same SQL account name." }
            $accounts[$account] = $key
        } elseif (-not $instance.Running) {
            $config = [string]$instance.ConfigPath
            if (-not [IO.Path]::IsPathRooted($config) -or -not ([IO.Path]::GetFullPath($config)).StartsWith($derivedRoot, [StringComparison]::OrdinalIgnoreCase)) { throw "Instance $key has a config outside run/etc/go_services." }
            $listen = [string](Get-YamlScalar -Path $config -KeyPath 'ListenOn').Value
            if ($listen -notmatch ':(?<port>[0-9]+)\z' -or [int]$Matches.port -ne [int]$instance.Port) { throw "Instance ${key}: ListenOn in its config does not match the planned port $($instance.Port)." }
            if ((Get-InstanceConfigZoneId -ConfigPath $config) -ne [long]$instance.Zone) { throw "Instance ${key}: its config does not carry zone $($instance.Zone)." }
            $metrics = [string](Get-YamlScalar -Path $config -KeyPath 'MetricsListenAddr').Value
            if ($metrics -match ':(?<port>[0-9]+)\z') { $claimed.Add([int]$Matches.port) }
            if ([string]$instance.Service -cin $writers) {
                $account = New-LocalSqlBootAccountName -BootId $BootId -InstanceKey $key
                if ($accounts.ContainsKey($account)) { throw "Instances $($accounts[$account]) and $key map to the same SQL account name; reduce the instance count or shorten the key." }
                $accounts[$account] = $key
            }
        }
        foreach ($port in $claimed) {
            if ($ports.ContainsKey($port)) { throw "Instances $($ports[$port]) and $key both claim port $port." }
            $ports[$port] = $key
        }
    }
    # zone标签和loopback仅说明语法合法。必须再绑定同一合并计划里真实Go login
    # 的zone/端口，否则自定义offset或把两区本机端口互换仍会误连。
    $loginInstances = @($Instances | Where-Object { $_.Service -ceq 'login' })
    foreach ($gateway in @($Instances | Where-Object { $_.PSObject.Properties['Java'] -and $_.Java -and $_.Service -ceq 'gateway' })) {
        foreach ($mapping in ([string]$gateway.Settings.loginGrpcEndpoints -split ',')) {
            if ($mapping -cnotmatch '^(?<zone>[1-9][0-9]*)=(?<endpoint>127\.0\.0\.1:[0-9]+)$') { throw '网关Go login映射形状改变。' }
            $zone = [long]$Matches.zone; $endpoint = [string]$Matches.endpoint
            $expected = @($loginInstances | Where-Object { $_.PSObject.Properties['Zone'] -and [long]$_.Zone -eq $zone } | ForEach-Object { '127.0.0.1:' + [string]$_.Port })
            if ($expected.Count -eq 0 -or $endpoint -cnotin $expected) { throw '网关Go login地址不属于同zone的本轮合并实例计划；不执行DDL或注册。' }
        }
    }
}

function New-LocalSqlBootCatalog {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$RepoRoot,
        [string[]]$Services = @(),
        [string]$Endpoint = '',
        [ValidateSet('auto', 'mysql', 'tidb')][string]$Backend = 'auto',
        # 多区一次生成:一次给全部目标区,内部按区规划、合并校验,只发布一份目录。不能循环调用本函数,每次调用都会替换目录。
        [int[]]$Zones = @(),
        $Counts = @{},
        [int]$PortStride = 1,
        [int]$ZonePortShift = 1000,
        [string[]]$ZonePortOffsets = @(),
        [string]$JavaInstancePlanPath = '',
        [string]$JavaInstancePlanSha256 = '',
        [string]$PhysicalAuthorityFile = '',
        [string]$PhysicalAuthoritySha256 = '',
        [string]$LockQualificationFile = '',
        [string]$LockQualificationSha256 = ''
    )
    if (-not $IsWindows) { throw 'sql-boot-catalog-local only supports Windows hosts.' }
    $repo = [IO.Path]::GetFullPath($RepoRoot)
    $javaInstances = @()
    if ($JavaInstancePlanPath -or $JavaInstancePlanSha256) {
        if (-not $JavaInstancePlanPath -or -not $JavaInstancePlanSha256 -or $Zones.Count -ne 2) { throw '四Java计划必须同时给路径/SHA及两个明确zone。' }
        $javaInstances = @(Read-LocalSqlBootJavaPlan -RepoRoot $repo -Path $JavaInstancePlanPath -Sha256 $JavaInstancePlanSha256 -Zones $Zones)
    }
    $requested = @(Resolve-LocalSqlBootRequestedServices -Services $Services)
    if ($javaInstances.Count -gt 0) { $requested = @($requested | Where-Object { $_ -cne 'gateway' }) + @('gateway','mall') }
    $countsArgument = ConvertTo-LocalSqlBootCountsArgument -Counts $Counts
    $multiZone = $Zones.Count -gt 0
    $planServices = @()
    $zoneOffsets = @{}
    if ($multiZone) {
        if (@($Zones | Where-Object { $_ -lt 1 }).Count -gt 0 -or @($Zones | Select-Object -Unique).Count -ne $Zones.Count) { throw '-Zones must be distinct positive zone ids.' }
        # 没有显式R Java计划时保持原Go目录形状；有计划时同一批追加四个全局副本。
        if ($javaInstances.Count -eq 0 -and @($Services | ForEach-Object { ([string]$_) -split '[,;]' }) -ccontains 'gateway') { throw 'Multi-zone gateway requires an explicit R Java instance plan.' }
        if ($javaInstances.Count -eq 0) { $requested = @($requested | Where-Object { $_ -cne 'gateway' }) }
        $zoneOffsets = ConvertTo-LocalSqlBootZoneOffsets -Raw $ZonePortOffsets -Zones $Zones
        # 没点名时把不碰 SQL 的 Go 服务也算进计划:它们和写者抢同一批端口。目录只发写者。
        $goRequested = @($requested | Where-Object { $_ -cnotin @('gateway','mall') })
        $planServices = if (@($Services).Count -eq 0) { $goRequested + @(Get-LocalSqlBootNonWriterServices) } else { $goRequested }
    } elseif ($countsArgument -or $PortStride -ne 1 -or @($ZonePortOffsets).Count -gt 0) {
        throw '-Counts, -PortStride and -ZonePortOffsets only apply with -Zones.'
    }
    # 回环校验先于任何子进程和落盘。
    $settings = Resolve-LocalSqlBootSettings -RepoRoot $repo -Endpoint $Endpoint
    if ($javaInstances.Count -gt 0) {
        $javaDatabases = @{}; foreach ($item in $javaInstances) { $javaDatabases[$item.Service] = $item.Database }
        foreach ($database in $javaDatabases.Values) { if ($database -cne 'mmorpg' -and $database -cin $settings.LogicalDatabases) { throw 'Java逻辑库不得接管Go服务或玩家存档库。' } }
        if ($javaDatabases.mall -ceq 'mmorpg') { throw 'Mall不得与Gateway默认库共库。' }
        $settings | Add-Member -NotePropertyName JavaDatabases -NotePropertyValue $javaDatabases
        $settings.LogicalDatabases = ConvertTo-LocalSqlBootOrdinalSet -Values (@($settings.LogicalDatabases) + @($javaDatabases.Values))
    }
    if ($PhysicalAuthorityFile -or $PhysicalAuthoritySha256 -or $LockQualificationFile -or $LockQualificationSha256) {
        foreach ($pair in @(@($PhysicalAuthorityFile,$PhysicalAuthoritySha256), @($LockQualificationFile,$LockQualificationSha256))) {
            if (-not $pair[0] -or $pair[1] -cnotmatch '^[a-fA-F0-9]{64}$' -or (Get-MaintenanceSha256 $pair[0]) -ine $pair[1]) { throw '独立物理authority与真实I3必须成套给固定原件/SHA。' }
        }
        $settings | Add-Member -NotePropertyName QualifiedPhysicalSha256 -NotePropertyValue $PhysicalAuthoritySha256.ToLowerInvariant()
        $settings | Add-Member -NotePropertyName QualifiedLockFile -NotePropertyValue ([IO.Path]::GetFullPath($LockQualificationFile))
    } elseif ($javaInstances.Count -gt 0) { throw '四Java联合验证必须显式绑定独立物理原件与真实I3；不用local声明冒充。' }

    $root = Get-LocalSqlBootRoot -RepoRoot $repo
    Initialize-LocalSqlBootPrivateDirectory -Path $root
    $catalogPath = Join-Path $root 'catalog.json'
    $summaryPath = Join-Path $root 'last-run.json'
    # 旧目录的授权要么已消费要么即将作废;生成失败时不能留下一份看起来可用的旧目录。
    foreach ($stale in @($catalogPath, $summaryPath)) { if (Test-Path -LiteralPath $stale) { Remove-Item -LiteralPath $stale -Force } }

    $bootId = New-LocalSqlBootSecret -Bytes 4
    $bootsRoot = Join-Path $root 'boots'
    $bootRoot = Join-Path $bootsRoot $bootId
    $schemaWork = Join-Path $bootRoot 'schema'
    $null = [IO.Directory]::CreateDirectory($schemaWork)
    try {
        $settings.Executor = Resolve-LocalSqlBootExecutor -Settings $settings -PrivateRoot $root
        $settings.Backend = Resolve-LocalSqlBootBackend -Settings $settings -Requested $Backend
        $settings.LockPrimitive = if ($settings.Backend -ceq 'mysql') { 'LOCK_TOUCH' } else { 'SELECT_ONLY' }
        Write-LocalSqlBootLog "endpoint=$($settings.Endpoint) backend=$($settings.Backend) lock=$($settings.LockPrimitive) sql-client=$($settings.Executor.Kind) boot=$bootId"

        # 多区:计划和校验先于任何 DDL。
        $mergedPlan = @()
        if ($multiZone) {
            $mergedPlan = @(Get-LocalSqlBootMultiZonePlan -Settings $settings -RepoRoot $repo -Services $planServices -Zones $Zones -Counts $countsArgument -PortStride $PortStride -ZonePortShift $ZonePortShift -ZonePortOffsets $zoneOffsets -WorkDirectory $bootRoot) + $javaInstances
            Assert-LocalSqlBootMergedPlan -RepoRoot $repo -Instances $mergedPlan -BootId $bootId
        }

        Initialize-LocalSqlBootDatabases -Settings $settings -RepoRoot $repo -Requested $requested
        if ($javaInstances.Count -gt 0) { Initialize-LocalSqlBootMall -Settings $settings -RepoRoot $repo -Database $settings.JavaDatabases.mall }
        $skipped = @(Invoke-LocalSqlBootMigrations -Settings $settings -RepoRoot $repo -Requested $requested -WorkDirectory $schemaWork)
        # 多区不发半份:哪个服务建表失败都整批失败,不像单区那样跳过可选服务。
        if ($multiZone -and $skipped.Count -gt 0) { throw "Schema step failed for $($skipped -join ', '); multi-zone mode publishes nothing." }
        $tablesBySchema = Get-LocalSqlBootTablesBySchema -Settings $settings

        $instances = @(if ($multiZone) { $mergedPlan | Where-Object { $_.Service -cin $requested } } else { Get-LocalSqlBootInstancePlan -Settings $settings -RepoRoot $repo -Requested @($requested | Where-Object { $_ -cnotin $skipped }) -WorkDirectory $bootRoot })
        $pending = @($instances | Where-Object { -not $_.Running })

        $accountsPath = Join-Path $root 'runtime-accounts.json'
        $knownAccounts = @(if (Test-Path -LiteralPath $accountsPath) { Get-Content -LiteralPath $accountsPath -Raw | ConvertFrom-Json })
        $accountPlan = Get-LocalSqlBootStaleAccounts -Accounts $knownAccounts -Instances $instances
        if ($javaInstances.Count -gt 0) { $accountPlan = [pscustomobject]@{ Keep = $knownAccounts; Drop = @() } }

        $helper = if ('gateway' -cin $requested) { Install-LocalSqlBootPrivateFileHelper -RepoRoot $repo } else { $null }
        $catalog = [ordered]@{}
        if ($pending.Count -gt 0) {
            $physical = Get-LocalSqlBootPhysicalAuthority -Settings $settings -PrivateRoot $root
            $qualification = Get-LocalSqlBootLockQualification -Settings $settings -PrivateRoot $root
            $operators = Get-LocalSqlBootOperators -PrivateRoot $root
            $databasesByInstance = @{}
            foreach ($instance in $pending) { $databasesByInstance[$instance.InstanceKey] = @(Get-LocalSqlBootWriterDatabases -Settings $settings -Service $instance.Service -TablesBySchema $tablesBySchema) }
            $metaByDatabase = Get-LocalSqlBootMetaByDatabase -Settings $settings -Databases @($databasesByInstance.Values | ForEach-Object { $_ })
            $prepared = @(foreach ($instance in $pending) {
                    New-LocalSqlBootInstanceFiles -Settings $settings -RepoRoot $repo -Instance $instance -BootRoot $bootRoot -BootId $bootId -Databases $databasesByInstance[$instance.InstanceKey] -MetaByDatabase $metaByDatabase -PhysicalSha256 $physical -QualificationFile $qualification
                })
            # 先记账再建账号:中途失败时下一轮也能按记录清掉,DROP USER IF EXISTS 对没建成的账号是空操作。
            $accountRecords = @($accountPlan.Keep) + @($prepared | ForEach-Object { [pscustomobject]@{ principal = $_.Principal; instanceKey = $_.InstanceKey } })
            Write-LocalSqlBootText -Path $accountsPath -Text (ConvertTo-Json -InputObject ([object[]]$accountRecords) -Depth 3)
            $manifest = Get-LocalSqlBootGrantManifest -Settings $settings
            $dropSql = @($accountPlan.Drop | ForEach-Object { "DROP USER IF EXISTS $(ConvertTo-AuthoritySqlAccount $_.principal);" }) -join "`n"
            $accountSql = New-LocalSqlBootAccountSql -Settings $settings -RepoRoot $repo -Operators $operators -Prepared $prepared -TablesBySchema $tablesBySchema -Manifest $manifest
            $null = Invoke-LocalSqlBootSql -Settings $settings -Sql ($dropSql + "`n" + $accountSql) -Label 'create writer accounts'

            $tool = Resolve-LocalSqlBootTool -RepoRoot $repo -Name 'writer_authority'
            $control = New-LocalSqlBootControlEnvironment -Settings $settings -Operators $operators -PhysicalSha256 $physical
            foreach ($item in $prepared) {
                $catalog[$item.InstanceKey] = Register-LocalSqlBootInstance -Item $item -Tool $tool -ControlEnvironment $control -PhysicalSha256 $physical
                Write-LocalSqlBootLog "registered $($item.InstanceKey) as $($item.Principal)"
            }
            # 自检通过才发布,校验失败不能留下一份看起来可用的目录。
            $candidatePath = Join-Path $root 'catalog.candidate.json'
            try {
                Write-LocalSqlBootText -Path $candidatePath -Text (ConvertTo-Json -InputObject $catalog -Depth 8)
                Assert-LocalSqlBootCatalog -CatalogPath $candidatePath -RepoRoot $repo -Prepared $prepared
                [IO.File]::Move($candidatePath, $catalogPath, $true)
            } finally {
                if (Test-Path -LiteralPath $candidatePath) { Remove-Item -LiteralPath $candidatePath -Force }
            }
        } else {
            Write-LocalSqlBootText -Path $catalogPath -Text '{}'
        }

        $summary = [ordered]@{
            bootId = $bootId
            catalog = $catalogPath
            allowedDatabases = ($settings.PlayerDatabases -join ',')
            registered = [object[]]@($pending | ForEach-Object { $_.InstanceKey })
            running = [object[]]@($instances | Where-Object { $_.Running } | ForEach-Object { $_.InstanceKey })
            skipped = [object[]]$skipped
            privateFileHelper = $helper
        }
        if ($multiZone) { $summary['zones'] = [object[]]$Zones }
        if ($javaInstances.Count -gt 0) { $summary['javaInstancePlan'] = @{ path = [IO.Path]::GetFullPath($JavaInstancePlanPath); sha256 = $JavaInstancePlanSha256.ToLowerInvariant() }; $summary['javaInstances'] = [object[]]@($javaInstances | ForEach-Object InstanceKey) }
        Write-LocalSqlBootText -Path $summaryPath -Text (ConvertTo-Json -InputObject $summary -Depth 4)

        # 不按本轮注册范围清理旧 boot：未请求的实例可能仍在运行，首次写入还要在原目录
        # 落消费标记，进程身份核验也依赖原完整配置。旧 catalog 已替换，已停实例的 SQL
        # 账号已撤销；文件仅作私有历史留存，清理须另行确认整批实例已退出及证据保留期。
        Write-LocalSqlBootLog "catalog: $catalogPath ($($pending.Count) instance(s) registered, $(@($instances | Where-Object { $_.Running }).Count) already running, $($skipped.Count) skipped)"
        Write-LocalSqlBootLog "next: `$env:MMORPG_SQL_BOOT_CATALOG='$catalogPath'; `$env:MMORPG_DB_ALLOWED_DATABASES='$($settings.PlayerDatabases -join ',')'"
        return $summary
    } finally {
        # root 口令只在这两处落过盘:建表配置和主机 mysql 客户端的选项文件。
        if (Test-Path -LiteralPath $schemaWork) { Remove-Item -LiteralPath $schemaWork -Recurse -Force }
        if ($null -ne $settings.Executor -and $settings.Executor.DefaultsFile -and (Test-Path -LiteralPath $settings.Executor.DefaultsFile)) { Remove-Item -LiteralPath $settings.Executor.DefaultsFile -Force }
    }
}

function Resolve-LocalSqlBootRequestedServices {
    param([string[]]$Services = @())
    $writers = @(Get-LocalSqlBootWriterServices)
    $expanded = @($Services | ForEach-Object { ([string]$_) -split '[,;]' } | Where-Object { $_ -ne '' })
    if ($expanded.Count -eq 0) { return $writers }
    $nonWriters = @(Get-LocalSqlBootNonWriterServices)
    foreach ($name in $expanded) {
        if ($name -cnotin $writers -and $name -cnotin $nonWriters) { throw "Unknown service '$name'." }
    }
    return @($writers | Where-Object { $_ -cin $expanded })
}

# 真库测试只增加控制面适配：权限/注册/闸门DDL继续调用上面的正常producer和正式工具。
# 不接收已有库，不扫描历史私密目录；每次动作只读当前java-fixture-<nonce>的原件。
function New-LocalJavaFixtureDiagnostic {
    # Only controlled labels and numeric process metadata. No process I/O, command lines, paths or SQL.
    return @{stage='prepare-inputs';failure=$null;nativeCalls=[Collections.Generic.List[object]]::new()}
}
function Set-LocalJavaFixtureFailure {
    param([hashtable]$Diagnostic,$Record,
        [ValidateSet('producer','sql-input','sql-launch','sql-read-result','sql-confirm','runner')][string]$Step,
        [string]$SqlOutput='')
    if ($null -eq $Diagnostic -or $null -ne $Diagnostic.failure) { return }
    $failed=$Diagnostic.nativeCalls | Where-Object { $_.classification -cne 'confirmed' } | Select-Object -First 1
    $scriptName=[IO.Path]::GetFileName($Record.InvocationInfo.ScriptName)
    $failure=[ordered]@{stage=$Diagnostic.stage;step=$Step;sourceLine=$Record.InvocationInfo.ScriptLineNumber
        source=if ($scriptName -cin @('sql_boot_catalog_local.ps1','docker_java_fixture.ps1','maintenance_drain.ps1','release_common.ps1')) { $scriptName } else { 'other' }
        classification=if ($null -ne $failed) { $failed.classification } else { 'contract-unconfirmed' }
        nativeCallCount=$Diagnostic.nativeCalls.Count}
    if ($null -ne $failed) { $failure.nativeOperation=$failed.operation; $failure.nativeCallIndex=$Diagnostic.nativeCalls.IndexOf($failed) }
    if ($SqlOutput) {
        $failure.attemptMarkerPresent=[IO.File]::Exists("$SqlOutput.started")
        $failure.resultPresent=[IO.File]::Exists($SqlOutput)
    }
    $Diagnostic.failure=$failure
}
function Assert-LocalJavaFixturePath {
    param($Settings, [string]$Path)
    $root = [IO.Path]::GetFullPath($Settings.FixtureRoot).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    $full = [IO.Path]::GetFullPath($Path)
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or -not $full.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) { throw 'fixture私密路径越过本例owner' }
    return $full
}
function Get-LocalJavaFixturePublicPin {
    param([string]$Name, [string]$ShaName)
    $path = [Environment]::GetEnvironmentVariable($Name)
    $sha = [Environment]::GetEnvironmentVariable($ShaName)
    if (-not $path -or -not [IO.Path]::IsPathFullyQualified($path) -or $sha -cnotmatch '^[a-fA-F0-9]{64}$') { throw "fixture缺公开工具/资格pin：$Name" }
    $item = Get-Item -LiteralPath $path -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -or (Get-MaintenanceSha256 $path) -ine $sha) { throw 'fixture公开工具/资格原件漂移' }
    return [IO.Path]::GetFullPath($path)
}
function Invoke-LocalJavaFixturePrivateIo {
    param($Settings, [ValidateSet('read-file','create-file','validate-directory')][string]$Command, [string]$Path, [string]$InputText = '')
    $full = Assert-LocalJavaFixturePath $Settings $Path
    $result = Invoke-LocalSqlBootProcess -FilePath $Settings.PrivateHelper -Arguments @('--command',$Command,'--path',$full,'--max-bytes','16777216') -StandardInput $InputText -TimeoutSeconds 10 -RequireCompleteExit -FixtureDiagnostic $Settings.Diagnostic -FixtureOperation ('cf3-'+$Command)
    if ($result.Code -ne 0) { throw 'fixture受信私密IO拒绝或结局未明，保留HOLD' }
    return [string]$result.Out
}
function Read-LocalJavaFixtureFile {
    param($Settings, [string]$Path, [string]$ExpectedSha256 = '')
    $text = Invoke-LocalJavaFixturePrivateIo $Settings 'read-file' $Path
    $bytes = [Text.UTF8Encoding]::new($false,$true).GetBytes($text)
    $sha = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
    if ($ExpectedSha256 -and ($ExpectedSha256 -cnotmatch '^[a-fA-F0-9]{64}$' -or $sha -ine $ExpectedSha256)) { throw 'fixture私密原件SHA不符' }
    return [pscustomobject]@{ Text = $text; Sha256 = $sha }
}
function Read-LocalJavaFixtureJson {
    param($Settings, [string]$Path, [string]$ExpectedSha256 = '')
    $record = Read-LocalJavaFixtureFile $Settings $Path $ExpectedSha256
    $document = [Text.Json.JsonDocument]::Parse($record.Text)
    try { Assert-MaintenanceJsonKeys $document.RootElement } finally { $document.Dispose() }
    return ($record.Text | ConvertFrom-Json -AsHashtable -Depth 80)
}
function Write-LocalJavaFixtureFile {
    param($Settings, [string]$Path, [string]$Text)
    $null = Invoke-LocalJavaFixturePrivateIo $Settings 'create-file' $Path $Text
    return (Read-LocalJavaFixtureFile $Settings $Path).Sha256
}
function Write-LocalJavaFixtureJson {
    param($Settings, [string]$Path, $Value)
    return Write-LocalJavaFixtureFile $Settings $Path ($Value | ConvertTo-Json -Depth 80 -Compress)
}
function Split-LocalJavaFixtureSql {
    param([string]$Sql)
    # 仅拆既有源文件，保留引号内分号；不支持的delimiter/注释语法先于连接拒绝。
    if ($Sql -match '(?im)^\s*DELIMITER\b' -or $Sql.Contains('/*!')) { throw 'fixture不接受SQL客户端指令或版本可执行注释' }
    $statements = [Collections.Generic.List[string]]::new(); $builder = [Text.StringBuilder]::new()
    $quote = [char]0; $lineComment = $false; $blockComment = $false
    for ($i=0; $i -lt $Sql.Length; $i++) {
        $c=$Sql[$i]; $next=if ($i+1 -lt $Sql.Length) { $Sql[$i+1] } else { [char]0 }
        if ($lineComment) { if ($c -eq "`n") { $lineComment=$false; $null=$builder.Append(' ') }; continue }
        if ($blockComment) { if ($c -eq '*' -and $next -eq '/') { $blockComment=$false; $i++; $null=$builder.Append(' ') }; continue }
        if ($quote -ne [char]0) {
            $null=$builder.Append($c)
            if ($c -eq '\' -and $next -ne [char]0) { $null=$builder.Append($next); $i++; continue }
            if ($c -eq $quote) { if ($next -eq $quote) { $null=$builder.Append($next); $i++ } else { $quote=[char]0 } }
            continue
        }
        if (($c -eq '-' -and $next -eq '-') -or $c -eq '#') { $lineComment=$true; if ($c -eq '-') { $i++ }; continue }
        if ($c -eq '/' -and $next -eq '*') { $blockComment=$true; $i++; continue }
        if ($c -cin @([char]39,[char]34,[char]96)) { $quote=$c; $null=$builder.Append($c); continue }
        if ($c -eq ';') { if ($builder.ToString().Trim()) { $statements.Add($builder.ToString().Trim()) }; $null=$builder.Clear(); continue }
        $null=$builder.Append($c)
    }
    if ($quote -ne [char]0 -or $blockComment) { throw 'fixture SQL引号或注释未闭合' }
    if ($builder.ToString().Trim()) { $statements.Add($builder.ToString().Trim()) }
    return $statements.ToArray()
}
function Get-LocalJavaFixtureOperationSeconds {
    param($Settings,[int]$Maximum=60)
    $budgetSeconds=if ($null -eq $Settings.ActionBudgetSeconds) { 110 } else { $Settings.ActionBudgetSeconds }
    $remaining = 1000L * $budgetSeconds - $Settings.Clock.ElapsedMilliseconds
    if ($remaining -le 0) { throw 'fixture动作总预算到期，保留HOLD' }
    return [Math]::Min($Maximum, [Math]::Max(1,[int][Math]::Floor($remaining / 1000)))
}
function Invoke-LocalJavaFixtureSql {
    param($Settings, [string]$Sql, [ValidateSet('query','exec')][string]$Mode = 'exec', [string]$Database = '', [string]$CredentialOutputFile = '')
    $seconds = Get-LocalJavaFixtureOperationSeconds $Settings
    $id = New-LocalSqlBootSecret 12
    $sqlFile=Join-Path $Settings.FixtureRoot "sql-$id.txt"; $output=Join-Path $Settings.FixtureRoot "sql-$id.result.json"
    $step='sql-input'
    try {
        $sha = Write-LocalJavaFixtureFile $Settings $sqlFile $Sql
        $args = @('--dsn-file',$Settings.SchemaDsnFile,'--sql-file',$sqlFile,'--sql-sha256',$sha,'--mode',$Mode,'--database',$Database,'--expected-principal',$Settings.RootPrincipal,'--output-file',$output,'--timeout',"${seconds}s")
        if ($CredentialOutputFile) { $null=Assert-LocalJavaFixturePath $Settings $CredentialOutputFile; $args+=@('--credential-output-file',$CredentialOutputFile) }
        $step='sql-launch'
        $native = Invoke-LocalSqlBootProcess -FilePath $Settings.SqlFileSurface -Arguments $args -TimeoutSeconds ($seconds+2) -RequireCompleteExit -FixtureDiagnostic $Settings.Diagnostic -FixtureOperation 'sql-surface'
        $step='sql-read-result'
        $result = Read-LocalJavaFixtureJson $Settings $output
        $step='sql-confirm'
        if ($native.Code -ne 0 -or $result.target_outcome -cne 'confirmed' -or ($result.Contains('error') -and $result.error) -or ($result.Contains('cleanup_errors') -and @($result.cleanup_errors).Count -gt 0) -or $result.sql_sha256 -cne $sha -or $result.principal -cne $Settings.RootPrincipal) { throw 'fixture SQL未取得确定终局，保留原件HOLD，不重发' }
        return $result
    } catch {
        Set-LocalJavaFixtureFailure $Settings.Diagnostic $_ $step $output
        throw
    }
}
function Invoke-LocalJavaFixtureSource {
    param($Settings,[string]$Source,[string]$Database)
    $statements=@(Split-LocalJavaFixtureSql $Source)
    foreach ($sql in $statements) { $null=Invoke-LocalJavaFixtureSql $Settings $sql 'exec' $Database }
}
function Invoke-LocalJavaFixtureCurrentRows {
    param($Settings,[string]$Projection,[string]$From,[int]$Columns)
    # R14不提供跨语句会话：读取值与强读证据必须来自同一条实际SQL和同一条连接。
    # 控制事务的RC/悲观会话仍由正式authoritysql装配；这里不把独立的SET或VERSION探针当作证据。
    $sql="SELECT $Projection,TIDB_VERSION(),@@session.tidb_snapshot,@@session.tidb_read_staleness,@@session.tidb_read_consistency,@@session.tidb_replica_read,@@session.tidb_low_resolution_tso FROM $From FOR UPDATE"
    $result=Invoke-LocalJavaFixtureSql $Settings $sql 'query'
    $rows=[Collections.Generic.List[object]]::new()
    foreach ($row in @($result.rows)) {
        if (@($row).Count -ne ($Columns+6) -or [string]::IsNullOrWhiteSpace([string]$row[$Columns]) -or $row[$Columns+1] -cne '' -or $row[$Columns+2] -cne '0' -or $row[$Columns+3] -cne 'strict' -or $row[$Columns+4] -cne 'leader' -or $row[$Columns+5] -cne '0') { throw 'fixture当前读的同连接强会话不符，保留HOLD' }
        $rows.Add([object[]]@($row[0..($Columns-1)]))
    }
    return @{rows=[object[]]$rows.ToArray()}
}
function Assert-LocalJavaFixtureOwner {
    param($Settings,$Owner)
    if ($Owner.format -cne 'JAVA_OWNED_TIDB_OWNER_V1' -or $Owner.nonce -cne $Settings.Nonce -or $Owner.schema -cne "mall_it_$($Settings.Nonce)" -or $Owner.endpoint -cne '127.0.0.1:24000' -or $Owner.physicalSha256 -cne $Settings.PhysicalSha256 -or $Owner.lockSha256 -cne $Settings.LockSha256 -or $Owner.schemaPrincipal -cne $Settings.RootPrincipal -or $Owner.writerToolSha256 -cne $env:COMMERCE_IT_WRITER_TOOL_SHA256.ToLowerInvariant() -or $Owner.sqlFileSurfaceSha256 -cne $env:COMMERCE_IT_SQL_FILE_SURFACE_SHA256.ToLowerInvariant()) { throw 'fixture owner与当前物理/I3或随机库不符' }
    $identifier=ConvertTo-AuthoritySqlIdentifier $Owner.schema
    $result=Invoke-LocalJavaFixtureCurrentRows $Settings 'HEX(nonce),physical_sha256' "$identifier.``__java_fixture_owner`` WHERE id=1" 2
    if (@($result.rows).Count -ne 1 -or @($result.rows[0]).Count -ne 2 -or $result.rows[0][0] -ine $Settings.Nonce -or $result.rows[0][1] -cne $Settings.PhysicalSha256) { throw 'fixture SQL归属标记不符，不退休或删除' }
}
function Get-LocalJavaFixtureControlEnvironment {
    param($Settings,$Operator,[string]$Schema,[ValidateSet('registrar','maintenance','schema')][string]$Role)
    # 明确抹除继承的其他能力；Schema、Registrar、Maintenance不会进入NORMAL上下文。
    $envs=@{ MMORPG_SQL_WRITER_GRANT_FILE=''; MMORPG_SQL_WRITER_GRANT_SHA256=''; MMORPG_SQL_WRITER_CONSUME_DIR=''; MMORPG_SQL_REGISTRAR_DSN=''; MMORPG_SQL_MAINTENANCE_DSN=''; MMORPG_SQL_SCHEMA_DSN=''; MMORPG_SQL_SCHEMA_PRINCIPAL=''; MMORPG_SQL_OPERATOR_PRINCIPAL=''; MMORPG_SQL_BACKEND='tidb'; MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256=$Settings.PhysicalSha256 }
    $dsn="$($Operator.user):$($Operator.password)@tcp(127.0.0.1:24000)/$Schema"
    if ($Role -ceq 'schema') { $envs.MMORPG_SQL_SCHEMA_DSN=$dsn; $envs.MMORPG_SQL_SCHEMA_PRINCIPAL=$Operator.principal }
    else { $envs["MMORPG_SQL_$($Role.ToUpperInvariant())_DSN"]=$dsn; $envs.MMORPG_SQL_OPERATOR_PRINCIPAL=$Operator.principal }
    return $envs
}
function New-LocalJavaFixtureBoot {
    param($Settings,$Owner,[switch]$First)
    $id=New-LocalSqlBootSecret 10; $bootRoot=Join-Path $Settings.FixtureRoot "boot-$id"; $null=[IO.Directory]::CreateDirectory($bootRoot)
    $null=Invoke-LocalJavaFixturePrivateIo $Settings 'validate-directory' $bootRoot
    $schema=$Owner.schema; $identifier=ConvertTo-AuthoritySqlIdentifier $schema
    $metaResult=Invoke-LocalJavaFixtureCurrentRows $Settings 'generation,writable,writer_epoch,LENGTH(freeze_run_id)' "$identifier.``route_meta`` WHERE id=1" 4
    if (@($metaResult.rows).Count -ne 1 -or @($metaResult.rows[0]).Count -ne 4 -or $metaResult.rows[0][0] -cnotmatch '^[a-fA-F0-9]{1,32}$' -or $metaResult.rows[0][1] -cne '1' -or $metaResult.rows[0][2] -cnotmatch '^[1-9][0-9]*$' -or $metaResult.rows[0][3] -cne '0') { throw 'fixture闸门不具备NORMAL注册条件' }
    $instance=[pscustomobject]@{ Java=$true; Service='mall'; InstanceKey='mall'; Database=$schema; Port=0; ConfigPath=(Join-Path $Settings.RepoRoot 'java/mall_game/src/main/resources/application.yaml') }
    $prepared=New-LocalSqlBootInstanceFiles -Settings $Settings -RepoRoot $Settings.RepoRoot -Instance $instance -BootRoot $bootRoot -BootId $id -Databases @($schema) -MetaByDatabase @{ $schema=[pscustomobject]@{Generation=$metaResult.rows[0][0]} } -PhysicalSha256 $Settings.PhysicalSha256 -QualificationFile $Settings.LockFile -FixtureIo $Settings
    foreach ($registration in @($prepared.Registrations)) {
        $null=Assert-LocalJavaFixturePath $Settings $registration.RequestFile
        if ([IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($registration.RequestFile)) -ine [IO.Path]::GetFullPath($prepared.InputRoot)) { throw 'fixture角色renderer只接收本次新input内原Request' }
        $null=Read-LocalJavaFixtureFile $Settings $registration.RequestFile $registration.RequestSha256
    }
    # 账号口令/原正式Request在第一次账号DDL前CreateNew。失败遗留的boot无registered原件会阻断整体清理。
    $descriptor=[ordered]@{ format='JAVA_OWNED_TIDB_BOOT_V1'; nonce=$Settings.Nonce; item=$prepared }
    $null=Write-LocalJavaFixtureJson $Settings (Join-Path $bootRoot 'boot.owner.json') $descriptor
    $tablesResult=Invoke-LocalJavaFixtureSql $Settings "SELECT table_name FROM information_schema.tables WHERE table_schema='$schema' AND table_type='BASE TABLE'" 'query'
    $tables=@($tablesResult.rows | ForEach-Object { $_[0] } | Where-Object { $_ -cne '__java_fixture_owner' })
    $sql=New-LocalSqlBootAccountSql -Settings $Settings -RepoRoot $Settings.RepoRoot -Operators $Owner.operators -Prepared @($prepared) -TablesBySchema @{$schema=$tables} -Manifest @{mall=@{Own=@($schema);Cross=@()}} -FreshAccountsOnly -SkipOperatorCreation:(-not $First)
    Invoke-LocalJavaFixtureSource $Settings $sql ''
    $control=Get-LocalJavaFixtureControlEnvironment $Settings $Owner.operators.registrar $schema 'registrar'
    $entry=Register-LocalSqlBootInstance -Item $prepared -Tool $Settings.WriterTool -ControlEnvironment $control -PhysicalSha256 $Settings.PhysicalSha256 -PreserveInputs -FixtureIo $Settings
    # writer_authority 把这一代每个逻辑库的完整请求记在 grant 旁的启动记录里;本例只写一个库,取那一份当退休用的原件。
    $bootRecord=Read-LocalJavaFixtureJson $Settings "$($prepared.GrantFile).register.started.protojson"
    $canonical=Join-Path $bootRoot 'register.canonical.json'
    $canonicalSha256=Write-LocalJavaFixtureJson $Settings $canonical @($bootRecord.requests)[0]
    $registered=[ordered]@{format='JAVA_OWNED_TIDB_REGISTERED_V1';nonce=$Settings.Nonce;entry=$entry;item=$prepared;canonicalFile=$canonical;canonicalSha256=$canonicalSha256}
    $null=Write-LocalJavaFixtureJson $Settings (Join-Path $bootRoot 'boot.registered.json') $registered
    return [ordered]@{ format='JAVA_OWNED_TIDB_CONTEXT_V1'; nonce=$Settings.Nonce; ownerFile=$Settings.OwnerFile; ownerSha256=$Settings.OwnerSha256; jdbcUrl="jdbc:mysql://127.0.0.1:24000/$schema"; username=$prepared.User; password=$prepared.Password; environment=[ordered]@{ MMORPG_SQL_WRITER_GRANT_FILE=$entry.grantFile; MMORPG_SQL_WRITER_GRANT_SHA256=$entry.grantSha256; MMORPG_SQL_WRITER_CONSUME_DIR=$entry.consumeDir; MMORPG_SQL_DEPLOYMENT_ID=$entry.deploymentId; MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256=$Settings.PhysicalSha256 } }
}
function Assert-LocalJavaFixtureRegistration {
    param($Owner,$Registered,$Request)
    if ($Request.kind -cne 'REGISTER' -or $Request.authoritySchema -cne $Owner.schema -or @($Request.protectedSchemas).Count -ne 1 -or $Request.protectedSchemas[0] -cne $Owner.schema -or $Request.deploymentId -cne $Registered.item.DeploymentId -or [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Request.writer.sqlPrincipal)) -cne $Registered.item.Principal -or -not $Request.writer.registrationDigest -or ($Request.Contains('maintenanceRunId') -and $Request.maintenanceRunId)) { throw 'fixture原注册不属于本例或不是普通NORMAL' }
}
function Close-LocalJavaFixture {
    param($Settings,$Owner)
    Assert-LocalJavaFixtureOwner $Settings $Owner
    $boots=@(Get-ChildItem -LiteralPath $Settings.FixtureRoot -Directory -Filter 'boot-*')
    if ($boots.Count -lt 1) { throw 'fixture缺原NORMAL boot，不清理' }
    $users=[Collections.Generic.List[string]]::new()
    foreach ($boot in $boots) {
        $null=Invoke-LocalJavaFixturePrivateIo $Settings 'validate-directory' $boot.FullName
        $descriptor=Read-LocalJavaFixtureJson $Settings (Join-Path $boot.FullName 'boot.owner.json')
        $registered=Read-LocalJavaFixtureJson $Settings (Join-Path $boot.FullName 'boot.registered.json')
        if ($descriptor.nonce -cne $Settings.Nonce -or $registered.nonce -cne $Settings.Nonce -or $descriptor.item.Principal -cne $registered.item.Principal -or $descriptor.item.RosterSha256 -cne $registered.item.RosterSha256) { throw 'fixture存在未确认或不同owner的注册，不清理' }
        $request=Read-LocalJavaFixtureJson $Settings $registered.canonicalFile $registered.canonicalSha256
        Assert-LocalJavaFixtureRegistration $Owner $registered $request
        $identifier=ConvertTo-AuthoritySqlIdentifier $Owner.schema
        $meta=Invoke-LocalJavaFixtureCurrentRows $Settings 'writer_epoch,catalog_revision,writable,LENGTH(freeze_run_id)' "$identifier.``route_meta`` WHERE id=1" 4
        if (@($meta.rows).Count -ne 1 -or $meta.rows[0][2] -cne '1' -or $meta.rows[0][3] -cne '0') { throw 'fixture不可普通退休，保留HOLD' }
        $request.kind='RETIRE'; $request.requestId=New-LocalSqlBootIdentity; $request.expectedEpoch=[string]$meta.rows[0][0]; $request.expectedCatalogRevision=[string]$meta.rows[0][1]
        $retire=Join-Path $boot.FullName 'retire.request.json'; $receipt=Join-Path $boot.FullName 'retire.receipt.json'
        $null=Write-LocalJavaFixtureJson $Settings $retire $request
        $envs=Get-LocalJavaFixtureControlEnvironment $Settings $Owner.operators.maintenance $Owner.schema 'maintenance'
        $seconds=Get-LocalJavaFixtureOperationSeconds $Settings 30
        $native=Invoke-LocalSqlBootProcess -FilePath $Settings.WriterTool -Arguments @('-command','retire','-request',$retire,'-input-root',$boot.FullName,'-receipt-output',$receipt,'-timeout',"${seconds}s") -Environment $envs -TimeoutSeconds ($seconds+2) -RequireCompleteExit -FixtureDiagnostic $Settings.Diagnostic -FixtureOperation 'retire'
        if ($native.Code -ne 0) { throw 'fixture原请求RETIRE未确认，不重发或清理' }
        $confirmed=Read-LocalJavaFixtureJson $Settings $receipt
        if ($confirmed.kind -cne 'RETIRE' -or $confirmed.requestId -cne $request.requestId -or $confirmed.generation -cne $request.writer.generation -or $confirmed.previousEpoch -cne $request.expectedEpoch -or $confirmed.resultingEpoch -cne $request.expectedEpoch) { throw 'fixture正式RETIRE收据不符，不清理' }
        $users.Add($registered.item.Principal)
    }
    # 每个池已经在Java侧seal/close；这里只在所有正式RETIRE确定后删除本例随机资源。
    Assert-LocalJavaFixtureOwner $Settings $Owner
    $identifier=ConvertTo-AuthoritySqlIdentifier $Owner.schema
    $active=Invoke-LocalJavaFixtureCurrentRows $Settings 'COUNT(*)' "$identifier.``writer_slot`` WHERE state=1" 1
    if (@($active.rows).Count -ne 1 -or $active.rows[0][0] -cne '0') { throw 'fixture仍有ACTIVE写者，不清理' }
    foreach ($principal in @($users.ToArray()) + @($Owner.operators.registrar.principal,$Owner.operators.maintenance.principal)) {
        $account=ConvertTo-AuthoritySqlAccount $principal
        $null=Invoke-LocalJavaFixtureSql $Settings "DROP USER $account" 'exec'
    }
    $null=Invoke-LocalJavaFixtureSql $Settings "DROP DATABASE $identifier" 'exec'
}
function Invoke-LocalJavaFixtureAction {
    param([string]$Action,[string]$RequestFile,[string]$RequestSha256,[hashtable]$Diagnostic,
        [ValidateRange(1,600)][int]$ActionBudgetSeconds=110)
    if ($null -eq $Diagnostic) { $Diagnostic=New-LocalJavaFixtureDiagnostic }
    $Diagnostic.stage='request-ownership'
    # 仅目录名和新请求路径可在读取前检查；私密正文由CF3原句柄读取。
    $root=[IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($RequestFile)); $leaf=[IO.Path]::GetFileName($root)
    if ($leaf -cnotmatch '^java-fixture-([a-f0-9]{32})$') { throw 'fixture请求不在本例随机owner根' }
    $nonce=$Matches[1]; $parent=[IO.Path]::GetFullPath([Environment]::GetEnvironmentVariable('COMMERCE_IT_PRIVATE_ROOT'))
    if ([IO.Path]::GetDirectoryName($root) -ine $parent -or [IO.Path]::GetFileName($RequestFile) -cnotmatch '^action-[1-9][0-9]*[.]request[.]json$') { throw 'fixture请求越过显式私密根' }
    $settings=[pscustomobject]@{ FixtureRoot=$root; Nonce=$nonce; Clock=[Diagnostics.Stopwatch]::StartNew(); ActionBudgetSeconds=$ActionBudgetSeconds; Diagnostic=$Diagnostic; PrivateHelper=(Get-LocalJavaFixturePublicPin 'MMORPG_SQL_PRIVATE_FILE_HELPER' 'MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256') }
    $request=Read-LocalJavaFixtureJson $settings $RequestFile $RequestSha256
    if ($request.format -cne 'JAVA_OWNED_TIDB_ACTION_V1' -or $request.nonce -cne $nonce -or [IO.Path]::GetFullPath($request.root) -ine $root) { throw 'fixture请求所有权不符' }
    $output=Assert-LocalJavaFixturePath $settings $request.outputFile
    $null=Write-LocalJavaFixtureJson $settings "$RequestFile.started" @{nonce=$nonce;action=$Action;requestSha256=$RequestSha256}
    try {
        foreach ($prior in @(Get-ChildItem -LiteralPath $root -File -Filter 'action-*.request.json.started')) {
            if ($prior.FullName -ine "$RequestFile.started" -and -not [IO.File]::Exists(($prior.FullName -replace '[.]started$','.completed'))) { throw 'fixture已有UNKNOWN动作，只保留原件HOLD，禁止新动作/新boot' }
        }
        $schemaDsn=[Environment]::GetEnvironmentVariable('COMMERCE_IT_SCHEMA_DSN_FILE'); $principal=[Environment]::GetEnvironmentVariable('COMMERCE_IT_SCHEMA_PRINCIPAL')
        if (-not $schemaDsn -or -not [IO.Path]::IsPathFullyQualified($schemaDsn) -or -not $principal) { throw 'fixture缺显式独立Schema凭据原件/主体' }
        $null=ConvertTo-AuthoritySqlAccount $principal
        $repo=[IO.Path]::GetFullPath([Environment]::GetEnvironmentVariable('COMMERCE_IT_REPO_ROOT'))
        if ([IO.Path]::GetFullPath((Join-Path $repo 'tools/scripts/lib/sql_boot_catalog_local.ps1')) -ine $PSCommandPath) { throw 'fixture必须执行本仓库正常producer原件' }
        $physical=Get-LocalJavaFixturePublicPin 'COMMERCE_IT_PHYSICAL_AUTHORITY_FILE' 'COMMERCE_IT_PHYSICAL_AUTHORITY_SHA256'
        $lock=Get-LocalJavaFixturePublicPin 'COMMERCE_IT_LOCK_QUALIFICATION_FILE' 'COMMERCE_IT_LOCK_QUALIFICATION_SHA256'
        foreach ($pair in @{Endpoint='127.0.0.1:24000';Backend='tidb';LockPrimitive='SELECT_ONLY';DirectorySchema='';DirectoryShards=@();RepoRoot=$repo;SchemaDsnFile=$schemaDsn;RootPrincipal=$principal;PhysicalSha256=([Environment]::GetEnvironmentVariable('COMMERCE_IT_PHYSICAL_AUTHORITY_SHA256').ToLowerInvariant());LockSha256=([Environment]::GetEnvironmentVariable('COMMERCE_IT_LOCK_QUALIFICATION_SHA256').ToLowerInvariant());LockFile=$lock;WriterTool=(Get-LocalJavaFixturePublicPin 'COMMERCE_IT_WRITER_TOOL' 'COMMERCE_IT_WRITER_TOOL_SHA256');SqlFileSurface=(Get-LocalJavaFixturePublicPin 'COMMERCE_IT_SQL_FILE_SURFACE' 'COMMERCE_IT_SQL_FILE_SURFACE_SHA256')}.GetEnumerator()) { $settings | Add-Member -NotePropertyName $pair.Key -NotePropertyValue $pair.Value }
        $ownerPath=Join-Path $root 'owner.json'
        $Diagnostic.stage='owner-record'
        if ($Action -ceq 'create') {
            Assert-MaintenanceProperties $request @('format','nonce','root','outputFile','endpoint','schema')
            if ($request.endpoint -cne 'jdbc:mysql://127.0.0.1:24000/' -or $request.schema -cne "mall_it_$nonce") { throw 'fixture仅接受本例随机库和已资格端点' }
            $operators=@{}
            foreach ($role in @('registrar','maintenance')) { $user='jf_'+(New-LocalSqlBootSecret 12); $operators[$role]=@{user=$user;password=(New-LocalSqlBootSecret);principal="$user@%"} }
            $owner=[ordered]@{format='JAVA_OWNED_TIDB_OWNER_V1';nonce=$nonce;schema=$request.schema;endpoint=$settings.Endpoint;physicalSha256=$settings.PhysicalSha256;lockSha256=$settings.LockSha256;schemaPrincipal=$principal;writerToolSha256=$env:COMMERCE_IT_WRITER_TOOL_SHA256.ToLowerInvariant();sqlFileSurfaceSha256=$env:COMMERCE_IT_SQL_FILE_SURFACE_SHA256.ToLowerInvariant();operators=$operators}
            $ownerSha=Write-LocalJavaFixtureJson $settings $ownerPath $owner
        } else {
            Assert-MaintenanceProperties $request @('format','nonce','root','outputFile','ownerFile','ownerSha256') @('resource')
            if ([IO.Path]::GetFullPath($request.ownerFile) -ine $ownerPath) { throw 'fixture只接受本例owner' }
            $ownerSha=$request.ownerSha256; $owner=Read-LocalJavaFixtureJson $settings $ownerPath $ownerSha
        }
        $settings | Add-Member -NotePropertyName OwnerFile -NotePropertyValue $ownerPath
        $settings | Add-Member -NotePropertyName OwnerSha256 -NotePropertyValue $ownerSha
        # R14本身验证DSN只指向24000且无Params；身份探针先于CREATE，导出的口令只存本例CF3新文件。
        $credentialFile=Join-Path $root ('schema-credentials-'+(New-LocalSqlBootSecret 12)+'.json')
        $Diagnostic.stage='schema-identity'
        $probe=Invoke-LocalJavaFixtureSql $settings 'SELECT CURRENT_USER()' 'query' '' $credentialFile
        if (-not $probe.credentials_written) { throw 'fixture Schema身份未确认' }
        $credentials=Read-LocalJavaFixtureJson $settings $credentialFile
        $settings | Add-Member -NotePropertyName RootUser -NotePropertyValue $credentials.User
        $settings | Add-Member -NotePropertyName RootPassword -NotePropertyValue $credentials.Password
        $schemaOperator=@{user=$credentials.User;password=$credentials.Password;principal=$principal}
        if ($Action -ceq 'create') {
            $Diagnostic.stage='schema-preconditions'
            $probe=Invoke-LocalJavaFixtureSql $settings 'SELECT TIDB_VERSION()' 'query'
            if (@($probe.rows).Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$probe.rows[0][0])) { throw 'fixture不是实际TiDB' }
            $schema=$owner.schema; $identifier=ConvertTo-AuthoritySqlIdentifier $schema
            $absent=Invoke-LocalJavaFixtureSql $settings "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='$schema'" 'query'
            if (@($absent.rows).Count -ne 1 -or $absent.rows[0][0] -cne '0') { throw 'fixture随机库已存在，不接管' }
            $Diagnostic.stage='create-owned-schema'
            $null=Invoke-LocalJavaFixtureSql $settings "CREATE DATABASE $identifier DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci" 'exec'
            $null=Invoke-LocalJavaFixtureSql $settings "CREATE TABLE $identifier.``__java_fixture_owner`` (id INT PRIMARY KEY,nonce VARBINARY(16) NOT NULL,physical_sha256 CHAR(64) NOT NULL)" 'exec'
            $null=Invoke-LocalJavaFixtureSql $settings "INSERT INTO $identifier.``__java_fixture_owner`` VALUES(1,X'$nonce','$($settings.PhysicalSha256)')" 'exec'
            $envs=Get-LocalJavaFixtureControlEnvironment $settings $schemaOperator $schema 'schema'
            $seconds=Get-LocalJavaFixtureOperationSeconds $settings
            $Diagnostic.stage='prepare-schema'
            $migration=Invoke-LocalSqlBootProcess -FilePath $settings.WriterTool -Arguments @('-command','prepare-schema','-schema',$schema,'-timeout',"${seconds}s") -Environment $envs -TimeoutSeconds ($seconds+2) -RequireCompleteExit -FixtureDiagnostic $Diagnostic -FixtureOperation 'prepare-schema'
            if ($migration.Code -ne 0) { throw 'fixture正式本库闸门Schema迁移未确认，不注册NORMAL' }
            $Diagnostic.stage='mall-schema'
            Invoke-LocalJavaFixtureSource $settings ([IO.File]::ReadAllText((Join-Path $repo 'java/mall_game/src/main/resources/db/schema.sql'))) $schema
            $Diagnostic.stage='register-boot'
            $context=New-LocalJavaFixtureBoot $settings $owner -First
        } else {
            $Diagnostic.stage='verify-owner'
            Assert-LocalJavaFixtureOwner $settings $owner
            $Diagnostic.stage=switch ($Action) { 'open-boot' {'register-boot'} 'migrate' {'migrate'} 'close' {'retire-owned-boots'} default {'request-ownership'} }
            if ($Action -ceq 'open-boot') { $context=New-LocalJavaFixtureBoot $settings $owner }
            elseif ($Action -ceq 'migrate') {
                if ($request.resource -cnotin @('db/migration/V3__catalog_admin.sql','db/migration/V4__mock_payment.sql','db/migration/V5__rmb_consignment.sql')) { throw 'fixture迁移不是已有商城事实源' }
                Invoke-LocalJavaFixtureSource $settings ([IO.File]::ReadAllText((Join-Path $repo ('java/mall_game/src/main/resources/'+$request.resource)))) $owner.schema
                $context=@{format='JAVA_OWNED_TIDB_CONTEXT_V1';nonce=$nonce}
            } elseif ($Action -ceq 'close') { Close-LocalJavaFixture $settings $owner; $context=@{format='JAVA_OWNED_TIDB_CONTEXT_V1';nonce=$nonce} }
            else { throw '未知fixture动作' }
        }
        $Diagnostic.stage='write-action-output'
        $outputSha=Write-LocalJavaFixtureJson $settings $output $context
        $null=Write-LocalJavaFixtureJson $settings "$RequestFile.completed" @{nonce=$nonce;action=$Action;outputSha256=$outputSha}
    } catch {
        Set-LocalJavaFixtureFailure $Diagnostic $_ 'producer'
        # Freeze the first failure before HOLD's own CF3 operations.
        try { $null=Write-LocalJavaFixtureJson $settings "$RequestFile.hold.json" @{format='JAVA_OWNED_TIDB_HOLD_V1';nonce=$nonce;action=$Action;automaticRetry=$false;automaticDrop=$false;failure=$Diagnostic.failure;nativeCalls=$Diagnostic.nativeCalls.ToArray()} } catch { }
        throw '真库fixture未取得完整确定终局，原件HOLD，不自动重发或清理'
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    $ErrorActionPreference = 'Stop'
    if ($JavaFixtureAction) {
        Invoke-LocalJavaFixtureAction -Action $JavaFixtureAction -RequestFile $JavaFixtureRequestFile -RequestSha256 $JavaFixtureRequestSha256
        return
    }
    # 本文件在 tools/scripts/lib,仓库根往上三级,和 sql_boot_catalog_docker.ps1 同一写法。原来只退两级,直接调用时把 tools 当成了仓库根。
    $null = New-LocalSqlBootCatalog -RepoRoot ([IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))) -Services $Services -Endpoint $Endpoint -Backend $Backend -Zones $Zones -Counts $Counts -PortStride $PortStride -ZonePortShift $ZonePortShift -ZonePortOffsets $ZonePortOffsets -JavaInstancePlanPath $JavaInstancePlanPath -JavaInstancePlanSha256 $JavaInstancePlanSha256 -PhysicalAuthorityFile $PhysicalAuthorityFile -PhysicalAuthoritySha256 $PhysicalAuthoritySha256 -LockQualificationFile $LockQualificationFile -LockQualificationSha256 $LockQualificationSha256
}
