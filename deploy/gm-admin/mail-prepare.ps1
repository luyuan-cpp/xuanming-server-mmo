#requires -Version 7
<# 只读采集运行栈身份，并在本任务忽略目录生成私有配置；不启动或重建容器。 #>
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$out = Join-Path $repo 'build/verification/gm-admin-20261117/mail'
$private = Join-Path $out 'private'
. (Join-Path $repo 'tools/scripts/lib/sql_boot_catalog_local.ps1')

function Get-MailContainer([string]$Name) {
    $result = Invoke-LocalSqlBootProcess -FilePath 'docker' -Arguments @('inspect', $Name)
    if ($result.Code -ne 0) { throw 'Required game container is missing.' }
    return @($result.Out | ConvertFrom-Json)[0]
}
function Get-MailEnvironment($Container) {
    $values = @{}
    foreach ($line in $Container.Config.Env) {
        $split = $line.IndexOf('=')
        if ($split -gt 0) { $values[$line.Substring(0, $split)] = $line.Substring($split + 1) }
    }
    return $values
}
function Write-MailEnvironment([string]$Path, [Collections.IDictionary]$Values) {
    $lines = foreach ($key in $Values.Keys) {
        if ($key -cnotmatch '^[A-Z][A-Z0-9_]*$' -or [string]$Values[$key] -match "[\r\n]") { throw 'Invalid environment file entry.' }
        "$key=$($Values[$key])"
    }
    Write-LocalSqlBootText -Path $Path -Text (($lines -join "`n") + "`n")
}

try {
    $imagePath = Join-Path $out 'image.json'
    if (-not (Test-Path -LiteralPath $imagePath)) { throw 'Build evidence is missing; run mail-build.py first.' }
    $image = Get-Content -LiteralPath $imagePath -Raw | ConvertFrom-Json
    $mysql = Get-MailContainer 'xuanming-debug-current-mysql-1'
    $data = Get-MailContainer 'xuanming-debug-current-data-service-1'
    $gate = Get-MailContainer 'xuanming-debug-gate'
    $scene = Get-MailContainer 'xuanming-debug-scene'
    $boot = Get-MailContainer 'xuanming-debug-current-rank-sql-boot-1'
    $mysqlEnv = Get-MailEnvironment $mysql
    $dataEnv = Get-MailEnvironment $data
    $gateEnv = Get-MailEnvironment $gate
    $sceneEnv = Get-MailEnvironment $scene
    foreach ($name in @('MMORPG_ROLE_FENCE_SECRET_MAIL', 'MMORPG_ROLE_FENCE_SECRET_COORDINATOR')) {
        if ([string]::IsNullOrWhiteSpace($dataEnv[$name])) { throw 'Data-service mail role-fence configuration is missing.' }
    }
    if ([string]::IsNullOrWhiteSpace($gateEnv['MMORPG_INTERNAL_AUTH_PUBLIC_KEYS']) -or [string]::IsNullOrWhiteSpace($mysqlEnv['MYSQL_ROOT_PASSWORD'])) { throw 'Required game service configuration is missing.' }
    $networkResult = Invoke-LocalSqlBootProcess -FilePath 'docker' -Arguments @('network', 'inspect', 'xuanming-debug-current_default')
    if ($networkResult.Code -ne 0) { throw 'Game network is missing.' }
    $network = @($networkResult.Out | ConvertFrom-Json)[0]
    foreach ($member in $network.Containers.PSObject.Properties.Value) {
        if ($member.IPv4Address -eq '172.29.220.19/24' -and $member.Name -notlike 'xuanming-gm-mail-*') { throw 'Mail address is already owned by another service.' }
    }
    $null = & git -C $repo check-ignore --quiet $private
    if ($LASTEXITCODE -ne 0) { throw 'Private configuration must be git-ignored.' }
    Initialize-LocalSqlBootPrivateDirectory -Path $private
    $config = Join-Path $private 'config'
    $null = [IO.Directory]::CreateDirectory($config)
    $secretPath = Join-Path $private 'secrets.json'
    if (Test-Path -LiteralPath $secretPath) { $secrets = Get-Content -LiteralPath $secretPath -Raw | ConvertFrom-Json }
    else {
        $secrets = @{ admin = (New-LocalSqlBootSecret); asset = (New-LocalSqlBootSecret) }
        Write-LocalSqlBootText -Path $secretPath -Text ($secrets | ConvertTo-Json)
    }
    $sourceConfig = Join-Path $repo 'build/docker-debug-current/batches/20261106-latest-r1/config'
    $dataConfig = Join-Path $sourceConfig 'data_service.yaml'
    $loginConfig = Join-Path $sourceConfig 'login.yaml'
    $globalDb = Get-LocalSqlBootYamlRequired -Path $dataConfig -KeyPath 'SnapshotMySQL.DBName'
    $shards = Get-LocalSqlBootYamlRequired -Path $dataConfig -KeyPath 'Directory.LogicalShards'
    $region = Get-LocalSqlBootYamlRequired -Path $loginConfig -KeyPath 'Placement.Region'
    $stores = Get-LocalSqlBootYamlRequired -Path $loginConfig -KeyPath 'Placement.StorageShards'
    Write-LocalSqlBootText -Path (Join-Path $config 'data_service.yaml') -Text "SnapshotMySQL:`n  DBName: $globalDb`nDirectory:`n  LogicalShards: $shards`n"
    Write-LocalSqlBootText -Path (Join-Path $config 'login.yaml') -Text "Placement:`n  Region: $region`n  StorageShards: $stores`n"
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'mail-config.yaml') -Destination (Join-Path $config 'mail.yaml')
    Write-MailEnvironment (Join-Path $private 'sql.env') @{ MYSQL_ROOT_PASSWORD = $mysqlEnv['MYSQL_ROOT_PASSWORD'] }
    Write-MailEnvironment (Join-Path $private 'migrate.env') @{
        MMORPG_MAIL_SQL_DSN = (New-LocalSqlBootDsn -User 'root' -Password $mysqlEnv['MYSQL_ROOT_PASSWORD'] -Endpoint 'mysql:3306' -Database 'mmorpg_mail')
        MMORPG_SQL_SCHEMA_PRINCIPAL = 'root@%'
    }
    $runtime = [ordered]@{
        MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = $gateEnv['MMORPG_INTERNAL_AUTH_PUBLIC_KEYS']
        MMORPG_ROLE_FENCE_SECRET_MAIL = $dataEnv['MMORPG_ROLE_FENCE_SECRET_MAIL']
        MMORPG_ROLE_FENCE_SECRET_COORDINATOR = $dataEnv['MMORPG_ROLE_FENCE_SECRET_COORDINATOR']
        MAIL_PRODUCER_SECRET_ADMIN = $secrets.admin
        MMORPG_ASSET_OP_SECRET_MAIL = $secrets.asset
    }
    foreach ($key in @('KAFKA_COMMAND_TOPIC_GENERATION', 'KAFKA_COMMAND_TOPIC_PARTITIONS')) {
        if ([string]::IsNullOrWhiteSpace($dataEnv[$key])) { throw 'Kafka command contract configuration is missing.' }
        $runtime[$key] = $dataEnv[$key]
    }
    Write-MailEnvironment (Join-Path $private 'runtime.env') $runtime
    Write-MailEnvironment (Join-Path $private 'admin.env') @{ MAIL_PRODUCER_SECRET_ADMIN = $secrets.admin; GM_MAIL_TARGET = '127.0.0.1:2354' }
    Write-MailEnvironment (Join-Path $private 'scene.env') @{ MMORPG_ASSET_OP_SECRET_MAIL = $secrets.asset }
    $originalPrivate = @($boot.Mounts | Where-Object Destination -EQ '/run/sql-boot/private')[0].Name
    if (-not $originalPrivate) { throw 'Original SQL authority volume is missing.' }
    $entry = Join-Path $repo 'build/docker-debug-current/batches/20261106-latest-r1/sql-writer-entry.sh'
    Write-MailEnvironment (Join-Path $private 'compose.env') @{
        GM_MAIL_IMAGE = $image.imageId
        GM_MAIL_SQL_BOOT_IMAGE = $boot.Image
        GM_MAIL_REPO = $repo.Replace('\', '/')
        GM_MAIL_PRIVATE = $private.Replace('\', '/')
        GM_MAIL_SQL_ENTRY = $entry.Replace('\', '/')
        GM_MAIL_ORIGINAL_PRIVATE = $originalPrivate
    }
    $ids = Invoke-LocalSqlBootProcess -FilePath 'docker' -Arguments @('ps', '-q', '--filter', 'name=xuanming-debug')
    if ($ids.Code -ne 0) { throw 'Could not capture existing game identities.' }
    $rows = foreach ($id in ($ids.Out -split '\r?\n' | Where-Object { $_ })) {
        $container = Get-MailContainer $id
        @{ name = $container.Name; id = $container.Id; startedAt = $container.State.StartedAt; imageId = $container.Image }
    }
    $evidence = @{ existing = @($rows); capturedAt = [DateTime]::UtcNow.ToString('o'); sceneMailKeyPresent = $sceneEnv.ContainsKey('MMORPG_ASSET_OP_SECRET_MAIL'); sceneMailKeyMatches = ($sceneEnv['MMORPG_ASSET_OP_SECRET_MAIL'] -ceq $secrets.asset); dataMailRoleFencePresent = $true; mailAssetClaimAccepted = $false }
    $snapshot = Join-Path $out 'existing-before.json'
    if (-not (Test-Path -LiteralPath $snapshot)) { Write-LocalSqlBootText -Path $snapshot -Text ($evidence | ConvertTo-Json -Depth 6) }
    Write-LocalSqlBootText -Path (Join-Path $out 'preflight.json') -Text ($evidence | ConvertTo-Json -Depth 6)
    Write-Output 'Prepared isolated mail configuration. No containers were started or restarted. Scene asset key still requires review.'
} catch {
    # 不把捕获中的 Docker 环境或原始异常对象写到日志。
    Write-Error ("Mail preparation failed: " + (ConvertTo-LocalSqlBootSafeText $_.Exception.Message))
    exit 1
}
