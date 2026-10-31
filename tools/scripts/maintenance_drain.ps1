#requires -Version 7
[CmdletBinding()]
param(
    [ValidateSet('library','render-roles','close-publication','activate-runtime','close-source','close-relay','retire-source-writers','retire-relay-writers','open-source','stop-and-collect','seed-redis-nonce','collect-redis','collect-kafka','manual-baseline','manual-sampling-request','normal-tool','verify-and-advance')]
    [string]$MaintenanceCommand = 'library',
    [string]$PlanPath = '',
    [string]$OutputPath = ''
)

# 本模块可安全dot-source；只定义函数，绝不在导入时运行native/SQL或编译helper。
function Read-MaintenanceJson {
    param([Parameter(Mandatory)][string]$Path,[int]$MaxBytes = 16777216,[string]$ExpectedSha256 = '',[ref]$ContentSha256)
    $full = [IO.Path]::GetFullPath($Path)
    $info = Get-Item -LiteralPath $full -ErrorAction Stop
    if ($info.PSIsContainer -or ($info.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $info.Length -gt $MaxBytes) { throw '维护原件类型或预算不符' }
    $bytes = [IO.File]::ReadAllBytes($full)
    if($bytes.Length -gt $MaxBytes){throw '读取期间维护原件超过预算'}
    $digest=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
    if($ExpectedSha256 -and $digest -cne $ExpectedSha256){throw '所读取维护原件与固定SHA不符'}
    if($null -ne $ContentSha256){$ContentSha256.Value=$digest}
    $text = [Text.UTF8Encoding]::new($false,$true).GetString($bytes)
    $doc = [Text.Json.JsonDocument]::Parse($text)
    try { Assert-MaintenanceJsonKeys -Element $doc.RootElement } finally { $doc.Dispose() }
    return ($text | ConvertFrom-Json -AsHashtable -Depth 80)
}

function Assert-MaintenanceJsonKeys {
    param([Text.Json.JsonElement]$Element)
    if ($Element.ValueKind -eq [Text.Json.JsonValueKind]::Object) {
        $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
        foreach ($property in $Element.EnumerateObject()) {
            if (-not $seen.Add($property.Name)) { throw '维护原件有重复或大小写混淆字段' }
            Assert-MaintenanceJsonKeys -Element $property.Value
        }
    } elseif ($Element.ValueKind -eq [Text.Json.JsonValueKind]::Array) {
        foreach ($item in $Element.EnumerateArray()) { Assert-MaintenanceJsonKeys -Element $item }
    }
}

function Assert-MaintenanceProperties {
    param([Collections.IDictionary]$Value,[string[]]$Required,[string[]]$Optional = @())
    foreach ($key in $Required) { if (-not $Value.Contains($key)) { throw "维护原件缺字段：$key" } }
    foreach ($key in $Value.Keys) { if ($key -cnotin ($Required + $Optional)) { throw "维护原件有未知字段：$key" } }
}

function Get-MaintenanceSha256 {
    param([Parameter(Mandatory)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Write-MaintenanceImmutableBytes {
    param([Parameter(Mandatory)][string]$Path,[Parameter(Mandatory)][byte[]]$Bytes)
    $full=[IO.Path]::GetFullPath($Path)
    $parent=Get-Item -LiteralPath ([IO.Path]::GetDirectoryName($full)) -ErrorAction Stop
    if(-not $parent.PSIsContainer -or $parent.Attributes -band [IO.FileAttributes]::ReparsePoint){throw '正式动作原件必须写入已存在的受控非重解析目录'}
    if([IO.File]::Exists($full)){
        $existing=Get-Item -LiteralPath $full -ErrorAction Stop
        if($existing.Attributes -band [IO.FileAttributes]::ReparsePoint -or $existing.Length -ne $Bytes.Length){throw '已存在动作原件类型/长度冲突'}
        $actual=[IO.File]::ReadAllBytes($full)
        if(-not [Security.Cryptography.CryptographicOperations]::FixedTimeEquals($actual,$Bytes)){throw '已存在动作原件与同一请求内容冲突'}
        return
    }
    $stream=[IO.FileStream]::new($full,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::Read)
    try{$stream.Write($Bytes,0,$Bytes.Length);$stream.Flush($true)}finally{$stream.Dispose()}
}

function Activate-MaintenanceRuntime {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$Output)
    if(-not [IO.Path]::IsPathRooted($Plan.controlDirectory) -or -not [IO.Directory]::Exists($Plan.controlDirectory)){throw '在线boot必须预先固定自己的私有controlDirectory'}
    $null=Read-MaintenanceJson -Path $Plan.planFile -ExpectedSha256 $Plan.runtimePlanSha256
    $bytes=[IO.File]::ReadAllBytes($Plan.planFile)
    if([Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant() -cne $Plan.runtimePlanSha256){throw '激活前正式runtime原字节变化'}
    # 先写完整runtime，再写其hash；应用只有两者齐全且SHA一致才激活，不把中断当新计划覆盖。
    Write-MaintenanceImmutableBytes -Path (Join-Path $Plan.controlDirectory 'active.runtime.json') -Bytes $bytes
    Write-MaintenanceImmutableBytes -Path (Join-Path $Plan.controlDirectory 'active.runtime.sha256') -Bytes ([Text.Encoding]::ASCII.GetBytes($Plan.runtimePlanSha256+"`n"))
    $null=Write-MaintenanceImmutableJson -Path $Output -Value ([ordered]@{kind='RUNTIME_CONTEXT_PUBLISHED';operationId=$Plan.operationId;manifestSha256=$Plan.manifestSha256;runtimePlanSha256=$Plan.runtimePlanSha256;controlDirectory=$Plan.controlDirectory})
}

function Get-MaintenanceRetirementRequests {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][ValidateSet('P1','P3')][string]$Stage)
    if($Plan.operationId -cnotmatch '^[a-f0-9]{32}$' -or @($Plan.writerRetirements).Count -eq 0 -or @($Plan.writerRetirements).Count -gt 1024){throw '维护退休缺固定run/有限Request清单'}
    $run=[Convert]::ToBase64String([Convert]::FromHexString($Plan.operationId))
    $requests=[Collections.Generic.List[object]]::new();$seen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($entry in $Plan.writerRetirements){
        Assert-MaintenanceProperties -Value $entry -Required @('stage','requestFile','requestSha256')
        if($entry.stage -cnotin @('P1','P3') -or $entry.requestSha256 -cnotmatch '^[a-f0-9]{64}$' -or -not [IO.Path]::IsPathRooted($entry.requestFile)){throw '退休原件阶段或固定来源非法'}
        $request=Read-MaintenanceJson -Path $entry.requestFile -ExpectedSha256 $entry.requestSha256 -MaxBytes 16384
        if($request.kind -cne 'RETIRE' -or $request.maintenanceRunId -cne $run -or [uint64]$request.expectedEpoch -eq 0 -or [uint64]$request.expectedCatalogRevision -eq 0 -or [Convert]::FromBase64String($request.requestId).Length -ne 16 -or [Convert]::FromBase64String($request.writer.writerKey).Length -ne 16){throw '退休原件未绑定同run、原writer、当前epoch/catalog'}
        if(-not $seen.Add([string]$request.writer.writerKey)){throw '同run退休物理writer重复，不能P1/P3各退一次'}
        $roles=@($Plan.issuers | Where-Object { $_.physicalWriterKey -ceq $request.writer.writerKey })
        if($roles.Count -eq 0){throw '退休writer不在P0固定issuer投影中'}
        foreach($role in $roles){if($role.instanceId -cne $request.writer.instanceId){throw '同物理writer的P0 boot身份冲突'}}
        $relay=@($roles | Where-Object { $_.logicalRole -ceq 'RELAY' -or ($_.Contains('sharesRelay') -and [bool]$_.sharesRelay) })
        if(($entry.stage -ceq 'P1' -and $relay.Count -ne 0) -or ($entry.stage -ceq 'P3' -and $relay.Count -eq 0)){throw 'P1只能退无relay writer；共享/relay必须P3实际闭合后退'}
        if($entry.stage -ceq $Stage){$requests.Add([ordered]@{entry=$entry;request=$request})}
    }
    if($requests.Count -eq 0){throw '本阶段没有已固定退休请求，不臆造新ID/目录版本'}
    return $requests.ToArray()
}

function Invoke-MaintenanceWriterRetirement {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][ValidateSet('P1','P3')][string]$Stage,[Parameter(Mandatory)][string]$OutputDirectory)
    $requests=@(Get-MaintenanceRetirementRequests -Plan $Plan -Stage $Stage)
    $exe=[IO.Path]::GetFullPath($Plan.writerAuthorityExecutable)
    if($Plan.writerAuthorityExecutableSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $exe) -cne $Plan.writerAuthorityExecutableSha256){throw '退休工具不是预先固定正规writer_authority工件'}
    if([IO.Directory]::Exists($OutputDirectory)){throw '退休收集目录必须新建；UNKNOWN只按原Request调用status，不重新整批执行'}
    $null=[IO.Directory]::CreateDirectory([IO.Path]::GetFullPath($OutputDirectory))
    $clock=[Diagnostics.Stopwatch]::StartNew();$receipts=[Collections.Generic.List[object]]::new()
    foreach($one in $requests){
        $left=60000-$clock.ElapsedMilliseconds;if($left -le 0){throw '本阶段退休耗尽既有60秒预算，保留所有原件HOLD'}
        if((Get-MaintenanceSha256 $one.entry.requestFile) -cne $one.entry.requestSha256){throw '执行前退休原件变化'}
        $id=[Convert]::ToHexString([Convert]::FromBase64String($one.request.requestId)).ToLowerInvariant()
        $output=Join-Path $OutputDirectory ($id+'.receipt.protojson')
        # 生命周期core在同SQL事务核P1/P3永久closure链，并将本run RETIRE追加到catalog；这里从不重拍/改P0 Plan。
        & $exe -command retire -request $one.entry.requestFile -input-root ([IO.Path]::GetDirectoryName($one.entry.requestFile)) -receipt-output $output -timeout ($left.ToString([Globalization.CultureInfo]::InvariantCulture)+'ms')
        if($LASTEXITCODE -ne 0){throw '退休未获原永久收据；保持关闭，仅对原Request做status'}
        $record=Read-MaintenanceJson -Path $output
        if($record.kind -cne 'RETIRE' -or $record.requestId -cne $one.request.requestId){throw '退休工具没有返回该请求的正式永久结局'}
        $receipts.Add([ordered]@{absolutePath=[IO.Path]::GetFullPath($output);sha256=[Convert]::ToBase64String([Convert]::FromHexString((Get-MaintenanceSha256 $output)))})
    }
    $null=Write-MaintenanceImmutableJson -Path (Join-Path $OutputDirectory 'retirement-receipts.sources.json') -Value $receipts.ToArray()
}



function Get-NormalFiniteToolArguments {
    param([Parameter(Mandatory)][Collections.IDictionary]$Tool,[Parameter(Mandatory)][string]$RuntimeConfig)
    if($Tool.kind -cne 'assetopfix' -or -not [IO.Path]::IsPathRooted($RuntimeConfig) -or @($Tool.arguments).Count -gt 128){throw 'finite normal只接受具名工具及有限参数'}
    $business=[Collections.Generic.List[string]]::new()
    foreach($arg in $Tool.arguments){
        if($arg -isnot [string] -or $arg.IndexOf([char]0) -ge 0 -or $arg -cmatch '^--?(maintenance-drain-(plan|dir|sha256)|f|config|db-config|file|dry-run)(=|$)'){throw 'finite参数不能覆盖固定配置、输入、写入模式或伪装维护runtime'}
        $business.Add($arg)
    }
    return @('-f',$RuntimeConfig,'resolve')+$business.ToArray()
}

function Invoke-NormalFiniteTool {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$OutputDirectory,[Parameter(Mandatory)][string]$RepoRoot)
    Assert-PublicationStartAllowed -RepoRoot $RepoRoot
    $tool=$Plan.normalTool
    foreach($name in @('kind','executable','executableSha256','instanceKey','workingDirectory','arguments','timeoutSeconds','retirementRequestFile','retirementRequestSha256')){if(-not $tool.Contains($name)){throw 'finite normal缺固定执行输入'}}
    if(-not [IO.Path]::IsPathRooted($tool.executable) -or -not [IO.Path]::IsPathRooted($tool.workingDirectory) -or -not [IO.Directory]::Exists($tool.workingDirectory) -or $tool.executableSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $tool.executable) -cne $tool.executableSha256 -or (Get-Item -LiteralPath $tool.executable).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'finite normal必须固定正规exe及工作目录'}
    $seconds=[int]$tool.timeoutSeconds;if($seconds -le 0 -or $seconds -gt 3600){throw 'finite normal执行预算必须在1至3600秒'}
    if(-not [IO.Path]::IsPathRooted($OutputDirectory) -or -not [IO.Directory]::Exists($OutputDirectory) -or (Get-Item -LiteralPath $OutputDirectory).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'finite输出必须已有私有目录'}
    $config='';$environment=Get-SqlWriterBootEnvironment -InstanceKey $tool.instanceKey -RepoRoot $RepoRoot -SelectedRuntimeConfigFile ([ref]$config)
    $grant=Read-MaintenanceJson -Path $environment.MMORPG_SQL_WRITER_GRANT_FILE -ExpectedSha256 $environment.MMORPG_SQL_WRITER_GRANT_SHA256
    if(-not [string]::IsNullOrEmpty([string]$grant.maintenanceRunId)){throw '此入口只运行非run NORMAL工具；已绑定run必须走存活应用的正式排空'}
    $arguments=@(Get-NormalFiniteToolArguments -Tool $tool -RuntimeConfig $config)
    if(-not [IO.Path]::IsPathRooted($tool.retirementRequestFile) -or $tool.retirementRequestSha256 -cnotmatch '^[a-f0-9]{64}$'){throw 'finite退休必须固定绝对Request与原bytes SHA'}
    $retirement=Read-MaintenanceJson -Path $tool.retirementRequestFile -ExpectedSha256 $tool.retirementRequestSha256
    if($retirement.kind -cne 'RETIRE' -or -not [string]::IsNullOrEmpty([string]$retirement.maintenanceRunId) -or $retirement.writer.writerKey -cne $grant.writerKey -or $retirement.writer.instanceId -cne $grant.instanceId -or $retirement.writer.registrationDigest -cne $grant.registrationDigest){throw 'finite RETIRE必须预绑定本NORMAL boot且未绑定维护run'}
    $registrar=[string]$Plan.writerAuthorityExecutable
    if(-not [IO.Path]::IsPathRooted($registrar) -or $Plan.writerAuthorityExecutableSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $registrar) -cne $Plan.writerAuthorityExecutableSha256){throw 'finite退休工具工件不符'}
    $attempt=Join-Path $OutputDirectory 'normal-tool.started.json'
    # CreateNew先于启动。任何中断都保留原尝试，禁止自动重跑业务命令。
    $null=Write-MaintenanceImmutableJson -Path $attempt -Value ([ordered]@{kind=$tool.kind;executableSha256=$tool.executableSha256;writerKey=$grant.writerKey;instanceId=$grant.instanceId;retirementRequestSha256=$tool.retirementRequestSha256})
    $start=[Diagnostics.ProcessStartInfo]::new();$start.FileName=$tool.executable;$start.WorkingDirectory=$tool.workingDirectory;$start.UseShellExecute=$false;$start.CreateNoWindow=$true
    foreach($arg in $arguments){$start.ArgumentList.Add($arg)}
    foreach($name in @('MMORPG_SQL_REGISTRAR_DSN','MMORPG_SQL_MAINTENANCE_DSN','MMORPG_SQL_SCHEMA_DSN','MMORPG_SQL_OPERATOR_PRINCIPAL','MMORPG_SQL_SCHEMA_PRINCIPAL','CHAT_AUTHORITY_DSN','MMORPG_GUILD_SQL_DSN','MMORPG_MAIL_SQL_DSN','MMORPG_RANK_SQL_DSN','MATCH_MYSQL_PASSWORD','MMORPG_MAINTENANCE_CONTROL_DIR','MMORPG_MAINTENANCE_PREPARE_ONLY')){$null=$start.Environment.Remove($name)}
    foreach($name in $environment.Keys){$start.Environment[$name]=[string]$environment[$name]}
    if((Get-MaintenanceSha256 $tool.executable) -cne $tool.executableSha256){throw 'finite执行前工件变化'}
    $process=[Diagnostics.Process]::new();$process.StartInfo=$start
    try{
        if(-not $process.Start()){throw 'finite normal进程未确认启动'}
        $clock=[Diagnostics.Stopwatch]::StartNew()
        while(-not $process.WaitForExit(100)){if($clock.Elapsed.TotalSeconds -ge $seconds){throw 'finite normal仍存活，保持原尝试，不杀进程、不签退休'}}
        if($process.ExitCode -ne 0){throw 'finite normal非零退出，结果未确认；不能签退休'}
        $null=Write-MaintenanceImmutableJson -Path (Join-Path $OutputDirectory 'normal-tool.exited.json') -Value ([ordered]@{writerKey=$grant.writerKey;instanceId=$grant.instanceId;exitCode=0;executableSha256=$tool.executableSha256})
    }finally{$process.Dispose()}
    # 工具自身只有资源join/Seal成功才退出0。P0竞态若已绑定run，Catalog会拒非run退休，绝不改revision绕过。
    if((Get-MaintenanceSha256 $tool.retirementRequestFile) -cne $tool.retirementRequestSha256){throw 'finite退休原件变化'}
    $receipt=Join-Path $OutputDirectory 'normal-tool.retired.protojson'
    & $registrar -command retire -request $tool.retirementRequestFile -input-root ([IO.Path]::GetDirectoryName($tool.retirementRequestFile)) -receipt-output $receipt -timeout 30s | Out-Null
    if($LASTEXITCODE -ne 0){throw 'finite非run退休未确认，保留原件仅查status，不重跑工具或重拍目录'}
    $confirmed=Read-MaintenanceJson -Path $receipt
    if($confirmed.kind -cne 'RETIRE' -or $confirmed.requestId -cne $retirement.requestId){throw 'finite退休不是原请求永久收据'}
}

function Invoke-MaintenancePublicationVerification {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[switch]$Prestart)
    $exe=[IO.Path]::GetFullPath($Plan.writerAuthorityExecutable)
    if(-not [IO.Path]::IsPathRooted($Plan.writerAuthorityExecutable) -or $Plan.writerAuthorityExecutableSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $exe) -cne $Plan.writerAuthorityExecutableSha256){throw 'publication验证工具必须固定绝对路径及正规工件SHA'}
    $arguments=@('-command',$(if($Prestart){'verify-publication-prestart'}else{'verify-publication-runtime'}),'-timeout','30s')
    if($Prestart){
        $file=[string]$Plan.releaseReceiptFile;$sha=[string]$Plan.releaseReceiptSha256
        $record=Read-MaintenanceJson -Path $file -ExpectedSha256 $sha
        if($record.kind -cne 'RELEASE' -or $record.runPhase -cne 'SQL_RELEASED' -or [uint64]$record.committedMs -eq 0){throw 'prepare启动必须是原永久RELEASE收据'}
        $root=[IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($file))
    }else{
        $root=Assert-MaintenanceControlDirectory -Plan $Plan
        $file=Join-Path $root 'active.runtime.json';$sha=[string]$Plan.runtimePlanSha256
        $record=Read-MaintenanceJson -Path $file -ExpectedSha256 $sha
        $preparedPath=Join-Path $root 'prepared.source.json';$preparedSha='';$prepared=Read-MaintenanceJson -Path $preparedPath -ContentSha256 ([ref]$preparedSha)
        foreach($pin in $prepared.sourcePins){if((Get-MaintenanceSha256 $pin.absolutePath) -cne [Convert]::ToHexString([Convert]::FromBase64String($pin.sha256)).ToLowerInvariant()){throw 'prepared固定原件发生变化'}}
        $arguments+=@('-prepared',$preparedPath,'-prepared-sha256',$preparedSha)
    }
    if(-not [IO.Path]::IsPathRooted($file) -or $sha -cnotmatch '^[a-f0-9]{64}$' -or [Convert]::ToHexString([Convert]::FromBase64String($record.runId)).ToLowerInvariant() -cne $Plan.operationId -or [Convert]::ToHexString([Convert]::FromBase64String($record.evidence.manifestDigest)).ToLowerInvariant() -cne $Plan.manifestSha256){throw 'publication资格原件不是固定同run/manifest'}
    $arguments+=@('-input-root',$root,'-request',$file,'-request-sha256',$sha)
    & $exe @arguments | Out-Null
    if($LASTEXITCODE -ne 0){throw 'publication SQL永久原件/当前run/meta/新boot资格不符，保持关闭'}
    return $record
}

function Request-MaintenanceSourceOpen {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$Output)
    $request=Read-MaintenanceJson -Path $Plan.openRequestFile -ExpectedSha256 $Plan.openRequestSha256
    Assert-MaintenanceProperties -Value $request -Required @('issuerId','instanceId','physicalWriterKey','logicalRole','registrationReceiptId','registrationReceiptDigest','operationId','manifestDigest','openRequestId') -Optional @('openedMs','acceptedFrontierDigest','sourcePins')
    if($request.logicalRole -cne 'SOURCE' -or [uint64]$request.openedMs -ne 0 -or -not [string]::IsNullOrEmpty([string]$request.acceptedFrontierDigest) -or [Convert]::FromBase64String($request.openRequestId).Length -ne 16){throw '开放输入必须是原SOURCE意图，不能伪填已开ACK'}
    if([Convert]::ToHexString([Convert]::FromBase64String($request.operationId)).ToLowerInvariant() -cne $Plan.operationId -or [Convert]::ToHexString([Convert]::FromBase64String($request.manifestDigest)).ToLowerInvariant() -cne $Plan.manifestSha256){throw '开放意图与同run原manifest不符'}
    $prepared=Read-MaintenanceJson -Path (Join-Path $Plan.drainDirectory 'prepared.source.json')
    foreach($key in @('issuerId','instanceId','physicalWriterKey','logicalRole','registrationReceiptId','registrationReceiptDigest')){if([string]$prepared[$key] -cne [string]$request[$key]){throw '开放必须绑定实际prepared新boot及原REGISTER收据'}}
    if([uint64]$prepared.preparedClosedMs -eq 0 -or [Convert]::FromBase64String($prepared.consumerReadyFrontierDigest).Length -ne 32){throw '新boot尚未给出真实prepare-closed frontier'}
    $raw=[IO.File]::ReadAllBytes($Plan.openRequestFile)
    if([Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($raw)).ToLowerInvariant() -cne $Plan.openRequestSha256){throw '开放意图在写入前变化'}
    $ackPath=Join-Path $Plan.drainDirectory 'source.open.ack.json'
    if(-not [IO.File]::Exists($ackPath)){
        $null=Invoke-MaintenancePublicationVerification -Plan $Plan
        # SQL强读后只写同run原意图；应用还须同consumer owner锁原子复核，不能拿此文件当ACK。
        Write-MaintenanceImmutableBytes -Path (Join-Path $Plan.drainDirectory 'open-source.request') -Bytes $raw
    }
    $clock=[Diagnostics.Stopwatch]::StartNew()
    while(-not [IO.File]::Exists($ackPath)){if($clock.ElapsedMilliseconds -ge 60000){throw '实际source开放ACK未确定，不能记PUBLICATION_REOPENED'};Start-Sleep -Milliseconds 50}
    $ack=Read-MaintenanceJson -Path $ackPath
    foreach($key in @('issuerId','instanceId','physicalWriterKey','logicalRole','registrationReceiptId','registrationReceiptDigest','operationId','manifestDigest','openRequestId')){if([string]$ack[$key] -cne [string]$request[$key]){throw '实际source ACK身份/原请求不符'}}
    if([uint64]$ack.openedMs -eq 0 -or [Convert]::FromBase64String($ack.acceptedFrontierDigest).Length -ne 32){throw '应用尚未确定source实际开放'}
    $null=Write-MaintenanceImmutableJson -Path $Output -Value ([ordered]@{absolutePath=[IO.Path]::GetFullPath($ackPath);sha256=[Convert]::ToBase64String([Convert]::FromHexString((Get-MaintenanceSha256 $ackPath)))})
}

function Write-MaintenanceImmutableJson {
    param([Parameter(Mandatory)][string]$Path,[Parameter(Mandatory)][AllowEmptyCollection()]$Value)
    $full = [IO.Path]::GetFullPath($Path)
    $parent = [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($full))
    if ($parent.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '维护输出父目录不允许重解析点' }
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes((ConvertTo-Json -InputObject $Value -Depth 80 -Compress))
    $stream = [IO.FileStream]::new($full,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::Read)
    try { $stream.Write($bytes,0,$bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
    return (Get-MaintenanceSha256 -Path $full)
}

function Get-PublicationFenceRoot {
    param([Parameter(Mandatory)][string]$RepoRoot)
    $configured = [Environment]::GetEnvironmentVariable('MMORPG_PUBLICATION_FENCE_DIR','Process')
    if ([string]::IsNullOrWhiteSpace($configured)) { return (Join-Path $RepoRoot 'run/maintenance/publication-fences') }
    return [IO.Path]::GetFullPath($configured)
}

function Assert-PublicationStartAllowed {
    param([Parameter(Mandatory)][string]$RepoRoot,[Collections.IDictionary]$PrepareGrant,[Collections.IDictionary]$PrepareRuntime,[string]$PrepareRunId)
    $root = Get-PublicationFenceRoot -RepoRoot $RepoRoot
    if (-not [IO.Directory]::Exists($root)) { return }
    if ((Get-Item -LiteralPath $root).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '发布围栏目录不可为重解析点' }
    # 永久围栏无TTL；普通start永远无权删除。重开必须先由正式reopen owner产新能力。
    $fences=@(Get-ChildItem -LiteralPath $root -Filter '*.closed.json' -File -ErrorAction Stop)
    if($fences.Count -eq 0){return}
    if($null -ne $PrepareGrant -and -not [string]::IsNullOrEmpty($PrepareRunId)){throw 'prepare-only不可混用SQL grant与非SQL prestart run'}
    $runBytes=if($null -ne $PrepareGrant){[string]$PrepareGrant.maintenanceRunId}else{$PrepareRunId}
    if($null -eq $PrepareRuntime -or [string]::IsNullOrEmpty($runBytes) -or $PrepareRuntime.runPhase -cne 'SQL_RELEASED'){throw '发布已永久关闭，拒绝重启；须正式同run prepare-only新能力与原件'}
    $run=[Convert]::ToHexString([Convert]::FromBase64String($runBytes)).ToLowerInvariant()
    if($run -cnotmatch '^[a-f0-9]{32}$' -or $PrepareRuntime.runId -cne $runBytes){throw 'prepare-only新能力未绑定同run'}
    $manifest=[Convert]::ToHexString([Convert]::FromBase64String($PrepareRuntime.evidence.manifestDigest)).ToLowerInvariant()
    $matching=@($fences | Where-Object {$_.BaseName -ceq ($run+'.closed')})
    if($matching.Count -ne 1){throw '新prepare能力没有对应原永久发布围栏'}
    $fence=Read-MaintenanceJson -Path $matching[0].FullName
    if($fence.format -cne 'SQL_PUBLICATION_FENCE_V1' -or $fence.operationId -cne $run -or $fence.manifestSha256 -cne $manifest){throw '新prepare能力与原发布围栏/manifest冲突'}
    # 这里只准新boot以应用内关闭准入启动；真正SQL资格仍由normal pool强读，source open必须独立实际ACK。
}

function Set-PermanentPublicationFence {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$RepoRoot)
    foreach ($name in @('operationId','manifestSha256','permanentRevivalFenceId')) {
        if ([string]::IsNullOrWhiteSpace([string]$Plan[$name])) { throw "发布围栏缺少$name" }
    }
    if ($Plan.operationId -cnotmatch '^[a-f0-9]{32}$' -or $Plan.manifestSha256 -cnotmatch '^[a-f0-9]{64}$') { throw '发布围栏身份格式不符' }
    $path = Join-Path (Get-PublicationFenceRoot -RepoRoot $RepoRoot) ($Plan.operationId + '.closed.json')
    $record = [ordered]@{format='SQL_PUBLICATION_FENCE_V1';operationId=$Plan.operationId;manifestSha256=$Plan.manifestSha256;permanentRevivalFenceId=$Plan.permanentRevivalFenceId;closedMs=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()}
    if ([IO.File]::Exists($path)) {
        $old = Read-MaintenanceJson -Path $path
        if ($old.operationId -cne $record.operationId -or $old.manifestSha256 -cne $record.manifestSha256 -or $old.permanentRevivalFenceId -cne $record.permanentRevivalFenceId) { throw '永久围栏原件冲突' }
        return $path
    }
    $null = Write-MaintenanceImmutableJson -Path $path -Value $record
    return $path
}

function Get-SqlWriterBootEnvironment {
    param([Parameter(Mandatory)][string]$InstanceKey,[Parameter(Mandatory)][string]$RepoRoot,[string]$ServiceName='', [ref]$SelectedRuntimeConfigFile,[ref]$PreparedRuntimeArguments,[switch]$PrepareOnlySupported)
    $catalogPath = [Environment]::GetEnvironmentVariable('MMORPG_SQL_BOOT_CATALOG','Process')
    if ([string]::IsNullOrWhiteSpace($catalogPath)) { throw 'SQL写者必须有显式MMORPG_SQL_BOOT_CATALOG，不允许共享账号或默认raw' }
    $catalog = Read-MaintenanceJson -Path $catalogPath
    if (-not $catalog.Contains($InstanceKey)) { throw "实例没有单boot授权：$InstanceKey" }
    $entry = $catalog[$InstanceKey]
    Assert-MaintenanceProperties -Value $entry -Required @('grantFile','grantSha256','consumeDir','deploymentId','physicalAuthoritySha256','runtimeConfigFile','runtimeConfigSha256') -Optional @('runtimeEnvironment','maintenanceControlDir','prepareRuntimeFile','prepareRuntimeSha256','prepareDrainDirectory')
    foreach ($digest in @($entry.grantSha256,$entry.physicalAuthoritySha256)) { if ($digest -cnotmatch '^[a-f0-9]{64}$') { throw 'boot授权摘要格式不符' } }
    if ((Get-MaintenanceSha256 -Path $entry.grantFile) -cne $entry.grantSha256) { throw 'boot授权文件摘要不符' }
    $grant = Read-MaintenanceJson -Path $entry.grantFile -MaxBytes 65536 -ExpectedSha256 $entry.grantSha256
    Assert-MaintenanceProperties -Value $grant -Required @('format','writerKey','instanceId','generation','authoritySchema','protectedSchemas','sqlPrincipal','registrationDigest','deploymentId','grantId','grantedMs','permission','physicalAuthoritySha256','lockPrimitive','lockQualificationSha256') -Optional @('maintenanceRunId')
    if($grant.lockPrimitive -cnotin @('SELECT_ONLY','LOCK_TOUCH') -or [Convert]::FromBase64String($grant.lockQualificationSha256).Length -ne 32){throw 'boot缺真实I3锁资格绑定，不自探针或扩权'}
    if ($grant.format -cne 'SQL_WRITER_BOOT_V1' -or $grant.permission -cne 'NORMAL' -or $grant.deploymentId -cne $entry.deploymentId -or [uint64]$grant.grantedMs -eq 0) { throw 'boot原件用途/部署身份不符' }
    $physical = [Convert]::FromBase64String($grant.physicalAuthoritySha256)
    if ($physical.Length -ne 32 -or [Convert]::ToHexString($physical).ToLowerInvariant() -cne $entry.physicalAuthoritySha256) { throw 'boot物理authority原件不符' }
    if ([Convert]::FromBase64String($grant.writerKey).Length -ne 16 -or [Convert]::FromBase64String($grant.instanceId).Length -ne 16 -or [Convert]::FromBase64String($grant.grantId).Length -ne 16 -or [Convert]::FromBase64String($grant.registrationDigest).Length -ne 32) { throw 'boot身份长度不符' }
    if (-not [IO.Path]::IsPathRooted($entry.consumeDir) -or -not [IO.Directory]::Exists($entry.consumeDir)) { throw 'boot消费目录必须已由受控启动器创建为私有持久目录' }
    if ((Get-Item -LiteralPath $entry.consumeDir).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'boot消费目录不允许重解析点' }
    if(-not [IO.Path]::IsPathRooted($entry.runtimeConfigFile) -or $entry.runtimeConfigSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 $entry.runtimeConfigFile) -cne $entry.runtimeConfigSha256){throw '每boot完整私有配置缺固定绝对路径/SHA，禁止沿用共享配置'}
    $configInfo=Get-Item -LiteralPath $entry.runtimeConfigFile -ErrorAction Stop
    if($configInfo.PSIsContainer -or $configInfo.Attributes -band [IO.FileAttributes]::ReparsePoint -or $configInfo.Length -gt 16777216){throw '每boot私有配置类型/大小不符'}
    if($null -ne $SelectedRuntimeConfigFile){$SelectedRuntimeConfigFile.Value=[IO.Path]::GetFullPath($entry.runtimeConfigFile)}
    $prepared=$null
    if(-not [string]::IsNullOrEmpty([string]$grant.maintenanceRunId)){
        if(-not $PrepareOnlySupported -or -not $entry.Contains('prepareRuntimeFile') -or -not $entry.Contains('prepareRuntimeSha256')){throw '该应用尚无正式prepare-only owner，禁止拿恢复grant直接放root流量'}
        $prepared=Read-MaintenanceJson -Path $entry.prepareRuntimeFile -ExpectedSha256 $entry.prepareRuntimeSha256
        if($prepared.runPhase -cne 'SQL_RELEASED' -or $prepared.runId -cne $grant.maintenanceRunId){throw '恢复runtime不是同run SQL_RELEASED'}
        $roles=@($prepared.evidence.publication.logicalRoles)
        if($roles.Count -eq 0){throw '恢复runtime没有新boot逻辑准入身份'}
        foreach($role in $roles){if($role.instanceId -cne $grant.instanceId -or $role.physicalWriterKey -cne $grant.writerKey){throw '恢复runtime身份与实际新grant不符'}}
        if($null -eq $PreparedRuntimeArguments -or -not $entry.Contains('prepareDrainDirectory') -or -not [IO.Path]::IsPathRooted($entry.prepareDrainDirectory) -or -not [IO.Directory]::Exists($entry.prepareDrainDirectory) -or (Get-Item -LiteralPath $entry.prepareDrainDirectory).Attributes -band [IO.FileAttributes]::ReparsePoint){throw '恢复boot必须实际装配原runtime参数与独立排空目录，不能只看grant后正常启动'}
        $PreparedRuntimeArguments.Value=@('-maintenance-drain-plan',[IO.Path]::GetFullPath($entry.prepareRuntimeFile),'-maintenance-drain-dir',[IO.Path]::GetFullPath($entry.prepareDrainDirectory),'-maintenance-drain-sha256',$entry.prepareRuntimeSha256)
    }
    Assert-PublicationStartAllowed -RepoRoot $RepoRoot -PrepareGrant $grant -PrepareRuntime $prepared
    $environment=[ordered]@{
        MMORPG_SQL_WRITER_GRANT_FILE=[IO.Path]::GetFullPath($entry.grantFile)
        MMORPG_SQL_WRITER_GRANT_SHA256=$entry.grantSha256
        MMORPG_SQL_WRITER_CONSUME_DIR=[IO.Path]::GetFullPath($entry.consumeDir)
        MMORPG_SQL_DEPLOYMENT_ID=$entry.deploymentId
        MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256=$entry.physicalAuthoritySha256
    }
    if($entry.Contains('maintenanceControlDir')){
        if(-not [IO.Path]::IsPathRooted($entry.maintenanceControlDir) -or -not [IO.Directory]::Exists($entry.maintenanceControlDir) -or (Get-Item -LiteralPath $entry.maintenanceControlDir).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'boot在线维护control目录必须预固定私有非重解析路径'}
        $environment.MMORPG_MAINTENANCE_CONTROL_DIR=$entry.maintenanceControlDir
    }
    $allowed=@(switch($ServiceName){'chat'{@('CHAT_AUTHORITY_DSN')};'guild'{@('MMORPG_GUILD_SQL_DSN')};'mail'{@('MMORPG_MAIL_SQL_DSN')};'rank'{@('MMORPG_RANK_SQL_DSN')};'match'{@('MATCH_MYSQL_PASSWORD')};default{@()}})
    $seen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($variable in $entry.runtimeEnvironment){
        Assert-MaintenanceProperties -Value $variable -Required @('name','valueFile','valueSha256')
        if($variable.name -cnotin @($allowed) -or -not $seen.Add($variable.name) -or $variable.valueSha256 -cnotmatch '^[a-f0-9]{64}$'){throw '本boot SQL变量不在对应服务固定白名单或重复'}
        $info=Get-Item -LiteralPath $variable.valueFile -ErrorAction Stop
        if(-not [IO.Path]::IsPathRooted($variable.valueFile) -or $info.PSIsContainer -or $info.Attributes -band [IO.FileAttributes]::ReparsePoint -or $info.Length -lt 1 -or $info.Length -gt 65536){throw '本boot SQL变量必须私有有限原字节文件'}
        $bytes=[IO.File]::ReadAllBytes($variable.valueFile)
        if([Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant() -cne $variable.valueSha256){throw '本boot SQL变量原字节SHA不符'}
        $value=[Text.UTF8Encoding]::new($false,$true).GetString($bytes)
        if($value.IndexOfAny([char[]]@([char]0,[char]10,[char]13)) -ge 0){throw '本boot SQL变量含非法文本边界'}
        $environment[$variable.name]=$value
    }
    if($seen.Count -ne @($allowed).Count){throw '本boot缺对应服务必须的SQL环境变量，禁止父进程共享值回退'}
    return $environment
}

function Enter-SqlWriterBootEnvironment {
    param([Collections.IDictionary]$Environment)
    $saved = @{}
    foreach($key in @('CHAT_AUTHORITY_DSN','MMORPG_GUILD_SQL_DSN','MMORPG_MAIL_SQL_DSN','MMORPG_RANK_SQL_DSN','MATCH_MYSQL_PASSWORD','MMORPG_MAINTENANCE_CONTROL_DIR')){$saved[$key]=[Environment]::GetEnvironmentVariable($key,'Process');[Environment]::SetEnvironmentVariable($key,$null,'Process')}
    foreach ($key in $Environment.Keys) { if(-not $saved.ContainsKey($key)){$saved[$key]=[Environment]::GetEnvironmentVariable($key,'Process')}; [Environment]::SetEnvironmentVariable($key,[string]$Environment[$key],'Process') }
    # 常驻服务永不继承控制身份，finally由调用方恢复父环境。
    foreach ($key in @('MMORPG_SQL_REGISTRAR_DSN','MMORPG_SQL_MAINTENANCE_DSN','MMORPG_SQL_SCHEMA_DSN','MMORPG_SQL_OPERATOR_PRINCIPAL','MMORPG_SQL_SCHEMA_PRINCIPAL')) { $saved[$key]=[Environment]::GetEnvironmentVariable($key,'Process'); [Environment]::SetEnvironmentVariable($key,$null,'Process') }
    return $saved
}

function Restore-SqlWriterBootEnvironment {
    param([Collections.IDictionary]$Saved)
    if ($null -eq $Saved) { return }
    foreach ($key in $Saved.Keys) { [Environment]::SetEnvironmentVariable($key,$Saved[$key],'Process') }
}

function Assert-MaintenanceProcessIdentity {
    param([Parameter(Mandatory)][Collections.IDictionary]$Expected)
    foreach ($key in @('pid','startTimeUtcTicks','executablePath','executableSha256')) { if (-not $Expected.Contains($key)) { throw "停止计划缺少进程身份$key" } }
    $process = Get-Process -Id ([int]$Expected.pid) -ErrorAction Stop
    if ($process.StartTime.ToUniversalTime().Ticks -ne [long]$Expected.startTimeUtcTicks -or
        [IO.Path]::GetFullPath($process.Path) -cne [IO.Path]::GetFullPath($Expected.executablePath) -or
        (Get-MaintenanceSha256 -Path $process.Path) -cne $Expected.executableSha256) { throw 'PID已复用或可执行来源变化，拒绝停止及闭合签认' }
    $null=$process.Handle
    return $process
}

function Get-VerifiedIssuerClosure {
    param([Parameter(Mandatory)][Collections.IDictionary]$Issuer,[Parameter(Mandatory)][Collections.IDictionary]$Plan,[ref]$ReportSha256)
    $actualReportSha=''
    $expectedReportSha=if($Issuer.Contains('applicationReportSha256')){[string]$Issuer.applicationReportSha256}else{''}
    $record = Read-MaintenanceJson -Path $Issuer.applicationReportPath -ExpectedSha256 $expectedReportSha -ContentSha256 ([ref]$actualReportSha)
    # 正式IssuerClosure protojson，不能用外部passed布尔、进程消失或日志替代frontier。
    Assert-MaintenanceProperties -Value $record -Required @('issuerId','instanceId','physicalWriterKey','logicalRole','permanentRevivalFenceId','acceptingClosedMs','acceptedSettledMs','acceptedFrontierDigest','unresolvedPublications','sourceEvidence')
    foreach ($key in @('issuerId','instanceId','physicalWriterKey','logicalRole','permanentRevivalFenceId')) {
        if ([string]$record[$key] -cne [string]$Issuer[$key]) { throw "闭合原件身份不符：$key" }
    }
    if ([uint64]$record.acceptingClosedMs -eq 0 -or [uint64]$record.acceptedSettledMs -lt [uint64]$record.acceptingClosedMs -or [uint64]$record.unresolvedPublications -ne 0 -or [Convert]::FromBase64String($record.acceptedFrontierDigest).Length -ne 32) { throw '发布尚未确定settled，保留永久关闭HOLD' }
    if (@($record.sourceEvidence).Count -eq 0 -or @($record.sourceEvidence).Count -gt 128) { throw '闭合原件缺来源或超过既有预算' }
    foreach ($pin in $record.sourceEvidence) {
        $expectedSha = [Convert]::FromBase64String($pin.sha256)
        if ($expectedSha.Length -ne 32 -or -not [IO.Path]::IsPathRooted($pin.absolutePath) -or (Get-Item -LiteralPath $pin.absolutePath).Length -gt 16777216 -or (Get-MaintenanceSha256 -Path $pin.absolutePath) -cne [Convert]::ToHexString($expectedSha).ToLowerInvariant()) { throw '闭合来源原件变化或超既有预算' }
    }
    # 初始目录固定受控报告路径/issuer/唯一围栏；动态报告完成后才捕获其精确SHA，不能预知时间戳后自签passed。
    # 若调用者提供已捕获SHA，则必须同本次实际读取bytes；工具owner仍须核完整app原件语义。
    if($null -ne $ReportSha256){$ReportSha256.Value=$actualReportSha}
    return $record
}

function Stop-LocalPublisherAndReport {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][Collections.IDictionary]$Issuer,[Parameter(Mandatory)][string]$RepoRoot,[Parameter(Mandatory)][scriptblock]$Signal)
    $fence = Set-PermanentPublicationFence -Plan $Plan -RepoRoot $RepoRoot
    $process = Assert-MaintenanceProcessIdentity -Expected $Issuer.process
    if ($Issuer.logicalRole -cin @('root','SOURCE') -and $Issuer.Contains('sharesRelay') -and [bool]$Issuer.sharesRelay) { throw '同boot root+relay必须由逻辑CloseAdmission owner先关闭，P1不能停止/退休物理进程' }
    $deadline = [Diagnostics.Stopwatch]::StartNew()
    $seconds = [double]$Plan.stopTimeoutSeconds
    if ($seconds -le 0 -or $seconds -gt 60) { throw '停止预算不在既有六十秒窗内' }
    if (-not (& $Signal $process.Id)) { throw '优雅停止信号未确定投递，HOLD；禁止force-kill冒排空' }
    while (-not $process.WaitForExit(100)) { if ($deadline.Elapsed.TotalSeconds -ge $seconds) { throw '优雅停止超时，进程/PID/队列原件全部保留，HOLD' } }
    $process.Refresh()
    if ($process.ExitCode -ne 0) { throw '应用非零退出，不能签闭合' }
    $reportSha=''
    $closure = Get-VerifiedIssuerClosure -Issuer $Issuer -Plan $Plan -ReportSha256 ([ref]$reportSha)
    return [ordered]@{issuerClosure=$closure;applicationReportPath=$Issuer.applicationReportPath;applicationReportSha256=$reportSha;revivalFencePath=$fence;revivalFenceSha256=(Get-MaintenanceSha256 -Path $fence);processId=$Issuer.process.pid;startTimeUtcTicks=$Issuer.process.startTimeUtcTicks;exitCode=$process.ExitCode}
}

function Invoke-LocalStopAndCollect {
    param([Parameter(Mandatory)][string]$PlanFile,[Parameter(Mandatory)][string]$ReportFile,[Parameter(Mandatory)][string]$Family,[Parameter(Mandatory)][string]$RepoRoot,[Parameter(Mandatory)]$Tracked,[Parameter(Mandatory)][scriptblock]$Signal)
    $plan=Read-MaintenanceJson -Path $PlanFile
    if ($plan.format -cne 'SQL_MAINTENANCE_DRAIN_LOCAL_V1' -or @($plan.issuers).Count -eq 0 -or @($plan.issuers).Count -gt 1024) { throw '本地维护计划格式或issuer预算不符' }
    $targets=@($plan.issuers | Where-Object { $_.family -ceq $Family })
    if ($targets.Count -eq 0) { throw '停止计划没有该owner的固定issuer集合' }
    $seen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($issuer in $targets) {
        if (-not $seen.Add([string]$issuer.instanceKey) -or $issuer.instanceKey -cnotmatch '^[A-Za-z0-9_-]{1,96}$') { throw '停止实例重复或身份非法' }
        if ($Tracked.PSObject.Properties.Name -notcontains $issuer.instanceKey) { throw '固定issuer不在真实launcher PID原件中，不能按gone猜闭合' }
        $entry=$Tracked.($issuer.instanceKey)
        $actualPid=if($entry -is [int] -or $entry -is [long]){[int]$entry}else{[int]$entry.Pid}
        if ($actualPid -ne [int]$issuer.process.pid) { throw '固定issuer与真实PID原件不符' }
        if ($entry -isnot [int] -and $entry -isnot [long] -and $entry.PSObject.Properties.Name -contains 'AllowChildProcess' -and $entry.AllowChildProcess) { throw '严格停止只接受具名原生业务exe，不接受go run父进程替代子进程frontier' }
    }
    $null=Set-PermanentPublicationFence -Plan $plan -RepoRoot $RepoRoot
    $clock=[Diagnostics.Stopwatch]::StartNew()
    $records=[Collections.Generic.List[object]]::new()
    foreach ($issuer in $targets) {
        $left=[Math]::Min([double]$plan.stopTimeoutSeconds,60.0)-$clock.Elapsed.TotalSeconds
        if($left -le 0){throw '本批停止耗尽既有全局预算，永久关闭保留HOLD'}
        $bounded=@{};foreach($key in $plan.Keys){$bounded[$key]=$plan[$key]};$bounded.stopTimeoutSeconds=$left
        $record=Stop-LocalPublisherAndReport -Plan $bounded -Issuer $issuer -RepoRoot $RepoRoot -Signal $Signal
        $records.Add($record)
        $onePath=$ReportFile+'.'+$issuer.instanceKey+'.issuer.json'
        $null=Write-MaintenanceImmutableJson -Path $onePath -Value $record
    }
    # 此产物只是具名进程/永久app原件集合；必须再由authority-phase v2核验，不能直接当NON_SQL_CLOSED。
    $null=Write-MaintenanceImmutableJson -Path $ReportFile -Value ([ordered]@{format='SQL_STOP_COLLECTION_V1';operationId=$plan.operationId;manifestSha256=$plan.manifestSha256;family=$Family;records=$records.ToArray()})
}

function Assert-MaintenanceControlDirectory {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan)
    if([string]::IsNullOrWhiteSpace([string]$Plan.controlDirectory) -or [string]::IsNullOrWhiteSpace([string]$Plan.drainDirectory) -or -not [IO.Path]::IsPathRooted($Plan.controlDirectory) -or -not [IO.Path]::IsPathRooted($Plan.drainDirectory)){throw '动作与激活必须固定同一绝对controlDirectory/drainDirectory'}
    $control=[IO.Path]::GetFullPath($Plan.controlDirectory).TrimEnd([IO.Path]::DirectorySeparatorChar,[IO.Path]::AltDirectorySeparatorChar)
    $drain=[IO.Path]::GetFullPath($Plan.drainDirectory).TrimEnd([IO.Path]::DirectorySeparatorChar,[IO.Path]::AltDirectorySeparatorChar)
    $comparison=if($IsWindows){[StringComparison]::OrdinalIgnoreCase}else{[StringComparison]::Ordinal}
    if(-not $control.Equals($drain,$comparison) -or -not [IO.Directory]::Exists($control) -or ((Get-Item -LiteralPath $control).Attributes -band [IO.FileAttributes]::ReparsePoint)){throw '动作与激活必须指向同一既有非重解析控制目录'}
    return $control
}

function Request-MaintenanceRoleClosure {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][ValidateSet('SOURCE','RELAY')][string]$Role,[Parameter(Mandatory)][string]$Output,[Parameter(Mandatory)][string]$RepoRoot)
    $fence=Set-PermanentPublicationFence -Plan $Plan -RepoRoot $RepoRoot
    $directory=Assert-MaintenanceControlDirectory -Plan $Plan
    if((Get-MaintenanceSha256 -Path $Plan.planFile) -cne $Plan.runtimePlanSha256){throw 'logical close runtime Receipt原件变化'}
    $lower=$Role.ToLowerInvariant();$request=Join-Path $directory ('close-'+$lower+'.request');$report=Join-Path $directory ($lower+'.issuer.json')
    # 只是已固定Session上下文中的动作信号，不另造JSON状态/授权。Session必须自行封准入并等真实accepted集合settled。
    Write-MaintenanceImmutableBytes -Path $request -Bytes ([Text.Encoding]::ASCII.GetBytes('CLOSE_'+$Role+"`n"))
    $clock=[Diagnostics.Stopwatch]::StartNew()
    while(-not [IO.File]::Exists($report)){
        if($clock.ElapsedMilliseconds -ge 60000){throw 'logical close没有确定app原件，保围栏HOLD；不停止共用relay进程'}
        Start-Sleep -Milliseconds 50
    }
    $issuer=@{};foreach($key in $Plan.issuer.Keys){$issuer[$key]=$Plan.issuer[$key]};$issuer.applicationReportPath=$report
    if($issuer.logicalRole -cne $Role){throw 'logical close角色与P0固定身份不符'}
    $sha='';$closure=Get-VerifiedIssuerClosure -Issuer $issuer -Plan $Plan -ReportSha256 ([ref]$sha)
    $null=Write-MaintenanceImmutableJson -Path $Output -Value ([ordered]@{issuerClosure=$closure;applicationReportPath=$report;applicationReportSha256=$sha;revivalFencePath=$fence;revivalFenceSha256=(Get-MaintenanceSha256 -Path $fence)})
}

function ConvertTo-AuthoritySqlIdentifier {
    param([string]$Value)
    if ($Value -cnotmatch '^[A-Za-z][A-Za-z0-9_]{0,63}$') { throw 'SQL标识不在固定manifest字母数字白名单' }
    return ('`' + $Value + '`')
}
function ConvertTo-AuthoritySqlAccount {
    param([string]$Value)
    if ($Value -cnotmatch '^(?<user>[a-z][a-z0-9_]{0,31})@(?<host>[A-Za-z0-9_.%:-]{1,253})$') { throw 'SQL主体格式不符' }
    return ("'"+$Matches.user+"'@'"+$Matches.host+"'")
}

function ConvertTo-AuthorityRoleSql {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$TemplateRoot)
    Assert-MaintenanceProperties -Value $Plan -Required @('authoritySchema','runtimePrincipal','registrarPrincipal','maintenancePrincipal','schemaPrincipal','registrationRequestFile','registrationRequestSha256','lockQualificationFile','tables','schemaDatabases')
    $principals=@($Plan.runtimePrincipal,$Plan.registrarPrincipal,$Plan.maintenancePrincipal,$Plan.schemaPrincipal)
    if (($principals | Select-Object -Unique).Count -ne 4) { throw 'runtime/registrar/maintenance/schema必须不同principal' }
    $values = @{
        AUTHORITY_SCHEMA=(ConvertTo-AuthoritySqlIdentifier $Plan.authoritySchema)
        RUNTIME_ACCOUNT=(ConvertTo-AuthoritySqlAccount $Plan.runtimePrincipal)
        REGISTRAR_ACCOUNT=(ConvertTo-AuthoritySqlAccount $Plan.registrarPrincipal)
        MAINTENANCE_ACCOUNT=(ConvertTo-AuthoritySqlAccount $Plan.maintenancePrincipal)
        SCHEMA_ACCOUNT=(ConvertTo-AuthoritySqlAccount $Plan.schemaPrincipal)
        LOCK_TOUCH_GRANT=''
        SCHEMA_DDL_GRANTS=''
        SCHEMA_METADATA_GRANTS=''
    }
    $registration=Read-MaintenanceJson -Path $Plan.registrationRequestFile -ExpectedSha256 $Plan.registrationRequestSha256
    if($registration.lockPrimitive -cnotin @('SELECT_ONLY','LOCK_TOUCH') -or -not $registration.Contains('lockQualificationSha256')){throw '正式REGISTER原件缺已资格锁原语'}
    $qualification=[Convert]::FromBase64String($registration.lockQualificationSha256)
    if($qualification.Length -ne 32 -or (Get-MaintenanceSha256 -Path $Plan.lockQualificationFile) -cne [Convert]::ToHexString($qualification).ToLowerInvariant()){throw '锁资格不是已固定I3原件SHA'}
    if([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($registration.writer.sqlPrincipal)) -cne $Plan.runtimePrincipal){throw '角色主体与正式REGISTER原件不一致'}
    if ($registration.lockPrimitive -ceq 'LOCK_TOUCH') { $values.LOCK_TOUCH_GRANT="GRANT UPDATE (lock_touch) ON $($values.AUTHORITY_SCHEMA).``writer_slot`` TO $($values.RUNTIME_ACCOUNT);" }
    $business = [Collections.Generic.List[string]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($table in $Plan.tables) {
        Assert-MaintenanceProperties -Value $table -Required @('schema','table','operations')
        $schema = ConvertTo-AuthoritySqlIdentifier $table.schema; $name = ConvertTo-AuthoritySqlIdentifier $table.table
        if (-not $seen.Add($table.schema+'.'+$table.table)) { throw '业务GRANT重复或触及权限真值表' }
        $ops = @($table.operations)
        if ($ops.Count -eq 0 -or ($ops | Select-Object -Unique).Count -ne $ops.Count) { throw '业务GRANT操作空或重复' }
        foreach ($op in $ops) { if ($op -cnotin @('SELECT','INSERT','UPDATE','DELETE')) { throw 'runtime禁止DDL/GRANT/注册能力' } }
        if ($table.table -iin @('route_meta','writer_registry','writer_slot','writer_lifecycle_receipt','sql_writer_drain_run','sql_writer_freeze_ticket','sql_writer_maintenance_receipt')) { throw '业务GRANT重复或触及权限真值表' }
        $business.Add("GRANT $($ops -join ',') ON $schema.$name TO $($values.RUNTIME_ACCOUNT);")
    }
    $values.BUSINESS_TABLE_GRANTS=$business -join "`n"
    $schemaGrants=[Collections.Generic.List[string]]::new()
    $metadataGrants=[Collections.Generic.List[string]]::new()
    $schemaSeen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($database in $Plan.schemaDatabases){
        $identifier=ConvertTo-AuthoritySqlIdentifier $database
        if(-not $schemaSeen.Add($database)){throw 'fresh schema授权库重复'}
        $schemaGrants.Add("GRANT SELECT,CREATE,ALTER,INDEX ON $identifier.* TO $($values.SCHEMA_ACCOUNT);")
        $metadataGrants.Add("GRANT CREATE,INSERT,UPDATE ON $identifier.``schema_migrations`` TO $($values.SCHEMA_ACCOUNT);")
    }
    if($schemaGrants.Count -eq 0){throw 'fresh prestart schema必须显式具名库清单，不能由业务表暗推'}
    if(-not $schemaSeen.Contains([string]$Plan.authoritySchema)){throw 'fresh schema清单必须包含同authority，不能从业务表旁路建控制表'}
    $metadataGrants.Add("GRANT CREATE,INSERT ON $($values.AUTHORITY_SCHEMA).``route_meta`` TO $($values.SCHEMA_ACCOUNT);")
    if($schemaSeen.Contains('mmorpg_guild')){$metadataGrants.Add("GRANT INSERT ON ``mmorpg_guild``.``guild_player_state`` TO $($values.SCHEMA_ACCOUNT);")}
    $values.SCHEMA_DDL_GRANTS=$schemaGrants -join "`n"
    $values.SCHEMA_METADATA_GRANTS=$metadataGrants -join "`n"
    $runtime=[IO.File]::ReadAllText((Join-Path $TemplateRoot 'authority_runtime_role.sql'))
    $operator=[IO.File]::ReadAllText((Join-Path $TemplateRoot 'authority_operator_role.sql'))
    foreach ($key in $values.Keys) { $runtime=$runtime.Replace('{{'+$key+'}}',[string]$values[$key]);$operator=$operator.Replace('{{'+$key+'}}',[string]$values[$key]) }
    if (($runtime+$operator) -match '\{\{[A-Z_]+\}\}') { throw '权限叶有未绑定占位' }
    return [ordered]@{runtime=$runtime;operator=$operator}
}


function Request-MaintenanceManualBaseline {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$Output)
    $directory=Assert-MaintenanceControlDirectory -Plan $Plan
    if($Plan.operationId -cnotmatch '^[a-f0-9]{32}$' -or $Plan.issuer.logicalRole -cne 'SOURCE' -or [Convert]::FromBase64String($Plan.issuer.issuerId).Length -ne 16 -or [Convert]::FromBase64String($Plan.issuer.instanceId).Length -ne 16){throw 'manual P0必须绑定实际SOURCE旧boot与既定run'}
    if(@($Plan.issuer.sourceEvidence).Count -eq 0){throw 'manual P0缺固定SOURCE配置/身份原件'}
    $ownDirectory=$false
    foreach($pin in $Plan.issuer.sourceEvidence){
        if([IO.Path]::GetFullPath([IO.Path]::GetDirectoryName($pin.absolutePath)) -ceq $directory){$ownDirectory=$true}
        if((Get-MaintenanceSha256 $pin.absolutePath) -cne [Convert]::ToHexString([Convert]::FromBase64String($pin.sha256)).ToLowerInvariant()){throw 'manual P0固定原件SHA不符'}
    }
    if(-not $ownDirectory){throw 'manual P0缺同boot私有目录身份原件'}
    # 正式ManualConsumerEvidence意图只绑定run/实际SOURCE/boot；offset与物理目录只能由consumer owner填写。
    $intent=[ordered]@{operationId=[Convert]::ToBase64String([Convert]::FromHexString($Plan.operationId));assignments=@([ordered]@{issuerId=$Plan.issuer.issuerId;instanceId=$Plan.issuer.instanceId});sourcePins=@($Plan.issuer.sourceEvidence)}
    $bytes=[Text.UTF8Encoding]::new($false).GetBytes(($intent | ConvertTo-Json -Depth 80 -Compress))
    Write-MaintenanceImmutableBytes -Path (Join-Path $directory 'manual-baseline.request.json') -Bytes $bytes
    $reportPath=Join-Path $directory 'manual-baseline.report.json';$clock=[Diagnostics.Stopwatch]::StartNew()
    while(-not [IO.File]::Exists($reportPath)){if($clock.ElapsedMilliseconds -ge 60000){throw 'manual P0未取得真实consumer原件，禁止拍Plan'};Start-Sleep -Milliseconds 50}
    $sha='';$report=Read-MaintenanceJson -Path $reportPath -ContentSha256 ([ref]$sha)
    if($report.operationId -cne $intent.operationId -or -not [string]::IsNullOrEmpty([string]$report.manifestDigest) -or @($report.assignments).Count -eq 0){throw 'manual P0原件身份或自引用摘要不符'}
    foreach($row in $report.assignments){if($row.issuerId -cne $Plan.issuer.issuerId -or $row.instanceId -cne $Plan.issuer.instanceId){throw 'manual P0回报其他boot'}}
    # 调用者将原assignments及本pin纳入唯一Plan；collector只核broker目录，不重写native position。
    $null=Write-MaintenanceImmutableJson -Path $Output -Value ([ordered]@{assignments=@($report.assignments);sourcePin=[ordered]@{absolutePath=[IO.Path]::GetFullPath($reportPath);sha256=[Convert]::ToBase64String([Convert]::FromHexString($sha))}})
}

function New-MaintenanceManualSamplingRequest {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$Output)
    $runtime=Read-MaintenanceJson -Path $Plan.planFile -ExpectedSha256 $Plan.runtimePlanSha256
    $run=[Convert]::ToBase64String([Convert]::FromHexString($Plan.operationId));$manifest=[Convert]::ToBase64String([Convert]::FromHexString($Plan.manifestSha256))
    if($runtime.runId -cne $run -or $runtime.evidence.operationId -cne $run -or $runtime.evidence.manifestDigest -cne $manifest -or [string]::IsNullOrEmpty([string]$runtime.planProto)){throw 'manual采样必须同run原Plan/Manifest'}
    $pins=[Collections.Generic.List[object]]::new();$seen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($issuer in $Plan.issuers){
        if($issuer.logicalRole -cne 'SOURCE'){continue}
        $directories=@($issuer.sourceEvidence | ForEach-Object {[IO.Path]::GetFullPath([IO.Path]::GetDirectoryName($_.absolutePath))} | Select-Object -Unique)
        foreach($directory in $directories){
            $file=Join-Path $directory 'manual-closure.report.json'
            if(-not [IO.File]::Exists($file) -or -not $seen.Add([IO.Path]::GetFullPath($file))){continue}
            $sha='';$report=Read-MaintenanceJson -Path $file -ContentSha256 ([ref]$sha)
            if($report.operationId -cne $run -or $report.manifestDigest -cne $manifest -or -not $report.acceptingClosed -or -not $report.callbacksJoined -or [uint64]$report.unresolvedPublications -ne 0 -or [uint64]$report.accepted -ne [uint64]$report.settled -or @($report.assignments).Count -eq 0){throw 'manual P3没有真实停止/join/known终局'}
            foreach($row in $report.assignments){if($row.issuerId -cne $issuer.issuerId -or $row.instanceId -cne $issuer.instanceId -or [long]$row.position -ne [long]$row.exclusiveEnd){throw 'manual P3不是原SOURCE旧boot或未消费到固定尾部'}}
            $pins.Add([ordered]@{absolutePath=[IO.Path]::GetFullPath($file);sha256=[Convert]::ToBase64String([Convert]::FromHexString($sha))})
        }
    }
    if($pins.Count -eq 0){throw 'manual采样缺P3固定报告原件'}
    # 这是正式Receipt的采样输入，不是NON_SQL_CLOSED收据；CLI继续核全部P0等集和broker实际尾部。
    $sampling=[ordered]@{runId=$runtime.runId;planProto=$runtime.planProto;evidence=[ordered]@{operationId=$runtime.evidence.operationId;manifestDigest=$runtime.evidence.manifestDigest;nonSqlClosure=[ordered]@{sourceEvidence=$pins.ToArray()}}}
    $sha=Write-MaintenanceImmutableJson -Path $Output -Value $sampling
    return [ordered]@{requestFile=[IO.Path]::GetFullPath($Output);requestSha256=$sha}
}

function Invoke-AuthorityPhaseCli {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$Phase,[Parameter(Mandatory)][string]$Output)
    $exe=[IO.Path]::GetFullPath($Plan.authorityPhaseExecutable)
    if ((Get-MaintenanceSha256 -Path $exe) -cne $Plan.authorityPhaseExecutableSha256) { throw 'authority-phase工具来源不符' }
    $runtimePlan=[IO.Path]::GetFullPath($Plan.planFile)
    if((Get-MaintenanceSha256 -Path $runtimePlan) -cne $Plan.runtimePlanSha256){throw '正式runtime Receipt原bytes SHA不符'}
    $drainDir=[IO.Path]::GetFullPath($Plan.drainDirectory)
    if(-not [IO.Directory]::Exists($drainDir) -or (Get-Item -LiteralPath $drainDir).Attributes -band [IO.FileAttributes]::ReparsePoint){throw '缺私有drain原件目录或路径重解析'}
    # 固定现有maintenance_phase子模式，无eval/任意命令/新gRPC。工具owner核全部v2原件，不读passed=true。
    if($Phase -cnotin @('collect-kafka-catalog','collect-kafka-baseline','collect-kafka','seal-request','advance','grant-ticket','revoke-ticket','freeze','release','read')){throw '未知正式authority子动作，不透传任意参数'}
    $arguments=@('-authority-phase',$Phase,'-maintenance-drain-plan',$runtimePlan,'-maintenance-drain-dir',$drainDir,'-maintenance-drain-sha256',$Plan.runtimePlanSha256,'-authority-phase-output',$Output,'-dsn-env','MMORPG_SQL_MAINTENANCE_DSN')
    $kafkaPhase=$Phase -cin @('collect-kafka-catalog','collect-kafka-baseline','collect-kafka')
    if($kafkaPhase){$arguments+=@('-authority-kafka-brokers-env','MMORPG_MAINTENANCE_KAFKA_BROKERS')}
    if(-not $kafkaPhase -or ($Phase -ceq 'collect-kafka' -and $Plan.Contains('authorityRequestFile'))){
        if(-not [IO.Path]::IsPathRooted($Plan.authorityRequestFile) -or $Plan.authorityRequestSha256 -cnotmatch '^[a-f0-9]{64}$'){throw '阶段动作必须显式完整正式Request及原bytes SHA'}
        $null=Read-MaintenanceJson -Path $Plan.authorityRequestFile -ExpectedSha256 $Plan.authorityRequestSha256
        $arguments+=@('-authority-request',$Plan.authorityRequestFile,'-authority-request-sha256',$Plan.authorityRequestSha256)
        if($Plan.Contains('authorityTicketFile')){
            if(-not [IO.Path]::IsPathRooted($Plan.authorityTicketFile) -or $Plan.authorityTicketSha256 -cnotmatch '^[a-f0-9]{64}$'){throw '票据原件及SHA必须完整绑定'}
            $null=Read-MaintenanceJson -Path $Plan.authorityTicketFile -ExpectedSha256 $Plan.authorityTicketSha256
            $arguments+=@('-authority-ticket',$Plan.authorityTicketFile,'-authority-ticket-sha256',$Plan.authorityTicketSha256)
        }
        if($Phase -cin @('grant-ticket','revoke-ticket','freeze') -and -not $Plan.Contains('authorityTicketFile')){throw 'grant/revoke/freeze缺原正式ticket，禁止脚本自铸票据'}
    }
    & $exe @arguments
    if ($LASTEXITCODE -ne 0) { throw "authority-phase ${Phase}失败，全部关闭保持HOLD" }
}

function Assert-MaintenanceRedisDeadline {
    param($Session)
    $remaining=60000-[int]$Session.Clock.ElapsedMilliseconds
    if($remaining -le 0){throw 'Redis静态闭合取样耗尽既有60秒预算，保持HOLD'}
    $Session.Client.ReceiveTimeout=[Math]::Max(1,[Math]::Min(1000,$remaining))
    $Session.Client.SendTimeout=$Session.Client.ReceiveTimeout
}

function Read-MaintenanceRedisLine {
    param($Session)
    $line=[Collections.Generic.List[byte]]::new()
    for(;;){
        Assert-MaintenanceRedisDeadline $Session
        $value=$Session.Stream.ReadByte()
        if($value -lt 0){throw 'Redis原件读取意外断线'}
        if($value -eq 13){if($Session.Stream.ReadByte() -ne 10){throw 'Redis原件RESP行终止非法'};break}
        if($line.Count -ge 4096){throw 'Redis原件RESP行超过预算'}
        $line.Add([byte]$value)
    }
    return [Text.Encoding]::ASCII.GetString($line.ToArray())
}

function Read-MaintenanceRedisResp {
    param($Session,[int]$Depth=0)
    if($Depth -gt 12){throw 'Redis原件RESP嵌套超过预算'}
    Assert-MaintenanceRedisDeadline $Session
    $prefix=$Session.Stream.ReadByte()
    if($prefix -lt 0){throw 'Redis原件读取断线'}
    $line=Read-MaintenanceRedisLine $Session
    switch($prefix){
        43 {return [pscustomobject]@{Kind='text';Text=$line}}
        45 {throw 'Redis只读取样命令失败，不能解释为零'}
        58 {return [pscustomobject]@{Kind='integer';Number=[long]::Parse($line,[Globalization.CultureInfo]::InvariantCulture)}}
        36 {
            $length=[int]::Parse($line,[Globalization.CultureInfo]::InvariantCulture)
            if($length -eq -1){return [pscustomobject]@{Kind='nil'}}
            if($length -lt 0 -or $length -gt 16777216){throw 'Redis原件bulk超过既有16MiB预算'}
            $data=[byte[]]::new($length);$offset=0
            while($offset -lt $length){Assert-MaintenanceRedisDeadline $Session;$read=$Session.Stream.Read($data,$offset,$length-$offset);if($read -le 0){throw 'Redis原件bulk截断'};$offset+=$read}
            if($Session.Stream.ReadByte() -ne 13 -or $Session.Stream.ReadByte() -ne 10){throw 'Redis原件bulk终止非法'}
            return [pscustomobject]@{Kind='bulk';Data=$data}
        }
        42 {
            $count=[int]::Parse($line,[Globalization.CultureInfo]::InvariantCulture)
            if($count -eq -1){return [pscustomobject]@{Kind='nil'}}
            if($count -lt 0 -or $count -gt 16384){throw 'Redis原件array超过既有16K预算'}
            $items=[Collections.Generic.List[object]]::new()
            for($i=0;$i -lt $count;$i++){$items.Add((Read-MaintenanceRedisResp -Session $Session -Depth ($Depth+1)))}
            return [pscustomobject]@{Kind='array';Items=$items}
        }
        default {throw 'Redis原件RESP类型非法'}
    }
}

function Invoke-MaintenanceRedisRead {
    param($Session,[Parameter(Mandatory)][string[]]$Arguments)
    if($Arguments.Count -eq 0 -or $Arguments[0] -cnotin @('AUTH','SELECT','PING','INFO','SCAN','TYPE','GET','PTTL','LLEN','ZCARD','HLEN','LRANGE','ZRANGE','HGETALL','DUMP','CLUSTER')){throw '维护collector拒绝非白名单只读命令'}
    if($Arguments[0] -ceq 'CLUSTER' -and ($Arguments.Count -ne 2 -or $Arguments[1] -cne 'NODES')){throw '维护collector只读CLUSTER NODES'}
    return (Send-MaintenanceRedisCommand -Session $Session -Arguments $Arguments)
}

function Send-MaintenanceRedisCommand {
    param($Session,[Parameter(Mandatory)][string[]]$Arguments)
    Assert-MaintenanceRedisDeadline $Session
    $buffer=[IO.MemoryStream]::new()
    try{
        $head=[Text.Encoding]::ASCII.GetBytes('*'+$Arguments.Count+"`r`n");$buffer.Write($head,0,$head.Length)
        foreach($argument in $Arguments){
            $bytes=[Text.UTF8Encoding]::new($false,$true).GetBytes($argument)
            $prefix=[Text.Encoding]::ASCII.GetBytes('$'+$bytes.Length+"`r`n")
            $buffer.Write($prefix,0,$prefix.Length);$buffer.Write($bytes,0,$bytes.Length);$buffer.WriteByte(13);$buffer.WriteByte(10)
        }
        $wire=$buffer.ToArray();$Session.Stream.Write($wire,0,$wire.Length);$Session.Stream.Flush()
    }finally{$buffer.Dispose()}
    return (Read-MaintenanceRedisResp -Session $Session)
}

function Get-MaintenanceRedisText {
    param($Reply)
    if($Reply.Kind -ceq 'text'){return [string]$Reply.Text}
    if($Reply.Kind -ceq 'bulk'){return [Text.UTF8Encoding]::new($false,$true).GetString($Reply.Data)}
    throw 'Redis原件预期文本类型不符'
}

function Get-MaintenanceRedisDb {
    param([Collections.IDictionary]$Target)
    if(-not $Target.Contains('db') -or $null -eq $Target.db){throw 'Redis目标必须明确整数DB'}
    $text=[string]$Target.db;$db=[int]0
    if($text -cnotmatch '^(0|[1-9][0-9]*)$' -or -not [int]::TryParse($text,[Globalization.NumberStyles]::None,[Globalization.CultureInfo]::InvariantCulture,[ref]$db)){throw 'Redis DB必须为非负int32整数，拒绝小数/范围外值'}
    return $db
}

function Open-MaintenanceRedis {
    param([Collections.IDictionary]$Target,[Diagnostics.Stopwatch]$Clock)
    # 真实有限本地collector，只接受回环公布端口；禁止从地址字符串获取凭据或隐式SSH/docker exec。
    if($Target.address -cnotmatch '^(?<host>127\.0\.0\.1|localhost|\[::1\]):(?<port>[1-9][0-9]{0,4})$'){throw '本地Redis collector必须明确回环端口'}
    $server=$Matches.host.Trim('[',']');$port=[int]$Matches.port;$db=Get-MaintenanceRedisDb -Target $Target
    if($port -gt 65535){throw 'Redis目标端口非法'}
    $client=[Net.Sockets.TcpClient]::new()
    try{
        $left=[Math]::Min(1000,60000-[int]$Clock.ElapsedMilliseconds)
        if($left -le 0 -or -not $client.ConnectAsync($server,$port).Wait($left)){throw 'Redis目标连接未在预算内确定'}
        $session=[pscustomobject]@{Client=$client;Stream=$client.GetStream();Clock=$Clock}
        if($Target.Contains('passwordEnv')){
            if($Target.passwordEnv -cnotmatch '^[A-Z][A-Z0-9_]{0,127}$'){throw 'Redis凭据环境变量名非法'}
            $secret=[Environment]::GetEnvironmentVariable($Target.passwordEnv,'Process')
            if([string]::IsNullOrEmpty($secret)){throw 'Redis collector缺凭据'}
            $null=Invoke-MaintenanceRedisRead -Session $session -Arguments @('AUTH',$secret)
            $secret=$null
        }
        $null=Invoke-MaintenanceRedisRead -Session $session -Arguments @('SELECT',$db.ToString([Globalization.CultureInfo]::InvariantCulture))
        return $session
    }catch{$client.Dispose();throw}
}

function Get-MaintenanceRedisRunId {
    param($Session)
    $info=Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('INFO','server'))
    $match=[regex]::Match($info,'(?m)^run_id:([a-f0-9]{40})\r?$')
    if(-not $match.Success){throw 'Redis缺物理server run_id原件'}
    return [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes($match.Groups[1].Value))
}


# Redis Cluster 目标(docs/design/redis-player-store-cluster.md §5.7,核对规则在 go/db/internal/maintenancephase/redis_cluster.go):
# 每个主节点是一个物理目标,计划要把它们列全。SCAN 与读键只看本节点、不跟随重定向,所以经 kubectl port-forward 取样也成立。
# CLUSTER NODES 证明取样首尾拓扑不变、本节点确实是持有 slot 的主;漏主节点由 Go 侧把全部目标放在一起判。
function ConvertFrom-MaintenanceClusterNodes {
    param([Parameter(Mandatory)][string]$Text)
    $entries=[Collections.Generic.List[string]]::new();$selfId='';$selfRanges=@();$selfIsMaster=$false;$total=0
    foreach($line in ($Text -split "`r?`n")){
        $fields=@($line -split '\s+' | Where-Object {$_ -ne ''})
        if($fields.Count -lt 8){continue}
        $flags=@($fields[2] -split ',')
        $tokens=@($fields | Select-Object -Skip 8)
        if($tokens | Where-Object {$_.StartsWith('[')}){throw 'Redis Cluster有进行中的逐键迁移，保持HOLD'}
        $isMaster=$flags -ccontains 'master'
        if($flags -ccontains 'myself'){$selfId=$fields[0];$selfRanges=$tokens;$selfIsMaster=$isMaster}
        if(-not $isMaster -or $tokens.Count -eq 0){continue}
        if($flags -ccontains 'fail' -or $flags -ccontains 'fail?' -or $flags -ccontains 'handshake' -or $flags -ccontains 'noaddr'){throw 'Redis Cluster持有slot的主节点状态不稳定，保持HOLD'}
        foreach($token in $tokens){
            if($token -cnotmatch '^(?<low>[0-9]+)(-(?<high>[0-9]+))?$'){throw 'CLUSTER NODES的slot段非法'}
            $low=[int]$Matches.low;$high=if($Matches.high){[int]$Matches.high}else{$low}
            $total+=$high-$low+1
        }
        $entries.Add($fields[0]+'|'+($tokens -join ','))
    }
    if($selfId -ceq ''){throw 'CLUSTER NODES没有myself行'}
    if(-not $selfIsMaster -or $selfRanges.Count -eq 0){throw 'Redis目标不是持有slot的主节点，计划要列主节点'}
    if($total -ne 16384){throw "Redis Cluster持有slot合计$total，不是16384，不能断言扫完了玩家域"}
    return [pscustomobject]@{NodeId=$selfId;Ranges=$selfRanges;Fingerprint=(@($entries | Sort-Object -CaseSensitive) -join ';')}
}

function Get-MaintenanceRedisClusterView {
    param($Session)
    $info=Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('INFO','cluster'))
    if($info -cnotmatch '(?m)^cluster_enabled:1\r?$'){return $null}
    return (ConvertFrom-MaintenanceClusterNodes -Text (Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('CLUSTER','NODES'))))
}

# HashSlot,与 go/shared/redisslot 同一算法:第一个 {} 之间非空时只算这段,CRC16-XMODEM 取低 14 位。
function Get-MaintenanceRedisKeySlot {
    param([Parameter(Mandatory)][string]$Key)
    [byte[]]$bytes=[Text.UTF8Encoding]::new($false,$true).GetBytes($Key)
    $open=[Array]::IndexOf($bytes,[byte]123)
    if($open -ge 0){
        $close=[Array]::IndexOf($bytes,[byte]125,$open+1)
        if($close -gt $open+1){$bytes=$bytes[($open+1)..($close-1)]}
    }
    [int]$crc=0
    foreach($value in $bytes){
        $crc=$crc -bxor ([int]$value -shl 8)
        for($bit=0;$bit -lt 8;$bit++){
            if($crc -band 0x8000){$crc=(($crc -shl 1) -bxor 0x1021) -band 0xFFFF}else{$crc=($crc -shl 1) -band 0xFFFF}
        }
    }
    return ($crc -band 16383)
}

# Cluster 下 nonce 要落在本节点持有的 slot 上,否则 SET 回 MOVED。候选 tag 按序号枚举,结果只由键前缀和 slot 段决定,P0 与 P4 算出同一把;
# 拓扑变了算出来就是另一把,读到 none 或 MOVED,照样保持 HOLD。
function Get-MaintenanceClusterNonceKey {
    param([Parameter(Mandatory)][string]$BaseKey,[Parameter(Mandatory)][string[]]$Ranges)
    for($i=0;$i -lt 65536;$i++){
        $key=$BaseKey+':{n'+$i.ToString([Globalization.CultureInfo]::InvariantCulture)+'}'
        $slot=Get-MaintenanceRedisKeySlot -Key $key
        foreach($range in $Ranges){
            $parts=@($range -split '-')
            if($slot -ge [int]$parts[0] -and $slot -le [int]$parts[-1]){return $key}
        }
    }
    throw '找不到落在本节点slot上的nonce键'
}

function Get-MaintenanceRedisNonceIdentity {
    param([Collections.IDictionary]$Plan,[Collections.IDictionary]$Target,$ClusterView=$null)
    if($Plan.operationId -cnotmatch '^[a-f0-9]{32}$' -or $Plan.operationId -ceq ('0'*32)){throw 'Redis nonce必须绑定正式非零run16'}
    try{$bytes=[Convert]::FromBase64String([string]$Target.maintenanceNonce)}catch{throw 'Redis nonce必须为正式16字节'}
    if($bytes.Length -ne 16 -or [Convert]::ToHexString($bytes) -ceq ('0'*32) -or [Convert]::ToBase64String($bytes) -cne $Target.maintenanceNonce){throw 'Redis nonce必须为规范非零16字节'}
    $key='sql:writer:maintenance:'+$Plan.operationId+':nonce'
    if($null -ne $ClusterView){$key=Get-MaintenanceClusterNonceKey -BaseKey $key -Ranges $ClusterView.Ranges}
    return @{key=$key;value=[Convert]::ToHexString($bytes).ToLowerInvariant();bytes=[Convert]::ToBase64String($bytes)}
}

function Get-MaintenanceRedisNonceObservation {
    param($Session,[Collections.IDictionary]$Identity)
    # 首尾必须强读同一个永久string；server run_id不变也不能把FLUSHDB后的空库认作排空。
    $type=Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('TYPE',$Identity.key))
    if($type -cne 'string'){throw 'Redis维护nonce缺失或类型改变，保持HOLD，禁止补种'}
    $actual=Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('GET',$Identity.key))
    $pttl=Invoke-MaintenanceRedisRead -Session $Session -Arguments @('PTTL',$Identity.key)
    if($actual -cne $Identity.value -or $pttl.Kind -cne 'integer' -or $pttl.Number -ne -1){throw 'Redis维护nonce值/永久性改变，保持HOLD，禁止补种'}
    return @{value=[Convert]::ToBase64String([Convert]::FromHexString($actual));pttl=[long]$pttl.Number}
}

function Initialize-MaintenanceRedisNonces {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$OutputDirectory)
    if(-not $Plan.Contains('redisTargets') -or @($Plan.redisTargets).Count -eq 0 -or @($Plan.redisTargets).Count -gt 128){throw 'P0 Redis目标缺失或超预算'}
    $attemptTargets=[Collections.Generic.List[object]]::new();$physical=@{};$targetsSeen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($target in $Plan.redisTargets){
        $identity=Get-MaintenanceRedisNonceIdentity -Plan $Plan -Target $target;$db=Get-MaintenanceRedisDb -Target $target
        $key=[string]$target.address+'|'+[string]$db
        if(-not $targetsSeen.Add([string]$target.role+'|'+$key)){throw 'P0 Redis角色/物理目标重复'}
        if($physical.ContainsKey($key)){throw 'P0 Redis按address/db禁止重复物理目标'}
        $physical[$key]=$identity.bytes
        $attemptTargets.Add([ordered]@{role=$target.role;address=$target.address;db=$db;serverRunId=$target.serverRunId;maintenanceNonce=$identity.bytes})
    }
    # CreateNew先于唯一SET NX；崩溃/未知结果恢复只读，绝不能重新种回已被清库的nonce。
    $outputRoot=Get-Item -LiteralPath ([IO.Path]::GetFullPath($OutputDirectory)) -ErrorAction Stop
    if(-not $outputRoot.PSIsContainer -or $outputRoot.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'P0 nonce attempt必须写入既定非重解析私有目录'}
    $attemptPath=Join-Path $outputRoot.FullName 'redis-nonce-seed.attempt.json'
    $attemptBytes=[Text.UTF8Encoding]::new($false).GetBytes(([ordered]@{operationId=$Plan.operationId;targets=$attemptTargets.ToArray()}|ConvertTo-Json -Depth 12 -Compress)+[Environment]::NewLine)
    $first=$false;$attemptStream=$null
    try{$attemptStream=[IO.FileStream]::new($attemptPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::Read);$attemptStream.Write($attemptBytes,0,$attemptBytes.Length);$attemptStream.Flush($true);$first=$true}
    catch [IO.IOException]{if(-not [IO.File]::Exists($attemptPath)){throw};Write-MaintenanceImmutableBytes -Path $attemptPath -Bytes $attemptBytes}
    finally{if($null -ne $attemptStream){$attemptStream.Dispose()}}
    $clock=[Diagnostics.Stopwatch]::StartNew();$seeded=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal);$pins=[Collections.Generic.List[object]]::new()
    foreach($target in $Plan.redisTargets){
        $db=Get-MaintenanceRedisDb -Target $target
        $session=Open-MaintenanceRedis -Target $target -Clock $clock
        try{
            $started=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds();$runId=Get-MaintenanceRedisRunId $session
            if($runId -cne $target.serverRunId){throw 'P0 Redis物理run_id改变'}
            $clusterBefore=Get-MaintenanceRedisClusterView $session
            $identity=Get-MaintenanceRedisNonceIdentity -Plan $Plan -Target $target -ClusterView $clusterBefore
            $physicalKey=[string]$target.address+'|'+[string]$db
            if($first -and $seeded.Add($physicalKey)){
                $reply=Send-MaintenanceRedisCommand -Session $session -Arguments @('SET',$identity.key,$identity.value,'NX')
                if($reply.Kind -cne 'text' -or $reply.Text -cne 'OK'){throw 'P0 Redis nonce并非本次唯一创建，保持HOLD，不能覆盖'}
            }
            $before=Get-MaintenanceRedisNonceObservation -Session $session -Identity $identity
            $after=Get-MaintenanceRedisNonceObservation -Session $session -Identity $identity
            if((Get-MaintenanceRedisRunId $session) -cne $runId){throw 'P0 Redis首尾物理run_id不同'}
            $clusterAfter=Get-MaintenanceRedisClusterView $session
            if(($null -eq $clusterBefore) -ne ($null -eq $clusterAfter) -or ($null -ne $clusterAfter -and $clusterAfter.Fingerprint -cne $clusterBefore.Fingerprint)){throw 'P0 Redis Cluster拓扑在取样期间变化，保持HOLD'}
            $ended=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds();Assert-MaintenanceRedisDeadline $session
            if($ended -lt $started -or $ended-$started -gt 60000){throw 'P0 Redis原件时窗非法'}
            $proof=[ordered]@{schema='SQL_DRAIN_REDIS_BASELINE_V1';role=$target.role;address=$target.address;db=$db;server_run_id=$runId;observed_start_ms=$started;observed_end_ms=$ended;nonce_key=$identity.key;nonce_before=$before.value;nonce_after=$after.value;nonce_pttl_before_ms=$before.pttl;nonce_pttl_after_ms=$after.pttl}
            if($null -ne $clusterAfter){$proof['cluster']=[ordered]@{node_id=$clusterAfter.NodeId;fingerprint_before=$clusterBefore.Fingerprint;fingerprint_after=$clusterAfter.Fingerprint}}
            $proofPath=Join-Path $OutputDirectory ('redis-'+$pins.Count+'.baseline.v1.json')
            if([IO.File]::Exists($proofPath)){
                $existing=Read-MaintenanceJson -Path $proofPath
                Assert-MaintenanceProperties -Value $existing -Required @($proof.Keys)
                foreach($name in @($proof.Keys|Where-Object {$_ -cnotin @('observed_start_ms','observed_end_ms','cluster')})){if([string]$existing[$name] -cne [string]$proof[$name]){throw 'P0 Redis恢复原件身份/nonce冲突'}}
                if($proof.Contains('cluster') -and [string]$existing['cluster']['fingerprint_after'] -cne [string]$proof['cluster']['fingerprint_after']){throw 'P0 Redis恢复原件拓扑已变，保持HOLD'}
                if([long]$existing.observed_start_ms -le 0 -or [long]$existing.observed_end_ms -lt [long]$existing.observed_start_ms -or [long]$existing.observed_end_ms-[long]$existing.observed_start_ms -gt 60000){throw 'P0 Redis旧原件时窗非法'}
                $sha=Get-MaintenanceSha256 $proofPath
            }else{$sha=Write-MaintenanceImmutableJson -Path $proofPath -Value $proof}
            $pins.Add([ordered]@{absolutePath=[IO.Path]::GetFullPath($proofPath);sha256=[Convert]::ToBase64String([Convert]::FromHexString($sha))})
        }finally{$session.Stream.Dispose();$session.Client.Dispose()}
    }
    $pinBytes=[Text.UTF8Encoding]::new($false).GetBytes((ConvertTo-Json -InputObject $pins.ToArray() -Depth 80 -Compress))
    Write-MaintenanceImmutableBytes -Path (Join-Path $OutputDirectory 'redis-baseline-pins.protojson') -Bytes $pinBytes
    return $pins.ToArray()
}

function Get-MaintenanceRedisKeyKind {
    param([string]$Key)
    if($Key -cmatch '^player:\{(?<player>[1-9][0-9]*)\}:handoff_fence$'){
        $number=[uint64]0
        if(-not [uint64]::TryParse($Matches.player,[ref]$number) -or $number.ToString([Globalization.CultureInfo]::InvariantCulture) -cne $Matches.player){throw '交接资格玩家键不是规范uint64'}
        return @{kind='handoff_fence';topic='';type='hash'}
    }
    throw 'SCAN发现未分类键，不能猜空或删掉'
}

function Get-MaintenanceRedisObservation {
    param($Session,[string]$Key,[Collections.IDictionary]$Meaning)
    $type=Get-MaintenanceRedisText (Invoke-MaintenanceRedisRead -Session $Session -Arguments @('TYPE',$Key))
    $pttl=Invoke-MaintenanceRedisRead -Session $Session -Arguments @('PTTL',$Key)
    if($pttl.Kind -cne 'integer'){throw 'Redis PTTL原件类型不符'}
    $members=[Collections.Generic.List[string]]::new();$values=[Collections.Generic.List[string]]::new();$count=[uint64]0
    if($type -ceq 'none'){
        if($pttl.Number -ne -2){throw 'Redis不存在键的PTTL不一致'}
    }else{
        if($type -cne $Meaning.type -or $pttl.Number -ne -1){throw 'Redis键类型/永久性不符，不能把错误类型/TTL当空'}
        $reply=Invoke-MaintenanceRedisRead -Session $Session -Arguments @('HLEN',$Key)
        if($reply.Kind -cne 'integer' -or $reply.Number -lt 0 -or $reply.Number -gt 16384){throw 'Redis成员数不符或超16K预算'}
        $count=[uint64]$reply.Number
        $reply=Invoke-MaintenanceRedisRead -Session $Session -Arguments @('HGETALL',$Key)
        if($reply.Kind -cne 'array'){throw 'Redis成员原件类型不符'}
        if($reply.Items.Count -ne 2*[int]$count){throw 'Redis取样期间集合变化，必须HOLD重开全新尝试'}
        for($i=0;$i -lt $reply.Items.Count;$i+=2){
            if($reply.Items[$i].Kind -cne 'bulk'){throw 'Redis成员非原始bytes'}
            $members.Add([Convert]::ToBase64String($reply.Items[$i].Data))
            if($reply.Items[$i+1].Kind -cne 'bulk'){throw 'Redis值非原始bytes'}
            $values.Add([Convert]::ToBase64String($reply.Items[$i+1].Data))
        }
    }
    $dump=Invoke-MaintenanceRedisRead -Session $Session -Arguments @('DUMP',$Key)
    if($dump.Kind -ceq 'nil'){$dumpBytes=[byte[]]::new(0)}elseif($dump.Kind -ceq 'bulk'){$dumpBytes=$dump.Data}else{throw 'Redis DUMP原件非法'}
    if(($type -ceq 'none') -ne ($dump.Kind -ceq 'nil')){throw 'Redis键在取样期间出现/消失，HOLD'}
    return [ordered]@{key=$Key;kind=$Meaning.kind;topic=$Meaning.topic;redisType=$type;pttlMs=$pttl.Number;count=$count;members=$members.ToArray();values=$values.ToArray();dumpSha256=[Convert]::ToBase64String([Security.Cryptography.SHA256]::HashData($dumpBytes))}
}

function Collect-MaintenanceRedisClosure {
    param([Parameter(Mandatory)][Collections.IDictionary]$Plan,[Parameter(Mandatory)][string]$OutputDirectory)
    # 本地输入只是正式P0 Plan的操作投影；不成为第二manifest。最终v2 owner逐项与PlanProto原件比等集。
    if(-not $Plan.Contains('redisTargets') -or @($Plan.redisTargets).Count -eq 0 -or @($Plan.redisTargets).Count -gt 128){throw '缺固定Redis物理目标或超既有source预算'}
    $patterns=@('player:{*}:handoff_fence')
    $clock=[Diagnostics.Stopwatch]::StartNew();$targets=[Collections.Generic.List[object]]::new();$targetSeen=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach($target in $Plan.redisTargets){
        $db=Get-MaintenanceRedisDb -Target $target
        $identity=$target.role+'|'+$target.address+'|'+[string]$db
        if(-not $targetSeen.Add($identity)){throw 'Redis物理目标重复'}
        $session=Open-MaintenanceRedis -Target $target -Clock $clock
        try{
            $started=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
            $runId=Get-MaintenanceRedisRunId $session
            if($runId -cne $target.serverRunId){throw 'Redis进程世代改变，不能缝合旧取样'}
            $clusterBefore=Get-MaintenanceRedisClusterView $session
            $nonceIdentity=Get-MaintenanceRedisNonceIdentity -Plan $Plan -Target $target -ClusterView $clusterBefore
            $nonceBefore=Get-MaintenanceRedisNonceObservation -Session $session -Identity $nonceIdentity
            $pages=[Collections.Generic.List[object]]::new();$keys=[Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
            foreach($seed in $target.observations){if(-not $keys.Add([string]$seed.key)){throw 'P0 required Redis键重复'}}
            foreach($pattern in $patterns){
                $cursor=[uint64]0
                do{
                    if($pages.Count -ge 16384){throw 'SCAN页数超既有16K预算，不分页绕过'}
                    $reply=Invoke-MaintenanceRedisRead -Session $session -Arguments @('SCAN',$cursor.ToString([Globalization.CultureInfo]::InvariantCulture),'MATCH',$pattern,'COUNT','128')
                    if($reply.Kind -cne 'array' -or $reply.Items.Count -ne 2 -or $reply.Items[1].Kind -cne 'array'){throw 'SCAN原件形状非法'}
                    $next=[uint64]::Parse((Get-MaintenanceRedisText $reply.Items[0]),[Globalization.CultureInfo]::InvariantCulture)
                    $pageKeys=[Collections.Generic.List[string]]::new()
                    foreach($raw in $reply.Items[1].Items){
                        $key=Get-MaintenanceRedisText $raw
                        if($key.Length -gt 4096){throw 'SCAN键超过预算'}
                        $null=Get-MaintenanceRedisKeyKind -Key $key
                        $pageKeys.Add($key);$null=$keys.Add($key)
                        if($keys.Count -gt 16384){throw 'Redis observation超过既有16K预算'}
                    }
                    $pages.Add([ordered]@{pattern=$pattern;input_cursor=$cursor;output_cursor=$next;keys=$pageKeys.ToArray()})
                    $cursor=$next
                }while($cursor -ne 0)
            }
            $observations=[Collections.Generic.List[object]]::new()
            foreach($key in @($keys | Sort-Object -CaseSensitive)){
                $meaning=Get-MaintenanceRedisKeyKind -Key $key
                $observations.Add((Get-MaintenanceRedisObservation -Session $session -Key $key -Meaning $meaning))
            }
            $nonceAfter=Get-MaintenanceRedisNonceObservation -Session $session -Identity $nonceIdentity
            if((Get-MaintenanceRedisRunId $session) -cne $runId){throw 'Redis取样首尾物理run_id不同'}
            $clusterAfter=Get-MaintenanceRedisClusterView $session
            if(($null -eq $clusterBefore) -ne ($null -eq $clusterAfter) -or ($null -ne $clusterAfter -and $clusterAfter.Fingerprint -cne $clusterBefore.Fingerprint)){throw 'Redis Cluster拓扑在取样期间变化，不能断言扫完了玩家域，保持HOLD'}
            $ended=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds();Assert-MaintenanceRedisDeadline $session
            if($ended -lt $started -or $ended-$started -gt 60000){throw 'Redis SCAN原件时窗非法'}
            $proof=[ordered]@{schema='SQL_DRAIN_REDIS_SCAN_V2';role=$target.role;address=$target.address;db=$db;server_run_id=$runId;observed_start_ms=$started;observed_end_ms=$ended;nonce_key=$nonceIdentity.key;nonce_before=$nonceBefore.value;nonce_after=$nonceAfter.value;nonce_pttl_before_ms=$nonceBefore.pttl;nonce_pttl_after_ms=$nonceAfter.pttl;pages=$pages.ToArray()}
            if($null -ne $clusterAfter){$proof['cluster']=[ordered]@{node_id=$clusterAfter.NodeId;fingerprint_before=$clusterBefore.Fingerprint;fingerprint_after=$clusterAfter.Fingerprint}}
            $safeName='redis-'+$targets.Count+'.scan.v2.json';$proofPath=Join-Path $OutputDirectory $safeName
            $proofSha=Write-MaintenanceImmutableJson -Path $proofPath -Value $proof
            if((Get-Item -LiteralPath $proofPath).Length -gt 16777216){throw '完整SCAN原件超过既有16MiB预算，不能拆源逃逸'}
            $targets.Add([ordered]@{role=$target.role;address=$target.address;db=$db;serverRunId=$runId;maintenanceNonce=$nonceAfter.value;zones=@($target.zones);observations=$observations.ToArray();scanSourceEvidence=@([ordered]@{absolutePath=[IO.Path]::GetFullPath($proofPath);sha256=[Convert]::ToBase64String([Convert]::FromHexString($proofSha))})})
        }finally{$session.Stream.Dispose();$session.Client.Dispose()}
    }
    $null=Write-MaintenanceImmutableJson -Path (Join-Path $OutputDirectory 'redis-targets.protojson') -Value $targets.ToArray()
    return $targets.ToArray()
}

if ($MyInvocation.InvocationName -ne '.' -and $MaintenanceCommand -ne 'library') {
    if ([string]::IsNullOrWhiteSpace($PlanPath) -or [string]::IsNullOrWhiteSpace($OutputPath)) { throw '维护命令必须显式计划及永久输出路径' }
    $maintenancePlan=Read-MaintenanceJson -Path $PlanPath
    $maintenanceRepo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
    switch ($MaintenanceCommand) {
        'normal-tool' { Invoke-NormalFiniteTool -Plan $maintenancePlan -OutputDirectory $OutputPath -RepoRoot $maintenanceRepo }
        'render-roles' {
            $roles=ConvertTo-AuthorityRoleSql -Plan $maintenancePlan -TemplateRoot (Join-Path $maintenanceRepo 'deploy/sql')
            $null=Write-MaintenanceImmutableJson -Path $OutputPath -Value $roles
        }
        'close-publication' {
            $fence=Set-PermanentPublicationFence -Plan $maintenancePlan -RepoRoot $maintenanceRepo
            # 仅发布关闭请求事实，尚不声称根接受集合已settled；P1由tool owner核真实IssuerClosure。
            $null=Write-MaintenanceImmutableJson -Path $OutputPath -Value ([ordered]@{kind='PUBLICATION_CLOSE_REQUESTED';operationId=$maintenancePlan.operationId;manifestSha256=$maintenancePlan.manifestSha256;fencePath=$fence;fenceSha256=(Get-MaintenanceSha256 $fence)})
        }
        'close-source' { Request-MaintenanceRoleClosure -Plan $maintenancePlan -Role SOURCE -Output $OutputPath -RepoRoot $maintenanceRepo }
        'close-relay' { Request-MaintenanceRoleClosure -Plan $maintenancePlan -Role RELAY -Output $OutputPath -RepoRoot $maintenanceRepo }
        'activate-runtime' { Activate-MaintenanceRuntime -Plan $maintenancePlan -Output $OutputPath }
        'retire-source-writers' { Invoke-MaintenanceWriterRetirement -Plan $maintenancePlan -Stage P1 -OutputDirectory $OutputPath }
        'retire-relay-writers' { Invoke-MaintenanceWriterRetirement -Plan $maintenancePlan -Stage P3 -OutputDirectory $OutputPath }
        'open-source' { Request-MaintenanceSourceOpen -Plan $maintenancePlan -Output $OutputPath }
        'stop-and-collect' {
            $launcher=if($maintenancePlan.collectorFamily -ceq 'go'){'go_services.ps1'}elseif($maintenancePlan.collectorFamily -ceq 'cpp'){'cpp_nodes.ps1'}else{throw '必须选择已具名go/cpp owner，J走自己的严格stop-slice'}
            $launcherPath=Join-Path $PSScriptRoot $launcher
            if((Get-MaintenanceSha256 -Path $launcherPath) -cne $maintenancePlan.launcherSha256){throw '实际launcher源码不在固定P0 source pin'}
            & $launcherPath -Command stop-and-report -MaintenancePlanPath $PlanPath -StopReportPath $OutputPath
            if(-not $?){throw '具名owner严格停止失败，保持HOLD'}
        }
        'seed-redis-nonce' { $null=Initialize-MaintenanceRedisNonces -Plan $maintenancePlan -OutputDirectory $OutputPath }
        'collect-redis' { $null=Collect-MaintenanceRedisClosure -Plan $maintenancePlan -OutputDirectory $OutputPath }
        'manual-baseline' { Request-MaintenanceManualBaseline -Plan $maintenancePlan -Output $OutputPath }
        'manual-sampling-request' { $null=New-MaintenanceManualSamplingRequest -Plan $maintenancePlan -Output $OutputPath }
        'collect-kafka' { Invoke-AuthorityPhaseCli -Plan $maintenancePlan -Phase collect-kafka -Output $OutputPath }
        'verify-and-advance' { Invoke-AuthorityPhaseCli -Plan $maintenancePlan -Phase $maintenancePlan.authorityPhase -Output $OutputPath }
    }
}
