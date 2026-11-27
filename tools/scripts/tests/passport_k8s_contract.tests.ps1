#requires -Version 7
<#
.SYNOPSIS
  官方通行证部署清单与初始化数据的契约(docs/design/official-passport-registration.md §3.6~§3.10、§8)。
.DESCRIPTION
  钉住「写错了不报错、只会静默失效」的几样东西:
    - 公网入口只放行名单里的接口,自助删除 / 改资料、管理接口一律不放行;页面请求带静默登录参数的拒绝;集群内入口只有 login 要的三个;
    - 生产清单与本机 compose 用同一个按摘要固定的镜像;
    - 初始化数据的安全缺省(argon2id、人机验证 Always、撞库与短信限额、注册协议勾选、不自动登录、回调地址只有桌面五个端口与手机协议头、回调缺省关闭);
    - render-init-data.ps1 按本环境填协议地址与回调密钥,别的不动。
.EXAMPLE
  pwsh -NoProfile -File tools/scripts/tests/passport_k8s_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# nginx.conf 里某个 server 块的文本(按 listen 端口找),到下一个 server 块或 http 块结束为止。
function Get-ServerBlock {
    param([string]$Conf, [int]$Port)
    $m = [regex]::Match($Conf, "(?ms)^      server \{\n        listen $Port;\n(?<body>.*?)^      \}\n")
    if (-not $m.Success) { throw "no server block listening on $Port" }
    return $m.Groups['body'].Value
}

# 一个 server 块里精确匹配的 /api/ 路径(location = /api/...)。
function Get-ExactApiLocations {
    param([string]$Block)
    return @([regex]::Matches($Block, '(?m)^\s*location = (?<path>/api/\S+) \{') | ForEach-Object { $_.Groups['path'].Value } | Sort-Object)
}

$script:Entry = Read-RepoText -RelativePath 'deploy/k8s/passport/public-entry.yaml'
$script:NginxConf = [regex]::Match($script:Entry, '(?ms)^  nginx\.conf: \|\n(?<conf>.*?)^---').Groups['conf'].Value
$script:InitData = (Read-RepoText -RelativePath 'deploy/passport/init_data.json') | ConvertFrom-Json -AsHashtable

Write-Host '=== passport_k8s_contract ==='

Test-Case '公网入口:/api/ 只放行玩家要用的接口,其余 404;自助删除 / 改资料、管理接口与初始化管理员口令都不在名单里' {
    Assert-True -Condition ($script:NginxConf.Length -gt 0) -Because 'ConfigMap 里要有 nginx.conf'
    $public = Get-ServerBlock -Conf $script:NginxConf -Port 8080
    $allowed = @(
        '/api/get-account', '/api/get-app-login', '/api/get-application', '/api/get-captcha', '/api/get-captcha-status',
        '/api/get-email-and-phone', '/api/login', '/api/login/oauth/access_token', '/api/logout', '/api/send-verification-code',
        '/api/set-password', '/api/signup', '/api/verify-captcha', '/api/verify-code'
    ) | Sort-Object
    Assert-Equal -Expected ($allowed -join ',') -Actual ((Get-ExactApiLocations -Block $public) -join ',') -Because '名单钉死:加接口要同步本用例与设计稿 §8.4 第 1 条'
    foreach ($forbidden in @('/api/delete-user', '/api/update-user', '/api/add-user', '/api/get-user', '/api/init-admin-password',
                             '/api/delete-own-account', '/api/upload-users', '/api/oauth/register', '/api/metrics', '/api/webhook')) {
        Assert-NotMatch -Text $public -Pattern ('location (= )?' + [regex]::Escape($forbidden) + '\b') -Because "$forbidden 不得出现在公网入口"
    }
    Assert-Match -Text $public -Pattern '(?m)^\s*location /api/ \{ return 404; \}' -Because '名单以外的 /api/ 一律 404'
    foreach ($backend in @('/cas', '/scim', '/swagger', '/orgs/', '/.well-known/')) {
        Assert-Match -Text $public -Pattern ('(?m)^\s*location ' + [regex]::Escape($backend) + ' \{ return 404; \}') -Because "后端入口 $backend 不放行"
    }
    $pages = [regex]::Match($public, '(?ms)^\s*location / \{\n(?<body>.*?)^\s*\}\n').Groups['body'].Value
    Assert-Match -Text $pages -Pattern 'limit_except GET \{ deny all; \}' -Because '页面与静态资源只读'
    Assert-Match -Text $pages -Pattern 'if \(\$passport_silent_signin\) \{ return 403; \}' -Because '页面请求带静默登录参数的拒绝(§3.10 第 2 条)'
    foreach ($auth in @('/api/login', '/api/signup', '/api/send-verification-code', '/api/verify-code', '/api/set-password', '/api/login/oauth/access_token')) {
        Assert-Match -Text $public -Pattern ('(?m)^\s*location = ' + [regex]::Escape($auth) + ' \{[^\n]*limit_req zone=passport_auth') -Because "$auth 按每分钟限速"
    }
    Assert-Match -Text $script:NginxConf -Pattern 'limit_req_zone \$binary_remote_addr zone=passport_auth:10m rate=20r/m;' -Because '登录类接口的限速'
}

Test-Case '静默登录参数:带 prompt / silentSignin(含参数名百分号编码)的页面请求被拦,游戏客户端的授权地址照常放行' {
    $map = [regex]::Match($script:NginxConf, '(?ms)map \$args \$passport_silent_signin \{\n(?<body>.*?)\n\s*\}').Groups['body'].Value
    $rules = @([regex]::Matches($map, '(?m)^\s*"(?<flag>~\*?)(?<re>[^"]+)"\s+1;') | ForEach-Object {
        $options = if ($_.Groups['flag'].Value -eq '~*') { [Text.RegularExpressions.RegexOptions]::IgnoreCase } else { [Text.RegularExpressions.RegexOptions]::None }
        [regex]::new($_.Groups['re'].Value, $options)
    })
    Assert-Equal -Expected 2 -Actual $rules.Count -Because 'map 里两条拦截规则:参数名、参数名里的百分号'
    Assert-Match -Text $map -Pattern '(?m)^\s*default 0;' -Because '缺省放行'
    $blocked = { param([string]$query) foreach ($rule in $rules) { if ($rule.IsMatch($query)) { return $true } }; return $false }
    foreach ($query in @('client_id=a&prompt=none', 'prompt=none', 'PROMPT=none&client_id=a', 'client_id=a&silentSignin=1', 'silentsignin',
                         'client_id=a&%70rompt=none', '%73ilentSignin=1', 'x=1&prompt&y=2', 'client_id=a&prompt=login')) {
        Assert-True -Condition (& $blocked $query) -Because "要拦: $query"
    }
    $client = 'client_id=xuanming-game-client&response_type=code&redirect_uri=http%3A%2F%2F127.0.0.1%3A18765%2Fpassport%2Fcallback' +
        '&scope=openid&state=s%2Bt&code_challenge=abc-_x&code_challenge_method=S256'
    foreach ($query in @('', $client, 'promptx=1', 'xprompt=1&a=b', 'redirect_uri=xuanming%3A%2F%2Fpassport%2Fcallback')) {
        Assert-True -Condition (-not (& $blocked $query)) -Because "要放行: $query"
    }
}

Test-Case '集群内入口只有 login 要的三个:验签公钥、查用户、删用户' {
    $internal = Get-ServerBlock -Conf $script:NginxConf -Port 8081
    Assert-Equal -Expected '/api/delete-user,/api/get-user' -Actual ((Get-ExactApiLocations -Block $internal) -join ',') -Because '管理接口只开这两个'
    Assert-Match -Text $internal -Pattern '(?m)^\s*location = /\.well-known/jwks \{' -Because '验签公钥'
    Assert-Match -Text $internal -Pattern '(?m)^\s*location / \{ return 404; \}' -Because '其余 404'
    Assert-Match -Text $script:Entry -Pattern '(?ms)name: passport-internal\n.*?ingressClassName: REPLACE_WITH_INTERNAL_INGRESS_CLASS' -Because '集群内入口挂内部 Ingress'
}

Test-Case '生产清单与本机 compose 用同一个按摘要固定的镜像' {
    $compose = Read-RepoText -RelativePath 'deploy/passport/docker-compose.yml'
    $casdoor = Read-RepoText -RelativePath 'deploy/k8s/passport/casdoor.yaml'
    $cleanup = Read-RepoText -RelativePath 'deploy/k8s/passport/verification-record-cleanup.yaml'
    $casdoorImage = [regex]::Match($compose, 'casbin/casdoor:v[0-9.]+@sha256:[0-9a-f]{64}').Value
    $mysqlImage = [regex]::Match($compose, 'mysql:8\.0\.[0-9]+@sha256:[0-9a-f]{64}').Value
    Assert-True -Condition ($casdoorImage.Length -gt 0 -and $mysqlImage.Length -gt 0) -Because 'compose 的两个镜像都要按摘要固定'
    Assert-Match -Text $casdoor -Pattern ('(?m)^\s*image: ' + [regex]::Escape($casdoorImage) + '\s*$') -Because 'Deployment 用同一个 Casdoor 镜像'
    Assert-Match -Text $cleanup -Pattern ('(?m)^\s*image: ' + [regex]::Escape($mysqlImage) + '\s*$') -Because '清理任务用同一个 mysql 镜像'
    Assert-Match -Text $script:Entry -Pattern '(?m)^\s*image: nginx:[0-9.]+-alpine@sha256:[0-9a-f]{64}\s*$' -Because 'nginx 也按摘要固定'
}

Test-Case '初始化数据的安全缺省' {
    $org = $script:InitData.organizations[0]
    Assert-Equal -Expected 'argon2id' -Actual $org.passwordType -Because '口令哈希用 argon2id(名字写错会被当成不哈希)'
    Assert-Equal -Expected $false -Actual $org.enableSoftDeletion -Because '软删除关着,删用户才真删'
    Assert-Equal -Expected 180 -Actual $org.recordRetentionDays -Because '审计记录保留 180 天'
    Assert-True -Condition ($org.securityPolicy.stuffingFailureThreshold -gt 0 -and $org.securityPolicy.stuffingBlockMinutes -gt 0) -Because '撞库拦截开着'
    Assert-True -Condition ($org.securityPolicy.smsPerPhoneHourly -gt 0 -and $org.securityPolicy.smsPerPhoneDaily -gt 0) -Because '每个号码的短信条数有上限'
    $game = @($script:InitData.applications | Where-Object { $_.name -eq 'app-xuanming-game' })[0]
    $captcha = @($game.providers | Where-Object { $_.name -eq 'provider_captcha_default' })
    Assert-True -Condition ($captcha.Count -eq 1 -and $captcha[0].rule -eq 'Always') -Because '注册、发码、登录都先过人机验证'
    $agreement = @($game.signupItems | Where-Object { $_.name -eq 'Agreement' })
    Assert-True -Condition ($agreement.Count -eq 1 -and $agreement[0].required -and $agreement[0].visible) -Because '注册要勾选协议'
    $email = @($game.signupItems | Where-Object { $_.name -eq 'Email' })
    Assert-Equal -Expected 'Normal' -Actual $email[0].rule -Because '注册必须验证邮箱'
    foreach ($port in 18765..18769) {
        Assert-True -Condition (@($game.redirectUris) -contains "http://127.0.0.1:$port/passport/callback") -Because "桌面回调端口 $port 要登记"
    }
    Assert-Equal -Expected 'authorization_code' -Actual ((@($game.grantTypes)) -join ',') -Because '只开授权码'
    Assert-Equal -Expected $false -Actual $game.enableAutoSignin -Because '已有会话时也要玩家点一下才签发授权码(手机自定义协议头回调的前提,§3.8)'
    Assert-True -Condition (@($game.redirectUris) -contains 'xuanming://passport/callback') -Because '手机回调用自定义协议头'
    Assert-Equal -Expected 6 -Actual @($game.redirectUris).Count -Because '回调地址只有桌面五个端口与手机协议头'
    $hook = @($script:InitData.webhooks | Where-Object { $_.name -eq 'webhook-xuanming-game-sessions' })
    Assert-True -Condition ($hook.Count -eq 1 -and -not $hook[0].isEnabled -and $hook[0].organization -eq 'built-in' -and $hook[0].isUserExtended) -Because '回调缺省关着、挂在 built-in(才连得到集群内)、带扩展用户'
    Assert-Equal -Expected 'delete-user,set-password,update-user' -Actual ((@($hook[0].events) | Sort-Object) -join ',') -Because '回调的事件'
}

Test-Case 'render-init-data.ps1 只按本环境填协议地址与回调密钥,输出 Secret 与 ConfigMap' {
    $envName = 'MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET'
    $saved = [Environment]::GetEnvironmentVariable($envName)
    $secret = 'contract-passport-webhook-secret-0123456789'
    try {
        [Environment]::SetEnvironmentVariable($envName, $secret)
        $out = & (Join-Path $script:RepoRoot 'deploy/k8s/passport/render-init-data.ps1') -PublicOrigin 'https://passport.contract.test' `
            -TermsUrl 'https://www.contract.test/terms' -PrivacyUrl 'https://www.contract.test/privacy' `
            -WebhookUrl 'http://gateway.zone-1.svc.cluster.local:8081/internal/passport/events'
        $text = ($out -join "`n") -replace "`r`n", "`n"
        Assert-Match -Text $text -Pattern '(?m)^kind: Secret\nmetadata:\n  name: passport-init-data\n' -Because '初始化数据进 Secret'
        Assert-Match -Text $text -Pattern '(?ms)^kind: ConfigMap\nmetadata:\n  name: passport-settings\n.*?^  origin: "https://passport\.contract\.test"' -Because 'origin 进 ConfigMap'
        $jsonText = [regex]::Match($text, '(?ms)^  init_data\.json: \|\n(?<json>.*?)\n---').Groups['json'].Value
        $rendered = ($jsonText -split "`n" | ForEach-Object { $_ -replace '^    ', '' }) -join "`n" | ConvertFrom-Json -AsHashtable
        $game = @($rendered.applications | Where-Object { $_.name -eq 'app-xuanming-game' })[0]
        $baseGame = @($script:InitData.applications | Where-Object { $_.name -eq 'app-xuanming-game' })[0]
        Assert-Equal -Expected ((@($baseGame.redirectUris)) -join ',') -Actual ((@($game.redirectUris)) -join ',') -Because '回调地址原样不动'
        Assert-Equal -Expected 'https://www.contract.test/terms' -Actual $game.termsOfUse -Because '用户协议地址'
        Assert-Equal -Expected 'https://www.contract.test/privacy' -Actual $game.privacyPolicy -Because '隐私政策地址'
        $hook = @($rendered.webhooks)[0]
        Assert-True -Condition ($hook.isEnabled -and $hook.url -eq 'http://gateway.zone-1.svc.cluster.local:8081/internal/passport/events') -Because '回调打开并填地址'
        Assert-Equal -Expected $secret -Actual @($hook.headers)[0].value -Because '回调密钥来自环境变量'
        Assert-Equal -Expected 'argon2id' -Actual $rendered.organizations[0].passwordType -Because '别的配置原样不动'
        Assert-NotMatch -Text $text -Pattern 'kind: ConfigMap[\s\S]*contract-passport-webhook-secret' -Because '密钥不进 ConfigMap'

        foreach ($bad in @(
                @{ Why = 'origin 不是 https'; Args = @{ PublicOrigin = 'http://passport.contract.test' } },
                @{ Why = 'origin 带路径'; Args = @{ PublicOrigin = 'https://passport.contract.test/x' } },
                @{ Why = '协议地址是本机'; Args = @{ TermsUrl = 'https://127.0.0.1/terms' } })) {
            $params = @{ PublicOrigin = 'https://passport.contract.test'; TermsUrl = 'https://www.contract.test/terms'; PrivacyUrl = 'https://www.contract.test/privacy' }
            foreach ($k in $bad.Args.Keys) { $params[$k] = $bad.Args[$k] }
            $threw = $false
            try { & (Join-Path $script:RepoRoot 'deploy/k8s/passport/render-init-data.ps1') @params | Out-Null } catch { $threw = $true }
            Assert-True -Condition $threw -Because "$($bad.Why) 要拒绝"
        }
        [Environment]::SetEnvironmentVariable($envName, 'short')
        $threw = $false
        try {
            & (Join-Path $script:RepoRoot 'deploy/k8s/passport/render-init-data.ps1') -PublicOrigin 'https://passport.contract.test' `
                -TermsUrl 'https://www.contract.test/terms' -PrivacyUrl 'https://www.contract.test/privacy' `
                -WebhookUrl 'http://gateway.zone-1.svc.cluster.local:8081/internal/passport/events' | Out-Null
        } catch { $threw = $true }
        Assert-True -Condition $threw -Because '回调密钥短于 32 字节要拒绝'
    }
    finally {
        [Environment]::SetEnvironmentVariable($envName, $saved)
    }
}

exit (Complete-TestRun -SuiteName 'passport_k8s_contract')
