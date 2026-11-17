#requires -Version 7
<# 只读验收：原栈身份、邮件健康和注册、空业务表。不会发送邮件或改变玩家资产。 #>
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$out = Join-Path $repo 'build/verification/gm-admin-20261117/mail'
. (Join-Path $repo 'tools/scripts/lib/sql_boot_catalog_local.ps1')
function Read-MailDocker([string[]]$Arguments) {
    $result = Invoke-LocalSqlBootProcess -FilePath docker -Arguments $Arguments -TimeoutSeconds 30
    if ($result.Code -ne 0) { throw 'Read-only Docker probe failed.' }
    return $result.Out
}
$before = Get-Content -LiteralPath (Join-Path $out 'existing-before.json') -Raw | ConvertFrom-Json
$comparisons = foreach ($old in $before.existing) {
    $current = @(Read-MailDocker @('inspect', $old.name) | ConvertFrom-Json)[0]
    @{ name = $old.name; unchanged = ($old.id -ceq $current.Id -and $old.startedAt -ceq $current.State.StartedAt -and $old.imageId -ceq $current.Image); running = $current.State.Running }
}
$mail = @(Read-MailDocker @('inspect', 'xuanming-gm-mail-mail-1') | ConvertFrom-Json)[0]
$health = Invoke-WebRequest -Uri 'http://127.0.0.1:9244/healthz' -TimeoutSec 5
$registry = Read-MailDocker @('exec', 'xuanming-debug-current-etcd-1', 'etcdctl', 'get', 'MailNodeService.rpc/zone/1/', '--prefix', '--write-out=json') | ConvertFrom-Json
$nodes = @($registry.kvs | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_.value)) | ConvertFrom-Json })
$matching = @($nodes | Where-Object { $_.grpcEndpoint.ip -ceq '172.29.220.19' -and $_.grpcEndpoint.port -eq 50900 })
# 口令由 mysql 容器本身的环境传递；参数、输出和证据均不包含口令。
$sql = 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=TCP -h127.0.0.1 -uroot -Nse "SELECT COUNT(*) FROM mmorpg_mail.mail_message; SELECT COUNT(*) FROM mmorpg_mail.mail_asset_op;"'
$counts = (Read-MailDocker @('exec', 'xuanming-debug-current-mysql-1', '/bin/sh', '-c', $sql)).Trim() -split '\r?\n'
$result = [ordered]@{
    verifiedAt = [DateTime]::UtcNow.ToString('o')
    existingServices = @($comparisons)
    existingServicesUnchanged = (@($comparisons | Where-Object { -not $_.unchanged -or -not $_.running }).Count -eq 0)
    mailContainerId = $mail.Id
    mailHealthy = ($mail.State.Health.Status -ceq 'healthy' -and $health.StatusCode -eq 200)
    mailNodeRegistered = ($matching.Count -eq 1)
    mailMessages = $counts[0]
    mailAssetOperations = $counts[1]
    playerMailSentByThisTask = $false
    sceneAssetClaimVerified = $false
}
Write-LocalSqlBootText -Path (Join-Path $out 'verification.json') -Text ($result | ConvertTo-Json -Depth 6)
foreach ($name in @('mail-prepare', 'mail-migrate', 'mail-register', 'mail')) {
    $log = Invoke-LocalSqlBootProcess -FilePath docker -Arguments @('logs', "xuanming-gm-mail-$name-1") -TimeoutSeconds 30
    $safe = (($log.Out + $log.Err) -split '\r?\n' | ForEach-Object { ConvertTo-LocalSqlBootSafeText $_ }) -join "`n"
    Write-LocalSqlBootText -Path (Join-Path $out "$name.log") -Text $safe
}
if (-not $result.existingServicesUnchanged -or -not $result.mailHealthy -or -not $result.mailNodeRegistered) { throw 'Mail read-only readiness verification failed; inspect verification.json.' }
Write-Output "Verified mail health and registration; $($comparisons.Count) existing game containers unchanged. Attachment claim remains unverified."
