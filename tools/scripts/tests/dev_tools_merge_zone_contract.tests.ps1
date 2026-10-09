#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 的 merge-zone 系列命令转发契约测试(合服 / 撤销 / 审计 / 落点:钉落点、搬库、放弃搬库、落点库审计)。
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

$functionNames = @('Get-MergeMappingRedis', 'Get-MergeDbKafka', 'Get-MergeZoneArgs', 'Get-MergeKafkaGateArgs', 'Resolve-MergeFullPath', 'Get-MergeZoneCommandArgs')
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
function Reset-MergeParams {
    foreach ($p in $mergeParams) {
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
    New-Item -ItemType Directory -Path (Join-Path $testRoot 'go/db/etc') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $testRoot 'generated/data') -Force | Out-Null
    # 与真实 data_service.yaml 同形:MappingRedis 不写 DB 键(go-zero RedisConf 没有 DB 字段)。
    Set-Content -LiteralPath (Join-Path $testRoot 'go/data_service/etc/data_service.yaml') -Encoding utf8 -Value @(
        'MappingRedis:', '  Host: 10.9.9.9:6379', '  Type: node', 'Regions:', '  - Id: 1')
    Set-Content -LiteralPath (Join-Path $testRoot 'go/db/etc/db.yaml') -Encoding utf8 -Value @(
        'ServerConfig:', '  Kafka:', '    GroupID: "db_rpc_consumer_group"', '    TopicGeneration: 3')
    Set-Content -LiteralPath (Join-Path $testRoot 'generated/data/mysql_database_table_list.json') -Encoding utf8 -Value '[]'
    $manifestFile = Join-Path $testRoot 'merge_1_to_2_fixture.json'
    Set-Content -LiteralPath $manifestFile -Encoding utf8 -Value '{}'
    $missingManifest = Join-Path $testRoot 'no_such_manifest.json'

    Test-Case '入口:ValidateSet、参数块与分发都含本批新增的命令和参数' {
        $commandParam = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Command' }
        $validateSet = $commandParam.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' }
        $allowed = @($validateSet.PositionalArguments | ForEach-Object { $_.Value })
        $paramNames = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        foreach ($cmd in @('merge-zone', 'merge-zone-audit', 'merge-zone-unmerge', 'merge-zone-relocate', 'merge-zone-relocate-abort', 'merge-zone-pin-placement', 'merge-zone-storage-audit', 'merge-zone-capability-check')) {
            Assert-True ($cmd -in $allowed) "-Command 的 ValidateSet 必须含 $cmd"
            Assert-Match $devToolsSource ([regex]::Escape("Invoke-MergeZoneGo -MergeCommand `"$cmd`"")) "$cmd 必须经 Invoke-MergeZoneGo 分发(同一份参数组装)"
            Assert-Match $devToolsSource ("-Command " + [regex]::Escape($cmd) + '\s') "帮助文本必须列出 $cmd"
        }
        foreach ($name in @('MergeSkipGuildMySql', 'MergeSkipGuildRank', 'MergePlayerRowsMode', 'MergeDbCapabilityZones', 'MergePinPlacementZone',
                            'MergeRelocateSourceStorage', 'MergeRelocateTargetStorage', 'MergeRelocatePlayerIds', 'MergeRelocateBatchSize',
                            'MergeRelocateLockWait', 'MergeAuditStorage')) {
            Assert-True ($name -in $paramNames) "param 块必须有 -$name"
        }
    }

    Test-Case '公共参数:mapping 取 data_service.yaml 的 Host、DB 缺省 0;Kafka 世代号取 db.yaml' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeDbCapabilityZones = 'none'
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal '10.9.9.9:6379' (Get-ArgValue $a '-mapping-redis-addr') 'mapping 地址必须现读 data_service.yaml'
        Assert-Equal '0' (Get-ArgValue $a '-mapping-redis-db') 'yaml 里没有 DB 键时 mapping DB 必须兜底为 0(不是 15)'
        Assert-Equal '3' (Get-ArgValue $a '-kafka-topic-generation') '积压门禁 / 搬库等锁的 topic 世代号必须取 db.yaml'
        Assert-Equal 'db_rpc_consumer_group' (Get-ArgValue $a '-kafka-group') 'consumer group 必须取 db.yaml'
        Assert-NotMatch $devToolsSource '今天是 15' '参数说明里不得再把 mapping DB 写成 15(gap-fixes B6)'
    }

    Test-Case '帮会跳过开关:默认不转发,给了就对合服 / 撤销 / 审计同口径转发' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeManifestPath = $manifestFile; $script:MergeDbCapabilityZones = 'none'
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

    Test-Case 'merge-zone:模式留空用工具默认 pin;-MergeDbCapabilityZones 没有缺省值,pin 缺省即拒,可 none' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        # pin(模式留空 = 工具默认 pin)不给能力标记 zone:点名拒绝,文案与工具一致(没有缺省、可 none、T-0 口径、capability-check)。
        $msg = Get-ThrownMessage { Get-CmdArgs 'merge-zone' }
        foreach ($want in @('MergeDbCapabilityZones', 'no default', "'none'", 'T-0', 'merge-zone-capability-check')) {
            Assert-Match $msg ([regex]::Escape($want)) "pin 合服缺 -MergeDbCapabilityZones 的提示必须含 $want"
        }
        $script:MergePlayerRowsMode = 'pin'
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone' }) 'MergeDbCapabilityZones' '显式 pin 同样必填'
        $script:MergePlayerRowsMode = ''

        $script:MergeDbCapabilityZones = 'none'
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal $null (Get-ArgValue $a '-player-rows-mode') '模式留空不转发,默认值只在工具里定义一处'
        Assert-Equal 'none' (Get-ArgValue $a '-db-capability-zones') 'none 原样转发(此刻没有别的 zone 在跑)'
        Assert-Equal '-apply' ([string]$a[-1]) '不带 -DryRun 时写意图是 -apply'
        Assert-Match (Get-ArgValue $a '-manifest-path') ('^' + [regex]::Escape($testRoot) + '[\\/]merge_1_to_2_\d{8}T\d{6}Z\.json$') '默认清单落在仓库根、绝对路径'

        $script:MergePlayerRowsMode = 'copy'; $script:MergeDbCapabilityZones = '1,2,3'; $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal 'copy' (Get-ArgValue $a '-player-rows-mode') '显式模式必须转发'
        Assert-Equal '1,2,3' (Get-ArgValue $a '-db-capability-zones') '显式能力标记 zone 集合必须转发'
        Assert-True ($a -contains '-dry-run' -and -not ($a -contains '-apply')) '-DryRun 只转发 -dry-run'

        # copy 模式入口不要求(只有清单玩家已有落点记录时才要,由工具在围栏之下判):留空不拒、不转发。
        $script:MergeDbCapabilityZones = ''
        $a = Get-CmdArgs 'merge-zone'
        Assert-Equal $null (Get-ArgValue $a '-db-capability-zones') 'copy 模式留空不转发,由工具按清单判'

        $script:MergeSourceZone = 0
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone' }) 'MergeSourceZone' '缺源区必须点名报错'
    }

    Test-Case 'merge-zone -MergeBackfillZone:走回填模式,不带合服参数' {
        Reset-MergeParams
        $script:MergeBackfillZone = 5
        $a = Get-CmdArgs 'merge-zone'
        Assert-True ($a -contains '-backfill-home-zone') '回填模式开关'
        Assert-Equal '5' (Get-ArgValue $a '-zone') '回填对象 zone'
        Assert-Equal $null (Get-ArgValue $a '-source-zone') '回填不是合服,不得带 -source-zone'
    }

    Test-Case 'merge-zone-unmerge:要求清单存在,并转发 Kafka 积压门禁的取证方式' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-unmerge' }) 'MergeManifestPath' '不给清单必须点名报错'
        $script:MergeManifestPath = $missingManifest
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-unmerge' }) 'manifest not found' '清单不存在必须拒绝'

        $script:MergeManifestPath = $manifestFile
        $a = Get-CmdArgs 'merge-zone-unmerge'
        Assert-Equal 'unmerge' (Get-ArgValue $a '-mode') '撤销模式'
        Assert-Equal $manifestFile (Get-ArgValue $a '-manifest-path') '清单绝对路径'
        Assert-True (-not ($a -contains '-assume-kafka-drained')) '没声明就不得替运维声明已排空'

        $script:MergeAssumeKafkaDrained = $true
        Assert-True ((Get-CmdArgs 'merge-zone-unmerge') -contains '-assume-kafka-drained') '撤销也要先跑目标区 P2~P7,必须转发 -assume-kafka-drained'

        $script:MergeAssumeKafkaDrained = $false; $script:MergeKafkaConsumerGroupsCmd = 'C:\kafka\bin\kafka-consumer-groups.bat'; $script:MergeKafkaBootstrap = '10.0.0.1:9092'
        $a = Get-CmdArgs 'merge-zone-unmerge'
        Assert-Equal 'C:\kafka\bin\kafka-consumer-groups.bat' (Get-ArgValue $a '-kafka-consumer-groups-cmd') '撤销必须转发 Kafka CLI'
        Assert-Equal '10.0.0.1:9092' (Get-ArgValue $a '-kafka-bootstrap') 'CLI 与 bootstrap 一起转发'
    }

    Test-Case 'merge-zone-unmerge:-MergeDbCapabilityZones 给了就转发(含 none),没给不拒(是否必填看清单模式,由工具判)' {
        Reset-MergeParams
        $script:MergeManifestPath = $manifestFile
        $a = Get-CmdArgs 'merge-zone-unmerge'
        Assert-Equal $null (Get-ArgValue $a '-db-capability-zones') '没给不转发,不在 ps1 里读清单复制规则'
        foreach ($v in @('none', '3,5')) {
            $script:MergeDbCapabilityZones = $v
            Assert-Equal $v (Get-ArgValue (Get-CmdArgs 'merge-zone-unmerge') '-db-capability-zones') "撤销必须原样转发 $v"
        }
        Assert-Match $devToolsSource ([regex]::Escape('Unmerging a pin-mode merge requires it (there is no default)')) '没给时的提示与工具口径一致'
    }

    Test-Case 'merge-zone-capability-check:必填 zone 列表,转发 -mode capability-check,只读不带写意图' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-capability-check' }) 'MergeDbCapabilityZones' '缺 zone 列表必须点名报错'
        $script:MergeDbCapabilityZones = '2'; $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone-capability-check'
        Assert-Equal 'capability-check' (Get-ArgValue $a '-mode') '能力标记核对模式'
        Assert-Equal '2' (Get-ArgValue $a '-db-capability-zones') '要核对的 zone'
        Assert-Equal '0' (Get-ArgValue $a '-mapping-redis-db') '能力标记在 mapping DB 0'
        Assert-True (-not ($a -contains '-apply' -or $a -contains '-dry-run')) '只读模式不转发写意图'
        # none 由工具拒绝(exit 1),不在 ps1 里复制规则:原样转发。
        $script:MergeDbCapabilityZones = 'none'
        Assert-Equal 'none' (Get-ArgValue (Get-CmdArgs 'merge-zone-capability-check') '-db-capability-zones') 'none 原样转发,由工具拒绝'
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

    Test-Case 'merge-zone-pin-placement:要求 zone,转发 -mode pin-placement -zone 与写意图' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-pin-placement' }) 'MergePinPlacementZone' '缺 zone 必须点名报错'
        $script:MergePinPlacementZone = 4
        $a = Get-CmdArgs 'merge-zone-pin-placement'
        Assert-Equal 'pin-placement' (Get-ArgValue $a '-mode') '钉落点模式'
        Assert-Equal '4' (Get-ArgValue $a '-zone') '对象 zone'
        Assert-Equal '0' (Get-ArgValue $a '-mapping-redis-db') '落点记录与 player:zone 同在 mapping DB 0'
        Assert-Equal '-apply' ([string]$a[-1]) '写意图'
    }

    Test-Case 'merge-zone-relocate:源 / 目标库与能力标记 zone 必填,可选参数只在给出时转发' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-relocate' }) 'MergeRelocateSourceStorage' '缺源库必须点名报错'
        $script:MergeRelocateSourceStorage = 101; $script:MergeRelocateTargetStorage = 1000000
        $msg = Get-ThrownMessage { Get-CmdArgs 'merge-zone-relocate' }
        Assert-Match $msg 'MergeDbCapabilityZones' '工具没有缺省值,能力标记 zone 集合必须显式给'
        Assert-Match $msg ([regex]::Escape("'none' is not accepted")) '搬库在线进行,提示必须说明不接受 none(与工具一致)'

        $script:MergeDbCapabilityZones = '101,102'
        $a = Get-CmdArgs 'merge-zone-relocate'
        Assert-Equal 'relocate' (Get-ArgValue $a '-mode') '搬库模式'
        Assert-Equal '101' (Get-ArgValue $a '-relocate-source-storage') '源库'
        Assert-Equal '1000000' (Get-ArgValue $a '-relocate-target-storage') '目标库(全局库 id 超出 zone 段)'
        Assert-Equal '101,102' (Get-ArgValue $a '-db-capability-zones') '能力标记 zone 集合'
        Assert-Equal '3' (Get-ArgValue $a '-kafka-topic-generation') 'R2 等的排序锁键依赖 db.yaml 的世代号'
        Assert-Match (Get-ArgValue $a '-manifest-path') ('^' + [regex]::Escape($testRoot) + '[\\/]relocate_101_to_1000000_\d{8}T\d{6}Z\.json$') '默认搬库清单落在仓库根、绝对路径'
        foreach ($flag in @('-relocate-player-ids', '-relocate-batch-size', '-relocate-lock-wait')) {
            Assert-Equal $null (Get-ArgValue $a $flag) "$flag 留空不转发,用工具默认"
        }
        Assert-Equal '-apply' ([string]$a[-1]) '写意图'

        $script:MergeRelocatePlayerIds = '7,8'; $script:MergeRelocateBatchSize = 20; $script:MergeRelocateLockWait = '3m'; $script:MergeManifestPath = $manifestFile
        $a = Get-CmdArgs 'merge-zone-relocate'
        Assert-Equal '7,8' (Get-ArgValue $a '-relocate-player-ids') '指定玩家'
        Assert-Equal '20' (Get-ArgValue $a '-relocate-batch-size') '批大小'
        Assert-Equal '3m' (Get-ArgValue $a '-relocate-lock-wait') '等锁时长'
        Assert-Equal $manifestFile (Get-ArgValue $a '-manifest-path') '续跑用同一份清单'
    }

    Test-Case 'merge-zone-relocate-abort:要求清单存在,源 / 目标库给了就转发供工具核对' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-relocate-abort' }) 'MergeManifestPath' '缺清单必须点名报错'
        $script:MergeManifestPath = $missingManifest
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-relocate-abort' }) 'manifest not found' '清单不存在必须拒绝'

        $script:MergeManifestPath = $manifestFile; $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone-relocate-abort'
        Assert-Equal 'relocate-abort' (Get-ArgValue $a '-mode') '放弃搬库模式'
        Assert-Equal $manifestFile (Get-ArgValue $a '-manifest-path') '清单'
        Assert-Equal $null (Get-ArgValue $a '-relocate-source-storage') '没给就不转发'
        Assert-True ($a -contains '-dry-run' -and -not ($a -contains '-apply')) '-DryRun 只转发 -dry-run'

        $script:MergeRelocateSourceStorage = 101; $script:MergeRelocateTargetStorage = 1000000
        $a = Get-CmdArgs 'merge-zone-relocate-abort'
        Assert-Equal '101' (Get-ArgValue $a '-relocate-source-storage') '给了就转发,工具与清单核对防拿错清单'
        Assert-Equal '1000000' (Get-ArgValue $a '-relocate-target-storage') '同上'
    }

    Test-Case 'merge-zone-storage-audit:要求 storage id,只读不带写意图' {
        Reset-MergeParams
        Assert-Match (Get-ThrownMessage { Get-CmdArgs 'merge-zone-storage-audit' }) 'MergeAuditStorage' '缺 storage 必须点名报错'
        $script:MergeAuditStorage = 1000000; $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone-storage-audit'
        Assert-Equal 'storage-audit' (Get-ArgValue $a '-mode') '落点库审计模式'
        Assert-Equal '1000000' (Get-ArgValue $a '-storage') '审计对象'
        Assert-True (-not ($a -contains '-apply' -or $a -contains '-dry-run')) '只读模式不转发写意图'
    }

    # 退出码透传:以前 Invoke-MergeZoneGo 是 `& go run ...` 且没有 exit,经 `pwsh -File` 调用时进程退出码
    # 恒为 0(合服 apply 失败 / storage-audit 连不上库都报「成功」),交互读 $LASTEXITCODE 又被 go run 压成 1。
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
            @(('$ScriptDir = ''{0}''' -f $script:ScriptDir.Replace("'", "''")), '$MergeAuditStorage = 1000000', $fakeGo,
              'Invoke-MergeZoneGo -MergeCommand ''merge-zone-storage-audit''', 'exit 99'))

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
            Assert-Equal 'storage-audit' (Get-ArgValue $toolArgs '-mode') '参数原样转发'
            Assert-Equal '1000000' (Get-ArgValue $toolArgs '-storage') '参数原样转发'
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

    Test-Case '帮助文本与参数说明:不再有 src,dst 缺省的过时说法' {
        Assert-NotMatch $devToolsSource ([regex]::Escape('default src,dst')) '帮助文本不得再写 default src,dst'
        Assert-NotMatch $devToolsSource ([regex]::Escape('list ALL running zones')) '帮助文本不得再写 list ALL running zones(T-0 时 src/dst 已下线)'
        Assert-NotMatch $devToolsSource ([regex]::Escape('工具默认 "src,dst"')) '参数说明不得再说工具默认 src,dst'
    }

    Test-Case '相对清单路径按调用者的当前目录解析(在切进 tools/merge_zone 之前)' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:MergeManifestPath = 'rel_manifest.json'; $script:MergeDbCapabilityZones = 'none'
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
