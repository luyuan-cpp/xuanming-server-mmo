#requires -Version 7
# 纯合同夹具；不运行进程/SQL/Redis，不将虚构qualification文件当真实I3资格。
$ErrorActionPreference='Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../maintenance_drain.ps1"
$taskRoot=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-drain-contract-'+[Guid]::NewGuid().ToString('N'))
$null=[IO.Directory]::CreateDirectory($taskRoot)
$savedFence=[Environment]::GetEnvironmentVariable('MMORPG_PUBLICATION_FENCE_DIR','Process')
try {
    Test-Case '永久发布关闭没有TTL，普通start无法重开' {
        [Environment]::SetEnvironmentVariable('MMORPG_PUBLICATION_FENCE_DIR',(Join-Path $taskRoot 'fences'),'Process')
        Assert-PublicationStartAllowed -RepoRoot $taskRoot
        $plan=@{operationId=('a'*32);manifestSha256=('b'*64);permanentRevivalFenceId='fixed-run-fence'}
        $first=Set-PermanentPublicationFence -Plan $plan -RepoRoot $taskRoot
        $second=Set-PermanentPublicationFence -Plan $plan -RepoRoot $taskRoot
        Assert-Equal -Expected $first -Actual $second -Because '同原件只认重，不改关闭时刻'
        $message='';try{Assert-PublicationStartAllowed -RepoRoot $taskRoot}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '永久关闭' -Because '无论过了多少时间都拒普通重启'
        $plan.manifestSha256='c'*64
        $message='';try{$null=Set-PermanentPublicationFence -Plan $plan -RepoRoot $taskRoot}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '冲突' -Because '同operation不能换manifest续租'
    }
    Test-Case '维护JSON拒重复/未知字段，读取bytes必须同固定SHA' {
        $p=Join-Path $taskRoot 'duplicate.json'
        [IO.File]::WriteAllText($p,'{"run":1,"run":2}')
        $message='';try{$null=Read-MaintenanceJson -Path $p}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '重复' -Because '重复键不能由JSON最后一个值偷换原件'
        [IO.File]::WriteAllText($p,'{"run":1}')
        $message='';try{$null=Read-MaintenanceJson -Path $p -ExpectedSha256 ('0'*64)}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern 'SHA' -Because '校验的是实际读取的同bytes'
        $message='';try{Assert-MaintenanceProperties -Value @{expected=1;unknown=2} -Required @('expected')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '未知' -Because '不默默忽略额外授权字段'
    }
    Test-Case '权限renderer只从正式REGISTER已固定原语出列级GRANT' {
        $qual=Join-Path $taskRoot 'fixture-only-not-I3.txt'
        [IO.File]::WriteAllText($qual,'仅纯renderer夹具，不是数据库资格')
        $qsha=Get-MaintenanceSha256 $qual
        $requestPath=Join-Path $taskRoot 'registration.fixture.json'
        $request=@{lockPrimitive='LOCK_TOUCH';lockQualificationSha256=[Convert]::ToBase64String([Convert]::FromHexString($qsha));writer=@{sqlPrincipal=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('runtime_boot@%'))}}
        $rsha=Write-MaintenanceImmutableJson -Path $requestPath -Value $request
        $plan=@{authoritySchema='authority_fixture';runtimePrincipal='runtime_boot@%';registrarPrincipal='registrar_fixture@%';maintenancePrincipal='maintenance_fixture@%';schemaPrincipal='schema_fixture@%';registrationRequestFile=$requestPath;registrationRequestSha256=$rsha;lockQualificationFile=$qual;tables=@(@{schema='domain_fixture';table='player';operations=@('SELECT','INSERT','UPDATE')});schemaDatabases=@('authority_fixture','domain_fixture')}
        $roles=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')
        $runtimeStatements=($roles.runtime -split "`r?`n" | Where-Object { $_ -notmatch '^\s*--' }) -join "`n"
        Assert-Match -Text $roles.runtime -Pattern 'UPDATE \(lock_touch\)' -Because '只授无权限意义列锁写'
        Assert-Match -Text $roles.runtime -Pattern 'writer_lifecycle_receipt' -Because 'normal强读原REGISTER核部署/原语'
        Assert-True -Condition ($runtimeStatements -notmatch '(?i)ALL PRIVILEGES|CREATE|ALTER|GRANT OPTION|ON\s+[^;]+\.\*') -Because 'runtime没有DDL/全schema/注册能力'
        Assert-Match -Text $roles.operator -Pattern 'schema_fixture' -Because 'fresh schema独立principal'
        Assert-True -Condition ($roles.operator -notmatch 'id_segment') -Because '发号器不用 SQL,schema 账号不该再有 id_segment 的写权限'
        Assert-Match -Text $roles.operator -Pattern 'CREATE,INSERT,UPDATE ON `domain_fixture`\.`schema_migrations`' -Because '迁移账本只给具名表写权限'
        Assert-True -Condition ($roles.operator -notmatch 'guild_player_state') -Because '没有具名guild schema时不额外授哨兵写权限'
        $plan.schemaDatabases=@('authority_fixture','domain_fixture','mmorpg_guild')
        $guildRoles=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')
        Assert-Match -Text $guildRoles.operator -Pattern 'GRANT INSERT ON `mmorpg_guild`\.`guild_player_state`' -Because 'guild正式migrate只给固定哨兵表INSERT'
        Assert-True -Condition ($guildRoles.runtime -notmatch 'guild_player_state') -Because 'Schema seed能力不进入normal业务grant'
        $plan.tables=@(@{schema='authority_fixture';table='writer_registry';operations=@('UPDATE')})
        $message='';try{$null=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '权限真值' -Because '业务表清单不能混入registry自助篡改'
        $plan.tables=@(@{schema='authority_fixture';table='player_route';operations=@('SELECT')})
        $route=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')
        Assert-Match -Text $route.runtime -Pattern ([regex]::Escape('GRANT SELECT ON `authority_fixture`.`player_route`')) -Because 'go/db认领前按player_route选库，只读授权由计划tables给出'
        Assert-True -Condition (($route.runtime+$route.operator) -notmatch 'route_reconcile') -Because '对账表已删除，任何账号都不再授权'
        Assert-Match -Text $route.operator -Pattern ([regex]::Escape('GRANT SELECT,INSERT,DELETE ON `authority_fixture`.`zone_merged_into`')) -Because '合服工具的操作员身份要能写映射表'
        Assert-True -Condition ($route.runtime -notmatch 'zone_merged_into') -Because '运行账号只有计划tables列出时才有映射表的读权限，操作员的写权限不进runtime'
        $plan.tables=@(@{schema='authority_fixture';table='zone_merged_into';operations=@('SELECT')})
        $merged=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')
        Assert-Match -Text $merged.runtime -Pattern ([regex]::Escape('GRANT SELECT ON `authority_fixture`.`zone_merged_into`')) -Because 'match启动读全局库的合服映射，只读授权由计划tables给出'
        $plan.tables=@(@{schema='authority_fixture';table='player_route';operations=@('SELECT');updateColumns=@('storage_id')})
        $message='';try{$null=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '未知' -Because '列级UPDATE特例已随对账表删除，计划不能再带updateColumns'
        foreach($forbidden in @('route_meta','ROUTE_META','Route_Meta','writer_registry','WRITER_REGISTRY','writer_slot','WRITER_SLOT','writer_lifecycle_receipt','WRITER_LIFECYCLE_RECEIPT','sql_writer_drain_run','SQL_WRITER_DRAIN_RUN','sql_writer_freeze_ticket','SQL_WRITER_FREEZE_TICKET','sql_writer_maintenance_receipt','SQL_WRITER_MAINTENANCE_RECEIPT')){$plan.tables=@(@{schema='authority_fixture';table=$forbidden;operations=@('UPDATE')});$message='';try{$null=ConvertTo-AuthorityRoleSql -Plan $plan -TemplateRoot (Join-Path $PSScriptRoot '../../../deploy/sql')}catch{$message=$_.Exception.Message};Assert-Match -Text $message -Pattern '权限真值' -Because '所有真正权限真值表仍禁止business清单写入'}
    }
    Test-Case '在线激活保留原runtime字节且拒绝替换既有上下文' {
        $control=Join-Path $taskRoot 'control';$null=[IO.Directory]::CreateDirectory($control)
        $runtime=Join-Path $taskRoot 'runtime.json';[IO.File]::WriteAllText($runtime,'{"runPhase":"ROOTS_CLOSED"}',[Text.UTF8Encoding]::new($false))
        $plan=@{controlDirectory=$control;planFile=$runtime;runtimePlanSha256=(Get-MaintenanceSha256 $runtime);operationId=('1'*32);manifestSha256=('2'*64)}
        Activate-MaintenanceRuntime -Plan $plan -Output (Join-Path $taskRoot 'activation.json')
        Assert-Equal -Expected $plan.runtimePlanSha256 -Actual (Get-MaintenanceSha256 (Join-Path $control 'active.runtime.json')) -Because '应用必须看到同一正式原件，不得重序列化改SHA'
        Assert-Equal -Expected ($plan.runtimePlanSha256+"`n") -Actual ([IO.File]::ReadAllText((Join-Path $control 'active.runtime.sha256'))) -Because 'hash发布后才允许应用消费'
        [IO.File]::WriteAllText($runtime,'{"runPhase":"DRAINING"}',[Text.UTF8Encoding]::new($false));$plan.runtimePlanSha256=Get-MaintenanceSha256 $runtime
        $message='';try{Activate-MaintenanceRuntime -Plan $plan -Output (Join-Path $taskRoot 'activation-other.json')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '冲突' -Because '同boot既有控制上下文不被后来计划覆盖'
    }
    Test-Case '本轮退休分P1直接writer与P3共享relay且原件不可跨run' {
        $writer=[Convert]::ToBase64String([Convert]::FromHexString('3'*32));$run='4'*32
        $request=@{kind='RETIRE';maintenanceRunId=[Convert]::ToBase64String([Convert]::FromHexString($run));requestId=[Convert]::ToBase64String([Convert]::FromHexString('5'*32));writer=@{writerKey=$writer;instanceId='boot-fixture'};expectedEpoch='7';expectedCatalogRevision='9'}
        $file=Join-Path $taskRoot 'retire.json';$sha=Write-MaintenanceImmutableJson -Path $file -Value $request
        $entry=@{stage='P1';requestFile=$file;requestSha256=$sha};$issuer=@{physicalWriterKey=$writer;instanceId='boot-fixture';logicalRole='SOURCE';sharesRelay=$false}
        $plan=@{operationId=$run;writerRetirements=@($entry);issuers=@($issuer)}
        Assert-Equal -Expected 1 -Actual @(Get-MaintenanceRetirementRequests -Plan $plan -Stage P1).Count -Because 'P1必须包含本轮直接writer实际RETIRE请求'
        $issuer.sharesRelay=$true
        $message='';try{$null=Get-MaintenanceRetirementRequests -Plan $plan -Stage P1}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '共享/relay必须P3' -Because 'source关闭不能提前退休仍在排空的共用relay'
        $entry.stage='P3'
        Assert-Equal -Expected 1 -Actual @(Get-MaintenanceRetirementRequests -Plan $plan -Stage P3).Count -Because 'P3必须退共享writer，不能推迟到P8'
        $plan.operationId='6'*32
        $message='';try{$null=Get-MaintenanceRetirementRequests -Plan $plan -Stage P3}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '未绑定同run' -Because '其他维护run的退休请求不能接入本轮'
    }
    Test-Case 'logical close信号与应用契约同字节且坏report不得宣称闭合' {
        $drain=Join-Path $taskRoot 'logical-drain';$null=[IO.Directory]::CreateDirectory($drain)
        $runtime=Join-Path $drain 'runtime.json';[IO.File]::WriteAllText($runtime,'{}')
        [IO.File]::WriteAllText((Join-Path $drain 'source.issuer.json'),'{"passed":true}')
        $plan=@{operationId=('7'*32);manifestSha256=('8'*64);permanentRevivalFenceId='logical-fence';controlDirectory=$drain;drainDirectory=$drain;planFile=$runtime;runtimePlanSha256=(Get-MaintenanceSha256 $runtime);issuer=@{logicalRole='SOURCE'}}
        $message='';try{Request-MaintenanceRoleClosure -Plan $plan -Role SOURCE -Output (Join-Path $drain 'result.json') -RepoRoot $taskRoot}catch{$message=$_.Exception.Message}
        Assert-Equal -Expected "CLOSE_SOURCE`n" -Actual ([IO.File]::ReadAllText((Join-Path $drain 'close-source.request'))) -Because '应用Requested只认正式动作字节，不认脚本命令名'
        Assert-Match -Text $message -Pattern '缺字段' -Because '信号已送达不等于accepted frontier已闭合'
        Assert-True -Condition (-not [IO.File]::Exists((Join-Path $drain 'result.json'))) -Because '坏report不能产出闭合原件'
        [IO.File]::WriteAllText((Join-Path $drain 'close-source.request'),"close-source`n")
        $message='';try{Request-MaintenanceRoleClosure -Plan $plan -Role SOURCE -Output (Join-Path $drain 'result.json') -RepoRoot $taskRoot}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '冲突' -Because '旧的错误信号必须HOLD，不能静默沿用或覆盖'
    }
    Test-Case '关闭动作与激活不得落入不同私有目录' {
        $one=Join-Path $taskRoot 'control-one';$two=Join-Path $taskRoot 'control-two'
        $null=[IO.Directory]::CreateDirectory($one);$null=[IO.Directory]::CreateDirectory($two)
        $plan=@{controlDirectory=$one;drainDirectory=$two}
        $message='';try{$null=Assert-MaintenanceControlDirectory -Plan $plan}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '同一' -Because '不能把关闭写给另一个boot后等待伪原件'
        $plan.drainDirectory=$one
        Assert-Equal -Expected ([IO.Path]::GetFullPath($one)) -Actual (Assert-MaintenanceControlDirectory -Plan $plan) -Because '正常动作使用激活过的固定目录'
        $plan.Remove('controlDirectory')
        $message='';try{$null=Assert-MaintenanceControlDirectory -Plan $plan}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern 'controlDirectory' -Because '缺少启动目录绑定不得凭drain路径猜测'
    }
    Test-Case 'Redis只认规范的交接资格键，退役键和非规范键HOLD' {
        $meaning=Get-MaintenanceRedisKeyKind -Key 'player:{18446744073709551615}:handoff_fence'
        Assert-Equal -Expected 'handoff_fence' -Actual $meaning.kind -Because '与restore.go的键角色一致'
        Assert-Equal -Expected 'hash' -Actual $meaning.type -Because '交接资格是hash'
        foreach($bad in @('kafka:retry:queue:task_g1','kafka:dead:queue:task_g1','player:save_pending:101:63','player:{01}:handoff_fence','player:{18446744073709551616}:handoff_fence','player:101:handoff')){
            $rejected=$false;try{$null=Get-MaintenanceRedisKeyKind -Key $bad}catch{$rejected=$true}
            Assert-True -Condition $rejected -Because '未覆盖/非规范键不能按glob忽略成零'
        }
    }
    Test-Case '伪进程消失或伪passed文件不能替代正式闭合原件' {
        $path=Join-Path $taskRoot 'fake-passed.json';[IO.File]::WriteAllText($path,'{"passed":true}')
        $issuer=@{applicationReportPath=$path;issuerId='a';instanceId='b';physicalWriterKey='c';logicalRole='SOURCE';permanentRevivalFenceId='d'}
        $message='';try{$null=Get-VerifiedIssuerClosure -Issuer $issuer -Plan @{}}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '缺字段' -Because '必须正式IssuerClosure全文，不是外部布尔'
    }

    Test-Case 'manual P3采样原件保留同run原Plan并拒未到尾部' {
        $dir=Join-Path $taskRoot 'manual-closure';$null=[IO.Directory]::CreateDirectory($dir)
        $identity=Join-Path $dir 'boot-identity.nodeinfo.json';[IO.File]::WriteAllText($identity,'{"nodeUuid":"fixture"}')
        $run=[Convert]::ToBase64String([Convert]::FromHexString('a'*32));$manifest=[Convert]::ToBase64String([Convert]::FromHexString('b'*64))
        $issuer=[Convert]::ToBase64String([Convert]::FromHexString('c'*32));$boot=[Convert]::ToBase64String([Convert]::FromHexString('d'*32))
        $pin=@{absolutePath=$identity;sha256=[Convert]::ToBase64String([Convert]::FromHexString((Get-MaintenanceSha256 $identity)))}
        $report=@{operationId=$run;manifestDigest=$manifest;acceptingClosed=$true;callbacksJoined=$true;accepted='4';settled='4';unresolvedPublications='0';assignments=@(@{issuerId=$issuer;instanceId=$boot;position='12';exclusiveEnd='12'});sourcePins=@($pin)}
        $reportFile=Join-Path $dir 'manual-closure.report.json';$null=Write-MaintenanceImmutableJson -Path $reportFile -Value $report
        $runtime=Join-Path $dir 'runtime.json';$runtimeSha=Write-MaintenanceImmutableJson -Path $runtime -Value @{runId=$run;planProto='AQID';evidence=@{operationId=$run;manifestDigest=$manifest}}
        $plan=@{operationId=('a'*32);manifestSha256=('b'*64);planFile=$runtime;runtimePlanSha256=$runtimeSha;issuers=@(@{logicalRole='SOURCE';issuerId=$issuer;instanceId=$boot;sourceEvidence=@($pin)})}
        $output=Join-Path $dir 'sampling.json';$request=New-MaintenanceManualSamplingRequest -Plan $plan -Output $output
        $actual=Read-MaintenanceJson -Path $output -ExpectedSha256 $request.requestSha256
        Assert-Equal -Expected 'AQID' -Actual $actual.planProto -Because '采样不重拍P0 Plan或重写native位置'
        Assert-Equal -Expected $run -Actual $actual.runId -Because '采样必须保留原run'
        Assert-Equal -Expected 1 -Actual @($actual.evidence.nonSqlClosure.sourceEvidence).Count -Because '只pin实际manual报告，不修改IssuerClosure'
        $report.assignments[0].position='11';[IO.File]::WriteAllText($reportFile,($report|ConvertTo-Json -Depth 30 -Compress))
        $message='';try{$null=New-MaintenanceManualSamplingRequest -Plan $plan -Output (Join-Path $dir 'bad.json')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '未消费到固定尾部' -Because '停止且join仍不等于已处理完manual固定尾部'
        Assert-True -Condition (-not [IO.File]::Exists((Join-Path $dir 'bad.json'))) -Because '坏原件不创建可投递采样请求'
    }


    Test-Case 'finite NORMAL只接受固定业务入口且不能覆盖配置或伪造维护runtime' {
        $config=Join-Path $taskRoot 'finite-config.yaml'
        $tool=@{kind='assetopfix';arguments=@('-op-id','123','-decision','abort')}
        $args=@(Get-NormalFiniteToolArguments -Tool $tool -RuntimeConfig $config)
        Assert-Equal -Expected '-f' -Actual $args[0] -Because '工具实际配置由选中NORMAL boot绑定'
        Assert-Equal -Expected $config -Actual $args[1] -Because '不沿用工作目录共享配置'
        Assert-Equal -Expected 'resolve' -Actual $args[2] -Because '有限写工具固定真实业务子命令'
        foreach($override in @('-f','--config=other','-dry-run=false','-maintenance-drain-plan=other')){
            $tool.arguments=@($override);$message='';try{$null=Get-NormalFiniteToolArguments -Tool $tool -RuntimeConfig $config}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern '不能覆盖' -Because 'argv不准改固定配置/模式或自带P6原件'
        }
        foreach($kind in @('unknown','debug_import')){
            $tool.kind=$kind;$tool.arguments=@();$message='';try{$null=Get-NormalFiniteToolArguments -Tool $tool -RuntimeConfig $config}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern '具名工具' -Because '有限入口不是任意命令执行器'
        }
    }


    Test-Case '不可变JSON保持单元素和空数组形状' {
        foreach($size in @(0,1)){
            $values=[object[]]@();if($size -eq 1){$values=[object[]]@([ordered]@{value='one'})}
            $file=Join-Path $taskRoot ('array-'+$size+'.json')
            $null=Write-MaintenanceImmutableJson -Path $file -Value $values
            $document=[Text.Json.JsonDocument]::Parse([IO.File]::ReadAllText($file))
            try{
                Assert-Equal -Expected ([Text.Json.JsonValueKind]::Array) -Actual $document.RootElement.ValueKind -Because '正式pins/targets列表不能因数量0/1退化成无JSON或对象'
                Assert-Equal -Expected $size -Actual $document.RootElement.GetArrayLength() -Because '列表成员数精确保留'
            }finally{$document.Dispose()}
        }
    }

    Test-Case 'Redis DB规范化只允许明确非负int32整数' {
        Assert-Equal -Expected 0 -Actual (Get-MaintenanceRedisDb -Target @{db='0'}) -Because '既有字符串整数入口输出规范int32'
        foreach($bad in @(@{},@{db=$null},@{db=-1},@{db=1.5},@{db='01'},@{db='1.1'},@{db='2147483648'},@{db='not-a-db'})){
            $message='';try{$null=Get-MaintenanceRedisDb -Target $bad}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern '整数' -Because '缺失/小数/溢出不能被cast静默取整或当0'
        }
    }

    Test-Case 'Redis nonce绑定唯一run且坏类型/值/TTL均拒绝' {
        $plan=@{operationId=('1'*32)};$target=@{maintenanceNonce=[Convert]::ToBase64String([Convert]::FromHexString('2'*32))}
        $identity=Get-MaintenanceRedisNonceIdentity -Plan $plan -Target $target
        Assert-Equal -Expected ('sql:writer:maintenance:'+('1'*32)+':nonce') -Actual $identity.key -Because '键只由正式run派生，不能输入替代键'
        $readOriginal=(Get-Command Invoke-MaintenanceRedisRead).ScriptBlock
        try{
            function Invoke-MaintenanceRedisRead {
                param($Session,[string[]]$Arguments)
                switch($Arguments[0]){
                    'TYPE' {return [pscustomobject]@{Kind='text';Text=$Session.type}}
                    'GET' {return [pscustomobject]@{Kind='bulk';Data=[Text.Encoding]::ASCII.GetBytes($Session.value)}}
                    'PTTL' {return [pscustomobject]@{Kind='integer';Number=$Session.pttl}}
                    default {throw '测试发现不应发出的命令'}
                }
            }
            $session=@{type='string';value=('2'*32);pttl=-1}
            Assert-Equal -Expected $target.maintenanceNonce -Actual (Get-MaintenanceRedisNonceObservation -Session $session -Identity $identity).value -Because '返回实际GET解析出的原16bytes'
            foreach($bad in @(@{type='none';value=('2'*32);pttl=-2},@{type='string';value=('3'*32);pttl=-1},@{type='string';value=('2'*32);pttl=1000})){
                $message='';try{$null=Get-MaintenanceRedisNonceObservation -Session $bad -Identity $identity}catch{$message=$_.Exception.Message}
                Assert-Match -Text $message -Pattern '禁止补种' -Because '缺失/替换/过期都不能解释为空库已排空'
            }
        }finally{Set-Item -Path Function:Invoke-MaintenanceRedisRead -Value $readOriginal}
    }

    Test-Case 'Redis Cluster目标：HashSlot与Go金样一致，nonce键落在本节点slot，拓扑不全或不稳定都HOLD' {
        foreach($sample in @(@('123456789',12739),@('{user1000}.following',3443),@('foo{}{bar}',8363),@('foo{{bar}}zap',4015),@('foo{bar}{zap}',5061),@('player:{42}:owner_epoch',8000))){
            Assert-Equal -Expected $sample[1] -Actual (Get-MaintenanceRedisKeySlot -Key $sample[0]) -Because 'CRC16金样与go/shared/redisslot、C++ redis_slot.h逐字一致'
        }
        $nodes=@(
            'aaa 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 7000',
            'bbb 10.0.0.2:6379@16379 master - 0 0 2 connected 5461-6999 7001-10922',
            'ccc 10.0.0.3:6379@16379 master - 0 0 3 connected 10923-16383',
            'ddd 10.0.0.4:6379@16379 slave aaa 0 0 1 connected'
        ) -join "`n"
        $view=ConvertFrom-MaintenanceClusterNodes -Text $nodes
        Assert-Equal -Expected 'aaa' -Actual $view.NodeId -Because '本节点身份取myself行'
        Assert-Equal -Expected 'aaa|0-5460,7000;bbb|5461-6999,7001-10922;ccc|10923-16383' -Actual $view.Fingerprint -Because '指纹只含持有slot的主节点，按节点ID排序，格式与Go parseClusterFingerprint一致'
        $base='sql:writer:maintenance:'+('1'*32)+':nonce'
        $key=Get-MaintenanceClusterNonceKey -BaseKey $base -Ranges $view.Ranges
        $slot=Get-MaintenanceRedisKeySlot -Key $key
        Assert-True -Condition (($slot -le 5460) -or ($slot -eq 7000)) -Because 'nonce必须落在本节点持有的slot上，否则SET回MOVED'
        Assert-Equal -Expected $key -Actual (Get-MaintenanceClusterNonceKey -BaseKey $base -Ranges $view.Ranges) -Because 'P0与P4按同一拓扑算出同一把键'
        foreach($bad in @(
            @{Why='本节点是副本';Text=($nodes -replace 'myself,master','master' -replace ' slave aaa',' myself,slave aaa');Pattern='不是持有slot的主节点'},
            @{Why='slot没有全覆盖';Text=($nodes -replace '10923-16383','10923-16000');Pattern='合计'},
            @{Why='主节点失联';Text=($nodes -replace 'bbb 10.0.0.2:6379@16379 master','bbb 10.0.0.2:6379@16379 master,fail');Pattern='状态不稳定'},
            @{Why='有逐键迁移';Text=($nodes -replace '10923-16383','10923-16383 [5->-bbb]');Pattern='逐键迁移'}
        )){
            $message='';try{$null=ConvertFrom-MaintenanceClusterNodes -Text $bad.Text}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern $bad.Pattern -Because "$($bad.Why)时不能断言扫完了玩家域"
        }
        $message='';try{$null=Invoke-MaintenanceRedisRead -Session @{} -Arguments @('CLUSTER','RESET')}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern 'CLUSTER NODES' -Because '维护collector不能发别的CLUSTER子命令'
    }

    Test-Case 'Redis P0先固定attempt且清库恢复绝不SET，P4带真实首尾时窗' {
        $dir=Join-Path $taskRoot 'redis-nonce';$null=[IO.Directory]::CreateDirectory($dir)
        $scanDir=Join-Path $taskRoot 'redis-scan';$null=[IO.Directory]::CreateDirectory($scanDir)
        $target=@{role='core';address='127.0.0.1:6379';db='0';serverRunId=[Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes('a'*40));maintenanceNonce=[Convert]::ToBase64String([Convert]::FromHexString('2'*32));zones=@(101);observations=@()}
        $plan=@{operationId=('1'*32);redisTargets=@($target)}
        $savedFunctions=@{};foreach($name in @('Open-MaintenanceRedis','Get-MaintenanceRedisRunId','Get-MaintenanceRedisClusterView','Assert-MaintenanceRedisDeadline','Send-MaintenanceRedisCommand','Invoke-MaintenanceRedisRead')){$savedFunctions[$name]=(Get-Command $name).ScriptBlock}
        $script:nonceFixture=@{value=$null;sets=0;attempt=(Join-Path $dir 'redis-nonce-seed.attempt.json');serverRunId=$target.serverRunId}
        try{
            function Open-MaintenanceRedis {
                param($Target,$Clock)
                $client=[pscustomobject]@{};$client|Add-Member -MemberType ScriptMethod -Name Dispose -Value {}
                return [pscustomobject]@{Client=$client;Stream=[IO.MemoryStream]::new();Clock=$Clock}
            }
            function Assert-MaintenanceRedisDeadline {param($Session)}
            function Get-MaintenanceRedisRunId {param($Session) return $script:nonceFixture.serverRunId}
            function Get-MaintenanceRedisClusterView {param($Session) return $null}
            function Send-MaintenanceRedisCommand {
                param($Session,[string[]]$Arguments)
                if($Arguments.Count -ne 4 -or $Arguments[0] -cne 'SET' -or $Arguments[3] -cne 'NX' -or -not [IO.File]::Exists($script:nonceFixture.attempt)){throw 'SET必须在固定attempt后且只能NX'}
                $script:nonceFixture.sets++;$script:nonceFixture.value=$Arguments[2]
                return [pscustomobject]@{Kind='text';Text='OK'}
            }
            function Invoke-MaintenanceRedisRead {
                param($Session,[string[]]$Arguments)
                switch($Arguments[0]){
                    'TYPE' {return [pscustomobject]@{Kind='text';Text=$(if($null -eq $script:nonceFixture.value){'none'}else{'string'})}}
                    'GET' {return [pscustomobject]@{Kind='bulk';Data=[Text.Encoding]::ASCII.GetBytes($script:nonceFixture.value)}}
                    'PTTL' {return [pscustomobject]@{Kind='integer';Number=-1}}
                    'SCAN' {return [pscustomobject]@{Kind='array';Items=@([pscustomobject]@{Kind='text';Text='0'},[pscustomobject]@{Kind='array';Items=@()})}}
                    default {throw '测试发现非预期命令'}
                }
            }
            $pins=@(Initialize-MaintenanceRedisNonces -Plan $plan -OutputDirectory $dir)
            Assert-Equal -Expected 1 -Actual $script:nonceFixture.sets -Because 'P0仅一次NX种入'
            $baseline=Read-MaintenanceJson -Path $pins[0].absolutePath
            $baselineDocument=[Text.Json.JsonDocument]::Parse([IO.File]::ReadAllText($pins[0].absolutePath))
            try{Assert-Equal -Expected ([Text.Json.JsonValueKind]::Number) -Actual ($baselineDocument.RootElement.GetProperty('db').ValueKind) -Because '实际SELECT使用的规范DB输出JSON数字，不透传字符串'}finally{$baselineDocument.Dispose()}
            $pinsFile=Join-Path $dir 'redis-baseline-pins.protojson';$pinsBefore=Get-MaintenanceSha256 $pinsFile
            $pinsDocument=[Text.Json.JsonDocument]::Parse([IO.File]::ReadAllText($pinsFile))
            try{Assert-Equal -Expected ([Text.Json.JsonValueKind]::Array) -Actual $pinsDocument.RootElement.ValueKind -Because '单物理Redis基线仍是pins数组'}finally{$pinsDocument.Dispose()}
            Assert-Equal -Expected 'SQL_DRAIN_REDIS_BASELINE_V1' -Actual $baseline.schema -Because 'P0原件只进入Plan.SourcePins，不冒充SCAN'
            $null=Initialize-MaintenanceRedisNonces -Plan $plan -OutputDirectory $dir
            Assert-Equal -Expected 1 -Actual $script:nonceFixture.sets -Because '已有attempt恢复只能读原nonce'
            Assert-Equal -Expected $pinsBefore -Actual (Get-MaintenanceSha256 $pinsFile) -Because '正常恢复保原pins文件同字节，不覆盖或重新CreateNew'
            $null=Collect-MaintenanceRedisClosure -Plan $plan -OutputDirectory $scanDir
            $scan=Read-MaintenanceJson -Path (Join-Path $scanDir 'redis-0.scan.v2.json')
            $scanDocument=[Text.Json.JsonDocument]::Parse([IO.File]::ReadAllText((Join-Path $scanDir 'redis-0.scan.v2.json')))
            try{Assert-Equal -Expected ([Text.Json.JsonValueKind]::Number) -Actual ($scanDocument.RootElement.GetProperty('db').ValueKind) -Because 'SCAN原件DB同样为实际规范整数'}finally{$scanDocument.Dispose()}
            $targetsDocument=[Text.Json.JsonDocument]::Parse([IO.File]::ReadAllText((Join-Path $scanDir 'redis-targets.protojson')))
            try{Assert-Equal -Expected ([Text.Json.JsonValueKind]::Array) -Actual $targetsDocument.RootElement.ValueKind -Because '单物理Redis闭合仍是targets数组'}finally{$targetsDocument.Dispose()}
            Assert-True -Condition ([long]$scan.observed_start_ms -gt 0 -and [long]$scan.observed_end_ms -ge [long]$scan.observed_start_ms) -Because 'SCAN输出实际首尾时刻供正式60秒窗口验收'
            Assert-Equal -Expected $target.maintenanceNonce -Actual $scan.nonce_before -Because 'SCAN首部来自GET'
            Assert-Equal -Expected $target.maintenanceNonce -Actual $scan.nonce_after -Because 'SCAN尾部必须再GET'
            Assert-Equal -Expected (-1) -Actual $scan.nonce_pttl_after_ms -Because '实际nonce无TTL'
            $script:nonceFixture.value=$null
            $message='';try{$null=Initialize-MaintenanceRedisNonces -Plan $plan -OutputDirectory $dir}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern '禁止补种' -Because '同进程FLUSHDB后恢复不能重种旧nonce'
            Assert-Equal -Expected 1 -Actual $script:nonceFixture.sets -Because '清库后零SET副作用'
            $message='';try{$null=Collect-MaintenanceRedisClosure -Plan $plan -OutputDirectory $scanDir}catch{$message=$_.Exception.Message}
            Assert-Match -Text $message -Pattern '禁止补种' -Because 'P4也不得把被清空的目标认作闭合'
        }finally{foreach($name in $savedFunctions.Keys){Set-Item -Path ('Function:'+$name) -Value $savedFunctions[$name]};Remove-Variable -Name nonceFixture -Scope Script}
    }

} finally {
    [Environment]::SetEnvironmentVariable('MMORPG_PUBLICATION_FENCE_DIR',$savedFence,'Process')
    $full=[IO.Path]::GetFullPath($taskRoot);$allowed=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
    if(-not $full.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $full) -notlike 'mmorpg-drain-contract-*'){throw '拒绝清理夹具范围外目录'}
    Remove-Item -LiteralPath $full -Recurse -Force
}
exit (Complete-TestRun -SuiteName '永久排空与权限renderer纯合同')
