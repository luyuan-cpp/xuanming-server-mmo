#requires -Version 7
<#
.SYNOPSIS
    GM 后台 gm-admin(java/gm_admin)上 K8s 与各资源服务管理入口接线的部署侧契约测试
    (docs/design/admin-identity-and-dual-control.md §11.4;deploy/k8s/README.md「GM 后台 gm-admin」)。

.DESCRIPTION
    本文件钉住:
      1. 登记与清单:服务目录、镜像名、端口;Service 只在集群内(ClusterIP、只暴露前端口)、没有 Ingress;两个容器;
         运维预置 Secret gm-admin-runtime 的键;邮件 / 榜单 admin 密钥直接引用 domain-service-secrets;
      2. 消费者:管理入口的每把口令 / 密钥只给后台与资源服务本身(目录表与全部清单);准入名单只在后台调到的口上加 gm-admin;
      3. 纯函数:Resolve-AdminEntrySecrets(可空、形状、互不相同、只读要的)、Get-GmAdminOidcSettings(四项形状、角色编码、排序)、
         Assert-DualControlEmergencySwitchesOff(非 dev 拒绝应急开关);
      4. DryRun(dev infra-up,带全局 Go / Java 服务、后台与各入口密钥,开 gRPC 口准入):后台的配置模板、清单、密钥注入与先后、
         入站准入;trade 与 match 打开人工处置入口;整份输出不含任何一把的值;没注入时入口保持关闭;
      5. DryRun(dev zone-up,注入 data-service / login / gateway 的管理口令):三个服务各自只拿自己的那把;
      6. 负向:zone-up 带 -GmAdminOidc* 在任何写操作之前拒绝;四项不齐拒绝。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_gm_admin_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== k8s_deploy.ps1 GM admin deployment contract tests ==='

# 被抽出来的函数按动态作用域读这些变量(在 k8s_deploy.ps1 里它们是脚本级变量或参数)。
$RepoRoot = Get-RepoRoot
$Infra = 'gmadmin-infra'
$Zone = 'gmadmin-test'
$ZoneNamespace = "mmorpg-zone-$Zone"
$AuthorityHost = 'tidb.invalid:4000'

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath)) -replace "`r`n", "`n")
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors[0].Message)" }
$deployFunctions = @('Resolve-AdminEntrySecrets', 'Get-GmAdminOidcSettings', 'Assert-DualControlEmergencySwitchesOff', 'Get-ZoneScopedJavaSvcNames')
foreach ($node in $deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($deployFunctions -contains $node.Name) { Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock() }
}
foreach ($name in $deployFunctions) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 $name" }
}

# k8s_deploy.ps1 顶层某个赋值右边的字面量(服务目录、名单)。只求值右边:目录里的取值脚本块不会被调用。
function Get-DeployLiteral {
    param([Parameter(Mandatory = $true)][string]$Left)
    $found = @($deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -ceq $Left }, $false))
    if ($found.Count -ne 1) { throw "k8s_deploy.ps1 里 $Left 的赋值应当恰好 1 处,实际 $($found.Count) 处" }
    return (& ([scriptblock]::Create($found[0].Right.Extent.Text)))
}

$script:GmAdminRoleCodes = @(Get-DeployLiteral -Left '$script:GmAdminRoleCodes')
$GmAdminRuntimeSecretKeys = @(Get-DeployLiteral -Left '$script:GmAdminRuntimeSecretKeys')
$JavaSvcCatalogue = Get-DeployLiteral -Left '$JavaSvcCatalogue'
$GoSvcCatalogue = Get-DeployLiteral -Left '$GoSvcCatalogue'
$Catalog = Get-DeployLiteral -Left '$script:WorkloadSecretCatalog'
$AllowLists = Get-DeployLiteral -Left '$script:GrpcAllowLists'

# 管理入口的几把(目录名 → 变量名、键、消费者)。与目录表逐项一致由第一组用例核。
$EntrySpecs = [ordered]@{
    'gm-admin-oidc'             = @{ Env = 'MMORPG_GM_OIDC_CLIENT_SECRET'; Key = 'client-secret'; Consumers = @('gm-admin'); Value = 'contract-gm-oidc-client-secret-000000001' }
    'trade-manual-resolution'   = @{ Env = 'MMORPG_TRADE_MANUAL_RESOLUTION_SECRET'; Key = 'secret'; Consumers = @('trade', 'gm-admin'); Value = 'contract-trade-manual-resolution-0000000002' }
    'match-manual-resolution'   = @{ Env = 'MMORPG_MATCH_MANUAL_RESOLUTION_SECRET'; Key = 'secret'; Consumers = @('match', 'gm-admin'); Value = 'contract-match-manual-resolution-0000000003' }
    'data-service-admin-token'  = @{ Env = 'MMORPG_DATA_SERVICE_ADMIN_TOKEN'; Key = 'token'; Consumers = @('data-service', 'gm-admin'); Value = 'contract-data-service-admin-token-00000004' }
    'login-admin-token'         = @{ Env = 'MMORPG_LOGIN_ADMIN_TOKEN'; Key = 'token'; Consumers = @('login', 'gm-admin'); Value = 'contract-login-admin-token-000000000000005' }
    'gateway-admin-machine-key' = @{ Env = 'MMORPG_GATEWAY_ADMIN_MACHINE_KEY'; Key = 'key'; Consumers = @('gateway'); Value = 'contract-gateway-machine-key-0000000000006' }
}

# DryRun 把一个 manifest 文件整份打成一个块。按 --- 切成单份文档,再按 kind + 顶层 metadata.name 精确取(恰好 1 份才返回;-AllowMissing 时 0 份返回 $null)。
function Select-Document {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name,
        [switch]$AllowMissing
    )
    $kindPattern = '(?m)^kind:[ \t]*{0}[ \t]*$' -f [regex]::Escape($Kind)
    $namePattern = '(?m)^  name:[ \t]*{0}[ \t]*$' -f [regex]::Escape($Name)
    $docs = @(foreach ($block in (Get-ManifestBlocks -Output $Output)) {
            foreach ($doc in (($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')) {
                if ($doc -cmatch $kindPattern -and $doc -cmatch $namePattern) { $doc }
            }
        })
    if ($docs.Count -eq 0 -and $AllowMissing) { return $null }
    if ($docs.Count -ne 1) { throw "DryRun 输出里 $Kind $Name 应当恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

# 与 k8s_deploy.ps1 Get-InjectedSecretFingerprint 同一算法:SHA256("<用途串>`n<值>") 的前 12 位十六进制。
function Get-ExpectedHash {
    param([Parameter(Mandatory = $true)][string]$Purpose, [Parameter(Mandatory = $true)][string]$Value)
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try { $bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$Purpose`n$Value")) } finally { $sha256.Dispose() }
    return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12))
}

# 工作负载里这把密钥:恰好一条 env,secretKeyRef 到本 namespace 的 Secret 与键、不是 optional;Pod 模板带只哈希这把密钥的指纹。
function Assert-EnvFromSecret {
    param(
        [Parameter(Mandatory = $true)][string]$Doc, [Parameter(Mandatory = $true)][string]$EnvName,
        [Parameter(Mandatory = $true)][string]$SecretName, [Parameter(Mandatory = $true)][string]$SecretKey,
        [Parameter(Mandatory = $true)][string]$HashKey, [Parameter(Mandatory = $true)][string]$ExpectedHash, [Parameter(Mandatory = $true)][string]$Label
    )
    $text = $Doc -replace "`r`n", "`n"
    $envPattern = [regex]::Escape($EnvName)
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]*- name: ' + $envPattern + '[ ]*$').Count) -Because "${Label}:$EnvName 只能注入一份"
    $reference = '(?m)^[ ]*- name: ' + $envPattern + '[ ]*\n[ ]+valueFrom:[ ]*\n[ ]+secretKeyRef:[ ]*\n[ ]+name: ' + [regex]::Escape($SecretName) + '[ ]*\n[ ]+key: ' + [regex]::Escape($SecretKey) + '[ ]*\n(?![ ]+optional:)'
    Assert-True -Condition ($text -cmatch $reference) -Because "${Label}:$EnvName 必须 secretKeyRef 到 Secret $SecretName 的键 $SecretKey,且不是 optional"
    $found = [regex]::Matches($text, '(?m)^[ ]+' + [regex]::Escape($HashKey) + ': "(?<hash>[0-9a-f]{12})"[ ]*$')
    Assert-True -Condition ($found.Count -eq 1 -and $found[0].Groups['hash'].Value -ceq $ExpectedHash) -Because "${Label}:Pod 模板上 $HashKey 恰好 1 条,只由这把密钥决定(换密钥即滚动)"
}

function Assert-EntrySecretInjected {
    param([Parameter(Mandatory = $true)][string]$Doc, [Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Label)
    $spec = $EntrySpecs[$Name]
    Assert-EnvFromSecret -Doc $Doc -EnvName $spec.Env -SecretName $Name -SecretKey $spec.Key -HashKey "mmorpg.io/$Name-hash" `
        -ExpectedHash (Get-ExpectedHash -Purpose "$Name/workload" -Value $spec.Value) -Label $Label
}

function Assert-EntrySecretAbsent {
    param([Parameter(Mandatory = $true)][string]$Doc, [Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Label)
    $text = $Doc -replace "`r`n", "`n"
    Assert-True -Condition ($text -notmatch ('(?m)^[ ]*- name: ' + [regex]::Escape($EntrySpecs[$Name].Env) + '[ ]*$')) -Because "${Label}:不读 $Name,不得注入 $($EntrySpecs[$Name].Env)"
    Assert-True -Condition (-not $text.Contains("mmorpg.io/$Name-hash")) -Because "${Label}:不读 $Name,不带它的指纹"
}

# ──────────────────────────────────────────────
# 1. 登记与清单(只读仓库文件)
# ──────────────────────────────────────────────
Test-Case '登记:gm-admin 是全局 Java 服务,zone-up 不部署它;配置模板、清单、端口与镜像名各处一致' {
    $entry = $JavaSvcCatalogue['gm-admin']
    Assert-True -Condition ($null -ne $entry -and [bool]$entry.Global) -Because '$JavaSvcCatalogue 里的 gm-admin 是全局服务(infra namespace 一份)'
    Assert-Equal -Expected 'java-svc-gm-admin-config/gm-admin.yaml/3101/mmorpg-gm-admin' -Actual ('{0}/{1}/{2}/{3}' -f $entry.ConfigMap, $entry.Manifest, $entry.HttpPort, $entry.ImageName) -Because '配置模板、清单、前端口与镜像名'
    Assert-True -Condition (@(Get-ZoneScopedJavaSvcNames) -cnotcontains 'gm-admin') -Because 'zone-up 不部署后台'
    $javaImage = [regex]::Match((Read-RepoText 'tools/scripts/java_svc_image.ps1'), '(?m)^\s*[''"]?gm-admin[''"]?\s*=\s*@\{[^}]*ImageName\s*=\s*[''"](?<n>[^''"]+)[''"]')
    Assert-True -Condition $javaImage.Success -Because 'java_svc_image.ps1 的服务表里要有 gm-admin'
    Assert-Equal -Expected ([string]$entry.ImageName) -Actual $javaImage.Groups['n'].Value -Because '发布的镜像名与部署拉的镜像名必须一致(gm-admin)'
    $nginx = Read-RepoText 'deploy/k8s/gm-admin-nginx.conf'
    Assert-Match -Text $nginx -Pattern ('(?m)^\s*listen\s+(\S+:)?' + $entry.HttpPort + '\b') -Because '前端 nginx 听目录里的端口(Service 只暴露它)'
    Assert-Match -Text $nginx -Pattern 'proxy_pass\s+http://127\.0\.0\.1:18090/jeecg-boot/;' -Because '/jeecgboot/ 反代到同 Pod 的后端(与清单里后端的 18090 一致)'
}

$Manifest = Read-RepoText 'deploy/k8s/manifests/java-svc/gm-admin.yaml'
$ManifestDocs = @($Manifest -split '(?m)^---[ \t]*$')

Test-Case '清单:Service 只在集群内、只暴露前端口;Deployment 两个容器、只读根文件系统、不挂 ServiceAccount 令牌;没有手写注解与由脚本注入的变量' {
    $service = @($ManifestDocs | Where-Object { $_ -cmatch '(?m)^kind: Service\s*$' })
    Assert-Equal -Expected 1 -Actual $service.Count -Because '清单里恰好一个 Service'
    Assert-Match -Text $service[0] -Pattern '(?m)^  type: ClusterIP\s*$' -Because '只在集群内可达,不建 LoadBalancer / NodePort'
    Assert-Equal -Expected '3101' -Actual (@([regex]::Matches($service[0], '(?m)^\s+port: (\d+)\s*$') | ForEach-Object { $_.Groups[1].Value }) -join ',') -Because 'Service 只暴露前端 3101,后端 18090 只在 Pod 内'
    Assert-NotMatch -Text $Manifest -Pattern '(?m)^kind: Ingress' -Because '不建 Ingress'
    $deployment = @($ManifestDocs | Where-Object { $_ -cmatch '(?m)^kind: Deployment\s*$' })
    Assert-Equal -Expected 1 -Actual $deployment.Count -Because '后台不是游戏的 SQL 写者,工作负载是 Deployment'
    $text = $deployment[0]
    Assert-Equal -Expected 'gm-admin,gm-admin-web' -Actual (@([regex]::Matches($text, '(?m)^        - name: (gm-admin[a-z-]*)\s*$') | ForEach-Object { $_.Groups[1].Value }) -join ',') -Because '两个容器:后端与前端 nginx'
    Assert-Match -Text $text -Pattern '--spring\.profiles\.active=prod,gm,k8s' -Because 'profile k8s 盖在 jar 内 application-gm.yml 之上(配置模板的键名是 application-k8s.yaml)'
    Assert-Match -Text $text -Pattern '--spring\.config\.additional-location=file:/app/k8s-config/' -Because '配置模板从 /app/k8s-config 读'
    Assert-Match -Text $text -Pattern '(?s)- name: svc-config\s+mountPath: /app/k8s-config\s+readOnly: true' -Because '配置模板只读挂在 /app/k8s-config'
    Assert-Match -Text $text -Pattern '(?s)- name: app-config\s+mountPath: /app/config\b' -Because '工作目录下的 config 可写:上游启动时往那里拷代码生成模板'
    Assert-Match -Text $text -Pattern '(?s)- name: app-config\s+emptyDir:\s+sizeLimit: \d+Mi' -Because '可写卷有上限(超了驱逐 Pod,不让它写满节点盘)'
    Assert-Match -Text $text -Pattern 'command: \["nginx", "-e", "/dev/stderr", "-c", "/etc/nginx/gm-admin\.conf", "-g", "daemon off;"\]' -Because '前端容器跑镜像里的 nginx 配置,错误日志从启动起就到 stderr'
    Assert-Match -Text $text -Pattern 'nginx -e /dev/stderr -c /etc/nginx/gm-admin\.conf -s quit' -Because '前端容器停机前先让 nginx 处理完手上的请求(SIGTERM 是立即退出)'
    Assert-Match -Text (Read-RepoText 'deploy/k8s/Dockerfile.gm-admin') -Pattern '(?m)^ENTRYPOINT \["java", "-XX:MaxRAMPercentage=\d+", "-jar", "/app/gm-admin\.jar"\]\s*$' -Because '后端容器跑镜像入口,清单只给 Spring 参数'
    Assert-Match -Text $text -Pattern '(?m)^      automountServiceAccountToken: false\s*$' -Because '后台不调 K8s API'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($text, 'readOnlyRootFilesystem: true').Count) -Because '两个容器根文件系统都只读'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^\s+env:\s*$').Count) -Because '只有后端有 env 段(Add-GoSvcSecretEnv 要恰好一个)'
    Assert-NotMatch -Text $text -Pattern '(?m)^\s+annotations:' -Because 'Pod 模板的注解全由部署脚本写(配置与密钥指纹)'
    foreach ($name in @('MMORPG_GM_OIDC_CLIENT_SECRET', 'MMORPG_TRADE_MANUAL_RESOLUTION_SECRET', 'MMORPG_MATCH_MANUAL_RESOLUTION_SECRET', 'MMORPG_DATA_SERVICE_ADMIN_TOKEN',
            'MMORPG_LOGIN_ADMIN_TOKEN', 'MMORPG_COMMERCE_ADMIN_SECRET', 'MMORPG_GATEWAY_ADMIN_API_KEY', 'MMORPG_PLAYER_INBOX_ADMIN_TOKEN')) {
        Assert-NotMatch -Text $text -Pattern ('(?m)^\s+- name: ' + $name + '\s*$') -Because "清单不手写 ${name}:它由部署脚本经 Secret 注入"
    }
}

Test-Case '清单:管理库与 Redis 只来自运维预置的 gm-admin-runtime(键与部署脚本核对的一致);邮件 / 榜单 admin 密钥引用运维预置的 domain-service-secrets' {
    $keys = @([regex]::Matches($Manifest, 'secretKeyRef: \{name: gm-admin-runtime, key: ([a-z-]+), optional: false\}') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected ($GmAdminRuntimeSecretKeys -join ',') -Actual ($keys -join ',') -Because '清单引用的键与 Assert-GmAdminDeployPreflight 核对的键逐个一致'
    foreach ($pair in @(@('MAIL_PRODUCER_SECRET_ADMIN', 'mail-producer-admin'), @('RANK_PRODUCER_SECRET_ADMIN', 'rank-producer-admin'))) {
        Assert-Match -Text $Manifest -Pattern ('(?m)^\s+- name: ' + $pair[0] + '\s*\n\s+valueFrom:\s*\n\s+secretKeyRef: \{name: domain-service-secrets, key: ' + $pair[1] + ', optional: false\}') -Because "$($pair[0]) 与资源服务读同一份运维预置的键"
    }
    $deployScript = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    Assert-True -Condition ($deployScript.Contains('''    mail-admin-secret: "${MAIL_PRODUCER_SECRET_ADMIN}"''')) -Because '配置模板对邮件密钥的引用与清单里的变量名一致'
    Assert-True -Condition ($deployScript.Contains('''    rank-admin-secret: "${RANK_PRODUCER_SECRET_ADMIN}"''')) -Because '配置模板对榜单密钥的引用与清单里的变量名一致'
}

# ──────────────────────────────────────────────
# 2. 消费者:只给后台与资源服务本身
# ──────────────────────────────────────────────
Test-Case '目录表:管理入口每把的变量名、键与消费者;商城 admin 角色只给商城与后台' {
    foreach ($name in $EntrySpecs.Keys) {
        Assert-True -Condition ($Catalog.Contains($name)) -Because "工作负载密钥目录里要有 $name"
        Assert-Equal -Expected $EntrySpecs[$name].Env -Actual $Catalog[$name].Env -Because "$name 的变量名"
        Assert-Equal -Expected $EntrySpecs[$name].Key -Actual $Catalog[$name].SecretKey -Because "$name 的键"
        Assert-Equal -Expected ($EntrySpecs[$name].Consumers -join ',') -Actual (@($Catalog[$name].Consumers) -join ',') -Because "$name 只给后台与资源服务本身(设计稿 §6 第 3 条)"
    }
    Assert-Equal -Expected 'mall,gm-admin' -Actual (@($Catalog['commerce-admin'].Consumers) -join ',') -Because '商城 admin 角色只给商城与后台'
    foreach ($name in @($Catalog.Keys)) {
        if ($EntrySpecs.Contains($name) -or $name -ceq 'commerce-admin') { continue }
        Assert-True -Condition (@($Catalog[$name].Consumers) -cnotcontains 'gm-admin') -Because "后台不读与管理入口无关的 $name"
    }
}

Test-Case '清单:管理口令的运维预置键只被后台与资源服务本身引用;login 的口令不再读 domain-service-secrets' {
    $manifestRoot = Join-Path (Get-RepoRoot) 'deploy/k8s/manifests'
    $owners = @{ 'mail-producer-admin' = @('go-svc/mail.yaml', 'java-svc/gm-admin.yaml'); 'rank-producer-admin' = @('go-svc/rank.yaml', 'java-svc/gm-admin.yaml'); 'login-admin-token' = @(); 'gm-admin-runtime' = @('java-svc/gm-admin.yaml') }
    $files = @(Get-ChildItem -LiteralPath $manifestRoot -Recurse -File -Filter '*.yaml')
    foreach ($key in $owners.Keys) {
        $users = @($files | Where-Object { ([IO.File]::ReadAllText($_.FullName)) -cmatch ('(?m)^[^#\r\n]*\b' + [regex]::Escape($key) + '\b') } |
                ForEach-Object { [IO.Path]::GetRelativePath($manifestRoot, $_.FullName).Replace('\', '/') } | Sort-Object)
        Assert-Equal -Expected (@($owners[$key] | Sort-Object) -join ',') -Actual ($users -join ',') -Because "$key 只能出现在这些清单里"
    }
}

Test-Case '准入名单:只在后台调到的口上放行 gm-admin(login、trade、mail、rank、match、mall)' {
    $expected = @('login', 'trade', 'mail', 'rank', 'match', 'mall')
    foreach ($svc in @($AllowLists.Keys)) {
        $has = @($AllowLists[$svc].AllowedApps) -ccontains 'gm-admin'
        Assert-Equal -Expected ($expected -ccontains $svc) -Actual $has -Because "$svc 的准入名单$(if ($expected -ccontains $svc) { '要' } else { '不该' })放行 gm-admin"
    }
}

# ──────────────────────────────────────────────
# 3. 纯函数
# ──────────────────────────────────────────────
$EntryEnvNames = @($EntrySpecs.Keys | Where-Object { $_ -cne 'gm-admin-oidc' } | ForEach-Object { $EntrySpecs[$_].Env })
function Invoke-WithEntryEnv {
    param([Parameter(Mandatory = $true)][hashtable]$Values, [Parameter(Mandatory = $true)][scriptblock]$Action)
    $saved = @{}
    foreach ($name in $EntryEnvNames) { $saved[$name] = [Environment]::GetEnvironmentVariable($name); [Environment]::SetEnvironmentVariable($name, $null) }
    try {
        foreach ($name in $Values.Keys) { [Environment]::SetEnvironmentVariable($name, $Values[$name]) }
        return (& $Action)
    }
    finally {
        foreach ($name in $EntryEnvNames) { [Environment]::SetEnvironmentVariable($name, $saved[$name]) }
    }
}
$AllWanted = @('trade-manual-resolution', 'match-manual-resolution', 'data-service-admin-token', 'login-admin-token', 'gateway-admin-machine-key')

Test-Case 'Resolve-AdminEntrySecrets:没设的不返回;只读要的那几把;合格的原样返回' {
    $none = Invoke-WithEntryEnv -Values @{} -Action { Resolve-AdminEntrySecrets -ReleaseProfile 'prod' -Wanted $AllWanted -OtherSecrets @() }
    Assert-Equal -Expected 0 -Actual $none.Count -Because '全部没设:入口保持关闭,不阻断部署(各档位都不回落)'
    $some = Invoke-WithEntryEnv -Values @{ MMORPG_TRADE_MANUAL_RESOLUTION_SECRET = $EntrySpecs['trade-manual-resolution'].Value; MMORPG_LOGIN_ADMIN_TOKEN = 'x' } -Action {
        Resolve-AdminEntrySecrets -ReleaseProfile 'prod' -Wanted @('trade-manual-resolution') -OtherSecrets @()
    }
    Assert-Equal -Expected 'trade-manual-resolution' -Actual (@($some.Keys) -join ',') -Because '不在 -Wanted 里的不读:不部署 login 的 infra-up 不因 login 口令不合格被拦下'
    Assert-Equal -Expected $EntrySpecs['trade-manual-resolution'].Value -Actual $some['trade-manual-resolution'] -Because '值原样返回'
}

Test-Case 'Resolve-AdminEntrySecrets:短于 32 字节、含空白 / 引号 / 反斜杠、含 "${"、彼此相同或与其它密钥相同,一律在写之前拒绝,报错不回显值' {
    $cases = @(
        @{ Value = 'contract-short-secret-0123456789'.Substring(0, 31); Why = '短于 32 字节' }
        @{ Value = 'contract-admin-secret-with space-0000000'; Why = '含空白' }
        @{ Value = 'contract-admin-secret-with-"quote"-000000'; Why = '含双引号' }
        @{ Value = 'contract-admin-secret-with-${x}-00000000000'; Why = '含 ${' }
    )
    foreach ($case in $cases) {
        $message = Get-ThrownMessage { Invoke-WithEntryEnv -Values @{ MMORPG_MATCH_MANUAL_RESOLUTION_SECRET = $case.Value } -Action { Resolve-AdminEntrySecrets -ReleaseProfile 'dev' -Wanted $AllWanted -OtherSecrets @() } }
        Assert-Match -Text $message -Pattern 'MMORPG_MATCH_MANUAL_RESOLUTION_SECRET' -Because "$($case.Why):拒绝并点名变量(dev 档同样查)"
        Assert-True -Condition (-not $message.Contains($case.Value)) -Because "$($case.Why):报错不回显值"
    }
    $same = 'contract-admin-secret-shared-by-two-0000000'
    $message = Get-ThrownMessage { Invoke-WithEntryEnv -Values @{ MMORPG_DATA_SERVICE_ADMIN_TOKEN = $same; MMORPG_LOGIN_ADMIN_TOKEN = $same } -Action { Resolve-AdminEntrySecrets -ReleaseProfile 'dev' -Wanted $AllWanted -OtherSecrets @() } }
    Assert-Match -Text $message -Pattern 'MMORPG_DATA_SERVICE_ADMIN_TOKEN must differ from MMORPG_LOGIN_ADMIN_TOKEN' -Because '两个入口不共用一把口令'
    Assert-True -Condition (-not $message.Contains($same)) -Because '报错不回显值'
    $message = Get-ThrownMessage { Invoke-WithEntryEnv -Values @{ MMORPG_GATEWAY_ADMIN_MACHINE_KEY = $same } -Action { Resolve-AdminEntrySecrets -ReleaseProfile 'dev' -Wanted $AllWanted -OtherSecrets @(@{ Env = 'MMORPG_GATEWAY_ADMIN_API_KEY'; Value = $same }) } }
    Assert-Match -Text $message -Pattern 'MMORPG_GATEWAY_ADMIN_MACHINE_KEY must differ from MMORPG_GATEWAY_ADMIN_API_KEY' -Because '机器口令与人用的 admin.api-key 不同(gateway 也拒启)'
}

function Invoke-OidcSettings {
    param([string]$Issuer = 'https://idp.invalid/realms/gm', [string]$ClientId = 'gm-admin', [string]$RedirectUri = 'http://127.0.0.1:3101/login',
        [string]$RoleMapping = '{"gm-approvers":"gm_approver","gm-admins":"admin"}')
    $GmAdminOidcIssuer = $Issuer; $GmAdminOidcClientId = $ClientId; $GmAdminOidcRedirectUri = $RedirectUri; $GmAdminOidcRoleMapping = $RoleMapping
    return Get-GmAdminOidcSettings
}

Test-Case 'Get-GmAdminOidcSettings:合格的四项原样返回,角色映射按分组名排序压成一行;回调地址可以是 https 或回环 http' {
    $settings = Invoke-OidcSettings
    Assert-Equal -Expected 'https://idp.invalid/realms/gm|gm-admin|http://127.0.0.1:3101/login|2' -Actual ('{0}|{1}|{2}|{3}' -f $settings.Issuer, $settings.ClientId, $settings.RedirectUri, $settings.GroupCount) -Because '四项原样返回'
    Assert-Equal -Expected '{"gm-admins":"admin","gm-approvers":"gm_approver"}' -Actual $settings.RoleMapping -Because '按分组名排序:同一份输入每次渲染出同一份配置,配置指纹不白变'
    Assert-Equal -Expected 'https://gm.invalid/login' -Actual (Invoke-OidcSettings -RedirectUri 'https://gm.invalid/login').RedirectUri -Because 'https 的回调地址也行(运维另配内部入口时)'
}

Test-Case 'Get-GmAdminOidcSettings:发行方非 https、回调地址是非回环的 http、带查询串、客户端号含引号、映射为空或角色编码不认、含 "${",一律拒绝' {
    $cases = @(
        @{ Args = @{ Issuer = 'http://idp.invalid/realms/gm' }; Pattern = '-GmAdminOidcIssuer must be an https URL' }
        @{ Args = @{ Issuer = 'https://idp.invalid/realms/gm?x=1' }; Pattern = '-GmAdminOidcIssuer must be an https URL' }
        @{ Args = @{ RedirectUri = 'http://gm.invalid/login' }; Pattern = '-GmAdminOidcRedirectUri must be https, or http on a loopback address' }
        @{ Args = @{ RedirectUri = 'http://127.0.0.1:3101/login#x' }; Pattern = '-GmAdminOidcRedirectUri must be https' }
        @{ Args = @{ ClientId = 'gm"admin' }; Pattern = '-GmAdminOidcClientId must be 1 to 255 visible ASCII' }
        @{ Args = @{ RoleMapping = '{}' }; Pattern = '-GmAdminOidcRoleMapping must be a non-empty JSON object' }
        @{ Args = @{ RoleMapping = '["admin"]' }; Pattern = '-GmAdminOidcRoleMapping must be a non-empty JSON object' }
        @{ Args = @{ RoleMapping = '{"gm-admins":"root"}' }; Pattern = '-GmAdminOidcRoleMapping maps every group to one of the GM admin role codes' }
        @{ Args = @{ RoleMapping = 'not json' }; Pattern = '-GmAdminOidcRoleMapping is not valid JSON' }
        @{ Args = @{ ClientId = 'gm-${x}' }; Pattern = '-GmAdminOidcClientId must not contain "\$\{"' }
    )
    foreach ($case in $cases) {
        $arguments = $case.Args
        $message = Get-ThrownMessage { Invoke-OidcSettings @arguments }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "拒绝 $(($arguments.Keys | ForEach-Object { "$_=$($arguments[$_])" }) -join ' ')"
    }
}

# 应急开关的门禁:在临时目录里放一份配置来源的副本,改其中一处再核。
$SwitchFixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-gm-admin-switch-' + [guid]::NewGuid().ToString('N'))
$SwitchSources = @('go/mail/etc/mail.yaml', 'go/rank/etc/rank.yaml', 'go/login/etc/login.yaml', 'go/data_service/etc/data_service.yaml',
    'go/player_inbox/etc/player_inbox.yaml', 'go/match/etc/match_service.yaml', 'go/trade/etc/trade.yaml', 'java/mall_game/src/main/resources/application.yaml',
    'deploy/k8s/manifests/java-svc/mall.yaml')
function Invoke-SwitchGate {
    param([Parameter(Mandatory = $true)][string]$Profile, [string]$EditPath = '', [scriptblock]$Edit = $null)
    if (Test-Path -LiteralPath $SwitchFixtureRoot) { Remove-Item -LiteralPath $SwitchFixtureRoot -Recurse -Force }
    foreach ($relative in $SwitchSources) {
        $target = Join-Path $SwitchFixtureRoot $relative
        $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $target)
        Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $relative) -Destination $target
    }
    if ($EditPath) {
        $path = Join-Path $SwitchFixtureRoot $EditPath
        [IO.File]::WriteAllText($path, (& $Edit ([IO.File]::ReadAllText($path))))
    }
    $RepoRoot = $SwitchFixtureRoot
    $JavaSvcManifestsDir = Join-Path $SwitchFixtureRoot 'deploy/k8s/manifests/java-svc'
    $ReleaseProfile = $Profile
    return (Get-ThrownMessage { Assert-DualControlEmergencySwitchesOff })
}

Test-Case 'Assert-DualControlEmergencySwitchesOff:仓库现状在 prod 放行;注释行不算;dev 档放行打开的开关' {
    Assert-Equal -Expected '' -Actual (Invoke-SwitchGate -Profile 'prod') -Because '仓库里提交的配置来源一律是关的(mail.yaml 里那行说明是注释)'
    Assert-Equal -Expected '' -Actual (Invoke-SwitchGate -Profile 'dev' -EditPath 'go/mail/etc/mail.yaml' -Edit { param($t) $t.TrimEnd() + "`nAdminAllowUnapproved: true`n" }) -Because 'dev 档放行(本机与开发集群一个人能跑通)'
}

Test-Case 'Assert-DualControlEmergencySwitchesOff:staging / prod 拒绝任何一处会进集群的开关为 true(整份复制的 mail、照抄段落的 trade / match、商城 jar 缺省值与清单)' {
    $cases = @(
        @{ Path = 'go/mail/etc/mail.yaml'; Edit = { param($t) $t.TrimEnd() + "`nAdminAllowUnapproved: true`n" }; Pattern = 'go/mail/etc/mail\.yaml sets AdminAllowUnapproved' }
        @{ Path = 'go/rank/etc/rank.yaml'; Edit = { param($t) $t.TrimEnd() + "`nAdminAllowUnapproved: `"true`"`n" }; Pattern = 'go/rank/etc/rank\.yaml sets AdminAllowUnapproved' }
        @{ Path = 'go/trade/etc/trade.yaml'; Edit = { param($t) $t -replace '(?m)^  ManualResolutionEnvironment:', "  ManualResolutionAllowUnapproved: true`n  ManualResolutionEnvironment:" }; Pattern = 'go/trade/etc/trade\.yaml sets ManualResolutionAllowUnapproved' }
        @{ Path = 'go/match/etc/match_service.yaml'; Edit = { param($t) $t -replace '(?m)^GlobalMatch:[ \t]*\r?$', "GlobalMatch:`n  ManualResolutionAllowUnapproved: yes" }; Pattern = 'go/match/etc/match_service\.yaml sets ManualResolutionAllowUnapproved' }
        @{ Path = 'java/mall_game/src/main/resources/application.yaml'; Edit = { param($t) $t.Replace('${COMMERCE_ADMIN_ALLOW_UNAPPROVED:false}', '${COMMERCE_ADMIN_ALLOW_UNAPPROVED:true}') }; Pattern = 'must keep commerce\.admin-allow-unapproved' }
        @{ Path = 'deploy/k8s/manifests/java-svc/mall.yaml'; Edit = { param($t) $t.Replace('            - name: SPRING_PROFILES_ACTIVE', "            - name: COMMERCE_ADMIN_ALLOW_UNAPPROVED`n              value: `"true`"`n            - name: SPRING_PROFILES_ACTIVE") }; Pattern = 'The mall manifest sets COMMERCE_ADMIN_ALLOW_UNAPPROVED' }
    )
    foreach ($case in $cases) {
        foreach ($profile in @('staging', 'prod')) {
            $message = Invoke-SwitchGate -Profile $profile -EditPath $case.Path -Edit $case.Edit
            Assert-Match -Text $message -Pattern $case.Pattern -Because "$profile 档拒绝 $($case.Path) 里打开的应急开关,并点名文件与键"
        }
    }
}
Remove-Item -LiteralPath $SwitchFixtureRoot -Recurse -Force -ErrorAction SilentlyContinue

# ──────────────────────────────────────────────
# 4. DryRun:dev infra-up,带全局 Go / Java 服务、后台与各入口密钥,开 gRPC 口准入
# ──────────────────────────────────────────────
$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-gm-admin-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot
$CatalogPath = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'catalog') -Namespaces @($ZoneNamespace, $Infra)

function Invoke-GmAdminDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture; MMORPG_SQL_K8S_BOOT_CATALOG = $CatalogPath }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $fixtureEnv
}

$GmAdminArgs = @('-GmAdminOidcIssuer', 'https://idp.invalid/realms/gm', '-GmAdminOidcClientId', 'gm-admin',
    '-GmAdminOidcRedirectUri', 'http://127.0.0.1:3101/login', '-GmAdminOidcRoleMapping', '{"gm-approvers":"gm_approver","gm-admins":"admin"}')
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-KafkaBrokers', '3', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$GatewayAdminKey = 'contract-gateway-admin-api-key-00000000007'
$InboxAdminToken = 'contract-player-inbox-admin-token-000000008'
$CommerceAdminSecret = 'contract-mall-admin-role-secret-00000000009'
$InfraEnv = @{
    MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345'
    MMORPG_COMMERCE_GATEWAY_SECRET = 'contract-mall-gateway-role-secret-0000000001'; MMORPG_COMMERCE_PAYMENT_SECRET = 'contract-mall-payment-role-secret-0000000002'
    MMORPG_COMMERCE_DELIVERY_SECRET = 'contract-mall-delivery-role-secret-000000003'; MMORPG_COMMERCE_ADMIN_SECRET = $CommerceAdminSecret
    MMORPG_GATEWAY_ADMIN_API_KEY = $GatewayAdminKey; MMORPG_PLAYER_INBOX_ADMIN_TOKEN = $InboxAdminToken
}
foreach ($name in $EntrySpecs.Keys) { $InfraEnv[$EntrySpecs[$name].Env] = $EntrySpecs[$name].Value }

$run = Invoke-GmAdminDryRun -Arguments ($InfraArgs + $GmAdminArgs + @('-NetworkPolicies', 'on')) -Env $InfraEnv
$out = [string]$run.Output -replace "`r`n", "`n"
$assumedZone = [regex]::Match($out, 'rendering as if (?<ns>\S+) has one').Groups['ns'].Value

Test-Case 'DryRun(dev infra-up,带后台):成功;入口打出后台的身份模式与将要核对的运维预置 Secret' {
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "带后台的 infra-up DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $out -Pattern '(?m)^GM admin: identity=oidc issuer=https://idp\.invalid/realms/gm redirect=http://127\.0\.0\.1:3101/login role_groups=2 runtime_secret=gm-admin-runtime\s*$' -Because '写前打一行后台的设置'
    Assert-Match -Text $out -Pattern ('\[dry-run\] would check that Secret gm-admin-runtime in namespace ' + [regex]::Escape($Infra) + ' has the non-empty keys ' + [regex]::Escape($GmAdminRuntimeSecretKeys -join ', ')) -Because 'DryRun 不连集群,打一行将要核对的键(真跑缺 Secret / 缺键在写之前拒绝)'
    Assert-True -Condition ($assumedZone -match '^mmorpg-zone-') -Because "夹具前提:DryRun 按某个 zone 的 data-service 已就绪渲染(实际: '$assumedZone')"
}

Test-Case 'DryRun:密钥只进 Secret —— 整份输出不含任何一把的值;后台读的每把在 infra namespace 恰好写一次,且先于后台的工作负载' {
    foreach ($value in @($EntrySpecs.Values | ForEach-Object { $_.Value }) + @($GatewayAdminKey, $InboxAdminToken, $CommerceAdminSecret)) {
        Assert-True -Condition (-not $out.Contains($value)) -Because '整份 DryRun 输出(配置模板、清单、Secret 意图行)里不得出现任何一把密钥的值'
    }
    $deployIndex = $out.IndexOf('name: gm-admin-web')
    Assert-True -Condition ($deployIndex -gt 0) -Because '夹具前提:输出里有后台的 Deployment'
    foreach ($secret in @('gm-admin-oidc', 'trade-manual-resolution', 'match-manual-resolution', 'data-service-admin-token', 'login-admin-token', 'commerce-admin', 'gateway-admin-api-key')) {
        $pattern = '(?m)^\[dry-run\] kubectl apply -n ' + [regex]::Escape($Infra) + ' -f -   # Secret ' + [regex]::Escape($secret) + ' \(key [a-z-]+; value hidden\)'
        $lines = [regex]::Matches($out, $pattern)
        Assert-Equal -Expected 1 -Actual $lines.Count -Because "infra namespace 的 Secret $secret 只写一次(几个消费者共用同一份)"
        Assert-True -Condition ($lines.Count -eq 1 -and $lines[0].Index -lt $deployIndex) -Because "Secret $secret 先于后台的 Deployment 落地(secretKeyRef 不是 optional)"
    }
    Assert-NotMatch -Text $out -Pattern '# Secret gateway-admin-machine-key ' -Because 'infra-up 不部署 gateway,机器口令不进 infra namespace(后台不读它)'
    Assert-NotMatch -Text $out -Pattern '# Secret gm-admin-runtime ' -Because '运维预置的 Secret 部署脚本不建'
}

Test-Case 'DryRun:后台的配置模板 —— oidc 模式、四项 OIDC 设置、各下游写集群内 Service 名与明文,口令 / 密钥只写对变量的引用' {
    $cm = Select-Document -Output $out -Kind 'ConfigMap' -Name 'java-svc-gm-admin-config'
    Assert-Match -Text $cm -Pattern '(?m)^  application-k8s\.yaml: \|\s*$' -Because '键名是 application-k8s.yaml(清单启用 profile k8s)'
    # 块缩进 4 格之内:gm.* 的叶子再缩 4 格(gm → identity / oidc / game → 键),logging.config 与 jeecg.uploadType 缩 2 格。
    $expectedLines = @(
        '        mode: oidc'
        '        issuer: "https://idp.invalid/realms/gm"'
        '        client-id: "gm-admin"'
        '        client-secret: "${MMORPG_GM_OIDC_CLIENT_SECRET}"'
        '        redirect-uri: "http://127.0.0.1:3101/login"'
        "        role-mapping: '{`"gm-admins`":`"admin`",`"gm-approvers`":`"gm_approver`"}'"
        '        plaintext: true'
        "        gateway-url: http://gateway.${assumedZone}.svc:8081"
        '        gateway-admin-key: "${MMORPG_GATEWAY_ADMIN_API_KEY}"'
        "        data-target: data-service.${assumedZone}.svc:$($GoSvcCatalogue['data-service'].Port)"
        "        locator-target: player-locator.${assumedZone}.svc:$($GoSvcCatalogue['player-locator'].Port)"
        "        mail-target: mail.${Infra}.svc:$($GoSvcCatalogue['mail'].Port)"
        '        mail-admin-secret: "${MAIL_PRODUCER_SECRET_ADMIN}"'
        "        trade-target: trade.${Infra}.svc:$($GoSvcCatalogue['trade'].Port)"
        '        trade-admin-secret: "${MMORPG_TRADE_MANUAL_RESOLUTION_SECRET}"'
        "        mall-url: http://mall.${Infra}.svc:$($JavaSvcCatalogue['mall'].HttpPort)"
        '        mall-admin-secret: "${MMORPG_COMMERCE_ADMIN_SECRET}"'
        '        mall-admin-subject: mall-admin'
        "        login-target: login.${assumedZone}.svc:$($GoSvcCatalogue['login'].Port)"
        '        login-admin-token: "${MMORPG_LOGIN_ADMIN_TOKEN}"'
        '        data-admin-token: "${MMORPG_DATA_SERVICE_ADMIN_TOKEN}"'
        "        inbox-target: player-inbox.${Infra}.svc:$($GoSvcCatalogue['player-inbox'].Port)"
        '        inbox-admin-token: "${MMORPG_PLAYER_INBOX_ADMIN_TOKEN}"'
        "        rank-target: rank.${Infra}.svc:$($GoSvcCatalogue['rank'].Port)"
        '        rank-admin-secret: "${RANK_PRODUCER_SECRET_ADMIN}"'
        "        match-target: match.${Infra}.svc:$($GoSvcCatalogue['match'].Port)"
        '        match-formation-secret: "${MMORPG_MATCH_MANUAL_RESOLUTION_SECRET}"'
        '      config: classpath:logback-console.xml'
        '      uploadType: local'
    )
    foreach ($line in $expectedLines) {
        Assert-Match -Text $cm -Pattern ('(?m)^' + [regex]::Escape($line) + '[ \t]*$') -Because "配置模板要有这一行:$line"
    }
    Assert-NotMatch -Text $cm -Pattern 'enforce:' -Because 'oidc 模式双人复核恒开,模板不写开关(写 false 后台拒启)'
    Assert-NotMatch -Text $cm -Pattern 'scene-(targets-json|etcd-url|admin-secret)' -Because '集群里不开场景玩家命令(只限 dev / test 服),不给场景地址与密钥'
}

Test-Case 'DryRun:后台的 Deployment —— 每把经 Secret 注入并带指纹;配置模板也带指纹;没有遗留占位' {
    $deploy = Select-Document -Output $out -Kind 'Deployment' -Name 'gm-admin'
    foreach ($name in @('gm-admin-oidc', 'trade-manual-resolution', 'match-manual-resolution', 'data-service-admin-token', 'login-admin-token')) {
        Assert-EntrySecretInjected -Doc $deploy -Name $name -Label 'gm-admin'
    }
    Assert-EntrySecretAbsent -Doc $deploy -Name 'gateway-admin-machine-key' -Label 'gm-admin'
    Assert-EnvFromSecret -Doc $deploy -EnvName 'MMORPG_COMMERCE_ADMIN_SECRET' -SecretName 'commerce-admin' -SecretKey 'secret' -HashKey 'mmorpg.io/commerce-admin-hash' `
        -ExpectedHash (Get-ExpectedHash -Purpose 'commerce-admin/workload' -Value $CommerceAdminSecret) -Label 'gm-admin'
    Assert-EnvFromSecret -Doc $deploy -EnvName 'MMORPG_GATEWAY_ADMIN_API_KEY' -SecretName 'gateway-admin-api-key' -SecretKey 'api-key' -HashKey 'mmorpg.io/gateway-admin-api-key-hash' `
        -ExpectedHash (Get-ExpectedHash -Purpose 'gateway-admin-api-key' -Value $GatewayAdminKey) -Label 'gm-admin'
    Assert-EnvFromSecret -Doc $deploy -EnvName 'MMORPG_PLAYER_INBOX_ADMIN_TOKEN' -SecretName 'player-inbox-admin-token' -SecretKey 'admin-token' -HashKey 'mmorpg.io/player-inbox-admin-token-hash' `
        -ExpectedHash (Get-ExpectedHash -Purpose 'player-inbox-admin-token' -Value $InboxAdminToken) -Label 'gm-admin'
    $cm = Select-Document -Output $out -Kind 'ConfigMap' -Name 'java-svc-gm-admin-config'
    Assert-Match -Text $deploy -Pattern '(?m)^\s+mmorpg\.io/gm-admin-config-hash: "[0-9a-f]{12}"\s*$' -Because '配置模板的指纹:只改 OIDC 设置或下游地址时 Pod 也滚动'
    Assert-Match -Text $deploy -Pattern 'image: registry\.invalid/test/mmorpg-gm-admin:' -Because '两个容器拉后台镜像'
    Assert-NotMatch -Text $deploy -Pattern 'PLACEHOLDER_' -Because '占位必须全部替换'
    Assert-True -Condition ($null -ne $cm) -Because '夹具前提:配置模板在输出里'
}

Test-Case 'DryRun:入站准入 —— 后台自己的策略拒绝其它 Pod;后台调到的服务的准入名单都放行 gm-admin' {
    $policy = Select-Document -Output $out -Kind 'NetworkPolicy' -Name 'gm-admin-ingress-deny'
    Assert-Match -Text $policy -Pattern '(?s)podSelector:\s*\n\s+matchLabels:\s*\n\s+app: gm-admin\s*\n\s+policyTypes:\s*\n\s+- Ingress\s*$' -Because '只选中后台、不列 ingress 规则 = 拒绝其它 Pod 的全部入站'
    foreach ($name in @('trade-grpc-allowlist', 'mail-grpc-allowlist', 'rank-grpc-allowlist', 'match-grpc-allowlist', 'mall-grpc-allowlist')) {
        $doc = Select-Document -Output $out -Kind 'NetworkPolicy' -Name $name
        Assert-Match -Text $doc -Pattern 'values: \[[^\]]*\bgm-admin\b[^\]]*\]\s*\n\s+ports:' -Because "$name 放行后台"
    }
    Assert-NotMatch -Text $out -Pattern 'kubectl delete networkpolicy gm-admin-ingress-deny' -Because '开着时不删自己的策略'
}

Test-Case 'DryRun:trade 与 match 打开人工处置入口 —— 配置只写变量名与按档位的环境名,工作负载经 Secret 注入' {
    $tradeCm = Select-Document -Output $out -Kind 'ConfigMap' -Name 'go-svc-trade-config'
    Assert-Match -Text $tradeCm -Pattern '(?m)^      ManualResolutionSecretEnv: MMORPG_TRADE_MANUAL_RESOLUTION_SECRET\s*$' -Because 'trade 的 Commerce 段只写变量名'
    Assert-Match -Text $tradeCm -Pattern '(?m)^      ManualResolutionEnvironment: dev-global\s*$' -Because '环境名按档位(发起人逐字填它防连错环境)'
    $matchCm = Select-Document -Output $out -Kind 'ConfigMap' -Name 'go-svc-match-config'
    Assert-Match -Text $matchCm -Pattern '(?m)^      ManualResolutionSecretEnv: MMORPG_MATCH_MANUAL_RESOLUTION_SECRET\s*$' -Because 'match 的 GlobalMatch 段只写变量名'
    Assert-Match -Text $matchCm -Pattern '(?m)^      ManualResolutionEnvironment: dev-global\s*$' -Because 'match 的环境名与 trade 同一写法'
    foreach ($pair in @(@('trade', 'trade-manual-resolution'), @('match', 'match-manual-resolution'))) {
        $doc = Select-Document -Output $out -Kind 'StatefulSet' -Name $pair[0] -AllowMissing
        if ($null -eq $doc) { $doc = Select-Document -Output $out -Kind 'Deployment' -Name $pair[0] }
        Assert-EntrySecretInjected -Doc $doc -Name $pair[1] -Label $pair[0]
        foreach ($other in @($EntrySpecs.Keys | Where-Object { $_ -cne $pair[1] })) { Assert-EntrySecretAbsent -Doc $doc -Name $other -Label $pair[0] }
    }
}

$plainRun = Invoke-GmAdminDryRun -Arguments ($InfraArgs + $GmAdminArgs) -Env @{
    MMORPG_BATTLE_TOKEN_SECRET = $InfraEnv.MMORPG_BATTLE_TOKEN_SECRET; MMORPG_GATE_TOKEN_SECRET = $InfraEnv.MMORPG_GATE_TOKEN_SECRET
    MMORPG_COMMERCE_GATEWAY_SECRET = $InfraEnv.MMORPG_COMMERCE_GATEWAY_SECRET; MMORPG_COMMERCE_PAYMENT_SECRET = $InfraEnv.MMORPG_COMMERCE_PAYMENT_SECRET
    MMORPG_COMMERCE_DELIVERY_SECRET = $InfraEnv.MMORPG_COMMERCE_DELIVERY_SECRET; MMORPG_COMMERCE_ADMIN_SECRET = $CommerceAdminSecret
    MMORPG_GM_OIDC_CLIENT_SECRET = $EntrySpecs['gm-admin-oidc'].Value
}
$plainOut = [string]$plainRun.Output -replace "`r`n", "`n"

Test-Case 'DryRun(各入口密钥都没设):入口保持关闭 —— trade 的变量名仍是空串、match 不写这两个键;后台的配置模板不写那几路的引用;准入关着时删掉以前的策略' {
    Assert-Equal -Expected 0 -Actual $plainRun.ExitCode -Because "没设入口密钥的 infra-up DryRun 必须成功(各入口可空)。输出: $($plainRun.Output)"
    $tradeCm = Select-Document -Output $plainOut -Kind 'ConfigMap' -Name 'go-svc-trade-config'
    Assert-Match -Text $tradeCm -Pattern '(?m)^      ManualResolutionSecretEnv: ""\s*$' -Because '没注入:保持服务 yaml 的空串,入口关闭'
    $matchCm = Select-Document -Output $plainOut -Kind 'ConfigMap' -Name 'go-svc-match-config'
    Assert-NotMatch -Text $matchCm -Pattern 'ManualResolution(SecretEnv|Environment)' -Because '没注入:match 不写这两个键(写了变量名却没注入 match 会拒启)'
    $cm = Select-Document -Output $plainOut -Kind 'ConfigMap' -Name 'java-svc-gm-admin-config'
    foreach ($key in @('trade-admin-secret', 'match-formation-secret', 'login-admin-token', 'data-admin-token', 'inbox-admin-token')) {
        Assert-NotMatch -Text $cm -Pattern ('(?m)^\s+' + $key + ':') -Because "没注入的那一路整行不写($key 落回 jar 内的空串 = 那一路不可用)"
    }
    Assert-Match -Text $plainOut -Pattern 'admin entry: MMORPG_TRADE_MANUAL_RESOLUTION_SECRET unset' -Because '入口没开要在输出里看得见'
    Assert-Match -Text $plainOut -Pattern ('kubectl delete networkpolicy gm-admin-ingress-deny -n ' + [regex]::Escape($Infra) + ' --ignore-not-found') -Because '准入关着:删掉以前开着时留下的同名策略'
    Assert-NotMatch -Text $plainOut -Pattern '# Secret (trade-manual-resolution|match-manual-resolution|login-admin-token|data-service-admin-token) ' -Because '没设的不建 Secret'
}

# ──────────────────────────────────────────────
# 5. DryRun:dev zone-up,注入 data-service / login / gateway 的管理口令
# ──────────────────────────────────────────────
$ZoneArgs = @('-Command', 'zone-up', '-ZoneName', $Zone, '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test')
$zoneEnv = @{ MMORPG_GATEWAY_ADMIN_API_KEY = $GatewayAdminKey }
foreach ($name in @('data-service-admin-token', 'login-admin-token', 'gateway-admin-machine-key')) { $zoneEnv[$EntrySpecs[$name].Env] = $EntrySpecs[$name].Value }
$zoneRun = Invoke-GmAdminDryRun -Arguments $ZoneArgs -Env $zoneEnv
$zoneOut = [string]$zoneRun.Output -replace "`r`n", "`n"

Test-Case 'DryRun(zone-up):data-service 写变量名并注入自己的口令;login 注入自己的口令;gateway 写机器口令的引用并注入;值不进输出' {
    Assert-Equal -Expected 0 -Actual $zoneRun.ExitCode -Because "zone-up DryRun 必须成功。输出: $($zoneRun.Output)"
    foreach ($name in @('data-service-admin-token', 'login-admin-token', 'gateway-admin-machine-key')) {
        Assert-True -Condition (-not $zoneOut.Contains($EntrySpecs[$name].Value)) -Because "整份输出不含 $name 的值"
        $pattern = '(?m)^\[dry-run\] kubectl apply -n ' + [regex]::Escape($ZoneNamespace) + ' -f -   # Secret ' + [regex]::Escape($name) + ' \(key ' + $EntrySpecs[$name].Key + '; value hidden\)'
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($zoneOut, $pattern).Count) -Because "zone namespace 的 Secret $name 只写一次"
    }
    $dsCm = Select-Document -Output $zoneOut -Kind 'ConfigMap' -Name 'go-svc-data-service-config'
    Assert-Match -Text $dsCm -Pattern '(?m)^    AdminTokenEnv: MMORPG_DATA_SERVICE_ADMIN_TOKEN\s*$' -Because 'data-service 只写变量名,常驻启动时从它填 AdminToken'
    Assert-NotMatch -Text $dsCm -Pattern '(?m)^\s+AdminToken:' -Because '口令值不进 ConfigMap'
    $gwCm = Select-Document -Output $zoneOut -Kind 'ConfigMap' -Name 'java-svc-gateway-config'
    Assert-Match -Text $gwCm -Pattern '(?m)^      machine-key: "\$\{MMORPG_GATEWAY_ADMIN_MACHINE_KEY\}"\s*$' -Because 'gateway 只写对机器口令变量的引用'
    foreach ($pair in @(@('data-service', 'data-service-admin-token'), @('login', 'login-admin-token'), @('gateway', 'gateway-admin-machine-key'))) {
        $doc = Select-Document -Output $zoneOut -Kind 'StatefulSet' -Name $pair[0] -AllowMissing
        if ($null -eq $doc) { $doc = Select-Document -Output $zoneOut -Kind 'Deployment' -Name $pair[0] }
        Assert-EntrySecretInjected -Doc $doc -Name $pair[1] -Label $pair[0]
        foreach ($other in @($EntrySpecs.Keys | Where-Object { $_ -cne $pair[1] })) { Assert-EntrySecretAbsent -Doc $doc -Name $other -Label $pair[0] }
    }
    Assert-NotMatch -Text $zoneOut -Pattern 'login-admin-token, optional: true' -Because 'login 的口令不再读 domain-service-secrets(目录 login-admin-token 是唯一来源)'
}

# ──────────────────────────────────────────────
# 6. 负向
# ──────────────────────────────────────────────
Test-Case '负向:zone-up 带 -GmAdminOidc* 在任何写操作之前拒绝(后台在 infra namespace);四项不齐同样拒绝' {
    $zoneRefused = Invoke-GmAdminDryRun -Arguments ($ZoneArgs + $GmAdminArgs)
    Assert-True -Condition ($zoneRefused.ExitCode -ne 0) -Because "给了却用不上的参数要拒绝。输出: $($zoneRefused.Output)"
    Assert-Match -Text $zoneRefused.Output -Pattern '-GmAdminOidc\* only apply when the global Go and Java services are deployed' -Because '点名原因'
    Assert-NotMatch -Text $zoneRefused.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '拒绝之前没有任何写操作'
    $partial = Invoke-GmAdminDryRun -Arguments ($InfraArgs + @('-GmAdminOidcIssuer', 'https://idp.invalid/realms/gm')) -Env $InfraEnv
    Assert-True -Condition ($partial.ExitCode -ne 0) -Because "四项不齐要拒绝。输出: $($partial.Output)"
    Assert-Match -Text $partial.Output -Pattern 'must be given together' -Because '点名原因'
    Assert-NotMatch -Text $partial.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '拒绝之前没有任何写操作'
    $noSecretEnv = @{} + $InfraEnv
    $noSecretEnv.Remove('MMORPG_GM_OIDC_CLIENT_SECRET')
    $noSecret = Invoke-GmAdminDryRun -Arguments ($InfraArgs + $GmAdminArgs) -Env $noSecretEnv
    Assert-True -Condition ($noSecret.ExitCode -ne 0) -Because "部署后台却没给 OIDC 客户端密钥要拒绝(各档位都不回落)。输出: $($noSecret.Output)"
    Assert-Match -Text $noSecret.Output -Pattern 'MMORPG_GM_OIDC_CLIENT_SECRET' -Because '点名缺的变量'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName 'k8s GM admin deploy contract')
