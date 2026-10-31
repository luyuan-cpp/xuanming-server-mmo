#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 的 merge-zone 系列命令转发契约测试(合服 / 撤销 / 审计,与 tools/merge_zone 现有的三个模式一一对应)。
.DESCRIPTION
    背景:dev_tools.ps1 是 runbook 里的合服入口,它漏转一个 flag 不会报任何错,只会让 tools/merge_zone
    静默用默认值 —— 历史上审计恒绿、撤销从 ps1 跑不起来、没部署 guild 的环境只能绕过 ps1 手敲 go run,
    都是这一类(server-merge-gap-fixes.md B4 / B6,player-storage-placement.md §12 A12)。

    做法:只从 AST 加载参数组装函数,参数默认值从 dev_tools.ps1 的 param 块现取(不在测试里抄第二份),
    仓库根换成临时目录里的假 yaml。不起 go、不连任何 Redis / MySQL / Kafka。
    退出码透传一条在子进程里 `pwsh -File` 真跑 Invoke-MergeZoneGo,`go` 与编译产物均为假替身。
.EXAMPLE
    pwsh -File tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$devToolsPath = Join-Path $PSScriptRoot '../dev_tools.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($devToolsPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$devToolsSource = Get-Content -LiteralPath $devToolsPath -Raw

$functionNames = @('Get-MergeMappingRedis', 'Get-MergeZoneArgs', 'Get-MergeRouteDbArgs', 'Resolve-MergeFullPath', 'Get-MergeZoneCommandArgs', 'Get-K8sRouteToolArgs')
$loaded = @()
foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $functionNames }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
    $loaded += $definition.Name
}
foreach ($name in $functionNames) {
    if ($name -notin $loaded) { throw "dev_tools.ps1 里找不到函数 $name(契约测试的前提被改掉了)" }
}

# 被测函数按 PowerShell 的动态作用域读 $Merge* / $DryRun / $VerifyMerged / $ScriptDir,
# 这里把它们放在本脚本作用域。默认值一律取 dev_tools.ps1 param 块的真实默认值。
$mergeParams = @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -match '^(Merge|VerifyMerged$|DryRun$)' })
# -RouteDbDsn:-MergeZoneRouteDbDsn 在 param 块里没有缺省值(必填)。别的用例钉的是别的转发,
# 复位时缺省填一个假 DSN 让它们照常组装;专测必填的用例显式传 -RouteDbDsn ''。
function Reset-MergeParams([string]$RouteDbDsn = 'route:secret@tcp(10.0.0.9:3306)/testdb') {
    foreach ($p in $mergeParams) {
        $value = if ($null -ne $p.DefaultValue) { & ([scriptblock]::Create($p.DefaultValue.Extent.Text)) }
                 elseif ($p.StaticType -eq [switch]) { $false }
                 else { $null }
        Set-Variable -Name $p.Name.VariablePath.UserPath -Value $value -Scope Script
    }
    $script:MergeZoneRouteDbDsn = $RouteDbDsn
}

# k8s-route-tool 的转发读 $ZoneName / $NamespacePrefix / $KubeContext / $KubeConfig / $DryRun / $RouteTool*,默认值同样现取。
$routeToolParamNames = @('ZoneName', 'NamespacePrefix', 'KubeContext', 'KubeConfig', 'DryRun', 'RouteToolArgs', 'RouteToolReportDir', 'RouteToolTimeoutSeconds', 'RouteToolImage')
function Reset-RouteToolParams {
    foreach ($p in @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -in $routeToolParamNames })) {
        $value = if ($null -ne $p.DefaultValue) { & ([scriptblock]::Create($p.DefaultValue.Extent.Text)) }
                 elseif ($p.StaticType -eq [switch]) { $false }
                 else { $null }
        Set-Variable -Name $p.Name.VariablePath.UserPath -Value $value -Scope Script
    }
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

# 取 flag 后面紧跟的值;flag 不在参数里返回 $null。
function Get-ArgValue($ArgList, [string]$Flag) {
    $s = @($ArgList | ForEach-Object { [string]$_ })
    $i = [array]::IndexOf($s, $Flag)
    if ($i -lt 0 -or $i + 1 -ge $s.Count) { return $null }
    return $s[$i + 1]
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-merge-zone-contract-tests-' + [guid]::NewGuid().ToString('N'))
$script:ScriptDir = Join-Path $testRoot 'tools/scripts'

function Get-CmdArgs([string]$MergeCommand) {
    return @(Get-MergeZoneCommandArgs -MergeCommand $MergeCommand -RepoRoot $testRoot)
}

try {
    New-Item -ItemType Directory -Path $script:ScriptDir -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $testRoot 'go/data_service/etc') -Force | Out-Null
    # 与真实 data_service.yaml 同形:MappingRedis 不写 DB 键(go-zero RedisConf 没有 DB 字段)。
    Set-Content -LiteralPath (Join-Path $testRoot 'go/data_service/etc/data_service.yaml') -Encoding utf8 -Value @(
        'MappingRedis:', '  Host: 10.9.9.9:6379', '  Type: node')
    $manifestFile = Join-Path $testRoot 'merge_1_to_2_fixture.json'
    Set-Content -LiteralPath $manifestFile -Encoding utf8 -Value '{}'
    $missingManifest = Join-Path $testRoot 'no_such_manifest.json'

    Test-Case '入口:ValidateSet、分发与帮助只有工具现有的三个模式;工具已删的命令与参数不再接受' {
        $commandParam = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Command' }
        $validateSet = $commandParam.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' }
        $allowed = @($validateSet.PositionalArguments | ForEach-Object { $_.Value })
        $paramNames = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        foreach ($cmd in @('merge-zone', 'merge-zone-audit', 'merge-zone-unmerge')) {
            Assert-True ($cmd -in $allowed) "-Command 的 ValidateSet 必须含 $cmd"
            Assert-Match $devToolsSource ([regex]::Escape("Invoke-MergeZoneGo -MergeCommand `"$cmd`"")) "$cmd 必须经 Invoke-MergeZoneGo 分发(同一份参数组装)"
            Assert-Match $devToolsSource ("-Command " + [regex]::Escape($cmd) + '\s') "帮助文本必须列出 $cmd"
        }
        foreach ($cmd in @('merge-zone-relocate', 'merge-zone-relocate-abort', 'merge-zone-pin-placement', 'merge-zone-storage-audit', 'merge-zone-capability-check')) {
            Assert-True (-not ($cmd -in $allowed)) "tools/merge_zone 已删掉对应模式,-Command 不再接受 $cmd"
        }
        foreach ($name in @('MergeSkipGuildMySql', 'MergeSkipGuildRank', 'MergeSkipTradeMySql', 'MergeZoneRouteDbDsn', 'MergeZoneWaitAofLocal', 'MergeZoneWaitAofReplicas', 'MergeZoneWaitAofTimeoutMs')) {
            Assert-True ($name -in $paramNames) "param 块必须有 -$name"
        }
        foreach ($name in @('MergeZoneRouteSource', 'MergeZoneTakeOverFence', 'MergePlayerRowsMode', 'MergeDbCapabilityZones', 'MergePinPlacementZone', 'MergeBackfillZone',
                            'MergeRelocateSourceStorage', 'MergeRelocateTargetStorage', 'MergeRelocatePlayerIds', 'MergeRelocateBatchSize', 'MergeRelocateLockWait', 'MergeAuditStorage',
                            'MergeKafkaConsumerGroupsCmd', 'MergeKafkaBootstrap', 'MergeKafkaGroup', 'MergeKafkaTopicGeneration', 'MergeAssumeKafkaDrained',
                            'MergeMigratePlayerBlobs', 'MergeClearSourceHotState', 'MergeSourceDataRedis', 'MergeTargetDataRedis', 'MergeDataRedisPassword',
                            'MergeSourceDataRedisDB', 'MergeTargetDataRedisDB', 'MergeExpectedSrcPlayers', 'MergeAllowEmptySource', 'MergeTableListJson')) {
            Assert-True (-not ($name -in $paramNames)) "-$name 对应的 tools/merge_zone 旗标已删除"
        }
    }

    Test-Case '公共参数:mapping 取 data_service.yaml 的 Host、DB 缺省 0' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal '10.9.9.9:6379' (Get-ArgValue $a '-mapping-redis-addr') 'mapping 地址必须现读 data_service.yaml'
        Assert-Equal '0' (Get-ArgValue $a '-mapping-redis-db') 'yaml 里没有 DB 键时 mapping DB 必须兜底为 0(不是 15)'
        Assert-NotMatch $devToolsSource '今天是 15' '参数说明里不得再把 mapping DB 写成 15(gap-fixes B6)'
    }

    # 工具用 flag 包解析:多一个未声明的旗标,整条命令在做任何事之前退出。旗标清单从 main.go 现读,不在测试里抄第二份。
    Test-Case '转发的旗标都由 tools/merge_zone/main.go 声明(开关全打开时逐个核对)' {
        $toolSource = Get-Content -LiteralPath (Join-Path $PSScriptRoot '../../merge_zone/main.go') -Raw
        $declared = @([regex]::Matches($toolSource, 'flag\.[A-Za-z0-9]+\((?:&[A-Za-z0-9_.]+,\s*)?"(?<n>[a-z0-9-]+)"') | ForEach-Object { $_.Groups['n'].Value })
        Assert-True ('route-db-dsn' -in $declared -and 'source-zone' -in $declared) '夹具前提:main.go 的旗标声明写法认得出来(换了写法要同步本用例的正则)'
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeManifestPath = $manifestFile
        $script:MergeRedisPassword = 'x'; $script:MergeMappingRedisPassword = 'x'; $script:MergeNoticeRedisAddr = 'h:1'; $script:MergeSceneRedisAddr = 'h:1'
        $script:MergeEtcdEndpoints = 'http://127.0.0.1:2379'; $script:MergeSkipTradeMySql = $true; $script:MergeSkipGuildMySql = $true; $script:MergeSkipGuildRank = $true
        $script:MergeZoneWaitAofLocal = 1; $script:MergeZoneWaitAofReplicas = 1; $script:MergeZoneWaitAofTimeoutMs = 1200; $script:VerifyMerged = $true
        foreach ($intentDry in @($false, $true)) {
            $script:DryRun = $intentDry
            foreach ($cmd in @('merge-zone', 'merge-zone-unmerge', 'merge-zone-audit')) {
                foreach ($token in @(Get-CmdArgs $cmd | ForEach-Object { [string]$_ } | Where-Object { $_ -cmatch '^-[a-z]' })) {
                    Assert-True ($token.Substring(1) -in $declared) "$cmd 转发的 $token 不是 tools/merge_zone/main.go 声明的旗标"
                }
            }
        }
    }

    Test-Case '公共参数:-MergeEtcdEndpoints 原样转发为 -etcd-endpoints,没给就不转发(缺参数由 merge_zone 在任何写入之前拒绝)' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        Assert-True (-not ((Get-CmdArgs 'merge-zone') -contains '-etcd-endpoints')) '没给 -MergeEtcdEndpoints 时不得转发空值'
        $script:MergeEtcdEndpoints = 'http://127.0.0.1:2379'
        foreach ($cmd in @('merge-zone', 'merge-zone-audit')) {
            Assert-Equal 'http://127.0.0.1:2379' (Get-ArgValue (Get-CmdArgs $cmd) '-etcd-endpoints') "${cmd}:-MergeEtcdEndpoints 必须原样转发(preflight P2 与审计要 etcd 注册表的正证,tools/merge_zone/scene_registry.go)"
        }
        $script:MergeEtcdEndpoints = ''
    }

    # 源码断言:dev_tools.ps1 与回滚脚本 / kafka_offset_reset.ps1 的透传口径(2026-10-30:存盘不再经 Kafka,相关参数全部删除)。
    # 分支文本按 switch 分支的起止锚点切出,只断本分支,不受参数块注释影响。
    function Get-DevToolsBranchText([string]$Branch, [string]$NextBranch) {
        $start = $devToolsSource.IndexOf("    `"$Branch`" {")
        $end = $devToolsSource.IndexOf("    `"$NextBranch`" {", [Math]::Max($start, 0))
        if ($start -lt 0 -or $end -le $start) { throw "dev_tools.ps1 里找不到 $Branch 分支(契约测试的前提被改掉了)" }
        return $devToolsSource.Substring($start, $end - $start)
    }

    Test-Case 'k8s-zone-rollback 分支:不转发任何 Kafka 参数(回滚脚本不再声明它们,再转发会以「找不到参数」失败)' {
        $branch = Get-DevToolsBranchText 'k8s-zone-rollback' 'dev-robot-zones'
        Assert-NotMatch $branch '\$rbArgs\.(Kafka|QuiesceSeconds|BarrierProtocol)|KafkaBootstrap\s*=|KafkaTopic\s*=' '回滚脚本已不声明 Kafka、静默窗口与屏障协议参数'
        Assert-NotMatch $branch 'DbConsumerGroup' '回滚脚本的 Step 6 不再传消费组'
    }

    Test-Case 'kafka-offset-reset 分支:hashtable splatting,不再转发 -RetireGroup 与 -FinalizeHandoff' {
        $branch = Get-DevToolsBranchText 'kafka-offset-reset' 'k8s-zone-rollback'
        Assert-NotMatch $branch '\$kArgs\s*=\s*@\(' '数组 splatting 按位置绑定,topic 名会撞上 [int]$Partitions;必须用 hashtable'
        Assert-Match $branch '\$kArgs\s*=\s*@\{' '必须用 hashtable splatting'
        Assert-NotMatch $branch 'RetireGroup|FinalizeHandoff' 'kafka_offset_reset.ps1 已删除这两个参数'
    }

    Test-Case '已删除的参数:param 块与 Invoke-K8sDeploy 都不再有它们(k8s_deploy.ps1 已不声明)' {
        $paramNames = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        foreach ($name in @('DbConsumerGroup', 'MatchRatingSnapshotPublish', 'KafkaRetireGroup', 'KafkaFinalizeHandoff', 'RollbackKafkaTopic', 'RollbackKafkaGroup',
                'RollbackKafkaLegacyGroup', 'RollbackKafkaDrainTimeoutSec', 'RollbackQuiesceSeconds', 'RollbackBarrierProtocol')) {
            Assert-True (-not ($name -in $paramNames)) "-$name 已删除"
        }
        $deploy = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-K8sDeploy' }, $true)
        Assert-NotMatch $deploy.Extent.Text 'DbConsumerGroup|MatchRatingSnapshotPublish' 'k8s_deploy.ps1 已不声明这两个参数,再透传会以「找不到参数」失败'
    }

    Test-Case '帮会跳过开关:默认不转发,给了就对合服 / 撤销 / 审计同口径转发' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeManifestPath = $manifestFile
        foreach ($cmd in @('merge-zone', 'merge-zone-unmerge', 'merge-zone-audit')) {
            $a = Get-CmdArgs $cmd
            Assert-True (-not ($a -contains '-skip-guild-mysql')) "$cmd 默认不得跳过 guild MySQL 步骤"
            Assert-True (-not ($a -contains '-skip-guild-rank')) "$cmd 默认不得跳过 guild 榜步骤"
        }
        $script:MergeSkipGuildMySql = $true; $script:MergeSkipGuildRank = $true
        foreach ($cmd in @('merge-zone', 'merge-zone-unmerge', 'merge-zone-audit')) {
            $a = Get-CmdArgs $cmd
            Assert-True ($a -contains '-skip-guild-mysql') "$cmd 必须转发 -skip-guild-mysql"
            Assert-True ($a -contains '-skip-guild-rank') "$cmd 必须转发 -skip-guild-rank(审计只在两个都给时才记 SKIPPED)"
        }
    }

    Test-Case 'merge-zone:两个 zone 必填;缺省清单落在仓库根、绝对路径;不带 -DryRun 写意图是 -apply,-DryRun 只转发 -dry-run' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone' }) 'MergeSourceZone' '缺源区必须点名报错'
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal '1' (Get-ArgValue $a '-source-zone') '源区'
        Assert-Equal '2' (Get-ArgValue $a '-target-zone') '目标区'
        Assert-Equal '-apply' ([string]$a[-1]) '不带 -DryRun 时写意图是 -apply'
        Assert-Match (Get-ArgValue $a '-manifest-path') ('^' + [regex]::Escape($testRoot) + '[\\/]merge_1_to_2_\d{8}T\d{6}Z\.json$') '默认清单落在仓库根、绝对路径'
        $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone'
        Assert-True ($a -contains '-dry-run' -and -not ($a -contains '-apply')) '-DryRun 只转发 -dry-run'
    }

    Test-Case 'merge-zone-unmerge:要求清单存在;zone 从清单读,不转发 -source-zone / -target-zone' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-unmerge' }) 'MergeManifestPath' '不给清单必须点名报错'
        $script:MergeManifestPath = $missingManifest
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-unmerge' }) 'manifest not found' '清单不存在必须拒绝'

        $script:MergeManifestPath = $manifestFile
        $a = Get-CmdArgs 'merge-zone-unmerge'
        Assert-Equal 'unmerge' (Get-ArgValue $a '-mode') '撤销模式'
        Assert-Equal $manifestFile (Get-ArgValue $a '-manifest-path') '清单绝对路径'
        Assert-Equal $null (Get-ArgValue $a '-source-zone') 'zone 从清单读:工具的约定是撤销的对象必须与当初合的一致'
        Assert-Equal '-apply' ([string]$a[-1]) '写意图'
    }

    Test-Case 'merge-zone-audit -VerifyMerged:必须带清单并转发 -manifest-path;普通审计不带写意图' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        $a = Get-CmdArgs 'merge-zone-audit'
        Assert-Equal 'audit' (Get-ArgValue $a '-mode') '审计模式'
        Assert-True (-not ($a -contains '-apply' -or $a -contains '-dry-run')) '审计只读,不带写意图'
        Assert-True (-not ($a -contains '-verify-merged')) '默认是合服前审计'

        $script:VerifyMerged = $true
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-audit' }) 'MergeManifestPath' '合服后复核逐 id 核对清单,没有清单必须点名报错'
        $script:MergeManifestPath = $manifestFile
        $a = Get-CmdArgs 'merge-zone-audit'
        Assert-True ($a -contains '-verify-merged') '转发复核开关'
        Assert-Equal $manifestFile (Get-ArgValue $a '-manifest-path') '复核必须转发清单'
    }

    # ── 路由库(工具的 checkRouteFlags:三个模式都读路由库,-route-db-dsn 必填)────────
    $routeCommands = [ordered]@{
        'merge-zone'         = { $script:MergeSourceZone = 1; $script:MergeTargetZone = 2 }
        'merge-zone-unmerge' = { $script:MergeManifestPath = $manifestFile }
        'merge-zone-audit'   = { $script:MergeSourceZone = 1; $script:MergeTargetZone = 2 }
    }

    Test-Case '路由库:-MergeZoneRouteDbDsn 没有缺省值,三个命令缺了都点名拒绝;给了原样转发;WAITAOF 只在给出时转发;DSN 不打到控制台' {
        $param = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'MergeZoneRouteDbDsn' }
        Assert-True ($null -ne $param) 'param 块必须有 -MergeZoneRouteDbDsn'
        Assert-Equal '' $param.DefaultValue.Value '缺省必须为空(必填),不能偷偷给一个库'
        $dsn = 'route:secret@tcp(10.0.0.9:3306)/testdb'
        foreach ($label in $routeCommands.Keys) {
            Reset-MergeParams -RouteDbDsn ''
            & $routeCommands[$label]
            Assert-Match (Get-ThrownMessage { Get-CmdArgs $label }) 'MergeZoneRouteDbDsn' "$label 缺 -MergeZoneRouteDbDsn 必须点名拒绝"

            Reset-MergeParams -RouteDbDsn $dsn
            & $routeCommands[$label]
            $a = Get-CmdArgs $label
            Assert-Equal $dsn (Get-ArgValue $a '-route-db-dsn') "$label 必须原样转发 -route-db-dsn"
            Assert-True (-not ($a -contains '-route-source')) "$label 不再转发 -route-source(工具已删,路由库是唯一真源)"
            foreach ($flag in @('-waitaof-local', '-waitaof-replicas', '-waitaof-timeout-ms')) {
                Assert-Equal $null (Get-ArgValue $a $flag) "$label 没给 $flag 对应的参数就不转发"
            }
            $script:MergeZoneWaitAofLocal = 1; $script:MergeZoneWaitAofReplicas = 1; $script:MergeZoneWaitAofTimeoutMs = 1200
            $a = Get-CmdArgs $label
            Assert-Equal '1' (Get-ArgValue $a '-waitaof-local') "$label 转发 -waitaof-local"
            Assert-Equal '1' (Get-ArgValue $a '-waitaof-replicas') "$label 转发 -waitaof-replicas(redis-ha 档)"
            Assert-Equal '1200' (Get-ArgValue $a '-waitaof-timeout-ms') "$label 转发 -waitaof-timeout-ms"
        }
        $fn = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Get-MergeRouteDbArgs' }, $true)
        Assert-NotMatch $fn.Extent.Text 'Write-Host' 'Get-MergeRouteDbArgs 不得 Write-Host(DSN 含口令)'
    }

    # ── k8s-route-tool(D1 第 1b-3 批,§13.4 第 3 条)───────────────────
    $routeToolPath = Join-Path $PSScriptRoot '../k8s_route_tool.ps1'
    $routeToolErrors = $null
    $routeToolAst = [System.Management.Automation.Language.Parser]::ParseFile($routeToolPath, [ref]$null, [ref]$routeToolErrors)
    if ($routeToolErrors.Count) { throw ($routeToolErrors | Out-String) }
    $routeToolParams = @($routeToolAst.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })

    Test-Case 'k8s-route-tool:入口、分发与帮助;hashtable 的键与 k8s_route_tool.ps1 的参数名逐字相同,留空不转发' {
        $commandParam = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Command' }
        $allowed = @(($commandParam.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' }).PositionalArguments | ForEach-Object { $_.Value })
        Assert-True ('k8s-route-tool' -in $allowed) '-Command 的 ValidateSet 必须含 k8s-route-tool'
        Assert-Match $devToolsSource '"k8s-route-tool"\s*\{\s*Invoke-K8sRouteTool\s*\}' 'k8s-route-tool 必须经 Invoke-K8sRouteTool 分发'
        Assert-Match $devToolsSource ('-Command ' + [regex]::Escape('k8s-route-tool') + '\s') '帮助文本必须列出 k8s-route-tool'
        $invoke = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-K8sRouteTool' }, $true)
        Assert-True ($null -ne $invoke) 'dev_tools.ps1 里必须有 Invoke-K8sRouteTool'
        Assert-Match $invoke.Extent.Text ([regex]::Escape('k8s_route_tool.ps1') + '"\)\s*@rtArgs') '必须 hashtable splatting 调 k8s_route_tool.ps1'
        Assert-Match $invoke.Extent.Text 'exit \$LASTEXITCODE' '必须以 route-tool 的退出码结束(pwsh -File 正常结束恒为 0)'
        foreach ($name in @('RouteToolArgs', 'RouteToolReportDir', 'RouteToolTimeoutSeconds', 'RouteToolImage')) {
            Assert-True ($name -in @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })) "param 块必须有 -$name"
        }

        Reset-RouteToolParams
        $script:ZoneName = 'z1'; $script:RouteToolArgs = @('audit', '-frozen-recovery')
        $h = Get-K8sRouteToolArgs
        Assert-Equal 'NamespacePrefix|RouteToolArgs|ZoneName' ((@($h.Keys) | Sort-Object) -join '|') '缺省只转发 zone / 子命令 / namespace 前缀,其余默认值只在 k8s_route_tool.ps1'
        Assert-Equal 'audit|-frozen-recovery' (@($h.RouteToolArgs) -join '|') '子命令与参数逐项原样转发(不拼成一个字符串)'
        Assert-Equal 'mmorpg-zone' $h.NamespacePrefix 'namespace 前缀取本脚本的 -NamespacePrefix'

        $script:RouteToolReportDir = 'C:\reports'; $script:RouteToolTimeoutSeconds = 4200; $script:RouteToolImage = 'ghcr.io/x/data-service:abc'
        $script:DryRun = $true; $script:KubeContext = 'kind-mmorpg'; $script:KubeConfig = 'C:\kube\config'
        $h = Get-K8sRouteToolArgs
        foreach ($key in @($h.Keys)) {
            Assert-True ($key -in $routeToolParams) "转发的键 $key 必须是 k8s_route_tool.ps1 的参数(逐字对齐,拼错会以「找不到参数」失败)"
        }
        Assert-Equal 'C:\reports' $h.ReportDir '-RouteToolReportDir → -ReportDir'
        Assert-Equal 4200 $h.TimeoutSeconds '-RouteToolTimeoutSeconds → -TimeoutSeconds'
        Assert-Equal 'ghcr.io/x/data-service:abc' $h.Image '-RouteToolImage → -Image'
        Assert-True ($h.DryRun -eq $true) '-DryRun 同名转发'
        Assert-Equal 'kind-mmorpg' $h.KubeContext '-KubeContext 同名转发'
        Assert-Equal 'C:\kube\config' $h.KubeConfig '-KubeConfig 同名转发'
        Reset-RouteToolParams
    }

    Test-Case 'k8s-route-tool:pwsh -File 下原样透传 k8s_route_tool.ps1 的退出码 0/1/2/3;没给子命令在调用之前以 2 拒绝;下游抛异常也是 2(不落到 pwsh 的 1)' {
        $names = @('Get-K8sRouteToolArgs', 'Invoke-K8sRouteTool')
        $defs = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $names }, $false))
        $childDir = Join-Path $testRoot 'route_tool_child'
        New-Item -ItemType Directory -Path $childDir -Force | Out-Null
        # 假 k8s_route_tool.ps1:参数块与真脚本同名(按真脚本的参数名现生成),记下收到的参数,按环境变量退出。
        $fakeParams = @($routeToolAst.ParamBlock.Parameters | ForEach-Object {
            $n = $_.Name.VariablePath.UserPath
            if ($_.StaticType -eq [switch]) { "[switch]`$$n" } elseif ($_.StaticType -eq [string[]]) { "[string[]]`$$n" } else { "`$$n" }
        })
        Set-Content -LiteralPath (Join-Path $childDir 'k8s_route_tool.ps1') -Encoding utf8 -Value @(
            ('param(' + ($fakeParams -join ', ') + ')'),
            # RT_TEST_THROW=1:模拟下游没兜住的异常(review B3-7 / OPS-5),包装层必须以 2 退出。
            'if ($env:RT_TEST_THROW -eq ''1'') { throw ''fake route tool internal failure'' }',
            'Set-Content -LiteralPath $env:RT_TEST_RECORD -Value @("zone=$ZoneName", "args=$($RouteToolArgs -join ''|'')", "dry=$DryRun")',
            'exit [int]$env:RT_TEST_EXIT')
        $childPath = Join-Path $childDir 'run_route_tool.ps1'
        $record = Join-Path $childDir 'record.txt'
        $writeChild = {
            param([string]$ArgsLiteral)
            Set-Content -LiteralPath $childPath -Encoding utf8 -Value (@('$ErrorActionPreference = ''Stop''') + @($defs | ForEach-Object { $_.Extent.Text }) + @(
                ('$ScriptDir = ''{0}''' -f $childDir.Replace("'", "''")), '$ZoneName = ''z1''', '$NamespacePrefix = ''mmorpg-zone''',
                ('$RouteToolArgs = {0}' -f $ArgsLiteral), '$RouteToolReportDir = ''''', '$RouteToolTimeoutSeconds = 0', '$RouteToolImage = ''''',
                '$DryRun = $false', '$KubeContext = ''''', '$KubeConfig = ''''', 'Invoke-K8sRouteTool', 'exit 99'))
        }
        $pwshPath = (Get-Process -Id $PID).Path
        $saved = @{ RT_TEST_RECORD = $env:RT_TEST_RECORD; RT_TEST_EXIT = $env:RT_TEST_EXIT; RT_TEST_THROW = $env:RT_TEST_THROW }
        try {
            $env:RT_TEST_THROW = ''
            & $writeChild "@('audit', '-frozen-recovery')"
            foreach ($code in @(0, 1, 2, 3)) {
                Remove-Item -LiteralPath $record -Force -ErrorAction SilentlyContinue
                $env:RT_TEST_RECORD = $record; $env:RT_TEST_EXIT = [string]$code
                $ErrorActionPreference = 'Continue'
                $out = & $pwshPath -NoProfile -NonInteractive -File $childPath 2>&1 | Out-String
                $got = $LASTEXITCODE
                $ErrorActionPreference = 'Stop'
                Assert-Equal $code $got "k8s_route_tool.ps1 退出 $code 时 dev_tools 必须原样退出 $code(99 = 漏了 exit)。输出: $out"
            }
            $rec = @(Get-Content -LiteralPath $record)
            Assert-Equal 'zone=z1' $rec[0] 'zone 原样转发'
            Assert-Equal 'args=audit|-frozen-recovery' $rec[1] '子命令数组逐项转发'
            Assert-Equal 'dry=False' $rec[2] '没给 -DryRun 不转发'

            Remove-Item -LiteralPath $record -Force -ErrorAction SilentlyContinue
            & $writeChild '@()'
            $ErrorActionPreference = 'Continue'
            $out = & $pwshPath -NoProfile -NonInteractive -File $childPath 2>&1 | Out-String
            $got = $LASTEXITCODE
            $ErrorActionPreference = 'Stop'
            Assert-Equal 2 $got "没给 -RouteToolArgs 必须在调用之前以 2 失败(本入口自己的失败;1 会与「完成但需要人工处理」撞号,99 = 漏了 exit)。输出: $out"
            Assert-Match $out 'RouteToolArgs' '提示必须点名 -RouteToolArgs'
            Assert-True (-not (Test-Path -LiteralPath $record)) '没给子命令时不得调用 k8s_route_tool.ps1'

            # 下游抛出未捕获的异常(真脚本的入口已兜成 2,这里防的是包装层这一跳):进程退出码必须是 2,不是 pwsh 的 1。
            & $writeChild "@('audit')"
            $env:RT_TEST_THROW = '1'; $env:RT_TEST_EXIT = '0'
            $ErrorActionPreference = 'Continue'
            $out = & $pwshPath -NoProfile -NonInteractive -File $childPath 2>&1 | Out-String
            $got = $LASTEXITCODE
            $ErrorActionPreference = 'Stop'
            Assert-Equal 2 $got "k8s_route_tool.ps1 抛异常时 dev_tools 必须以 2 退出(exit=$got)。输出: $out"
            Assert-Match $out 'fake route tool internal failure' '报错要带上下游的异常文本'
        }
        finally {
            foreach ($n in $saved.Keys) { [Environment]::SetEnvironmentVariable($n, $saved[$n], 'Process') }
        }
    }

    # 退出码透传:以前 Invoke-MergeZoneGo 是 `& go run ...` 且没有 exit,经 `pwsh -File` 调用时进程退出码
    # 恒为 0(合服 apply 失败 / 审计连不上库都报「成功」),交互读 $LASTEXITCODE 又被 go run 压成 1。
    # 这里在子进程里用 `pwsh -File` 真跑 Invoke-MergeZoneGo:`go` 换成假函数(记录调用、按环境变量决定
    # 编译成败,并把 -o 指向的「产物」写成一个按环境变量 exit 的脚本),产物路径经 New-MergeZoneExePath 接缝换掉。
    # 子脚本在 Invoke-MergeZoneGo 之后 `exit 99`:函数若正常返回(漏了 exit)就会被这个哨兵值抓住。
    Test-Case '退出码:pwsh -File 下原样透传工具的 0/1/2/4;编译失败给 2 且不运行工具;不走 go run' {
        $runnerNames = $functionNames + @('New-MergeZoneExePath', 'Invoke-MergeZoneGo')
        $definitions = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $runnerNames }, $false))
        foreach ($name in $runnerNames) {
            Assert-True ($name -in @($definitions | ForEach-Object { $_.Name })) "dev_tools.ps1 里必须有函数 $name"
        }
        $paramAssignments = foreach ($p in $mergeParams) {
            $rhs = if ($null -ne $p.DefaultValue) { $p.DefaultValue.Extent.Text } elseif ($p.StaticType -eq [switch]) { '$false' } else { '$null' }
            '${0} = {1}' -f $p.Name.VariablePath.UserPath, $rhs
        }
        $childDir = Join-Path $testRoot 'exit_code_child'
        New-Item -ItemType Directory -Path $childDir -Force | Out-Null
        New-Item -ItemType Directory -Path (Join-Path $testRoot 'tools/merge_zone') -Force | Out-Null
        $childPath = Join-Path $childDir 'run_invoke.ps1'
        $fakeGo = @'
function New-MergeZoneExePath { return $env:MZ_TEST_FAKE_EXE }
function go {
    Set-Content -LiteralPath $env:MZ_TEST_GO_RECORD -Value (@("cwd=" + $PWD.Path) + @($args | ForEach-Object { [string]$_ }))
    if ([int]$env:MZ_TEST_BUILD_EXIT -ne 0) { $global:LASTEXITCODE = [int]$env:MZ_TEST_BUILD_EXIT; return }
    $list = @($args | ForEach-Object { [string]$_ })
    $out = $list[[array]::IndexOf($list, '-o') + 1]
    Set-Content -LiteralPath $out -Value @(
        'Set-Content -LiteralPath $env:MZ_TEST_TOOL_RECORD -Value (@("cwd=" + $PWD.Path) + @($args | ForEach-Object { [string]$_ }))',
        'exit [int]$env:MZ_TEST_TOOL_EXIT')
    $global:LASTEXITCODE = 0
}
'@
        Set-Content -LiteralPath $childPath -Encoding utf8 -Value (@(
            '$ErrorActionPreference = ''Stop''') +
            @($definitions | ForEach-Object { $_.Extent.Text }) +
            @($paramAssignments) +
            @(('$ScriptDir = ''{0}''' -f $script:ScriptDir.Replace("'", "''")), '$MergeSourceZone = 1', '$MergeTargetZone = 2', '$MergeZoneRouteDbDsn = ''route:x@tcp(h:1)/db''', $fakeGo,
              'Invoke-MergeZoneGo -MergeCommand ''merge-zone-audit''', 'exit 99'))

        $pwshPath = (Get-Process -Id $PID).Path
        $fakeExe = Join-Path $childDir 'fake_merge_zone.ps1'
        $goRecord = Join-Path $childDir 'go_record.txt'
        $toolRecord = Join-Path $childDir 'tool_record.txt'
        $expectedCwd = (Resolve-Path (Join-Path $testRoot 'tools/merge_zone')).Path
        $envNames = @('MZ_TEST_FAKE_EXE', 'MZ_TEST_GO_RECORD', 'MZ_TEST_TOOL_RECORD', 'MZ_TEST_BUILD_EXIT', 'MZ_TEST_TOOL_EXIT')
        $saved = @{}; foreach ($n in $envNames) { $saved[$n] = [Environment]::GetEnvironmentVariable($n, 'Process') }
        function Invoke-Child([int]$BuildExit, [int]$ToolExit) {
            foreach ($f in @($goRecord, $toolRecord, $fakeExe)) { Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue }
            $env:MZ_TEST_FAKE_EXE = $fakeExe; $env:MZ_TEST_GO_RECORD = $goRecord; $env:MZ_TEST_TOOL_RECORD = $toolRecord
            $env:MZ_TEST_BUILD_EXIT = [string]$BuildExit; $env:MZ_TEST_TOOL_EXIT = [string]$ToolExit
            # 子进程的 stderr(编译失败那行)经 2>&1 收进来做断言,不能让本脚本的 Stop 把它当异常。
            $ErrorActionPreference = 'Continue'
            $output = & $pwshPath -NoProfile -NonInteractive -File $childPath 2>&1 | Out-String
            return [pscustomobject]@{ Code = $LASTEXITCODE; Output = $output }
        }
        try {
            foreach ($toolExit in @(0, 1, 2, 4)) {
                $r = Invoke-Child -BuildExit 0 -ToolExit $toolExit
                Assert-Equal $toolExit $r.Code "工具退出 $toolExit 时 pwsh -File 的进程退出码必须原样是 $toolExit(99 = 函数正常返回、漏了 exit)。子进程输出: $($r.Output)"
                Assert-True (-not (Test-Path -LiteralPath $fakeExe)) '跑完必须删掉临时编译产物'
            }

            $goCall = @(Get-Content -LiteralPath $goRecord)
            Assert-Equal "cwd=$expectedCwd" $goCall[0] 'tools/merge_zone 是独立 go module,必须在那个目录里编译'
            Assert-Equal 5 $goCall.Count '只调一次 go,参数恰为 build -o <产物> .'
            Assert-Equal 'build' $goCall[1] '必须先 go build 再直接执行产物(go run 会把非零码压成 1)'
            Assert-Equal '-o' $goCall[2] '显式指定产物路径'
            Assert-Equal $fakeExe $goCall[3] '-o 指向 New-MergeZoneExePath 给出的独占路径'
            Assert-Equal '.' $goCall[4] '编译 tools/merge_zone 这个 module 本身'

            $toolCall = @(Get-Content -LiteralPath $toolRecord)
            Assert-Equal "cwd=$expectedCwd" $toolCall[0] '工具在 tools/merge_zone 里运行(与旧 go run 的工作目录一致)'
            $toolArgs = @($toolCall | Select-Object -Skip 1)
            Assert-Equal '-mode' $toolArgs[0] '执行产物时必须去掉 "run", "." 前缀,首个参数就是工具 flag'
            Assert-Equal 'audit' (Get-ArgValue $toolArgs '-mode') '参数原样转发'
            Assert-Equal '1' (Get-ArgValue $toolArgs '-source-zone') '参数原样转发'
            Assert-Equal '0' (Get-ArgValue $toolArgs '-mapping-redis-db') '公共参数同一份组装'

            $r = Invoke-Child -BuildExit 1 -ToolExit 0
            Assert-Equal 2 $r.Code "编译失败时工具一行没跑,退出码必须是 2(没做成 / 结论不可信),不得是 0。子进程输出: $($r.Output)"
            Assert-True (-not (Test-Path -LiteralPath $toolRecord)) '编译失败不得运行工具'
            Assert-Match $r.Output 'merge_zone build failed' '编译失败必须点名报错'
        }
        finally {
            foreach ($n in $envNames) { [Environment]::SetEnvironmentVariable($n, $saved[$n], 'Process') }
        }
    }

    Test-Case '退出码:参数没给齐时往上抛(pwsh -File 下为 1 = 拒绝、什么都没做),不编译、不运行工具' {
        $runnerNames = $functionNames + @('New-MergeZoneExePath', 'Invoke-MergeZoneGo')
        $definitions = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $runnerNames }, $false))
        $paramAssignments = foreach ($p in $mergeParams) {
            $rhs = if ($null -ne $p.DefaultValue) { $p.DefaultValue.Extent.Text } elseif ($p.StaticType -eq [switch]) { '$false' } else { '$null' }
            '${0} = {1}' -f $p.Name.VariablePath.UserPath, $rhs
        }
        $childDir = Join-Path $testRoot 'param_error_child'
        New-Item -ItemType Directory -Path $childDir -Force | Out-Null
        New-Item -ItemType Directory -Path (Join-Path $testRoot 'tools/merge_zone') -Force | Out-Null
        $goRecord = Join-Path $childDir 'go_record.txt'
        $childPath = Join-Path $childDir 'run_param_error.ps1'
        $pwshPath = (Get-Process -Id $PID).Path
        # 假 go:一旦被调用就留记录(参数没给齐时不得编译);真 go 不会被碰到。
        $fakeGo = 'function go {{ Set-Content -LiteralPath ''{0}'' -Value ''go called''; $global:LASTEXITCODE = 0 }}' -f $goRecord.Replace("'", "''")
        Set-Content -LiteralPath $childPath -Encoding utf8 -Value (@('$ErrorActionPreference = ''Stop''') +
            @($definitions | ForEach-Object { $_.Extent.Text }) + @($paramAssignments) +
            @(('$ScriptDir = ''{0}''' -f $script:ScriptDir.Replace("'", "''")), $fakeGo, '$MergeZoneRouteDbDsn = ''route:x@tcp(h:1)/db''',
              'Invoke-MergeZoneGo -MergeCommand ''merge-zone''', 'exit 99'))
        $ErrorActionPreference = 'Continue'
        $out = & $pwshPath -NoProfile -NonInteractive -File $childPath 2>&1 | Out-String
        $got = $LASTEXITCODE
        $ErrorActionPreference = 'Stop'
        Assert-Equal 1 $got "merge-zone 缺 zone:参数错误往上抛,进程退出码 1(99 = 函数正常返回、漏了 exit)。子进程输出: $out"
        Assert-Match $out 'MergeSourceZone' '报错点名缺的参数'
        Assert-True (-not (Test-Path -LiteralPath $goRecord)) '参数没给齐时不编译、不运行工具'
    }

    Test-Case '相对清单路径按调用者的当前目录解析(在切进 tools/merge_zone 之前)' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeManifestPath = 'rel_manifest.json'
        Push-Location $testRoot
        try { $a = Get-CmdArgs 'merge-zone' }
        finally { Pop-Location }
        Assert-Equal (Join-Path $testRoot 'rel_manifest.json') (Get-ArgValue $a '-manifest-path') '相对路径必须按调用者目录转成绝对路径'
    }
}
finally {
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-merge-zone-contract-tests-*') { throw "拒绝清理测试目录以外的路径: $resolvedRoot" }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'dev_tools merge-zone 转发契约')
