#requires -Version 7.0
<#
.SYNOPSIS
  生成官方通行证的初始化数据 Secret(passport-init-data)与设置 ConfigMap(passport-settings),写到标准输出,接 kubectl apply -f -。
.DESCRIPTION
  以仓库里的 deploy/passport/init_data.json(本机沙盒用的那份)为底,只按本环境改下面几项,别的安全相关配置原样不动
  (docs/design/official-passport-registration.md §3.10):
    - 游戏应用加安卓 App Links 回调 <PublicOrigin>/passport/app-callback(§3.8);
    - 游戏应用填《用户协议》《隐私政策》地址:注册页的协议勾选链接到它们(§3.9);
    - 给了 -WebhookUrl 时打开回调(§3.7):填地址与请求头密钥(环境变量 MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET,与网关是同一个值)。
  回调密钥只进 Secret,不进 ConfigMap,脚本也不打印它。Casdoor 只在全新初始化时读这份数据(initDataNewOnly):
  已经初始化过的库要在后台按同样的值改。
.EXAMPLE
  $env:MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET = '<至少 32 字节>'
  pwsh -File deploy/k8s/passport/render-init-data.ps1 -PublicOrigin https://passport.example.com `
    -TermsUrl https://www.example.com/terms -PrivacyUrl https://www.example.com/privacy `
    -WebhookUrl http://gateway.zone-1.svc.cluster.local:8081/internal/passport/events | kubectl apply -f -
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PublicOrigin,
    [Parameter(Mandatory = $true)][string]$TermsUrl,
    [Parameter(Mandatory = $true)][string]$PrivacyUrl,
    [string]$WebhookUrl = '',
    [string]$Namespace = 'passport',
    [string]$InitDataPath = (Join-Path $PSScriptRoot '../../passport/init_data.json')
)

$ErrorActionPreference = 'Stop'

function Assert-ReleaseUrl {
    param([string]$Name, [string]$Value, [switch]$Origin)
    $uri = $null
    if (-not [Uri]::TryCreate($Value, [UriKind]::Absolute, [ref]$uri) -or $uri.Scheme -ne 'https' -or $uri.IsLoopback -or $uri.UserInfo) {
        throw "-$Name must be an https URL that is not on this machine"
    }
    if ($Origin -and ($uri.AbsolutePath -ne '/' -or $uri.Query -or $uri.Fragment -or $Value.EndsWith('/'))) {
        throw "-$Name must be scheme://host[:port] without a path or trailing slash: it is the token issuer"
    }
}

Assert-ReleaseUrl -Name 'PublicOrigin' -Value $PublicOrigin -Origin
Assert-ReleaseUrl -Name 'TermsUrl' -Value $TermsUrl
Assert-ReleaseUrl -Name 'PrivacyUrl' -Value $PrivacyUrl

$data = Get-Content -LiteralPath $InitDataPath -Raw | ConvertFrom-Json -AsHashtable
$games = @($data.applications | Where-Object { $_.name -eq 'app-xuanming-game' })
if ($games.Count -ne 1) { throw "init data must contain exactly one application app-xuanming-game" }
$game = $games[0]
$appLink = "$PublicOrigin/passport/app-callback"
if (@($game.redirectUris) -notcontains $appLink) { $game.redirectUris = @($game.redirectUris) + $appLink }
$game.termsOfUse = $TermsUrl
$game.privacyPolicy = $PrivacyUrl

if ($WebhookUrl) {
    $hookUri = $null
    if (-not [Uri]::TryCreate($WebhookUrl, [UriKind]::Absolute, [ref]$hookUri) -or $hookUri.Scheme -notin @('http', 'https') -or
        $hookUri.AbsolutePath -ne '/internal/passport/events' -or $hookUri.Query -or $hookUri.UserInfo) {
        throw "-WebhookUrl must be http(s)://<gateway service>[:port]/internal/passport/events"
    }
    $secret = [Environment]::GetEnvironmentVariable('MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET')
    if ([string]::IsNullOrEmpty($secret) -or [Text.Encoding]::UTF8.GetByteCount($secret) -lt 32 -or $secret -cne $secret.Trim() -or $secret.Contains('${')) {
        throw "-WebhookUrl needs MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET: at least 32 bytes, no surrounding whitespace, the same value the gateway gets"
    }
    $hooks = @($data.webhooks | Where-Object { $_.name -eq 'webhook-xuanming-game-sessions' })
    if ($hooks.Count -ne 1) { throw "init data must contain exactly one webhook webhook-xuanming-game-sessions" }
    $hooks[0].url = $WebhookUrl
    $hooks[0].headers = @([ordered]@{ name = 'X-Passport-Webhook-Secret'; value = $secret })
    $hooks[0].isEnabled = $true
}

$json = $data | ConvertTo-Json -Depth 32
$indented = ($json -split "`r?`n" | ForEach-Object { "    $_" }) -join "`n"
@"
apiVersion: v1
kind: Secret
metadata:
  name: passport-init-data
  namespace: $Namespace
type: Opaque
stringData:
  init_data.json: |
$indented
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: passport-settings
  namespace: $Namespace
data:
  origin: "$PublicOrigin"
"@
