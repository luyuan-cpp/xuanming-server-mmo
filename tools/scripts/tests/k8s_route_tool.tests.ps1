#requires -Version 7
<#
.SYNOPSIS
    route-tool 一次性 Job 入口(tools/scripts/k8s_route_tool.ps1)与 Job 模板(deploy/k8s/manifests/go-svc/data-service-route-tool.yaml)的契约测试。
.DESCRIPTION
    D1 第 1b-3 批(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7 第 3 条、§13.4 第 3 条)。
    只测渲染与参数校验:从脚本 AST 抽函数直接调用(不执行入口);另以子进程跑两次 -DryRun / 非法参数,PATH 最前面放一个
    一被调用就记账的假 kubectl,断言这两条路径**一次都不调用 kubectl**、不建报告目录。不访问任何集群。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_route_tool.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
# Get-RepoRoot / Get-ToolsScriptsDir / Invoke-ToolScript / Get-ManifestBlocks,以及 release_common(Resolve-ImagePullPolicy)。
. "$PSScriptRoot/lib/deploy_capture.ps1"

$script:RouteToolScript = Join-Path (Get-ToolsScriptsDir) 'k8s_route_tool.ps1'
$parseTokens = $null
$parseErrors = $null
$script:RouteToolAst = [Management.Automation.Language.Parser]::ParseFile($script:RouteToolScript, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "k8s_route_tool.ps1 语法错误: $($parseErrors.Message -join '; ')" }
foreach ($node in $script:RouteToolAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock()
}
foreach ($name in @('Get-RouteToolContract', 'Get-RouteToolArgErrors', 'New-RouteToolRunId', 'ConvertTo-RouteToolArgsYaml', 'New-RouteToolJobManifest',
        'Split-RouteToolLog', 'Resolve-RouteToolExitCode', 'Get-RouteToolJobPhase', 'Invoke-RouteTool', 'Save-RouteToolSummary', 'Save-RouteToolReportText')) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_route_tool.ps1 缺少函数 $name(后续断言无意义)" }
}

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath)) -replace "`r`n", "`n")
}

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

# 渲染一份清单(缺省参数可逐项覆盖)。
function New-TestManifest {
    param(
        [string]$RunId = '20261019-120000-00ab',
        [string]$Image = 'ghcr.io/luyuancpp/mmorpg-data-service:0123456789ab',
        [string]$PullPolicy = 'IfNotPresent',
        [string[]]$Arguments = @('audit', '-frozen-recovery'),
        [int]$TimeoutSeconds = 2400,
        [string]$TemplateText = (Read-RepoText (Get-RouteToolContract).JobTemplate)
    )
    return New-RouteToolJobManifest -TemplateText $TemplateText -RunId $RunId -Image $Image -PullPolicy $PullPolicy -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
}

# 清单里容器 args 那一行(JSON 数组)还原成字符串数组。
function Get-ManifestArgs {
    param([Parameter(Mandatory = $true)][string]$Manifest)
    $lines = @([regex]::Matches($Manifest, '(?m)^[ ]+args:[ ]*(?<v>\[.*\])[ ]*$'))
    if ($lines.Count -ne 1) { throw "清单里 args 行应恰好一处(实际 $($lines.Count) 处)" }
    return [string[]]@($lines[0].Groups['v'].Value | ConvertFrom-Json)
}

Write-Host ''
Write-Host '=== k8s_route_tool.ps1 / data-service-route-tool.yaml contract tests ==='

Test-Case '参数块:-ZoneName / -RouteToolArgs 必填;-TimeoutSeconds 缺省 2400 且有上下限;与 k8s_deploy.ps1 同名的 -NamespacePrefix / -KubeContext / -KubeConfig 缺省值一致' {
    $params = @{}
    foreach ($p in $script:RouteToolAst.ParamBlock.Parameters) { $params[$p.Name.VariablePath.UserPath] = $p }
    foreach ($name in @('ZoneName', 'RouteToolArgs', 'ReportDir', 'TimeoutSeconds', 'Image', 'DryRun', 'NamespacePrefix', 'KubeContext', 'KubeConfig')) {
        Assert-True -Condition $params.ContainsKey($name) -Because "缺少参数 -$name"
    }
    Assert-Equal -Expected 2400 -Actual $params['TimeoutSeconds'].DefaultValue.Value -Because '缺省 40 分钟:大于子命令自己的 -timeout 缺省 30m 再加启动开销,K8s 不会抢在 route-tool 自己超时之前杀它'
    $range = @($params['TimeoutSeconds'].Attributes | Where-Object { $_ -is [Management.Automation.Language.AttributeAst] -and $_.TypeName.Name -eq 'ValidateRange' })
    Assert-Equal -Expected 1 -Actual $range.Count -Because '-TimeoutSeconds 必须有 ValidateRange'
    $deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$null)
    foreach ($name in @('NamespacePrefix', 'KubeContext', 'KubeConfig')) {
        $deployParam = @($deployAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq $name })
        Assert-Equal -Expected 1 -Actual $deployParam.Count -Because "k8s_deploy.ps1 应当有 -$name"
        Assert-Equal -Expected $deployParam[0].DefaultValue.Extent.Text -Actual $params[$name].DefaultValue.Extent.Text -Because "-$name 的缺省值与 k8s_deploy.ps1 一致(namespace = <NamespacePrefix>-<ZoneName> 同一口径)"
    }
    $zonePattern = @($params['ZoneName'].Attributes | Where-Object { $_ -is [Management.Automation.Language.AttributeAst] -and $_.TypeName.Name -eq 'ValidatePattern' })
    Assert-Equal -Expected 1 -Actual $zonePattern.Count -Because '-ZoneName 必须有 ValidatePattern(进 namespace 名,只许 DNS 标签字符)'
}

Test-Case '契约字面量与源头一致:固定 args 前缀 == data-service.yaml 的 args;ConfigMap 名 == 服务 Deployment 与 migrate Job;cli.go 的每个子命令都在白名单里;镜像 ENTRYPOINT 是 /app/service' {
    $c = Get-RouteToolContract
    $serviceManifest = Read-RepoText 'deploy/k8s/manifests/go-svc/data-service.yaml'
    $serviceArgs = [regex]::Match($serviceManifest, '(?m)^[ ]+args:[ ]*(?<v>\[.*\])[ ]*$')
    Assert-True -Condition $serviceArgs.Success -Because 'data-service.yaml 里应当有一行 args: [...]'
    $serviceArgList = [string[]]@($serviceArgs.Groups['v'].Value | ConvertFrom-Json)
    Assert-Equal -Expected ($serviceArgList -join ' ') -Actual (@($c.FixedArgs | Select-Object -First 2) -join ' ') -Because 'route-tool 与在役服务读同一份配置文件(-f /app/etc/data_service.yaml)'
    Assert-Equal -Expected '-route-tool' -Actual $c.FixedArgs[2] -Because 'data_service.go 的工具入口 flag 是 -route-tool'
    Assert-Match -Text (Read-RepoText 'go/data_service/data_service.go') -Pattern '"route-tool"' -Because 'data_service.go 里要有 -route-tool 这个 flag'
    foreach ($path in @('deploy/k8s/manifests/go-svc/data-service.yaml', 'deploy/k8s/manifests/go-svc/data-service-migrate.yaml', $c.JobTemplate)) {
        Assert-Match -Text (Read-RepoText $path) -Pattern ('(?m)^[ ]+name:[ ]*' + [regex]::Escape($c.ConfigMapName) + '[ ]*$') -Because "$path 挂的是同一个 ConfigMap $($c.ConfigMapName)"
    }
    Assert-Match -Text (Read-RepoText 'tools/scripts/k8s_deploy.ps1') -Pattern ('"data-service"\s*=\s*@\{\s*ConfigMap\s*=\s*"' + [regex]::Escape($c.ConfigMapName) + '"') -Because 'ConfigMap 名来自部署目录 $GoSvcCatalogue."data-service"'
    $cli = Read-RepoText 'go/data_service/internal/routetoolcli/cli.go'
    $subs = @([regex]::Matches($cli, '(?m)^\s*sub[A-Z][A-Za-z0-9]*\s*=\s*"(?<s>[a-z][a-z0-9-]*)"') | ForEach-Object { $_.Groups['s'].Value })
    Assert-True -Condition ($subs.Count -ge 6) -Because "cli.go 里应当至少有 6 个 sub* 子命令常量(实际 $($subs.Count) 个),正则失配时这条先红"
    foreach ($sub in $subs) {
        Assert-True -Condition ($c.Subcommands -ccontains $sub) -Because "cli.go 的子命令 '$sub' 必须在 Get-RouteToolContract.Subcommands 里,否则 K8s 上跑不了它"
    }
    Assert-Match -Text (Read-RepoText 'deploy/k8s/Dockerfile.go-svc') -Pattern '(?m)^ENTRYPOINT \["/app/service"\]' -Because '模板写了 command,要显式调 /app/service;镜像入口改了这里先红'
}

Test-Case 'Get-RouteToolArgErrors:合法组合放行;空、未知子命令、入口参数、控制字符、整条命令行塞成一项都拒绝并点名' {
    foreach ($ok in @(
            @('audit'), @('audit', '-frozen-recovery', '-timeout=60m'), @('import-from-redis', '-dry-run=false', '-allow-db-first'),
            @('tidb-prepare', '-zones=1,2,3', '-dry-run=false'), @('freeze', '-reason=全局库回档 2026-10-19'), @('reconcile-from-cache', '-dry-run=true'),
            @('unfreeze', '-confirm-audited'), @('rewarm', '-wait=0'), @('audit', '-max-report', '50')
        )) {
        $errors = @(Get-RouteToolArgErrors -Arguments $ok)
        Assert-Equal -Expected 0 -Actual $errors.Count -Because "合法参数 '$($ok -join ' ')' 不该被拒:$($errors -join ';')"
    }
    foreach ($case in @(
            @{ Args = @(); Pattern = '为空' }
            @{ Args = @('audit -frozen-recovery'); Pattern = '整条命令行塞成了一个字符串' }
            @{ Args = @('audit,-frozen-recovery'); Pattern = '整条命令行塞成了一个字符串' }
            @{ Args = @('Audit'); Pattern = "'Audit' 不是已知子命令" }
            @{ Args = @('-frozen-recovery', 'audit'); Pattern = '不是已知子命令' }
            @{ Args = @('audit', '-f', 'other.yaml'); Pattern = "'-f' 是入口参数" }
            @{ Args = @('audit', '--f=other.yaml'); Pattern = "'--f=other.yaml' 是入口参数" }
            @{ Args = @('audit', '-route-tool'); Pattern = '是入口参数' }
            @{ Args = @('audit', '-migrate'); Pattern = '是入口参数' }
            @{ Args = @('audit', '-version'); Pattern = '是入口参数' }
            @{ Args = @('audit', ''); Pattern = '第 2 项是空串' }
            @{ Args = @('audit', "-reason=a`nb"); Pattern = '控制字符' }
            @{ Args = @('audit', "-reason=a`tb"); Pattern = '控制字符' }
        )) {
        $errors = @(Get-RouteToolArgErrors -Arguments $case.Args)
        Assert-True -Condition ($errors.Count -gt 0) -Because "非法参数 '$($case.Args -join '|')' 必须被拒"
        Assert-Match -Text ($errors -join ';') -Pattern $case.Pattern -Because "拒绝文本要点名原因('$($case.Args -join '|')')"
    }
    Assert-True -Condition (@(Get-RouteToolArgErrors -Arguments $null).Count -gt 0) -Because '$null 与空数组同样拒绝'
}

Test-Case 'New-RouteToolRunId:yyyyMMdd-HHmmss-<4 位小写十六进制>,能进 Job 名(≤ 63 字符的 DNS 标签)' {
    $id = New-RouteToolRunId -Now ([datetime]::new(2026, 10, 19, 7, 5, 9)) -Random 0xAB
    Assert-Equal -Expected '20261019-070509-00ab' -Actual $id -Because '时间按 InvariantCulture 格式化,随机数补零到 4 位小写十六进制'
    Assert-Equal -Expected '20261019-070509-ffff' -Actual (New-RouteToolRunId -Now ([datetime]::new(2026, 10, 19, 7, 5, 9)) -Random 65535) -Because '上界'
    $jobName = "$((Get-RouteToolContract).JobNamePrefix)-$id"
    Assert-True -Condition ($jobName.Length -le 63 -and $jobName -cmatch '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') -Because "Job 名 '$jobName' 必须是 ≤ 63 字符的 DNS 标签(Pod 的 job-name 标签值同长)"
}

Test-Case 'New-RouteToolJobManifest:Job 名 / 运行号 / 镜像 / 拉取策略 / deadline 替换到位,args = 固定前缀 + 子命令,backoffLimit 0、restartPolicy Never、不带 app=data-service,不残留占位' {
    $c = Get-RouteToolContract
    $manifest = New-TestManifest -TimeoutSeconds 4200
    Assert-Match -Text $manifest -Pattern '(?m)^kind: Job$' -Because '渲染结果是一个 Job'
    Assert-Match -Text $manifest -Pattern ('(?m)^  name: ' + [regex]::Escape("$($c.JobNamePrefix)-20261019-120000-00ab") + '$') -Because 'Job 名 = 前缀 + 运行号(每次新名字,不删上一次的 Job)'
    Assert-Match -Text $manifest -Pattern '(?m)^  backoffLimit: 0$' -Because '不自动重试:重跑与否由人看过输出再定'
    Assert-Match -Text $manifest -Pattern '(?m)^  activeDeadlineSeconds: 4200$' -Because 'activeDeadlineSeconds = -TimeoutSeconds'
    Assert-Match -Text $manifest -Pattern '(?m)^  ttlSecondsAfterFinished: \d+$' -Because '结束的 Job 由 K8s 自己回收(报告目录才是长期留档)'
    Assert-Match -Text $manifest -Pattern '(?m)^      restartPolicy: Never$' -Because 'Pod 不原地重启'
    Assert-Match -Text $manifest -Pattern ('(?m)^          image: "' + [regex]::Escape('ghcr.io/luyuancpp/mmorpg-data-service:0123456789ab') + '"$') -Because '镜像原样写入(JSON 字符串)'
    Assert-Match -Text $manifest -Pattern '(?m)^          imagePullPolicy: IfNotPresent$' -Because '拉取策略原样写入'
    Assert-Match -Text $manifest -Pattern ('(?m)^        - name: ' + [regex]::Escape($c.ContainerName) + '$') -Because "容器名是 $($c.ContainerName)(取退出码与日志按它认)"
    Assert-Match -Text $manifest -Pattern '(?m)^              value: "20261019-120000-00ab"$' -Because '运行号进容器环境变量,标记行带它'
    Assert-Match -Text $manifest -Pattern '(?m)^            - name: svc-config$' -Because '容器挂载 svc-config 卷'
    Assert-Match -Text $manifest -Pattern '(?m)^              mountPath: /app/etc$' -Because 'ConfigMap 挂到 /app/etc(与在役服务同一路径)'
    Assert-NotMatch -Text $manifest -Pattern '(?m)^\s+app: data-service\s*$' -Because '标签刻意不用 app=data-service:Service / PDB 的 selector 会把工具 Pod 当成 gRPC 后端'
    Assert-Match -Text $manifest -Pattern ('(?m)^    app: ' + [regex]::Escape($c.JobAppLabel) + '$') -Because '查在途 Job 按 app=data-service-route-tool'
    Assert-NotMatch -Text $manifest -Pattern 'PLACEHOLDER_|__[A-Z][A-Z0-9_]*__' -Because '不残留任何占位'
    $renderedArgs = Get-ManifestArgs -Manifest $manifest
    Assert-Equal -Expected '-f|/app/etc/data_service.yaml|-route-tool|audit|-frozen-recovery' -Actual ($renderedArgs -join '|') -Because 'args = -f /app/etc/data_service.yaml -route-tool <子命令与参数>'
}

Test-Case 'args 逐项 JSON 转义:引号、反斜杠、中文、逗号、等号原样往返;镜像里的说明文字(含空格与括号)也是合法 YAML 字符串' {
    $tricky = @('freeze', '-reason=回档 "PITR" C:\tmp, 2026-10-19', '-zones=1,2')
    $manifest = New-TestManifest -Arguments $tricky -Image '(resolved at run time from deployment/data-service in mmorpg-zone-z1)'
    $renderedArgs = Get-ManifestArgs -Manifest $manifest
    Assert-Equal -Expected (@('-f', '/app/etc/data_service.yaml', '-route-tool') + $tricky -join '|') -Actual ($renderedArgs -join '|') -Because '每一项都必须原样还原(不按逗号 / 空格切,不吞引号与反斜杠)'
    $imageLine = [regex]::Match($manifest, '(?m)^          image: (?<v>.*)$').Groups['v'].Value
    Assert-Equal -Expected '(resolved at run time from deployment/data-service in mmorpg-zone-z1)' -Actual ($imageLine | ConvertFrom-Json) -Because '镜像写成 JSON 字符串,说明文字不会被 YAML 误解析'
}

Test-Case 'New-RouteToolJobManifest 负向:运行号形状、拉取策略、镜像含换行、参数非法、模板多出未知占位都 throw' {
    Assert-Match -Text (Get-ThrownMessage { New-TestManifest -RunId 'NOT-A-RUN-ID' }) -Pattern '运行号' -Because '运行号进 Job 名,形状不对就拒绝'
    Assert-Match -Text (Get-ThrownMessage { New-TestManifest -PullPolicy 'always' }) -Pattern '拉取策略' -Because 'K8s 枚举区分大小写'
    Assert-Match -Text (Get-ThrownMessage { New-TestManifest -Image "a`nb" }) -Pattern '镜像引用' -Because '镜像不许含控制字符'
    Assert-Match -Text (Get-ThrownMessage { New-TestManifest -Arguments @('drop-everything') }) -Pattern '不是已知子命令' -Because '渲染前再核一遍参数(纵深防御)'
    $template = (Read-RepoText (Get-RouteToolContract).JobTemplate) + "`n# PLACEHOLDER_SOMETHING_NEW`n"
    Assert-Match -Text (Get-ThrownMessage { New-TestManifest -TemplateText $template }) -Pattern 'PLACEHOLDER_SOMETHING_NEW' -Because '模板加了新占位而渲染没跟上时 fail-closed,点名占位'
}

Test-Case 'Job 模板的 sh 包装:标记行与 Get-RouteToolContract 的格式一致、带运行号;stdout 先进 emptyDir 再打印;以 route-tool 的退出码退出' {
    $c = Get-RouteToolContract
    $template = Read-RepoText $c.JobTemplate
    $begin = $c.StdoutBeginFormat -f '${ROUTE_TOOL_RUN_ID}'
    $end = $c.StdoutEndFormat -f '${ROUTE_TOOL_RUN_ID}'
    Assert-Match -Text $template -Pattern ([regex]::Escape("printf '%s\n' `"$begin`"")) -Because 'BEGIN 行由 printf 打印,后面紧跟换行'
    Assert-Match -Text $template -Pattern ([regex]::Escape("printf '\n%s\n' `"$end`"")) -Because 'END 行前面固定一个换行(Split-RouteToolLog 按它还原 stdout 末尾有没有换行)'
    Assert-Match -Text $template -Pattern '(?m)^\s+/app/service "\$@" >"\$out"$' -Because 'stdout 先写进文件;stderr 直接进容器日志'
    Assert-Match -Text $template -Pattern '(?m)^\s+out=/tmp/route-tool/stdout$' -Because 'stdout 文件在 emptyDir 里'
    Assert-Match -Text $template -Pattern '(?m)^\s+mountPath: /tmp/route-tool$' -Because 'emptyDir 挂在 /tmp/route-tool'
    Assert-Match -Text $template -Pattern '(?m)^\s+exit "\$rc"$' -Because '容器以 route-tool 自己的退出码退出(本脚本原样返回 0/1/2/3)'
    Assert-Match -Text $template -Pattern '(?m)^\s+- name: ROUTE_TOOL_RUN_ID$' -Because '运行号经环境变量交给包装'
}

Test-Case 'Split-RouteToolLog:统计行末尾有无换行都原样还原;stdout 为空;标记缺失时整段算 stderr;别的运行号的标记不认' {
    $run = '20261019-120000-00ab'
    $c = Get-RouteToolContract
    $b = $c.StdoutBeginFormat -f $run
    $e = $c.StdoutEndFormat -f $run
    $cases = @(
        @{ Why = '统计行带换行'; Log = "NOTE x`nTarget host=mysql db=g`n$b`nroute_audit total=3`n`n$e`n"; Out = "route_audit total=3`n"; Err = "NOTE x`nTarget host=mysql db=g`n"; Found = $true }
        @{ Why = '统计行不带换行'; Log = "e1`n$b`nroute_audit total=3`n$e`n"; Out = 'route_audit total=3'; Err = "e1`n"; Found = $true }
        @{ Why = 'stdout 为空'; Log = "e1`ne2`n$b`n`n$e`n"; Out = ''; Err = "e1`ne2`n"; Found = $true }
        @{ Why = 'CRLF'; Log = "e1`r`n$b`r`nline`r`n`r`n$e`r`n"; Out = "line`n"; Err = "e1`n"; Found = $true }
        @{ Why = '进程被杀,没有标记'; Log = "e1`ne2`n"; Out = ''; Err = "e1`ne2`n"; Found = $false }
        @{ Why = '只有 BEGIN 没有 END'; Log = "e1`n$b`npartial"; Out = ''; Err = "e1`n$b`npartial"; Found = $false }
        @{ Why = '别的运行号的标记'; Log = "e1`n$($c.StdoutBeginFormat -f '20261019-000000-0000')`nx`n`n$($c.StdoutEndFormat -f '20261019-000000-0000')`n"; Out = ''; Found = $false }
    )
    foreach ($case in $cases) {
        $split = Split-RouteToolLog -Text $case.Log -RunId $run
        Assert-Equal -Expected $case.Found -Actual $split.MarkersFound -Because "$($case.Why):MarkersFound"
        Assert-Equal -Expected $case.Out -Actual $split.Stdout -Because "$($case.Why):stdout"
        if ($case.ContainsKey('Err')) { Assert-Equal -Expected $case.Err -Actual $split.Stderr -Because "$($case.Why):stderr" }
    }
}

Test-Case 'Resolve-RouteToolExitCode:0/1/2/3 原样返回;取不到退出码、OOMKilled 137、127 一律 2' {
    foreach ($pair in @(@(0, 0, 'clean'), @(1, 1, 'needs-attention'), @(2, 2, 'failed'), @(3, 3, 'merge-fence-active'))) {
        $r = Resolve-RouteToolExitCode -ContainerExitCode $pair[0]
        Assert-Equal -Expected "$($pair[1])/$($pair[2])" -Actual "$($r.ExitCode)/$($r.Verdict)" -Because "容器退出码 $($pair[0]) 原样返回"
    }
    foreach ($code in @(137, 127, 143, -1, 4)) {
        $r = Resolve-RouteToolExitCode -ContainerExitCode $code
        Assert-Equal -Expected 2 -Actual $r.ExitCode -Because "容器退出码 $code 不在 route-tool 的 0..3 里,按失败(2)报"
        Assert-Match -Text $r.Verdict -Pattern "unexpected-exit-code-$code" -Because '判定里点名原始退出码'
    }
    $none = Resolve-RouteToolExitCode -ContainerExitCode $null
    Assert-Equal -Expected '2/no-exit-code' -Actual "$($none.ExitCode)/$($none.Verdict)" -Because '取不到退出码(Pod 没起来、被 deadline 删掉)按失败报,不猜'
}

Test-Case 'Get-RouteToolJobPhase:Complete / SuccessCriteriaMet → complete;Failed / FailureTarget → failed;其余 running' {
    foreach ($case in @(
            @{ Types = @(); Phase = 'running' }
            @{ Types = @('Complete'); Phase = 'complete' }
            @{ Types = @('SuccessCriteriaMet'); Phase = 'complete' }
            @{ Types = @('Failed'); Phase = 'failed' }
            @{ Types = @('FailureTarget'); Phase = 'failed' }
            @{ Types = @('Suspended'); Phase = 'running' }
        )) {
        $job = [pscustomobject]@{ status = [pscustomobject]@{ conditions = @($case.Types | ForEach-Object { [pscustomobject]@{ type = $_; status = 'True' } }) } }
        Assert-Equal -Expected $case.Phase -Actual (Get-RouteToolJobPhase -Job $job) -Because "conditions=$($case.Types -join ',')"
    }
    $falseCondition = [pscustomobject]@{ status = [pscustomobject]@{ conditions = @([pscustomobject]@{ type = 'Complete'; status = 'False' }) } }
    Assert-Equal -Expected 'running' -Actual (Get-RouteToolJobPhase -Job $falseCondition) -Because 'status=False 的条件不算'
}

# ─── 子进程:-DryRun 与非法参数都不调用 kubectl ───

# 一个一被调用就往 calls.txt 记一行、以 99 退出的假 kubectl。PATH 最前面放它所在目录,子进程里的 Get-Command kubectl 会先找到它。
function New-FakeKubectlDir {
    $dir = Join-Path ([IO.Path]::GetTempPath()) ("route-tool-test-" + [guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $dir
    if ($IsWindows) {
        [IO.File]::WriteAllText((Join-Path $dir 'kubectl.cmd'), "@echo off`r`necho FAKE_KUBECTL_CALLED %*>>`"%~dp0calls.txt`"`r`nexit /b 99`r`n")
    }
    else {
        $path = Join-Path $dir 'kubectl'
        [IO.File]::WriteAllText($path, "#!/bin/sh`necho `"FAKE_KUBECTL_CALLED `$*`" >> `"`$(dirname `"`$0`")/calls.txt`"`nexit 99`n")
        & chmod +x $path
    }
    return $dir
}

Test-Case '子进程 -DryRun:退出码 0,打印 BEGIN / END MANIFEST 之间的 Job,一次都不调用 kubectl,不建报告目录;没给 -Image 时清单写说明文字并提示' {
    $fake = New-FakeKubectlDir
    try {
        $report = Join-Path $fake 'report-should-not-exist'
        $fakeEnv = @{ PATH = "$fake$([IO.Path]::PathSeparator)$env:PATH" }
        $withImage = Invoke-ToolScript -ScriptName 'k8s_route_tool.ps1' -Env $fakeEnv -Arguments @(
            '-ZoneName', 'z1', '-RouteToolArgs', 'audit', '-Image', 'ghcr.io/luyuancpp/mmorpg-data-service:0123456789ab', '-DryRun', '-ReportDir', $report, '-KubeContext', 'contract-bogus')
        Assert-Equal -Expected 0 -Actual $withImage.ExitCode -Because "DryRun 应当成功。输出: $($withImage.Output)"
        $blocks = @(Get-ManifestBlocks -Output $withImage.Output)
        Assert-Equal -Expected 1 -Actual $blocks.Count -Because 'DryRun 打印恰好一份清单(与 k8s_deploy.ps1 同一对标记)'
        Assert-Match -Text $blocks[0] -Pattern '(?m)^kind: Job\s*$' -Because '打印的是 Job'
        Assert-Match -Text $blocks[0] -Pattern 'mmorpg-data-service:0123456789ab' -Because '给了 -Image 就用它'
        $renderedArgs = Get-ManifestArgs -Manifest ($blocks[0] -replace "`r`n", "`n")
        Assert-Equal -Expected '-f|/app/etc/data_service.yaml|-route-tool|audit' -Actual ($renderedArgs -join '|') -Because 'args 原样'
        Assert-Match -Text $withImage.Output -Pattern 'namespace=mmorpg-zone-z1 ' -Because 'namespace = <NamespacePrefix>-<ZoneName>'
        Assert-NotMatch -Text $withImage.Output -Pattern '(?i)password|Mmorpg#' -Because 'DryRun 不读 ConfigMap,输出里不会有口令'

        $noImage = Invoke-ToolScript -ScriptName 'k8s_route_tool.ps1' -Env $fakeEnv -Arguments @('-ZoneName', 'z1', '-RouteToolArgs', 'audit', '-DryRun', '-ReportDir', $report)
        Assert-Equal -Expected 0 -Actual $noImage.ExitCode -Because "没给 -Image 的 DryRun 也应当成功。输出: $($noImage.Output)"
        Assert-Match -Text $noImage.Output -Pattern 'resolved at run time from deployment/data-service in mmorpg-zone-z1' -Because '清单里的镜像写成说明文字'
        Assert-Match -Text $noImage.Output -Pattern '-Image not given' -Because '提示真跑时从 deployment/data-service 读镜像'

        Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $fake 'calls.txt'))) -Because "DryRun 不得调用 kubectl(假 kubectl 记了:$(if (Test-Path -LiteralPath (Join-Path $fake 'calls.txt')) { Get-Content -LiteralPath (Join-Path $fake 'calls.txt') -Raw }))"
        Assert-True -Condition (-not (Test-Path -LiteralPath $report)) -Because 'DryRun 不建报告目录'
    }
    finally {
        Remove-Item -LiteralPath $fake -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '子进程非法参数:退出码 2、点名原因,在任何 kubectl 之前(pwsh -File 下整条命令行 / 逗号列表塞成一个字符串、未知子命令)' {
    # 入口参数(-f / -migrate …)的拒绝只在纯函数用例里测:pwsh -File 会把 '-migrate' 当成本脚本的参数名去绑定,到不了校验。
    $fake = New-FakeKubectlDir
    try {
        $fakeEnv = @{ PATH = "$fake$([IO.Path]::PathSeparator)$env:PATH" }
        foreach ($case in @(
                @{ Args = @('-ZoneName', 'z1', '-RouteToolArgs', 'audit -frozen-recovery'); Pattern = '整条命令行塞成了一个字符串' }
                @{ Args = @('-ZoneName', 'z1', '-RouteToolArgs', 'audit,-frozen-recovery'); Pattern = '整条命令行塞成了一个字符串' }
                @{ Args = @('-ZoneName', 'z1', '-RouteToolArgs', 'drop-everything'); Pattern = "'drop-everything' 不是已知子命令" }
            )) {
            $run = Invoke-ToolScript -ScriptName 'k8s_route_tool.ps1' -Env $fakeEnv -Arguments $case.Args
            Assert-Equal -Expected 2 -Actual $run.ExitCode -Because "非法参数 '$($case.Args -join ' ')' 以 2 退出。输出: $($run.Output)"
            Assert-Match -Text $run.Output -Pattern $case.Pattern -Because "输出点名原因('$($case.Args -join ' ')')"
        }
        Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $fake 'calls.txt'))) -Because '参数校验在任何 kubectl 之前'
    }
    finally {
        Remove-Item -LiteralPath $fake -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '脚本入口:函数之外只有参数、$ErrorActionPreference、两个路径变量、dot-source 与入口两条;入口以 Invoke-RouteTool 的返回值作退出码,未预料的异常兜成 2' {
    $statements = @($script:RouteToolAst.EndBlock.Statements | Where-Object { $_ -isnot [Management.Automation.Language.FunctionDefinitionAst] })
    $texts = @($statements | ForEach-Object { $_.Extent.Text })
    Assert-Equal -Expected 6 -Actual $statements.Count -Because "函数之外只有 6 条语句(主流程不得漏在入口里,测不到):$($texts -join ' || ')"
    Assert-Match -Text $texts[4] -Pattern '^try\s*\{\s*\$exitCode = Invoke-RouteTool\s*\}\s*catch\s*\{' -Because '入口调用 Invoke-RouteTool,外面包一层 try(review B3-7 / OPS-5)'
    Assert-Match -Text $texts[4] -Pattern '\$exitCode = \(Get-RouteToolContract\)\.ExitFailed' -Because '未预料的异常按本脚本自己的失败(2)退出:pwsh -File 下未捕获异常是 1,与「完成但需要人工处理」撞号'
    Assert-Equal -Expected 'exit ([int]$exitCode)' -Actual $texts[5] -Because '以主流程返回值作退出码(0/1/2/3 原样)'
}

# ─── 退出码兜底(review B3-7 / OPS-5):本脚本自己的失败一律 2,Job 之后的落盘失败不盖掉容器退出码 ───

# 在本进程里调 Invoke-RouteTool:它按动态作用域读脚本参数,这里在调用方作用域给全;调用方作用域里定义的同名函数就是替身。
# 返回 { Codes(返回值,应恰好一个);Text(Write-Host 与 Write-Warning 合并)}。
function Invoke-RouteToolInProcess {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    foreach ($name in $Variables.Keys) { Set-Variable -Name $name -Value $Variables[$name] }
    $items = @(Invoke-RouteTool 3>&1 6>&1)
    $isStream = { param($item) $item -is [Management.Automation.InformationRecord] -or $item -is [Management.Automation.WarningRecord] }
    $codes = @($items | Where-Object { -not (& $isStream $_) } | ForEach-Object { [string]$_ })
    $text = (@($items | Where-Object { & $isStream $_ } | ForEach-Object { [string]$_ })) -join "`n"
    return [pscustomobject]@{ Codes = ($codes -join ','); Text = $text }
}

Test-Case 'Invoke-RouteTool:Job 模板读不到时返回 2 并点名原因,不 throw 到入口、不调用 kubectl、不建报告目录(DryRun 与真跑同口径)' {
    $missingRoot = Join-Path ([IO.Path]::GetTempPath()) ('route-tool-no-template-' + [guid]::NewGuid().ToString('N'))
    $kubectlCalls = [System.Collections.Generic.List[string]]::new()
    function Invoke-RouteToolKubectlChecked { param([string[]]$Arguments, [string]$What) $kubectlCalls.Add($Arguments -join ' '); return '' }
    function Invoke-RouteToolKubectl { param([string[]]$Arguments, [int]$TimeoutSeconds) $kubectlCalls.Add($Arguments -join ' '); return [pscustomobject]@{ TimedOut = $false; ExitCode = 0; Output = ''; ErrorOutput = '' } }
    foreach ($dry in @($true, $false)) {
        $r = Invoke-RouteToolInProcess -Variables @{ RepoRoot = $missingRoot; RouteToolArgs = @('audit'); ZoneName = 'z1'; NamespacePrefix = 'mmorpg-zone'; Image = ''
            TimeoutSeconds = 2400; ReportDir = ''; DryRun = $dry; KubeContext = ''; KubeConfig = '' }
        Assert-Equal -Expected '2' -Actual $r.Codes -Because "DryRun=${dry}:模板读不到是本脚本自己的失败,返回 2(以前 throw 出去,pwsh -File 下是 1)。输出: $($r.Text)"
        Assert-Match -Text $r.Text -Pattern 'cannot read the Job template, nothing was created' -Because "DryRun=${dry}:点名原因,并说明什么都没建"
    }
    Assert-Equal -Expected 0 -Actual $kubectlCalls.Count -Because '模板读不到时在任何 kubectl 之前返回'
    Assert-True -Condition (-not (Test-Path -LiteralPath $missingRoot)) -Because '不建报告目录'
}

Test-Case 'Invoke-RouteTool:Job 跑完、容器退出 3 之后 stdout.txt / stderr.txt / summary.txt 都写不了:只告警,仍返回 3,done 一行照常打出' {
    $c = Get-RouteToolContract
    $runId = '20261022-120000-00ab'
    # Get-Command kubectl -CommandType Application 要找得到一个 kubectl;真正的 kubectl 调用全部经下面的替身函数,假 kubectl 一次都不该被调。
    $fake = New-FakeKubectlDir
    $report = Join-Path $fake 'report'
    # 三个报告文件的位置先建成同名目录:WriteAllText 在 Windows / Linux 上都必然失败(确定、可重复,不靠磁盘满)。
    foreach ($name in @('stdout.txt', 'stderr.txt', 'summary.txt')) { $null = New-Item -ItemType Directory -Force -Path (Join-Path $report $name) }
    $kubectlCalls = [System.Collections.Generic.List[string]]::new()
    function New-RouteToolRunId { param($Now, $Random) return '20261022-120000-00ab' }
    function Invoke-RouteToolKubectlChecked { param([string[]]$Arguments, [string]$What) $kubectlCalls.Add($Arguments -join ' '); return '' }
    function Get-ActiveRouteToolJobs { return @() }
    function Get-RouteToolJobState { param($Namespace, $JobName) return [pscustomobject]@{ Phase = 'complete'; Reason = ''; Uid = 'uid-1' } }
    function Get-RouteToolPodResult { param($Namespace, $JobName, $JobUid) return [pscustomobject]@{ PodName = 'route-tool-pod'; ExitCode = 3; Reason = 'Completed' } }
    function Invoke-RouteToolKubectl {
        param([string[]]$Arguments, [int]$TimeoutSeconds)
        $kubectlCalls.Add($Arguments -join ' ')
        $log = "merge fence active`n" + ($c.StdoutBeginFormat -f $runId) + "`nroute_audit total=3`n`n" + ($c.StdoutEndFormat -f $runId) + "`n"
        return [pscustomobject]@{ TimedOut = $false; ExitCode = 0; Output = $log; ErrorOutput = '' }
    }
    $savedPath = $env:PATH
    try {
        $env:PATH = "$fake$([IO.Path]::PathSeparator)$env:PATH"
        $r = Invoke-RouteToolInProcess -Variables @{ RepoRoot = (Get-RepoRoot); RouteToolArgs = @('audit'); ZoneName = 'z1'; NamespacePrefix = 'mmorpg-zone'
            Image = 'ghcr.io/luyuancpp/mmorpg-data-service:0123456789ab'; TimeoutSeconds = 2400; ReportDir = $report; DryRun = $false; KubeContext = ''; KubeConfig = '' }
        Assert-Equal -Expected '3' -Actual $r.Codes -Because "容器退出码 3(有生效的合服围栏)必须原样返回,报告写不了也不能盖成 2 或 1。输出: $($r.Text)"
        Assert-Match -Text $r.Text -Pattern 'could not write [^\n]*stdout\.txt' -Because 'stdout.txt 写不了要告警'
        Assert-Match -Text $r.Text -Pattern 'could not write [^\n]*stderr\.txt' -Because 'stderr.txt 写不了要告警'
        Assert-Match -Text $r.Text -Pattern 'could not write [^\n]*summary\.txt' -Because 'summary.txt 写不了要告警'
        Assert-Match -Text $r.Text -Pattern 'done: job_phase=complete container_exit=3 verdict=merge-fence-active exit=3 ' -Because 'done 一行照常打出,运维在控制台看得到 3'
        Assert-Match -Text $r.Text -Pattern 'route_audit total=3' -Because '统计行照常打到控制台'
        Assert-True -Condition (Test-Path -LiteralPath (Join-Path $report 'manifest.yaml') -PathType Leaf) -Because 'apply 之前写的清单照常落盘(apply 读的就是它)'
        Assert-Equal -Expected 1 -Actual @($kubectlCalls | Where-Object { $_ -like 'apply -n mmorpg-zone-z1 -f *' }).Count -Because '夹具前提:走到了 apply'
        Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $fake 'calls.txt'))) -Because '夹具前提:kubectl 调用全部经替身函数,假 kubectl 没被直接调用'
    }
    finally {
        $env:PATH = $savedPath
        Remove-Item -LiteralPath $fake -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'k8s_route_tool')
