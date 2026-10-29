#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 透传 D1 映射开关给 k8s_deploy.ps1 / 拒绝转给 k8s_image.ps1 的契约测试。

.DESCRIPTION
    k8s_deploy.ps1 的 -RouteStoreMode / -RouteReconcileArm / -RouteReadRequiresReady / -DbPlacementRequireRouteReady /
    -GuildRequireRouteReady 是粘滞的:非 DryRun 时读线上 ConfigMap,与本次参数不一致且没带 -ConfirmRouteSwitchChange 即拒绝
    (Assert-RouteSwitchClusterState)。包装入口 dev_tools.ps1 的 k8s-infra-up / k8s-zone-up / k8s-all-up 以前不透传这六个参数,
    集群切 db_first 之后经它们的部署一律被拒,且没法照传现状(placement-line-handoff-20261019 §3 第 3 条)。本文件钉住:
      - Invoke-K8sDeploy:-RouteStoreMode 非空才传(空串 / 空白不传),五个 switch 只在被指定时传,值原样;
      - Invoke-K8sImage:六个参数显式给了即拒绝(k8s_image.ps1 不部署 Go 服务),报错点名参数;
      - k8s-zone-rollback 分支:六个参数显式给了即在组装回档参数之前拒绝(回档脚本不声明它们,Step 6 按 k8s_deploy 缺省重建);
      - param 块:六个参数都在,-RouteStoreMode 缺省为空、ValidateSet 为 空 / redis / db_first。
    包装层 splat 键与下游 param 块逐字相同的 AST 契约在 k8s_client_entry_contract.tests.ps1。
    全部在进程内跑:k8s_deploy.ps1 由临时目录里的假脚本顶替,不碰任何集群。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/dev_tools_route_switch_forwarding.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

$devToolsPath = Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'
$devToolsAst = [System.Management.Automation.Language.Parser]::ParseFile($devToolsPath, [ref]$null, [ref]$null)
$routeSwitchNames = @('RouteStoreMode', 'RouteReconcileArm', 'RouteReadRequiresReady', 'DbPlacementRequireRouteReady', 'GuildRequireRouteReady', 'ConfirmRouteSwitchChange')

function Get-DevToolsFunction([string]$Name) {
    $definition = $devToolsAst.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $Name }, $true)
    if ($null -eq $definition) { throw "dev_tools.ps1 里找不到函数 $Name(契约测试的前提被改掉了)" }
    return $definition
}

# 用假 k8s_deploy.ps1 执行 Invoke-K8sDeploy:假脚本打印每个 D1 参数是否被绑定、绑定成什么值。
function Invoke-DevToolsK8sDeployWith {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-route-switch-passthrough-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        $fakeBody = @(
            'param($RouteStoreMode = "<unset>", [switch]$RouteReconcileArm, [switch]$RouteReadRequiresReady, [switch]$DbPlacementRequireRouteReady, [switch]$GuildRequireRouteReady, [switch]$ConfirmRouteSwitchChange)',
            '"RouteStoreMode=$RouteStoreMode"',
            '"RouteReconcileArm=$($PSBoundParameters.ContainsKey(''RouteReconcileArm''))"',
            '"RouteReadRequiresReady=$($PSBoundParameters.ContainsKey(''RouteReadRequiresReady''))"',
            '"DbPlacementRequireRouteReady=$($PSBoundParameters.ContainsKey(''DbPlacementRequireRouteReady''))"',
            '"GuildRequireRouteReady=$($PSBoundParameters.ContainsKey(''GuildRequireRouteReady''))"',
            '"ConfirmRouteSwitchChange=$($PSBoundParameters.ContainsKey(''ConfirmRouteSwitchChange''))"'
        )
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_deploy.ps1') -Encoding utf8 -Value $fakeBody
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        . ([scriptblock]::Create((Get-DevToolsFunction 'Invoke-K8sDeploy').Extent.Text))
        return (Invoke-K8sDeploy -K8sCommand 'zone-up' | Out-String)
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'param 块:六个 D1 参数都在;-RouteStoreMode 缺省为空(不透传),ValidateSet 为 空 / redis / db_first;其余为 switch' {
    $params = @{}
    foreach ($p in $devToolsAst.ParamBlock.Parameters) { $params[$p.Name.VariablePath.UserPath] = $p }
    foreach ($name in $routeSwitchNames) {
        Assert-True -Condition $params.ContainsKey($name) -Because "dev_tools.ps1 的 param 块必须有 -$name"
    }
    $mode = $params['RouteStoreMode']
    Assert-Equal -Expected '' -Actual $mode.DefaultValue.Value -Because '-RouteStoreMode 缺省必须为空:默认值(2026-10-21 起 db_first)只留在 k8s_deploy.ps1 一处'
    $validateSet = $mode.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' }
    Assert-Equal -Expected '|redis|db_first' -Actual (@($validateSet.PositionalArguments | ForEach-Object { $_.Value }) -join '|') -Because 'ValidateSet 必须是 空 / redis / db_first'
    foreach ($name in $routeSwitchNames | Where-Object { $_ -ne 'RouteStoreMode' }) {
        Assert-Equal -Expected 'SwitchParameter' -Actual $params[$name].StaticType.Name -Because "-$name 必须是 switch"
    }
}

Test-Case 'Invoke-K8sDeploy:-RouteStoreMode 非空才透传,五个 switch 只在被指定时透传;没给就一个都不传' {
    $unset = Invoke-DevToolsK8sDeployWith -Variables @{}
    foreach ($expected in @('RouteStoreMode=<unset>', 'RouteReconcileArm=False', 'RouteReadRequiresReady=False', 'DbPlacementRequireRouteReady=False',
            'GuildRequireRouteReady=False', 'ConfirmRouteSwitchChange=False')) {
        Assert-Match -Text $unset -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "没给就不透传($expected):默认值只在 k8s_deploy.ps1 一处"
    }
    $blank = Invoke-DevToolsK8sDeployWith -Variables @{ RouteStoreMode = '   ' }
    Assert-Match -Text $blank -Pattern '(?m)^RouteStoreMode=<unset>\r?$' -Because '空白字符串不透传'

    $all = Invoke-DevToolsK8sDeployWith -Variables @{
        RouteStoreMode = 'db_first'; RouteReconcileArm = $true; RouteReadRequiresReady = $true; DbPlacementRequireRouteReady = $true
        GuildRequireRouteReady = $true; ConfirmRouteSwitchChange = $true
    }
    foreach ($expected in @('RouteStoreMode=db_first', 'RouteReconcileArm=True', 'RouteReadRequiresReady=True', 'DbPlacementRequireRouteReady=True',
            'GuildRequireRouteReady=True', 'ConfirmRouteSwitchChange=True')) {
        Assert-Match -Text $all -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "给了就原样透传($expected),漏一个就被粘滞核对拒绝或把线上开关翻掉"
    }
    $redis = Invoke-DevToolsK8sDeployWith -Variables @{ RouteStoreMode = 'redis' }
    Assert-Match -Text $redis -Pattern '(?m)^RouteStoreMode=redis\r?$' -Because '显式 redis 同样原样透传:取值是否允许只由 k8s_deploy.ps1 一处判(全球部署入口闸会拒绝它)'
}

Test-Case 'k8s-zone-rollback:六个 D1 参数显式给了即在调用回档脚本之前拒绝(k8s_zone_rollback.ps1 不声明它们,Step 6 按 k8s_deploy 缺省重建)' {
    # 被静默丢掉的 -ConfirmRouteSwitchChange 最危险:Step 6 要到 namespace 已删、库已回档之后才被粘滞核对拒绝(review B3-1 / OPS-3)。
    $clauses = @($devToolsAst.FindAll({ param($node) $node -is [System.Management.Automation.Language.SwitchStatementAst] }, $true) |
        ForEach-Object { $_.Clauses } | Where-Object { $_.Item1 -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $_.Item1.Value -eq 'k8s-zone-rollback' })
    Assert-Equal -Expected 1 -Actual $clauses.Count -Because 'dev_tools.ps1 里应当恰好一个 k8s-zone-rollback 分支'
    $body = $clauses[0].Item2
    $text = $body.Extent.Text
    Assert-Match -Text $text -Pattern 'if \(-not \[string\]::IsNullOrWhiteSpace\(\$RouteStoreMode\)\)\s*\{\s*\$rollbackRouteSwitches \+= ''-RouteStoreMode''\s*\}' -Because '-RouteStoreMode 非空即拒绝'
    foreach ($name in $routeSwitchNames | Where-Object { $_ -ne 'RouteStoreMode' }) {
        Assert-Match -Text $text -Pattern ('if \(\$' + $name + '\)\s*\{\s*\$rollbackRouteSwitches \+= ''-' + $name + '''\s*\}') -Because "-$name 被指定即拒绝,不静默丢掉"
    }
    $guard = $body.Find({ param($node) $node -is [System.Management.Automation.Language.IfStatementAst] -and $node.Clauses[0].Item1.Extent.Text -match '\$rollbackRouteSwitches\.Count -gt 0' }, $true)
    Assert-True -Condition ($null -ne $guard -and $guard.Extent.Text -match '\bthrow\b') -Because '给了任一 D1 开关就 throw'
    $rbArgs = $body.Find({ param($node) $node -is [System.Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$rbArgs' }, $true)
    Assert-True -Condition ($null -ne $rbArgs -and $guard.Extent.StartOffset -lt $rbArgs.Extent.StartOffset) -Because '拒绝必须早于组装回档脚本参数(更早于调用回档脚本,尚未做任何操作)'
    Assert-NotMatch -Text $text -Pattern '\$rbArgs\.(RouteStoreMode|RouteReconcileArm|RouteReadRequiresReady|DbPlacementRequireRouteReady|GuildRequireRouteReady|ConfirmRouteSwitchChange)\b' -Because '回档脚本不声明这六个参数,不得往 $rbArgs 里塞(splat 会以「找不到参数」失败)'
}

Test-Case 'Invoke-K8sImage:六个 D1 参数显式给了即拒绝(k8s_image.ps1 不部署 Go 服务),报错点名参数' {
    $text = (Get-DevToolsFunction 'Invoke-K8sImage').Extent.Text
    Assert-Match -Text $text -Pattern 'if \(-not \[string\]::IsNullOrWhiteSpace\(\$RouteStoreMode\)\)\s*\{\s*\$notForwarded \+= ''-RouteStoreMode''\s*\}' -Because '-RouteStoreMode 非空即拒绝'
    foreach ($name in $routeSwitchNames | Where-Object { $_ -ne 'RouteStoreMode' }) {
        Assert-Match -Text $text -Pattern ('if \(\$' + $name + '\)\s*\{\s*\$notForwarded \+= ''-' + $name + '''\s*\}') -Because "-$name 被指定即拒绝,不静默吞掉"
    }
}

exit (Complete-TestRun -SuiteName 'dev_tools D1 route switch forwarding')
