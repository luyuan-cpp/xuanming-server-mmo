#requires -Version 7
<#
.SYNOPSIS
    login 第三方登录(微信 / QQ)防误关闸与 dev_tools.ps1 透传的契约测试。

.DESCRIPTION
    k8s_deploy.ps1 的 -LoginWeChatAppId / -LoginWeChatEndpoint / -LoginWeChatAllowOpenId / -LoginQQAppId / -LoginQQEndpoint /
    -LoginQQAllowOpenId 不粘滞。2026-10-18 起包装入口照传,线上已开的第三方登录会被本次参数关掉或改掉时 fail-closed。本文件钉住:
      - 判据(lib/k8s_client_entry.ps1 第 9 节):ConvertFrom-LoginAuthProvidersYaml / Get-LoginAuthProviderDrift 两个纯函数的每条
        判定与报错文本;Get-LoginAuthProviderLiveState 只读两次 kubectl(ConfigMap -o json、Secret -o name),读不全即 throw;
      - 生成器(k8s_deploy.ps1 New-LoginAuthProvidersYaml)与解析器口径一致:同一组参数渲染再解析,差异为零,照传串还原参数;
      - JSON 往返(Providers 变成 pscustomobject)判定不变;
      - 报错与照传串不回显手改进 ConfigMap 的任意文本(内联 AppSecret、带用户信息的 Endpoint、带空白的 AppId);
      - k8s_deploy.ps1 Assert-LoginAuthProviderClusterState:全部 zone 判完再一次拒绝(all-up 不留半截部署),
        -ConfirmLoginAuthProviderChange 只告警放行;只在 Assert-ClientEntryDeployPreflight 的非 DryRun 分支、本次部署 login 时调用;
      - 库契约里的三个名字与 k8s_deploy.ps1 的 $script:GoSvcInfo.login / $script:LoginOAuthSecretName 逐字相等;
      - dev_tools.ps1 Invoke-K8sDeploy 非空 / 被指定才透传这七个参数;Invoke-K8sImage 在调 k8s_image.ps1 之前逐个拒绝它们。
    2026-11-12 起同一套判据也覆盖小游戏两段(-LoginWeChatMiniGame* / -LoginDouyinMiniGame*,AuthProviders.WeChatMiniGame / DouyinMiniGame),
    本文件的往返、差异、照传串、防误关闸与两处包装入口用例都带上了它们。
    包装层 splat 键的 AST 契约在 k8s_client_entry_contract.tests.ps1。
    全部在进程内跑,kubectl 一律由同名替身函数顶替(函数先于外部命令解析),不碰任何集群。
    负向用例一律断错误文本,不只断有没有抛异常。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) "lib" "k8s_client_entry.ps1")

Write-Host ""
Write-Host "=== login third-party auth (WeChat / QQ) anti-disable guard contract tests ==="

$DeployScriptPath = Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'
$deployParseErrors = $null
$DeployAst = [System.Management.Automation.Language.Parser]::ParseFile($DeployScriptPath, [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败: $($deployParseErrors | Out-String)" }

# 被测的 k8s_deploy.ps1 生产函数(顶层定义)按语法树取出,定义进本脚本作用域;找不到即前提被改掉,直接失败。
$DeployFunctionNames = @('Get-LoginAuthProviderParamErrors', 'New-LoginAuthProvidersYaml', 'Assert-LoginAuthProviderClusterState', 'Get-ZoneNamespace')
foreach ($definition in $DeployAst.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -in $DeployFunctionNames }) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
foreach ($functionName in $DeployFunctionNames) {
    if (-not (Test-Path "Function:$functionName")) { throw "k8s_deploy.ps1 缺少函数 $functionName(契约测试的前提被改掉了)" }
}

# ─────────────────────────────────────────────────────────────────
# 0. 小工具
# ─────────────────────────────────────────────────────────────────

# 一组本次参数(键:WeChatAppId / WeChatEndpoint / WeChatAllowOpenId / QQAppId / QQEndpoint / QQAllowOpenId,缺省即不给)
# → 生产生成器 New-LoginAuthProvidersYaml 渲染的 AuthProviders 段(与 New-GoSvcConfigMapYaml 同一调用口径)。
function New-AuthYaml {
    param([hashtable]$P = @{}, [string]$ReleaseProfile = 'dev')
    return New-LoginAuthProvidersYaml -WeChatAppId ([string]$P.WeChatAppId) -WeChatEndpoint ([string]$P.WeChatEndpoint) `
        -WeChatAllowOpenId ([bool]$P.WeChatAllowOpenId) -QQAppId ([string]$P.QQAppId) -QQEndpoint ([string]$P.QQEndpoint) `
        -QQAllowOpenId ([bool]$P.QQAllowOpenId) `
        -WeChatMiniGameAppId ([string]$P.WeChatMiniGameAppId) -WeChatMiniGameEndpoint ([string]$P.WeChatMiniGameEndpoint) `
        -WeChatMiniGameAllowOpenId ([bool]$P.WeChatMiniGameAllowOpenId) `
        -DouyinMiniGameAppId ([string]$P.DouyinMiniGameAppId) -DouyinMiniGameEndpoint ([string]$P.DouyinMiniGameEndpoint) `
        -DouyinMiniGameAllowOpenId ([bool]$P.DouyinMiniGameAllowOpenId) `
        -AppleClientId ([string]$P.AppleClientId) -AppleEndpoint ([string]$P.AppleEndpoint) `
        -GoogleClientId ([string]$P.GoogleClientId) -GoogleEndpoint ([string]$P.GoogleEndpoint) `
        -ReleaseProfile $ReleaseProfile -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET'
}

# 把 AuthProviders 段嵌进一份 login.yaml 的前后文(插入点同 New-GoSvcConfigMapYaml:RequireClientEndpoint 之后、Kafka 之前)。
# 前面带一个假口令:断言它从不进报错、照传串与读回的现状。
function New-LoginYamlWith {
    param([AllowEmptyString()][string]$AuthYaml = '')
    $lines = @('Name: login.rpc', 'Secrets:', '  InternalAuth:', '    Value: "contract-guard-secret-never-echoed"', 'RequireClientEndpoint: false')
    if (-not [string]::IsNullOrEmpty($AuthYaml)) { $lines += @($AuthYaml -split "`r?`n") }
    $lines += @('Kafka:', '  Brokers:', '    - "kafka.contract-infra:9092"')
    return ($lines -join "`n")
}

function Get-Drift {
    param([Parameter(Mandatory = $true)]$Live, [bool]$Secret = $false, [hashtable]$P = @{})
    return Get-LoginAuthProviderDrift -Live $Live -OAuthSecretPresent $Secret -WeChatAppId ([string]$P.WeChatAppId) `
        -WeChatEndpoint ([string]$P.WeChatEndpoint) -WeChatAllowOpenId ([bool]$P.WeChatAllowOpenId) -QQAppId ([string]$P.QQAppId) `
        -QQEndpoint ([string]$P.QQEndpoint) -QQAllowOpenId ([bool]$P.QQAllowOpenId) `
        -WeChatMiniGameAppId ([string]$P.WeChatMiniGameAppId) -WeChatMiniGameEndpoint ([string]$P.WeChatMiniGameEndpoint) `
        -WeChatMiniGameAllowOpenId ([bool]$P.WeChatMiniGameAllowOpenId) `
        -DouyinMiniGameAppId ([string]$P.DouyinMiniGameAppId) -DouyinMiniGameEndpoint ([string]$P.DouyinMiniGameEndpoint) `
        -DouyinMiniGameAllowOpenId ([bool]$P.DouyinMiniGameAllowOpenId) `
        -AppleClientId ([string]$P.AppleClientId) -AppleEndpoint ([string]$P.AppleEndpoint) `
        -GoogleClientId ([string]$P.GoogleClientId) -GoogleEndpoint ([string]$P.GoogleEndpoint)
}

# 在 Base 上改几项 / 去掉几项,返回新表(不改 Base)。
function Get-Params {
    param([Parameter(Mandatory = $true)][hashtable]$Base, [hashtable]$Set = @{}, [string[]]$Remove = @())
    $copy = $Base.Clone()
    foreach ($key in $Remove) { $copy.Remove($key) }
    foreach ($key in $Set.Keys) { $copy[$key] = $Set[$key] }
    return $copy
}

function New-LoginConfigMapJson {
    param([AllowEmptyString()][string]$LoginYaml, [switch]$WithoutConfigKey)
    $data = if ($WithoutConfigKey) { [ordered]@{ 'other.yaml' = 'x: 1' } } else { [ordered]@{ 'login.yaml' = $LoginYaml } }
    return ([ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = 'go-svc-login-config' }; data = $data } | ConvertTo-Json -Depth 10)
}

$FullParams = @{ WeChatAppId = 'wx0123456789abcdef'; WeChatEndpoint = 'https://wechat-mock.contract.test:18090'; WeChatAllowOpenId = $true; QQAppId = '100000001' }
$FullKeepText = '-LoginWeChatAppId wx0123456789abcdef -LoginWeChatEndpoint https://wechat-mock.contract.test:18090 -LoginWeChatAllowOpenId -LoginQQAppId 100000001'
$LiveFull = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $FullParams))
$LiveQQOnly = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P @{ QQAppId = '100000001' }))
$LiveNone = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith)

# ─────────────────────────────────────────────────────────────────
# 1. 生成器 ↔ 解析器往返
# ─────────────────────────────────────────────────────────────────

$RoundTripCases = @(
    @{ Label = '只开微信'; P = @{ WeChatAppId = 'wx0123456789abcdef' }; Keep = '-LoginWeChatAppId wx0123456789abcdef' },
    @{ Label = '只开 QQ'; P = @{ QQAppId = '100000001' }; Keep = '-LoginQQAppId 100000001' },
    @{ Label = '两个都开,带沙箱 Endpoint(含路径、dev 档 http)与 openid 回落'
        P = @{ WeChatAppId = 'wx0123456789abcdef'; WeChatEndpoint = 'http://wechat-mock.contract.test:18090/sns'; WeChatAllowOpenId = $true
            QQAppId = '100000001'; QQEndpoint = 'https://qq-mock.contract.test'; QQAllowOpenId = $true }
        Keep = '-LoginWeChatAppId wx0123456789abcdef -LoginWeChatEndpoint http://wechat-mock.contract.test:18090/sns -LoginWeChatAllowOpenId -LoginQQAppId 100000001 -LoginQQEndpoint https://qq-mock.contract.test -LoginQQAllowOpenId' },
    @{ Label = '只开两个小游戏'; P = @{ WeChatMiniGameAppId = 'wxfedcba9876543210'; DouyinMiniGameAppId = 'tt0123456789abcdef' }
        Keep = '-LoginWeChatMiniGameAppId wxfedcba9876543210 -LoginDouyinMiniGameAppId tt0123456789abcdef' },
    @{ Label = '四个都开,小游戏带沙箱 Endpoint(dev 档 http)与 openid 回落(照传串按微信 / QQ / 微信小游戏 / 抖音小游戏的顺序)'
        P = @{ WeChatAppId = 'wx0123456789abcdef'; QQAppId = '100000001'
            WeChatMiniGameAppId = 'wxfedcba9876543210'; WeChatMiniGameEndpoint = 'http://wx-mini-mock.contract.test:18090'; WeChatMiniGameAllowOpenId = $true
            DouyinMiniGameAppId = 'tt0123456789abcdef'; DouyinMiniGameEndpoint = 'https://dy-mini-mock.contract.test'; DouyinMiniGameAllowOpenId = $true }
        Keep = '-LoginWeChatAppId wx0123456789abcdef -LoginQQAppId 100000001 -LoginWeChatMiniGameAppId wxfedcba9876543210 -LoginWeChatMiniGameEndpoint http://wx-mini-mock.contract.test:18090 -LoginWeChatMiniGameAllowOpenId -LoginDouyinMiniGameAppId tt0123456789abcdef -LoginDouyinMiniGameEndpoint https://dy-mini-mock.contract.test -LoginDouyinMiniGameAllowOpenId' },
    @{ Label = '只开 Apple / Google,Apple 带沙箱 Endpoint(dev 档 http)'
        P = @{ AppleClientId = 'com.example.mmorpg'; AppleEndpoint = 'http://apple-mock.contract.test:18090'; GoogleClientId = '1-abc.apps.googleusercontent.com' }
        Keep = '-LoginAppleClientId com.example.mmorpg -LoginAppleEndpoint http://apple-mock.contract.test:18090 -LoginGoogleClientId 1-abc.apps.googleusercontent.com' },
    @{ Label = '六个都开(照传串按微信 / QQ / 两个小游戏 / Apple / Google 的顺序)'
        P = @{ WeChatAppId = 'wx0123456789abcdef'; QQAppId = '100000001'; WeChatMiniGameAppId = 'wxfedcba9876543210'; DouyinMiniGameAppId = 'tt0123456789abcdef'
            AppleClientId = 'com.example.mmorpg'; GoogleClientId = '1-abc.apps.googleusercontent.com'; GoogleEndpoint = 'https://google-mock.contract.test' }
        Keep = '-LoginWeChatAppId wx0123456789abcdef -LoginQQAppId 100000001 -LoginWeChatMiniGameAppId wxfedcba9876543210 -LoginDouyinMiniGameAppId tt0123456789abcdef -LoginAppleClientId com.example.mmorpg -LoginGoogleClientId 1-abc.apps.googleusercontent.com -LoginGoogleEndpoint https://google-mock.contract.test' }
)
foreach ($roundTrip in $RoundTripCases) {
    Test-Case "生成器 ↔ 解析器往返:$($roundTrip.Label) → 同一组参数零差异,照传串还原参数(LF 与 CRLF 都认)" {
        $authYaml = New-AuthYaml -P $roundTrip.P
        Assert-Match -Text $authYaml -Pattern '(?m)^AuthProviders:$' -Because '前提:生成器确实写了 AuthProviders 段'
        foreach ($loginYaml in @((New-LoginYamlWith -AuthYaml $authYaml), ((New-LoginYamlWith -AuthYaml $authYaml) -replace "`n", "`r`n"))) {
            $live = ConvertFrom-LoginAuthProvidersYaml -LoginYaml $loginYaml
            Assert-True -Condition $live.Present -Because '有 AuthProviders 段即 Present'
            Assert-Equal -Expected '' -Actual (@($live.Errors) -join ' | ') -Because '生成器写的形状必须能被解析器认出,否则每次重部署都被误判为判不出'
            $drift = Get-Drift -Live $live -Secret ([bool]$roundTrip.P.WeChatAppId) -P $roundTrip.P
            Assert-Equal -Expected '' -Actual ($drift.Drifts -join ' | ') -Because '照传同一组参数不能被判为差异,否则照传也过不了闸'
            Assert-Equal -Expected $roundTrip.Keep -Actual $drift.KeepArgs -Because '照传串必须还原出渲染它的那组参数(报错里让操作者照抄)'
        }
    }
}

Test-Case '没有 AuthProviders:Present 为假;不给参数零差异;线上没开、本次新开不算差异(开启是显式的),照传串为空' {
    Assert-True -Condition (-not $LiveNone.Present) -Because 'login.yaml 里没有顶格 AuthProviders'
    Assert-Equal -Expected 0 -Actual @($LiveNone.Errors).Count -Because '没有这一段不是错误'
    $none = Get-Drift -Live $LiveNone
    Assert-Equal -Expected 0 -Actual $none.Drifts.Count -Because '两边都没开'
    Assert-Equal -Expected '' -Actual $none.KeepArgs -Because '线上没有可照传的 provider'
    $enable = Get-Drift -Live $LiveNone -P $FullParams
    Assert-Equal -Expected 0 -Actual $enable.Drifts.Count -Because '新开第三方登录不需要确认'
    $empty = ConvertFrom-LoginAuthProvidersYaml -LoginYaml ''
    Assert-True -Condition (-not $empty.Present -and @($empty.Errors).Count -eq 0) -Because '空文本同样是没开'
}

# ─────────────────────────────────────────────────────────────────
# 2. 差异判定(线上 = 生产生成器渲染的段)
# ─────────────────────────────────────────────────────────────────

$DriftCases = @(
    @{ Label = '全部漏传:两个 provider 都关闭,微信还删 Secret'; Live = $LiveFull; Secret = $true; P = @{}
        Patterns = @('^线上已开 AuthProviders\.WeChat,本次没给 -LoginWeChatAppId:部署后 WeChat 登录随新 Pod 关闭,并删除 Secret login-oauth$',
            '^线上已开 AuthProviders\.QQ,本次没给 -LoginQQAppId:部署后 QQ 登录随新 Pod 关闭$') },
    @{ Label = '只漏传 QQ'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Remove 'QQAppId')
        Patterns = @('^线上已开 AuthProviders\.QQ,本次没给 -LoginQQAppId') },
    @{ Label = '漏传 Endpoint:沙箱改连生产'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Remove 'WeChatEndpoint')
        Patterns = @('^AuthProviders\.WeChat\.Endpoint:线上指向自定义 Endpoint\(沙箱\),本次没给 -LoginWeChatEndpoint 会改连生产地址$') },
    @{ Label = '新给 Endpoint:生产改连自定义地址'; Live = $LiveQQOnly; P = @{ QQAppId = '100000001'; QQEndpoint = 'https://qq-mock.contract.test' }
        Patterns = @('^AuthProviders\.QQ\.Endpoint:线上走生产地址,本次 -LoginQQEndpoint 会改连自定义 Endpoint$') },
    @{ Label = '换了 Endpoint'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Set @{ WeChatEndpoint = 'https://other-mock.contract.test' })
        Patterns = @('^AuthProviders\.WeChat\.Endpoint:本次 -LoginWeChatEndpoint 与线上的自定义 Endpoint 不同$') },
    @{ Label = '漏传 AllowOpenId:RequireUnionId 翻回 true'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Remove 'WeChatAllowOpenId')
        Patterns = @('^AuthProviders\.WeChat\.RequireUnionId:线上 RequireUnionId: false\(按 openid 建账号\),本次没给 -LoginWeChatAllowOpenId 会翻回 true') },
    @{ Label = '新加 AllowOpenId:RequireUnionId 改成 false'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Set @{ QQAllowOpenId = $true })
        Patterns = @('^AuthProviders\.QQ\.RequireUnionId:线上 RequireUnionId 为 true,本次 -LoginQQAllowOpenId 会改成 false') },
    @{ Label = '换了 AppId'; Live = $LiveFull; Secret = $true; P = (Get-Params -Base $FullParams -Set @{ WeChatAppId = 'wx9999999999999999' })
        Patterns = @('^AuthProviders\.WeChat\.AppId 与线上不同\(本次 -LoginWeChatAppId 换了应用号\)$') },
    @{ Label = '线上只开 QQ、本次加开微信'; Live = $LiveQQOnly; P = $FullParams; Patterns = @() },
    @{ Label = 'Secret 残留(ConfigMap 没有微信)、本次不开微信'; Live = $LiveNone; Secret = $true; P = @{}
        Patterns = @('^namespace 里有 Secret login-oauth\(微信 AppSecret\),login ConfigMap 却没有 AuthProviders\.WeChat[^\r\n]*本次没给 -LoginWeChatAppId 会删掉它$') },
    @{ Label = 'Secret 残留、本次开微信'; Live = $LiveNone; Secret = $true; P = @{ WeChatAppId = 'wx0123456789abcdef' }; Patterns = @() },
    @{ Label = '照传全部'; Live = $LiveFull; Secret = $true; P = $FullParams; Patterns = @() }
)
foreach ($driftCase in $DriftCases) {
    Test-Case "差异判定:$($driftCase.Label)" {
        $drift = Get-Drift -Live $driftCase.Live -Secret ([bool]$driftCase.Secret) -P $driftCase.P
        Assert-Equal -Expected $driftCase.Patterns.Count -Actual $drift.Drifts.Count -Because "差异逐项计数(实际: $($drift.Drifts -join ' | '))"
        foreach ($pattern in $driftCase.Patterns) {
            Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match $pattern }).Count -eq 1) -Because "恰有一项匹配 /$pattern/(实际: $($drift.Drifts -join ' | '))"
        }
    }
}

# 小游戏两段(AuthProviders.WeChatMiniGame / DouyinMiniGame)的差异判定:规则与微信 / QQ 相同,关闭时各删自己的 AppSecret Secret。
$MiniParams = @{ WeChatMiniGameAppId = 'wxfedcba9876543210'; WeChatMiniGameEndpoint = 'https://wx-mini-mock.contract.test'
    DouyinMiniGameAppId = 'tt0123456789abcdef'; DouyinMiniGameAllowOpenId = $true }
$MiniKeepText = '-LoginWeChatMiniGameAppId wxfedcba9876543210 -LoginWeChatMiniGameEndpoint https://wx-mini-mock.contract.test -LoginDouyinMiniGameAppId tt0123456789abcdef -LoginDouyinMiniGameAllowOpenId'
$LiveMini = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $MiniParams))
$MiniDriftCases = @(
    @{ Label = '全部漏传:两个小游戏都关闭,各删自己的 AppSecret Secret'; Live = $LiveMini; P = @{}
        Patterns = @('^线上已开 AuthProviders\.WeChatMiniGame,本次没给 -LoginWeChatMiniGameAppId:部署后 WeChatMiniGame 登录随新 Pod 关闭,并删除它的 AppSecret Secret$',
            '^线上已开 AuthProviders\.DouyinMiniGame,本次没给 -LoginDouyinMiniGameAppId:部署后 DouyinMiniGame 登录随新 Pod 关闭,并删除它的 AppSecret Secret$') },
    @{ Label = '漏传微信小游戏的 Endpoint:沙箱改连生产'; Live = $LiveMini; P = (Get-Params -Base $MiniParams -Remove 'WeChatMiniGameEndpoint')
        Patterns = @('^AuthProviders\.WeChatMiniGame\.Endpoint:线上指向自定义 Endpoint\(沙箱\),本次没给 -LoginWeChatMiniGameEndpoint 会改连生产地址$') },
    @{ Label = '漏传抖音小游戏的 AllowOpenId:RequireUnionId 翻回 true'; Live = $LiveMini; P = (Get-Params -Base $MiniParams -Remove 'DouyinMiniGameAllowOpenId')
        Patterns = @('^AuthProviders\.DouyinMiniGame\.RequireUnionId:线上 RequireUnionId: false\(按 openid 建账号\),本次没给 -LoginDouyinMiniGameAllowOpenId 会翻回 true') },
    @{ Label = '换了微信小游戏的 AppId'; Live = $LiveMini; P = (Get-Params -Base $MiniParams -Set @{ WeChatMiniGameAppId = 'wx1111111111111111' })
        Patterns = @('^AuthProviders\.WeChatMiniGame\.AppId 与线上不同\(本次 -LoginWeChatMiniGameAppId 换了应用号\)$') },
    @{ Label = '线上只开微信 App 与 QQ、本次加开两个小游戏'; Live = $LiveFull; Secret = $true; P = ($FullParams + $MiniParams); Patterns = @() },
    @{ Label = '线上开着小游戏、本次照传小游戏却漏传微信 App 与 QQ(只报那两段)'; Live = (ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P ($FullParams + $MiniParams))))
        Secret = $true; P = $MiniParams
        Patterns = @('^线上已开 AuthProviders\.WeChat,本次没给 -LoginWeChatAppId', '^线上已开 AuthProviders\.QQ,本次没给 -LoginQQAppId') },
    @{ Label = '照传全部小游戏'; Live = $LiveMini; P = $MiniParams; Patterns = @() }
)
foreach ($driftCase in $MiniDriftCases) {
    Test-Case "差异判定(小游戏):$($driftCase.Label)" {
        $drift = Get-Drift -Live $driftCase.Live -Secret ([bool]$driftCase.Secret) -P $driftCase.P
        Assert-Equal -Expected $driftCase.Patterns.Count -Actual $drift.Drifts.Count -Because "差异逐项计数(实际: $($drift.Drifts -join ' | '))"
        foreach ($pattern in $driftCase.Patterns) {
            Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match $pattern }).Count -eq 1) -Because "恰有一项匹配 /$pattern/(实际: $($drift.Drifts -join ' | '))"
        }
        Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match '生成器不生成的 AuthProviders\.(WeChatMiniGame|DouyinMiniGame)' }).Count -eq 0) -Because '两个小游戏段是生成器生成的,不能被当成「本次会丢掉」的未知 provider'
    }
}

Test-Case '照传串(小游戏):线上只开两个小游戏、本次全部漏传时,照传串还原两段的 AppId / Endpoint / AllowOpenId' {
    $drift = Get-Drift -Live $LiveMini
    Assert-Equal -Expected $MiniKeepText -Actual $drift.KeepArgs -Because '照传串 = 线上现状对应的参数,按微信小游戏、抖音小游戏的顺序'
}

# Apple / Google 两段的差异判定:标识键是 ClientId,没有 RequireUnionId,关闭时没有 Secret 要删。
$IdTokenParams = @{ AppleClientId = 'com.example.mmorpg'; AppleEndpoint = 'https://apple-mock.contract.test'; GoogleClientId = '1-abc.apps.googleusercontent.com' }
$IdTokenKeepText = '-LoginAppleClientId com.example.mmorpg -LoginAppleEndpoint https://apple-mock.contract.test -LoginGoogleClientId 1-abc.apps.googleusercontent.com'
$LiveIdToken = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $IdTokenParams))
# 手改出来的两种形状:Apple 段把标识写成了 AppId;Google 段带着生成器不写的 RequireUnionId。
$LiveIdTokenHandEdited = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (@('AuthProviders:',
            '  Apple:', '    AppId: "com.example.mmorpg"',
            '  Google:', '    ClientId: "1-abc.apps.googleusercontent.com"', '    RequireUnionId: true') -join "`n"))
$IdTokenDriftCases = @(
    @{ Label = '全部漏传:两段都关闭,没有 Secret 要删'; Live = $LiveIdToken; P = @{}
        Patterns = @('^线上已开 AuthProviders\.Apple,本次没给 -LoginAppleClientId:部署后 Apple 登录随新 Pod 关闭$',
            '^线上已开 AuthProviders\.Google,本次没给 -LoginGoogleClientId:部署后 Google 登录随新 Pod 关闭$') },
    @{ Label = '漏传 Apple 的 Endpoint:沙箱改连生产'; Live = $LiveIdToken; P = (Get-Params -Base $IdTokenParams -Remove 'AppleEndpoint')
        Patterns = @('^AuthProviders\.Apple\.Endpoint:线上指向自定义 Endpoint\(沙箱\),本次没给 -LoginAppleEndpoint 会改连生产地址$') },
    @{ Label = '换了 Google 的 ClientId'; Live = $LiveIdToken; P = (Get-Params -Base $IdTokenParams -Set @{ GoogleClientId = '2-xyz.apps.googleusercontent.com' })
        Patterns = @('^AuthProviders\.Google\.ClientId 与线上不同\(本次 -LoginGoogleClientId 换了客户端 ID') },
    @{ Label = '线上 Apple 段写的是 AppId、Google 段带 RequireUnionId:都算生成器不写的键,Apple 的 ClientId 也算不同'; Live = $LiveIdTokenHandEdited
        P = @{ AppleClientId = 'com.example.mmorpg'; GoogleClientId = '1-abc.apps.googleusercontent.com' }
        Patterns = @('^AuthProviders\.Apple\.ClientId 与线上不同', '^线上 AuthProviders\.Apple 有生成器不写的键\(AppId\),本次部署会丢掉$',
            '^线上 AuthProviders\.Google 有生成器不写的键\(RequireUnionId\),本次部署会丢掉$') },
    @{ Label = '线上只开微信 App 与 QQ、本次加开 Apple / Google'; Live = $LiveFull; Secret = $true; P = ($FullParams + $IdTokenParams); Patterns = @() },
    @{ Label = '照传全部'; Live = $LiveIdToken; P = $IdTokenParams; Patterns = @() }
)
foreach ($driftCase in $IdTokenDriftCases) {
    Test-Case "差异判定(Apple / Google):$($driftCase.Label)" {
        $drift = Get-Drift -Live $driftCase.Live -Secret ([bool]$driftCase.Secret) -P $driftCase.P
        Assert-Equal -Expected $driftCase.Patterns.Count -Actual $drift.Drifts.Count -Because "差异逐项计数(实际: $($drift.Drifts -join ' | '))"
        foreach ($pattern in $driftCase.Patterns) {
            Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match $pattern }).Count -eq 1) -Because "恰有一项匹配 /$pattern/(实际: $($drift.Drifts -join ' | '))"
        }
        Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match '生成器不生成的 AuthProviders\.(Apple|Google)|RequireUnionId:' }).Count -eq 0) -Because 'Apple / Google 是生成器生成的段,也没有 unionid 策略可比'
        Assert-NotMatch -Text $drift.KeepArgs -Pattern 'AllowOpenId' -Because 'Apple / Google 没有 AllowOpenId 参数,照传串里不能出现'
    }
}

Test-Case '照传串(Apple / Google):线上只开这两段、本次全部漏传时,照传串还原 ClientId / Endpoint;经 JSON 往返后判定不变' {
    $drift = Get-Drift -Live $LiveIdToken
    Assert-Equal -Expected $IdTokenKeepText -Actual $drift.KeepArgs -Because '照传串 = 线上现状对应的参数,按 Apple、Google 的顺序'
    $restored = [pscustomobject]@{ Live = $LiveIdTokenHandEdited } | ConvertTo-Json -Depth 6 | ConvertFrom-Json
    $p = @{ AppleClientId = 'com.example.mmorpg'; GoogleClientId = '1-abc.apps.googleusercontent.com' }
    Assert-Equal -Expected ((Get-Drift -Live $LiveIdTokenHandEdited -P $p).Drifts -join ' | ') -Actual ((Get-Drift -Live $restored.Live -P $p).Drifts -join ' | ') -Because 'JSON 往返前后的判定必须一致(含手改出来的标识键与 RequireUnionId)'
}

Test-Case '照传串只按线上现状拼,与本次参数无关(全部漏传时报错里照抄它就能保持现状)' {
    $drift = Get-Drift -Live $LiveFull -Secret $true
    Assert-Equal -Expected $FullKeepText -Actual $drift.KeepArgs -Because '照传串 = 线上现状对应的参数'
}

# ─────────────────────────────────────────────────────────────────
# 3. 认不出的形状(fail-closed)、生成器不写的键与 provider、不回显
# ─────────────────────────────────────────────────────────────────

$ParseErrorCases = @(
    @{ Label = 'flow 写法'; Yaml = 'AuthProviders: { WeChat: { AppId: "wx1" } }'; Error = '第 \d+ 行:AuthProviders 不是块映射' },
    @{ Label = '顶格 AuthProviders 出现两次'; Yaml = "AuthProviders:`n  QQ:`n    AppId: `"1`"`nAuthProviders:`n  QQ:`n    AppId: `"2`""; Error = '顶格 AuthProviders 出现 2 次' },
    @{ Label = '制表符缩进'; Yaml = "AuthProviders:`n`tQQ:`n`t`tAppId: `"1`""; Error = '第 \d+ 行:制表符缩进' },
    @{ Label = 'RequireUnionId 带引号(是字符串,login 解析 bool 时拒启)'; Yaml = "AuthProviders:`n  QQ:`n    AppId: `"1`"`n    RequireUnionId: `"false`""; Error = 'AuthProviders\.QQ\.RequireUnionId 不是裸 true / false' },
    @{ Label = '带转义的引号串'; Yaml = "AuthProviders:`n  WeChat:`n    AppId: `"wx\`"1`""; Error = 'AuthProviders\.WeChat\.AppId 的值认不出' },
    @{ Label = '同一 provider 下重复的键'; Yaml = "AuthProviders:`n  QQ:`n    AppId: `"1`"`n    AppId: `"2`""; Error = 'AuthProviders\.QQ\.AppId 出现多次' },
    @{ Label = '同一 provider 出现两次'; Yaml = "AuthProviders:`n  QQ:`n    AppId: `"1`"`n  QQ:`n    AppId: `"2`""; Error = 'AuthProviders\.QQ 出现多次' },
    @{ Label = '三格缩进'; Yaml = "AuthProviders:`n   QQ:`n    AppId: `"1`""; Error = '形状认不出' },
    # login(go-zero conf)读配置不区分大小写:下面两种写法在 login 里照样开着 QQ,解析器只认生成器写法,必须判不出,不能当成没开。
    @{ Label = '顶格键小写(login 照样生效)'; Yaml = "authProviders:`n  QQ:`n    AppId: `"1`""; Error = '第 \d+ 行:顶格键 AuthProviders 的大小写或引号不是生成器的写法' },
    @{ Label = '顶格键带引号(login 照样生效)'; Yaml = "`"AuthProviders`":`n  QQ:`n    AppId: `"1`""; Error = '第 \d+ 行:顶格键 AuthProviders 的大小写或引号不是生成器的写法' },
    # 两种标识键同时出现:login 按 provider 只认其中一种,另一种静默忽略,判不出操作者想要哪一个。
    @{ Label = '同一 provider 同时写 AppId 与 ClientId'; Yaml = "AuthProviders:`n  Apple:`n    ClientId: `"com.example.game`"`n    AppId: `"com.example.other`""; Error = '第 \d+ 行:AuthProviders\.Apple 同时写了 AppId 与 ClientId' }
)
foreach ($parseCase in $ParseErrorCases) {
    Test-Case "认不出的形状:$($parseCase.Label) → Errors 非空,差异里整体算一项「判不出」(fail-closed)" {
        $live = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml $parseCase.Yaml)
        Assert-True -Condition $live.Present -Because '有 AuthProviders 键'
        Assert-True -Condition (@($live.Errors | Where-Object { $_ -match $parseCase.Error }).Count -ge 1) -Because "Errors 要点名 /$($parseCase.Error)/(实际: $(@($live.Errors) -join ' | '))"
        $drift = Get-Drift -Live $live -P @{ QQAppId = '1'; WeChatAppId = 'wx1' }
        Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -match '^线上 login ConfigMap 的 AuthProviders 段认不出\(' }).Count -eq 1) -Because "判不出要算一项差异,不能当成没开第三方登录(实际: $($drift.Drifts -join ' | '))"
    }
}

Test-Case '生成器不写的键(含内联 AppSecret)与 provider:各算一项差异(本次会丢掉);只报键名,值从不回显' {
    $yaml = @('AuthProviders:', '  WeChat:', '    AppId: "wx0123456789abcdef"', '    AppSecretEnv: "LOGIN_WECHAT_APP_SECRET"',
        '    AppSecret: "inline-secret-must-not-echo"   # 手改进去的内联密钥', '  Steam:', '    AppId: "480"') -join "`n"
    $live = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml $yaml)
    Assert-Equal -Expected 0 -Actual @($live.Errors).Count -Because "形状本身认得出(错误: $(@($live.Errors) -join ' | '))"
    $drift = Get-Drift -Live $live -Secret $true -P @{ WeChatAppId = 'wx0123456789abcdef' }
    Assert-Equal -Expected 2 -Actual $drift.Drifts.Count -Because "逐项计数(实际: $($drift.Drifts -join ' | '))"
    Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -ceq '线上 AuthProviders.WeChat 有生成器不写的键(AppSecret),本次部署会丢掉' }).Count -eq 1) -Because '未知键按名字列出'
    Assert-True -Condition (@($drift.Drifts | Where-Object { $_ -ceq '线上有生成器不生成的 AuthProviders.Steam,本次部署会丢掉' }).Count -eq 1) -Because '未知 provider 按名字列出'
    Assert-NotMatch -Text (($drift.Drifts -join "`n") + "`n" + $drift.KeepArgs + "`n" + (@($live.Errors) -join "`n")) -Pattern 'inline-secret-must-not-echo|contract-guard-secret-never-echoed' -Because '报错、照传串、解析错误都不回显值'
}

Test-Case '照传串只在值落进安全字符集时回显:带空白 / 分号的 AppId、带用户信息或 shell 元字符的 Endpoint 一律写 <见 ConfigMap>' {
    $yaml = @('AuthProviders:', '  WeChat:', '    AppId: "wx 01;rm"', '    Endpoint: "https://user:pw@wechat-mock.contract.test"') -join "`n"
    $live = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml $yaml)
    $drift = Get-Drift -Live $live
    Assert-Equal -Expected '-LoginWeChatAppId <见 ConfigMap> -LoginWeChatEndpoint <见 ConfigMap>' -Actual $drift.KeepArgs -Because '手改进去的任意文本不打进日志'
    Assert-NotMatch -Text (($drift.Drifts -join "`n") + "`n" + $drift.KeepArgs) -Pattern 'user:pw|;rm' -Because '报错同样不回显'
    # 照传串是让人照抄的:Endpoint 只按 k8s_deploy.ps1 参数门禁的同一形状回显,shell 元字符、查询串一律不回显。
    foreach ($endpoint in @('https://a.contract.test/;rm', 'https://a.contract.test/$(x)', 'https://a.contract.test/p|q', 'https://a.contract.test/?q=1')) {
        $metaLive = ConvertFrom-LoginAuthProvidersYaml -LoginYaml (New-LoginYamlWith -AuthYaml (@('AuthProviders:', '  QQ:', '    AppId: "100000001"', "    Endpoint: `"$endpoint`"") -join "`n"))
        $metaDrift = Get-Drift -Live $metaLive
        Assert-Equal -Expected '-LoginQQAppId 100000001 -LoginQQEndpoint <见 ConfigMap>' -Actual $metaDrift.KeepArgs -Because "Endpoint '$endpoint' 不在参数门禁的形状里,不回显"
    }
}

Test-Case 'JSON 往返:线上现状经 ConvertTo-Json / ConvertFrom-Json(Providers 变成 pscustomobject)后判定与照传串不变' {
    $state = [pscustomobject]@{ ConfigMapPresent = $true; Live = $LiveFull; OAuthSecretPresent = $true }
    $restored = $state | ConvertTo-Json -Depth 6 | ConvertFrom-Json
    $direct = Get-Drift -Live $LiveFull -Secret $true
    $roundTripped = Get-Drift -Live $restored.Live -Secret ([bool]$restored.OAuthSecretPresent)
    Assert-Equal -Expected ($direct.Drifts -join ' | ') -Actual ($roundTripped.Drifts -join ' | ') -Because 'the verdict must be identical before and after the JSON round trip'
    Assert-Equal -Expected 2 -Actual $roundTripped.Drifts.Count -Because '前提:全部漏传时两个 provider 都算差异'
    Assert-Equal -Expected $FullKeepText -Actual $roundTripped.KeepArgs -Because '照传串同样不变'
    Assert-Equal -Expected 0 -Actual (Get-Drift -Live $restored.Live -Secret $true -P $FullParams).Drifts.Count -Because '照传全部即放行'
}

# ─────────────────────────────────────────────────────────────────
# 4. Get-LoginAuthProviderLiveState:只读两次 kubectl,读不全即 throw 或记成判不出
# ─────────────────────────────────────────────────────────────────

$LiveStateCases = @(
    @{ Label = 'ConfigMap 与 Secret 都不在(首次部署 / namespace 已删)'; ConfigMap = @{ Stdout = '' }; Secret = @{ Stdout = '' }
        Expect = { param($s) Assert-True -Condition (-not $s.ConfigMapPresent -and -not $s.Live.Present -and -not $s.OAuthSecretPresent) -Because '不在即 ConfigMapPresent 为假' } },
    @{ Label = '开着微信,Secret 在'; ConfigMap = @{ Stdout = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $FullParams))) }; Secret = @{ Stdout = 'secret/login-oauth' }
        Expect = {
            param($s)
            Assert-True -Condition ($s.ConfigMapPresent -and $s.Live.Present -and $s.OAuthSecretPresent) -Because '都读到'
            Assert-Equal -Expected 0 -Actual (Get-Drift -Live $s.Live -Secret $s.OAuthSecretPresent -P $FullParams).Drifts.Count -Because '读回的现状与渲染它的参数一致'
        } },
    @{ Label = 'ConfigMap 返回不是 JSON'; ConfigMap = @{ Stdout = 'not-json{' }; Secret = @{ Stdout = '' }
        Expect = { param($s) Assert-True -Condition ($s.ConfigMapPresent -and @($s.Live.Errors | Where-Object { $_ -match '返回不是 JSON' }).Count -eq 1) -Because '判不出,交给调用方 fail-closed' } },
    @{ Label = 'ConfigMap 没有 login.yaml 键'; ConfigMap = @{ Stdout = (New-LoginConfigMapJson -LoginYaml '' -WithoutConfigKey) }; Secret = @{ Stdout = '' }
        Expect = { param($s) Assert-True -Condition ($s.ConfigMapPresent -and @($s.Live.Errors | Where-Object { $_ -match '没有 data\.login\.yaml' }).Count -eq 1) -Because '判不出,交给调用方 fail-closed' } }
)
foreach ($liveCase in $LiveStateCases) {
    Test-Case "Get-LoginAuthProviderLiveState:$($liveCase.Label)" {
        $calls = [System.Collections.Generic.List[object]]::new()
        function Invoke-ClientEntryKubectl {
            param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
            $line = $KubectlArgs -join ' '
            $calls.Add([pscustomobject]@{ Line = $line; KubeContext = $KubeContext; KubeConfig = $KubeConfig; CaptureOutput = $CaptureOutput.IsPresent; AllowFailure = $AllowFailure.IsPresent })
            if ($line -ceq 'get configmap go-svc-login-config -n mmorpg-zone-guard --ignore-not-found -o json') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$liveCase.ConfigMap.Stdout; Stderr = '' } }
            if ($line -ceq 'get secret login-oauth -n mmorpg-zone-guard --ignore-not-found -o name') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$liveCase.Secret.Stdout; Stderr = '' } }
            throw "假 kubectl:没有规则命中这次调用:kubectl $line"
        }
        $state = Get-LoginAuthProviderLiveState -Namespace 'mmorpg-zone-guard' -KubeContext 'contract-ctx' -KubeConfig 'C:/contract/kubeconfig'
        & $liveCase.Expect $state
        Assert-Equal -Expected 2 -Actual $calls.Count -Because '只读 ConfigMap 与 Secret 是否存在各一次'
        foreach ($call in $calls) {
            Assert-True -Condition ($call.Line -cmatch '^get ') -Because '只能查询'
            Assert-True -Condition ($call.CaptureOutput -and -not $call.AllowFailure) -Because 'kubectl 非 0 退出必须 throw(查不到不等于没开)'
            Assert-Equal -Expected 'contract-ctx' -Actual $call.KubeContext -Because '查询必须携带调用方给的 context'
            Assert-Equal -Expected 'C:/contract/kubeconfig' -Actual $call.KubeConfig -Because '查询必须携带调用方给的 kubeconfig'
        }
        Assert-NotMatch -Text (($state | ConvertTo-Json -Depth 8)) -Pattern 'contract-guard-secret-never-echoed' -Because '读回的现状只有 AuthProviders 的公开值,不带 ConfigMap 里别的值'
    }
}

Test-Case 'Get-LoginAuthProviderLiveState:kubectl 失败(集群不可达 / Forbidden)原样 throw,不当成没开' {
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        throw "kubectl failed (exit 1): kubectl $($KubectlArgs -join ' ')`nError from server (Forbidden)"
    }
    $errorText = ''
    try { Get-LoginAuthProviderLiveState -Namespace 'mmorpg-zone-guard' -KubeContext '' -KubeConfig '' | Out-Null } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'kubectl failed \(exit 1\)' -Because '读不到必须让调用方拒绝(fail-closed)'
}

# ─────────────────────────────────────────────────────────────────
# 5. k8s_deploy.ps1 Assert-LoginAuthProviderClusterState(进程内,kubectl 替身)
# ─────────────────────────────────────────────────────────────────

Test-Case 'k8s_deploy 防误关闸:全部 zone 判完再一次拒绝(只列有差异的 zone、给出照传串);-ConfirmLoginAuthProviderChange 只告警放行;照传放行;kubectl 失败即拒绝' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-ctx'
    $KubeConfig = 'C:/contract/kubeconfig'
    $LoginWeChatAppId = ''; $LoginWeChatEndpoint = ''; $LoginWeChatAllowOpenId = [switch]$false
    $LoginQQAppId = ''; $LoginQQEndpoint = ''; $LoginQQAllowOpenId = [switch]$false
    $ConfirmLoginAuthProviderChange = [switch]$false
    $configMaps = @{
        'mmorpg-zone-z1' = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $FullParams)))
        'mmorpg-zone-z2' = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith))
        'mmorpg-zone-z3' = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P @{ QQAppId = '100000001' })))
    }
    $secrets = @{ 'mmorpg-zone-z1' = 'secret/login-oauth' }
    $failNamespace = ''
    $calls = [System.Collections.Generic.List[string]]::new()
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        $line = $KubectlArgs -join ' '
        $calls.Add($line)
        Assert-Equal -Expected 'get' -Actual $KubectlArgs[0] -Because '防误关闸只能查询'
        Assert-Equal -Expected 'contract-ctx' -Actual $KubeContext -Because '查询必须携带部署用的 context'
        $namespace = $KubectlArgs[[array]::IndexOf($KubectlArgs, '-n') + 1]
        if ($namespace -ceq $failNamespace) { throw "kubectl failed (exit 1): kubectl $line" }
        if ($KubectlArgs[1] -ceq 'configmap') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$configMaps[$namespace]; Stderr = '' } }
        if ($KubectlArgs[1] -ceq 'secret') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$secrets[$namespace]; Stderr = '' } }
        throw "假 kubectl:没有规则命中这次调用:kubectl $line"
    }
    $zones = @([pscustomobject]@{ Name = 'z1' }, [pscustomobject]@{ Name = 'z2' }, [pscustomobject]@{ Name = 'z3' })

    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'login 第三方登录现状预检失败\(2 个 zone\),未做任何写操作' -Because '有差异的 zone 逐个计数,一次拒绝'
    Assert-Equal -Expected 2 -Actual @([regex]::Matches($errorText, '(?m)^  - zone ')).Count -Because '每个有差异的 zone 一行'
    Assert-Match -Text $errorText -Pattern ('zone z1\(namespace=mmorpg-zone-z1\):线上已开 AuthProviders\.WeChat[^\r\n]*要保持现状请照传 ' + [regex]::Escape($FullKeepText) + ';确认要关闭或改动请加 -ConfirmLoginAuthProviderChange') -Because 'z1 漏传两个 provider,给出照传串与确认开关'
    Assert-Match -Text $errorText -Pattern 'zone z3\(namespace=mmorpg-zone-z3\):线上已开 AuthProviders\.QQ[^\r\n]*要保持现状请照传 -LoginQQAppId 100000001;' -Because 'z3 只开了 QQ'
    Assert-NotMatch -Text $errorText -Pattern 'zone z2' -Because 'z2 没开第三方登录,不列'
    Assert-Equal -Expected 6 -Actual $calls.Count -Because '三个 zone 都判完再拒绝(all-up 逐 zone 部署,不能留下前几个 zone 已关掉、后面才被拒的半截部署)'

    $ConfirmLoginAuthProviderChange = [switch]$true
    $calls.Clear()
    $records = @(Assert-LoginAuthProviderClusterState -Zones $zones 3>&1)
    $warnings = @($records | Where-Object { $_ -is [System.Management.Automation.WarningRecord] } | ForEach-Object { $_.Message })
    Assert-Equal -Expected 2 -Actual $warnings.Count -Because '确认后不拒绝,每个有差异的 zone 告警一次'
    Assert-True -Condition (@($warnings | Where-Object { $_ -match '^\[login-auth\] -ConfirmLoginAuthProviderChange supplied; allowed: zone z1\(' }).Count -eq 1) -Because '告警写明放行的是什么'

    $ConfirmLoginAuthProviderChange = [switch]$false
    $LoginWeChatAppId = $FullParams.WeChatAppId; $LoginWeChatEndpoint = $FullParams.WeChatEndpoint; $LoginWeChatAllowOpenId = [switch]$true
    $LoginQQAppId = $FullParams.QQAppId
    $kept = @(Assert-LoginAuthProviderClusterState -Zones $zones 3>&1)
    Assert-Equal -Expected 0 -Actual @($kept | Where-Object { $_ -is [System.Management.Automation.WarningRecord] }).Count -Because '照传(z2 / z3 上是新开)不拒绝、不告警'

    $failNamespace = 'mmorpg-zone-z2'
    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'kubectl failed \(exit 1\)' -Because '读不到就拒绝部署(查不到不等于没开)'
}

Test-Case 'k8s_deploy 防误关闸(小游戏):线上开着小游戏、本次漏传就拒绝并给出照传串;照传放行;漏传 AllowOpenId 同样拒绝(六个参数都传进了判据)' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-ctx'
    $KubeConfig = 'C:/contract/kubeconfig'
    $LoginWeChatAppId = ''; $LoginWeChatEndpoint = ''; $LoginWeChatAllowOpenId = [switch]$false
    $LoginQQAppId = ''; $LoginQQEndpoint = ''; $LoginQQAllowOpenId = [switch]$false
    $LoginWeChatMiniGameAppId = ''; $LoginWeChatMiniGameEndpoint = ''; $LoginWeChatMiniGameAllowOpenId = [switch]$false
    $LoginDouyinMiniGameAppId = ''; $LoginDouyinMiniGameEndpoint = ''; $LoginDouyinMiniGameAllowOpenId = [switch]$false
    $ConfirmLoginAuthProviderChange = [switch]$false
    $miniConfigMap = New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $MiniParams))
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        Assert-Equal -Expected 'get' -Actual $KubectlArgs[0] -Because '防误关闸只能查询'
        if ($KubectlArgs[1] -ceq 'configmap') { return [pscustomobject]@{ ExitCode = 0; Stdout = $miniConfigMap; Stderr = '' } }
        if ($KubectlArgs[1] -ceq 'secret') { return [pscustomobject]@{ ExitCode = 0; Stdout = ''; Stderr = '' } }
        throw "假 kubectl:没有规则命中这次调用:kubectl $($KubectlArgs -join ' ')"
    }
    $zones = @([pscustomobject]@{ Name = 'z1' })

    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'login 第三方登录现状预检失败\(1 个 zone\),未做任何写操作' -Because '漏传两个小游戏的参数要拒绝'
    Assert-Match -Text $errorText -Pattern ('线上已开 AuthProviders\.WeChatMiniGame[^\r\n]*要保持现状请照传 ' + [regex]::Escape($MiniKeepText) + ';确认要关闭或改动请加 -ConfirmLoginAuthProviderChange') -Because '报错给出小游戏的照传串与确认开关'

    $LoginWeChatMiniGameAppId = $MiniParams.WeChatMiniGameAppId; $LoginWeChatMiniGameEndpoint = $MiniParams.WeChatMiniGameEndpoint
    $LoginDouyinMiniGameAppId = $MiniParams.DouyinMiniGameAppId; $LoginDouyinMiniGameAllowOpenId = [switch]$true
    $kept = @(Assert-LoginAuthProviderClusterState -Zones $zones 3>&1)
    Assert-Equal -Expected 0 -Actual @($kept | Where-Object { $_ -is [System.Management.Automation.WarningRecord] }).Count -Because '照传小游戏参数即放行、不告警'

    $LoginDouyinMiniGameAllowOpenId = [switch]$false
    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'AuthProviders\.DouyinMiniGame\.RequireUnionId:线上 RequireUnionId: false' -Because '漏传 -LoginDouyinMiniGameAllowOpenId 同样拒绝(开关的值确实传进了判据)'
    Assert-NotMatch -Text $errorText -Pattern 'AuthProviders\.WeChatMiniGame' -Because '微信小游戏照传了,不报'
}

Test-Case 'k8s_deploy 防误关闸(Apple / Google):线上开着这两段、本次漏传就拒绝并给出照传串;照传放行;换了 ClientId 同样拒绝(四个参数都传进了判据)' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-ctx'
    $KubeConfig = 'C:/contract/kubeconfig'
    $LoginWeChatAppId = ''; $LoginWeChatEndpoint = ''; $LoginWeChatAllowOpenId = [switch]$false
    $LoginQQAppId = ''; $LoginQQEndpoint = ''; $LoginQQAllowOpenId = [switch]$false
    $LoginWeChatMiniGameAppId = ''; $LoginWeChatMiniGameEndpoint = ''; $LoginWeChatMiniGameAllowOpenId = [switch]$false
    $LoginDouyinMiniGameAppId = ''; $LoginDouyinMiniGameEndpoint = ''; $LoginDouyinMiniGameAllowOpenId = [switch]$false
    $LoginAppleClientId = ''; $LoginAppleEndpoint = ''; $LoginGoogleClientId = ''; $LoginGoogleEndpoint = ''
    $ConfirmLoginAuthProviderChange = [switch]$false
    $idTokenConfigMap = New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith -AuthYaml (New-AuthYaml -P $IdTokenParams))
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        Assert-Equal -Expected 'get' -Actual $KubectlArgs[0] -Because '防误关闸只能查询'
        if ($KubectlArgs[1] -ceq 'configmap') { return [pscustomobject]@{ ExitCode = 0; Stdout = $idTokenConfigMap; Stderr = '' } }
        if ($KubectlArgs[1] -ceq 'secret') { return [pscustomobject]@{ ExitCode = 0; Stdout = ''; Stderr = '' } }
        throw "假 kubectl:没有规则命中这次调用:kubectl $($KubectlArgs -join ' ')"
    }
    $zones = @([pscustomobject]@{ Name = 'z1' })

    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'login 第三方登录现状预检失败\(1 个 zone\),未做任何写操作' -Because '漏传 Apple / Google 的参数要拒绝'
    Assert-Match -Text $errorText -Pattern ('线上已开 AuthProviders\.Apple[^\r\n]*要保持现状请照传 ' + [regex]::Escape($IdTokenKeepText) + ';确认要关闭或改动请加 -ConfirmLoginAuthProviderChange') -Because '报错给出 Apple / Google 的照传串与确认开关'

    $LoginAppleClientId = $IdTokenParams.AppleClientId; $LoginAppleEndpoint = $IdTokenParams.AppleEndpoint
    $LoginGoogleClientId = $IdTokenParams.GoogleClientId
    $kept = @(Assert-LoginAuthProviderClusterState -Zones $zones 3>&1)
    Assert-Equal -Expected 0 -Actual @($kept | Where-Object { $_ -is [System.Management.Automation.WarningRecord] }).Count -Because '照传 Apple / Google 参数即放行、不告警'

    $LoginGoogleClientId = '2-xyz.apps.googleusercontent.com'
    $errorText = ''
    try { Assert-LoginAuthProviderClusterState -Zones $zones } catch { $errorText = $_.Exception.Message }
    Assert-Match -Text $errorText -Pattern 'AuthProviders\.Google\.ClientId 与线上不同' -Because '换了 -LoginGoogleClientId 同样拒绝(参数确实传进了判据)'
    Assert-NotMatch -Text $errorText -Pattern 'AuthProviders\.Apple' -Because 'Apple 照传了,不报'
}

Test-Case 'k8s_deploy 接入位置:防误关闸全文件只调一次,在 Assert-ClientEntryDeployPreflight 的 if ($DryRun) 的 else 分支、if ($deploysLogin) 之内,排在 Assert-ClientEntryClusterState 之后' {
    $guardCalls = @($DeployAst.FindAll({ param($node) $node -is [System.Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginAuthProviderClusterState' }, $true))
    Assert-Equal -Expected 1 -Actual $guardCalls.Count -Because '只有一个调用点'
    $call = $guardCalls[0]
    $ifs = [System.Collections.Generic.List[object]]::new()
    $function = $null
    for ($node = $call.Parent; $null -ne $node; $node = $node.Parent) {
        if ($node -is [System.Management.Automation.Language.IfStatementAst]) { $ifs.Add($node) }
        if ($node -is [System.Management.Automation.Language.FunctionDefinitionAst]) { $function = $node; break }
    }
    Assert-Equal -Expected 'Assert-ClientEntryDeployPreflight' -Actual $function.Name -Because '与集群现状预检同处(写路径入口、密钥解析与任何写操作之前)'
    Assert-True -Condition ($ifs.Count -ge 2) -Because '外层 if ($DryRun),内层 if ($deploysLogin)'
    Assert-Equal -Expected '$deploysLogin' -Actual $ifs[0].Clauses[0].Item1.Extent.Text -Because 'infra-up、-SkipGoSvc、k8s_image.ps1 路径都不碰 login,不能被这道闸拦住'
    $dryRunIf = $ifs[1]
    Assert-Equal -Expected '$DryRun' -Actual $dryRunIf.Clauses[0].Item1.Extent.Text -Because 'DryRun 不读集群'
    Assert-True -Condition ($null -ne $dryRunIf.ElseClause -and $call.Extent.StartOffset -gt $dryRunIf.ElseClause.Extent.StartOffset -and $call.Extent.EndOffset -le $dryRunIf.ElseClause.Extent.EndOffset) -Because '只在非 DryRun 分支里调用'
    Assert-Match -Text $dryRunIf.Clauses[0].Item2.Extent.Text -Pattern 'Skipping login third-party auth' -Because 'DryRun 要写明跳过了这道现状预检'
    $clusterStateCall = $dryRunIf.ElseClause.Find({ param($node) $node -is [System.Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-ClientEntryClusterState' }, $true)
    Assert-True -Condition ($null -ne $clusterStateCall -and $clusterStateCall.Extent.StartOffset -lt $call.Extent.StartOffset) -Because '与 gate / battle / Ingress 的现状预检同一分支,排在其后'
}

Test-Case '库契约 LoginConfigMapName / LoginConfigMapKey / LoginOAuthSecretName 与 k8s_deploy.ps1 逐字相等;参数注释不再写「包装入口不透传」' {
    $contract = Get-ClientEntryContract
    $deployText = Get-Content -LiteralPath $DeployScriptPath -Raw -Encoding utf8
    Assert-Match -Text $deployText -Pattern ('(?m)^\s*login\s+= @\{ ConfigMap = "' + [regex]::Escape($contract.LoginConfigMapName) + '";[^\r\n]*ConfigFile = "' + [regex]::Escape($contract.LoginConfigMapKey) + '"') -Because '防误关读的 ConfigMap 名与数据键必须是部署脚本生成的那一份'
    Assert-Match -Text $deployText -Pattern ('(?m)^\$script:LoginOAuthSecretName = "' + [regex]::Escape($contract.LoginOAuthSecretName) + '"\s*$') -Because '防误关探测的 Secret 必须是部署脚本建 / 删的那一个'
    Assert-NotMatch -Text $deployText -Pattern '目前不透传这六个参数' -Because '包装入口已照传,参数注释要同步'
}

# ─────────────────────────────────────────────────────────────────
# 6. dev_tools.ps1 Invoke-K8sDeploy:非空 / 被指定才透传
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
    以给定的脚本级参数值调一次 dev_tools.ps1 的 Invoke-K8sDeploy -K8sCommand zone-up,返回假 k8s_deploy.ps1 打出的实收值。
    假下游是简单脚本(无 CmdletBinding / [Parameter]):包装函数 splat 过来的其余具名参数落进 $args,不会报找不到参数;
    只把七个参数的实收值打出来,字符串没收到是 <unset>,switch 记「是否收到」。其余脚本级参数未定义即 $null。
#>
function Invoke-DevToolsK8sDeployWith {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-auth-passthrough-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        $fakeBody = @(
            ('param($LoginWeChatAppId = "<unset>", $LoginWeChatEndpoint = "<unset>", [switch]$LoginWeChatAllowOpenId, $LoginQQAppId = "<unset>", $LoginQQEndpoint = "<unset>", [switch]$LoginQQAllowOpenId, [switch]$ConfirmLoginAuthProviderChange, ' +
                '$LoginWeChatMiniGameAppId = "<unset>", $LoginWeChatMiniGameEndpoint = "<unset>", [switch]$LoginWeChatMiniGameAllowOpenId, $LoginDouyinMiniGameAppId = "<unset>", $LoginDouyinMiniGameEndpoint = "<unset>", [switch]$LoginDouyinMiniGameAllowOpenId, ' +
                '$LoginAppleClientId = "<unset>", $LoginAppleEndpoint = "<unset>", $LoginGoogleClientId = "<unset>", $LoginGoogleEndpoint = "<unset>")'),
            '"LoginWeChatAppId=$LoginWeChatAppId"',
            '"LoginWeChatEndpoint=$LoginWeChatEndpoint"',
            '"LoginWeChatAllowOpenId=$($PSBoundParameters.ContainsKey(''LoginWeChatAllowOpenId''))"',
            '"LoginQQAppId=$LoginQQAppId"',
            '"LoginQQEndpoint=$LoginQQEndpoint"',
            '"LoginQQAllowOpenId=$($PSBoundParameters.ContainsKey(''LoginQQAllowOpenId''))"',
            '"ConfirmLoginAuthProviderChange=$($PSBoundParameters.ContainsKey(''ConfirmLoginAuthProviderChange''))"',
            '"LoginWeChatMiniGameAppId=$LoginWeChatMiniGameAppId"',
            '"LoginWeChatMiniGameEndpoint=$LoginWeChatMiniGameEndpoint"',
            '"LoginWeChatMiniGameAllowOpenId=$($PSBoundParameters.ContainsKey(''LoginWeChatMiniGameAllowOpenId''))"',
            '"LoginDouyinMiniGameAppId=$LoginDouyinMiniGameAppId"',
            '"LoginDouyinMiniGameEndpoint=$LoginDouyinMiniGameEndpoint"',
            '"LoginDouyinMiniGameAllowOpenId=$($PSBoundParameters.ContainsKey(''LoginDouyinMiniGameAllowOpenId''))"',
            '"LoginAppleClientId=$LoginAppleClientId"',
            '"LoginAppleEndpoint=$LoginAppleEndpoint"',
            '"LoginGoogleClientId=$LoginGoogleClientId"',
            '"LoginGoogleEndpoint=$LoginGoogleEndpoint"'
        )
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_deploy.ps1') -Encoding utf8 -Value $fakeBody
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        $definition = $null
        $devToolsAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'), [ref]$null, [ref]$null)
        $definition = $devToolsAst.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-K8sDeploy' }, $true)
        if ($null -eq $definition) { throw 'dev_tools.ps1 里找不到函数 Invoke-K8sDeploy(契约测试的前提被改掉了)' }
        . ([scriptblock]::Create($definition.Extent.Text))
        return (Invoke-K8sDeploy -K8sCommand 'zone-up' | Out-String)
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'dev_tools Invoke-K8sDeploy:七个第三方登录参数非空 / 被指定才透传给 k8s_deploy.ps1,留空 / 空白不透传' {
    $unset = Invoke-DevToolsK8sDeployWith -Variables @{}
    foreach ($expected in @('LoginWeChatAppId=<unset>', 'LoginWeChatEndpoint=<unset>', 'LoginWeChatAllowOpenId=False', 'LoginQQAppId=<unset>', 'LoginQQEndpoint=<unset>', 'LoginQQAllowOpenId=False', 'ConfirmLoginAuthProviderChange=False')) {
        Assert-Match -Text $unset -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "没给就不透传($expected):默认值只在 k8s_deploy.ps1 一处"
    }
    $blank = Invoke-DevToolsK8sDeployWith -Variables @{ LoginWeChatAppId = '   '; LoginQQEndpoint = '' }
    Assert-Match -Text $blank -Pattern '(?m)^LoginWeChatAppId=<unset>\r?$' -Because '空白字符串不透传(透传了会被下游当成非法 AppId)'
    Assert-Match -Text $blank -Pattern '(?m)^LoginQQEndpoint=<unset>\r?$' -Because '空串不透传'

    $all = Invoke-DevToolsK8sDeployWith -Variables @{
        LoginWeChatAppId = 'wx0123456789abcdef'; LoginWeChatEndpoint = 'https://wechat-mock.contract.test:18090'; LoginWeChatAllowOpenId = $true
        LoginQQAppId = '100000001'; LoginQQEndpoint = 'https://qq-mock.contract.test'; LoginQQAllowOpenId = $true; ConfirmLoginAuthProviderChange = $true
    }
    foreach ($expected in @('LoginWeChatAppId=wx0123456789abcdef', 'LoginWeChatEndpoint=https://wechat-mock.contract.test:18090', 'LoginWeChatAllowOpenId=True',
            'LoginQQAppId=100000001', 'LoginQQEndpoint=https://qq-mock.contract.test', 'LoginQQAllowOpenId=True', 'ConfirmLoginAuthProviderChange=True')) {
        Assert-Match -Text $all -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "给了就原样透传($expected),漏一个重部署就关掉或改掉线上第三方登录"
    }
}

Test-Case 'dev_tools Invoke-K8sDeploy:小游戏的六个参数非空 / 被指定才透传给 k8s_deploy.ps1,留空 / 空白不透传' {
    $unset = Invoke-DevToolsK8sDeployWith -Variables @{}
    foreach ($expected in @('LoginWeChatMiniGameAppId=<unset>', 'LoginWeChatMiniGameEndpoint=<unset>', 'LoginWeChatMiniGameAllowOpenId=False',
            'LoginDouyinMiniGameAppId=<unset>', 'LoginDouyinMiniGameEndpoint=<unset>', 'LoginDouyinMiniGameAllowOpenId=False')) {
        Assert-Match -Text $unset -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "没给就不透传($expected):默认值只在 k8s_deploy.ps1 一处"
    }
    $blank = Invoke-DevToolsK8sDeployWith -Variables @{ LoginWeChatMiniGameAppId = '   '; LoginDouyinMiniGameEndpoint = '' }
    Assert-Match -Text $blank -Pattern '(?m)^LoginWeChatMiniGameAppId=<unset>\r?$' -Because '空白字符串不透传(透传了会被下游当成非法 AppId)'
    Assert-Match -Text $blank -Pattern '(?m)^LoginDouyinMiniGameEndpoint=<unset>\r?$' -Because '空串不透传'

    $all = Invoke-DevToolsK8sDeployWith -Variables @{
        LoginWeChatMiniGameAppId = 'wxfedcba9876543210'; LoginWeChatMiniGameEndpoint = 'https://wx-mini-mock.contract.test'; LoginWeChatMiniGameAllowOpenId = $true
        LoginDouyinMiniGameAppId = 'tt0123456789abcdef'; LoginDouyinMiniGameEndpoint = 'https://dy-mini-mock.contract.test'; LoginDouyinMiniGameAllowOpenId = $true
    }
    foreach ($expected in @('LoginWeChatMiniGameAppId=wxfedcba9876543210', 'LoginWeChatMiniGameEndpoint=https://wx-mini-mock.contract.test', 'LoginWeChatMiniGameAllowOpenId=True',
            'LoginDouyinMiniGameAppId=tt0123456789abcdef', 'LoginDouyinMiniGameEndpoint=https://dy-mini-mock.contract.test', 'LoginDouyinMiniGameAllowOpenId=True')) {
        Assert-Match -Text $all -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "给了就原样透传($expected),漏一个重部署就关掉或改掉线上的小游戏登录"
    }
    # 小游戏参数不影响微信 App / QQ 那七个的透传(各管各的)。
    Assert-Match -Text $all -Pattern '(?m)^LoginWeChatAppId=<unset>\r?$' -Because '只给小游戏参数时,微信 App 的参数照旧不透传'
}

Test-Case 'dev_tools Invoke-K8sDeploy:Apple / Google 的四个参数非空才透传给 k8s_deploy.ps1,留空 / 空白不透传' {
    $unset = Invoke-DevToolsK8sDeployWith -Variables @{}
    foreach ($expected in @('LoginAppleClientId=<unset>', 'LoginAppleEndpoint=<unset>', 'LoginGoogleClientId=<unset>', 'LoginGoogleEndpoint=<unset>')) {
        Assert-Match -Text $unset -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "没给就不透传($expected):默认值只在 k8s_deploy.ps1 一处"
    }
    $blank = Invoke-DevToolsK8sDeployWith -Variables @{ LoginAppleClientId = '   '; LoginGoogleEndpoint = '' }
    Assert-Match -Text $blank -Pattern '(?m)^LoginAppleClientId=<unset>\r?$' -Because '空白字符串不透传(透传了会被下游当成非法 ClientId)'
    Assert-Match -Text $blank -Pattern '(?m)^LoginGoogleEndpoint=<unset>\r?$' -Because '空串不透传'

    $all = Invoke-DevToolsK8sDeployWith -Variables @{
        LoginAppleClientId = 'com.example.mmorpg'; LoginAppleEndpoint = 'https://apple-mock.contract.test'
        LoginGoogleClientId = '1-abc.apps.googleusercontent.com'; LoginGoogleEndpoint = 'https://google-mock.contract.test'
    }
    foreach ($expected in @('LoginAppleClientId=com.example.mmorpg', 'LoginAppleEndpoint=https://apple-mock.contract.test',
            'LoginGoogleClientId=1-abc.apps.googleusercontent.com', 'LoginGoogleEndpoint=https://google-mock.contract.test')) {
        Assert-Match -Text $all -Pattern ('(?m)^' + [regex]::Escape($expected) + '\r?$') -Because "给了就原样透传($expected),漏一个重部署就关掉或改掉线上的 Apple / Google 登录"
    }
    Assert-Match -Text $all -Pattern '(?m)^LoginWeChatMiniGameAppId=<unset>\r?$' -Because '只给 Apple / Google 参数时,小游戏的参数照旧不透传'
}

# ─────────────────────────────────────────────────────────────────
# 7. dev_tools.ps1 Invoke-K8sImage:k8s_image.ps1 不部署 login,显式给了就拒绝
# ─────────────────────────────────────────────────────────────────

# dev_tools.ps1 Invoke-K8sImage:按 AST 取出函数,$ScriptDir 指到假 k8s_image.ps1 所在的临时目录(照 k8s_deploy_contract.tests.ps1)。
function Get-DevToolsFunctionText {
    param([Parameter(Mandatory = $true)][string]$FunctionName)
    $path = Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "dev_tools.ps1 解析失败: $($parseErrors | Out-String)" }
    $definition = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $FunctionName }, $true)
    if ($null -eq $definition) { throw "dev_tools.ps1 里找不到函数 $FunctionName(契约测试的前提被改掉了)" }
    return $definition.Extent.Text
}

<#
.SYNOPSIS
    以给定的脚本级参数值调一次 dev_tools.ps1 的 Invoke-K8sImage -ImageCommand release-zone,返回 @{ Error; DownstreamCalled; DownstreamArgs }。
    被测函数按动态作用域读 dev_tools.ps1 的脚本级参数:这里在本函数作用域里设置 -Variables 给的那几个,其余未设即 $null(开关视为未指定)。
    下游是假 k8s_image.ps1,只把收到的参数($args 以空格拼接)写进标记文件:即使拒绝失效,也不会真的构建、推送或部署。
    假脚本不声明参数块,splat 进来的具名参数全部落进 $args,形如 "-Name:" 后跟值;没被调到时 DownstreamArgs 为 $null。
#>
function Invoke-DevToolsK8sImageWith {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-k8s-image-reject-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        $marker = Join-Path $fakeDir 'k8s_image.called'
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_image.ps1') -Encoding utf8 -Value ('Set-Content -LiteralPath ''{0}'' -Value ($args -join '' ''); exit 0' -f $marker.Replace("'", "''"))
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        . ([scriptblock]::Create((Get-DevToolsFunctionText -FunctionName 'Invoke-K8sImage')))
        $errorText = $null
        try { Invoke-K8sImage -ImageCommand 'release-zone' *>&1 | Out-Null } catch { $errorText = $_.Exception.Message }
        $called = Test-Path -LiteralPath $marker
        $downstreamArgs = if ($called) { Get-Content -LiteralPath $marker -Raw } else { $null }
        return [pscustomobject]@{ Error = $errorText; DownstreamCalled = $called; DownstreamArgs = $downstreamArgs }
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'dev_tools Invoke-K8sImage:-LoginDevPasswordAuth 在调 k8s_image.ps1 之前拒绝;-RequireClientEndpoint 原样透传给 k8s_image.ps1' {
    # 对照组:什么都不给时必须真的调到假下游,否则"没调到"的断言在这套替身下会空转通过。
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'RequireClientEndpoint' -Because '留空不透传,由 k8s_deploy.ps1 的默认值接管(默认值只在一处)'

    # -LoginDevPasswordAuth 只落在 login 上,k8s_image.ps1 不部署 login、也不声明它:显式给了必须拒绝而不是静默吞掉。
    $reject = Invoke-DevToolsK8sImageWith -Variables @{ LoginDevPasswordAuth = $true }
    Assert-True -Condition ($null -ne $reject.Error) -Because '-LoginDevPasswordAuth 经 k8s-release-* 不生效,显式给了必须拒绝'
    Assert-Match -Text $reject.Error -Pattern 'k8s-image-\* / k8s-release-\* 不支持 [^:]*-LoginDevPasswordAuth' -Because '错误文本必须点名 -LoginDevPasswordAuth'
    Assert-True -Condition (-not $reject.DownstreamCalled) -Because '-LoginDevPasswordAuth:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)'

    # -RequireClientEndpoint 以前在这里被拒;现在 k8s_image.ps1 声明并透传它(只参与 k8s_deploy.ps1 的组合预检,
    # 不改写 login / scene_manager 的 ConfigMap,给了由 k8s_image.ps1 告警说明)。
    $forward = Invoke-DevToolsK8sImageWith -Variables @{ RequireClientEndpoint = 'true' }
    Assert-True -Condition ($null -eq $forward.Error -and $forward.DownstreamCalled) -Because "-RequireClientEndpoint 不再拒绝,必须调到 k8s_image.ps1。错误: $($forward.Error)"
    Assert-Match -Text $forward.DownstreamArgs -Pattern '(^|\s)-RequireClientEndpoint:?\s*true(\s|$)' -Because 'dev_tools.ps1 必须把 -RequireClientEndpoint 原样透传给 k8s_image.ps1,不能静默吞掉'
}

Test-Case 'dev_tools Invoke-K8sImage:第三方登录 -Login* 与 -ConfirmLoginAuthProviderChange 在调 k8s_image.ps1 之前逐个拒绝(k8s_image.ps1 不部署 login)' {
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'LoginWeChat|LoginQQ|ConfirmLogin' -Because '没给就不出现在下游参数里'
    $cases = @(
        @{ LoginWeChatAppId = 'wx0123456789abcdef' }, @{ LoginWeChatEndpoint = 'https://wechat-mock.contract.test' }, @{ LoginWeChatAllowOpenId = $true },
        @{ LoginQQAppId = '100000001' }, @{ LoginQQEndpoint = 'https://qq-mock.contract.test' }, @{ LoginQQAllowOpenId = $true },
        @{ ConfirmLoginAuthProviderChange = $true }
    )
    foreach ($variables in $cases) {
        $name = @($variables.Keys)[0]
        $reject = Invoke-DevToolsK8sImageWith -Variables $variables
        Assert-True -Condition ($null -ne $reject.Error) -Because "-$name 经 k8s-release-* 不生效(k8s_image.ps1 不部署 login),显式给了必须拒绝而不是静默吞掉"
        Assert-Match -Text $reject.Error -Pattern "k8s-image-\* / k8s-release-\* 不支持 [^:]*-$name(,|:)" -Because "错误文本必须点名 -$name"
        Assert-True -Condition (-not $reject.DownstreamCalled) -Because "-${name}:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)"
    }
}

Test-Case 'dev_tools Invoke-K8sImage:小游戏的六个参数在调 k8s_image.ps1 之前逐个拒绝(k8s_image.ps1 不部署 login)' {
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'MiniGame' -Because '没给就不出现在下游参数里'
    $cases = @(
        @{ LoginWeChatMiniGameAppId = 'wxfedcba9876543210' }, @{ LoginWeChatMiniGameEndpoint = 'https://wx-mini-mock.contract.test' }, @{ LoginWeChatMiniGameAllowOpenId = $true },
        @{ LoginDouyinMiniGameAppId = 'tt0123456789abcdef' }, @{ LoginDouyinMiniGameEndpoint = 'https://dy-mini-mock.contract.test' }, @{ LoginDouyinMiniGameAllowOpenId = $true }
    )
    foreach ($variables in $cases) {
        $name = @($variables.Keys)[0]
        $reject = Invoke-DevToolsK8sImageWith -Variables $variables
        Assert-True -Condition ($null -ne $reject.Error) -Because "-$name 经 k8s-release-* 不生效(k8s_image.ps1 不部署 login),显式给了必须拒绝而不是静默吞掉"
        Assert-Match -Text $reject.Error -Pattern "k8s-image-\* / k8s-release-\* 不支持 [^:]*-$name(,|:)" -Because "错误文本必须点名 -$name"
        Assert-True -Condition (-not $reject.DownstreamCalled) -Because "-${name}:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)"
    }
}

Test-Case 'dev_tools Invoke-K8sImage:Apple / Google 的四个参数在调 k8s_image.ps1 之前逐个拒绝(k8s_image.ps1 不部署 login)' {
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1。错误: $($control.Error)"
    Assert-NotMatch -Text $control.DownstreamArgs -Pattern 'LoginApple|LoginGoogle' -Because '没给就不出现在下游参数里'
    $cases = @(
        @{ LoginAppleClientId = 'com.example.mmorpg' }, @{ LoginAppleEndpoint = 'https://apple-mock.contract.test' },
        @{ LoginGoogleClientId = '1-abc.apps.googleusercontent.com' }, @{ LoginGoogleEndpoint = 'https://google-mock.contract.test' }
    )
    foreach ($variables in $cases) {
        $name = @($variables.Keys)[0]
        $reject = Invoke-DevToolsK8sImageWith -Variables $variables
        Assert-True -Condition ($null -ne $reject.Error) -Because "-$name 经 k8s-release-* 不生效(k8s_image.ps1 不部署 login),显式给了必须拒绝而不是静默吞掉"
        Assert-Match -Text $reject.Error -Pattern "k8s-image-\* / k8s-release-\* 不支持 [^:]*-$name(,|:)" -Because "错误文本必须点名 -$name"
        Assert-True -Condition (-not $reject.DownstreamCalled) -Because "-${name}:拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)"
    }
}

exit (Complete-TestRun -SuiteName 'login third-party auth anti-disable guard')
