#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 的 merge-zone 系列命令转发契约测试(合服 / 撤销 / 审计,与 tools/merge_zone 的三个模式一一对应)。
.DESCRIPTION
    dev_tools.ps1 是手册里的合服入口。它多转一个工具已删的 flag,整条命令会被 flag 包拒绝;漏转一个 flag,工具不是拒绝就是做错事。

    做法:只从 AST 加载参数组装函数,参数默认值从 dev_tools.ps1 的 param 块现取(不在测试里抄第二份)。
    不起 go、不连任何库。退出码透传一条在子进程里 `pwsh -File` 真跑 Invoke-MergeZoneGo,`go` 与编译产物均为假替身。
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

$functionNames = @('Get-MergeRouteDbArgs', 'Get-MergeZoneCommandArgs', 'Get-K8sRouteToolArgs')
$loaded = @()
foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $functionNames }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
    $loaded += $definition.Name
}
foreach ($name in $functionNames) {
    if ($name -notin $loaded) { throw "dev_tools.ps1 里找不到函数 $name(契约测试的前提被改掉了)" }
}

# 被测函数按 PowerShell 的动态作用域读 $Merge* / $DryRun,这里把它们放在本脚本作用域。
# 默认值一律取 dev_tools.ps1 param 块的真实默认值。
$mergeParams = @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -match '^(Merge|DryRun$)' })
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
    return @(Get-MergeZoneCommandArgs -MergeCommand $MergeCommand)
}

try {
    New-Item -ItemType Directory -Path $script:ScriptDir -Force | Out-Null

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
        # 合服参数只留这三个(-DryRun 与其它命令共用):多一个就是工具不认的旗标,或者一个没人读的开关。
        $mergeNames = @($paramNames | Where-Object { $_ -like 'Merge*' } | Sort-Object)
        Assert-Equal 'MergeSourceZone|MergeTargetZone|MergeZoneRouteDbDsn' ($mergeNames -join '|') '合服参数只留源区、目标区和全局库 DSN'
        Assert-True (-not ('VerifyMerged' -in $paramNames)) '-VerifyMerged 对应的合服后复核已删除'
    }

    # 工具用 flag 包解析:多一个未声明的旗标,整条命令在做任何事之前退出。旗标清单从 main.go 现读,不在测试里抄第二份。
    Test-Case '转发的旗标都由 tools/merge_zone/main.go 声明' {
        $toolSource = Get-Content -LiteralPath (Join-Path $PSScriptRoot '../../merge_zone/main.go') -Raw
        $declared = @([regex]::Matches($toolSource, '\bfs\.[A-Z][A-Za-z0-9]+\((?:&[A-Za-z0-9_.]+,\s*)?"(?<n>[a-z0-9-]+)"') | ForEach-Object { $_.Groups['n'].Value })
        Assert-True ('route-db-dsn' -in $declared -and 'source-zone' -in $declared -and 'mode' -in $declared) '夹具前提:main.go 的旗标声明写法认得出来(换了写法要同步本用例的正则)'
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
        foreach ($intentDry in @($false, $true)) {
            $script:DryRun = $intentDry
            foreach ($cmd in @('merge-zone', 'merge-zone-unmerge', 'merge-zone-audit')) {
                foreach ($token in @(Get-CmdArgs $cmd | ForEach-Object { [string]$_ } | Where-Object { $_ -cmatch '^-[a-z]' })) {
                    Assert-True ($token.Substring(1) -in $declared) "$cmd 转发的 $token 不是 tools/merge_zone/main.go 声明的旗标"
                }
            }
        }
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

    foreach ($spec in @(@{ Command = 'merge-zone'; Mode = 'merge' }, @{ Command = 'merge-zone-unmerge'; Mode = 'unmerge' })) {
        Test-Case "$($spec.Command):两个 zone 必填;转发模式与 zone;只有 -DryRun 才转发 -dry-run" {
            Reset-MergeParams
            Assert-Match (Get-ThrownMessage { Get-CmdArgs $spec.Command }) 'MergeSourceZone' '缺 zone 必须点名报错'
            $script:MergeSourceZone = 1
            Assert-Match (Get-ThrownMessage { Get-CmdArgs $spec.Command }) 'MergeTargetZone' '缺目标区必须点名报错'
            $script:MergeTargetZone = 2
            $a = Get-CmdArgs $spec.Command
            Assert-Equal $spec.Mode (Get-ArgValue $a '-mode') '模式'
            Assert-Equal '1' (Get-ArgValue $a '-source-zone') '源区'
            Assert-Equal '2' (Get-ArgValue $a '-target-zone') '目标区'
            Assert-True (-not ($a -contains '-dry-run')) '不带 -DryRun 不转发 -dry-run'
            Assert-True (-not ($a -contains '-apply')) '工具已没有 -apply'
            $script:DryRun = $true
            Assert-True ((Get-CmdArgs $spec.Command) -contains '-dry-run') '-DryRun 转发 -dry-run'
        }
    }

    Test-Case 'merge-zone-audit:只读,不转发 zone 和 -dry-run(工具拒绝这些组合)' {
        Reset-MergeParams
        $script:MergeSourceZone = 1; $script:MergeTargetZone = 2; $script:DryRun = $true
        $a = Get-CmdArgs 'merge-zone-audit'
        Assert-Equal 'audit' (Get-ArgValue $a '-mode') '审计模式'
        foreach ($flag in @('-source-zone', '-target-zone', '-dry-run')) {
            Assert-True (-not ($a -contains $flag)) "审计不转发 $flag"
        }
    }

    Test-Case '全局库:-MergeZoneRouteDbDsn 没有缺省值,三个命令缺了都点名拒绝;给了原样转发;DSN 不打到控制台' {
        $param = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'MergeZoneRouteDbDsn' }
        Assert-True ($null -ne $param) 'param 块必须有 -MergeZoneRouteDbDsn'
        Assert-Equal '' $param.DefaultValue.Value '缺省必须为空(必填),不能偷偷给一个库'
        $dsn = 'route:secret@tcp(10.0.0.9:3306)/testdb'
        foreach ($cmd in @('merge-zone', 'merge-zone-unmerge', 'merge-zone-audit')) {
            Reset-MergeParams -RouteDbDsn ''
            $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
            Assert-Match (Get-ThrownMessage { Get-CmdArgs $cmd }) 'MergeZoneRouteDbDsn' "$cmd 缺 -MergeZoneRouteDbDsn 必须点名拒绝"

            Reset-MergeParams -RouteDbDsn $dsn
            $script:MergeSourceZone = 1; $script:MergeTargetZone = 2
            Assert-Equal $dsn (Get-ArgValue (Get-CmdArgs $cmd) '-route-db-dsn') "$cmd 必须原样转发 -route-db-dsn"
        }
        foreach ($name in @('Get-MergeRouteDbArgs', 'Get-MergeZoneCommandArgs', 'Invoke-MergeZoneGo')) {
            $fn = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true)
            Assert-NotMatch $fn.Extent.Text 'Write-Host' "$name 不得 Write-Host(DSN 含口令)"
        }
    }

    # ── k8s-route-tool ───────────────────
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
        $script:ZoneName = 'z1'; $script:RouteToolArgs = @('tidb-prepare', '-zones=1')
        $h = Get-K8sRouteToolArgs
        Assert-Equal 'NamespacePrefix|RouteToolArgs|ZoneName' ((@($h.Keys) | Sort-Object) -join '|') '缺省只转发 zone / 子命令 / namespace 前缀,其余默认值只在 k8s_route_tool.ps1'
        Assert-Equal 'tidb-prepare|-zones=1' (@($h.RouteToolArgs) -join '|') '子命令与参数逐项原样转发(不拼成一个字符串)'
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
            & $writeChild "@('tidb-prepare', '-zones=1')"
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
            Assert-Equal 'args=tidb-prepare|-zones=1' $rec[1] '子命令数组逐项转发'
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
            & $writeChild "@('tidb-prepare')"
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
    Test-Case '退出码:pwsh -File 下原样透传工具的 0/1/2;编译失败给 2 且不运行工具;不走 go run' {
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
              'Invoke-MergeZoneGo -MergeCommand ''merge-zone''', 'exit 99'))

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
            foreach ($toolExit in @(0, 1, 2)) {
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
            Assert-Equal '-mode' $toolArgs[0] '首个参数就是工具 flag,没有 go run 的前缀'
            Assert-Equal 'merge' (Get-ArgValue $toolArgs '-mode') '参数原样转发'
            Assert-Equal '1' (Get-ArgValue $toolArgs '-source-zone') '参数原样转发'
            Assert-Equal 'route:x@tcp(h:1)/db' (Get-ArgValue $toolArgs '-route-db-dsn') '参数原样转发'

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
}
finally {
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-merge-zone-contract-tests-*') { throw "拒绝清理测试目录以外的路径: $resolvedRoot" }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'dev_tools merge-zone 转发契约')
