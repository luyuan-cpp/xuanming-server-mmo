#requires -Version 7
<#
.SYNOPSIS
    docker 调试栈写者注册步骤(lib/sql_boot_catalog_docker.ps1)与 compose 接线的合同回归。
.DESCRIPTION
    不连真库、不起容器:mysql、writer_authority、chown 全走进程桩。角色模板和网关配置用仓库原件,
    容器配置是 prepare_docker_debug.py 产物同形状的最小夹具,产物只写临时目录。compose 和脚本只做文本合同。
.EXAMPLE
    pwsh -File tools/scripts/tests/docker_debug_sql_boot.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/sql_boot_catalog_docker.ps1"

$realRepo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-sql-boot-docker-tests-' + [guid]::NewGuid().ToString('N'))
$authorityTables = @(Get-LocalSqlBootAuthorityTables)
# compose 服务名 → 写者清单里的服务名。
$composeWriters = [ordered]@{ db = 'db'; 'data-service' = 'data_service'; login = 'login'; match = 'match'; 'player-inbox' = 'player_inbox'; gateway = 'gateway' }

function Reset-Fixture {
    $script:Calls = [Collections.Generic.List[object]]::new()
    $script:SqlScripts = [Collections.Generic.List[string]]::new()
    $script:GlobalTables = @($authorityTables) + @('account_role', 'account_role_admission', 'player_route', 'zone_merged_into')
    $script:LiveSessions = '0'
}

function Get-FixtureTableRows {
    $rows = [Collections.Generic.List[string]]::new()
    foreach ($table in $script:GlobalTables) { $rows.Add("testdb`t$table") }
    $business = [ordered]@{
        mmorpg = @('zone_config', 'zone_whitelist', 'announcement')
        mmorpg_guild = @('guild_trial_launch', 'guild_trial_launch_role', 'guild_trial_lobby', 'guild_trial_lobby_slot', 'schema_migrations')
        mmorpg_match = @('match_authority_ticket', 'match_battle_completion', 'team_launch', 'team_launch_role', 'schema_migrations')
        mmorpg_player_inbox = @('player_inbox_stream', 'player_inbox_item', 'player_inbox_audit', 'schema_migrations')
        zone_1_db = @('player_store', 'user_accounts', 'schema_migrations')
    }
    foreach ($schema in $business.Keys) { foreach ($table in $business[$schema]) { $rows.Add("$schema`t$table") } }
    return ($rows -join "`n")
}

function Get-FixtureSqlReply {
    param([string]$Sql)
    $script:SqlScripts.Add($Sql)
    $out = ''
    if ($Sql -match 'SELECT VERSION') { $out = "8.4.0`tfixture server`n" }
    elseif ($Sql -match "table_schema='testdb'") { $out = (@($script:GlobalTables) -join "`n") }
    elseif ($Sql -match 'route_meta` WHERE id=1') { $out = "abcdef0123456789abcdef0123456789`t1`t7`t0`n" }
    elseif ($Sql -match 'SELECT table_schema,table_name') { $out = Get-FixtureTableRows }
    elseif ($Sql -match 'performance_schema\.threads') { $out = "$($script:LiveSessions)`n" }
    return [pscustomobject]@{ Code = 0; Out = $out; Err = '' }
}

# 与 writer_authority register 同一输入输出形状:核 roster SHA,抄选中配置与环境值,写 16 字段 grant 和 .sha256。
function Invoke-FixtureRegister {
    param([string[]]$Arguments)
    $map = @{}
    for ($i = 0; $i -lt $Arguments.Count - 1; $i += 2) { $map[$Arguments[$i]] = $Arguments[$i + 1] }
    $rosterSha = (Get-FileHash -LiteralPath $map['-roster'] -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($rosterSha -cne $map['-roster-sha256']) { return [pscustomobject]@{ Code = 1; Out = ''; Err = 'roster sha mismatch' } }
    $roster = Get-Content -LiteralPath $map['-roster'] -Raw | ConvertFrom-Json -AsHashtable
    $entry = $roster.entries[0]
    $request = $entry.request
    [IO.File]::WriteAllBytes($map['-runtime-config-output'], [IO.File]::ReadAllBytes((Join-Path $map['-input-root'] $entry.runtimeConfigPath)))
    foreach ($variable in @($entry.runtimeEnvironment)) {
        if ($null -eq $variable) { continue }
        [IO.File]::WriteAllBytes((Join-Path $map['-runtime-env-output'] $variable.name), [IO.File]::ReadAllBytes((Join-Path $map['-input-root'] $variable.valuePath)))
    }
    $grant = [ordered]@{
        format = 'SQL_WRITER_BOOT_V1'; writerKey = $request.writer.writerKey; instanceId = $request.writer.instanceId; generation = $request.writer.generation
        authoritySchema = $request.authoritySchema; protectedSchemas = $request.protectedSchemas; sqlPrincipal = $request.writer.sqlPrincipal
        registrationDigest = [Convert]::ToBase64String([byte[]]::new(32)); deploymentId = $map['-deployment-id']; grantId = $request.requestId
        grantedMs = $request.writer.registeredMs; permission = 'NORMAL'; physicalAuthoritySha256 = $request.deploymentDigest
        lockPrimitive = $request.lockPrimitive; lockQualificationSha256 = $request.lockQualificationSha256
    }
    [IO.File]::WriteAllText($map['-boot-output'], (ConvertTo-Json -InputObject $grant -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
    $grantSha = (Get-FileHash -LiteralPath $map['-boot-output'] -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$($map['-boot-output']).sha256", "$grantSha`n")
    return [pscustomobject]@{ Code = 0; Out = 'writer_authority command=register confirmed=true'; Err = '' }
}

function Invoke-LocalSqlBootProcess {
    param([string]$FilePath, [string[]]$Arguments = @(), [Collections.IDictionary]$Environment = @{}, [string]$WorkingDirectory = '', [string]$StandardInput = '', [int]$TimeoutSeconds = 120)
    $leaf = [IO.Path]::GetFileName($FilePath)
    $environmentCopy = @{}
    foreach ($key in @($Environment.Keys)) { $environmentCopy[$key] = $Environment[$key] }
    $script:Calls.Add([pscustomobject]@{ Leaf = $leaf; Arguments = $Arguments; Environment = $environmentCopy })
    switch -CaseSensitive ($leaf) {
        'mysql' { return Get-FixtureSqlReply -Sql $StandardInput }
        'chown' { return [pscustomobject]@{ Code = 0; Out = ''; Err = '' } }
        'writer_authority' { return Invoke-FixtureRegister -Arguments $Arguments }
    }
    throw "Unexpected process $leaf"
}

# prepare_docker_debug.py 产物同形状的最小配置:常驻配置不带账号。
function New-FixturePaths {
    $root = Join-Path $fixtureRoot ([guid]::NewGuid().ToString('N'))
    $paths = [pscustomobject]@{
        RepoRoot = $realRepo
        ConfigRoot = (Join-Path $root 'config')
        PrivateRoot = (Join-Path $root 'private')
        StateRoot = (Join-Path $root 'state')
        BootRoot = (Join-Path $root 'boot')
        ClientRoot = (Join-Path $root 'client')
        Tool = (Join-Path $root 'writer_authority')
    }
    foreach ($directory in @($paths.ConfigRoot, $paths.PrivateRoot, $paths.StateRoot, $paths.BootRoot, $paths.ClientRoot)) { $null = New-Item -ItemType Directory -Path $directory -Force }
    Set-Content -LiteralPath (Join-Path $paths.ConfigRoot 'db.yaml') -Value "ZoneId: 1`nServerConfig:`n  Database:`n    Backend: mysql`n    Hosts: mysql:3306`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $paths.ConfigRoot 'data_service.yaml') -Value "SnapshotMySQL:`n  Backend: mysql`n  Host: mysql:3306`n  DBName: testdb`nSchema:`n  AutoMigrate: false`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $paths.ConfigRoot 'login.yaml') -Value "Node:`n  RedisClient:`n    Host: redis:6379`n    DB: 0`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $paths.ConfigRoot 'match.yaml') -Value "LoginRedis:`n  Addrs: [redis:6379]`n  DB: 0`nGlobalMatch:`n  Backend: mysql`n  Host: mysql:3306`n  User: root`n  AccountRoleSchema: testdb`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $paths.ConfigRoot 'player_inbox.yaml') -Value "AccountRoleSchema: testdb`nMySQL:`n  Host: mysql:3306`n  DBName: mmorpg_player_inbox`n  Backend: mysql`nSchema:`n  AutoMigrate: false`n" -NoNewline
    return $paths
}

function Get-ThrownMessage([scriptblock]$Action) { try { & $Action | Out-Null } catch { return $_.Exception.Message }; return '' }
function Get-RegisterCalls { return @($script:Calls | Where-Object { $_.Leaf -ceq 'writer_authority' }) }
function Get-ArgumentValue { param($Call, [string]$Name) return $Call.Arguments[[array]::IndexOf($Call.Arguments, $Name) + 1] }
function Get-SqlText { return ($script:SqlScripts -join "`n") }
function Get-BootUser {
    param($Paths, [string]$Service)
    $grant = Read-MaintenanceJson -Path (Join-Path $Paths.BootRoot "$Service/grant/boot-grant.json")
    return ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($grant.sqlPrincipal))).Split('@')[0]
}

# 取 compose 里一个服务的整段文本:到下一个同级键或顶层键为止。
function Get-ComposeBlock {
    param([string]$Text, [string]$Name)
    $match = [regex]::Match($Text, "(?ms)^  $([regex]::Escape($Name)):[ ]*\r?\n.*?(?=^  [^ #\r\n]|^[^ #\r\n]|\z)")
    if (-not $match.Success) { throw "compose 没有服务 $Name" }
    return $match.Value
}

$savedEnvironment = @{}
$touchedVariables = @('MYSQL_ROOT_PASSWORD', 'DB_ALLOWED_DATABASES', 'MMORPG_DB_ALLOWED_DATABASES')
foreach ($name in $touchedVariables) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    [Environment]::SetEnvironmentVariable($name, $null, 'Process')
}
[Environment]::SetEnvironmentVariable('MYSQL_ROOT_PASSWORD', 'fixture-root-password', 'Process')
[Environment]::SetEnvironmentVariable('DB_ALLOWED_DATABASES', 'zone_1_db', 'Process')
try {
    $null = New-Item -ItemType Directory -Path $fixtureRoot -Force
    Reset-Fixture

    Test-Case 'prepare:补库和网关的表,生成共用的口令和原件,客户端选项文件不留' {
        Reset-Fixture
        $script:GlobalTables = @()
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        $sql = Get-SqlText
        Assert-Match $sql 'CREATE DATABASE IF NOT EXISTS `zone_1_db`' '补玩家库'
        Assert-Match $sql 'CREATE DATABASE IF NOT EXISTS `testdb`' '补全局库'
        Assert-Match $sql 'CREATE TABLE IF NOT EXISTS zone_config' '网关的表来自 deploy/mysql-init'
        foreach ($file in @('operators.json', 'physical-authority.original', 'lock-qualification.mysql.LOCK_TOUCH.json')) {
            Assert-True (Test-Path -LiteralPath (Join-Path $paths.PrivateRoot $file)) "生成 $file"
        }
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $paths.ClientRoot 'mysql-client.cnf'))) 'root 口令不留盘'
        $mysql = @($script:Calls | Where-Object { $_.Leaf -ceq 'mysql' })
        Assert-True ($mysql.Count -gt 0) '走容器内 mysql 客户端'
        Assert-Match $mysql[0].Arguments[0] '^--defaults-extra-file=' '口令不上命令行'
    }
    Test-Case 'prepare:再跑一次沿用口令和原件' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        $first = [IO.File]::ReadAllText((Join-Path $paths.PrivateRoot 'operators.json'))
        $firstPhysical = Get-MaintenanceSha256 -Path (Join-Path $paths.PrivateRoot 'physical-authority.original')
        Invoke-DockerSqlBootPrepare -Paths $paths
        Assert-Equal $first ([IO.File]::ReadAllText((Join-Path $paths.PrivateRoot 'operators.json'))) '口令不轮换'
        Assert-Equal $firstPhysical (Get-MaintenanceSha256 -Path (Join-Path $paths.PrivateRoot 'physical-authority.original')) '物理 authority 不变'
    }
    Test-Case 'prepare:空库不留标记,权威表齐全留标记,半成品拒绝且不留标记' {
        Reset-Fixture
        $paths = New-FixturePaths
        $marker = Join-Path $paths.StateRoot 'global-schema-ready'
        $script:GlobalTables = @()
        Invoke-DockerSqlBootPrepare -Paths $paths
        Assert-True (-not (Test-Path -LiteralPath $marker)) '空库要跑 -migrate'
        $script:GlobalTables = @($authorityTables)
        Invoke-DockerSqlBootPrepare -Paths $paths
        Assert-True (Test-Path -LiteralPath $marker) '齐全时 data-migrate 跳过'
        $script:GlobalTables = @()
        Invoke-DockerSqlBootPrepare -Paths $paths
        Assert-True (-not (Test-Path -LiteralPath $marker)) '卷被清空后回到要跑 -migrate'
        $script:GlobalTables = @('route_meta')
        $message = Get-ThrownMessage { Invoke-DockerSqlBootPrepare -Paths $paths }
        Assert-Match $message 'lacks the authority set' '半成品拒绝,不替人删数据'
        Assert-Match $message 'down -v' '告诉怎么重来'
        Assert-True (-not (Test-Path -LiteralPath $marker)) '不留标记'
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $paths.ClientRoot 'mysql-client.cnf'))) '失败也不留 root 口令'
    }
    Test-Case '缺 root 口令拒绝' {
        Reset-Fixture
        $paths = New-FixturePaths
        [Environment]::SetEnvironmentVariable('MYSQL_ROOT_PASSWORD', $null, 'Process')
        try { Assert-Match (Get-ThrownMessage { Invoke-DockerSqlBootPrepare -Paths $paths }) 'MYSQL_ROOT_PASSWORD' '明确报缺什么' }
        finally { [Environment]::SetEnvironmentVariable('MYSQL_ROOT_PASSWORD', 'fixture-root-password', 'Process') }
    }
    Test-Case 'register:清单外的服务拒绝,不碰卷和数据库' {
        Reset-Fixture
        $paths = New-FixturePaths
        Assert-Match (Get-ThrownMessage { Invoke-DockerSqlBootRegister -Paths $paths -Service 'nope' }) 'Unknown writer service' '只收写者清单'
        Assert-Equal 0 $script:Calls.Count '没有任何子进程'
    }

    foreach ($service in @($composeWriters.Values)) {
        Test-Case "register ${service}:走正式 roster 注册,产物在卷内,输入不留,交给 10001" {
            Reset-Fixture
            $paths = New-FixturePaths
            Invoke-DockerSqlBootPrepare -Paths $paths
            Invoke-DockerSqlBootRegister -Paths $paths -Service $service
            $instanceRoot = Join-Path $paths.BootRoot $service
            $calls = @(Get-RegisterCalls)
            Assert-Equal 1 $calls.Count '一次注册'
            $call = $calls[0]
            Assert-Equal 'register' (Get-ArgumentValue $call '-command') '正式 register'
            Assert-Equal (Join-Path (Join-Path $instanceRoot 'runtime') "$service.yaml") (Get-ArgumentValue $call '-runtime-config-output') '运行配置路径和 compose 的 command 对应'
            Assert-Equal (Join-Path (Join-Path $instanceRoot 'grant') 'boot-grant.json') (Get-ArgumentValue $call '-boot-output') '授权路径和入口脚本对应'
            Assert-Equal 'mysql' $call.Environment['MMORPG_SQL_BACKEND'] 'compose 的 mysql 是 MySQL 档'
            Assert-Equal 'mmorpg_local_registrar@%' $call.Environment['MMORPG_SQL_OPERATOR_PRINCIPAL'] '注册账号'
            Assert-Match $call.Environment['MMORPG_SQL_REGISTRAR_DSN'] '@tcp\(mysql:3306\)/testdb\?' '连 compose 的 mysql 与全局库'

            $grant = Read-MaintenanceJson -Path (Join-Path $instanceRoot 'grant/boot-grant.json')
            Assert-Equal $grant.deploymentId ([IO.File]::ReadAllText((Join-Path $instanceRoot 'grant/deployment-id'))) '部署身份给入口脚本读'
            $physical = [IO.File]::ReadAllText((Join-Path $instanceRoot 'grant/physical-authority-sha256'))
            Assert-Equal ([Convert]::ToHexString([Convert]::FromBase64String($grant.physicalAuthoritySha256)).ToLowerInvariant()) $physical '物理 authority 摘要给入口脚本读'
            Assert-True (Test-Path -LiteralPath (Join-Path $instanceRoot "runtime/$service.yaml")) '运行配置写进卷'
            Assert-True (Test-Path -LiteralPath (Join-Path $instanceRoot 'consume')) '消费目录在'
            Assert-True (-not (Test-Path -LiteralPath (Join-Path $instanceRoot 'input'))) '输入里有明文口令,不留'
            $chown = @($script:Calls | Where-Object { $_.Leaf -ceq 'chown' })
            Assert-Equal 1 $chown.Count '只交一次'
            Assert-Equal "-R 10001:10001 $instanceRoot" ($chown[0].Arguments -join ' ') '整棵树交给常驻容器的账号'
            $leaves = [string[]]@($script:Calls | ForEach-Object { $_.Leaf })
            Assert-True ([array]::IndexOf($leaves, 'chown') -gt [array]::IndexOf($leaves, 'writer_authority')) '注册之后才交'
            Assert-True (-not (Test-Path -LiteralPath (Join-Path $paths.ClientRoot 'mysql-client.cnf'))) 'root 口令不留盘'
        }
    }
    Test-Case '运行配置连 compose 的 mysql,账号是这一 boot 的,控制凭据不进运行配置' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        foreach ($service in @($composeWriters.Values)) { Invoke-DockerSqlBootRegister -Paths $paths -Service $service }
        $data = Join-Path $paths.BootRoot 'data_service/runtime/data_service.yaml'
        Assert-Equal 'mysql:3306' (Get-YamlScalar -Path $data -KeyPath 'SnapshotMySQL.Host').Value 'data_service 端点'
        Assert-Equal (Get-BootUser $paths 'data_service') (Get-YamlScalar -Path $data -KeyPath 'SnapshotMySQL.User').Value 'data_service 账号'
        $db = Join-Path $paths.BootRoot 'db/runtime/db.yaml'
        Assert-Equal 'mysql:3306' (Get-YamlScalar -Path $db -KeyPath 'ServerConfig.Database.Hosts').Value 'db 端点'
        Assert-Equal (Get-BootUser $paths 'db') (Get-YamlScalar -Path $db -KeyPath 'ServerConfig.Database.User').Value 'db 账号'
        $match = Join-Path $paths.BootRoot 'match/runtime/match.yaml'
        Assert-Equal (Get-BootUser $paths 'match') (Get-YamlScalar -Path $match -KeyPath 'GlobalMatch.User').Value 'match 账号'
        Assert-Equal '["redis:6379"]' (Get-YamlScalar -Path $match -KeyPath 'LoginRedis.Addrs').Value '登录 Redis 取容器地址'
        Assert-Equal 'testdb' (Get-YamlScalar -Path $match -KeyPath 'GlobalMatch.AccountRoleSchema').Value '全局库与 data_service 同库'
        $inbox = Join-Path $paths.BootRoot 'player_inbox/runtime/player_inbox.yaml'
        Assert-Equal 'mysql:3306' (Get-YamlScalar -Path $inbox -KeyPath 'MySQL.Host').Value 'player_inbox 端点'
        Assert-Equal (Get-BootUser $paths 'player_inbox') (Get-YamlScalar -Path $inbox -KeyPath 'MySQL.User').Value 'player_inbox 账号'
        Assert-Equal 'testdb' (Get-YamlScalar -Path $inbox -KeyPath 'AccountRoleSchema').Value '角色目录与 data_service 同库'
        $sql = Get-SqlText
        $matchUser = Get-BootUser $paths 'match'
        $password = [regex]::Match($sql, "CREATE USER IF NOT EXISTS '$matchUser'@'%' IDENTIFIED BY '([0-9a-f]+)'").Groups[1].Value
        Assert-True ($password.Length -eq 64) '随机口令'
        Assert-Equal $password ([IO.File]::ReadAllText((Join-Path $paths.BootRoot 'match/env/MATCH_MYSQL_PASSWORD'))) 'match 口令走白名单变量,不进配置'
        $gateway = [IO.File]::ReadAllText((Join-Path $paths.BootRoot 'gateway/runtime/gateway.yaml'))
        Assert-Match $gateway '//mysql:3306/mmorpg\?' '网关端点'
        Assert-Match $gateway 'run-mode: dev' 'MySQL 档要 dev'
        Assert-Match $gateway 'require-tidb: false' 'MySQL 档不要 TiDB 强核验'
        $operators = Get-Content -LiteralPath (Join-Path $paths.PrivateRoot 'operators.json') -Raw | ConvertFrom-Json
        foreach ($service in @($composeWriters.Values)) {
            $text = [IO.File]::ReadAllText((Join-Path $paths.BootRoot "$service/runtime/$service.yaml"))
            Assert-True (-not $text.Contains($operators.registrar.password)) "$service 配置不含注册账号口令"
            Assert-True (-not $text.Contains('fixture-root-password')) "$service 配置不含 root 口令"
        }
    }
    Test-Case '账号按 deploy/sql 模板建:逐表授权,runtime 无 DDL,不给 root 授权' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'data_service'
        $sql = Get-SqlText
        $user = Get-BootUser $paths 'data_service'
        Assert-Match $sql "CREATE USER IF NOT EXISTS 'mmorpg_local_registrar'@'%'" '注册账号'
        Assert-Match $sql "CREATE USER IF NOT EXISTS '$user'@'%'" '这一 boot 的账号'
        Assert-NotMatch $sql "TO 'root'@'%'" 'schema 身份就是 root,不再授权'
        $block = $sql.Substring($sql.IndexOf("CREATE USER IF NOT EXISTS '$user'@'%'"))
        Assert-Match $block 'GRANT SELECT ON `testdb`\.`route_meta`' '权限真值表只读'
        Assert-Match $block 'GRANT UPDATE \(lock_touch\) ON `testdb`\.`writer_slot`' 'MySQL 档用 LOCK_TOUCH'
        Assert-Match $block 'GRANT SELECT,INSERT,UPDATE,DELETE ON `zone_1_db`\.`player_store`' '清单里的跨库表逐表授权'
        Assert-NotMatch $block 'GRANT [^\r\n]*(CREATE|ALTER|DROP|INDEX)' 'runtime 没有 DDL'
        Assert-NotMatch $block 'ON `[a-z0-9_]+`\.\*' '没有 schema.*'
    }
    Test-Case '再次注册:全新身份,上一轮产物和消费标记清掉,消费目录本身留着(它是单独的卷)' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        $instanceRoot = Join-Path $paths.BootRoot 'db'
        $firstGrant = Read-MaintenanceJson -Path (Join-Path $instanceRoot 'grant/boot-grant.json')
        $firstUser = Get-BootUser $paths 'db'
        Set-Content -LiteralPath (Join-Path $instanceRoot 'consume/grant.consumed') -Value 'consumed'
        Set-Content -LiteralPath (Join-Path $instanceRoot 'grant/stale') -Value 'stale'
        $null = New-Item -ItemType Directory -Path (Join-Path $instanceRoot 'input') -Force
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        $secondGrant = Read-MaintenanceJson -Path (Join-Path $instanceRoot 'grant/boot-grant.json')
        Assert-True ($firstGrant.grantId -cne $secondGrant.grantId) '授权一次一换'
        Assert-True ($firstGrant.writerKey -cne $secondGrant.writerKey) '写者身份一次一换'
        Assert-True ($firstUser -cne (Get-BootUser $paths 'db')) '账号一次一换:sql_principal 永久唯一'
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $instanceRoot 'consume/grant.consumed'))) '消费标记清掉'
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $instanceRoot 'grant/stale'))) '残留文件清掉'
        Assert-True (Test-Path -LiteralPath (Join-Path $instanceRoot 'consume')) '挂载点留着'
    }
    Test-Case '旧账号:没有连接才删,还有连接(服务还在跑)就留着' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        $first = Get-BootUser $paths 'db'
        $script:LiveSessions = '2'
        $script:SqlScripts.Clear()
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        $second = Get-BootUser $paths 'db'
        Assert-NotMatch (Get-SqlText) 'DROP USER' '还在跑的不删'
        $accounts = @(Get-Content -LiteralPath (Join-Path $paths.PrivateRoot 'runtime-accounts.db.json') -Raw | ConvertFrom-Json)
        Assert-Equal 2 $accounts.Count '两轮都记着'
        $script:LiveSessions = '0'
        $script:SqlScripts.Clear()
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        $sql = Get-SqlText
        Assert-Match $sql "DROP USER IF EXISTS '$first'@'%'" '停了就删'
        Assert-Match $sql "DROP USER IF EXISTS '$second'@'%'" '停了就删'
        $accounts = @(Get-Content -LiteralPath (Join-Path $paths.PrivateRoot 'runtime-accounts.db.json') -Raw | ConvertFrom-Json)
        Assert-Equal 1 $accounts.Count '只记这一轮'
    }
    Test-Case '账号记录每服务一份:并行注册不互相覆盖' {
        Reset-Fixture
        $paths = New-FixturePaths
        Invoke-DockerSqlBootPrepare -Paths $paths
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'db'
        Invoke-DockerSqlBootRegister -Paths $paths -Service 'match'
        foreach ($service in @('db', 'match')) {
            $accounts = @(Get-Content -LiteralPath (Join-Path $paths.PrivateRoot "runtime-accounts.$service.json") -Raw | ConvertFrom-Json)
            Assert-Equal 1 $accounts.Count "$service 一条"
            Assert-Equal $service $accounts[0].instanceKey "$service 的记录"
        }
        Assert-NotMatch (Get-SqlText) 'DROP USER' '别的服务的账号不动'
    }
    Test-Case '本机入口的回环门禁没放宽' {
        Assert-Match (Get-ThrownMessage { Assert-LocalSqlBootEndpoint -Endpoint 'mysql:3306' }) 'loopback' 'compose 的端点只由 docker 入口写死,不能经本机入口传入'
    }

    $composeText = [IO.File]::ReadAllText((Join-Path $realRepo 'deploy/debug/compose.game.yml'))
    $debugText = [IO.File]::ReadAllText((Join-Path $realRepo 'deploy/debug/compose.debug.yml'))
    $prepareText = [IO.File]::ReadAllText((Join-Path $realRepo 'tools/scripts/prepare_docker_debug.py'))
    $rebuildText = [IO.File]::ReadAllText((Join-Path $realRepo 'tools/scripts/rebuild_docker_debug.py'))
    $buildText = [IO.File]::ReadAllText((Join-Path $realRepo 'tools/scripts/build_docker_go_debug.py'))

    foreach ($composeName in @($composeWriters.Keys)) {
        $service = $composeWriters[$composeName]
        Test-Case "compose ${composeName}:启动前有一次性注册,按容器内路径读授权" {
            Assert-True ($service -cin @(Get-LocalSqlBootWriterServices)) '在写者清单里'
            $boot = Get-ComposeBlock $composeText "$composeName-sql-boot"
            Assert-Match $boot '<<: \*sql-boot' '公共部分'
            Assert-True ($boot.Contains("command: [-Step, register, -WriterService, $service]")) '注册这个服务'
            Assert-True ($boot.Contains("$composeName-sql-writer:/run/sql-boot/boot/$service")) '产物写进它的卷'
            Assert-True ($boot.Contains("$composeName-sql-consume:/run/sql-boot/boot/$service/consume")) '消费标记的卷也要清'
            Assert-Match $boot 'depends_on: \*sql-boot-deps' '排在建表之后'
            $runtime = Get-ComposeBlock $composeText $composeName
            Assert-True ($runtime.Contains('entrypoint: [/bin/sh, /sql-writer-entry.sh]')) '入口脚本取摘要和变量'
            Assert-True ($runtime.Contains("/run/sql-writer/runtime/$service.yaml")) '读每 boot 配置'
            Assert-True ($runtime.Contains("$composeName-sql-writer:/run/sql-writer:ro")) '授权只读'
            Assert-True ($runtime.Contains("$composeName-sql-consume:/run/sql-writer/consume")) '消费标记可写'
            Assert-Match $runtime "(?s)$composeName-sql-boot:\s+condition: service_completed_successfully" '注册成功才启动'
            Assert-NotMatch $runtime 'config/(db|data_service|login|match|player_inbox)\.yaml' '不再读不带账号的配置'
            foreach ($volume in @("$composeName-sql-writer", "$composeName-sql-consume")) {
                Assert-Match $composeText "(?m)^  $([regex]::Escape($volume)):\s*$" "声明卷 $volume"
            }
        }
    }
    Test-Case 'compose:注册排在全部建表之后,没有共享账号,口令不经 compose 环境' {
        $deps = [regex]::Match($composeText, '(?ms)^x-sql-boot-deps: &sql-boot-deps\r?\n.*?(?=^\S|\z)').Value
        foreach ($name in @('sql-boot', 'db-migrate', 'data-migrate', 'match-migrate', 'guild-migrate', 'player-inbox-migrate')) {
            Assert-Match $deps "(?s)$name`:\s+condition: service_completed_successfully" "等 $name"
        }
        Assert-NotMatch $composeText 'appuser' '不留共享账号'
        Assert-NotMatch $composeText '(?<![A-Z_])MYSQL_PASSWORD' '不留共享账号口令'
        Assert-NotMatch $composeText '(?m)^\s+MATCH_MYSQL_PASSWORD:' 'match 口令只来自本 boot 的私有卷'
        Assert-NotMatch $composeText 'MMORPG_SQL_(REGISTRAR|MAINTENANCE)_DSN' '控制凭据不进 compose'
    }
    Test-Case 'compose:建表用 root 配置,data-migrate 见到标记跳过' {
        Assert-True ((Get-ComposeBlock $composeText 'db-migrate').Contains('config/db.schema.yaml')) 'go/db 建表用 root'
        $dataMigrate = Get-ComposeBlock $composeText 'data-migrate'
        Assert-True ($dataMigrate.Contains('config/data_service.schema.yaml')) 'data_service 建表用 root'
        Assert-True ($dataMigrate.Contains('MMORPG_SQL_SCHEMA_PRINCIPAL: root@%')) 'schema 身份'
        Assert-True ($dataMigrate.Contains('/run/sql-boot/state/global-schema-ready')) '标记与 prepare 同名'
        Assert-True ($dataMigrate.Contains('sql-boot-state:/run/sql-boot/state:ro')) '只读状态卷'
        Assert-True (-not $dataMigrate.Contains('sql-boot-private')) '口令卷不给迁移'
    }
    Test-Case 'compose player-inbox:建表用 root,两把调用方密钥都到位,prepare 打开 scene 消费' {
        $migrate = Get-ComposeBlock $composeText 'player-inbox-migrate'
        Assert-True ($migrate.Contains('config/player_inbox.schema.yaml')) '建表用 root 配置'
        Assert-True ($migrate.Contains('MMORPG_SQL_SCHEMA_PRINCIPAL: root@%')) 'schema 身份'
        Assert-True (-not $migrate.Contains('sql-boot-private')) '口令卷不给迁移'
        $runtime = Get-ComposeBlock $composeText 'player-inbox'
        Assert-True ($runtime.Contains('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE: ${MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE:')) 'scene 拉取的验签密钥,缺失拒启'
        Assert-True ($runtime.Contains('MMORPG_PLAYER_INBOX_SECRET_MATCH: ${MMORPG_PLAYER_INBOX_SECRET_MATCH:')) 'match 入箱的验签密钥'
        Assert-True ($runtime.Contains('POD_IP: ${PLAYER_INBOX_IP:')) 'etcd 注册写容器地址'
        Assert-True ((Get-ComposeBlock $composeText 'match').Contains('MMORPG_PLAYER_INBOX_SECRET_MATCH: ${MMORPG_PLAYER_INBOX_SECRET_MATCH:')) 'match 用同一把签名'
        Assert-True ((Get-ComposeBlock $composeText 'scene').Contains('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE: ${MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE:')) 'scene 用同一把签名'
        Assert-Match $prepareText '"player_inbox": "player_inbox\.yaml"' '生成配置'
        Assert-Match $prepareText '"player_inbox": 17' '固定地址,PLAYER_INBOX_IP 由它派生'
        Assert-Match $prepareText 'game\["player_inbox"\]\["consumer_enabled"\] = True' 'scene 真的拉取,否则结算到不了玩家'
        Assert-Match (Get-ComposeBlock $debugText 'player-inbox-migrate') 'entrypoint: \[/app/service\]' '迁移不被调试器包装'
    }
    Test-Case 'K8s player-inbox:scene 消费密钥进容器,缺了服务拒启' {
        $manifest = [IO.File]::ReadAllText((Join-Path $realRepo 'deploy/k8s/manifests/go-svc/player-inbox.yaml'))
        Assert-Match $manifest 'name: MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: account-directory-scene, optional: false\}' '与 match.yaml 同一写法'
    }
    Test-Case 'compose.debug:Go 写者的调试器接在入口脚本后面,data-migrate 不再覆盖入口' {
        foreach ($composeName in @('login', 'db', 'data-service', 'match', 'player-inbox')) {
            $block = Get-ComposeBlock $debugText $composeName
            Assert-True ($block.Contains('command: [/usr/local/bin/dlv, exec, /app/service,')) "$composeName 走 dlv"
            Assert-True ($block.Contains("/run/sql-writer/runtime/$($composeWriters[$composeName]).yaml")) "$composeName 读每 boot 配置"
        }
        Assert-NotMatch $debugText '(?m)^  data-migrate:' '基础文件的入口已经直接跑 /app/service,覆盖会绕过标记'
    }
    Test-Case 'prepare_docker_debug:脚本和模板都复制,常驻配置不留账号,入口脚本读齐五个变量' {
        $sources = [regex]::Match($prepareText, '(?s)SQL_BOOT_SOURCES = \((.*?)\n\)').Groups[1].Value
        $listed = @([regex]::Matches($sources, '"([^"]+)"') | ForEach-Object { $_.Groups[1].Value })
        foreach ($path in $listed) { Assert-True (Test-Path -LiteralPath (Join-Path $realRepo $path)) "源文件存在:$path" }
        foreach ($path in @('tools/scripts/lib/sql_boot_catalog_docker.ps1', 'tools/scripts/lib/sql_boot_catalog_local.ps1', 'tools/scripts/lib/release_common.ps1', 'tools/scripts/maintenance_drain.ps1', 'deploy/sql/authority_runtime_role.sql', 'deploy/sql/authority_operator_role.sql', 'deploy/mysql-init/gateway_tables.sql', 'java/gateway_node/src/main/resources/application.yaml')) {
            Assert-True ($path -cin $listed) "复制 $path"
        }
        Assert-NotMatch $prepareText 'appuser|(?<![A-Z_])MYSQL_PASSWORD' '不留共享账号'
        foreach ($name in @('db.schema.yaml', 'data_service.schema.yaml', 'player_inbox.schema.yaml')) { Assert-True ($prepareText.Contains($name)) "写 $name" }
        foreach ($name in @('MMORPG_SQL_WRITER_GRANT_FILE', 'MMORPG_SQL_WRITER_GRANT_SHA256', 'MMORPG_SQL_WRITER_CONSUME_DIR', 'MMORPG_SQL_DEPLOYMENT_ID', 'MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256')) {
            Assert-Match $prepareText "(?m)^export [^\r\n]*\b$name\b" "入口脚本导出 $name"
        }
        foreach ($name in @('LOGIN', 'TRADE', 'SCENE', 'SCENE_MANAGER')) {
            Assert-True ($prepareText.Contains("""MMORPG_ACCOUNT_DIRECTORY_SECRET_$name""")) "生成账号目录密钥 $name"
        }
    }
    Test-Case '重编入口:写者连注册步骤一起重建,sql-boot 只换镜像' {
        Assert-True ($buildText.Contains('"sql-boot": ("data_service", "./cmd/writer_authority")')) '构建正式 writer_authority'
        $writers = [regex]::Match($rebuildText, 'SQL_WRITERS = \(([^)]*)\)').Groups[1].Value
        $listed = @([regex]::Matches($writers, '"([^"]+)"') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
        Assert-Equal ((@($composeWriters.Keys) | Sort-Object) -join ',') ($listed -join ',') '和 compose 的写者同一份'
        Assert-True ($rebuildText.Contains('(s + "-sql-boot", s)')) '连注册步骤'
        Assert-True ($rebuildText.Contains('"sql-boot": "SQL_BOOT_IMAGE"')) '镜像写回 .env'
        Assert-True ($composeText.Contains('${SQL_BOOT_IMAGE:-')) 'compose 认这个键'
    }
    Test-Case 'Dockerfile.sql-boot 只装运行时,注册步骤用的 pwsh 和 mysql 都在' {
        $dockerfile = [IO.File]::ReadAllText((Join-Path $realRepo 'deploy/debug/Dockerfile.sql-boot'))
        Assert-Match $dockerfile 'mysql-client' '容器内 SQL 客户端'
        Assert-Match $dockerfile '/usr/local/bin/pwsh' '容器内 pwsh'
    }
}
finally {
    foreach ($name in $touchedVariables) { [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process') }
    $resolved = [IO.Path]::GetFullPath($fixtureRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolved) -notlike 'mmorpg-sql-boot-docker-tests-*') { throw '拒绝清理临时测试范围之外的目录。' }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'docker 调试栈写者注册')
