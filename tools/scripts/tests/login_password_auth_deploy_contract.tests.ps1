#requires -Version 7
<#
.SYNOPSIS
    login 口令登录部署开关(k8s_deploy.ps1 -LoginPasswordAuth)的契约测试。

.DESCRIPTION
    docs/design/no-single-node-horizontal-scaling-20261001.md §6.5。开关打开时:login 的配置模板多一段 PasswordAuth(只写变量名),
    口令库只读连接串经 Secret login-password-auth 注入 login 的常驻容器,Pod 模板带连接串指纹,login 的内存请求与上限加 Argon2id
    工作集,SQL 与 Redis 两份连接总账记上 login 的口令只读池与失败配额池。本文件钉住:
      - 纯函数(按语法树从 k8s_deploy.ps1 取出):连接串形状门禁、上限镜像、配置段生成、内存加量,以及它们与 Go 源码的跨文件常量;
      - 判据(lib/k8s_client_entry.ps1 第 10 节):配置段读回、差异判定、线上现状读取(kubectl 替身,只查询);
      - k8s_deploy.ps1 Assert-LoginPasswordAuthClusterState:全部 zone 判完再一次拒绝,确认开关只告警放行;接入位置;
      - 两份连接总账的增量;
      - DryRun 端到端:开 / 不开两种产物;互斥、缺连接串、库名不对在任何写操作之前拒绝;连接串从不出现在输出里;
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
$deployParseErrors = $null
$DeployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败: $($deployParseErrors[0].Message)" }
$DeployFunctionNames = @(
    'Get-AuthoritativeScalar', 'Assert-LoginPasswordAuthDsnShape', 'Get-LoginPasswordAuthLimits', 'New-LoginPasswordAuthYaml', 'Add-LoginPasswordAuthMemory',
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

# k8s_deploy.ps1 顶层一条赋值语句的右边,求值后返回(只用于字面量:目录表、名字常量)。
function Get-DeployAssignedValue([string]$Left) {
    $assign = $DeployAst.Find({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq $Left }, $true)
    if ($null -eq $assign) { throw "k8s_deploy.ps1 里找不到 ${Left} 的赋值(契约测试的前提被改掉了)" }
    return & ([scriptblock]::Create($assign.Right.Extent.Text))
}

# 只哈希这把密钥的期望指纹(与 k8s_deploy.ps1 Get-WorkloadSecretFingerprint 同一算法:SHA256("<Secret 名>/workload`n<值>") 的前 12 位十六进制)。
function Get-ExpectedWorkloadSecretHash([string]$SecretName, [string]$Value) {
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try { $bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$SecretName/workload`n$Value")) } finally { $sha256.Dispose() }
    return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12))
}

# 清单文本里 resources.requests / limits 下的两行 memory(<整数>Mi),返回 @{ Request; Limit }。不是恰好两行即 throw。
function Get-MemoryMi([string]$Text) {
    $lines = [regex]::Matches($Text, '(?m)^[ ]+memory:[ \t]*(\d+)Mi[ \t]*\r?$')
    if ($lines.Count -ne 2) { throw "清单里 memory 行应恰好 2 行(requests、limits),实际 $($lines.Count) 行" }
    return @{ Request = [int]$lines[0].Groups[1].Value; Limit = [int]$lines[1].Groups[1].Value }
}

$LoginEtc = 'go/login/etc/login.yaml'
$LoginManifestRelative = 'deploy/k8s/manifests/go-svc/login.yaml'
$DsnEnv = 'MMORPG_LOGIN_PASSWORD_AUTH_DSN'
$DsnSecretMarker = 'contract-dsn-never-echoed-0123'
$GoodDsn = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)/mmorpg?timeout=3s"
$WorkloadSecretCatalog = Get-DeployAssignedValue '$script:WorkloadSecretCatalog'
$PasswordAuthSecretName = [string](Get-DeployAssignedValue '$script:LoginPasswordAuthSecretName')
$GoPasswordAuthDatabase = Get-GoConstant 'go/login/internal/logic/pkg/auth/password_schema.go' 'PasswordAuthDatabase' '"([^"]+)"'
$GoKdfMemoryMi = [int](Get-GoConstant 'go/login/internal/logic/pkg/auth/password_provider.go' 'defaultArgonMemory' '(\d+)\s*\*\s*1024')
$GoMaxKdfConcurrency = [int](Get-GoConstant 'go/login/internal/logic/pkg/auth/password_provider.go' 'MaxPasswordKDFConcurrency' '(\d+)')
$GoQuotaRedisPool = [int](Get-GoConstant 'go/login/internal/svc/auth_init.go' 'passwordQuotaRedisPoolSize' '(\d+)')

# ─────────────────────────────────────────────────────────────────
# 1. 跨文件常量
# ─────────────────────────────────────────────────────────────────

Test-Case '跨文件常量:目录表 login-password-auth 的变量名 / 键 / 消费者;删残留 Secret 用的名字等于目录键;口令库名与 Go 的 PasswordAuthDatabase 相同' {
    Assert-True -Condition ($WorkloadSecretCatalog.Contains('login-password-auth')) -Because '工作负载密钥目录里要有 login-password-auth'
    $entry = $WorkloadSecretCatalog['login-password-auth']
    Assert-Equal -Expected $DsnEnv -Actual $entry.Env -Because '容器变量名与部署输入变量同名(工作负载密钥目录的约定)'
    Assert-Equal -Expected 'dsn' -Actual $entry.SecretKey -Because 'Secret 的键'
    Assert-Equal -Expected 'login' -Actual (@($entry.Consumers) -join ',') -Because '只有 login 读口令库连接串(最小权限)'
    Assert-Equal -Expected 'login-password-auth' -Actual $PasswordAuthSecretName -Because '开关关闭时删的 Secret 就是目录建的那一个'
    Assert-Equal -Expected 'mmorpg' -Actual $GoPasswordAuthDatabase -Because '生成器写 Database: mmorpg,连接串门禁也按 mmorpg 核,与 login 的库名常量同值'
    $keys = @($WorkloadSecretCatalog.Keys)
    Assert-Equal -Expected ([array]::IndexOf($keys, 'login-queue-token') + 1) -Actual ([array]::IndexOf($keys, 'login-password-auth')) -Because '目录里紧跟 login-queue-token(Pod 模板上指纹的先后与目录顺序一致)'
}

# ─────────────────────────────────────────────────────────────────
# 2. 连接串形状门禁(Assert-LoginPasswordAuthDsnShape)
# ─────────────────────────────────────────────────────────────────

Test-Case '连接串形状:合法写法放行(带口令、不带口令、参数里的 / 写成 %2F)' {
    foreach ($dsn in @($GoodDsn, 'login_pw_reader@tcp(tidb.invalid:4000)/mmorpg', "login_pw_reader:$DsnSecretMarker@tcp(10.0.0.8:3306)/mmorpg?loc=Asia%2FShanghai&timeout=3s")) {
        Assert-Equal -Expected '' -Actual (Get-ThrownMessage { Assert-LoginPasswordAuthDsnShape -Value $dsn }) -Because '合法的 go-sql-driver 连接串必须放行'
    }
}

$BadDsnCases = @(
    @{ Label = '空串'; Dsn = ''; Pattern = 'is empty' }
    @{ Label = '行尾换行'; Dsn = "$GoodDsn`n"; Pattern = 'leading or trailing whitespace or a control character' }
    @{ Label = '开头空格'; Dsn = " $GoodDsn"; Pattern = 'leading or trailing whitespace or a control character' }
    @{ Label = '中间夹着制表符'; Dsn = "login_pw_reader:$DsnSecretMarker`t@tcp(tidb.invalid:4000)/mmorpg"; Pattern = 'leading or trailing whitespace or a control character' }
    @{ Label = '未展开的 ${...}'; Dsn = ('login_pw_reader:${LOGIN_PW}' + "$DsnSecretMarker@tcp(tidb.invalid:4000)/mmorpg"); Pattern = 'contains an unexpanded' }
    @{ Label = '库名是别的库'; Dsn = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)/mmorpg_guild"; Pattern = "must point at the database 'mmorpg'" }
    @{ Label = '参数里的 / 没写成 %2F(驱动把它后面那一段当成库名)'; Dsn = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)/mmorpg?loc=Asia/Shanghai"; Pattern = "must point at the database 'mmorpg'" }
    @{ Label = '没有库名'; Dsn = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)/"; Pattern = "must point at the database 'mmorpg'" }
    @{ Label = '没有 /'; Dsn = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)mmorpg"; Pattern = "has no '/'" }
    @{ Label = '没有账号(匿名连接)'; Dsn = 'tcp(tidb.invalid:4000)/mmorpg'; Pattern = 'must carry the account and the server address' }
    @{ Label = '没有地址(驱动会连 127.0.0.1:3306)'; Dsn = "login_pw_reader:$DsnSecretMarker@/mmorpg"; Pattern = 'must carry the account and the server address' }
)
foreach ($case in $BadDsnCases) {
    Test-Case "连接串形状负向:$($case.Label) → 拒绝并点名原因,不回显值" {
        $message = Get-ThrownMessage { Assert-LoginPasswordAuthDsnShape -Value $case.Dsn }
        Assert-Match -Text $message -Pattern ([regex]::Escape($case.Pattern)) -Because '报错要写明哪一条不满足'
        Assert-Match -Text $message -Pattern ([regex]::Escape($DsnEnv)) -Because '报错要点名环境变量'
        Assert-Match -Text $message -Pattern 'Nothing was written\.' -Because '门禁在写路径入口,任何写操作之前'
        Assert-True -Condition (-not $message.Contains($DsnSecretMarker)) -Because '报错不回显连接串(里面有口令)'
    }
}

# ─────────────────────────────────────────────────────────────────
# 3. 上限镜像、配置段与内存加量
# ─────────────────────────────────────────────────────────────────

Test-Case 'New-LoginPasswordAuthYaml:键集合与 Go 的 PasswordAuthConf 一致;Enabled、DSNEnv(= 目录变量名)、Backend 跟参数、Database 固定 mmorpg,其余镜像 login.yaml;不开即空串' {
    $struct = [regex]::Match((Read-RepoText 'go/login/internal/config/config.go'), '(?s)type PasswordAuthConf struct \{(.*?)\n\}')
    Assert-True -Condition $struct.Success -Because 'go/login/internal/config/config.go 里要有 PasswordAuthConf(本用例的前提)'
    $goKeys = @([regex]::Matches($struct.Groups[1].Value, 'json:"([A-Za-z0-9]+)') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
    Assert-True -Condition ($goKeys.Count -ge 11) -Because "PasswordAuthConf 的 json 键要能读到(实际 $($goKeys -join ','))"
    $limits = Get-LoginPasswordAuthLimits
    foreach ($backend in @('tidb', 'mysql')) {
        $scalars = (ConvertFrom-YamlToFlatMap -Text (New-LoginPasswordAuthYaml -Enabled $true -Backend $backend)).Scalars
        $rendered = @($scalars.Keys | Where-Object { $_ -like 'PasswordAuth.*' } | ForEach-Object { $_.Substring('PasswordAuth.'.Length) } | Sort-Object)
        Assert-Equal -Expected ($goKeys -join ',') -Actual ($rendered -join ',') -Because 'Go 认的键一个不少、也没有 Go 不认的键(go-zero 对未知键静默忽略,写错键名等于没写)'
        Assert-Equal -Expected 'true' -Actual $scalars['PasswordAuth.Enabled'] -Because '开关打开 = Enabled: true'
        Assert-Equal -Expected $WorkloadSecretCatalog['login-password-auth'].Env -Actual $scalars['PasswordAuth.DSNEnv'] -Because 'DSNEnv 指向的变量就是目录注入的那一个'
        Assert-Equal -Expected $backend -Actual $scalars['PasswordAuth.Backend'] -Because 'Backend 跟 -DatabaseBackend'
        Assert-Equal -Expected $GoPasswordAuthDatabase -Actual $scalars['PasswordAuth.Database'] -Because 'Database 固定口令库(生产模式的 login 只接受它)'
        foreach ($key in @('MaxOpenConns', 'MaxIdleConns', 'KDFConcurrency', 'KDFWaitTimeout', 'FailureLimit', 'FailureWindow', 'FailureLock')) {
            Assert-Equal -Expected (Get-EtcValue -RelativePath $LoginEtc -KeyPath "PasswordAuth.$key") -Actual $scalars["PasswordAuth.$key"] -Because "PasswordAuth.$key 镜像 $LoginEtc"
        }
    }
    Assert-Equal -Expected ([string]$limits.KdfConcurrency) -Actual (Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.KDFConcurrency') -Because '内存加量与配置段读的是同一个值'
    Assert-Equal -Expected '' -Actual (New-LoginPasswordAuthYaml -Enabled $false -Backend 'tidb') -Because '不开 = 空串,login 模板的产物与接入前逐字相同'
}

Test-Case 'Get-LoginPasswordAuthLimits:取值越界(并发超过 Go 的上限、空闲池大于池上限、配额次数为 0)在任何写操作之前拒绝' {
    $source = Read-RepoText $LoginEtc
    $limitsText = $DeployFunctionText['Get-LoginPasswordAuthLimits']
    $declaredMax = [regex]::Match($limitsText, "'KDFConcurrency' 1 (\d+)")
    Assert-True -Condition $declaredMax.Success -Because 'Get-LoginPasswordAuthLimits 里要有 KDFConcurrency 的范围(本用例的前提)'
    Assert-Equal -Expected $GoMaxKdfConcurrency -Actual ([int]$declaredMax.Groups[1].Value) -Because '部署侧的并发上限与 Go 的 MaxPasswordKDFConcurrency 相同'
    $cases = @(
        @{ Label = 'KDFConcurrency 超过上限'; Pattern = '(?m)^(\s+KDFConcurrency:\s*)\d+'; Replace = ('${1}' + ($GoMaxKdfConcurrency + 1)); Expect = 'PasswordAuth\.KDFConcurrency' }
        @{ Label = 'MaxIdleConns 大于 MaxOpenConns'; Pattern = '(?m)^(\s+MaxIdleConns:\s*)\d+'; Replace = '${1}1000'; Expect = 'PasswordAuth\.MaxIdleConns' }
        @{ Label = 'FailureLimit 为 0'; Pattern = '(?m)^(\s+FailureLimit:\s*)\d+'; Replace = '${1}0'; Expect = 'PasswordAuth\.FailureLimit' }
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
            $message = Get-ThrownMessage { Get-LoginPasswordAuthLimits }
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
# 4. 判据(lib 第 10 节)
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
    Assert-Match -Text $closing.Drifts[0] -Pattern 'restarted Pods refuse to start' -Because '写明后果:运行配置里还开着,新 Pod 拿不到连接串'
    Assert-Equal -Expected '-LoginPasswordAuth' -Actual $closing.KeepArgs -Because '照传串就是这个开关'
    foreach ($live in @($liveOn, $liveOff, $liveBad)) {
        $kept = Get-LoginPasswordAuthDrift -Live $live -Enabled $true
        Assert-Equal -Expected 0 -Actual $kept.Drifts.Count -Because '本次给了开关:段会被整段重写成生成器的形状,不拦'
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
        Assert-Equal -Expected 1 -Actual $calls.Count -Because '只读 login 的 ConfigMap 一次(连接串在 Secret 里,本进程不读)'
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
# 5. k8s_deploy.ps1 Assert-LoginPasswordAuthClusterState(进程内,kubectl 替身)与接入位置
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

Test-Case 'k8s_deploy 接入位置:口令登录防误关闸只调一次,在 Assert-ClientEntryDeployPreflight 的非 DryRun 分支、if ($deploysLogin) 之内,排在第三方登录那道闸之后;连接串只在 Initialize-InjectedSecrets 里解析' {
    $guardCalls = @($DeployAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginPasswordAuthClusterState' }, $true))
    Assert-Equal -Expected 1 -Actual $guardCalls.Count -Because '只有一个调用点'
    $call = $guardCalls[0]
    $ifs = [System.Collections.Generic.List[object]]::new()
    $function = $null
    for ($node = $call.Parent; $null -ne $node; $node = $node.Parent) {
        if ($node -is [Management.Automation.Language.IfStatementAst]) { $ifs.Add($node) }
        if ($node -is [Management.Automation.Language.FunctionDefinitionAst]) { $function = $node; break }
    }
    Assert-Equal -Expected 'Assert-ClientEntryDeployPreflight' -Actual $function.Name -Because '与集群现状预检同处(写路径入口、密钥解析与任何写操作之前)'
    Assert-True -Condition ($ifs.Count -ge 2) -Because '外层 if ($DryRun),内层 if ($deploysLogin)'
    Assert-Equal -Expected '$deploysLogin' -Actual $ifs[0].Clauses[0].Item1.Extent.Text -Because 'infra-up、-SkipGoSvc 不碰 login,不能被这道闸拦住'
    Assert-Equal -Expected '$DryRun' -Actual $ifs[1].Clauses[0].Item1.Extent.Text -Because 'DryRun 不读集群'
    Assert-True -Condition ($null -ne $ifs[1].ElseClause -and $call.Extent.StartOffset -gt $ifs[1].ElseClause.Extent.StartOffset) -Because '只在非 DryRun 分支里调用'
    Assert-Match -Text $ifs[1].Clauses[0].Item2.Extent.Text -Pattern 'Skipping login password auth' -Because 'DryRun 要写明跳过了这道现状预检'
    $thirdParty = $ifs[1].ElseClause.Find({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Assert-LoginAuthProviderClusterState' }, $true)
    Assert-True -Condition ($null -ne $thirdParty -and $thirdParty.Extent.StartOffset -lt $call.Extent.StartOffset) -Because '与第三方登录的防误关闸同一分支、排在其后(共用确认开关)'

    $resolveCalls = @($DeployAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Resolve-LoginPasswordAuthDsn' }, $true))
    Assert-Equal -Expected 1 -Actual $resolveCalls.Count -Because '连接串只解析一次'
    $owner = $resolveCalls[0].Parent
    while ($null -ne $owner -and -not ($owner -is [Management.Automation.Language.FunctionDefinitionAst])) { $owner = $owner.Parent }
    Assert-Equal -Expected 'Initialize-InjectedSecrets' -Actual $owner.Name -Because '在写路径入口懒解析(*-down / *-status 不被它挡住)'
}

# ─────────────────────────────────────────────────────────────────
# 6. 连接总账
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
# 7. DryRun 端到端(dev zone-up,夹具虚构,不连任何集群)
# ─────────────────────────────────────────────────────────────────

$Zone = 'pwauth'
$Infra = 'pwauth-infra'
$ZoneNamespace = "mmorpg-zone-$Zone"
$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-login-password-auth-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot
$CatalogPath = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'catalog') -Namespaces @($ZoneNamespace, $Infra)
$ZoneArgs = @('-Command', 'zone-up', '-ZoneName', $Zone, '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000', '-GoSvcRegistry', 'registry.invalid/test', '-SkipJavaSvc')
$NoWritePattern = '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b'

function Invoke-PasswordAuthDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    # 连接串缺省清空:跑测试的机器上若设了这个变量,不能漏进「不开」的那几次。
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture; MMORPG_SQL_K8S_BOOT_CATALOG = $CatalogPath; MMORPG_LOGIN_PASSWORD_AUTH_DSN = '' }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $fixtureEnv
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

$onRun = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth')) -Env @{ MMORPG_LOGIN_PASSWORD_AUTH_DSN = $GoodDsn }
$offRun = Invoke-PasswordAuthDryRun -Arguments $ZoneArgs
$ManifestMemory = Get-MemoryMi (Read-RepoText $LoginManifestRelative)
$EtcKdfConcurrency = [int](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.KDFConcurrency')
$EtcMaxOpenConns = [int](Get-EtcValue -RelativePath $LoginEtc -KeyPath 'PasswordAuth.MaxOpenConns')

Test-Case 'DryRun(dev zone-up,开):配置模板写 PasswordAuth,只写变量名;连接串不出现在输出的任何地方' {
    Assert-Equal -Expected 0 -Actual $onRun.ExitCode -Because "开关打开的 DryRun 应当成功。输出: $($onRun.Output)"
    $out = [string]$onRun.Output
    Assert-True -Condition (-not $out.Contains($DsnSecretMarker)) -Because '整份输出(ConfigMap、工作负载、Secret 意图行、日志)都不得出现连接串'
    $flat = ConvertTo-FlatManifest -Block (Get-ConfigMapBlock -Output $out -Name 'go-svc-login-config')
    foreach ($pair in @(@('Enabled', 'true'), @('DSNEnv', $DsnEnv), @('Backend', 'tidb'), @('Database', 'mmorpg'))) {
        Assert-Equal -Expected $pair[1] -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.PasswordAuth.$($pair[0])") -Because "PasswordAuth.$($pair[0])"
    }
    foreach ($key in @('MaxOpenConns', 'MaxIdleConns', 'KDFConcurrency', 'KDFWaitTimeout', 'FailureLimit', 'FailureWindow', 'FailureLock')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath $LoginEtc -KeyPath "PasswordAuth.$key") -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.PasswordAuth.$key") -Because "PasswordAuth.$key 镜像 $LoginEtc"
    }
    Assert-True -Condition (-not $flat.Scalars.Contains('data.login.yaml.DevPasswordAuth.Enabled')) -Because '与 DevPasswordAuth 互斥,同一份配置不会两段都有'
    Assert-Match -Text $out -Pattern 'login_password_auth=True' -Because '入口摘要行写明开了'
    Assert-Match -Text $out -Pattern ('Login password auth: on \(database=mmorpg backend=tidb max_open_conns={0} kdf_concurrency={1};' -f $EtcMaxOpenConns, $EtcKdfConcurrency) -Because '写路径入口先渲染一次并打印这次的取值'
    Assert-Match -Text $out -Pattern 'copy the PasswordAuth section of go-svc-login-config into it' -Because '提醒运维把这一段抄进 login 的运行配置 Secret(login 读的是那一份)'
    Assert-Match -Text $out -Pattern 'Skipping login password auth \(PasswordAuth\) cluster-state preflight' -Because 'DryRun 写明跳过了现状预检'
}

Test-Case 'DryRun(dev zone-up,开):连接串经 Secret login-password-auth 注入常驻容器(secretKeyRef,不是 optional),Pod 模板带只哈希连接串的指纹,Secret 先于引用、只写一次' {
    $out = [string]$onRun.Output
    $doc = Get-LoginWorkloadDoc -Output $out
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($doc, '(?m)^[ ]*- name: MMORPG_LOGIN_PASSWORD_AUTH_DSN[ ]*$').Count) -Because '只注入一份'
    $ref = [regex]::Match($doc, '(?m)^[ ]*- name: MMORPG_LOGIN_PASSWORD_AUTH_DSN[ ]*\n[ ]+valueFrom:[ ]*\n[ ]+secretKeyRef:[ ]*\n[ ]+name: login-password-auth[ ]*\n[ ]+key: dsn[ ]*\n(?![ ]+optional:)')
    Assert-True -Condition $ref.Success -Because 'secretKeyRef 到本 namespace 的 login-password-auth / dsn,不写 optional(缺 Secret 就停在 CreateContainerConfigError)'
    $containers = [regex]::Match($doc, '(?m)^      containers:[ ]*$')
    Assert-True -Condition ($containers.Success -and $ref.Index -gt $containers.Index) -Because '注入的是常驻容器,不是 registrar init 容器'
    $hash = [regex]::Matches($doc, '(?m)^[ ]+mmorpg\.io/login-password-auth-hash: "(?<hash>[0-9a-f]{12})"[ ]*$')
    Assert-Equal -Expected 1 -Actual $hash.Count -Because 'Pod 模板上恰好一条连接串指纹'
    Assert-Equal -Expected (Get-ExpectedWorkloadSecretHash -SecretName 'login-password-auth' -Value $GoodDsn) -Actual $hash[0].Groups['hash'].Value -Because '指纹只由连接串决定,换连接串即滚动'
    $secretLine = '[dry-run] kubectl apply -n ' + $ZoneNamespace + ' -f -   # Secret login-password-auth (key dsn; value hidden)'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($out, [regex]::Escape($secretLine)).Count) -Because '同一次运行里 Secret 只写一次'
    Assert-True -Condition ($out.IndexOf($secretLine) -ge 0 -and $out.IndexOf($secretLine) -lt $out.IndexOf('- name: MMORPG_LOGIN_PASSWORD_AUTH_DSN')) -Because 'Secret 先于引用它的工作负载落地'
    Assert-NotMatch -Text $out -Pattern 'kubectl delete secret login-password-auth' -Because '开着时不删自己要用的 Secret'
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

Test-Case 'DryRun(dev zone-up,不开):配置模板没有 PasswordAuth,工作负载不注入、不带指纹,内存与清单相同,删掉残留 Secret' {
    Assert-Equal -Expected 0 -Actual $offRun.ExitCode -Because "不开的 DryRun 应当成功。输出: $($offRun.Output)"
    $out = [string]$offRun.Output
    $flat = ConvertTo-FlatManifest -Block (Get-ConfigMapBlock -Output $out -Name 'go-svc-login-config')
    Assert-Equal -Expected 0 -Actual @($flat.Scalars.Keys | Where-Object { $_ -like 'data.login.yaml.PasswordAuth*' }).Count -Because '不开 = 不写这一段(login 模板的产物与接入前逐字相同)'
    $doc = Get-LoginWorkloadDoc -Output $out
    Assert-NotMatch -Text $doc -Pattern 'MMORPG_LOGIN_PASSWORD_AUTH_DSN' -Because '不注入连接串'
    Assert-NotMatch -Text $doc -Pattern 'login-password-auth-hash' -Because '不带指纹'
    $memory = Get-MemoryMi $doc
    Assert-Equal -Expected ('{0}/{1}' -f $ManifestMemory.Request, $ManifestMemory.Limit) -Actual ('{0}/{1}' -f $memory.Request, $memory.Limit) -Because '内存与清单相同'
    Assert-Match -Text $out -Pattern ('(?m)^\[dry-run\] kubectl delete secret login-password-auth -n ' + [regex]::Escape($ZoneNamespace) + ' --ignore-not-found\s*$') -Because '不粘滞:上次开过、这次没给,删掉残留 Secret'
    Assert-NotMatch -Text $out -Pattern '# Secret login-password-auth ' -Because '不开就不建 Secret'
    Assert-Match -Text $out -Pattern 'login_password_auth=False' -Because '入口摘要行写明没开'
    Assert-NotMatch -Text $out -Pattern 'login password auth is on' -Because '总账不记口令只读池'
}

Test-Case 'DryRun 负向:两个口令开关同时给、开了却没给连接串、连接串指向别的库,都在任何写操作之前拒绝,连接串不回显' {
    $both = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth', '-LoginDevPasswordAuth')) -Env @{ MMORPG_LOGIN_PASSWORD_AUTH_DSN = $GoodDsn; MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = 'contract-dev-shared-secret-0123' }
    $noDsn = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth'))
    $wrongDb = Invoke-PasswordAuthDryRun -Arguments ($ZoneArgs + @('-LoginPasswordAuth')) -Env @{ MMORPG_LOGIN_PASSWORD_AUTH_DSN = "login_pw_reader:$DsnSecretMarker@tcp(tidb.invalid:4000)/mmorpg_guild" }
    foreach ($case in @(
            @{ Label = '两个开关同时给'; Run = $both; Pattern = '-LoginPasswordAuth 与 -LoginDevPasswordAuth 互斥' }
            @{ Label = '没给连接串'; Run = $noDsn; Pattern = '-LoginPasswordAuth needs the environment variable MMORPG_LOGIN_PASSWORD_AUTH_DSN' }
            @{ Label = '库名不对'; Run = $wrongDb; Pattern = "must point at the database 'mmorpg'" }
        )) {
        Assert-True -Condition ($case.Run.ExitCode -ne 0) -Because "$($case.Label):必须拒绝。输出: $($case.Run.Output)"
        Assert-Match -Text $case.Run.Output -Pattern ([regex]::Escape($case.Pattern)) -Because "$($case.Label):报错写明原因"
        Assert-NotMatch -Text $case.Run.Output -Pattern $NoWritePattern -Because "$($case.Label):在任何写操作之前拒绝,不留半截部署"
        Assert-True -Condition (-not ([string]$case.Run.Output).Contains($DsnSecretMarker)) -Because "$($case.Label):输出里不出现连接串"
    }
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

# ─────────────────────────────────────────────────────────────────
# 8. 包装入口:dev_tools.ps1
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
