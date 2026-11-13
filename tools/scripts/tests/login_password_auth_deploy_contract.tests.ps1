#requires -Version 7
<#
.SYNOPSIS
    login 口令登录部署开关(k8s_deploy.ps1 -LoginPasswordAuth)的契约测试。

.DESCRIPTION
    docs/design/no-single-node-horizontal-scaling-20261001.md §6.5。开关打开时:login 的配置模板多一段 PasswordAuth(只写变量名);
    口令库只读连接串在运维预置的 Secret 里,名字写在 SQL boot 目录 <zone-ns>/login 条目的 passwordAuthSecret,部署脚本只按名字经
    secretKeyRef 注入 login 的常驻容器,从不读、不建、不删它;login 的内存请求与上限加 Argon2id 工作集;SQL 与 Redis 两份连接总账记上
    login 的口令只读池与失败配额池。本文件钉住:
      - 纯函数(按语法树从 k8s_deploy.ps1 取出):目录字段的核对、设置镜像、配置段生成、内存加量,以及它们与 Go 源码的跨文件常量;
      - 写路径入口对运维 Secret 的核对(kubectl 替身):只列键名与长度,不读值;DryRun 不连集群;
      - 判据(lib/k8s_client_entry.ps1 第 10 节):配置段读回、差异判定、线上现状读取(kubectl 替身,只查询);
      - k8s_deploy.ps1 Assert-LoginPasswordAuthClusterState:全部 zone 判完再一次拒绝,确认开关只告警放行;接入位置;
      - 两份连接总账的增量;
      - DryRun 端到端:开 / 不开两种产物;互斥、目录缺字段、别的写者条目带这个字段都拒绝;部署脚本不碰连接串;
      - dev_tools.ps1 只在被指定时透传开关,k8s-image-* / k8s-release-* 显式给了即拒绝。
    负向用例一律断错误文本。DryRun 夹具全部虚构,不连接任何集群。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/login_password_auth_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) 'lib' 'k8s_client_entry.ps1')

Write-Host ''
Write-Host '=== login password auth (-LoginPasswordAuth) deployment contract tests ==='

# 被测的 k8s_deploy.ps1 函数里,读文件的那几个按动态作用域读 $RepoRoot(在 k8s_deploy.ps1 里它是脚本级变量)。
$RepoRoot = Get-RepoRoot
$DeployScriptPath = Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'
$deployParseErrors = $null
$DeployAst = [Management.Automation.Language.Parser]::ParseFile($DeployScriptPath, [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败: $($deployParseErrors[0].Message)" }
$DeployFunctionNames = @(
    'Get-AuthoritativeScalar', 'Get-LoginPasswordAuthSettings', 'New-LoginPasswordAuthYaml', 'Add-LoginPasswordAuthMemory',
    'Resolve-LoginPasswordAuthSecretName', 'Assert-LoginPasswordAuthSecrets', 'Build-KubectlBaseArgs',
    'Assert-LoginPasswordAuthClusterState', 'Get-ZoneNamespace',
    'Get-MysqlConnectionPlan', 'Get-MysqlMaxConnectionsFromManifest', 'Get-ManifestDeploymentReplicas', 'Get-AccountDirectoryPoolMaxOpen',
    'Get-MysqlConnectionLedgerInputs', 'Get-MysqlConnectionLedger',
    'Get-RedisMaxClientsFromManifest', 'Get-RedisPoolMirror', 'Get-RedisPlayerPoolMirror', 'Get-RedisConnectionLedgerInputs', 'Get-RedisPlayerConnectionLedgerInputs'
)
$DeployFunctionText = @{}
foreach ($node in $DeployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($DeployFunctionNames -contains $node.Name) {
        Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock()
        $DeployFunctionText[$node.Name] = $node.Extent.Text
    }
}
foreach ($name in $DeployFunctionNames) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 ${name}(契约测试的前提被改掉了)" }
}

# ─────────────────────────────────────────────────────────────────
# 0. 小工具与常量
# ─────────────────────────────────────────────────────────────────

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Read-RepoText([string]$RelativePath) {
    return [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath))
}

# Go 源码里的一个常量(写法 `Name = 值`,可带 const 前缀、可在 const 块里)。读不到即 throw:写法变了要同步本用例。
function Get-GoConstant {
    param([string]$RelativePath, [string]$Name, [string]$ValuePattern)
    $match = [regex]::Match((Read-RepoText $RelativePath), '(?m)^\s*(?:const\s+)?' + [regex]::Escape($Name) + '\s*=\s*' + $ValuePattern)
    if (-not $match.Success) { throw "$RelativePath 里找不到 ${Name}(写法变了就同步本用例)" }
    return $match.Groups[1].Value
}

# k8s_deploy.ps1 顶层一条赋值语句的右边,求值后返回(只用于字面量)。
function Get-DeployAssignedValue([string]$Left) {
    $assign = $DeployAst.Find({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq $Left }, $true)
    if ($null -eq $assign) { throw "k8s_deploy.ps1 里找不到 ${Left} 的赋值(契约测试的前提被改掉了)" }
    return & ([scriptblock]::Create($assign.Right.Extent.Text))
}

# 清单文本里 resources.requests / limits 下的两行 memory(<整数>Mi),返回 @{ Request; Limit }。不是恰好两行即 throw。
function Get-MemoryMi([string]$Text) {
    $lines = [regex]::Matches($Text, '(?m)^[ ]+memory:[ \t]*(\d+)Mi[ \t]*\r?$')
    if ($lines.Count -ne 2) { throw "清单里 memory 行应恰好 2 行(requests、limits),实际 $($lines.Count) 行" }
    return @{ Request = [int]$lines[0].Groups[1].Value; Limit = [int]$lines[1].Groups[1].Value }
}

$LoginEtc = 'go/login/etc/login.yaml'
$LoginManifestRelative = 'deploy/k8s/manifests/go-svc/login.yaml'
$EtcDsnEnv = [string](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.DSNEnv')
$WorkloadSecretCatalog = Get-DeployAssignedValue '$script:WorkloadSecretCatalog'
$GoPasswordAuthDatabase = Get-GoConstant 'go/login/internal/logic/pkg/auth/password_schema.go' 'PasswordAuthDatabase' '"([^"]+)"'
$GoKdfMemoryMi = [int](Get-GoConstant 'go/login/internal/logic/pkg/auth/password_provider.go' 'defaultArgonMemory' '(\d+)\s*\*\s*1024')
$GoMaxKdfConcurrency = [int](Get-GoConstant 'go/login/internal/logic/pkg/auth/password_provider.go' 'MaxPasswordKDFConcurrency' '(\d+)')
$GoQuotaRedisPool = [int](Get-GoConstant 'go/login/internal/svc/auth_init.go' 'passwordQuotaRedisPoolSize' '(\d+)')

# ─────────────────────────────────────────────────────────────────
# 1. 部署脚本不经手连接串
# ─────────────────────────────────────────────────────────────────

Test-Case '部署脚本不经手连接串:工作负载密钥目录里没有口令库连接串,脚本不读连接串环境变量;只有 login 的 boot 目录条目认 passwordAuthSecret;口令库名与 Go 相同' {
    $dsnEntries = @($WorkloadSecretCatalog.Keys | Where-Object { [string]$WorkloadSecretCatalog[$_].Env -match 'PASSWORD_AUTH' -or $_ -match 'password-auth' })
    Assert-Equal -Expected '' -Actual ($dsnEntries -join ',') -Because '连接串由运维预置,不进工作负载密钥目录(目录里的值都由部署脚本解析并写成 Secret)'
    Assert-True -Condition (-not (Read-RepoText 'tools/scripts/k8s_deploy.ps1').Contains('MMORPG_LOGIN_PASSWORD_AUTH_DSN')) -Because '部署脚本不从部署机环境变量读连接串'
    $deploymentEntry = $DeployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-SqlWriterDeploymentEntry' }, $false)
    Assert-True -Condition ($null -ne $deploymentEntry) -Because '要有 Get-SqlWriterDeploymentEntry(本用例的前提)'
    Assert-Match -Text $deploymentEntry.Extent.Text -Pattern 'if\(\$SvcName -ceq ''login''\)\{\$optional\+=''passwordAuthSecret''\}' -Because 'SQL boot 目录只有 login 条目可以带 passwordAuthSecret(别的写者写了按未知字段拒绝)'
    Assert-Equal -Expected 'mmorpg' -Actual $GoPasswordAuthDatabase -Because '生成器写 Database: mmorpg,与 login 的库名常量同值'
}

# ─────────────────────────────────────────────────────────────────
# 2. 目录字段的核对(Resolve-LoginPasswordAuthSecretName)
# ─────────────────────────────────────────────────────────────────

function New-LoginCatalogEntry([hashtable]$Extra = @{}) {
    $entry = [ordered]@{
        requestSecret = 'pwauth-request'; registrarSecret = 'pwauth-registrar'; runtimeConfigSecret = 'pwauth-runtime'
        schemaConfigSecret = 'pwauth-schema-config'; schemaPermissionSecret = 'pwauth-schema-permission'
    }
    foreach ($key in $Extra.Keys) { $entry[$key] = $Extra[$key] }
    return $entry
}

Test-Case 'Resolve-LoginPasswordAuthSecretName:合法名字原样返回' {
    foreach ($name in @('login-password-auth', 'pw0', 'a', ('a' * 63))) {
        Assert-Equal -Expected $name -Actual (Resolve-LoginPasswordAuthSecretName -Entry (New-LoginCatalogEntry @{ passwordAuthSecret = $name }) -EntryKey 'mmorpg-zone-x/login') -Because "$name 是合法的 Secret 名"
    }
}

$BadEntryCases = @(
    @{ Label = '条目为空'; Entry = $null; Pattern = 'needs passwordAuthSecret in the SQL boot catalog entry mmorpg-zone-x/login' }
    @{ Label = '开了开关却没写字段'; Entry = (New-LoginCatalogEntry); Pattern = 'needs passwordAuthSecret in the SQL boot catalog entry mmorpg-zone-x/login' }
    @{ Label = '大写字母'; Entry = (New-LoginCatalogEntry @{ passwordAuthSecret = 'Login-Pw' }); Pattern = 'is not a valid Secret name' }
    @{ Label = '下划线'; Entry = (New-LoginCatalogEntry @{ passwordAuthSecret = 'login_pw' }); Pattern = 'is not a valid Secret name' }
    @{ Label = '以 - 开头'; Entry = (New-LoginCatalogEntry @{ passwordAuthSecret = '-login-pw' }); Pattern = 'is not a valid Secret name' }
    @{ Label = '超过 63 个字符'; Entry = (New-LoginCatalogEntry @{ passwordAuthSecret = ('a' * 64) }); Pattern = 'is not a valid Secret name' }
    @{ Label = '空串'; Entry = (New-LoginCatalogEntry @{ passwordAuthSecret = '' }); Pattern = 'is not a valid Secret name' }
)
foreach ($field in @('requestSecret', 'registrarSecret', 'runtimeConfigSecret', 'schemaConfigSecret', 'schemaPermissionSecret')) {
    $sameName = New-LoginCatalogEntry
    $sameName['passwordAuthSecret'] = $sameName[$field]
    $BadEntryCases += @{ Label = "与 $field 同名"; Entry = $sameName; Pattern = "names the same Secret as ${field}:" }
}
foreach ($case in $BadEntryCases) {
    Test-Case "Resolve-LoginPasswordAuthSecretName 负向:$($case.Label) → 拒绝并点名原因" {
        $message = Get-ThrownMessage { Resolve-LoginPasswordAuthSecretName -Entry $case.Entry -EntryKey 'mmorpg-zone-x/login' }
        Assert-Match -Text $message -Pattern ([regex]::Escape($case.Pattern)) -Because '报错要写明哪一条不满足'
        Assert-Match -Text $message -Pattern 'Nothing was written\.' -Because '核对在写路径入口,任何写操作之前'
    }
}

# ─────────────────────────────────────────────────────────────────
# 3. 写路径入口对运维 Secret 的核对(Assert-LoginPasswordAuthSecrets,kubectl 与目录读取都用替身)
# ─────────────────────────────────────────────────────────────────

Test-Case 'Assert-LoginPasswordAuthSecrets(DryRun):只核目录,每个 zone 打一行将要核对什么,不调 kubectl;目录有问题的 zone 全部判完一次拒绝' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = ''; $KubeConfig = ''
    $DryRun = [switch]$true
    $entries = @{
        'mmorpg-zone-z1/login' = (New-LoginCatalogEntry @{ passwordAuthSecret = 'pw-z1' })
        'mmorpg-zone-z2/login' = (New-LoginCatalogEntry @{ passwordAuthSecret = 'pw-z2' })
    }
    function Get-SqlWriterCatalogEntry([string]$Namespace, [string]$SvcName) {
        $key = "$Namespace/$SvcName"
        if (-not $entries.ContainsKey($key)) { throw '部署实例缺少预固定per-boot计划及独立Secret' }
        return $entries[$key]
    }
    $kubectlCalls = [System.Collections.Generic.List[string]]::new()
    function kubectl { $kubectlCalls.Add(($args -join ' ')); $global:LASTEXITCODE = 0; return '' }
    $zones = @([pscustomobject]@{ Name = 'z1' }, [pscustomobject]@{ Name = 'z2' })
    $lines = @(Assert-LoginPasswordAuthSecrets -Zones $zones 6>&1 | ForEach-Object { [string]$_ })
    Assert-Equal -Expected 0 -Actual $kubectlCalls.Count -Because 'DryRun 不连集群'
    Assert-Equal -Expected 2 -Actual @($lines | Where-Object { $_ -match '^\[dry-run\] would check that Secret pw-z[12] in namespace mmorpg-zone-z[12] has a non-empty key dsn' }).Count -Because '每个 zone 一行,写明 Secret 名与 namespace'

    $entries.Remove('mmorpg-zone-z1/login')
    $entries['mmorpg-zone-z2/login'] = New-LoginCatalogEntry
    $message = Get-ThrownMessage { Assert-LoginPasswordAuthSecrets -Zones $zones 6>$null }
    Assert-Match -Text $message -Pattern 'login password auth Secret preflight failed \(2 zone\(s\)\)\. Nothing was written\.' -Because '全部 zone 判完一次拒绝'
    Assert-Match -Text $message -Pattern 'zone z1: 部署实例缺少预固定per-boot计划' -Because '目录里没有这个 zone 的 login 条目'
    Assert-Match -Text $message -Pattern 'zone z2: -LoginPasswordAuth needs passwordAuthSecret in the SQL boot catalog entry mmorpg-zone-z2/login' -Because '有条目、没字段'
}

Test-Case 'Assert-LoginPasswordAuthSecrets(真跑):只用 go-template 列键名与长度,不读值;缺 Secret、缺键 dsn、dsn 为空都拒绝;kubectl 失败即拒绝' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-ctx'; $KubeConfig = 'C:/contract/kubeconfig'
    $DryRun = [switch]$false
    $entries = @{}
    foreach ($zoneName in @('z1', 'z2', 'z3', 'z4')) { $entries["mmorpg-zone-$zoneName/login"] = New-LoginCatalogEntry @{ passwordAuthSecret = "pw-$zoneName" } }
    function Get-SqlWriterCatalogEntry([string]$Namespace, [string]$SvcName) { return $entries["$Namespace/$SvcName"] }
    $outputs = @{ 'mmorpg-zone-z1' = 'found;dsn=120;'; 'mmorpg-zone-z2' = ''; 'mmorpg-zone-z3' = 'found;other=8;'; 'mmorpg-zone-z4' = 'found;dsn=0;' }
    $failNamespace = ''
    $kubectlCalls = [System.Collections.Generic.List[object]]::new()
    function kubectl {
        $kubectlCalls.Add(@($args))
        $namespace = $args[[array]::IndexOf($args, '-n') + 1]
        if ($namespace -ceq $failNamespace) { $global:LASTEXITCODE = 1; return 'Error from server (Forbidden)' }
        $global:LASTEXITCODE = 0
        return $outputs[$namespace]
    }
    $zones = @('z1', 'z2', 'z3', 'z4' | ForEach-Object { [pscustomobject]@{ Name = $_ } })
    $message = Get-ThrownMessage { Assert-LoginPasswordAuthSecrets -Zones $zones }
    Assert-Match -Text $message -Pattern 'login password auth Secret preflight failed \(3 zone\(s\)\)\. Nothing was written\.' -Because 'z2 没有 Secret、z3 没有 dsn、z4 的 dsn 为空'
    Assert-Match -Text $message -Pattern 'zone z2: Secret pw-z2 does not exist in namespace mmorpg-zone-z2' -Because '没有 Secret 与没有键分开报'
    Assert-Match -Text $message -Pattern 'zone z3: Secret pw-z3 in namespace mmorpg-zone-z3 has no non-empty key dsn' -Because '有 Secret、没有 dsn'
    Assert-Match -Text $message -Pattern 'zone z4: Secret pw-z4 in namespace mmorpg-zone-z4 has no non-empty key dsn' -Because 'dsn 为空等于没给'
    Assert-NotMatch -Text $message -Pattern 'zone z1:' -Because 'z1 的 dsn 非空,通过'
    Assert-Equal -Expected 4 -Actual $kubectlCalls.Count -Because '每个 zone 查一次,全部判完再拒绝'
    foreach ($call in $kubectlCalls) {
        Assert-Match -Text ($call -join ' ') -Pattern '^--context contract-ctx --kubeconfig C:/contract/kubeconfig get secret pw-z\d -n mmorpg-zone-z\d --ignore-not-found -o ' -Because '只查询,带部署用的 context 与 kubeconfig'
        $template = [string]$call[-1]
        Assert-Match -Text $template -Pattern '^go-template=found;\{\{range \$k, \$v := \.data\}\}\{\{\$k\}\}=\{\{len \$v\}\};\{\{end\}\}$' -Because '只取键名与长度(第一项 found 区分没有 Secret 与没有键)'
        Assert-NotMatch -Text $template -Pattern '\{\{\s*\$v\s*\}\}|base64|index' -Because '不把值打出来,也不解码'
    }
    $failNamespace = 'mmorpg-zone-z1'
    Assert-Match -Text (Get-ThrownMessage { Assert-LoginPasswordAuthSecrets -Zones $zones }) -Pattern 'Cannot read Secret pw-z1 in namespace mmorpg-zone-z1 \(login password auth\): kubectl exit code 1' -Because '读不到就拒绝部署(查不到不等于在)'
}

# ─────────────────────────────────────────────────────────────────
# 4. 设置镜像、配置段与内存加量
# ─────────────────────────────────────────────────────────────────

Test-Case 'New-LoginPasswordAuthYaml:键集合与 Go 的 PasswordAuthConf 一致;Enabled、DSNEnv(镜像 login.yaml)、Backend 跟参数、Database 固定 mmorpg,其余镜像 login.yaml;不开即空串' {
    $struct = [regex]::Match((Read-RepoText 'go/login/internal/config/config.go'), '(?s)type PasswordAuthConf struct \{(.*?)\n\}')
    Assert-True -Condition $struct.Success -Because 'go/login/internal/config/config.go 里要有 PasswordAuthConf(本用例的前提)'
    $goKeys = @([regex]::Matches($struct.Groups[1].Value, 'json:"([A-Za-z0-9]+)') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
    Assert-True -Condition ($goKeys.Count -ge 11) -Because "PasswordAuthConf 的 json 键要能读到(实际 $($goKeys -join ','))"
    Assert-Equal -Expected $EtcDsnEnv -Actual (Get-LoginPasswordAuthSettings).DsnEnv -Because 'DSNEnv 镜像 login.yaml:配置模板写它,注入 env 也用它'
    foreach ($backend in @('tidb', 'mysql')) {
        $scalars = (ConvertFrom-YamlToFlatMap -Text (New-LoginPasswordAuthYaml -Enabled $true -Backend $backend)).Scalars
        $rendered = @($scalars.Keys | Where-Object { $_ -like 'PasswordAuth.*' } | ForEach-Object { $_.Substring('PasswordAuth.'.Length) } | Sort-Object)
        Assert-Equal -Expected ($goKeys -join ',') -Actual ($rendered -join ',') -Because 'Go 认的键一个不少、也没有 Go 不认的键(go-zero 对未知键静默忽略,写错键名等于没写)'
        Assert-Equal -Expected 'true' -Actual $scalars['PasswordAuth.Enabled'] -Because '开关打开 = Enabled: true'
        Assert-Equal -Expected $EtcDsnEnv -Actual $scalars['PasswordAuth.DSNEnv'] -Because 'DSNEnv 只写变量名,与注入的 env 同名'
        Assert-Equal -Expected $backend -Actual $scalars['PasswordAuth.Backend'] -Because 'Backend 跟 -DatabaseBackend'
        Assert-Equal -Expected $GoPasswordAuthDatabase -Actual $scalars['PasswordAuth.Database'] -Because 'Database 固定口令库(生产模式的 login 只接受它)'
        foreach ($key in @('MaxOpenConns', 'MaxIdleConns', 'KDFConcurrency', 'KDFWaitTimeout', 'FailureLimit', 'FailureWindow', 'FailureLock')) {
            Assert-Equal -Expected (Get-EtcValue -RelativePath $LoginEtc -KeyPath "PasswordAuth.$key") -Actual $scalars["PasswordAuth.$key"] -Because "PasswordAuth.$key 镜像 $LoginEtc"
        }
    }
    Assert-Equal -Expected '' -Actual (New-LoginPasswordAuthYaml -Enabled $false -Backend 'tidb') -Because '不开 = 空串,login 模板的产物与接入前逐字相同'
}

Test-Case 'Get-LoginPasswordAuthSettings:取值越界(并发超过 Go 的上限、空闲池大于池上限、配额次数为 0、DSNEnv 不是变量名)在任何写操作之前拒绝' {
    $source = Read-RepoText $LoginEtc
    $declaredMax = [regex]::Match($DeployFunctionText['Get-LoginPasswordAuthSettings'], "'KDFConcurrency' 1 (\d+)")
    Assert-True -Condition $declaredMax.Success -Because 'Get-LoginPasswordAuthSettings 里要有 KDFConcurrency 的范围(本用例的前提)'
    Assert-Equal -Expected $GoMaxKdfConcurrency -Actual ([int]$declaredMax.Groups[1].Value) -Because '部署侧的并发上限与 Go 的 MaxPasswordKDFConcurrency 相同'
    $cases = @(
        @{ Label = 'KDFConcurrency 超过上限'; Pattern = '(?m)^(\s+KDFConcurrency:\s*)\d+'; Replace = ('${1}' + ($GoMaxKdfConcurrency + 1)); Expect = 'PasswordAuth\.KDFConcurrency' }
        @{ Label = 'MaxIdleConns 大于 MaxOpenConns'; Pattern = '(?m)^(\s+MaxIdleConns:\s*)\d+'; Replace = '${1}1000'; Expect = 'PasswordAuth\.MaxIdleConns' }
        @{ Label = 'FailureLimit 为 0'; Pattern = '(?m)^(\s+FailureLimit:\s*)\d+'; Replace = '${1}0'; Expect = 'PasswordAuth\.FailureLimit' }
        @{ Label = 'DSNEnv 不是变量名'; Pattern = '(?m)^(\s+DSNEnv:\s*)"[^"]*"'; Replace = '${1}"login-dsn"'; Expect = 'PasswordAuth\.DSNEnv' }
    )
    foreach ($case in $cases) {
        $root = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-pw-limits-' + [guid]::NewGuid().ToString('N'))
        try {
            $target = Join-Path $root $LoginEtc
            $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $target)
            $changed = [regex]::Replace($source, $case.Pattern, $case.Replace)
            Assert-True -Condition ($changed -cne $source) -Because "夹具前提:$($case.Label) 改到了 $LoginEtc 的副本"
            [IO.File]::WriteAllText($target, $changed, [Text.UTF8Encoding]::new($false))
            $RepoRoot = $root
            $message = Get-ThrownMessage { Get-LoginPasswordAuthSettings }
            Assert-Match -Text $message -Pattern $case.Expect -Because "$($case.Label):点名是哪个键"
            Assert-Match -Text $message -Pattern 'Nothing was written\.' -Because "$($case.Label):在写路径入口拒绝"
        }
        finally {
            $RepoRoot = Get-RepoRoot
            Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

Test-Case 'Add-LoginPasswordAuthMemory:login 清单的内存请求与上限各加 KDFConcurrency × 每次 Argon2id 内存(与 Go 的 defaultArgonMemory 相同),别的行不动,CRLF 照旧;形状不对拒绝' {
    $psKdfMi = [regex]::Match($DeployFunctionText['Add-LoginPasswordAuthMemory'], '\$kdfMemoryMi\s*=\s*(\d+)')
    Assert-True -Condition $psKdfMi.Success -Because 'Add-LoginPasswordAuthMemory 里要有每次 Argon2id 的内存常数(本用例的前提)'
    Assert-Equal -Expected $GoKdfMemoryMi -Actual ([int]$psKdfMi.Groups[1].Value) -Because '部署侧按 Go 的 defaultArgonMemory(KiB)/ 1024 MiB 加内存'
    $validate = [regex]::Match($DeployFunctionText['Add-LoginPasswordAuthMemory'], 'ValidateRange\(1, (\d+)\)')
    Assert-Equal -Expected $GoMaxKdfConcurrency -Actual ([int]$validate.Groups[1].Value) -Because '并发参数的上限与 Go 的 MaxPasswordKDFConcurrency 相同'

    $manifest = Read-RepoText $LoginManifestRelative
    $before = Get-MemoryMi $manifest
    foreach ($kdf in @(1, 2, $GoMaxKdfConcurrency)) {
        $after = Add-LoginPasswordAuthMemory -ManifestContent $manifest -KdfConcurrency $kdf
        $raised = Get-MemoryMi $after
        Assert-Equal -Expected ($before.Request + $kdf * $GoKdfMemoryMi) -Actual $raised.Request -Because "KDFConcurrency=${kdf}:请求加工作集(调度按请求放 Pod,只加上限会超卖节点)"
        Assert-Equal -Expected ($before.Limit + $kdf * $GoKdfMemoryMi) -Actual $raised.Limit -Because "KDFConcurrency=${kdf}:上限加工作集(不加的话口令登录一上量就 OOMKill)"
        $strip = { param($t) [regex]::Replace($t, '(?m)^[ ]+memory:[^\r\n]*', '<memory>') }
        Assert-Equal -Expected (& $strip $manifest) -Actual (& $strip $after) -Because '只改两行 memory,清单别处一个字节都不动'
    }
    $crlf = ($manifest -replace "`r?`n", "`r`n")
    $crlfAfter = Add-LoginPasswordAuthMemory -ManifestContent $crlf -KdfConcurrency 2
    Assert-True -Condition ($crlfAfter.Contains("memory: $($before.Request + 2 * $GoKdfMemoryMi)Mi`r`n")) -Because 'CRLF 清单改完行尾仍是 CRLF'

    $noMemory = "spec:`n  template:`n    spec:`n      containers:`n        - name: login`n"
    Assert-Match -Text (Get-ThrownMessage { Add-LoginPasswordAuthMemory -ManifestContent $noMemory -KdfConcurrency 2 }) -Pattern 'exactly two' -Because '没有两行 memory 就不猜'
    $swapped = "          resources:`n            limits:`n              memory: 512Mi`n            requests:`n              memory: 256Mi`n"
    Assert-Match -Text (Get-ThrownMessage { Add-LoginPasswordAuthMemory -ManifestContent $swapped -KdfConcurrency 2 }) -Pattern 'is not under resources\.requests' -Because '顺序反了(第一行在 limits 下)就不猜哪行是请求'
    Assert-Match -Text (Get-ThrownMessage { Add-LoginPasswordAuthMemory -ManifestContent $manifest -KdfConcurrency ($GoMaxKdfConcurrency + 1) }) -Pattern 'KdfConcurrency' -Because '并发越界在参数绑定时就拒绝'
}

# ─────────────────────────────────────────────────────────────────
# 5. 判据(lib 第 10 节)
# ─────────────────────────────────────────────────────────────────

function New-LoginYamlWith([AllowEmptyString()][string]$Section = '') {
    $lines = @('Name: login.rpc', 'Mode: dev', 'RequireClientEndpoint: false')
    if (-not [string]::IsNullOrEmpty($Section)) { $lines += @($Section -split "`r?`n") }
    $lines += @('Kafka:', '  Brokers:', '    - "kafka.contract-infra:9092"')
    return ($lines -join "`n")
}

# 统一成 LF:k8s_deploy.ps1 检出成 CRLF 时 here-string 带 CRLF,下面要自己构造 CRLF 变体。
$RenderedSection = (New-LoginPasswordAuthYaml -Enabled $true -Backend 'tidb') -replace "`r`n", "`n"

Test-Case 'ConvertFrom-LoginPasswordAuthYaml:生成器写出的段读回 Enabled(LF、CRLF 都认);没有段、只有 DevPasswordAuth、Enabled: false 都不算开' {
    $lf = (New-LoginYamlWith $RenderedSection) -replace "`r`n", "`n"
    foreach ($text in @($lf, ($lf -replace "`n", "`r`n"))) {
        $live = ConvertFrom-LoginPasswordAuthYaml -LoginYaml $text
        Assert-True -Condition ($live.Present -and $live.Enabled) -Because '生成器写出的段要读得回 Enabled'
        Assert-Equal -Expected 0 -Actual @($live.Errors).Count -Because "生成器的形状不能被判成认不出: $(@($live.Errors) -join '; ')"
    }
    $none = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith)
    Assert-True -Condition (-not $none.Present -and -not $none.Enabled -and @($none.Errors).Count -eq 0) -Because '没有段 = 没开'
    $devOnly = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith "DevPasswordAuth:`n  Enabled: true`n  SharedSecretEnv: `"LOGIN_DEV_PASSWORD_SHARED_SECRET`"")
    Assert-True -Condition (-not $devOnly.Present -and -not $devOnly.Enabled) -Because 'DevPasswordAuth 是另一个键,不算口令登录'
    $off = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith "PasswordAuth:`n  Enabled: false`n  DSNEnv: `"X`"")
    Assert-True -Condition ($off.Present -and -not $off.Enabled -and @($off.Errors).Count -eq 0) -Because 'Enabled: false = 有段没开'
    $lower = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith "PasswordAuth:`n  enabled: true")
    Assert-True -Condition ($lower.Enabled -and @($lower.Errors).Count -eq 0) -Because 'login 读键名不区分大小写,enabled 照样生效'
}

$UnreadableSections = @(
    @{ Label = '顶格键大小写不是生成器的写法'; Section = "passwordAuth:`n  Enabled: true" }
    @{ Label = '顶格键带引号'; Section = "`"PasswordAuth`":`n  Enabled: true" }
    @{ Label = '同一行带值(flow 写法)'; Section = 'PasswordAuth: {Enabled: true}' }
    @{ Label = '顶格键出现两次'; Section = "PasswordAuth:`n  Enabled: true`nPasswordAuth:`n  Enabled: false" }
    @{ Label = 'Enabled 带引号'; Section = "PasswordAuth:`n  Enabled: `"true`"" }
    @{ Label = 'Enabled 出现两次'; Section = "PasswordAuth:`n  Enabled: true`n  Enabled: false" }
    @{ Label = '制表符缩进'; Section = "PasswordAuth:`n`tEnabled: true" }
    @{ Label = '四格缩进'; Section = "PasswordAuth:`n    Enabled: true" }
)
foreach ($case in $UnreadableSections) {
    Test-Case "ConvertFrom-LoginPasswordAuthYaml 认不出:$($case.Label) → Errors 非空(调用方 fail-closed),不回显值" {
        $live = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith $case.Section)
        Assert-True -Condition ($live.Present) -Because '顶格键在(任何写法)就算 Present'
        Assert-True -Condition (@($live.Errors).Count -gt 0) -Because '认不出就记进 Errors,不能当作没开'
        Assert-True -Condition (-not ((@($live.Errors) -join ' ').Contains('kafka.contract-infra'))) -Because 'Errors 只写行号与键名'
    }
}

Test-Case 'Get-LoginPasswordAuthDrift:线上开着、本次没给 = 一项差异并给出照传开关;本次给了不算;线上没开不算;判不出且本次没给 = 一项差异;JSON 往返不变' {
    $liveOn = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith $RenderedSection)
    $liveOff = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith)
    $liveBad = ConvertFrom-LoginPasswordAuthYaml -LoginYaml (New-LoginYamlWith "PasswordAuth:`n  Enabled: `"true`"")

    $closing = Get-LoginPasswordAuthDrift -Live $liveOn -Enabled $false
    Assert-Equal -Expected 1 -Actual $closing.Drifts.Count -Because '线上开着、本次没给:关掉口令登录是一项差异'
    Assert-Match -Text $closing.Drifts[0] -Pattern 'refuses to start' -Because '写明后果:运行配置里还开着,新 Pod 拿不到连接串'
    Assert-NotMatch -Text $closing.Drifts[0] -Pattern 'deleted' -Because 'Secret 是运维的,部署脚本不删它'
    Assert-Equal -Expected '-LoginPasswordAuth' -Actual $closing.KeepArgs -Because '照传串就是这个开关'
    foreach ($live in @($liveOn, $liveOff, $liveBad)) {
        Assert-Equal -Expected 0 -Actual (Get-LoginPasswordAuthDrift -Live $live -Enabled $true).Drifts.Count -Because '本次给了开关:段会被整段重写成生成器的形状,不拦'
    }
    Assert-Equal -Expected 0 -Actual (Get-LoginPasswordAuthDrift -Live $liveOff -Enabled $false).Drifts.Count -Because '线上没开、本次也不开:没有差异'
    $unknown = Get-LoginPasswordAuthDrift -Live $liveBad -Enabled $false
    Assert-Equal -Expected 1 -Actual $unknown.Drifts.Count -Because '判不出且本次没给:整体算一项差异(fail-closed)'
    Assert-Match -Text $unknown.Drifts[0] -Pattern 'cannot be read' -Because '写明是判不出'
    Assert-Equal -Expected '-LoginPasswordAuth' -Actual $unknown.KeepArgs -Because '判不出时也给出照传开关'
    $roundTrip = $liveOn | ConvertTo-Json -Depth 8 | ConvertFrom-Json
    Assert-Equal -Expected 1 -Actual (Get-LoginPasswordAuthDrift -Live $roundTrip -Enabled $false).Drifts.Count -Because 'JSON 往返之后判定不变'
}

function New-LoginConfigMapJson([AllowEmptyString()][string]$LoginYaml, [switch]$WithoutConfigKey) {
    $data = if ($WithoutConfigKey) { [ordered]@{ 'other.yaml' = 'x: 1' } } else { [ordered]@{ 'login.yaml' = $LoginYaml } }
    return ([ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = 'go-svc-login-config' }; data = $data } | ConvertTo-Json -Depth 10)
}

$LiveStateCases = @(
    @{ Label = 'ConfigMap 不在(首次部署 / namespace 已删)'; Stdout = ''; Expect = { param($s) Assert-True -Condition (-not $s.ConfigMapPresent -and -not $s.Live.Present -and -not $s.Live.Enabled) -Because '不在即没开' } }
    @{ Label = '开着口令登录'; Stdout = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith $RenderedSection)); Expect = { param($s) Assert-True -Condition ($s.ConfigMapPresent -and $s.Live.Enabled -and @($s.Live.Errors).Count -eq 0) -Because '读回开着' } }
    @{ Label = '返回不是 JSON'; Stdout = 'not-json{'; Expect = { param($s) Assert-True -Condition ($s.ConfigMapPresent -and @($s.Live.Errors | Where-Object { $_ -match 'did not come back as JSON' }).Count -eq 1) -Because '判不出,交给调用方 fail-closed' } }
    @{ Label = '没有 login.yaml 键'; Stdout = (New-LoginConfigMapJson -LoginYaml '' -WithoutConfigKey); Expect = { param($s) Assert-True -Condition ($s.ConfigMapPresent -and @($s.Live.Errors | Where-Object { $_ -match 'has no data\.login\.yaml' }).Count -eq 1) -Because '判不出,交给调用方 fail-closed' } }
)
foreach ($liveCase in $LiveStateCases) {
    Test-Case "Get-LoginPasswordAuthLiveState:$($liveCase.Label)(只查询一次 ConfigMap,带上调用方的 context / kubeconfig)" {
        $calls = [System.Collections.Generic.List[object]]::new()
        function Invoke-ClientEntryKubectl {
            param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
            $line = $KubectlArgs -join ' '
            $calls.Add([pscustomobject]@{ Line = $line; KubeContext = $KubeContext; KubeConfig = $KubeConfig; CaptureOutput = $CaptureOutput.IsPresent; AllowFailure = $AllowFailure.IsPresent })
            if ($line -ceq 'get configmap go-svc-login-config -n mmorpg-zone-pwguard --ignore-not-found -o json') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$liveCase.Stdout; Stderr = '' } }
            throw "假 kubectl:没有规则命中这次调用:kubectl $line"
        }
        $state = Get-LoginPasswordAuthLiveState -Namespace 'mmorpg-zone-pwguard' -KubeContext 'contract-ctx' -KubeConfig 'C:/contract/kubeconfig'
        & $liveCase.Expect $state
        Assert-Equal -Expected 1 -Actual $calls.Count -Because '只读 login 的 ConfigMap 一次(连接串在运维的 Secret 里,本进程不读)'
        Assert-True -Condition ($calls[0].CaptureOutput -and -not $calls[0].AllowFailure) -Because 'kubectl 非 0 退出必须 throw(查不到不等于没开)'
        Assert-Equal -Expected 'contract-ctx|C:/contract/kubeconfig' -Actual ("{0}|{1}" -f $calls[0].KubeContext, $calls[0].KubeConfig) -Because '查询必须带调用方给的 context 与 kubeconfig'
    }
}

Test-Case 'Get-LoginPasswordAuthLiveState:kubectl 失败(集群不可达 / Forbidden)原样 throw,不当成没开' {
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        throw "kubectl failed (exit 1): kubectl $($KubectlArgs -join ' ')`nError from server (Forbidden)"
    }
    Assert-Match -Text (Get-ThrownMessage { Get-LoginPasswordAuthLiveState -Namespace 'mmorpg-zone-pwguard' -KubeContext '' -KubeConfig '' }) -Pattern 'kubectl failed \(exit 1\)' -Because '读不到必须让调用方拒绝(fail-closed)'
}

# ─────────────────────────────────────────────────────────────────
# 6. k8s_deploy.ps1 Assert-LoginPasswordAuthClusterState(进程内,kubectl 替身)与接入位置
# ─────────────────────────────────────────────────────────────────

Test-Case 'k8s_deploy 口令登录防误关闸:全部 zone 判完再一次拒绝;-ConfirmLoginAuthProviderChange 只告警放行;给了开关放行;kubectl 失败即拒绝' {
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-ctx'
    $KubeConfig = 'C:/contract/kubeconfig'
    $LoginPasswordAuth = [switch]$false
    $ConfirmLoginAuthProviderChange = [switch]$false
    $configMaps = @{
        'mmorpg-zone-z1' = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith $RenderedSection))
        'mmorpg-zone-z2' = (New-LoginConfigMapJson -LoginYaml (New-LoginYamlWith))
        'mmorpg-zone-z3' = (New-LoginConfigMapJson -LoginYaml '' -WithoutConfigKey)
        'mmorpg-zone-z4' = ''
    }
    $failNamespace = ''
    $calls = [System.Collections.Generic.List[string]]::new()
    function Invoke-ClientEntryKubectl {
        param([string[]]$KubectlArgs, [string]$KubeContext, [string]$KubeConfig, [switch]$CaptureOutput, [switch]$AllowFailure)
        $line = $KubectlArgs -join ' '
        $calls.Add($line)
        Assert-Equal -Expected 'get' -Actual $KubectlArgs[0] -Because '防误关闸只能查询'
        Assert-Equal -Expected 'contract-ctx' -Actual $KubeContext -Because '查询必须带部署用的 context'
        $namespace = $KubectlArgs[[array]::IndexOf($KubectlArgs, '-n') + 1]
        if ($namespace -ceq $failNamespace) { throw "kubectl failed (exit 1): kubectl $line" }
        if ($KubectlArgs[1] -ceq 'configmap') { return [pscustomobject]@{ ExitCode = 0; Stdout = [string]$configMaps[$namespace]; Stderr = '' } }
        throw "假 kubectl:没有规则命中这次调用:kubectl $line"
    }
    $zones = @([pscustomobject]@{ Name = 'z1' }, [pscustomobject]@{ Name = 'z2' }, [pscustomobject]@{ Name = 'z3' }, [pscustomobject]@{ Name = 'z4' })

    $errorText = Get-ThrownMessage { Assert-LoginPasswordAuthClusterState -Zones $zones }
    Assert-Match -Text $errorText -Pattern 'login password auth cluster-state preflight failed \(2 zone\(s\)\)\. Nothing was written\.' -Because '有差异的 zone 逐个计数,一次拒绝'
    Assert-Equal -Expected 2 -Actual @([regex]::Matches($errorText, '(?m)^  - zone ')).Count -Because '每个有差异的 zone 一行'
    Assert-Match -Text $errorText -Pattern 'zone z1 \(namespace=mmorpg-zone-z1\): password login is on[^\r\n]*To keep password login pass -LoginPasswordAuth' -Because 'z1 开着、本次没给:给出照传开关'
    Assert-Match -Text $errorText -Pattern 'zone z3 \(namespace=mmorpg-zone-z3\): the PasswordAuth section of the live login ConfigMap cannot be read' -Because 'z3 判不出,fail-closed'
    Assert-Match -Text $errorText -Pattern 'add -ConfirmLoginAuthProviderChange' -Because '写明怎么确认关闭'
    Assert-NotMatch -Text $errorText -Pattern 'zone z2|zone z4' -Because 'z2 没开、z4 没有 ConfigMap,不列'
    Assert-Equal -Expected 4 -Actual $calls.Count -Because '四个 zone 都判完再拒绝(all-up 逐 zone 部署,不能留半截)'

    $ConfirmLoginAuthProviderChange = [switch]$true
    $records = @(Assert-LoginPasswordAuthClusterState -Zones $zones 3>&1)
    $warnings = @($records | Where-Object { $_ -is [System.Management.Automation.WarningRecord] } | ForEach-Object { $_.Message })
    Assert-Equal -Expected 2 -Actual $warnings.Count -Because '确认后不拒绝,每个有差异的 zone 告警一次'
    Assert-True -Condition (@($warnings | Where-Object { $_ -match '^\[login-auth\] -ConfirmLoginAuthProviderChange supplied; allowed: zone z1 \(' }).Count -eq 1) -Because '告警写明放行的是什么'

    $ConfirmLoginAuthProviderChange = [switch]$false
    $LoginPasswordAuth = [switch]$true
    $kept = @(Assert-LoginPasswordAuthClusterState -Zones $zones 3>&1)
    Assert-Equal -Expected 0 -Actual @($kept | Where-Object { $_ -is [System.Management.Automation.WarningRecord] }).Count -Because '给了开关:不拒绝、不告警(z3 判不出也一样,模板会被整段重写)'

    $LoginPasswordAuth = [switch]$false
    $failNamespace = 'mmorpg-zone-z2'
    Assert-Match -Text (Get-ThrownMessage { Assert-LoginPasswordAuthClusterState -Zones $zones }) -Pattern 'kubectl failed \(exit 1\)' -Because '读不到就拒绝部署(查不到不等于没开)'
}

Test-Case 'k8s_deploy 接入位置:防误关闸在 Assert-ClientEntryDeployPreflight 的非 DryRun 分支、if ($deploysLogin) 之内,排在第三方登录那道闸之后;运维 Secret 的核对也在这个函数里、在 DryRun 分支之前' {
    $preflight = $DeployAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Assert-ClientEntryDeployPreflight' }, $false)
    Assert-True -Condition ($null -ne $preflight) -Because '要有 Assert-ClientEntryDeployPreflight'
    $guardCalls = @($DeployAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginPasswordAuthClusterState' }, $true))
    Assert-Equal -Expected 1 -Actual $guardCalls.Count -Because '防误关闸只有一个调用点'
    $call = $guardCalls[0]
    $ifs = [System.Collections.Generic.List[object]]::new()
    $function = $null
    for ($node = $call.Parent; $null -ne $node; $node = $node.Parent) {
        if ($node -is [Management.Automation.Language.IfStatementAst]) { $ifs.Add($node) }
        if ($node -is [Management.Automation.Language.FunctionDefinitionAst]) { $function = $node; break }
    }
    Assert-Equal -Expected 'Assert-ClientEntryDeployPreflight' -Actual $function.Name -Because '与集群现状预检同处(写路径入口、任何写操作之前)'
    Assert-True -Condition ($ifs.Count -ge 2) -Because '外层 if ($DryRun),内层 if ($deploysLogin)'
    Assert-Equal -Expected '$deploysLogin' -Actual $ifs[0].Clauses[0].Item1.Extent.Text -Because 'infra-up、-SkipGoSvc 不碰 login,不能被这道闸拦住'
    $dryRunIf = $ifs[1]
    Assert-Equal -Expected '$DryRun' -Actual $dryRunIf.Clauses[0].Item1.Extent.Text -Because 'DryRun 不读集群'
    Assert-True -Condition ($null -ne $dryRunIf.ElseClause -and $call.Extent.StartOffset -gt $dryRunIf.ElseClause.Extent.StartOffset) -Because '只在非 DryRun 分支里调用'
    Assert-Match -Text $dryRunIf.Clauses[0].Item2.Extent.Text -Pattern 'Skipping login password auth' -Because 'DryRun 要写明跳过了这道现状预检'
    $thirdParty = $dryRunIf.ElseClause.Find({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginAuthProviderClusterState' }, $true)
    Assert-True -Condition ($null -ne $thirdParty -and $thirdParty.Extent.StartOffset -lt $call.Extent.StartOffset) -Because '与第三方登录的防误关闸同一分支、排在其后(共用确认开关)'

    $secretChecks = @($DeployAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginPasswordAuthSecrets' }, $true))
    Assert-Equal -Expected 1 -Actual $secretChecks.Count -Because '运维 Secret 的核对只有一个调用点'
    Assert-True -Condition ($secretChecks[0].Extent.StartOffset -gt $preflight.Extent.StartOffset -and $secretChecks[0].Extent.EndOffset -lt $preflight.Extent.EndOffset -and $secretChecks[0].Extent.StartOffset -lt $dryRunIf.Extent.StartOffset) -Because '在写路径入口、DryRun 分支之前:DryRun 也核目录字段,真跑再核 Secret 本身'
}

# ─────────────────────────────────────────────────────────────────
# 7. 连接总账
# ─────────────────────────────────────────────────────────────────

Test-Case 'SQL 连接总账:开了口令登录,每 zone 多「login 副本数 × MaxOpenConns」,滚动余量多一份,mmorpg 那一项多 (Z × 副本 + R) × MaxOpenConns;不开时不读、不校验这两项' {
    $inputs = Get-MysqlConnectionLedgerInputs
    $replicas = [int]$inputs['LoginReplicas']
    $maxOpen = [int]$inputs['LoginPasswordAuthMaxOpen']
    Assert-Equal -Expected ([int][regex]::Match((Read-RepoText $LoginManifestRelative), '(?m)^  replicas:\s*(\d+)').Groups[1].Value) -Actual $replicas -Because 'login 副本数读 login 清单'
    Assert-Equal -Expected ([int](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.MaxOpenConns')) -Actual $maxOpen -Because '池上限读 login.yaml,配置模板镜像同一个键'
    $shape = @{ ZoneCount = 3; PlayerStoreShards = 2; RolloutExtraPodsPerDeployment = 2; OpsReserve = 20 }
    $off = Get-MysqlConnectionLedger -Inputs $inputs @shape
    $on = Get-MysqlConnectionLedger -Inputs $inputs @shape -LoginPasswordAuth $true
    Assert-Equal -Expected ($replicas * $maxOpen) -Actual ($on.PerZone - $off.PerZone) -Because '每 zone 多 login 副本数 × 口令只读池'
    Assert-Equal -Expected (3 * $replicas * $maxOpen) -Actual ($on.Steady - $off.Steady) -Because '稳态多 Z 份'
    Assert-Equal -Expected (2 * $maxOpen) -Actual ($on.RolloutSurge - $off.RolloutSurge) -Because '滚动余量多 R 个 login 实例的池'
    Assert-Equal -Expected (3 * $replicas * $maxOpen + 2 * $maxOpen) -Actual ($on.Peak - $off.Peak) -Because '峰值 = 稳态 + 滚动余量 + 运维余量'
    Assert-Equal -Expected ((3 * $replicas + 2) * $maxOpen) -Actual ($on.PerLogicalDb['mmorpg'] - $off.PerLogicalDb['mmorpg']) -Because '这些连接都在 mmorpg 上(搬库时按它算目标集群的预算)'
    foreach ($db in @($off.PerLogicalDb.Keys | Where-Object { $_ -ne 'mmorpg' })) {
        Assert-Equal -Expected $off.PerLogicalDb[$db] -Actual $on.PerLogicalDb[$db] -Because "别的库($db)不变"
    }
    Assert-Equal -Expected ('0/{0}' -f $maxOpen) -Actual ('{0}/{1}' -f $off.LoginPasswordAuthPerInstance, $on.LoginPasswordAuthPerInstance) -Because '分项里写明每个 login 实例的口令池'

    $missing = [ordered]@{}
    foreach ($key in $inputs.Keys) { if ($key -ne 'LoginPasswordAuthMaxOpen') { $missing[$key] = $inputs[$key] } }
    Assert-Equal -Expected $off.Peak -Actual (Get-MysqlConnectionLedger -Inputs $missing @shape).Peak -Because '不开时不读这一项(不开的部署不该因为 login.yaml 的口令段被拒)'
    Assert-Match -Text (Get-ThrownMessage { Get-MysqlConnectionLedger -Inputs $missing @shape -LoginPasswordAuth $true }) -Pattern "MySQL connection ledger: input LoginPasswordAuthMaxOpen=" -Because '开了却缺输入:点名拒绝,不当成 0 条'
}

Test-Case 'Redis 连接总账:开了口令登录,每个 login 副本多一个失败配额池(与 Go 的 passwordQuotaRedisPoolSize 相同),跟着登录域走' {
    $rowOf = { param($Inputs, $Name) @($Inputs.Clients | Where-Object { $_.Name -eq $Name })[0] }
    $coreOff = & $rowOf (Get-RedisConnectionLedgerInputs) 'login'
    $coreOn = & $rowOf (Get-RedisConnectionLedgerInputs -LoginPasswordAuth $true) 'login'
    Assert-Equal -Expected ([int]$coreOff.Fixed + $GoQuotaRedisPool) -Actual ([int]$coreOn.Fixed) -Because '没建玩家域集群:登录域就是这台 Redis,配额池记在 login 行'
    Assert-Equal -Expected ([int]$coreOff.ConfiguredPoolConns) -Actual ([int]$coreOn.ConfiguredPoolConns) -Because '其余池不变'
    $splitOff = & $rowOf (Get-RedisConnectionLedgerInputs -PlayerDomainSplit $true) 'login'
    $splitOn = & $rowOf (Get-RedisConnectionLedgerInputs -PlayerDomainSplit $true -LoginPasswordAuth $true) 'login'
    Assert-Equal -Expected ([int]$splitOff.Fixed) -Actual ([int]$splitOn.Fixed) -Because '建了玩家域集群:配额池跟登录域搬到玩家域那一份,这里不记'
    $playerOff = & $rowOf (Get-RedisPlayerConnectionLedgerInputs) 'login'
    $playerOn = & $rowOf (Get-RedisPlayerConnectionLedgerInputs -LoginPasswordAuth $true) 'login'
    Assert-Equal -Expected ([int]$playerOff.Fixed + $GoQuotaRedisPool) -Actual ([int]$playerOn.Fixed) -Because '玩家域那一份每节点多一池'
    foreach ($name in @('data-service', 'scene-manager', 'guild')) {
        Assert-Equal -Expected ([int](& $rowOf (Get-RedisConnectionLedgerInputs) $name).Fixed) -Actual ([int](& $rowOf (Get-RedisConnectionLedgerInputs -LoginPasswordAuth $true) $name).Fixed) -Because "$name 的行不受影响"
    }
}

# ─────────────────────────────────────────────────────────────────
# 8. DryRun 端到端(dev zone-up,夹具虚构,不连任何集群)
# ─────────────────────────────────────────────────────────────────

$Zone = 'pwauth'
$Infra = 'pwauth-infra'
$ZoneNamespace = "mmorpg-zone-$Zone"
$PasswordAuthSecretName = 'login-pw-reader'
$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-password-auth-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot

# SQL boot 目录夹具:-Fields 里给的条目键加上 passwordAuthSecret。
function New-PasswordAuthCatalog([string]$Name, [hashtable]$Fields) {
    $path = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot $Name) -Namespaces @($ZoneNamespace, $Infra)
    $catalog = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable
    foreach ($key in $Fields.Keys) { $catalog[$key]['passwordAuthSecret'] = $Fields[$key] }
    [IO.File]::WriteAllText($path, ($catalog | ConvertTo-Json -Depth 8 -Compress), [Text.UTF8Encoding]::new($false))
    return $path
}
$CatalogWithSecret = New-PasswordAuthCatalog -Name 'with-secret' -Fields @{ "$ZoneNamespace/login" = $PasswordAuthSecretName }
$CatalogWithoutSecret = New-PasswordAuthCatalog -Name 'without-secret' -Fields @{}
$CatalogOnWrongWriter = New-PasswordAuthCatalog -Name 'wrong-writer' -Fields @{ "$ZoneNamespace/login" = $PasswordAuthSecretName; "$ZoneNamespace/db" = 'db-pw-reader' }

$ZoneArgs = @('-Command', 'zone-up', '-ZoneName', $Zone, '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000', '-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc')
$NoWritePattern = '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b'

function Invoke-PasswordAuthDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [Parameter(Mandatory = $true)][string]$Catalog, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture; MMORPG_SQL_K8S_BOOT_CATALOG = $Catalog }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath $DeployScriptPath -Arguments $Arguments -Env $fixtureEnv
}

function Get-ConfigMapBlock([string]$Output, [string]$Name) {
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: ConfigMap\s*$' -and $_ -cmatch ('(?m)^  name:\s*' + [regex]::Escape($Name) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 ConfigMap $Name 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

# DryRun 输出里名为 login 的工作负载那一份文档(manifest 块按 --- 切开)。不是恰好 1 份即 throw。
function Get-LoginWorkloadDoc([string]$Output) {
    $docs = @(foreach ($block in (Get-ManifestBlocks -Output $Output)) {
            foreach ($doc in (($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')) {
                if ($doc -cmatch '(?m)^kind:[ \t]*(Deployment|StatefulSet)[ \t]*$' -and $doc -cmatch '(?m)^  name:[ \t]*login[ \t]*$') { $doc }
            }
        })
    if ($docs.Count -ne 1) { throw "DryRun 输出里名为 login 的工作负载应恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

$onRun = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth')) -Catalog $CatalogWithSecret
$offRun = Invoke-PasswordAuthDryRun -Arguments $ZoneArgs -Catalog $CatalogWithSecret
$ManifestMemory = Get-MemoryMi (Read-RepoText $LoginManifestRelative)
$EtcKdfConcurrency = [int](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.KDFConcurrency')
$EtcMaxOpenConns = [int](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.MaxOpenConns')

Test-Case 'DryRun(dev zone-up,开):配置模板写 PasswordAuth,只写变量名;入口只核目录字段并写明真跑要核的 Secret' {
    Assert-Equal -Expected 0 -Actual $onRun.ExitCode -Because "开关打开的 DryRun 应当成功。输出: $($onRun.Output)"
    $out = [string]$onRun.Output
    $flat = ConvertTo-FlatManifest -Block (Get-ConfigMapBlock -Output $out -Name 'go-svc-login-config')
    foreach ($pair in @(@('Enabled', 'true'), @('DSNEnv', $EtcDsnEnv), @('Backend', 'tidb'), @('Database', 'mmorpg'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.PasswordAuth.$($pair[0])") -Because "PasswordAuth.$($pair[0])"
    }
    foreach ($key in @('MaxOpenConns', 'MaxIdleConns', 'KDFConcurrency', 'KDFWaitTimeout', 'FailureLimit', 'FailureWindow', 'FailureLock')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath $LoginEtc -KeyPath "PasswordAuth.$key") -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.PasswordAuth.$key") -Because "PasswordAuth.$key 镜像 $LoginEtc"
    }
    Assert-True -Condition (-not $flat.Scalars.Contains('data.login.yaml.DevPasswordAuth.Enabled')) -Because '与 DevPasswordAuth 互斥,同一份配置不会两段都有'
    Assert-Match -Text $out -Pattern 'login_password_auth=True' -Because '入口摘要行写明开了'
    Assert-Match -Text $out -Pattern ('Login password auth: on \(database=mmorpg backend=tidb dsn_env={0} max_open_conns={1} kdf_concurrency={2};' -f [regex]::Escape($EtcDsnEnv), $EtcMaxOpenConns, $EtcKdfConcurrency) -Because '写路径入口先渲染一次并打印这次的取值'
    Assert-Match -Text $out -Pattern 'copy the PasswordAuth section of go-svc-login-config into it' -Because '提醒运维把这一段抄进 login 的运行配置 Secret(login 读的是那一份)'
    Assert-Match -Text $out -Pattern ('\[dry-run\] would check that Secret ' + [regex]::Escape($PasswordAuthSecretName) + ' in namespace ' + [regex]::Escape($ZoneNamespace) + ' has a non-empty key dsn') -Because 'DryRun 只核目录字段,写明真跑要核的 Secret'
    Assert-Match -Text $out -Pattern 'Skipping login password auth \(PasswordAuth\) cluster-state preflight' -Because 'DryRun 写明跳过了现状预检'
}

Test-Case 'DryRun(dev zone-up,开):连接串按目录里的名字 secretKeyRef 注入常驻容器(不是 optional);部署脚本不建、不删这个 Secret,也不给它写指纹' {
    $out = [string]$onRun.Output
    $doc = Get-LoginWorkloadDoc -Output $out
    $envPattern = [regex]::Escape($EtcDsnEnv)
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($doc, '(?m)^[ ]*- name: ' + $envPattern + '[ ]*$').Count) -Because '只注入一份'
    $ref = [regex]::Match($doc, '(?m)^[ ]*- name: ' + $envPattern + '[ ]*\n[ ]+valueFrom:[ ]*\n[ ]+secretKeyRef:[ ]*\n[ ]+name: ' + [regex]::Escape($PasswordAuthSecretName) + '[ ]*\n[ ]+key: dsn[ ]*\n(?![ ]+optional:)')
    Assert-True -Condition $ref.Success -Because 'secretKeyRef 到目录里写的那个运维 Secret 的 dsn,不写 optional(缺 Secret 就停在 CreateContainerConfigError)'
    $containers = [regex]::Match($doc, '(?m)^      containers:[ ]*$')
    Assert-True -Condition ($containers.Success -and $ref.Index -gt $containers.Index) -Because '注入的是常驻容器,不是 registrar init 容器'
    Assert-NotMatch -Text $doc -Pattern ([regex]::Escape($PasswordAuthSecretName) + '-hash|password-auth-hash') -Because '运维的 Secret 本脚本读不到值,没有指纹(换口令要运维 rollout restart)'
    Assert-NotMatch -Text $out -Pattern ('# Secret ' + [regex]::Escape($PasswordAuthSecretName) + ' ') -Because '部署脚本不建运维的 Secret'
    Assert-NotMatch -Text $out -Pattern ('kubectl delete secret ' + [regex]::Escape($PasswordAuthSecretName)) -Because '部署脚本不删运维的 Secret'
}

Test-Case 'DryRun(dev zone-up,开):login 的内存请求与上限各加 KDFConcurrency × Argon2id 内存;两份连接总账记账' {
    $out = [string]$onRun.Output
    $memory = Get-MemoryMi (Get-LoginWorkloadDoc -Output $out)
    Assert-Equal -Expected ($ManifestMemory.Request + $EtcKdfConcurrency * $GoKdfMemoryMi) -Actual $memory.Request -Because '请求加工作集'
    Assert-Equal -Expected ($ManifestMemory.Limit + $EtcKdfConcurrency * $GoKdfMemoryMi) -Actual $memory.Limit -Because '上限加工作集'
    Assert-Match -Text $out -Pattern ('MySQL connection ledger: login password auth is on; each login instance opens up to {0} read-only connections on mmorpg' -f $EtcMaxOpenConns) -Because 'SQL 总账写明口令只读池已进账'
    $onLine = @($out -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    $offLine = @(([string]$offRun.Output) -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-True -Condition ($onLine.Count -eq 1 -and $offLine.Count -eq 1 -and $onLine[0] -ne $offLine[0]) -Because '开与不开的 SQL 总账行不同(峰值多了口令只读池)'
}

Test-Case 'DryRun(dev zone-up,不开):配置模板没有 PasswordAuth,工作负载不注入,内存与清单相同,不核也不碰运维 Secret' {
    Assert-Equal -Expected 0 -Actual $offRun.ExitCode -Because "不开的 DryRun 应当成功。输出: $($offRun.Output)"
    $out = [string]$offRun.Output
    $flat = ConvertTo-FlatManifest -Block (Get-ConfigMapBlock -Output $out -Name 'go-svc-login-config')
    Assert-Equal -Expected 0 -Actual @($flat.Scalars.Keys | Where-Object { $_ -like 'data.login.yaml.PasswordAuth*' }).Count -Because '不开 = 不写这一段(login 模板的产物与接入前逐字相同)'
    $doc = Get-LoginWorkloadDoc -Output $out
    Assert-NotMatch -Text $doc -Pattern ('(?m)^[ ]*- name: ' + [regex]::Escape($EtcDsnEnv) + '[ ]*$') -Because '不注入连接串'
    $memory = Get-MemoryMi $doc
    Assert-Equal -Expected ('{0}/{1}' -f $ManifestMemory.Request, $ManifestMemory.Limit) -Actual ('{0}/{1}' -f $memory.Request, $memory.Limit) -Because '内存与清单相同'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($PasswordAuthSecretName)) -Because '不开时既不核、也不建、不删运维的 Secret'
    Assert-Match -Text $out -Pattern 'login_password_auth=False' -Because '入口摘要行写明没开'
    Assert-NotMatch -Text $out -Pattern 'login password auth is on' -Because '总账不记口令只读池'
}

Test-Case 'DryRun 负向:两个口令开关同时给、目录 login 条目没有 passwordAuthSecret 都在任何写操作之前拒绝;别的写者条目带这个字段按未知字段拒绝' {
    $both = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth', '-LoginDevPasswordAuth')) -Catalog $CatalogWithSecret -Env @{ MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = 'contract-dev-shared-secret-0123' }
    $noField = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth')) -Catalog $CatalogWithoutSecret
    foreach ($case in @(
            @{ Label = '两个开关同时给'; Run = $both; Pattern = '-LoginPasswordAuth 与 -LoginDevPasswordAuth 互斥' }
            @{ Label = '目录没有 passwordAuthSecret'; Run = $noField; Pattern = "-LoginPasswordAuth needs passwordAuthSecret in the SQL boot catalog entry $ZoneNamespace/login" }
        )) {
        Assert-True -Condition ($case.Run.ExitCode -ne 0) -Because "$($case.Label):必须拒绝。输出: $($case.Run.Output)"
        Assert-Match -Text $case.Run.Output -Pattern ([regex]::Escape($case.Pattern)) -Because "$($case.Label):报错写明原因"
        Assert-NotMatch -Text $case.Run.Output -Pattern $NoWritePattern -Because "$($case.Label):在任何写操作之前拒绝,不留半截部署"
    }
    $wrongWriter = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth')) -Catalog $CatalogOnWrongWriter
    Assert-True -Condition ($wrongWriter.ExitCode -ne 0) -Because "db 条目带 passwordAuthSecret 必须拒绝。输出: $($wrongWriter.Output)"
    Assert-Match -Text $wrongWriter.Output -Pattern '维护原件有未知字段：passwordAuthSecret' -Because '只有 login 条目认这个字段,别的写者按未知字段拒绝(zone 开头核名册时就拦下)'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

# ─────────────────────────────────────────────────────────────────
# 9. 包装入口:dev_tools.ps1
# ─────────────────────────────────────────────────────────────────

function Get-DevToolsFunctionText([string]$FunctionName) {
    $parseErrors = $null
    $ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'), [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "dev_tools.ps1 解析失败: $($parseErrors | Out-String)" }
    $definition = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $FunctionName }, $true)
    if ($null -eq $definition) { throw "dev_tools.ps1 里找不到函数 ${FunctionName}(契约测试的前提被改掉了)" }
    return $definition.Extent.Text
}

# 以给定的脚本级参数值调一次 dev_tools.ps1 的 Invoke-K8sDeploy -K8sCommand zone-up,返回假 k8s_deploy.ps1 打出的实收值。
# 假下游没有 CmdletBinding:包装函数 splat 过来的其余具名参数落进 $args,不报找不到参数。
function Invoke-DevToolsK8sDeployWith([hashtable]$Variables) {
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-pw-passthrough-' + [guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $fakeDir
    try {
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_deploy.ps1') -Encoding utf8 -Value @(
            'param([switch]$LoginPasswordAuth, [switch]$LoginDevPasswordAuth)'
            '"LoginPasswordAuth=$($PSBoundParameters.ContainsKey(''LoginPasswordAuth''))"'
            '"LoginDevPasswordAuth=$($PSBoundParameters.ContainsKey(''LoginDevPasswordAuth''))"'
        )
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        . ([scriptblock]::Create((Get-DevToolsFunctionText 'Invoke-K8sDeploy')))
        return (Invoke-K8sDeploy -K8sCommand 'zone-up' | Out-String)
    }
    finally { Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue }
}

# 以给定的脚本级参数值调一次 dev_tools.ps1 的 Invoke-K8sImage -ImageCommand release-zone,返回 @{ Error; DownstreamCalled }。
# 下游是假 k8s_image.ps1,只写一个标记文件:即使拒绝失效,也不会真的构建、推送或部署。
function Invoke-DevToolsK8sImageWith([hashtable]$Variables) {
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-pw-image-reject-' + [guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $fakeDir
    try {
        $marker = Join-Path $fakeDir 'k8s_image.called'
        Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_image.ps1') -Encoding utf8 -Value ('Set-Content -LiteralPath ''{0}'' -Value ($args -join '' ''); exit 0' -f $marker.Replace("'", "''"))
        $ScriptDir = $fakeDir
        foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
        . ([scriptblock]::Create((Get-DevToolsFunctionText 'Invoke-K8sImage')))
        $errorText = $null
        try { Invoke-K8sImage -ImageCommand 'release-zone' *>&1 | Out-Null } catch { $errorText = $_.Exception.Message }
        return [pscustomobject]@{ Error = $errorText; DownstreamCalled = (Test-Path -LiteralPath $marker) }
    }
    finally { Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue }
}

Test-Case 'dev_tools Invoke-K8sDeploy:-LoginPasswordAuth 只在被指定时透传给 k8s_deploy.ps1' {
    Assert-Match -Text (Invoke-DevToolsK8sDeployWith -Variables @{}) -Pattern '(?m)^LoginPasswordAuth=False\r?$' -Because '没给就不透传(默认值只在 k8s_deploy.ps1 一处)'
    $given = Invoke-DevToolsK8sDeployWith -Variables @{ LoginPasswordAuth = $true }
    Assert-Match -Text $given -Pattern '(?m)^LoginPasswordAuth=True\r?$' -Because '给了就透传:漏传一次,开着口令登录的 zone 重新部署就会被防误关闸拒绝'
    Assert-Match -Text $given -Pattern '(?m)^LoginDevPasswordAuth=False\r?$' -Because '两个开关各管各的'
}

Test-Case 'dev_tools Invoke-K8sImage:-LoginPasswordAuth 在调 k8s_image.ps1 之前拒绝(k8s_image.ps1 不部署 login)' {
    $control = Invoke-DevToolsK8sImageWith -Variables @{}
    Assert-True -Condition ($null -eq $control.Error -and $control.DownstreamCalled) -Because "对照组必须调到假 k8s_image.ps1,否则下面「没调到」的断言会空转。错误: $($control.Error)"
    $reject = Invoke-DevToolsK8sImageWith -Variables @{ LoginPasswordAuth = $true }
    Assert-True -Condition ($null -ne $reject.Error) -Because '经 k8s-release-* 不生效,显式给了必须拒绝而不是静默吞掉'
    Assert-Match -Text $reject.Error -Pattern 'k8s-image-\* / k8s-release-\* 不支持 [^:]*-LoginPasswordAuth(,|:)' -Because '错误文本点名 -LoginPasswordAuth'
    Assert-True -Condition (-not $reject.DownstreamCalled) -Because '拒绝必须早于调用 k8s_image.ps1(镜像构建与推送都在那边)'
}

exit (Complete-TestRun -SuiteName 'login password auth deployment')
