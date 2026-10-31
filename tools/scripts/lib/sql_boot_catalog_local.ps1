#requires -Version 7
<#
.SYNOPSIS
    本机 SQL 写者启动目录(MMORPG_SQL_BOOT_CATALOG)生成器,只接受回环数据库。

.DESCRIPTION
    一条命令备齐 start_game.ps1 / go_services.ps1 需要的写者授权:
      1. 用本机 root 建库、建表(各服务自己的 -migrate 与 go/db cmd/migrate;常驻账号没有 DDL)。
      2. 按 deploy/sql 的角色模板(maintenance_drain.ps1 的 ConvertTo-AuthorityRoleSql)给每个实例建独立账号。
      3. 每个实例调一次正式的 writer_authority register,拿到单次启动授权和选中配置。
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
    [ValidateSet('auto', 'mysql', 'tidb')][string]$Backend = 'auto'
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
    @('route_meta', 'writer_registry', 'writer_slot', 'writer_lifecycle_receipt', 'sql_writer_drain_run', 'sql_writer_freeze_ticket', 'sql_writer_maintenance_receipt')
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
        [int]$TimeoutSeconds = 120
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
    try {
        if (-not $process.Start()) { throw "Failed to start $([IO.Path]::GetFileName($FilePath))." }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if ($hasInput) {
            $process.StandardInput.Write($StandardInput)
            $process.StandardInput.Close()
        }
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            try { $process.Kill($true) } catch { $null = $_ }
            throw "$([IO.Path]::GetFileName($FilePath)) did not finish within $TimeoutSeconds seconds."
        }
        # 后台孙进程可能继承管道,读尾部只等有限时间。
        $out = if ($stdout.Wait(5000)) { $stdout.Result } else { '' }
        $err = if ($stderr.Wait(5000)) { $stderr.Result } else { '' }
        return [pscustomobject]@{ Code = $process.ExitCode; Out = $out; Err = $err }
    } finally {
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

# 本机账号、库名与登录 Redis 全部取已有配置的原件,缺了不猜。root 与 go_services.ps1 的本机建表同一来源:
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
        -DbConfigPath $dbConfig -DataServiceConfigPath (Join-Path $RepoRoot 'go/data_service/etc/data_service.yaml') -LoginConfigPath (Join-Path $RepoRoot 'go/login/etc/login.yaml')
}

# 库名清单、保护集合与登录 Redis 只有这一份。端点和 root 身份由调用方定:本机入口先过回环校验,docker 调试栈固定 compose 的 mysql。
function New-LocalSqlBootSettings {
    param(
        [Parameter(Mandatory)][string]$Endpoint,
        [Parameter(Mandatory)][string]$EndpointName,
        [Parameter(Mandatory)][int]$Port,
        [Parameter(Mandatory)][string]$RootUser,
        [Parameter(Mandatory)][string]$RootPassword,
        [Parameter(Mandatory)][string]$RootPrincipal,
        [Parameter(Mandatory)][string]$DbConfigPath,
        [Parameter(Mandatory)][string]$DataServiceConfigPath,
        [Parameter(Mandatory)][string]$LoginConfigPath
    )
    $globalSchema = Get-LocalSqlBootYamlRequired -Path $DataServiceConfigPath -KeyPath 'SnapshotMySQL.DBName'
    $zoneId = Get-LocalSqlBootYamlRequired -Path $DbConfigPath -KeyPath 'ZoneId'
    if ($zoneId -cnotmatch '^[1-9][0-9]*$') { throw 'go/db ZoneId must be a positive integer.' }
    $zoneDatabase = "zone_${zoneId}_db"
    $allowList = [string][Environment]::GetEnvironmentVariable('DB_ALLOWED_DATABASES', 'Process')
    if ([string]::IsNullOrWhiteSpace($allowList)) { $allowList = [string][Environment]::GetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES', 'Process') }
    if ([string]::IsNullOrWhiteSpace($allowList)) { $allowList = $zoneDatabase }
    $playerDatabases = [string[]]@($allowList -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })
    if ($zoneDatabase -cnotin $playerDatabases) { throw "$zoneDatabase must be in the player database allow list." }

    $loginRedisHost = Get-LocalSqlBootYamlRequired -Path $LoginConfigPath -KeyPath 'Node.RedisClient.Host'
    $loginRedisDb = Get-LocalSqlBootYamlRequired -Path $LoginConfigPath -KeyPath 'Node.RedisClient.DB'

    # gateway 的 mmorpg 也在保护集合里:写者连接的 DATABASE() 必须落在集合内(authoritysql pool.go、Java WriterAuthority)。
    $schemas = ConvertTo-LocalSqlBootOrdinalSet -Values (@('mmorpg', $globalSchema, 'mmorpg_chat', 'mmorpg_friend', 'mmorpg_guild', 'mmorpg_mail', 'mmorpg_match', 'mmorpg_player_inbox', 'mmorpg_rank', 'mmorpg_trade') + $playerDatabases)
    foreach ($schema in $schemas) { $null = ConvertTo-AuthoritySqlIdentifier $schema }

    return [pscustomobject]@{
        Endpoint = $Endpoint; EndpointName = $EndpointName; Port = $Port
        RootUser = $RootUser; RootPassword = $RootPassword; RootPrincipal = $RootPrincipal
        GlobalSchema = $globalSchema; ZoneDatabase = $zoneDatabase; PlayerDatabases = $playerDatabases
        ProtectedSchemas = [string[]]@($schemas); LoginRedisHost = $loginRedisHost; LoginRedisDb = $loginRedisDb
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
    $schema = $Settings.GlobalSchema
    $redisHost = ConvertTo-LocalSqlBootYamlString -Value $Settings.LoginRedisHost
    $redisDb = $Settings.LoginRedisDb
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
            $values['AccountRoleSchema'] = $schema
            $values['GlobalLoginSessions'] = 'true'
            $values['LoginRedis.Addrs'] = "[$redisHost]"
            $values['LoginRedis.DB'] = $redisDb
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
            $values['AccountRoleSchema'] = $schema
            $values['GlobalLoginSessions'] = 'true'
            $values['LoginRedis.Addrs'] = "[$redisHost]"
            $values['LoginRedis.DB'] = $redisDb
        }
        'rank' {
            $values['Etcd.Key'] = '""'
            $values['Backend'] = $backend
        }
        'match' {
            $values['GlobalMatch.Backend'] = $backend
            $values['GlobalMatch.Host'] = $endpoint
            $values['GlobalMatch.User'] = $userLiteral
            $values['GlobalMatch.AccountRoleSchema'] = $schema
            $values['LoginRedis.Addrs'] = "[$redisHost]"
            $values['LoginRedis.DB'] = $redisDb
        }
        'trade' {
            $values['MySQL.Backend'] = $backend
            $values['MySQL.Host'] = $endpoint
            $values['MySQL.User'] = $userLiteral
            $values['MySQL.Password'] = $passwordLiteral
            $values['AccountRoleSchema'] = $schema
        }
        'player_inbox' {
            $values['MySQL.Backend'] = $backend
            $values['MySQL.Host'] = $endpoint
            $values['MySQL.User'] = $userLiteral
            $values['MySQL.Password'] = $passwordLiteral
            $values['AccountRoleSchema'] = $schema
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
    $statements = foreach ($schema in $Settings.ProtectedSchemas) {
        $identifier = ConvertTo-AuthoritySqlIdentifier $schema
        if ($schema -ceq 'mmorpg' -or $schema -cin $Settings.PlayerDatabases) { "CREATE DATABASE IF NOT EXISTS $identifier;" }
        else { "CREATE DATABASE IF NOT EXISTS $identifier DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" }
    }
    $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql (($statements -join "`n") + "`n") -Label 'create local databases'
    if ('gateway' -cin $Requested) {
        $gatewayTables = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/mysql-init/gateway_tables.sql'))
        $null = Invoke-LocalSqlBootSql -Settings $Settings -Sql $gatewayTables -Label 'create gateway tables'
    }
}

# fresh:库是空的,跑 -migrate。ready:权威表齐全,跳过(-migrate 拒绝已有表)。partial:半成品或旧布局,不替人删数据。
function Get-LocalSqlBootGlobalSchemaState {
    param([Parameter(Mandatory)]$Settings)
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT table_name FROM information_schema.tables WHERE table_schema='$($Settings.GlobalSchema)';" -Label 'inspect the global schema')
    if ($rows.Count -eq 0) { return [pscustomobject]@{ State = 'fresh'; Missing = @() } }
    $missing = @(Get-LocalSqlBootAuthorityTables | Where-Object { $_ -cnotin $rows })
    return [pscustomobject]@{ State = $(if ($missing.Count -eq 0) { 'ready' } else { 'partial' }); Missing = $missing }
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

# go/db 的正式入口是 cmd/migrate,不走 authoritysql。退出码 4 = 迁移成功、有待人工过目的漂移(cmd/migrate/main.go),不拦。
function Invoke-LocalSqlBootDbMigration {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$RepoRoot, [Parameter(Mandatory)][string]$WorkDirectory)
    $tool = Resolve-LocalSqlBootTool -RepoRoot $RepoRoot -Name 'db_migrate'
    $config = Join-Path $WorkDirectory 'db.yaml'
    $source = [IO.File]::ReadAllText((Get-LocalSqlBootSourceConfigPath -RepoRoot $RepoRoot -Service 'db'))
    Write-LocalSqlBootText -Path $config -Text (ConvertTo-LocalSqlBootConfig -Settings $Settings -Service 'db' -Content $source -User $Settings.RootUser -Password $Settings.RootPassword)
    $workingDirectory = [IO.Path]::GetFullPath((Join-Path $RepoRoot 'go/db'))
    $result = Invoke-LocalSqlBootProcess -FilePath $tool -Arguments @('-f', $config, '-command', 'up') -Environment @{ DB_ALLOWED_DATABASES = ($Settings.PlayerDatabases -join ',') } -WorkingDirectory $workingDirectory -TimeoutSeconds 600
    if ($result.Code -eq 4) {
        Write-Warning "go/db migrate reported drift that needs manual review: $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Out))"
    } elseif ($result.Code -ne 0) {
        throw "go/db migrate failed (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err + [string]$result.Out))"
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

function Get-LocalSqlBootRouteMeta {
    param([Parameter(Mandatory)]$Settings)
    $schema = ConvertTo-AuthoritySqlIdentifier $Settings.GlobalSchema
    $rows = @(Invoke-LocalSqlBootSql -Settings $Settings -Sql "SELECT generation,writable,writer_epoch,LENGTH(freeze_run_id) FROM $schema.``route_meta`` WHERE id=1;" -Label 'read route_meta')
    if ($rows.Count -ne 1) { throw 'route_meta has no seed row; data_service -migrate has not completed.' }
    $fields = $rows[0].Split("`t")
    $epoch = [uint64]0
    if ($fields.Count -ne 4 -or $fields[0] -cnotmatch '^[0-9a-fA-F]{1,32}$' -or -not [uint64]::TryParse($fields[2], [ref]$epoch) -or $epoch -eq 0) { throw 'route_meta seed row has an unexpected shape.' }
    # 冻结或未释放的维护 run 里不能发普通授权,Register 也会拒绝;这里先报清楚。
    if ($fields[1] -cne '1' -or $fields[3] -cne '0') { throw 'route_meta is frozen by a maintenance run; refusing to register writers.' }
    return [pscustomobject]@{ Generation = $fields[0]; WriterEpoch = $epoch }
}

function Get-LocalSqlBootTablesBySchema {
    param([Parameter(Mandatory)]$Settings)
    $names = ($Settings.ProtectedSchemas | ForEach-Object { "'$_'" }) -join ','
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

# 每个写者能碰什么。本库 DML;跨库按常驻代码实际访问的表(MySQL 的 SELECT ... FOR UPDATE 除 SELECT 外还要 UPDATE,所以锁读授 SELECT,UPDATE)。
# data_service 的角色交易证明要锁多个域的多张表,按库放开、逐表枚举,域里加表不用改这里;模板要求逐表 GRANT,没有 schema.*。
function Get-LocalSqlBootGrantManifest {
    param([Parameter(Mandatory)]$Settings)
    $authority = $Settings.GlobalSchema
    $read = @('SELECT')
    $lock = @('SELECT', 'UPDATE')
    $write = @('SELECT', 'INSERT', 'UPDATE', 'DELETE')
    $dataServiceCross = [Collections.Generic.List[object]]::new()
    foreach ($schema in @('mmorpg_guild', 'mmorpg_mail', 'mmorpg_match', 'mmorpg_player_inbox', 'mmorpg_trade')) { $dataServiceCross.Add(@{ Schema = $schema; Operations = $lock }) }
    foreach ($schema in $Settings.PlayerDatabases) { $dataServiceCross.Add(@{ Schema = $schema; Operations = $write }) }
    return @{
        data_service = @{ Own = @($authority); Cross = $dataServiceCross.ToArray() }
        db = @{ Own = @($Settings.ZoneDatabase); Cross = @(@{ Schema = $authority; Tables = @('player_route'); Operations = $read }) }
        login = @{ Own = @(); Cross = @() }
        gateway = @{ Own = @('mmorpg'); Cross = @() }
        chat = @{ Own = @('mmorpg_chat'); Cross = @(@{ Schema = $authority; Tables = @('account_role', 'player_route'); Operations = $read }) }
        friend = @{ Own = @('mmorpg_friend'); Cross = @(@{ Schema = $authority; Tables = @('account_role'); Operations = $lock }) }
        guild = @{ Own = @('mmorpg_guild'); Cross = @(
                @{ Schema = $authority; Tables = @('account_role', 'player_route'); Operations = $lock }
                @{ Schema = 'mmorpg_match'; Tables = @('match_authority_ticket', 'team_launch', 'team_launch_role'); Operations = $lock }
                @{ Schema = 'mmorpg_match'; Tables = @('match_battle_completion'); Operations = $read }
            )
        }
        mail = @{ Own = @('mmorpg_mail'); Cross = @(
                @{ Schema = $authority; Tables = @('account_role'); Operations = $lock }
                @{ Schema = 'mmorpg_guild'; Tables = @('guild_member'); Operations = $read }
            )
        }
        rank = @{ Own = @('mmorpg_rank'); Cross = @() }
        match = @{ Own = @('mmorpg_match'); Cross = @(
                @{ Schema = $authority; Tables = @('account_role', 'account_role_admission', 'player_route'); Operations = $lock }
                @{ Schema = $authority; Tables = @('zone_merged_into'); Operations = $read }
                @{ Schema = 'mmorpg_guild'; Tables = @('guild_trial_launch', 'guild_trial_launch_role', 'guild_trial_lobby', 'guild_trial_lobby_slot'); Operations = $read }
            )
        }
        trade = @{ Own = @('mmorpg_trade'); Cross = @(@{ Schema = $authority; Tables = @('account_role'); Operations = $lock }) }
        player_inbox = @{ Own = @('mmorpg_player_inbox'); Cross = @(@{ Schema = $authority; Tables = @('account_role', 'account_role_admission', 'player_route'); Operations = $lock }) }
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
    $path = Join-Path $PrivateRoot 'physical-authority.original'
    if (-not (Test-Path -LiteralPath $path)) {
        Write-LocalSqlBootText -Path $path -Text "LOCAL_SQL_AUTHORITY_V1`nendpoint=$($Settings.Endpoint)`nnonce=$(New-LocalSqlBootSecret)`n"
    }
    return Get-MaintenanceSha256 -Path $path
}

# 本机声明,不是真实 I3 报告:writer_authority 只核它的 SHA 与 Request 一致。锁原语按档位定,生产的资格仍由操作者提供。
function Get-LocalSqlBootLockQualification {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)][string]$PrivateRoot)
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

# 为一个实例备齐注册原件:私有配置、环境值、正式 Request 与单条目 roster。不碰数据库。
# 账号每次启动都换新:writer_registry.sql_principal 永久唯一,主体不能复用。
function New-LocalSqlBootInstanceFiles {
    param(
        [Parameter(Mandatory)]$Settings,
        [Parameter(Mandatory)][string]$RepoRoot,
        [Parameter(Mandatory)]$Instance,
        [Parameter(Mandatory)][string]$BootRoot,
        [Parameter(Mandatory)][string]$BootId,
        [Parameter(Mandatory)]$Meta,
        [Parameter(Mandatory)][string]$PhysicalSha256,
        [Parameter(Mandatory)][string]$QualificationFile
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

    $configText = if ($service -ceq 'gateway') {
        New-LocalSqlBootGatewayConfig -Settings $Settings -RepoRoot $RepoRoot -User $user -Password $password
    } else {
        ConvertTo-LocalSqlBootConfig -Settings $Settings -Service $service -Content ([IO.File]::ReadAllText($Instance.ConfigPath)) -User $user -Password $password
    }
    $inputConfig = Join-Path $inputRoot 'runtime.yaml'
    Write-LocalSqlBootText -Path $inputConfig -Text $configText

    $environmentEntries = [Collections.Generic.List[object]]::new()
    $environmentNames = [Collections.Generic.List[string]]::new()
    if ($service -cne 'gateway') {
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
    [IO.File]::Copy($QualificationFile, $qualificationCopy)
    $deploymentId = "local-$BootId-$key"
    $request = [ordered]@{
        requestId = (New-LocalSqlBootIdentity)
        kind = 'REGISTER'
        writer = [ordered]@{
            writerKey = (New-LocalSqlBootIdentity)
            generation = $Meta.Generation
            instanceId = (New-LocalSqlBootIdentity)
            sqlPrincipal = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($principal))
            state = 'ACTIVE'
            registeredMs = [string][DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
        }
        expectedEpoch = [string]$Meta.WriterEpoch
        authoritySchema = $Settings.GlobalSchema
        protectedSchemas = [string[]]$Settings.ProtectedSchemas
        deploymentDigest = (ConvertTo-LocalSqlBootBase64 -Hex $PhysicalSha256)
        deploymentId = $deploymentId
        lockPrimitive = $Settings.LockPrimitive
        lockQualificationSha256 = (ConvertTo-LocalSqlBootBase64 -Hex (Get-MaintenanceSha256 -Path $qualificationCopy))
    }
    $requestFile = Join-Path $inputRoot 'request.json'
    $requestSha256 = Write-MaintenanceImmutableJson -Path $requestFile -Value $request
    $entry = [ordered]@{
        request = $request
        runtimeConfigPath = 'runtime.yaml'
        runtimeConfigSha256 = (ConvertTo-LocalSqlBootBase64 -Hex (Get-MaintenanceSha256 -Path $inputConfig))
    }
    if ($environmentEntries.Count -gt 0) { $entry['runtimeEnvironment'] = [object[]]$environmentEntries.ToArray() }
    $rosterFile = Join-Path $inputRoot 'roster.json'
    $rosterSha256 = Write-MaintenanceImmutableJson -Path $rosterFile -Value ([ordered]@{ format = 'SQL_WRITER_BOOT_ROSTER_V1'; entries = [object[]]@($entry) })

    return [pscustomobject]@{
        InstanceKey = $key; Service = $service; User = $user; Password = $password; Principal = $principal
        InputRoot = $inputRoot; RosterFile = $rosterFile; RosterSha256 = $rosterSha256
        RequestFile = $requestFile; RequestSha256 = $requestSha256; QualificationFile = $qualificationCopy
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
        [Parameter(Mandatory)][hashtable]$Manifest
    )
    $templateRoot = Join-Path $RepoRoot 'deploy/sql'
    $rootAccount = ConvertTo-AuthoritySqlAccount $Settings.RootPrincipal
    $builder = [Text.StringBuilder]::new()
    $operatorDone = $false
    foreach ($item in $Prepared) {
        $tables = @(Get-LocalSqlBootInstanceTables -Grant $Manifest[$item.Service] -TablesBySchema $TablesBySchema)
        $roles = ConvertTo-AuthorityRoleSql -TemplateRoot $templateRoot -Plan @{
            authoritySchema = $Settings.GlobalSchema
            runtimePrincipal = $item.Principal
            registrarPrincipal = $Operators.registrar.principal
            maintenancePrincipal = $Operators.maintenance.principal
            schemaPrincipal = $Settings.RootPrincipal
            registrationRequestFile = $item.RequestFile
            registrationRequestSha256 = $item.RequestSha256
            lockQualificationFile = $item.QualificationFile
            tables = $tables
            schemaDatabases = @($Settings.GlobalSchema)
        }
        if (-not $operatorDone) {
            foreach ($operator in @($Operators.registrar, $Operators.maintenance)) {
                $account = ConvertTo-AuthoritySqlAccount $operator.principal
                $null = $builder.AppendLine("CREATE USER IF NOT EXISTS $account IDENTIFIED BY '$($operator.password)';")
                $null = $builder.AppendLine("ALTER USER $account IDENTIFIED BY '$($operator.password)';")
            }
            foreach ($line in ($roles.operator -split '\r?\n')) {
                if (-not $line.EndsWith(" TO $rootAccount;")) { $null = $builder.AppendLine($line) }
            }
            $operatorDone = $true
        }
        $null = $builder.AppendLine("CREATE USER IF NOT EXISTS $(ConvertTo-AuthoritySqlAccount $item.Principal) IDENTIFIED BY '$($item.Password)';")
        $null = $builder.AppendLine($roles.runtime)
    }
    return $builder.ToString()
}

# writer_authority register 的控制环境。只给注册子进程,不进常驻进程。
function New-LocalSqlBootControlEnvironment {
    param([Parameter(Mandatory)]$Settings, [Parameter(Mandatory)]$Operators, [Parameter(Mandatory)][string]$PhysicalSha256)
    return @{
        MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256 = $PhysicalSha256
        MMORPG_SQL_OPERATOR_PRINCIPAL = $Operators.registrar.principal
        MMORPG_SQL_REGISTRAR_DSN = (New-LocalSqlBootDsn -User $Operators.registrar.user -Password $Operators.registrar.password -Endpoint $Settings.Endpoint -Database $Settings.GlobalSchema)
        MMORPG_SQL_BACKEND = $Settings.Backend
    }
}

# 正式注册:每实例一次,单条目 roster。控制凭据只在这一个子进程的环境里;常驻进程拿不到(Enter-SqlWriterBootEnvironment 也会清)。
function Register-LocalSqlBootInstance {
    param([Parameter(Mandatory)]$Item, [Parameter(Mandatory)][string]$Tool, [Parameter(Mandatory)][hashtable]$ControlEnvironment, [Parameter(Mandatory)][string]$PhysicalSha256)
    $arguments = @(
        '-command', 'register'
        '-roster', $Item.RosterFile
        '-roster-sha256', $Item.RosterSha256
        '-input-root', $Item.InputRoot
        '-lock-qualification', $Item.QualificationFile
        '-runtime-config-output', $Item.RuntimeFile
        '-boot-output', $Item.GrantFile
        '-deployment-id', $Item.DeploymentId
        '-timeout', '30s'
    )
    if ($Item.EnvironmentNames.Count -gt 0) { $arguments += @('-runtime-env-output', $Item.EnvironmentDirectory) }
    $result = Invoke-LocalSqlBootProcess -FilePath $Tool -Arguments $arguments -Environment $ControlEnvironment -TimeoutSeconds 60
    if ($result.Code -ne 0) { throw "writer_authority register failed for $($Item.InstanceKey) (exit code $($result.Code)): $(ConvertTo-LocalSqlBootSafeText -Text ([string]$result.Err))" }
    $grantSha256 = Get-MaintenanceSha256 -Path $Item.GrantFile
    if (([IO.File]::ReadAllText("$($Item.GrantFile).sha256")).Trim() -cne $grantSha256) { throw "Grant digest mismatch for $($Item.InstanceKey)." }
    $environmentEntries = @(foreach ($name in $Item.EnvironmentNames) {
            $valueFile = Join-Path $Item.EnvironmentDirectory $name
            [ordered]@{ name = $name; valueFile = $valueFile; valueSha256 = (Get-MaintenanceSha256 -Path $valueFile) }
        })
    # 输入里有明文口令,选中副本已写出,不再留第二份。
    Remove-Item -LiteralPath $Item.InputRoot -Recurse -Force
    return [ordered]@{
        grantFile = $Item.GrantFile
        grantSha256 = $grantSha256
        consumeDir = $Item.ConsumeDirectory
        deploymentId = $Item.DeploymentId
        physicalAuthoritySha256 = $PhysicalSha256
        runtimeConfigFile = $Item.RuntimeFile
        runtimeConfigSha256 = (Get-MaintenanceSha256 -Path $Item.RuntimeFile)
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
# 仓库目录通常继承 Authenticated Users 的修改权限,过不了;%LOCALAPPDATA% 只属于当前账号,放这里,不改仓库目录的 ACL。
function Install-LocalSqlBootPrivateFileHelper {
    param([Parameter(Mandatory)][string]$RepoRoot)
    $source = Resolve-LocalSqlBootTool -RepoRoot $RepoRoot -Name 'private_control_file'
    $directory = [IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'mmorpg/sql-boot-local'))
    $null = [IO.Directory]::CreateDirectory($directory)
    $target = Join-Path $directory 'private_control_file.exe'
    $sha256 = Get-MaintenanceSha256 -Path $source
    if (-not (Test-Path -LiteralPath $target -PathType Leaf) -or (Get-MaintenanceSha256 -Path $target) -cne $sha256) { Copy-Item -LiteralPath $source -Destination $target -Force }
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

function New-LocalSqlBootCatalog {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$RepoRoot,
        [string[]]$Services = @(),
        [string]$Endpoint = '',
        [ValidateSet('auto', 'mysql', 'tidb')][string]$Backend = 'auto'
    )
    if (-not $IsWindows) { throw 'sql-boot-catalog-local only supports Windows hosts.' }
    $repo = [IO.Path]::GetFullPath($RepoRoot)
    $requested = @(Resolve-LocalSqlBootRequestedServices -Services $Services)
    # 回环校验先于任何子进程和落盘。
    $settings = Resolve-LocalSqlBootSettings -RepoRoot $repo -Endpoint $Endpoint

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

        Initialize-LocalSqlBootDatabases -Settings $settings -RepoRoot $repo -Requested $requested
        $skipped = @(Invoke-LocalSqlBootMigrations -Settings $settings -RepoRoot $repo -Requested $requested -WorkDirectory $schemaWork)
        $meta = Get-LocalSqlBootRouteMeta -Settings $settings
        $tablesBySchema = Get-LocalSqlBootTablesBySchema -Settings $settings

        $instances = @(Get-LocalSqlBootInstancePlan -Settings $settings -RepoRoot $repo -Requested @($requested | Where-Object { $_ -cnotin $skipped }) -WorkDirectory $bootRoot)
        $pending = @($instances | Where-Object { -not $_.Running })

        $accountsPath = Join-Path $root 'runtime-accounts.json'
        $knownAccounts = @(if (Test-Path -LiteralPath $accountsPath) { Get-Content -LiteralPath $accountsPath -Raw | ConvertFrom-Json })
        $accountPlan = Get-LocalSqlBootStaleAccounts -Accounts $knownAccounts -Instances $instances

        $helper = if ('gateway' -cin $requested) { Install-LocalSqlBootPrivateFileHelper -RepoRoot $repo } else { $null }
        $catalog = [ordered]@{}
        if ($pending.Count -gt 0) {
            $physical = Get-LocalSqlBootPhysicalAuthority -Settings $settings -PrivateRoot $root
            $qualification = Get-LocalSqlBootLockQualification -Settings $settings -PrivateRoot $root
            $operators = Get-LocalSqlBootOperators -PrivateRoot $root
            $prepared = @(foreach ($instance in $pending) {
                    New-LocalSqlBootInstanceFiles -Settings $settings -RepoRoot $repo -Instance $instance -BootRoot $bootRoot -BootId $bootId -Meta $meta -PhysicalSha256 $physical -QualificationFile $qualification
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
            Write-LocalSqlBootText -Path $catalogPath -Text (ConvertTo-Json -InputObject $catalog -Depth 8)
            Assert-LocalSqlBootCatalog -CatalogPath $catalogPath -RepoRoot $repo -Prepared $prepared
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
        Write-LocalSqlBootText -Path $summaryPath -Text (ConvertTo-Json -InputObject $summary -Depth 4)

        # 只留本轮:运行中的实例启动后不再读这些文件,旧授权已没有用处。
        foreach ($old in @(Get-ChildItem -LiteralPath $bootsRoot -Directory | Where-Object { $_.Name -cne $bootId })) {
            try { Remove-Item -LiteralPath $old.FullName -Recurse -Force } catch { Write-Warning "Could not remove the old boot directory $($old.Name): $($_.Exception.Message)" }
        }
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

if ($MyInvocation.InvocationName -ne '.') {
    $ErrorActionPreference = 'Stop'
    $null = New-LocalSqlBootCatalog -RepoRoot (Join-Path $PSScriptRoot '../..') -Services $Services -Endpoint $Endpoint -Backend $Backend
}
