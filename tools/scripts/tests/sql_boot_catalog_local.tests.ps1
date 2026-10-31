#requires -Version 7
<#
.SYNOPSIS
    本机写者启动目录生成器(lib/sql_boot_catalog_local.ps1)的合同回归。
.DESCRIPTION
    不连真库、不起服务:SQL 客户端、writer_authority、各服务 -migrate 和 go_services plan-instances 全走进程桩。
    配置与角色模板用仓库原件(配置形状漂移时这里先红),产物只写临时目录,再用正式的 Get-SqlWriterBootEnvironment 校验。
    只有 icacls 是真实命令,作用范围是临时目录。
.EXAMPLE
    pwsh -File tools/scripts/tests/sql_boot_catalog_local.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/sql_boot_catalog_local.ps1"

$realRepo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-sql-boot-tests-' + [guid]::NewGuid().ToString('N'))
$rootPassword = (Get-YamlScalar -Path (Join-Path $realRepo 'go/db/etc/db.yaml') -KeyPath 'ServerConfig.Database.Passwd').Value
$authorityTables = @(Get-LocalSqlBootAuthorityTables)
$script:Generation = 'abcdef0123456789abcdef0123456789'

$script:RealProcess = ${function:Invoke-LocalSqlBootProcess}

function Reset-Fixture {
    $script:Calls = [Collections.Generic.List[object]]::new()
    $script:SqlScripts = [Collections.Generic.List[string]]::new()
    $script:GlobalTables = @($authorityTables) + @('account_role', 'account_role_admission', 'player_route', 'zone_merged_into', 'player_name')
    $script:ServerVersion = '8.4.0'
    $script:DockerPort = '0.0.0.0:3306'
    $script:RunningKeys = @()
    $script:GatewayRunning = $false
    $script:MigrateCodes = @{}
    $script:DbMigrateCode = 0
}

function Get-FixtureTableRows {
    $rows = [Collections.Generic.List[string]]::new()
    foreach ($table in $script:GlobalTables) { $rows.Add("mmorpg_global`t$table") }
    $business = [ordered]@{
        mmorpg = @('zone_config', 'zone_whitelist', 'announcement')
        mmorpg_chat = @('chat_send_authority', 'chat_policy')
        mmorpg_friend = @('friend', 'friend_request', 'schema_migrations')
        mmorpg_guild = @('guild', 'guild_member', 'guild_asset_op', 'guild_trial_launch', 'guild_trial_launch_role', 'guild_trial_lobby', 'guild_trial_lobby_slot', 'schema_migrations')
        mmorpg_mail = @('mail_message', 'mail_asset_op', 'schema_migrations')
        mmorpg_match = @('match_authority_ticket', 'match_battle_completion', 'team_launch', 'team_launch_role', 'schema_migrations')
        mmorpg_player_inbox = @('player_inbox_item', 'schema_migrations')
        mmorpg_rank = @('rank_score', 'schema_migrations')
        mmorpg_trade = @('trade_commerce', 'trade_asset_op', 'schema_migrations')
        zone_1_db = @('player_store', 'user_accounts', 'schema_migrations')
    }
    foreach ($schema in $business.Keys) { foreach ($table in $business[$schema]) { $rows.Add("$schema`t$table") } }
    return ($rows -join "`n")
}

function Get-FixtureSqlReply {
    param([string]$Sql)
    $script:SqlScripts.Add($Sql)
    $out = ''
    if ($Sql -match 'SELECT VERSION') { $out = "$($script:ServerVersion)`tfixture server`n" }
    elseif ($Sql -match "table_schema='mmorpg_global'") { $out = (@($script:GlobalTables) -join "`n") }
    elseif ($Sql -match 'route_meta` WHERE id=1') { $out = "$($script:Generation)`t1`t7`t0`n" }
    elseif ($Sql -match 'SELECT table_schema,table_name') { $out = Get-FixtureTableRows }
    return [pscustomobject]@{ Code = 0; Out = $out; Err = '' }
}

function Invoke-FixturePlan {
    param([string[]]$Arguments)
    $services = ($Arguments[[array]::IndexOf($Arguments, '-Services') + 1]) -split ','
    $output = $Arguments[[array]::IndexOf($Arguments, '-InstancePlanOutput') + 1]
    $repo = $script:CurrentRepo
    $instances = @(foreach ($service in $services) {
            $config = Get-LocalSqlBootSourceConfigPath -RepoRoot $repo -Service $service
            $running = $service -cin $script:RunningKeys
            [pscustomobject]@{ InstanceKey = $service; Service = $service; Running = $running; Port = 1; ConfigPath = $(if ($running) { '' } else { $config }) }
        })
    [IO.File]::WriteAllText($output, (ConvertTo-Json -InputObject ([object[]]$instances) -Depth 4))
    return [pscustomobject]@{ Code = 0; Out = ''; Err = '' }
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
    $configBytes = [IO.File]::ReadAllBytes((Join-Path $map['-input-root'] $entry.runtimeConfigPath))
    [IO.File]::WriteAllBytes($map['-runtime-config-output'], $configBytes)
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
    if ($leaf -ceq 'icacls.exe') { return & $script:RealProcess -FilePath $FilePath -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds }
    $environmentCopy = @{}
    foreach ($key in @($Environment.Keys)) { $environmentCopy[$key] = $Environment[$key] }
    $script:Calls.Add([pscustomobject]@{ Leaf = $leaf; FilePath = $FilePath; Arguments = $Arguments; Environment = $environmentCopy; WorkingDirectory = $WorkingDirectory })
    switch -CaseSensitive ($leaf) {
        'docker.exe' {
            if ($Arguments -ccontains 'port') { return [pscustomobject]@{ Code = 0; Out = "$($script:DockerPort)`n"; Err = '' } }
            return Get-FixtureSqlReply -Sql $StandardInput
        }
        'mysql.exe' { return Get-FixtureSqlReply -Sql $StandardInput }
        'pwsh.exe' { return Invoke-FixturePlan -Arguments $Arguments }
        'writer_authority.exe' { return Invoke-FixtureRegister -Arguments $Arguments }
        'db_migrate.exe' { return [pscustomobject]@{ Code = $script:DbMigrateCode; Out = ''; Err = '' } }
    }
    $service = [IO.Path]::GetFileNameWithoutExtension($leaf)
    # 迁移成功才有权威表:桩在这里补上,和真实 -migrate 的效果一致。
    if ($service -ceq 'data_service') { $script:GlobalTables = @($authorityTables) + @('account_role', 'account_role_admission', 'player_route', 'zone_merged_into', 'player_name') }
    $code = if ($script:MigrateCodes.ContainsKey($service)) { $script:MigrateCodes[$service] } else { 0 }
    return [pscustomobject]@{ Code = $code; Out = ''; Err = "fixture failure user:secret-in-dsn@tcp(127.0.0.1:3306)/x" }
}
function Find-LocalSqlBootDocker { return 'C:\fixture\docker.exe' }
function Test-LocalSqlBootGatewayRunning { param([string]$RepoRoot) return $script:GatewayRunning }

function New-FixtureRepo {
    $repo = Join-Path $fixtureRoot ('repo-' + [guid]::NewGuid().ToString('N'))
    foreach ($service in @(Get-LocalSqlBootWriterServices)) {
        if ($service -ceq 'gateway') { continue }
        $file = if ($service -ceq 'match') { 'match_service.yaml' } else { "$service.yaml" }
        $target = Join-Path $repo "go/$service/etc/$file"
        $null = New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force
        Copy-Item -LiteralPath (Join-Path $realRepo "go/$service/etc/$file") -Destination $target
        $null = New-Item -ItemType Directory -Path (Join-Path $repo 'bin/go_services') -Force
        Set-Content -LiteralPath (Join-Path $repo "bin/go_services/$service.exe") -Value 'fixture'
    }
    foreach ($tool in @('writer_authority', 'db_migrate', 'private_control_file')) { Set-Content -LiteralPath (Join-Path $repo "bin/go_services/$tool.exe") -Value "fixture-$tool" }
    $null = New-Item -ItemType Directory -Path (Join-Path $repo 'deploy/sql'), (Join-Path $repo 'deploy/mysql-init'), (Join-Path $repo 'java/gateway_node/src/main/resources') -Force
    foreach ($file in @('authority_runtime_role.sql', 'authority_operator_role.sql')) { Copy-Item -LiteralPath (Join-Path $realRepo "deploy/sql/$file") -Destination (Join-Path $repo "deploy/sql/$file") }
    Copy-Item -LiteralPath (Join-Path $realRepo 'deploy/mysql-init/gateway_tables.sql') -Destination (Join-Path $repo 'deploy/mysql-init/gateway_tables.sql')
    Copy-Item -LiteralPath (Join-Path $realRepo 'java/gateway_node/src/main/resources/application.yaml') -Destination (Join-Path $repo 'java/gateway_node/src/main/resources/application.yaml')
    $script:CurrentRepo = $repo
    return $repo
}
function Get-ThrownMessage([scriptblock]$Action) { try { & $Action | Out-Null } catch { return $_.Exception.Message }; return '' }
function Get-RegisterCalls { return @($script:Calls | Where-Object { $_.Leaf -ceq 'writer_authority.exe' }) }
function Get-SqlText { return ($script:SqlScripts -join "`n") }
function Get-AccountBlock {
    param([string]$User)
    # 账号自己的 CREATE USER 到下一个 CREATE USER 之间就是它的角色文本。
    $text = Get-SqlText
    $start = $text.IndexOf("CREATE USER IF NOT EXISTS '$User'@'%'")
    if ($start -lt 0) { throw "no account block for $User" }
    $next = $text.IndexOf('CREATE USER IF NOT EXISTS', $start + 10)
    if ($next -lt 0) { return $text.Substring($start) }
    return $text.Substring($start, $next - $start)
}
function Get-ConfigValue { param($Summary, [string]$Key, [string]$KeyPath)
    $catalog = Read-MaintenanceJson -Path $Summary.catalog
    return Get-YamlScalar -Path $catalog[$Key].runtimeConfigFile -KeyPath $KeyPath
}
function Get-BootUser { param($Summary, [string]$Key)
    $catalog = Read-MaintenanceJson -Path $Summary.catalog
    $grant = Read-MaintenanceJson -Path $catalog[$Key].grantFile
    return ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($grant.sqlPrincipal))).Split('@')[0]
}

$savedEnvironment = @{}
$touchedVariables = @('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST', 'MMORPG_LOCAL_SQL_ROOT_USER', 'MMORPG_LOCAL_SQL_ROOT_PASSWORD', 'MMORPG_SQL_SCHEMA_PRINCIPAL', 'MMORPG_SQL_REGISTRAR_DSN', 'MMORPG_SQL_OPERATOR_PRINCIPAL', 'DB_ALLOWED_DATABASES', 'MMORPG_DB_ALLOWED_DATABASES', 'MMORPG_SQL_BOOT_CATALOG', 'MMORPG_PUBLICATION_FENCE_DIR', 'LOCALAPPDATA')
foreach ($name in $touchedVariables) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    [Environment]::SetEnvironmentVariable($name, $null, 'Process')
}
try {
    $null = New-Item -ItemType Directory -Path $fixtureRoot -Force
    $env:LOCALAPPDATA = Join-Path $fixtureRoot 'localappdata'

    foreach ($endpoint in @('127.0.0.1:3306', 'localhost:3306', '[::1]:3306', '127.0.0.1:4000')) {
        Test-Case "回环端点 $endpoint 可用" {
            Assert-Match (Assert-LocalSqlBootEndpoint -Endpoint $endpoint).Name '^(127\.0\.0\.1|localhost|::1)$' '解析出回环主机'
        }
    }
    foreach ($endpoint in @('', '10.0.0.5:3306', '192.168.1.9:3306', 'mysql.example.com:3306', 'localhost.example.com:3306', '127.0.0.1.nip.io:3306', '127.0.0.2:3306', '0.0.0.0:3306', '[::2]:3306', '::1:3306', 'root:pw@127.0.0.1:3306', '127.0.0.1', '127.0.0.1:0', '127.0.0.1:70000', "127.0.0.1:3306`n")) {
        Test-Case "拒绝非回环或畸形端点 '$endpoint'" {
            Assert-Match (Get-ThrownMessage { Assert-LocalSqlBootEndpoint -Endpoint $endpoint }) 'loopback' '其他地址一律拒绝'
        }
    }
    Test-Case '远端地址在任何子进程和落盘之前拒绝,口令不进错误文本' {
        Reset-Fixture
        $repo = New-FixtureRepo
        Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo -Endpoint 'root:hunter2@tidb.example.com:4000' }) 'loopback' '参数给远端'
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = 'tidb.example.com:4000'
        try { Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo }) 'loopback' '环境变量给远端' } finally { $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = $null }
        Assert-NotMatch (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo -Endpoint 'root:hunter2@tidb.example.com:4000' }) 'hunter2' '错误不回显原值'
        Assert-Equal 0 $script:Calls.Count '没有任何子进程'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path $repo 'run/secrets')) '没有落盘'
    }

    Test-Case '全部写者实例的产物都过得了正式 Get-SqlWriterBootEnvironment' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        $catalog = Read-MaintenanceJson -Path $summary.catalog
        Assert-Equal ((@(Get-LocalSqlBootWriterServices) | Sort-Object) -join ',') ((@($catalog.Keys) | Sort-Object) -join ',') '每个写者一条'
        $env:MMORPG_SQL_BOOT_CATALOG = $summary.catalog
        try {
            foreach ($key in @($catalog.Keys)) {
                $selected = ''
                $result = Get-SqlWriterBootEnvironment -InstanceKey $key -RepoRoot $repo -ServiceName $key -SelectedRuntimeConfigFile ([ref]$selected)
                foreach ($name in @('MMORPG_SQL_WRITER_GRANT_FILE', 'MMORPG_SQL_WRITER_GRANT_SHA256', 'MMORPG_SQL_WRITER_CONSUME_DIR', 'MMORPG_SQL_DEPLOYMENT_ID', 'MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256')) {
                    Assert-True (-not [string]::IsNullOrEmpty([string]$result[$name])) "$key 缺 $name"
                }
                Assert-Equal $catalog[$key].runtimeConfigFile $selected '选中的是本实例私有配置'
                Assert-True (Test-Path -LiteralPath $catalog[$key].consumeDir -PathType Container) '消费目录已预建'
            }
            $user = Get-BootUser -Summary $summary -Key 'chat'
            $dsn = (Get-SqlWriterBootEnvironment -InstanceKey chat -RepoRoot $repo -ServiceName chat).CHAT_AUTHORITY_DSN
            Assert-Match $dsn "^${user}:[0-9a-f]{64}@tcp\(127\.0\.0\.1:3306\)/mmorpg_chat\?" 'chat DSN 用本实例账号和回环端点'
            $matchUser = Get-BootUser -Summary $summary -Key 'match'
            Assert-Equal $matchUser (Get-ConfigValue -Summary $summary -Key 'match' -KeyPath 'GlobalMatch.User').Value 'match 配置写本实例账号'
            Assert-Match (Get-SqlWriterBootEnvironment -InstanceKey match -RepoRoot $repo -ServiceName match).MATCH_MYSQL_PASSWORD '^[0-9a-f]{64}$' 'match 口令走白名单环境文件'
            Assert-Match (Get-SqlWriterBootEnvironment -InstanceKey guild -RepoRoot $repo -ServiceName guild).MMORPG_GUILD_SQL_DSN '/mmorpg_guild\?' 'guild 连接串库名固定'
            Assert-Match (Get-SqlWriterBootEnvironment -InstanceKey mail -RepoRoot $repo -ServiceName mail).MMORPG_MAIL_SQL_DSN '/mmorpg_mail\?' 'mail 连接串'
            Assert-Match (Get-SqlWriterBootEnvironment -InstanceKey rank -RepoRoot $repo -ServiceName rank).MMORPG_RANK_SQL_DSN '/mmorpg_rank\?' 'rank 连接串'
        } finally { $env:MMORPG_SQL_BOOT_CATALOG = $null }
    }

    Test-Case '每 boot 配置连本机档,不留共享账号,其余配置不变' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $sourceHash = (Get-FileHash -LiteralPath (Join-Path $repo 'go/data_service/etc/data_service.yaml') -Algorithm SHA256).Hash
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        foreach ($check in @(
                @{ Key = 'data_service'; Path = 'SnapshotMySQL.Backend'; Value = 'mysql' }
                @{ Key = 'data_service'; Path = 'SnapshotMySQL.Host'; Value = '127.0.0.1:3306' }
                @{ Key = 'data_service'; Path = 'Schema.AutoMigrate'; Value = 'false' }
                @{ Key = 'db'; Path = 'ServerConfig.Database.Backend'; Value = 'mysql' }
                @{ Key = 'db'; Path = 'ServerConfig.Database.Hosts'; Value = '127.0.0.1:3306' }
                @{ Key = 'chat'; Path = 'Backend'; Value = 'mysql' }
                @{ Key = 'friend'; Path = 'Backend'; Value = 'mysql' }
                @{ Key = 'friend'; Path = 'AccountRoleSchema'; Value = 'mmorpg_global' }
                @{ Key = 'friend'; Path = 'GlobalLoginSessions'; Value = 'true' }
                @{ Key = 'friend'; Path = 'LoginRedis.Addrs'; Value = '["127.0.0.1:6379"]' }
                @{ Key = 'friend'; Path = 'MySQL.Host'; Value = '127.0.0.1:3306' }
                @{ Key = 'guild'; Path = 'MySQL.Backend'; Value = 'mysql' }
                @{ Key = 'guild'; Path = 'MySQL.DataSourceEnv'; Value = 'MMORPG_GUILD_SQL_DSN' }
                @{ Key = 'mail'; Path = 'Etcd.Key'; Value = '' }
                @{ Key = 'mail'; Path = 'Backend'; Value = 'mysql' }
                @{ Key = 'mail'; Path = 'AccountRoleSchema'; Value = 'mmorpg_global' }
                @{ Key = 'mail'; Path = 'GlobalLoginSessions'; Value = 'true' }
                @{ Key = 'rank'; Path = 'Etcd.Key'; Value = '' }
                @{ Key = 'rank'; Path = 'Backend'; Value = 'mysql' }
                @{ Key = 'match'; Path = 'GlobalMatch.Backend'; Value = 'mysql' }
                @{ Key = 'match'; Path = 'GlobalMatch.Host'; Value = '127.0.0.1:3306' }
                @{ Key = 'trade'; Path = 'MySQL.Backend'; Value = 'mysql' }
                @{ Key = 'trade'; Path = 'MySQL.Host'; Value = '127.0.0.1:3306' }
                @{ Key = 'player_inbox'; Path = 'MySQL.Backend'; Value = 'mysql' }
                @{ Key = 'player_inbox'; Path = 'MySQL.Host'; Value = '127.0.0.1:3306' }
                @{ Key = 'player_inbox'; Path = 'AccountRoleSchema'; Value = 'mmorpg_global' }
            )) {
            $actual = Get-ConfigValue -Summary $summary -Key $check.Key -KeyPath $check.Path
            Assert-True $actual.Found "$($check.Key) 缺 $($check.Path)"
            Assert-Equal $check.Value $actual.Value "$($check.Key) 的 $($check.Path)"
        }
        Assert-Equal $false (Get-ConfigValue -Summary $summary -Key 'guild' -KeyPath 'MySQL.DataSource').Found 'guild 配置不留连接串原文'
        foreach ($key in @('data_service', 'db', 'friend', 'trade', 'player_inbox')) {
            $catalog = Read-MaintenanceJson -Path $summary.catalog
            $text = [IO.File]::ReadAllText($catalog[$key].runtimeConfigFile)
            Assert-NotMatch $text 'appuser|apppass123' "$key 不留共享账号"
            Assert-Match $text ([regex]::Escape((Get-BootUser -Summary $summary -Key $key))) "$key 用自己的账号"
        }
        $source = Get-YamlScalar -Path (Join-Path $repo 'go/friend/etc/friend.yaml') -KeyPath 'ListenOn'
        Assert-Equal $source.Value (Get-ConfigValue -Summary $summary -Key 'friend' -KeyPath 'ListenOn').Value '与 Start-ServiceInstance 核对的 ListenOn 原样保留'
        Assert-Equal $sourceHash (Get-FileHash -LiteralPath (Join-Path $repo 'go/data_service/etc/data_service.yaml') -Algorithm SHA256).Hash '不改源配置'
        $gateway = Read-MaintenanceJson -Path $summary.catalog
        $gatewayText = [IO.File]::ReadAllText($gateway['gateway'].runtimeConfigFile)
        Assert-Match $gatewayText 'jdbc:mysql://127\.0\.0\.1:3306/mmorpg\?' 'gateway 走本次端点,查询串取包内原件'
        Assert-Match $gatewayText 'require-tidb: false' 'mysql 档关 TiDB 强核验'
        Assert-Match $gatewayText 'run-mode: dev' '只有 dev/test 能关,见 AuthorityDatabaseConfiguration'
    }

    Test-Case 'writer_authority 走正式 roster 注册,控制凭据只在子进程环境' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        $registers = @(Get-RegisterCalls)
        Assert-Equal @(Get-LocalSqlBootWriterServices).Count $registers.Count '每实例注册一次'
        $identities = @{}
        foreach ($call in $registers) {
            foreach ($flag in @('-roster', '-roster-sha256', '-input-root', '-lock-qualification', '-runtime-config-output', '-boot-output', '-deployment-id')) {
                Assert-True ($call.Arguments -ccontains $flag) "register 缺 $flag"
            }
            Assert-Equal 'register' $call.Arguments[1] '只注册'
            $registrarPassword = [regex]::Match($call.Environment.MMORPG_SQL_REGISTRAR_DSN, ':(?<password>[0-9a-f]{64})@').Groups['password'].Value
            Assert-NotMatch ($call.Arguments -join ' ') "@tcp\(|$registrarPassword" '命令行不带 DSN 或口令'
            Assert-Match $call.Environment.MMORPG_SQL_REGISTRAR_DSN '^mmorpg_local_registrar:[0-9a-f]{64}@tcp\(127\.0\.0\.1:3306\)/mmorpg_global\?' 'DSN 库名等 authority schema'
            Assert-Equal 'mmorpg_local_registrar@%' $call.Environment.MMORPG_SQL_OPERATOR_PRINCIPAL '操作主体等 CURRENT_USER'
            Assert-Equal 'mysql' $call.Environment.MMORPG_SQL_BACKEND '工具的档位走环境变量'
            Assert-Match $call.Environment.MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256 '^[0-9a-f]{64}$' '物理 authority 摘要'
        }
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_SQL_REGISTRAR_DSN)) '父环境没有控制 DSN'
        Assert-True ([string]::IsNullOrEmpty($env:MMORPG_SQL_OPERATOR_PRINCIPAL)) '父环境没有控制主体'
        $catalog = Read-MaintenanceJson -Path $summary.catalog
        foreach ($key in @($catalog.Keys)) {
            $grant = Read-MaintenanceJson -Path $catalog[$key].grantFile
            foreach ($value in @($grant.grantId, $grant.writerKey, $grant.instanceId, $grant.sqlPrincipal)) {
                Assert-True (-not $identities.ContainsKey($value)) '身份和主体逐实例不同'
                $identities[$value] = $true
            }
            Assert-Equal $script:Generation $grant.generation 'generation 取库里 route_meta'
            Assert-Equal 'LOCK_TOUCH' $grant.lockPrimitive 'mysql 档 LOCK_TOUCH'
            Assert-Equal 'mmorpg_global' $grant.authoritySchema '权威库取 data_service 配置'
            $schemas = @($grant.protectedSchemas)
            Assert-True ($schemas -ccontains 'mmorpg') 'gateway 的库在保护集合'
            Assert-True ($schemas -ccontains 'zone_1_db') 'go/db 的库在保护集合'
            $sorted = [string[]]@($schemas)
            [Array]::Sort($sorted, [StringComparer]::Ordinal)
            Assert-Equal ($sorted -join ',') ($schemas -join ',') 'protected schemas 必须按字节序(validSchemas)'
        }
    }

    Test-Case '账号按模板建:runtime 无 DDL,逐表授权,权限真值表只读' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        foreach ($key in @((Read-MaintenanceJson -Path $summary.catalog).Keys)) {
            $block = Get-AccountBlock -User (Get-BootUser -Summary $summary -Key $key)
            $statements = ($block -split "`r?`n" | Where-Object { $_ -notmatch '^\s*--' }) -join "`n"
            Assert-NotMatch $statements '(?i)ALL PRIVILEGES|GRANT OPTION|\bCREATE,|\bALTER\b|ON\s+\S+\.\*' "$key 没有 DDL / 全库授权"
            Assert-Match $statements 'GRANT UPDATE \(lock_touch\) ON `mmorpg_global`\.`writer_slot`' "$key 有列级锁写"
            Assert-Match $statements 'GRANT SELECT ON `mmorpg_global`\.`writer_lifecycle_receipt`' "$key 能读原 REGISTER"
            foreach ($table in $authorityTables) { Assert-NotMatch $statements "GRANT (INSERT|DELETE|SELECT,INSERT)[^;]*``$table``" "$key 不能写权限真值表 $table" }
        }
        $friend = Get-AccountBlock -User (Get-BootUser -Summary $summary -Key 'friend')
        Assert-Match $friend 'GRANT SELECT,INSERT,UPDATE,DELETE ON `mmorpg_friend`\.`friend`' '本库 DML'
        Assert-Match $friend 'GRANT SELECT ON `mmorpg_friend`\.`schema_migrations`' '台账只读:Plan 要看得见,不该写'
        Assert-Match $friend 'GRANT SELECT,UPDATE ON `mmorpg_global`\.`account_role`' 'FOR UPDATE 在 MySQL 上要 UPDATE'
        Assert-NotMatch $friend 'mmorpg_guild|mmorpg_match' '不碰别的域'
        $db = Get-AccountBlock -User (Get-BootUser -Summary $summary -Key 'db')
        Assert-Match $db 'GRANT SELECT ON `mmorpg_global`\.`player_route`' 'go/db 只读路由表'
        Assert-Match $db 'GRANT SELECT,INSERT,UPDATE,DELETE ON `zone_1_db`\.`player_store`' 'go/db 写玩家库'
        $dataService = Get-AccountBlock -User (Get-BootUser -Summary $summary -Key 'data_service')
        Assert-Match $dataService 'GRANT SELECT,UPDATE ON `mmorpg_guild`\.`guild_member`' '角色交易证明的跨域锁读'
        Assert-Match $dataService 'GRANT SELECT,INSERT,UPDATE,DELETE ON `mmorpg_global`\.`player_name`' '全局库 DML'
        Assert-NotMatch $dataService 'GRANT SELECT,INSERT,UPDATE,DELETE ON `mmorpg_global`\.`route_meta`' 'route_meta 不进业务表'
        $login = Get-AccountBlock -User (Get-BootUser -Summary $summary -Key 'login')
        Assert-NotMatch $login 'mmorpg_friend|zone_1_db' 'login 没有业务表'
        $text = Get-SqlText
        Assert-Match $text "GRANT SELECT,INSERT ON ``mmorpg_global``\.``writer_registry`` TO 'mmorpg_local_registrar'@'%'" '注册账号按 operator 模板'
        Assert-Match $text "GRANT SELECT,INSERT,UPDATE ON ``mmorpg_global``\.``sql_writer_drain_run`` TO 'mmorpg_local_maintenance'@'%'" '维护账号按 operator 模板'
        Assert-NotMatch $text "TO 'root'@'%'" '本机 schema 身份就是 root,不再给 root 授权'
        Assert-NotMatch $text 'appuser' '不碰旧共享账号'
    }

    Test-Case '只授已存在的表:GRANT 表级不存在的表 MySQL 直接报错' {
        $tables = @(Get-LocalSqlBootInstanceTables -Grant @{ Own = @('a'); Cross = @(@{ Schema = 'b'; Tables = @('present', 'absent'); Operations = @('SELECT') }, @{ Schema = 'empty'; Operations = @('SELECT', 'UPDATE') }) } -TablesBySchema @{ a = [Collections.Generic.List[string]]@('t1', 'route_meta'); b = [Collections.Generic.List[string]]@('present', 'other') })
        $names = @($tables | ForEach-Object { "$($_.schema).$($_.table)" })
        Assert-Equal 'a.t1,b.present' ($names -join ',') '过滤不存在的表和权限真值表'
    }

    Test-Case '口令和授权只在被忽略的私有目录,不进被跟踪路径和控制台' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $console = & { New-LocalSqlBootCatalog -RepoRoot $repo } 6>&1 3>&1 | Out-String
        $passwords = @([regex]::Matches((Get-SqlText), "IDENTIFIED BY '([0-9a-f]{64})'") | ForEach-Object { $_.Groups[1].Value } | Select-Object -Unique)
        Assert-True ($passwords.Count -ge 14) '注册、维护加每个实例各一个随机口令'
        $private = Get-LocalSqlBootRoot -RepoRoot $repo
        foreach ($password in $passwords) { Assert-True (-not $console.Contains($password)) '控制台不出现口令' }
        foreach ($file in @(Get-ChildItem -LiteralPath $repo -Recurse -File | Where-Object { -not $_.FullName.StartsWith($private, [StringComparison]::OrdinalIgnoreCase) })) {
            $text = [IO.File]::ReadAllText($file.FullName)
            foreach ($password in $passwords) { Assert-True (-not $text.Contains($password)) "口令不能出现在 $($file.Name)" }
        }
        foreach ($file in @(Get-ChildItem -LiteralPath $private -Recurse -File)) {
            Assert-True (-not ([IO.File]::ReadAllText($file.FullName)).Contains($rootPassword)) "本机 root 口令不留在 $($file.Name)"
        }
        & git -C $realRepo check-ignore -q 'run/secrets/sql-boot-local/catalog.json'
        Assert-Equal 0 $LASTEXITCODE '产物路径被 .gitignore 覆盖'
        $acl = Get-Acl -LiteralPath $private
        Assert-True $acl.AreAccessRulesProtected '私有目录断开继承'
        $trusted = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18', 'S-1-5-32-544')
        foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) { Assert-True ($rule.IdentityReference.Value -cin $trusted) '只留当前账号、SYSTEM、Administrators' }
        $catalog = Read-MaintenanceJson -Path (Join-Path $private 'catalog.json')
        $grantAcl = Get-Acl -LiteralPath $catalog['db'].grantFile
        foreach ($rule in $grantAcl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) { Assert-True ($rule.IdentityReference.Value -cin $trusted) '文件继承私有 ACL' }
        foreach ($leftover in @(Get-ChildItem -LiteralPath (Join-Path $private 'boots') -Recurse -Directory | Where-Object { $_.Name -cin @('schema', 'input') })) { throw "遗留 $($leftover.FullName):明文输入和 root 配置用完即删" }
    }

    Test-Case '空库先跑 go/db 与 data_service 迁移,再其余服务,全部建完才授权和注册' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:GlobalTables = @()
        $null = New-LocalSqlBootCatalog -RepoRoot $repo
        $order = @($script:Calls | ForEach-Object { $_.Leaf })
        Assert-Equal 0 (@($order | Where-Object { $_ -notin @('docker.exe') }).IndexOf('db_migrate.exe')) 'go/db 最先'
        $dataService = [array]::IndexOf($order, 'data_service.exe')
        Assert-True ($dataService -gt [array]::IndexOf($order, 'db_migrate.exe')) 'data_service 随后'
        foreach ($service in @('chat', 'friend', 'guild', 'mail', 'rank', 'match', 'trade', 'player_inbox')) {
            Assert-True ([array]::IndexOf($order, "$service.exe") -gt $dataService) "$service 在 data_service 之后"
            Assert-True ([array]::IndexOf($order, "$service.exe") -lt [array]::IndexOf($order, 'writer_authority.exe')) "$service 先于注册"
        }
        $migrate = @($script:Calls | Where-Object { $_.Leaf -ceq 'data_service.exe' })
        Assert-Equal 1 $migrate.Count '只迁一次'
        Assert-Equal '-migrate' $migrate[0].Arguments[2] '走正式 -migrate'
        Assert-Equal 'root@%' $migrate[0].Environment.MMORPG_SQL_SCHEMA_PRINCIPAL 'schema 主体只给迁移子进程'
        Assert-Match (Get-SqlText) 'CREATE DATABASE IF NOT EXISTS `mmorpg_match` DEFAULT CHARACTER SET utf8mb4' '已有卷也补库'
        Assert-Match (Get-SqlText) 'CREATE DATABASE IF NOT EXISTS `mmorpg_trade`' '新卷和旧卷走同一条路'
        Assert-Match (Get-SqlText) 'CREATE TABLE IF NOT EXISTS zone_config' 'gateway 的表补齐'
        $match = @($script:Calls | Where-Object { $_.Leaf -ceq 'match.exe' })[0]
        Assert-Equal 'root' $match.Environment.MATCH_MIGRATION_USER 'match 迁移账号走专用变量'
        Assert-True (-not $match.Environment.ContainsKey('MATCH_MYSQL_PASSWORD')) '不用常驻变量做 DDL'
        $guild = @($script:Calls | Where-Object { $_.Leaf -ceq 'guild.exe' })[0]
        Assert-Match $guild.Environment.MMORPG_GUILD_SQL_DSN '^root:.*/mmorpg_guild\?' 'guild 迁移 DSN'
        $dbMigrate = @($script:Calls | Where-Object { $_.Leaf -ceq 'db_migrate.exe' })[0]
        Assert-Match $dbMigrate.Environment.DB_ALLOWED_DATABASES '^zone_1_db$' 'go/db 迁移白名单'
    }
    Test-Case '权威表齐全时跳过 data_service -migrate(它拒绝已有表),go/db 退出码 4 不拦' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:DbMigrateCode = 4
        $null = New-LocalSqlBootCatalog -RepoRoot $repo
        Assert-Equal 0 @($script:Calls | Where-Object { $_.Leaf -ceq 'data_service.exe' }).Count '不重跑'
        Assert-True (@(Get-RegisterCalls).Count -gt 0) '继续注册'
    }
    Test-Case '旧布局或半成品的全局库拒绝继续,不替人删数据,也不留可用目录' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:GlobalTables = @('player_name', 'transaction_log')
        Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo }) 'never drops data' '说清楚怎么办'
        Assert-Equal 0 @(Get-RegisterCalls).Count '没有注册'
        Assert-NotMatch (Get-SqlText) '(?i)DROP\s+(DATABASE|TABLE)' '绝不删库删表'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path (Get-LocalSqlBootRoot -RepoRoot $repo) 'catalog.json')) '不留目录'
    }

    Test-Case '单次授权:重新生成全新身份,删掉已停实例的账号,运行中的不动,旧目录作废' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $first = New-LocalSqlBootCatalog -RepoRoot $repo
        $firstCatalog = Read-MaintenanceJson -Path $first.catalog
        $firstDb = Get-BootUser -Summary $first -Key 'db'
        $firstChat = Get-BootUser -Summary $first -Key 'chat'
        $firstGrant = (Read-MaintenanceJson -Path $firstCatalog['chat'].grantFile).grantId
        $firstBoot = $first.bootId
        $script:SqlScripts.Clear()
        $script:RunningKeys = @('db')
        $second = New-LocalSqlBootCatalog -RepoRoot $repo
        $secondCatalog = Read-MaintenanceJson -Path $second.catalog
        Assert-True (-not $secondCatalog.Contains('db')) '运行中的实例 start 会 skip,不发新授权'
        Assert-Match ($second.running -join ',') '^db$' '摘要记下运行中的实例'
        Assert-True ((Get-BootUser -Summary $second -Key 'chat') -cne $firstChat) 'sql_principal 永久唯一,每次换新账号'
        Assert-True ((Read-MaintenanceJson -Path $secondCatalog['chat'].grantFile).grantId -cne $firstGrant) 'grant 全新'
        $text = Get-SqlText
        Assert-Match $text "DROP USER IF EXISTS '$firstChat'@'%'" '已停实例的账号删掉'
        Assert-NotMatch $text "DROP USER IF EXISTS '$firstDb'@'%'" '运行中实例的账号留着,它的连接池还要重连'
        Assert-Equal $false (Test-Path -LiteralPath $firstCatalog['chat'].grantFile) '旧授权文件清掉'
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path (Get-LocalSqlBootRoot -RepoRoot $repo) "boots/$firstBoot")) '只留本轮目录'
        $accounts = @(Get-Content -LiteralPath (Join-Path (Get-LocalSqlBootRoot -RepoRoot $repo) 'runtime-accounts.json') -Raw | ConvertFrom-Json)
        Assert-True (@($accounts | Where-Object { $_.principal -ceq "$firstDb@%" }).Count -eq 1) '账本继续记着运行中实例的账号'
        Assert-True (@($accounts | Where-Object { $_.principal -ceq "$firstChat@%" }).Count -eq 0) '删掉的账号出账本'
    }
    Test-Case '全部在运行时写空目录,不建账号' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:RunningKeys = @(Get-LocalSqlBootWriterServices)
        $script:GatewayRunning = $true
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        Assert-Equal 0 @((Read-MaintenanceJson -Path $summary.catalog).Keys).Count '空目录'
        Assert-Equal 0 @(Get-RegisterCalls).Count '没有注册'
        Assert-NotMatch (Get-SqlText) 'CREATE USER' '没有建账号'
    }

    Test-Case '可选服务建表失败只跳过它,不拖垮其余;错误文本抹掉 DSN 口令' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:MigrateCodes = @{ trade = 1 }
        $warnings = & { $null = New-LocalSqlBootCatalog -RepoRoot $repo } 3>&1 | Out-String
        $summary = Get-Content -LiteralPath (Join-Path (Get-LocalSqlBootRoot -RepoRoot $repo) 'last-run.json') -Raw | ConvertFrom-Json
        Assert-Equal 'trade' (@($summary.skipped) -join ',') 'start_game 据此跳过'
        $catalog = Read-MaintenanceJson -Path $summary.catalog
        Assert-True (-not $catalog.Contains('trade')) '目录里没有它'
        Assert-True $catalog.Contains('db') '其余照常'
        Assert-NotMatch $warnings 'secret-in-dsn' '错误文本不带 DSN 口令'
    }
    Test-Case '必需服务建表失败中止;没请求启动的服务失败只告警' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:MigrateCodes = @{ guild = 1 }
        Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo }) 'guild -migrate failed' 'guild 必需'
        Assert-Equal 0 @(Get-RegisterCalls).Count '中止在注册之前'
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:MigrateCodes = @{ mail = 1 }
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo -Services 'db,data_service,client_rpc_router'
        Assert-Equal 'data_service,db' ((@((Read-MaintenanceJson -Path $summary.catalog).Keys) | Sort-Object) -join ',') '只发请求的写者,不碰 SQL 的 Go 服务名被忽略'
        Assert-Equal 0 @($summary.skipped).Count 'mail 没请求启动,不算跳过'
        Assert-True (@($script:Calls | Where-Object { $_.Leaf -ceq 'mail.exe' }).Count -eq 1) '表仍尽量建好,data_service 的跨域探针才看得到'
        Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo -Services 'dbb' }) 'Unknown service' '写错的名字不静默吞掉'
    }

    Test-Case 'TiDB 档:SELECT_ONLY、不带 -LocalMySQL、gateway 保留强核验' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $script:ServerVersion = '8.0.11-TiDB-v8.5.2'
        $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = '127.0.0.1:4000'
        try {
            # 只在本用例的作用域里替换:端点不是容器发布的端口,主机上有没有 mysql.exe 不该影响结果。
            function Get-Command { param($Name, $CommandType, $ErrorAction) return [pscustomobject]@{ Source = 'C:\fixture\mysql.exe' } }
            $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        } finally { $env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST = $null }
        $catalog = Read-MaintenanceJson -Path $summary.catalog
        Assert-Equal 'SELECT_ONLY' (Read-MaintenanceJson -Path $catalog['db'].grantFile).lockPrimitive 'tidb 档 SELECT_ONLY'
        Assert-NotMatch (Get-SqlText) 'lock_touch' '没有列级锁写'
        $plan = @($script:Calls | Where-Object { $_.Leaf -ceq 'pwsh.exe' })[0]
        Assert-True ($plan.Arguments -cnotcontains '-LocalMySQL') '不走本机 MySQL 档'
        Assert-Equal '127.0.0.1:4000' (Get-ConfigValue -Summary $summary -Key 'player_inbox' -KeyPath 'MySQL.Host').Value '端点来自环境变量'
        Assert-Equal 'tidb' (Get-ConfigValue -Summary $summary -Key 'trade' -KeyPath 'MySQL.Backend').Value '配置档位跟服务器走'
        Assert-Match ([IO.File]::ReadAllText($catalog['gateway'].runtimeConfigFile)) 'require-tidb: true' 'gateway 强核验不关'
        Assert-True (@($script:Calls | Where-Object { $_.Leaf -ceq 'mysql.exe' }).Count -gt 0) '端点不是容器发布端口时走主机客户端'
        foreach ($call in @($script:Calls | Where-Object { $_.Leaf -ceq 'mysql.exe' })) { Assert-NotMatch ($call.Arguments -join ' ') '(?i)password' 'root 口令不上命令行' }
        Assert-Equal $false (Test-Path -LiteralPath (Join-Path (Get-LocalSqlBootRoot -RepoRoot $repo) 'mysql-client.cnf')) '选项文件用完即删'
    }
    Test-Case '要求的档位和服务器不符时拒绝' {
        Reset-Fixture
        $repo = New-FixtureRepo
        Assert-Match (Get-ThrownMessage { New-LocalSqlBootCatalog -RepoRoot $repo -Backend tidb }) 'is mysql, not tidb' '不猜'
    }

    Test-Case '生产闸门没放宽:缺目录、改动 grant 都还是拒绝' {
        Reset-Fixture
        $repo = New-FixtureRepo
        $summary = New-LocalSqlBootCatalog -RepoRoot $repo
        Assert-Match (Get-ThrownMessage { Get-SqlWriterBootEnvironment -InstanceKey db -RepoRoot $repo -ServiceName db }) 'MMORPG_SQL_BOOT_CATALOG' '没设目录照旧拒绝'
        $env:MMORPG_SQL_BOOT_CATALOG = $summary.catalog
        try {
            $catalog = Read-MaintenanceJson -Path $summary.catalog
            $original = [IO.File]::ReadAllBytes($catalog['db'].grantFile)
            [IO.File]::WriteAllText($catalog['db'].grantFile, '{"format":"SQL_WRITER_BOOT_V1"}')
            Assert-Match (Get-ThrownMessage { Get-SqlWriterBootEnvironment -InstanceKey db -RepoRoot $repo -ServiceName db }) '摘要不符' '改动 grant 拒绝'
            [IO.File]::WriteAllBytes($catalog['db'].grantFile, $original)
            Assert-Match (Get-ThrownMessage { Get-SqlWriterBootEnvironment -InstanceKey not_registered -RepoRoot $repo -ServiceName db }) '单boot授权' '没登记的实例拒绝'
        } finally { $env:MMORPG_SQL_BOOT_CATALOG = $null }
        $libSource = [IO.File]::ReadAllText((Join-Path $PSScriptRoot '../lib/sql_boot_catalog_local.ps1'))
        Assert-NotMatch $libSource 'SetEnvironmentVariable\(''MMORPG_SQL_WRITER' '生成器不往父环境塞 grant'
        $goConfig = [IO.File]::ReadAllText((Join-Path $realRepo 'go/shared/authoritysql/config.go'))
        Assert-NotMatch $goConfig '(?i)dev.?mode|skip.?writer|bypass' 'LoadWriterConfig 没有 dev 旁路'
    }

    Test-Case 'YAML 改写只动目标行,缺段和重复都拒绝' {
        $text = "A: 1`r`nB:`r`n  # comment`r`n  C: old # tail`r`n  D:`r`n    E: x`r`nF: 2`r`n"
        Assert-Equal ($text.Replace('C: old # tail', 'C: new')) (Set-LocalSqlBootYamlScalar -Content $text -KeyPath 'B.C' -Literal 'new') '保留其余字节和 CRLF'
        Assert-Equal ($text.Replace('    E: x', "    G: y`r`n    E: x")) (Set-LocalSqlBootYamlScalar -Content $text -KeyPath 'B.D.G' -Literal 'y') '叶子不存在就插到所在段首'
        Assert-Equal ($text.Replace("  C: old # tail`r`n", '')) (Set-LocalSqlBootYamlScalar -Content $text -KeyPath 'B.C' -Remove) '删除一行'
        Assert-Equal ("Z: 9`r`n" + $text) (Set-LocalSqlBootYamlScalar -Content $text -KeyPath 'Z' -Literal '9') '顶层新键插到文件头'
        Assert-Match (Get-ThrownMessage { Set-LocalSqlBootYamlScalar -Content $text -KeyPath 'Q.R' -Literal 'x' }) 'missing' '缺段不猜'
        Assert-Match (Get-ThrownMessage { Set-LocalSqlBootYamlScalar -Content "A: {x: 1}`n" -KeyPath 'A.B' -Literal 'x' }) 'block mapping' '流式映射不猜'
        Assert-Match (Get-ThrownMessage { Set-LocalSqlBootYamlScalar -Content "A: 1`nA: 2`n" -KeyPath 'A' -Literal 'x' }) 'duplicated' '重复键不猜'
        Assert-Equal '"a\\b\"c"' (ConvertTo-LocalSqlBootYamlString -Value 'a\b"c') '字符串转义'
    }
    Test-Case '错误文本抹掉口令形状的内容' {
        $safe = ConvertTo-LocalSqlBootSafeText -Text ("user:pw@tcp(127.0.0.1:3306)/x IDENTIFIED BY 'short' " + ('a' * 64))
        Assert-NotMatch $safe ":pw@|short|$('a' * 64)" '三种形状都抹掉'
    }

    Test-Case 'dev_tools 正式入口转发到生成器' {
        $driverAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../dev_tools.ps1'), [ref]$null, [ref]$null)
        $commandParameter = @($driverAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Command' })[0]
        $validate = @($commandParameter.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' })[0]
        Assert-True (@($validate.PositionalArguments | ForEach-Object { $_.Value }) -ccontains 'sql-boot-catalog-local') 'ValidateSet 收了命令名'
        $dispatch = @($driverAst.FindAll({ param($node) $node -is [Management.Automation.Language.SwitchStatementAst] -and $node.Condition.Extent.Text -eq '$Command' }, $false))[-1]
        $clause = @($dispatch.Clauses | Where-Object { $_.Item1.Value -eq 'sql-boot-catalog-local' })
        Assert-Equal 1 $clause.Count '分派一处'
        Assert-Match $clause[0].Item2.Extent.Text 'sql_boot_catalog_local\.ps1' '调的是生成器'
        Assert-Match $clause[0].Item2.Extent.Text '-Services \$GoServices' '透传服务清单'
    }
    Test-Case 'start_game 每次启动都先生成目录,再设环境,再起第一个写者' {
        $startAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../start_game.ps1'), [ref]$null, [ref]$null)
        $commands = @($startAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] }, $true))
        $assignments = @($startAst.FindAll({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] }, $true))
        $generate = @($commands | Where-Object { $_.GetCommandName() -eq 'Invoke-Dev' -and $_.Extent.Text.Contains('sql-boot-catalog-local') })
        Assert-Equal 1 $generate.Count '生成接线唯一'
        $firstWriter = @($commands | Where-Object { $_.GetCommandName() -eq 'Start-LocalGoServices' -and $_.Extent.Text -match "'db','data_service'" })
        Assert-Equal 1 $firstWriter.Count '第一批写者'
        $setCatalog = @($assignments | Where-Object { $_.Left.Extent.Text -ceq '$env:MMORPG_SQL_BOOT_CATALOG' -and $_.Right.Extent.Text.Contains('$sqlBootSummary.catalog') })
        Assert-Equal 1 $setCatalog.Count '设置目录环境变量'
        Assert-True ($generate[0].Extent.EndOffset -lt $setCatalog[0].Extent.StartOffset) '生成在设置之前'
        Assert-True ($setCatalog[0].Extent.EndOffset -lt $firstWriter[0].Extent.StartOffset) '设置在起服务之前'
        $allowList = @($assignments | Where-Object { $_.Left.Extent.Text -ceq '$env:MMORPG_DB_ALLOWED_DATABASES' -and $_.Right.Extent.Text.Contains('zone_1_db') })
        Assert-Equal 1 $allowList.Count '玩家库白名单,全球 DS/DB 没有它就拒启'
        Assert-True ($allowList[0].Extent.EndOffset -lt $firstWriter[0].Extent.StartOffset) '白名单先于第一批写者'
        Assert-True ($generate[0].Extent.Text.Contains("'gateway'")) '网关也要授权'
        foreach ($restore in @('$env:MMORPG_SQL_BOOT_CATALOG = $oldSqlBootCatalog', '$env:MMORPG_DB_ALLOWED_DATABASES = $oldAllowedDatabases')) {
            Assert-True ($startAst.Extent.Text.Contains($restore)) 'finally 还原父环境'
        }
        $gatewayGate = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-SqlWriterBootEnvironment' -and $_.Extent.Text.Contains("'gateway'") })
        $gatewayStart = @($commands | Where-Object { $_.GetCommandName() -eq 'Start-Process' -and $_.Extent.Text.Contains('$java') -and $_.Extent.Text.Contains('-jar') })
        Assert-Equal 1 $gatewayGate.Count '网关走同一闸门'
        Assert-Equal 1 $gatewayStart.Count '网关启动一处'
        Assert-True ($gatewayGate[0].Extent.EndOffset -lt $gatewayStart[0].Extent.StartOffset) '授权先于网关进程'
        Assert-Match $gatewayStart[0].Extent.Text 'spring\.config\.additional-location=file:' '账号走每 boot 配置'
        Assert-NotMatch $startAst.Extent.Text 'MMORPG_MAIL_SQL_DSN|MMORPG_RANK_SQL_DSN' '连接串只来自目录,不再要求父环境'
    }
    Test-Case '写者清单与 go_services 的启动门禁一致' {
        $launcherAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../go_services.ps1'), [ref]$null, [ref]$null)
        $start = $launcherAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Start-ServiceInstance' }, $true)
        $match = [regex]::Match($start.Extent.Text, 'if \(\$Name -in @\((?<list>[^)]+)\)\) \{\s+\$selectedConfig')
        Assert-True $match.Success '找到门禁清单'
        $gated = @($match.Groups['list'].Value -split ',' | ForEach-Object { $_.Trim().Trim("'") } | Sort-Object)
        $expected = @(Get-LocalSqlBootWriterServices | Where-Object { $_ -cne 'gateway' } | Sort-Object)
        Assert-Equal ($expected -join ',') ($gated -join ',') 'go_services 要授权的服务,目录都发'
        Assert-NotMatch $launcherAst.Extent.Text 'DB_AUTO_MIGRATE_SCHEMA.,.1' 'InitDB 见到它拒启,表由 go/db cmd/migrate 建'
    }
    Test-Case '每 boot 配置的进程身份认得出来:分批启动和重复启动才不会误判' {
        $launcherAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../go_services.ps1'), [ref]$null, [ref]$null)
        $definition = $launcherAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Test-InstanceProcessIdentity' }, $true)
        . ([scriptblock]::Create($definition.Extent.Text))
        $GoRoot = 'C:\fixture\go'
        $GoBinDir = 'C:\fixture\bin'
        $DerivedEtcDir = 'C:\fixture\run\etc'
        $info = @{ Dir = 'db'; Entry = '.'; ConfigFlag = '-f'; ConfigFile = 'etc/db.yaml' }
        $private = 'C:\fixture\run\secrets\sql-boot-local\boots\ab\db\runtime\db.yaml'
        $process = [pscustomobject]@{ ExecutablePath = 'C:\fixture\bin\db.exe'; CommandLine = "`"C:\fixture\bin\db.exe`" `"-f`" `"$private`"" }
        Assert-Equal $true (Test-InstanceProcessIdentity -Name db -Info $info -InstanceKey db -Process $process -RecordedConfigPath $private) '启动时记下的路径 + 带引号的开关'
        Assert-Equal $false (Test-InstanceProcessIdentity -Name db -Info $info -InstanceKey db -Process $process) '没记录就不认'
        Assert-Equal $false (Test-InstanceProcessIdentity -Name db -Info $info -InstanceKey db -Process $process -RecordedConfigPath 'C:\other\db.yaml') '记录对不上不认'
        $derived = [pscustomobject]@{ ExecutablePath = 'C:\fixture\bin\db.exe'; CommandLine = "`"C:\fixture\bin\db.exe`" `"-f`" `"C:\fixture\run\etc\db.yaml`"" }
        Assert-Equal $true (Test-InstanceProcessIdentity -Name db -Info $info -InstanceKey db -Process $derived) '派生配置的真实命令行形状也认'
    }
    Test-Case 'player_inbox 拿到 scene 的入场验签密钥,否则常驻进程拒启' {
        . "$PSScriptRoot/../lib/account_directory_dev_secret.ps1"
        Assert-Equal 'MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE' ((Get-AccountDirectoryDevSecretNames -Service 'player-inbox') -join ',') '只给这一把'
        Assert-Match ([IO.File]::ReadAllText((Join-Path $PSScriptRoot '../go_services.ps1'))) "@\('data-service','login','trade','match','scene-manager','db','player-inbox'\)" '启动器接线'
    }
}
finally {
    foreach ($name in $touchedVariables) { [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process') }
    $resolved = [IO.Path]::GetFullPath($fixtureRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolved) -notlike 'mmorpg-sql-boot-tests-*') { throw '拒绝清理临时测试范围之外的目录。' }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
exit (Complete-TestRun -SuiteName '本机写者启动目录生成器')
