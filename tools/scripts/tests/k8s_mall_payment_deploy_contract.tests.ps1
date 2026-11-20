#requires -Version 7
<#
.SYNOPSIS
    官方商城 mall(java/mall_game)与支付 payment(go/payment)上 K8s 的部署侧契约测试
    (docs/design/payment-recharge.md §22;deploy/k8s/README.md「商城 mall」「支付 payment」)。

.DESCRIPTION
    本文件钉住:
      1. 登记:两份服务目录、镜像名、端口、写者名单;payment 与 mall 都不提供金丝雀;zone-up 不部署商城;
         调用密钥在工作负载密钥目录里的变量名、键与消费者;
      2. 清单:payment 的三个端口与服务 yaml 同源,回调入口 Service 只暴露回调口、类型留给部署脚本填;
         两份清单的 tier 标签(容器日志按它采集);
      3. 纯函数:商城调用密钥的解析(Resolve-CommerceSecrets)、Payment 配置段的渲染(New-PaymentSectionYaml、
         Get-PaymentDeployPlan)、商城的库档位(Resolve-MallSqlMode);
      4. DryRun(dev infra-up,带全局 Go 服务与商城,开 gRPC 口准入):payment 与 mall 的配置、清单、Job、密钥注入、
         准入策略与先后顺序;trade 指向集群内商城;
      5. DryRun(-PaymentConfigPath):运维的 Payment 段替换缺省段,两项地址仍由脚本写死,开着的渠道要的密钥只核对
         运维预置 Secret 的键;zone-up 带这个参数在任何写操作之前拒绝。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。
    断言不写 payment 的端口字面量:一律从 go/payment/etc/payment.yaml 取,与清单、配置模板互相核对。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== k8s_deploy.ps1 mall and payment deployment contract tests ==='

# 被抽出来的函数按动态作用域读这个变量(在 k8s_deploy.ps1 里它是脚本级变量)。
$RepoRoot = Get-RepoRoot
$Infra = 'commerce-infra'
$Zone = 'commerce-test'
$ZoneNamespace = "mmorpg-zone-$Zone"
$AuthorityHost = 'tidb.invalid:4000'
$PaymentYamlPath = 'go/payment/etc/payment.yaml'

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
$deployFunctions = @(
    'Get-AuthoritativeScalar', 'Get-AuthoritativeYamlBlock', 'Get-MallServiceUrl', 'Get-ZoneScopedJavaSvcNames', 'Get-ManifestContainerPorts',
    'Resolve-CommerceSecrets', 'New-PaymentSectionYaml', 'Get-PaymentDeployPlan',
    'Get-FriendSqlSettings', 'Get-GlobalAuthoritySqlSettings', 'Resolve-MallSqlMode'
)
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

# payment 的三个端口:权威值在服务 yaml。
$PaymentGrpcPort = [regex]::Match([string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'ListenOn'), ':(?<p>\d+)\s*$').Groups['p'].Value
$PaymentMetricsPort = [regex]::Match([string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'MetricsListenAddr'), ':(?<p>\d+)\s*$').Groups['p'].Value
$PaymentNotifyPort = [regex]::Match([string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.NotifyListenAddr'), ':(?<p>\d+)\s*$').Groups['p'].Value
# 商城的端口:权威值是 jar 内 application.yaml 的 server.port 缺省值。
$MallPort = [regex]::Match((Read-RepoText 'java/mall_game/src/main/resources/application.yaml'), '(?m)^  port: \$\{COMMERCE_PORT:(?<p>\d+)\}\s*$').Groups['p'].Value
$MallUrl = "http://mall.${Infra}:$MallPort"

# 本套 DryRun 注入的五把密钥:每把一个互不相同的值,用来核指纹、核值不进输出。变量名、键、消费者与目录表逐项一致由第一条用例核。
$SecretSpecs = [ordered]@{
    'commerce-gateway'  = @{ Env = 'MMORPG_COMMERCE_GATEWAY_SECRET'; Value = 'contract-mall-gateway-role-secret-0000000001'; Consumers = @('mall', 'trade') }
    'commerce-payment'  = @{ Env = 'MMORPG_COMMERCE_PAYMENT_SECRET'; Value = 'contract-mall-payment-role-secret-0000000002'; Consumers = @('mall', 'payment') }
    'commerce-delivery' = @{ Env = 'MMORPG_COMMERCE_DELIVERY_SECRET'; Value = 'contract-mall-delivery-role-secret-000000003'; Consumers = @('mall', 'trade') }
    'commerce-admin'    = @{ Env = 'MMORPG_COMMERCE_ADMIN_SECRET'; Value = 'contract-mall-admin-role-secret-00000000004'; Consumers = @('mall') }
    'payment-devpay'    = @{ Env = 'MMORPG_PAYMENT_DEVPAY_SECRET'; Value = 'contract-payment-devpay-secret-000000000005'; Consumers = @('payment') }
}

$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-mall-payment-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot
$CatalogPath = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'catalog') -Namespaces @($ZoneNamespace, $Infra)

# 与 k8s_deploy_contract.tests.ps1 同一套 DryRun 夹具:存档库清单占位值与写者计划;全球 SQL 入口由各组参数自己给。
function Invoke-CommerceDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture; MMORPG_SQL_K8S_BOOT_CATALOG = $CatalogPath }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $fixtureEnv
}

# DryRun 把一个 manifest 文件整份打成一个块。按 --- 切成单份文档,再按 kind + 顶层 metadata.name 精确取:
# 恰好 1 份才返回;-AllowMissing 时 0 份返回 $null;其余一律 throw(fail-closed:取不到不能让断言在空串上通过)。
function Select-CommerceDocument {
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

# 一行文字在输出里第一次出现的位置;没有就 throw,免得 -1 把「先后」断言变成假绿。
function Get-OutputIndex {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Needle)
    $at = $Text.IndexOf($Needle, [System.StringComparison]::Ordinal)
    if ($at -lt 0) { throw "输出里找不到「$Needle」" }
    return $at
}

# 与 k8s_deploy.ps1 Get-InjectedSecretFingerprint 同一算法:SHA256("<Secret 名>/workload`n<值>") 的前 12 位十六进制。
function Get-ExpectedSecretHash {
    param([Parameter(Mandatory = $true)][string]$SecretName, [Parameter(Mandatory = $true)][string]$Value)
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try { $bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$SecretName/workload`n$Value")) } finally { $sha256.Dispose() }
    return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12))
}

# 工作负载里这把密钥:恰好一条 env,secretKeyRef 到本 namespace 同名 Secret 的 secret 键、不是 optional;Pod 模板带只哈希这把密钥的指纹。
function Assert-SecretInjected {
    param([Parameter(Mandatory = $true)][string]$Doc, [Parameter(Mandatory = $true)][string]$SecretName, [Parameter(Mandatory = $true)][string]$Label)
    $spec = $SecretSpecs[$SecretName]
    $text = $Doc -replace "`r`n", "`n"
    $envName = [regex]::Escape($spec.Env)
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^[ ]*- name: ' + $envName + '[ ]*$').Count) -Because "${Label}:$($spec.Env) 只能注入一份"
    $reference = '(?m)^[ ]*- name: ' + $envName + '[ ]*\n[ ]+valueFrom:[ ]*\n[ ]+secretKeyRef:[ ]*\n[ ]+name: ' + [regex]::Escape($SecretName) + '[ ]*\n[ ]+key: secret[ ]*\n(?![ ]+optional:)'
    Assert-True -Condition ($text -cmatch $reference) -Because "${Label}:$($spec.Env) 必须 secretKeyRef 到本 namespace 的 Secret $SecretName 的 secret 键,且不是 optional"
    $found = [regex]::Matches($text, '(?m)^[ ]+' + [regex]::Escape("mmorpg.io/$SecretName-hash") + ': "(?<hash>[0-9a-f]{12})"[ ]*$')
    Assert-Equal -Expected 1 -Actual $found.Count -Because "${Label}:Pod 模板上 mmorpg.io/$SecretName-hash 应恰好 1 条"
    Assert-True -Condition ($found.Count -eq 1 -and $found[0].Groups['hash'].Value -ceq (Get-ExpectedSecretHash -SecretName $SecretName -Value $spec.Value)) -Because "${Label}:$SecretName 的指纹只由这把密钥决定(换密钥即滚动)"
}

# 不该读这把密钥的工作负载:不注入、不带指纹。
function Assert-SecretAbsent {
    param([Parameter(Mandatory = $true)][string]$Doc, [Parameter(Mandatory = $true)][string]$SecretName, [Parameter(Mandatory = $true)][string]$Label)
    $text = $Doc -replace "`r`n", "`n"
    Assert-True -Condition ($text -notmatch ('(?m)^[ ]*- name: ' + [regex]::Escape($SecretSpecs[$SecretName].Env) + '[ ]*$')) -Because "${Label}:不读 $SecretName,不得注入 $($SecretSpecs[$SecretName].Env)"
    Assert-True -Condition (-not $text.Contains("mmorpg.io/$SecretName-hash")) -Because "${Label}:不读 $SecretName,不带它的指纹"
}

# NetworkPolicy 文档 → 目标 app、对所有来源开放的端口、受保护的端口与放行名单(只认 New-GrpcAllowListNetworkPolicyYaml 生成的两条 ingress 形状)。
function Get-AllowListShape {
    param([Parameter(Mandatory = $true)][string]$Block)
    $text = $Block -replace "`r`n", "`n"
    $target = [regex]::Match($text, '(?s)podSelector:\s*\n\s+matchExpressions:\s*\n\s+- key: app\s*\n\s+operator: In\s*\n\s+values: \[(?<v>[^\]]*)\]')
    $fromIndex = $text.IndexOf('    - from:')
    $ingressIndex = $text.IndexOf('  ingress:')
    $openText = if ($fromIndex -gt $ingressIndex -and $ingressIndex -ge 0) { $text.Substring($ingressIndex, $fromIndex - $ingressIndex) } else { '' }
    $open = @([regex]::Matches($openText, 'port: (\d+)') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    $guarded = [regex]::Match($text.Substring([Math]::Max(0, $fromIndex)), '(?s)values: \[(?<v>[^\]]*)\]\s*\n\s+ports:\s*\n\s+- protocol: TCP\s*\n\s+port: (?<p>\d+)')
    return [pscustomobject]@{
        TargetApps  = if ($target.Success) { @($target.Groups['v'].Value -split ',\s*') } else { @() }
        OpenPorts   = $open
        GuardedPort = if ($guarded.Success) { [int]$guarded.Groups['p'].Value } else { 0 }
        AllowedApps = if ($guarded.Success) { @($guarded.Groups['v'].Value -split ',\s*') } else { @() }
    }
}

# 写者的控制文件都在 emptyDir 挂载根下的私有子目录(与 logical_db_deploy_contract.tests.ps1 的同名断言同一口径)。
function Assert-WriterPrivatePaths {
    param([Parameter(Mandatory = $true)][string]$Block, [Parameter(Mandatory = $true)][string]$Service, [Parameter(Mandatory = $true)][string]$ConfigFile)
    $file = [regex]::Escape($ConfigFile)
    Assert-Match -Text $Block -Pattern 'umask 077; mkdir -p /run/sql-writer/grant/private /run/sql-writer/runtime/private/env;' -Because "${Service}:init 建私有子目录,挂载根过不了私有目录校验"
    Assert-Match -Text $Block -Pattern '"-boot-output","/run/sql-writer/grant/private/boot-grant\.json"' -Because "${Service}:grant 写进子目录"
    Assert-Match -Text $Block -Pattern ('"-runtime-config-output","/run/sql-writer/runtime/private/' + $file + '"') -Because "${Service}:选中配置写进子目录"
    Assert-Match -Text $Block -Pattern '"-runtime-env-output","/run/sql-writer/runtime/private/env"' -Because "${Service}:环境文件写进子目录"
    Assert-Match -Text $Block -Pattern '"-pod-name","\$\(POD_NAME\)"' -Because "${Service}:init 按 Pod 序号选名册的槽位"
    Assert-Match -Text $Block -Pattern 'name: MMORPG_SQL_WRITER_GRANT_FILE\s+value: /run/sql-writer/grant/private/boot-grant\.json' -Because "${Service}:主容器读子目录"
    Assert-Match -Text $Block -Pattern 'name: MMORPG_SQL_WRITER_CONSUME_DIR\s+value: /run/sql-writer/consume/private' -Because "${Service}:消费标记写子目录"
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($Block, 'mmorpg\.io/sql-writer: "true"').Count) -Because "${Service}:回收 CronJob 只处理带这个 label 的 Pod,label 只写一次"
    Assert-Match -Text $Block -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "tidb"' -Because "${Service}:registrar 按 -DatabaseBackend 核真实引擎"
    Assert-Match -Text $Block -Pattern "name: MMORPG_DB_PLACEMENT\s+value: '\{" -Because "${Service}:registrar 按库映射连库"
    Assert-NotMatch -Text $Block -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because "${Service}:占位必须全部替换"
}

# ──────────────────────────────────────────────
# 1. 登记与清单(只读仓库文件)
# ──────────────────────────────────────────────
Test-Case '登记:payment 是全局池写者、带迁移 Job;mall 是全局 Java 服务,zone-up 不部署它;两者都不提供金丝雀;镜像名、端口各处一致' {
    Assert-True -Condition ($PaymentGrpcPort -match '^\d+$' -and $PaymentMetricsPort -match '^\d+$' -and $PaymentNotifyPort -match '^\d+$') -Because "夹具前提:$PaymentYamlPath 的三个监听地址能解析出端口"
    Assert-True -Condition ($MallPort -match '^\d+$') -Because '夹具前提:商城 jar 内 application.yaml 的 server.port 缺省值能解析出端口'

    $goCatalogue = Get-DeployLiteral -Left '$GoSvcCatalogue'
    $payment = $goCatalogue['payment']
    Assert-True -Condition ($null -ne $payment) -Because '$GoSvcCatalogue 里要有 payment'
    Assert-True -Condition ([bool]$payment.Global -and -not $payment.StartsBeforeZones) -Because 'payment 是全局池,进程排在第一个 zone 的 data-service 之后'
    Assert-Equal -Expected 'go-svc-payment-config/payment.yaml/payment.yaml/payment-migrate.yaml' -Actual ('{0}/{1}/{2}/{3}' -f $payment.ConfigMap, $payment.Manifest, $payment.ConfigFile, $payment.MigrateJob) -Because 'payment 的配置模板、清单、配置文件名与迁移 Job'
    Assert-Equal -Expected $PaymentGrpcPort -Actual ([string]$payment.Port) -Because '目录里的 gRPC 端口 == 服务 yaml 的 ListenOn'
    Assert-True -Condition (@(Get-DeployLiteral -Left '$script:SqlWriterGoSvcs') -ccontains 'payment') -Because 'payment 写 SQL,要在写者名单里(StatefulSet、写者 boot、boot 目录条目都按它走)'

    $JavaSvcCatalogue = Get-DeployLiteral -Left '$JavaSvcCatalogue'
    $mall = $JavaSvcCatalogue['mall']
    Assert-True -Condition ($null -ne $mall -and [bool]$mall.Global) -Because '$JavaSvcCatalogue 里的 mall 是全局服务'
    Assert-Equal -Expected 'java-svc-mall-config/mall.yaml' -Actual ('{0}/{1}' -f $mall.ConfigMap, $mall.Manifest) -Because 'mall 的配置模板与清单'
    Assert-Equal -Expected $MallPort -Actual ([string]$mall.HttpPort) -Because '目录里的端口 == jar 内 server.port 的缺省值'
    $zoneScoped = @(Get-ZoneScopedJavaSvcNames)
    Assert-True -Condition ($zoneScoped -ccontains 'gateway' -and $zoneScoped -cnotcontains 'mall') -Because 'zone-up 部署 gateway,不部署商城'
    $InfraNamespace = 'unit-infra'
    Assert-Equal -Expected "http://mall.unit-infra:$MallPort" -Actual (Get-MallServiceUrl) -Because 'trade 与 payment 拨的是 infra namespace 的 Service mall'

    $canaryReady = @(Get-DeployLiteral -Left '$script:CanaryReadyGoSvcs')
    Assert-True -Condition ($canaryReady -cnotcontains 'payment' -and $canaryReady -cnotcontains 'mall') -Because 'payment 与 mall 不进金丝雀名单(负责人 2026-11-19 定):不起金丝雀副本,准入名单里也不补 <app>-canary'

    $deployScript = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    $goImageName = [regex]::Match((Read-RepoText 'tools/scripts/go_svc_image.ps1'), '(?m)^\s*payment\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    Assert-True -Condition $goImageName.Success -Because 'go_svc_image.ps1 的服务表里要有 payment'
    Assert-Equal -Expected ([string]$payment.ImageName) -Actual $goImageName.Groups['n'].Value -Because '发布的镜像名与部署拉的镜像名必须一致(payment)'
    $javaImageName = [regex]::Match((Read-RepoText 'tools/scripts/java_svc_image.ps1'), '(?m)^\s*mall\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    Assert-True -Condition $javaImageName.Success -Because 'java_svc_image.ps1 的服务表里要有 mall'
    Assert-Equal -Expected ([string]$mall.ImageName) -Actual $javaImageName.Groups['n'].Value -Because '发布的镜像名与部署拉的镜像名必须一致(mall)'
    $localPort = [regex]::Match((Read-RepoText 'tools/scripts/go_services.ps1'), '(?m)^\s*payment\s*=\s*@\{[^}]*Port\s*=\s*(?<p>\d+)')
    Assert-Equal -Expected $PaymentGrpcPort -Actual $localPort.Groups['p'].Value -Because 'go_services.ps1 目录里的 payment 端口 == 服务 yaml 的 ListenOn'
    Assert-Match -Text $deployScript -Pattern "PaymentNodeService\s*=\s*'go/payment/etc/payment\.yaml'" -Because '路由服预算链(Assert-RouterForwardBudget)要核 payment 的 zrpc Timeout'
    # 商城的 Hikari 池上限写在 Java 代码里,连接总账里是常数:两边不一致时总账会少算或多算连接。
    $mallPoolSource = [regex]::Match((Read-RepoText 'java/mall_game/src/main/java/com/game/mall/DatabaseConfiguration.java'), 'setMaximumPoolSize\((?<n>\d+)\)')
    $mallPoolLedger = [regex]::Match($deployScript, '(?m)^\s*MallMaxPool\s*=\s*(?<n>\d+)\s*$')
    Assert-True -Condition ($mallPoolSource.Success -and $mallPoolLedger.Success) -Because '夹具前提:商城的池上限在 DatabaseConfiguration.poolConfig,连接总账的输入里有 MallMaxPool'
    Assert-Equal -Expected $mallPoolSource.Groups['n'].Value -Actual $mallPoolLedger.Groups['n'].Value -Because '连接总账的 MallMaxPool == 商城代码里的 Hikari 池上限'
}

Test-Case '登记:五把密钥在工作负载密钥目录里的变量名、键与消费者;部署脚本不读也不建运维预置的渠道密钥 Secret' {
    $catalog = Get-DeployLiteral -Left '$script:WorkloadSecretCatalog'
    foreach ($name in $SecretSpecs.Keys) {
        Assert-True -Condition ($catalog.Contains($name)) -Because "目录里要有 $name"
        Assert-Equal -Expected $SecretSpecs[$name].Env -Actual $catalog[$name].Env -Because "$name 的变量名(与部署输入变量同名)"
        Assert-Equal -Expected 'secret' -Actual $catalog[$name].SecretKey -Because "$name 的键"
        Assert-Equal -Expected (@($SecretSpecs[$name].Consumers) -join ',') -Actual (@($catalog[$name].Consumers) -join ',') -Because "$name 的消费者"
    }
    $deployScript = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    Assert-NotMatch -Text $deployScript -Pattern 'Apply-InjectedSecret[^\r\n]*payment-channel-secrets|-Name\s+[''"]payment-channel-secrets[''"]' -Because '渠道密钥是商户凭据,由运维预置;部署脚本只核对键在不在,不经手值'
}

Test-Case '清单:payment 三个端口与服务 yaml 同源,回调入口 Service 只暴露回调口、类型由部署脚本填;渠道密钥整体挂运维预置的 Secret' {
    $manifest = Read-RepoText 'deploy/k8s/manifests/go-svc/payment.yaml'
    $expectedPorts = @(@($PaymentGrpcPort, $PaymentMetricsPort, $PaymentNotifyPort) | ForEach-Object { [int]$_ } | Sort-Object) -join ','
    Assert-Equal -Expected $expectedPorts -Actual ((Get-ManifestContainerPorts -ManifestContent $manifest) -join ',') -Because '清单声明的 containerPort 就是服务 yaml 的三个监听端口(准入策略按它放行指标口与回调口)'
    Assert-Match -Text $manifest -Pattern ('\{name: grpc,\s*port: ' + $PaymentGrpcPort + ',\s*targetPort: ' + $PaymentGrpcPort + '\}') -Because 'Service payment 的 port / targetPort == 服务 yaml 的 ListenOn'
    Assert-Match -Text $manifest -Pattern ('prometheus\.io/port: "' + $PaymentMetricsPort + '"') -Because '抓取注解 == 服务 yaml 的 MetricsListenAddr'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($manifest, 'PLACEHOLDER_PAYMENT_NOTIFY_SERVICE_TYPE').Count) -Because '回调入口 Service 的类型占位恰好一处(Apply-OneGoSvc 按它替换,不是一处就拒绝)'
    $notify = @(($manifest -split '(?m)^---[ \t]*$') | Where-Object { $_ -cmatch '(?m)^  name: payment-notify\s*$' })
    Assert-Equal -Expected 1 -Actual $notify.Count -Because '清单里恰好一份 Service payment-notify'
    Assert-Match -Text $notify[0] -Pattern '(?m)^kind: Service\s*$' -Because 'payment-notify 是 Service(不用 Ingress:HTTPS 在云负载均衡或外部七层终结)'
    Assert-Match -Text $notify[0] -Pattern '(?m)^  type: PLACEHOLDER_PAYMENT_NOTIFY_SERVICE_TYPE\s*$' -Because '类型与 gate 对外 Service 同一口径,由部署脚本按运维档位填'
    Assert-Match -Text $notify[0] -Pattern '\{name: notify,\s*port: 443,\s*targetPort: notify\}' -Because '对外 443,回源到容器的回调口'
    $notifySpec = @(($notify[0] -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-NotMatch -Text $notifySpec -Pattern ('name: grpc|name: metrics|' + $PaymentGrpcPort + '|' + $PaymentMetricsPort) -Because '回调入口只暴露回调口,gRPC 与指标口不出集群'
    Assert-NotMatch -Text $manifest -Pattern '(?m)^kind: Ingress\s*$' -Because '不引入 Ingress'
    Assert-Match -Text $manifest -Pattern '- secretRef: \{name: payment-channel-secrets, optional: true\}' -Because '渠道密钥从运维预置的 Secret 整体注入;没开要它的渠道时这个 Secret 可以不存在'
    Assert-Match -Text $manifest -Pattern 'labels: \{app: payment, tier: go-svc, mmorpg\.io/sql-writer: "true"\}' -Because 'Pod 模板带 tier: go-svc(容器日志按它采集)与写者标签'
    $migrate = Read-RepoText 'deploy/k8s/manifests/go-svc/payment-migrate.yaml'
    Assert-Match -Text $migrate -Pattern 'name: MMORPG_PAYMENT_SQL_DSN\s+valueFrom:\s+secretKeyRef: \{name: PLACEHOLDER_SCHEMA_PERMISSION_SECRET, key: dsn, optional: false\}' -Because '迁移用 schema 账号,不用常驻账号'
    Assert-Match -Text $migrate -Pattern '"-migrate"' -Because '部署先迁结构再启动'
}

Test-Case '清单:mall 是两副本写者 StatefulSet + ClusterIP Service + PDB,端口与目录一致,Pod 模板带 tier: java-svc;建表 Job 建闸门表并装业务表' {
    $manifest = Read-RepoText 'deploy/k8s/manifests/java-svc/mall.yaml'
    Assert-Equal -Expected $MallPort -Actual ((Get-ManifestContainerPorts -ManifestContent $manifest) -join ',') -Because '商城只声明内部 HTTP 口'
    Assert-Match -Text $manifest -Pattern '(?m)^kind: StatefulSet\s*$' -Because '写者是 StatefulSet:Pod 序号就是写者名册的槽位'
    Assert-NotMatch -Text $manifest -Pattern '(?m)^kind: (Deployment|Ingress)\s*$' -Because '不是 Deployment,也没有对集群外的入口'
    Assert-Match -Text $manifest -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 官方商城是单点'
    Assert-Match -Text $manifest -Pattern '(?m)^  serviceName: mall\s*$' -Because 'StatefulSet 要 serviceName,用同名 Service'
    Assert-Match -Text $manifest -Pattern '(?m)^  type: ClusterIP\s*$' -Because '调用方只有集群内的 trade 与 payment'
    Assert-Match -Text $manifest -Pattern '(?m)^  minAvailable: 1\s*$' -Because 'PDB 写在主清单里,至少留一个副本'
    Assert-Match -Text $manifest -Pattern '(?m)^      labels:\s*\n        app: mall\s*\n        tier: java-svc\s*$' -Because 'Pod 模板带 tier: java-svc(容器日志按它采集);labels 用块式,写者标签由部署脚本补'
    Assert-NotMatch -Text $manifest -Pattern 'secretKeyRef|COMMERCE_' -Because '四把调用密钥由部署脚本按目录注入,清单里不手写'
    $schemaJob = Read-RepoText 'deploy/k8s/manifests/java-svc/mall-schema.yaml'
    Assert-Match -Text $schemaJob -Pattern 'args: \["-command", "prepare-schema", "-schema", "mmorpg_mall", "-ddl-file", "/app/mall-schema/schema\.sql", "-timeout", "60s"\]' -Because '闸门表走 prepare-schema,业务表由同一次调用的 -ddl-file 装(做法同 gateway-schema)'
    Assert-Match -Text $schemaJob -Pattern 'configMap: \{name: mall-schema-sql\}' -Because '-ddl-file 读的是部署脚本渲染的 ConfigMap mall-schema-sql'
    $mallDdl = Read-RepoText 'java/mall_game/src/main/resources/db/schema.sql'
    Assert-NotMatch -Text $mallDdl -Pattern '(?im)^\s*INSERT\s+INTO\b' -Because '-ddl-file 只收幂等语句:存储合同戳要用 INSERT IGNORE INTO,否则 Job 第二次跑就失败'
    Assert-Match -Text $mallDdl -Pattern '(?im)^INSERT IGNORE INTO commerce_schema_version\b' -Because '合同戳仍由这份文件盖,重跑不改已有的戳'
}

Test-Case '告警:payment-alerts.yaml 引用的指标名与 outcome 取值都能在 go/payment 的源码里找到' {
    $alerts = Read-RepoText 'deploy/k8s/payment-alerts.yaml'
    $source = Read-RepoText 'go/payment/internal/core/workers.go'
    Assert-Match -Text $alerts -Pattern '(?m)^kind: PrometheusRule\s*$' -Because '与 rank / mail 同一种告警资源'
    Assert-NotMatch -Text $alerts -Pattern 'player_id|order_id|transaction_id' -Because '告警不按玩家、订单或交易分组(高基数)'
    $metrics = @([regex]::Matches($alerts, '\bpayment_[a-z_]+\b') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected $true -Actual ($metrics.Count -ge 5) -Because '告警要覆盖回调、事实、商城同步、渠道调用与退款'
    foreach ($metric in $metrics) {
        $name = [regex]::Escape($metric.Substring('payment_'.Length))
        Assert-Match -Text $source -Pattern ('Namespace: "payment", Name: "' + $name + '"') -Because "告警引用的 $metric 必须是支付服务真的在发的指标"
    }
    $outcomes = @([regex]::Matches($alerts, 'outcome=~?"([^"]+)"') | ForEach-Object { $_.Groups[1].Value -split '\|' } | Sort-Object -Unique)
    Assert-Equal -Expected $true -Actual ($outcomes.Count -gt 0) -Because '告警按 outcome 取值筛选'
    foreach ($outcome in $outcomes) {
        Assert-Match -Text $source -Pattern ('(?<![a-z_])' + [regex]::Escape($outcome) + '(?![a-z_])') -Because "outcome=$outcome 必须是源码里出现的取值"
    }
}

# ──────────────────────────────────────────────
# 2. 纯函数
# ──────────────────────────────────────────────
$CommerceEnvNames = @('MMORPG_COMMERCE_GATEWAY_SECRET', 'MMORPG_COMMERCE_PAYMENT_SECRET', 'MMORPG_COMMERCE_DELIVERY_SECRET', 'MMORPG_COMMERCE_ADMIN_SECRET')

# 在给定的四个环境变量取值下跑一段脚本(没列出的变量清掉),跑完还原。
function Invoke-WithCommerceEnv {
    param([Parameter(Mandatory = $true)][hashtable]$Values, [Parameter(Mandatory = $true)][scriptblock]$Action)
    $saved = @{}
    foreach ($name in $CommerceEnvNames) { $saved[$name] = [System.Environment]::GetEnvironmentVariable($name) }
    try {
        foreach ($name in $CommerceEnvNames) {
            [System.Environment]::SetEnvironmentVariable($name, $(if ($Values.ContainsKey($name)) { [string]$Values[$name] } else { $null }))
        }
        return (& $Action)
    }
    finally {
        foreach ($name in $CommerceEnvNames) { [System.Environment]::SetEnvironmentVariable($name, $saved[$name]) }
    }
}

Test-Case '纯函数 Resolve-CommerceSecrets:dev 未注入用公开回落值;staging / prod 必须注入且不收回落值;形状不对、两把相同一律拒绝且不回显值' {
    $good = @{
        MMORPG_COMMERCE_GATEWAY_SECRET  = 'unit-commerce-gateway-secret-000000000000001'
        MMORPG_COMMERCE_PAYMENT_SECRET  = 'unit-commerce-payment-secret-000000000000002'
        MMORPG_COMMERCE_DELIVERY_SECRET = 'unit-commerce-delivery-secret-00000000000003'
        MMORPG_COMMERCE_ADMIN_SECRET    = 'unit-commerce-admin-secret-00000000000000004'
    }
    $fallbacks = @{}
    foreach ($role in @('gateway', 'payment', 'delivery', 'admin')) { $fallbacks['MMORPG_COMMERCE_' + $role.ToUpperInvariant() + '_SECRET'] = "change-me-in-production-commerce-$role-hmac-key" }

    $dev = Invoke-WithCommerceEnv -Values @{} -Action { Resolve-CommerceSecrets -ReleaseProfile 'dev' -IncludeAdmin $true }
    Assert-Equal -Expected 'gateway,payment,delivery,admin' -Actual (@($dev.Keys) -join ',') -Because '部署商城时四个角色各一把'
    foreach ($role in @($dev.Keys)) {
        Assert-Equal -Expected "change-me-in-production-commerce-$role-hmac-key" -Actual ([string]$dev[$role]) -Because "dev 未注入时 $role 用公开回落值(只给 dev)"
    }
    $withoutAdmin = Invoke-WithCommerceEnv -Values @{} -Action { Resolve-CommerceSecrets -ReleaseProfile 'dev' -IncludeAdmin $false }
    Assert-Equal -Expected 'gateway,payment,delivery' -Actual (@($withoutAdmin.Keys) -join ',') -Because '不部署商城就不解析 admin 那把(只有商城读它)'

    foreach ($profileName in @('dev', 'staging', 'prod')) {
        $got = Invoke-WithCommerceEnv -Values $good -Action { Resolve-CommerceSecrets -ReleaseProfile $profileName -IncludeAdmin $true }
        foreach ($role in @('gateway', 'payment', 'delivery', 'admin')) {
            Assert-True -Condition ([string]$got[$role] -ceq $good['MMORPG_COMMERCE_' + $role.ToUpperInvariant() + '_SECRET']) -Because "$profileName 档合法密钥原样返回($role)"
        }
    }

    # 在四把合格值的基础上换掉其中一把(哈希表相加遇到重复键会抛错,所以逐项拷贝)。
    $override = {
        param([string]$Name, [string]$Value)
        $copy = @{}
        foreach ($key in $good.Keys) { $copy[$key] = $good[$key] }
        $copy[$Name] = $Value
        return $copy
    }
    $short = 'short-commerce-secret'
    $spaced = 'unit commerce delivery secret 0000000000003'
    $placeholder = 'unit-commerce-admin-secret-${X}-000000000004'
    $cases = @(
        @{ Why = 'staging 未注入'; Profile = 'staging'; Values = @{}; Pattern = 'MMORPG_COMMERCE_GATEWAY_SECRET'; NoEcho = '' }
        @{ Why = 'prod 未注入'; Profile = 'prod'; Values = @{}; Pattern = 'MMORPG_COMMERCE_GATEWAY_SECRET'; NoEcho = '' }
        @{ Why = 'prod 注入的是公开回落值'; Profile = 'prod'; Values = $fallbacks; Pattern = 'MMORPG_COMMERCE_GATEWAY_SECRET'; NoEcho = '' }
        @{ Why = 'dev 短于 32 字节'; Profile = 'dev'; Values = (& $override 'MMORPG_COMMERCE_PAYMENT_SECRET' $short); Pattern = 'MMORPG_COMMERCE_PAYMENT_SECRET must be at least 32 bytes'; NoEcho = $short }
        @{ Why = 'dev 含空白'; Profile = 'dev'; Values = (& $override 'MMORPG_COMMERCE_DELIVERY_SECRET' $spaced); Pattern = 'MMORPG_COMMERCE_DELIVERY_SECRET must be at least 32 bytes and must not contain whitespace'; NoEcho = $spaced }
        @{ Why = 'dev 含 Spring 占位'; Profile = 'dev'; Values = (& $override 'MMORPG_COMMERCE_ADMIN_SECRET' $placeholder); Pattern = 'MMORPG_COMMERCE_ADMIN_SECRET must be at least 32 bytes'; NoEcho = $placeholder }
        @{ Why = '两个角色共用一把'; Profile = 'dev'; Values = (& $override 'MMORPG_COMMERCE_PAYMENT_SECRET' $good.MMORPG_COMMERCE_GATEWAY_SECRET); Pattern = 'MMORPG_COMMERCE_PAYMENT_SECRET must differ from MMORPG_COMMERCE_GATEWAY_SECRET'; NoEcho = $good.MMORPG_COMMERCE_GATEWAY_SECRET }
    )
    foreach ($case in $cases) {
        $message = Invoke-WithCommerceEnv -Values $case.Values -Action { Get-ThrownMessage { Resolve-CommerceSecrets -ReleaseProfile $case.Profile -IncludeAdmin $true } }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "$($case.Why):必须拒绝并点名环境变量"
        if ($case.NoEcho -ne '') {
            Assert-True -Condition (-not ([string]$message).Contains([string]$case.NoEcho)) -Because "$($case.Why):报错不回显密钥"
        }
    }
}

# New-PaymentSectionYaml 的调用包装:Mall 段的两项镜像值与各段 *Env 键的缺省值给固定的测试值。
$UnitEnvDefaults = [ordered]@{
    'DevPay.SecretEnv'        = 'PAYMENT_DEVPAY_SECRET'
    'WechatPay.PrivateKeyEnv' = 'PAYMENT_WECHATPAY_PRIVATE_KEY'
    'WechatPay.APIv3KeyEnv'   = 'PAYMENT_WECHATPAY_APIV3_KEY'
}
function Invoke-PaymentSection {
    param([Parameter(Mandatory = $true)][string]$Block)
    return New-PaymentSectionYaml -BaseBlock $Block -MallBaseUrl 'http://mall.unit-infra:18120' -MallPaymentSubject 'unit-subject' -MallTimeoutMs '1234' `
        -DefaultEnvNames $UnitEnvDefaults -SourceLabel 'unit block'
}

Test-Case '纯函数 New-PaymentSectionYaml:回调监听口与 Mall 子段由脚本写死,其余原样;开着的渠道与它们要的密钥变量按段算出' {
    $block = @(
        'Payment:'
        '  NotifyListenAddr: 127.0.0.1:1'
        '  NotifyBaseURL: "https://pay.unit.invalid"'
        '  Mall:'
        '    BaseURL: http://wrong.invalid:1'
        '    PaymentSecretEnv: WRONG_ENV'
        ''
        '  Platforms:'
        '    - Platform: desktop'
        '      Channels: [wechat_pay]'
        '  DevPay:'
        '    Enabled: false'
        '    SecretEnv: UNIT_DEVPAY_SECRET'
        '  WechatPay:'
        '    Enabled: true'
        '    AppID: "wx-unit"'
        '    PrivateKeyEnv: UNIT_WECHATPAY_PRIVATE_KEY'
        '  Alipay:'
        '    Enabled: no'
        '  Apple:'
        '    BundleID: "unit.bundle"'
    ) -join "`n"
    $section = Invoke-PaymentSection -Block $block
    $text = [string]$section.Text
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^  NotifyListenAddr:').Count) -Because '回调监听口只留脚本写的那一行'
    Assert-Match -Text $text -Pattern '(?m)^  NotifyListenAddr: 0\.0\.0\.0:18130$' -Because 'Service payment-notify 指向容器的 18130,监听地址必须是 0.0.0.0'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^  Mall:').Count) -Because 'Mall 子段只留脚本写的那一份'
    foreach ($line in @('    BaseURL: http://mall.unit-infra:18120', '    PaymentSubject: unit-subject', '    PaymentSecretEnv: MMORPG_COMMERCE_PAYMENT_SECRET', '    TimeoutMs: 1234')) {
        Assert-True -Condition ($text.Contains($line)) -Because "Mall 子段要有:$line"
    }
    Assert-NotMatch -Text $text -Pattern 'wrong\.invalid|WRONG_ENV|127\.0\.0\.1' -Because '原文里的回调监听口与 Mall 子段不管写了什么都不用'
    foreach ($kept in @('  NotifyBaseURL: "https://pay.unit.invalid"', '      Channels: [wechat_pay]', '    AppID: "wx-unit"', '    BundleID: "unit.bundle"')) {
        Assert-True -Condition ($text.Contains($kept)) -Because "其余内容原样保留:$kept"
    }
    Assert-Equal -Expected 'wechat_pay' -Actual (@($section.EnabledChannels) -join ',') -Because 'Enabled 是 false / no 或没写都算关;渠道名与 go/payment 的常量一致'
    Assert-Equal -Expected 'PAYMENT_WECHATPAY_APIV3_KEY,UNIT_WECHATPAY_PRIVATE_KEY' -Actual (@($section.SecretEnvNames) -join ',') -Because '只算开着的渠道;原文写了变量名用原文的,没写取服务 yaml 的缺省值'

    $allOn = Invoke-PaymentSection -Block ("Payment:`n  DevPay:`n    Enabled: TRUE`n  WechatPay:`n    Enabled: yes`n    APIv3KeyEnv: `"`"`n")
    Assert-Equal -Expected 'devpay,wechat_pay' -Actual (@($allOn.EnabledChannels) -join ',') -Because '开关判定偏向开:不是 false / no / off / n 就算开着(漏判成关,商城会拒收这个渠道已经收到的钱)'
    Assert-Equal -Expected 'PAYMENT_DEVPAY_SECRET,PAYMENT_WECHATPAY_PRIVATE_KEY' -Actual (@($allOn.SecretEnvNames) -join ',') -Because '变量名写成空串表示不用这一把'
}

Test-Case '纯函数 New-PaymentSectionYaml:形状不对一律拒绝,报错不回显配置内容' {
    $leak = 'sk_live_unit_secret_value'
    $cases = @(
        @{ Why = '制表符缩进'; Block = "Payment:`n`tDevPay:`n`t`tEnabled: true"; Pattern = 'must not contain tab characters' }
        @{ Why = '多一个顶层键'; Block = "Payment:`n  DevPay:`n    Enabled: false`nOther: $leak"; Pattern = 'exactly one top-level key' }
        @{ Why = '顶层键不是 Payment'; Block = "Pay:`n  Secret: $leak"; Pattern = 'exactly one top-level key' }
        @{ Why = '子键缩进不是两格'; Block = "Payment:`n    DevPay:`n        Enabled: true"; Pattern = 'exactly two spaces' }
        @{ Why = '渠道段写成流式'; Block = "Payment:`n  DevPay: {Enabled: true, SecretEnv: $leak}"; Pattern = 'block style' }
        @{ Why = '把密钥值写进了变量名的位置'; Block = "Payment:`n  DevPay:`n    Enabled: true`n    SecretEnv: $leak"; Pattern = 'Payment\.DevPay\.SecretEnv must name an environment variable' }
    )
    foreach ($case in $cases) {
        $message = Get-ThrownMessage { Invoke-PaymentSection -Block $case.Block }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "$($case.Why):必须拒绝并说明原因"
        Assert-Match -Text $message -Pattern 'Nothing was written' -Because "$($case.Why):写明什么都没写"
        Assert-True -Condition (-not ([string]$message).Contains($leak)) -Because "$($case.Why):报错不回显配置内容"
    }
}

Test-Case '纯函数 Get-PaymentDeployPlan:dev 用服务 yaml 的段(只开 devpay,密钥走目录);staging / prod 不给文件时一个渠道都不开;给了文件用文件;非 dev 档开 devpay 拒绝' {
    $InfraNamespace = 'unit-infra'
    $PaymentConfigPath = ''
    $ReleaseProfile = 'dev'
    $script:PaymentDeployPlan = $null
    $dev = Get-PaymentDeployPlan
    Assert-Equal -Expected 'devpay' -Actual (@($dev.EnabledChannels) -join ',') -Because 'dev 用服务 yaml 的段,只开 devpay'
    Assert-True -Condition ([bool]$dev.BuiltinDevPay) -Because 'devpay 的密钥走工作负载密钥目录(payment-devpay),dev 有回落值'
    Assert-Equal -Expected 0 -Actual @($dev.SecretEnvNames).Count -Because '不用运维预置任何渠道密钥'
    Assert-Match -Text ([string]$dev.Text) -Pattern '(?m)^    SecretEnv: MMORPG_PAYMENT_DEVPAY_SECRET\s*$' -Because 'devpay 的密钥变量改指目录里的那一把'
    Assert-Match -Text ([string]$dev.Text) -Pattern '(?m)^    BaseURL: http://mall\.unit-infra:\d+\s*$' -Because '商城地址是集群内的 Service'

    foreach ($profileName in @('staging', 'prod')) {
        $ReleaseProfile = $profileName
        $script:PaymentDeployPlan = $null
        $plan = Get-PaymentDeployPlan
        Assert-Equal -Expected '' -Actual (@($plan.EnabledChannels) -join ',') -Because "$profileName 不给 -PaymentConfigPath 时一个渠道都不开(合规闸不在部署脚本里放宽)"
        Assert-True -Condition (-not [bool]$plan.BuiltinDevPay) -Because "$profileName 不开 devpay"
        Assert-Equal -Expected 0 -Actual @($plan.SecretEnvNames).Count -Because "$profileName 不开渠道就不要任何渠道密钥"
        Assert-NotMatch -Text ([string]$plan.Text) -Pattern '(?m)^\s+Enabled:\s*true\s*$' -Because "$profileName 的缺省段里没有开着的渠道"
        Assert-NotMatch -Text ([string]$plan.Text) -Pattern '(?m)^\s+Channels:\s*\[\s*[a-z]' -Because "$profileName 的缺省段里各平台都不挂渠道"
    }

    $file = Join-Path $TempRoot 'unit-payment.yaml'
    [IO.File]::WriteAllText($file, "Payment:`n  DevPay:`n    Enabled: true`n    SecretEnv: UNIT_DEVPAY_SECRET`n", [Text.UTF8Encoding]::new($false))
    $PaymentConfigPath = $file
    $ReleaseProfile = 'dev'
    $script:PaymentDeployPlan = $null
    $fromFile = Get-PaymentDeployPlan
    Assert-Equal -Expected '-PaymentConfigPath' -Actual ([string]$fromFile.Source) -Because '给了文件就用文件'
    Assert-Equal -Expected 'devpay' -Actual (@($fromFile.EnabledChannels) -join ',') -Because '文件里开着 devpay'
    Assert-True -Condition (-not [bool]$fromFile.BuiltinDevPay) -Because '运维给了文件,渠道密钥全部来自运维预置的 Secret,目录里的 payment-devpay 不注入'
    Assert-Equal -Expected 'UNIT_DEVPAY_SECRET' -Actual (@($fromFile.SecretEnvNames) -join ',') -Because '要预置的键就是文件里写的变量名'

    $ReleaseProfile = 'prod'
    $script:PaymentDeployPlan = $null
    Assert-Match -Text (Get-ThrownMessage { Get-PaymentDeployPlan }) -Pattern 'enables Payment\.DevPay, which is only allowed with -ReleaseProfile dev' -Because '非 dev 档开 devpay 在写之前拒绝:payment 与商城的生产模式都不收它'
    $PaymentConfigPath = Join-Path $TempRoot 'missing-payment.yaml'
    $script:PaymentDeployPlan = $null
    Assert-Match -Text (Get-ThrownMessage { Get-PaymentDeployPlan }) -Pattern 'is not a file' -Because '文件不存在要拒绝,不回落到缺省段'
    $script:PaymentDeployPlan = $null
}

Test-Case '纯函数 Resolve-MallSqlMode:tidb 档核对 TiDB、运行模式跟发布档位;mysql 档只有 dev 能用,staging / prod 拒绝并说明做法' {
    $InfraNamespace = 'unit-infra'
    $GlobalAuthorityTiDBHost = $AuthorityHost
    $FriendTiDBHost = ''; $TradeTiDBHost = ''; $MatchTiDBHost = ''
    foreach ($case in @(
            @{ Profile = 'dev'; Backend = 'tidb'; Expected = 'tidb/dev/true' }
            @{ Profile = 'staging'; Backend = 'tidb'; Expected = 'tidb/prod/true' }
            @{ Profile = 'prod'; Backend = 'tidb'; Expected = 'tidb/prod/true' }
            @{ Profile = 'dev'; Backend = 'mysql'; Expected = 'mysql/dev/false' }
        )) {
        $ReleaseProfile = $case.Profile
        $DatabaseBackend = $case.Backend
        $mode = Resolve-MallSqlMode
        Assert-Equal -Expected $case.Expected -Actual ('{0}/{1}/{2}' -f $mode.Backend, $mode.RunMode, $mode.RequireTidb) -Because "$($case.Profile) + $($case.Backend):后端 / 运行模式 / 是否核对 TiDB"
        Assert-Equal -Expected $AuthorityHost -Actual ([string]$mode.Host) -Because 'mmorpg_mall 与其余逻辑库在同一个全球权威 SQL 入口上'
    }
    foreach ($profileName in @('staging', 'prod')) {
        $ReleaseProfile = $profileName
        $DatabaseBackend = 'mysql'
        $message = Get-ThrownMessage { Resolve-MallSqlMode }
        Assert-Match -Text $message -Pattern 'has no mysql tier' -Because "$profileName 的 mysql 档:商城的生产模式只认 TiDB,不拿 dev 模式跑生产商城"
        Assert-Match -Text $message -Pattern 'without -JavaSvcRegistry' -Because '报错给出做法:这种部署不带商城'
    }
}

# ──────────────────────────────────────────────
# 3. DryRun:dev infra-up,带全局 Go 服务与商城,开 gRPC 口准入
# ──────────────────────────────────────────────
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-KafkaBrokers', '3', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
foreach ($name in $SecretSpecs.Keys) { $InfraEnv[$SecretSpecs[$name].Env] = $SecretSpecs[$name].Value }

$devRun = Invoke-CommerceDryRun -Arguments ($InfraArgs + @('-NetworkPolicies', 'on')) -Env $InfraEnv
$devOut = [string]$devRun.Output -replace "`r`n", "`n"

Test-Case 'DryRun(dev infra-up):成功;入口打出支付配置的来源、开着的渠道、回调入口类型,与商城的库档位' {
    Assert-Equal -Expected 0 -Actual $devRun.ExitCode -Because "带全局 Go 服务与商城的 infra-up DryRun 必须成功。输出: $($devRun.Output)"
    Assert-Match -Text $devOut -Pattern '(?m)^Payment: config=go/payment/etc/payment\.yaml \(dev\) channels=devpay notify_service_type=NodePort channel_secret_keys=0\s*$' -Because 'dev 不给 -PaymentConfigPath:用服务 yaml 的段,只开 devpay;custom 档回调入口取 -GateServiceType 的缺省值 NodePort'
    Assert-Match -Text $devOut -Pattern '(?m)^Mall: database=mmorpg_mall backend=tidb run_mode=dev require_tidb=true allowed_payment_channels=devpay\s*$' -Because '商城的库档位与允许的支付渠道(= payment 开着的渠道)'
    Assert-NotMatch -Text $devOut -Pattern 'would check that Secret payment-channel-secrets' -Because '没有渠道要运维预置的密钥时不核对那个 Secret'
}

Test-Case 'DryRun:密钥只进 Secret —— 整份输出不含任何一把的值;每把在 infra namespace 恰好写一次,且先于引用它的工作负载' {
    foreach ($name in $SecretSpecs.Keys) {
        Assert-True -Condition (-not $devOut.Contains($SecretSpecs[$name].Value)) -Because "整份 DryRun 输出(配置模板、清单、Secret 意图行)里不得出现 $($SecretSpecs[$name].Env) 的值"
        $pattern = '(?m)^\[dry-run\] kubectl apply -n ' + [regex]::Escape($Infra) + ' -f -   # Secret ' + [regex]::Escape($name) + ' \(key secret; value hidden\)'
        $lines = [regex]::Matches($devOut, $pattern)
        Assert-Equal -Expected 1 -Actual $lines.Count -Because "同一次运行里 Secret $name 只写一次(几个消费者共用同一份)"
        $firstRef = $devOut.IndexOf("- name: $($SecretSpecs[$name].Env)")
        Assert-True -Condition ($lines.Count -eq 1 -and $firstRef -gt $lines[0].Index) -Because "Secret $name 必须先于引用它的工作负载落地(secretKeyRef 不是 optional)"
    }
}

Test-Case 'payment 的配置模板:监听与发现改成集群内的;Payment 段两项由脚本写死;dev 只开 devpay 且密钥变量指向目录;没有本机地址' {
    $configMap = Select-CommerceDocument -Output $devOut -Kind 'ConfigMap' -Name 'go-svc-payment-config'
    $flat = ConvertTo-FlatManifest -Block $configMap
    $expected = [ordered]@{
        'ListenOn'                           = "0.0.0.0:$PaymentGrpcPort"
        'AdvertiseHost'                      = ''
        'Mode'                               = 'dev'
        'MetricsListenAddr'                  = "0.0.0.0:$PaymentMetricsPort"
        'Etcd.Hosts'                         = "[etcd.${Infra}:2379]"
        'Etcd.Key'                           = ''
        'Backend'                            = 'tidb'
        'DSNEnv'                             = 'MMORPG_PAYMENT_SQL_DSN'
        'MaxOpenConns'                       = [string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'MaxOpenConns')
        'AutoMigrate'                        = 'false'
        'Payment.NotifyListenAddr'           = "0.0.0.0:$PaymentNotifyPort"
        'Payment.Mall.BaseURL'               = $MallUrl
        'Payment.Mall.PaymentSecretEnv'      = 'MMORPG_COMMERCE_PAYMENT_SECRET'
        'Payment.Mall.PaymentSubject'        = [string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.Mall.PaymentSubject')
        'Payment.Mall.TimeoutMs'             = [string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.Mall.TimeoutMs')
        'Payment.DevPay.Enabled'             = 'true'
        'Payment.DevPay.SecretEnv'           = 'MMORPG_PAYMENT_DEVPAY_SECRET'
        'DbPlacement.DefaultCluster'         = 'main'
        'DbPlacement.SqlClusters.main.Hosts' = $AuthorityHost
    }
    foreach ($key in $expected.Keys) {
        Assert-Equal -Expected $expected[$key] -Actual (Get-FlatValue -Flat $flat -KeyPath "data.payment.yaml.$key") -Because "payment 配置模板的 $key"
    }
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($configMap, '(?m)^    Payment:').Count) -Because 'Payment 段整段替换,不留两份'
    # 只看非注释行:服务 yaml 的注释会提到本机联调。
    $effective = @(($configMap -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-NotMatch -Text $effective -Pattern '127\.0\.0\.1|localhost' -Because '配置模板里不能留本机地址:服务 yaml 新增一个要连的地址时,生成器要跟着改成集群内的'
}

Test-Case 'payment 的工作负载:写者 StatefulSet 两副本 + PDB;三个端口与服务 yaml 同源;探针走指标口;回调入口类型已填;迁移 Job 用 schema 身份' {
    $sts = Select-CommerceDocument -Output $devOut -Kind 'StatefulSet' -Name 'payment'
    $svc = Select-CommerceDocument -Output $devOut -Kind 'Service' -Name 'payment'
    $notify = Select-CommerceDocument -Output $devOut -Kind 'Service' -Name 'payment-notify'
    $pdb = Select-CommerceDocument -Output $devOut -Kind 'PodDisruptionBudget' -Name 'payment-pdb'
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 全服充值是单点'
    Assert-Match -Text $sts -Pattern '(?m)^  serviceName: payment\s*$' -Because 'StatefulSet 要 serviceName,用同名 Service'
    Assert-Match -Text $sts -Pattern 'podAntiAffinity' -Because '两个副本不该落在同一个节点上'
    Assert-Match -Text $pdb -Pattern '(?m)^  minAvailable: 1\s*$' -Because 'PDB 写在主清单里,至少留一个副本'
    Assert-Match -Text $sts -Pattern 'image: registry\.invalid/test/mmorpg-payment:' -Because '镜像名 mmorpg-payment(与 go_svc_image.ps1 一致)'
    Assert-Match -Text $sts -Pattern ('\{name: grpc,\s*containerPort: ' + $PaymentGrpcPort + '\}') -Because 'gRPC 容器端口 == 服务 yaml 的 ListenOn'
    Assert-Match -Text $sts -Pattern ('\{name: metrics,\s*containerPort: ' + $PaymentMetricsPort + '\}') -Because 'metrics 容器端口 == 服务 yaml 的 MetricsListenAddr'
    Assert-Match -Text $sts -Pattern ('\{name: notify,\s*containerPort: ' + $PaymentNotifyPort + '\}') -Because '回调容器端口 == 服务 yaml 的 Payment.NotifyListenAddr'
    Assert-Match -Text $sts -Pattern 'readinessProbe:\s+httpGet: \{path: /healthz, port: metrics\}' -Because 'readiness 检查服务的健康入口'
    Assert-Match -Text $sts -Pattern 'livenessProbe:\s+httpGet: \{path: /healthz, port: metrics\}' -Because 'liveness 检查服务的健康入口'
    Assert-Match -Text $sts -Pattern 'name: POD_IP\s+valueFrom:\s+fieldRef: \{fieldPath: status\.podIP\}' -Because 'ListenOn 是 0.0.0.0,注册进 etcd 的必须是 Pod IP'
    Assert-Match -Text $sts -Pattern '"/app/etc/private/payment\.yaml"' -Because '常驻进程读本次启动选中的配置(运维预置的 runtimeConfigSecret),不挂配置模板'
    Assert-NotMatch -Text $sts -Pattern 'go-svc-payment-config' -Because '写者不挂部署脚本渲染的 ConfigMap'
    Assert-Match -Text $sts -Pattern '- secretRef: \{name: payment-channel-secrets, optional: true\}' -Because '渠道密钥从运维预置的 Secret 整体注入'
    Assert-Match -Text $sts -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "tidb"' -Because 'registrar 按 -DatabaseBackend 核真实引擎'
    Assert-Match -Text $svc -Pattern ('\{name: grpc,\s*port: ' + $PaymentGrpcPort + ',\s*targetPort: ' + $PaymentGrpcPort + '\}') -Because 'Service payment 只在集群内,端口 == 服务 yaml 的 ListenOn'
    Assert-NotMatch -Text $svc -Pattern '(?m)^  type:' -Because 'gRPC 的 Service 留在集群内(缺省 ClusterIP)'
    Assert-Match -Text $notify -Pattern '(?m)^  type: NodePort\s*$' -Because '回调入口的类型取 gate 对外 Service 的口径(custom 档 = -GateServiceType 的缺省值)'
    Assert-Match -Text $notify -Pattern '\{name: notify,\s*port: 443,\s*targetPort: notify\}' -Because '对外 443,回源到容器的回调口'
    foreach ($doc in @($sts, $svc, $notify, $pdb)) {
        Assert-NotMatch -Text $doc -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because '占位必须全部替换'
    }
    $migration = Select-CommerceDocument -Output $devOut -Kind 'Job' -Name 'payment-migrate'
    Assert-Match -Text $migration -Pattern '"-migrate"' -Because '迁移 Job 必须实际渲染'
    Assert-Match -Text $migration -Pattern 'name: MMORPG_PAYMENT_SQL_DSN\s+valueFrom:\s+secretKeyRef: \{name: fixture-schema-permission, key: dsn, optional: false\}' -Because 'Job 的 DSN 和 MMORPG_SQL_SCHEMA_PRINCIPAL 出自同一个 schemaPermissionSecret'
    Assert-Match -Text $migration -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because '主体和账号来自同一个 Secret'
    Assert-Match -Text $migration -Pattern '- name: svc-config\s+secret:\s+secretName: fixture-schema-config' -Because '迁移 Job 的配置来自 schemaConfigSecret'
    Assert-NotMatch -Text $migration -Pattern 'payment-channel-secrets|- name: MMORPG_COMMERCE_|- name: MMORPG_PAYMENT_DEVPAY_SECRET' -Because '迁移只建表,不拿渠道密钥与商城密钥'
}

Test-Case 'payment 的密钥:商城 payment 角色那把与 dev 的 devpay 联调密钥经目录里的 Secret 注入并带指纹;别的角色的不注入;连接串不经 env' {
    $sts = Select-CommerceDocument -Output $devOut -Kind 'StatefulSet' -Name 'payment'
    foreach ($name in $SecretSpecs.Keys) {
        if ($SecretSpecs[$name].Consumers -contains 'payment') { Assert-SecretInjected -Doc $sts -SecretName $name -Label 'payment' }
        else { Assert-SecretAbsent -Doc $sts -SecretName $name -Label 'payment' }
    }
    Assert-NotMatch -Text $sts -Pattern '(?m)^[ ]*- name: MMORPG_PAYMENT_SQL_DSN[ ]*$' -Because '连接串来自本次启动选中的环境文件,不经 Secret 直接进主容器(否则绕过写者闸门)'
    Assert-True -Condition ($sts.Contains('MMORPG_PAYMENT_SQL_DSN=\"$(cat /app/etc/private/env/MMORPG_PAYMENT_SQL_DSN)\"')) -Because '连接串从 init 选中的环境文件读'
}

Test-Case 'trade 指向集群内商城:Mall.URL 是 infra 的 Service mall;gateway / delivery 两把只写变量名,值经目录里的 Secret 注入;清单不手写' {
    $flat = ConvertTo-FlatManifest -Block (Select-CommerceDocument -Output $devOut -Kind 'ConfigMap' -Name 'go-svc-trade-config')
    Assert-Equal -Expected $MallUrl -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.trade.yaml.Mall.URL') -Because 'trade 调集群内的商城,不留空(留空 = 商城功能关闭)'
    Assert-Equal -Expected 'MMORPG_COMMERCE_GATEWAY_SECRET' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.trade.yaml.Mall.GatewaySecretEnv') -Because 'gateway 角色的密钥变量名与目录一致'
    Assert-Equal -Expected 'MMORPG_COMMERCE_DELIVERY_SECRET' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.trade.yaml.Mall.DeliverySecretEnv') -Because 'delivery 角色的密钥变量名与目录一致'
    $sts = Select-CommerceDocument -Output $devOut -Kind 'StatefulSet' -Name 'trade'
    foreach ($name in $SecretSpecs.Keys) {
        if ($SecretSpecs[$name].Consumers -contains 'trade') { Assert-SecretInjected -Doc $sts -SecretName $name -Label 'trade' }
        else { Assert-SecretAbsent -Doc $sts -SecretName $name -Label 'trade' }
    }
    $manifest = @((Read-RepoText 'deploy/k8s/manifests/go-svc/trade.yaml') -split "`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-NotMatch -Text $manifest -Pattern 'COMMERCE_(GATEWAY|DELIVERY)_SECRET|commerce-(gateway|delivery)' -Because '这两把只有目录一条注入路径,清单里不再手写对 domain-service-secrets 的引用'
}

Test-Case '商城的配置模板:监听 0.0.0.0;连全球权威入口上的 mmorpg_mall;四把密钥只写对环境变量的引用;允许的支付渠道 = payment 开着的渠道' {
    $configMap = Select-CommerceDocument -Output $devOut -Kind 'ConfigMap' -Name 'java-svc-mall-config'
    $flat = ConvertTo-FlatManifest -Block $configMap
    $prefix = 'data.application.yaml.'
    Assert-Equal -Expected '0.0.0.0' -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'server.address')) -Because 'jar 内缺省只听 127.0.0.1,集群里别的 Pod 连不上'
    Assert-Equal -Expected $MallPort -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'server.port')) -Because '端口与目录、清单一致'
    $url = Get-FlatValue -Flat $flat -KeyPath ($prefix + 'spring.datasource.url')
    Assert-True -Condition ($url.StartsWith("jdbc:mysql://$AuthorityHost/mmorpg_mall?")) -Because "商城连的是全球权威入口上的 mmorpg_mall,实际 $url"
    Assert-Equal -Expected '' -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'spring.datasource.password')) -Because '数据源口令不进模板:商城是写者,口令在运维预置的 runtimeConfigSecret 里'
    Assert-Equal -Expected 'dev/true' -Actual ('{0}/{1}' -f (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'commerce.run-mode')), (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'commerce.require-tidb'))) -Because 'dev 档的 tidb 入口:运行模式 dev(devpay 只许 dev),照常核对 TiDB'
    foreach ($role in @('gateway', 'payment', 'delivery', 'admin')) {
        Assert-Equal -Expected ('${MMORPG_COMMERCE_' + $role.ToUpperInvariant() + '_SECRET}') -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + "commerce.$role-secret")) -Because "$role 角色的密钥只写对容器环境变量的引用"
    }
    Assert-Equal -Expected ([string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.Mall.PaymentSubject')) -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'commerce.payment-subject')) -Because 'payment 角色的调用方名两边取同一个键'
    Assert-Equal -Expected 'devpay' -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'commerce.allowed-payment-channels')) -Because '允许的支付渠道就是 payment 模板里开着的渠道,不各写一份'
    $effective = @(($configMap -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-NotMatch -Text $effective -Pattern '127\.0\.0\.1|localhost' -Because '模板里不能留本机地址(span 导出要么发节点采集器,要么关掉)'
}

Test-Case '商城的工作负载:闸门表 Job 用 schema 身份、先于进程;StatefulSet 走与 gateway 相同的写者注入;四把密钥都注入;不挂配置模板' {
    $job = Select-CommerceDocument -Output $devOut -Kind 'Job' -Name 'mall-schema'
    Assert-Match -Text $job -Pattern 'image: fixture\.invalid/registrar@sha256:a{64}' -Because '闸门表 Job 跑 boot 目录里 mall 条目的 registrar 镜像(固定摘要)'
    Assert-Match -Text $job -Pattern '"-command", "prepare-schema", "-schema", "mmorpg_mall"' -Because '建的是 mmorpg_mall 的闸门表'
    Assert-Match -Text $job -Pattern 'name: MMORPG_SQL_SCHEMA_DSN\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: dsn, optional: false\}\}' -Because 'DSN 取 schemaPermissionSecret 的 dsn'
    Assert-Match -Text $job -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because '主体和账号来自同一个 Secret'
    Assert-Match -Text $job -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "tidb"' -Because '后端档位跟 -DatabaseBackend'
    Assert-Match -Text $job -Pattern "name: MMORPG_DB_PLACEMENT\s+value: '\{" -Because '按库映射连库'
    Assert-NotMatch -Text $job -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because '占位必须全部替换'

    $sts = Select-CommerceDocument -Output $devOut -Kind 'StatefulSet' -Name 'mall'
    $svc = Select-CommerceDocument -Output $devOut -Kind 'Service' -Name 'mall'
    $null = Select-CommerceDocument -Output $devOut -Kind 'PodDisruptionBudget' -Name 'mall-pdb'
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 2\s*$' -Because '两副本'
    Assert-Match -Text $sts -Pattern 'image: registry\.invalid/test/mmorpg-mall:' -Because '镜像名 mmorpg-mall(与 java_svc_image.ps1 一致)'
    Assert-WriterPrivatePaths -Block $sts -Service 'mall' -ConfigFile 'application-k8s.yaml'
    Assert-Match -Text $sts -Pattern 'name: SPRING_CONFIG_ADDITIONAL_LOCATION\s+value: file:/app/config/private/' -Because 'Java 的选中配置在子目录,显式告诉 Spring'
    Assert-Match -Text $sts -Pattern 'name: SPRING_PROFILES_ACTIVE\s+value: k8s' -Because '选中配置的文件名是 application-k8s.yaml'
    Assert-Match -Text $sts -Pattern '- name: svc-config\s+emptyDir: \{medium: Memory, sizeLimit: 20Mi\}' -Because '常驻进程读 init 选中的配置,不挂配置模板'
    Assert-NotMatch -Text $sts -Pattern 'java-svc-mall-config' -Because '写者不挂部署脚本渲染的 ConfigMap'
    Assert-Match -Text $sts -Pattern '(?m)^        tier: java-svc\s*$' -Because 'Pod 模板带 tier: java-svc(容器日志按它采集)'
    Assert-Match -Text $sts -Pattern ('startupProbe:\s+tcpSocket:\s+port: ' + $MallPort) -Because '商城没有可供探针访问的 HTTP 路径,用 TCP 探针'
    foreach ($name in $SecretSpecs.Keys) {
        if ($SecretSpecs[$name].Consumers -contains 'mall') { Assert-SecretInjected -Doc $sts -SecretName $name -Label 'mall' }
        else { Assert-SecretAbsent -Doc $sts -SecretName $name -Label 'mall' }
    }
    Assert-Match -Text $svc -Pattern ('port: ' + $MallPort + '\s+targetPort: ' + $MallPort) -Because 'Service mall 的端口'

    # 先后:配置模板与闸门表 Job 在第一步,进程在全局 Go 服务之后。
    $prepared = Get-OutputIndex -Text $devOut -Needle '[prepared] mall ->'
    $schemaApplied = Get-OutputIndex -Text $devOut -Needle '[applied] mall-schema ->'
    $goStart = Get-OutputIndex -Text $devOut -Needle 'Starting global Go services that depend on data-service'
    $tradeApplied = Get-OutputIndex -Text $devOut -Needle '[applied] trade ->'
    $mallApplied = Get-OutputIndex -Text $devOut -Needle '[applied] mall ->'
    Assert-True -Condition ($schemaApplied -lt $prepared -and $prepared -lt $goStart) -Because '商城的配置模板与闸门表 Job 在第一步(基础设施阶段)'
    Assert-True -Condition ($mallApplied -gt $tradeApplied) -Because '商城的进程排在全局 Go 服务之后:它出问题不挡住别的全局服务'
    Assert-True -Condition ((Get-OutputIndex -Text $devOut -Needle "[applied] sql-writer-slot-retirer-mall (namespace $Infra)") -gt $mallApplied) -Because '缩容收尾 CronJob 紧跟在商城的 StatefulSet 之后'
    Assert-True -Condition ((Get-OutputIndex -Text $devOut -Needle "[applied] sql-writer-slot-retirer-payment (namespace $Infra)") -gt (Get-OutputIndex -Text $devOut -Needle '[applied] payment-migrate ->')) -Because 'payment 同样带缩容收尾 CronJob'
}

Test-Case '准入策略(-NetworkPolicies on):payment 的 gRPC 口只放行路由服,指标口与回调口对所有来源开放;商城的内部 HTTP 口只放行 trade 与 payment' {
    $paymentPolicy = Get-AllowListShape -Block (Select-CommerceDocument -Output $devOut -Kind 'NetworkPolicy' -Name 'payment-grpc-allowlist')
    Assert-Equal -Expected 'payment' -Actual ($paymentPolicy.TargetApps -join ',') -Because 'payment 不提供金丝雀,策略只选中它自己'
    Assert-Equal -Expected 'client-rpc-router,client-rpc-router-canary' -Actual ($paymentPolicy.AllowedApps -join ',') -Because '客户端请求只经 gate → 路由服到达(含路由服的金丝雀副本)'
    Assert-Equal -Expected ([int]$PaymentGrpcPort) -Actual $paymentPolicy.GuardedPort -Because '受保护的是 gRPC 口'
    $expectedOpen = @(@($PaymentMetricsPort, $PaymentNotifyPort) | ForEach-Object { [int]$_ } | Sort-Object) -join ','
    Assert-Equal -Expected $expectedOpen -Actual ($paymentPolicy.OpenPorts -join ',') -Because '指标口要给抓取、回调口要给集群外的负载均衡,对所有来源开放;渠道回调靠验签防伪'

    $mallPolicy = Get-AllowListShape -Block (Select-CommerceDocument -Output $devOut -Kind 'NetworkPolicy' -Name 'mall-grpc-allowlist')
    Assert-Equal -Expected 'mall' -Actual ($mallPolicy.TargetApps -join ',') -Because '策略只选中商城'
    Assert-Equal -Expected 'trade,payment,trade-canary' -Actual ($mallPolicy.AllowedApps -join ',') -Because '商城的调用方只有 trade(含它的金丝雀副本)与 payment'
    Assert-Equal -Expected ([int]$MallPort) -Actual $mallPolicy.GuardedPort -Because '受保护的是内部 HTTP 口'
    Assert-Equal -Expected '' -Actual ($mallPolicy.OpenPorts -join ',') -Because '商城没有别的端口'

    foreach ($pair in @(@{ Policy = 'payment-grpc-allowlist'; Workload = 'payment' }, @{ Policy = 'mall-grpc-allowlist'; Workload = 'mall' })) {
        $policyAt = Get-OutputIndex -Text $devOut -Needle "name: $($pair.Policy)"
        $workload = [regex]::Match($devOut, ('(?m)^kind: StatefulSet[ \t]*\nmetadata:[ \t]*\n  name: ' + $pair.Workload + '[ \t]*$'))
        Assert-True -Condition ($workload.Success -and $policyAt -lt $workload.Index) -Because "$($pair.Policy) 先于 $($pair.Workload) 的 StatefulSet 落地"
        Assert-NotMatch -Text $devOut -Pattern ('kubectl delete networkpolicy ' + $pair.Policy) -Because '开着时不删自己要用的策略'
    }
}

# ──────────────────────────────────────────────
# 4. DryRun:-PaymentConfigPath
# ──────────────────────────────────────────────
$PaymentConfigFile = Join-Path $TempRoot 'payment-channels.yaml'
$PaymentConfigLines = @(
    'Payment:'
    '  NotifyListenAddr: 127.0.0.1:1'
    '  NotifyBaseURL: "https://pay.contract.invalid"'
    '  Mall:'
    '    BaseURL: http://wrong.invalid:1'
    '    PaymentSubject: wrong-subject'
    '    PaymentSecretEnv: WRONG_ENV'
    '    TimeoutMs: 1'
    '  Platforms:'
    '    - Platform: wechat_minigame'
    '      Channels: [wechat_minigame]'
    '    - Platform: douyin_minigame'
    '      Channels: [douyin_minigame]'
    '  Products: []'
    '  Compliance:'
    '    MinorSpendingGate: none'
    '  DevPay:'
    '    Enabled: false'
    '  WechatMinigame:'
    '    Enabled: true'
    '    AppID: "wxcontract0001"'
    '    OfferID: "offer-0001"'
    '    Env: 1'
    '    AppSecretEnv: CONTRACT_WXMINI_APP_SECRET'
    '    AppKeyEnv: CONTRACT_WXMINI_APP_KEY'
    '  DouyinMinigame:'
    '    Enabled: true'
    '    AppID: "ttcontract0001"'
)
[IO.File]::WriteAllText($PaymentConfigFile, (($PaymentConfigLines -join "`n") + "`n"), [Text.UTF8Encoding]::new($false))
# 这两段没写的 *Env 键取服务 yaml 的缺省值。
$ChannelSecretKeys = @(
    'CONTRACT_WXMINI_APP_SECRET', 'CONTRACT_WXMINI_APP_KEY'
    [string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.WechatMinigame.PushTokenEnv')
    [string](Get-EtcValue -RelativePath $PaymentYamlPath -KeyPath 'Payment.DouyinMinigame.PushTokenEnv')
)

$fileRun = Invoke-CommerceDryRun -Arguments ($InfraArgs + @('-PaymentConfigPath', $PaymentConfigFile)) -Env $InfraEnv
$fileOut = [string]$fileRun.Output -replace "`r`n", "`n"

Test-Case 'DryRun(-PaymentConfigPath):运维的 Payment 段替换缺省段,两项地址仍由脚本写死;开着的渠道要的密钥只核对运维预置 Secret 的键;devpay 那把不再注入' {
    Assert-Equal -Expected 0 -Actual $fileRun.ExitCode -Because "给了 -PaymentConfigPath 的 infra-up DryRun 必须成功。输出: $($fileRun.Output)"
    Assert-Match -Text $fileOut -Pattern ('(?m)^Payment: config=-PaymentConfigPath channels=wechat_minigame,douyin_minigame notify_service_type=NodePort channel_secret_keys=' + $ChannelSecretKeys.Count + '\s*$') -Because '入口打出本次的来源、开着的渠道与要预置的键数'
    $check = [regex]::Match($fileOut, '(?m)^\[dry-run\] would check that Secret payment-channel-secrets in namespace ' + [regex]::Escape($Infra) + ' has the non-empty keys (?<keys>[^()]+) \(payment channel secrets; the values are not read\)\s*$')
    Assert-True -Condition $check.Success -Because 'DryRun 不连集群,打一行将要核对的 Secret 与键(真跑时缺 Secret、缺键在任何写操作之前拒绝)'
    $keys = @($check.Groups['keys'].Value -split ',\s*' | ForEach-Object { $_.Trim() })
    Assert-Equal -Expected (@($ChannelSecretKeys | Sort-Object) -join ',') -Actual (@($keys | Sort-Object) -join ',') -Because '要预置的键 = 开着的渠道的各 *Env 变量名(没写的取服务 yaml 的缺省值)'
    Assert-True -Condition ($check.Index -lt $fileOut.IndexOf('--- BEGIN MANIFEST ---')) -Because '核对排在任何清单之前'
    Assert-Match -Text $fileOut -Pattern '(?m)^Mall: database=mmorpg_mall backend=tidb run_mode=dev require_tidb=true allowed_payment_channels=wechat_minigame,douyin_minigame\s*$' -Because '商城允许的渠道跟着 payment 开着的渠道'

    $configMap = Select-CommerceDocument -Output $fileOut -Kind 'ConfigMap' -Name 'go-svc-payment-config'
    $flat = ConvertTo-FlatManifest -Block $configMap
    Assert-Equal -Expected "0.0.0.0:$PaymentNotifyPort" -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.NotifyListenAddr') -Because '回调监听口由脚本写死,不用文件里的'
    Assert-Equal -Expected $MallUrl -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.Mall.BaseURL') -Because '商城地址由脚本写死,不用文件里的'
    Assert-Equal -Expected 'MMORPG_COMMERCE_PAYMENT_SECRET' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.Mall.PaymentSecretEnv') -Because '商城密钥的变量名由脚本写死'
}

Test-Case 'DryRun(-PaymentConfigPath):文件里的渠道进了配置模板与商城的允许渠道;devpay 的目录密钥不注入、不建 Secret' {
    $configMap = Select-CommerceDocument -Output $fileOut -Kind 'ConfigMap' -Name 'go-svc-payment-config'
    $flat = ConvertTo-FlatManifest -Block $configMap
    Assert-Equal -Expected 'https://pay.contract.invalid' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.NotifyBaseURL') -Because '其余内容用文件里的'
    Assert-Equal -Expected 'true/CONTRACT_WXMINI_APP_SECRET' -Actual ('{0}/{1}' -f (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.WechatMinigame.Enabled'), (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.WechatMinigame.AppSecretEnv')) -Because '渠道段用文件里的'
    Assert-Equal -Expected 'false' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.payment.yaml.Payment.DevPay.Enabled') -Because '文件关了 devpay'
    Assert-NotMatch -Text $configMap -Pattern 'wrong\.invalid|WRONG_ENV|wrong-subject|127\.0\.0\.1:1\b' -Because '文件里的回调监听口与 Mall 子段不管写了什么都不用'
    $mallFlat = ConvertTo-FlatManifest -Block (Select-CommerceDocument -Output $fileOut -Kind 'ConfigMap' -Name 'java-svc-mall-config')
    Assert-Equal -Expected 'wechat_minigame,douyin_minigame' -Actual (Get-FlatValue -Flat $mallFlat -KeyPath 'data.application.yaml.commerce.allowed-payment-channels') -Because '商城允许的渠道 = payment 开着的渠道'

    $sts = Select-CommerceDocument -Output $fileOut -Kind 'StatefulSet' -Name 'payment'
    Assert-SecretInjected -Doc $sts -SecretName 'commerce-payment' -Label 'payment(-PaymentConfigPath)'
    Assert-SecretAbsent -Doc $sts -SecretName 'payment-devpay' -Label 'payment(-PaymentConfigPath)'
    Assert-NotMatch -Text $fileOut -Pattern '# Secret payment-devpay ' -Because '给了 -PaymentConfigPath 时渠道密钥全部来自运维预置的 Secret,目录里的 devpay 那把不建'
    Assert-NotMatch -Text $fileOut -Pattern '# Secret payment-channel-secrets ' -Because '运维预置的 Secret 部署脚本不建'
    foreach ($key in $ChannelSecretKeys) {
        Assert-NotMatch -Text $sts -Pattern ('(?m)^[ ]*- name: ' + [regex]::Escape($key) + '[ ]*$') -Because "渠道密钥 $key 只经 envFrom 整体注入,脚本不逐条写 env"
    }
}

Test-Case '负向:zone-up 带 -PaymentConfigPath 在任何写操作之前拒绝(payment 在 infra namespace,zone-up 不部署它)' {
    $zoneArgs = @('-Command', 'zone-up', '-ZoneName', $Zone, '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
        '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test',
        '-PaymentConfigPath', $PaymentConfigFile)
    $run = Invoke-CommerceDryRun -Arguments $zoneArgs
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "给了却用不上的参数要拒绝,不静默吞掉。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '-PaymentConfigPath only applies when the global Go services are deployed' -Because '点名原因'
    Assert-NotMatch -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '拒绝之前没有任何写操作'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName 'k8s mall and payment deploy contract')
