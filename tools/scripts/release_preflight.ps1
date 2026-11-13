#requires -Version 7
<#
.SYNOPSIS
    发布前配置门禁。把 docs/ops/release-checklist.md 附录 A.2 的人工勾选框变成可执行判据。

.DESCRIPTION
    为什么不是"两边一致就算过":三处默认值本来就一致 —— 全是同一个占位串
    `change-me-in-production-use-a-strong-random-key`,散在 7 个文件里。
    "一致"这个判据对占位串完全免疫,勾了也等于没勾。

    所以本脚本的判据是三条,缺一不可:
      1. 值不得为空
      2. 值不得等于任何已知占位串
      3. 长度达标(密钥类默认 >= 32 字符)

    **fail-safe**:目标文件不存在、或路径下查不到该键,一律判 FAIL,
    绝不"跳过 = 通过"。唯一的例外是"必须关断的开关":代码侧默认值已核实为关,
    键缺失等价于关,这类检查显式标注 MissingPolicy=pass-off 并在输出里说明。

    给了 -ReleaseVersion 时追加 H 节"制品与版本"检查(版本号、CHANGELOG 段落、制品目录
    sha256sums、build-info 版本与脏树、release manifest、镜像 tag 与制品 commit 对应);
    不给时整节不跑,k8s_image.ps1 / k8s_deploy.ps1 的既有调用行为完全不变。
    制品目录布局见 docs/ops/release-checklist.md §1。

    退出码:
      0 = 无 FAIL(可能有 WARN)
      1 = 存在 FAIL,阻断发布
      2 = 脚本自身出错(参数非法等)

.EXAMPLE
    # 生产发布门禁(默认 profile=prod)
    pwsh -File tools/scripts/release_preflight.ps1 -ImageTag 0ddfcad4a8bb

.EXAMPLE
    # 本地看一眼,不阻断
    pwsh -File tools/scripts/release_preflight.ps1 -ReleaseProfile dev

.EXAMPLE
    # 版本发布门禁:配置 + 制品(制品根默认取 $env:MMORPG_ARTIFACT_ROOT,再默认 <仓库父目录>/artifacts)
    pwsh -File tools/scripts/release_preflight.ps1 -ReleaseVersion v1.2.3 -ImageTag v1.2.3-0ddfcad4a8bb
#>
param(
    # dev  : 只跑结构性检查(键在不在、tag 可不可变),密钥类降级为 WARN
    # staging/prod : 全量 FAIL 判定
    [ValidateSet("dev", "staging", "prod")]
    [string]$ReleaseProfile = "prod",

    # 待发布的镜像 tag(k8s_image.ps1 / dev_tools.ps1 会自动传)
    [string]$ImageTag = "",

    # 待发布的完整镜像引用,可多个(NodeImage / go-svc / java-svc)
    [string[]]$ImageRef = @(),

    [int]$MinSecretLength = 32,
    [int]$MinKafkaBrokers = 3,
    [int]$MinLeaseTtlSeconds = 30,

    # 待发布的版本号(vX.Y.Z)。给了才跑 H 节"制品与版本"检查。
    # 新参数刻意排在已有参数之后:脚本参数默认可按位置绑定,插在中间会让旧的位置参数错位。
    [string]$ReleaseVersion = "",

    # 制品根目录。留空按 lib/artifacts_lib.ps1 的 Get-ArtifactRoot 口径
    # ($env:MMORPG_ARTIFACT_ROOT,再默认 <仓库父目录>/artifacts)。只在给了 -ReleaseVersion 时使用。
    [string]$ArtifactRoot = "",

    # 只打印结果不返回非 0(给"看一眼"用,发布路径绝不能加)
    [switch]$NoFailExit,

    [switch]$Quiet
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
# 逐级 Split-Path 上溯而不是拼 "..\..":反斜杠在 Linux pwsh 里是普通字符,
# 而调用本脚本的契约测试在 deploy-config-tests.yml / release.yml 里跑在 ubuntu-latest 上。
$RepoRoot = (Resolve-Path (Split-Path -Parent (Split-Path -Parent $ScriptDir))).Path
. (Join-Path $ScriptDir "lib" "release_common.ps1")

# 密钥类检查在 dev 下降级成 WARN;staging/prod 是硬 FAIL。
$SecretSeverity = if ($ReleaseProfile -eq 'dev') { 'WARN' } else { 'FAIL' }
$RejectDirtyTag = ($ReleaseProfile -eq 'prod')

$script:Results = New-Object System.Collections.Generic.List[object]

function Add-Result {
    param(
        [Parameter(Mandatory = $true)][string]$Id,
        [Parameter(Mandatory = $true)][string]$Status,   # PASS / FAIL / WARN
        [Parameter(Mandatory = $true)][string]$Target,
        [Parameter(Mandatory = $true)][string]$Detail
    )
    $script:Results.Add([pscustomobject]@{
        Id     = $Id
        Status = $Status
        Target = $Target
        Detail = $Detail
    })
}

function Resolve-RepoPath {
    param([Parameter(Mandatory = $true)][string]$Relative)
    return (Join-Path $RepoRoot ($Relative -replace '/', [System.IO.Path]::DirectorySeparatorChar))
}

# ─────────────────────────────────────────────────────────────────
# 通用检查原语
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
    密钥类检查:非空 + 非占位串 + 长度达标。文件/键缺失一律 FAIL。
#>
function Test-SecretValue {
    param(
        [Parameter(Mandatory = $true)][string]$Id,
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath,
        [int]$MinLength = 0,
        [string]$Severity = ""
    )

    if ($MinLength -le 0) { $MinLength = $MinSecretLength }
    if ([string]::IsNullOrWhiteSpace($Severity)) { $Severity = $SecretSeverity }

    $full = Resolve-RepoPath -Relative $RelativePath
    $target = "${RelativePath}:${KeyPath}"
    $r = Get-YamlScalar -Path $full -KeyPath $KeyPath
    if (-not $r.Found) {
        # fail-safe:查不到 != 通过
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("Key not found; FAIL per fail-safe. " + $r.Reason)
        return $null
    }

    $value = $r.Value
    if ([string]::IsNullOrWhiteSpace($value)) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "Value is empty. An empty secret disables verification; it is not 'secure by default'."
        return $value
    }
    if (Test-PlaceholderSecret -Value $value) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "Value is still the placeholder '$value'; replace it with a strong random value before launch."
        return $value
    }
    if ($value.Length -lt $MinLength) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "Length $($value.Length) < required $MinLength."
        return $value
    }

    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "Non-empty, not a placeholder, length $($value.Length) >= $MinLength."
    return $value
}

<#
.SYNOPSIS
    数值下界检查。文件/键缺失一律 FAIL。
#>
function Test-NumericFloor {
    param(
        [Parameter(Mandatory = $true)][string]$Id,
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath,
        [Parameter(Mandatory = $true)][double]$Floor,
        [Parameter(Mandatory = $true)][string]$Why,
        [string]$Severity = 'FAIL'
    )

    $full = Resolve-RepoPath -Relative $RelativePath
    $target = "${RelativePath}:${KeyPath}"
    $r = Get-YamlScalar -Path $full -KeyPath $KeyPath
    if (-not $r.Found) {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("Key not found; FAIL per fail-safe. " + $r.Reason)
        return
    }

    $num = 0.0
    if (-not [double]::TryParse($r.Value, [ref]$num)) {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail "Value '$($r.Value)' is not a number."
        return
    }
    if ($num -lt $Floor) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "Value $num < floor $Floor. $Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "Value $num >= floor $Floor."
}

<#
.SYNOPSIS
    序列元素个数下界检查(Kafka broker 数)。
#>
function Test-ListFloor {
    param(
        [Parameter(Mandatory = $true)][string]$Id,
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath,
        [Parameter(Mandatory = $true)][int]$Floor,
        [Parameter(Mandatory = $true)][string]$Why,
        [string]$Severity = 'FAIL'
    )

    $full = Resolve-RepoPath -Relative $RelativePath
    $target = "${RelativePath}:${KeyPath}"
    $r = Get-YamlListCount -Path $full -KeyPath $KeyPath
    if (-not $r.Found) {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("Sequence not found; FAIL per fail-safe. " + $r.Reason)
        return
    }
    if ([int]$r.Value -lt $Floor) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "Element count $($r.Value) < floor $Floor. $Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "Element count $($r.Value) >= floor $Floor."
}

<#
.SYNOPSIS
    "必须关断的开关"检查。

.PARAMETER MissingPolicy
    fail     - 键缺失判 FAIL(这个开关必须是显式决策,不能靠默认值)
    pass-off - 键缺失判 PASS(代码侧默认值已核实为关)
#>
function Test-SwitchOff {
    param(
        [Parameter(Mandatory = $true)][string]$Id,
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath,
        [ValidateSet('fail', 'pass-off')][string]$MissingPolicy = 'fail',
        [Parameter(Mandatory = $true)][string]$Why
    )

    $full = Resolve-RepoPath -Relative $RelativePath
    $target = "${RelativePath}:${KeyPath}"
    $r = Get-YamlScalar -Path $full -KeyPath $KeyPath
    if (-not $r.Found) {
        if ($MissingPolicy -eq 'pass-off') {
            Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "Key absent = code default (off). $Why"
        }
        else {
            Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("Key not found; FAIL per fail-safe. " + $r.Reason)
        }
        return
    }

    if ($r.Value.Trim().ToLowerInvariant() -eq 'true') {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail "Switch is true; production must turn it off. $Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "Switch is '$($r.Value)' (off)."
}

# ─────────────────────────────────────────────────────────────────
# A. GateTokenSecret —— 全链共享密钥
# ─────────────────────────────────────────────────────────────────

# 这五处必须同时非占位:任何一处漏改,gate 验签就会整片 fail(或被绕过)。
$GateSecretTargets = @(
    @{ Id = 'secret.gate.cpp';        Path = 'bin/etc/base_deploy_config.yaml';                        Key = 'GateTokenSecret' }
    @{ Id = 'secret.gate.login';      Path = 'go/login/etc/login.yaml';                                Key = 'GateTokenSecret' }
    @{ Id = 'secret.gate.scenemgr';   Path = 'go/scene_manager/etc/scene_manager_service.yaml';        Key = 'GateTokenSecret' }
    @{ Id = 'secret.gate.loginstack'; Path = 'deploy/login-stack.linux/login.yaml';                    Key = 'GateTokenSecret' }
    @{ Id = 'secret.gate.gateway';    Path = 'java/gateway_node/src/main/resources/application.yaml';  Key = 'gate.token-secret' }
)

$gateSecretValues = @{}
foreach ($t in $GateSecretTargets) {
    $v = Test-SecretValue -Id $t.Id -RelativePath $t.Path -KeyPath $t.Key
    if ($null -ne $v) { $gateSecretValues[$t.Path] = $v }
}

# 一致性是**附加**判据,不是主判据 —— 占位串本来就处处一致。
if ($gateSecretValues.Count -ge 2) {
    $distinct = @($gateSecretValues.Values | Sort-Object -Unique)
    if ($distinct.Count -gt 1) {
        Add-Result -Id 'secret.gate.consistency' -Status 'FAIL' -Target 'GateTokenSecret (across 5 files)' `
            -Detail "Found $($distinct.Count) distinct values; gate token verification will fail across the board. Files: $(($gateSecretValues.Keys | Sort-Object) -join ', ')"
    }
    else {
        Add-Result -Id 'secret.gate.consistency' -Status 'PASS' -Target 'GateTokenSecret (across 5 files)' -Detail "All $($gateSecretValues.Count) values match."
    }
}
else {
    Add-Result -Id 'secret.gate.consistency' -Status 'FAIL' -Target 'GateTokenSecret (across 5 files)' `
        -Detail "Only $($gateSecretValues.Count) comparable values were read; cannot complete the consistency check; FAIL per fail-safe."
}

# Admin API key(Java Gateway 管理面)
Test-SecretValue -Id 'secret.admin.apikey' -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'admin.api-key' -MinLength 16 | Out-Null

# ─────────────────────────────────────────────────────────────────
# B. MySQL / Redis 密码
# ─────────────────────────────────────────────────────────────────

Test-SecretValue -Id 'secret.mysql.db'      -RelativePath 'go/db/etc/db.yaml'                                     -KeyPath 'ServerConfig.Database.Passwd' -MinLength 12 | Out-Null
Test-SecretValue -Id 'secret.mysql.gateway' -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.password'    -MinLength 12 | Out-Null

Test-SecretValue -Id 'secret.redis.locator' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'RedisClient.Password'          -MinLength 12 | Out-Null
Test-SecretValue -Id 'secret.redis.login'   -RelativePath 'go/login/etc/login.yaml'                -KeyPath 'Node.RedisClient.Password'        -MinLength 12 | Out-Null

# ─────────────────────────────────────────────────────────────────
# C. 镜像 tag 不可变
# ─────────────────────────────────────────────────────────────────

if ([string]::IsNullOrWhiteSpace($ImageTag) -and $ImageRef.Count -eq 0) {
    # fail-safe:没人告诉我要发什么 tag,不能当"没问题"
    Add-Result -Id 'image.tag.provided' -Status 'FAIL' -Target '-ImageTag / -ImageRef' `
        -Detail "The caller passed no image tag to release; cannot judge its mutability; FAIL per fail-safe."
}
else {
    Add-Result -Id 'image.tag.provided' -Status 'PASS' -Target '-ImageTag / -ImageRef' -Detail "Image identifiers to check were provided."
}

# 可变 tag 在 dev 只是提醒(本地 minikube 天天用 latest);
# staging/prod 是硬阻断 —— 三级回滚的前提就是新旧 revision 指向不同 digest。
$ImageTagSeverity = if ($ReleaseProfile -eq 'dev') { 'WARN' } else { 'FAIL' }

if (-not [string]::IsNullOrWhiteSpace($ImageTag)) {
    $chk = Test-ImmutableImageTag -Tag $ImageTag -RejectDirty:$RejectDirtyTag
    if ($chk.Ok) {
        Add-Result -Id 'image.tag.immutable' -Status 'PASS' -Target "tag=$ImageTag" -Detail "Immutable tag; rollout undo really switches the digest."
    }
    else {
        Add-Result -Id 'image.tag.immutable' -Status $ImageTagSeverity -Target "tag=$ImageTag" -Detail $chk.Reason
    }
}

foreach ($ref in $ImageRef) {
    if ([string]::IsNullOrWhiteSpace($ref)) { continue }
    $tag = Get-ImageTagFromRef -ImageRef $ref
    $chk = Test-ImmutableImageTag -Tag $tag -RejectDirty:$RejectDirtyTag
    if ($chk.Ok) {
        Add-Result -Id 'image.ref.immutable' -Status 'PASS' -Target $ref -Detail "tag='$tag' is immutable."
    }
    else {
        Add-Result -Id 'image.ref.immutable' -Status $ImageTagSeverity -Target $ref -Detail $chk.Reason
    }
}

# ─────────────────────────────────────────────────────────────────
# D. Kafka broker 数下界
# ─────────────────────────────────────────────────────────────────

$kafkaWhy = "A single broker has no ISR redundancy; losing it stops writes for the whole zone (release-checklist §F.B-2)."
Test-ListFloor -Id 'kafka.brokers.cpp'      -RelativePath 'bin/etc/base_deploy_config.yaml'            -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.login'    -RelativePath 'go/login/etc/login.yaml'                    -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.db'       -RelativePath 'go/db/etc/db.yaml'                          -KeyPath 'ServerConfig.Kafka.Brokers'  -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.locator'  -RelativePath 'go/player_locator/etc/player_locator.yaml'  -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity

# login / db 的 PartitionCnt 必须完全一致(启动门禁 fail-closed,见 db.yaml 注释)
$loginPart = Get-YamlScalar -Path (Resolve-RepoPath 'go/login/etc/login.yaml') -KeyPath 'Kafka.PartitionCnt'
$dbPart = Get-YamlScalar -Path (Resolve-RepoPath 'go/db/etc/db.yaml') -KeyPath 'ServerConfig.Kafka.PartitionCnt'
if (-not $loginPart.Found -or -not $dbPart.Found) {
    Add-Result -Id 'kafka.partition.match' -Status 'FAIL' -Target 'Kafka.PartitionCnt (login vs db)' `
        -Detail "At least one side not found; FAIL per fail-safe. login: $($loginPart.Reason) db: $($dbPart.Reason)"
}
elseif ($loginPart.Value -ne $dbPart.Value) {
    Add-Result -Id 'kafka.partition.match' -Status 'FAIL' -Target 'Kafka.PartitionCnt (login vs db)' `
        -Detail "login=$($loginPart.Value) db=$($dbPart.Value) differ; the db startup gate fails closed."
}
else {
    Add-Result -Id 'kafka.partition.match' -Status 'PASS' -Target 'Kafka.PartitionCnt (login vs db)' -Detail "Both sides are $($loginPart.Value)."
}

# ─────────────────────────────────────────────────────────────────
# E. player_locator LeaseTTL 下界
# ─────────────────────────────────────────────────────────────────

Test-NumericFloor -Id 'locator.lease.disconnect' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Lease.DefaultTTLSeconds' `
    -Floor $MinLeaseTtlSeconds -Why "This is the reconnect window; below 30s breaks the 30s reconnect promise (release-checklist §A.2)."

Test-NumericFloor -Id 'locator.lease.etcd' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Node.LeaseTTL' `
    -Floor $MinLeaseTtlSeconds -Why "etcd lease TTL (seconds); too small and nodes are wrongly marked offline during GC pauses."

# ─────────────────────────────────────────────────────────────────
# F. 限流默认值
# ─────────────────────────────────────────────────────────────────

$rlPath = 'java/gateway_node/src/main/resources/application.yaml'
$rlFull = Resolve-RepoPath -Relative $rlPath

Test-NumericFloor -Id 'ratelimit.zone.rps'   -RelativePath $rlPath -KeyPath 'gate.rate-limit.zone-default-rps'   -Floor 1 -Why "A rate limit <= 0 welds the gate shut; every request gets 429 at launch."
Test-NumericFloor -Id 'ratelimit.zone.burst' -RelativePath $rlPath -KeyPath 'gate.rate-limit.zone-default-burst' -Floor 1 -Why "Same as above."
Test-NumericFloor -Id 'ratelimit.ip.rps'     -RelativePath $rlPath -KeyPath 'gate.rate-limit.ip-rps'             -Floor 1 -Why "Same as above."
Test-NumericFloor -Id 'ratelimit.queue.timeout' -RelativePath $rlPath -KeyPath 'gate.rate-limit.queue-timeout-ms' -Floor 1000 -Why "Too small a queue timeout kicks out players who are queueing normally."

$rps = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.zone-default-rps'
$burst = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.zone-default-burst'
if (-not $rps.Found -or -not $burst.Found) {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit" -Detail "At least one of rps/burst not found; FAIL per fail-safe."
}
elseif ([double]$burst.Value -lt [double]$rps.Value) {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit" -Detail "burst=$($burst.Value) < rps=$($rps.Value); the token bucket config contradicts itself."
}
else {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'PASS' -Target "${rlPath}:gate.rate-limit" -Detail "burst=$($burst.Value) >= rps=$($rps.Value)."
}

# enabled 本身是灰度决策(§C T+0 要求默认关),这里只提醒不阻断;
# 但键必须存在 —— 缺键意味着走 Java 侧默认值,那是没人决策过的状态。
$rlEnabled = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.enabled'
if (-not $rlEnabled.Found) {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "Key absent; the rate-limit switch must be an explicit decision; FAIL per fail-safe."
}
elseif ($rlEnabled.Value.Trim().ToLowerInvariant() -ne 'true') {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'WARN' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "Currently '$($rlEnabled.Value)'. Per checklist §C it should be off at canary T+0; turn it on before launch day (§D.1 waves)."
}
else {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'PASS' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "Rate limiting is on."
}

# ─────────────────────────────────────────────────────────────────
# G. debug / GM 开关关断
# ─────────────────────────────────────────────────────────────────

# show-sql 会把每条 SQL 打进日志,生产开着既是性能问题也是数据泄漏面
Test-SwitchOff -Id 'debug.jpa.showsql' -RelativePath $rlPath -KeyPath 'spring.jpa.show-sql' -MissingPolicy 'fail' `
    -Why "JPA show-sql writes every SQL statement (with parameters) to the log."

# DevPasswordAuth 是"绕过账号库口令体系"的开发后门。
# go/login/internal/config/config.go:30 是 optional,零值 Enabled=false,
# 所以键缺失 = 关断,这里用 pass-off。
Test-SwitchOff -Id 'debug.login.devpassword' -RelativePath 'go/login/etc/login.yaml' -KeyPath 'DevPasswordAuth.Enabled' -MissingPolicy 'pass-off' `
    -Why "DevPasswordAuth bypasses the account password system; dev/test only."

# Actuator 暴露面
$actuator = Get-YamlScalar -Path $rlFull -KeyPath 'management.endpoints.web.exposure.include'
if (-not $actuator.Found) {
    Add-Result -Id 'debug.actuator.exposure' -Status 'FAIL' -Target "${rlPath}:management.endpoints.web.exposure.include" `
        -Detail "Key absent; Actuator exposure must be an explicit decision (Spring exposes endpoints beyond health/info by default); FAIL per fail-safe."
}
else {
    $dangerous = @('*', 'env', 'heapdump', 'threaddump', 'configprops', 'beans', 'shutdown', 'loggers')
    $items = @($actuator.Value -split ',' | ForEach-Object { $_.Trim().ToLowerInvariant() } | Where-Object { $_ })
    $hit = @($items | Where-Object { $dangerous -contains $_ })
    if ($hit.Count -gt 0) {
        Add-Result -Id 'debug.actuator.exposure' -Status 'FAIL' -Target "${rlPath}:management.endpoints.web.exposure.include" `
            -Detail "Dangerous endpoints exposed: $($hit -join ', '). Production should expose only health/info."
    }
    else {
        Add-Result -Id 'debug.actuator.exposure' -Status 'PASS' -Target "${rlPath}:management.endpoints.web.exposure.include" -Detail "Exposes only '$($actuator.Value)'."
    }
}

# C++ 节点日志级别。数值直接按 muduo Logger::LogLevel 枚举解释(0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL;
# node.cpp 的 Node::InitLogSystem 直接 static_cast,没有映射表),下界 2 = INFO。
# 2026-10-22 前这里按「0=DEBUG」把下界设成 1,恰好放过了 DEBUG。
# K8s 的 node-config ConfigMap 由 k8s_deploy.ps1 从同一个键镜像(同日起不再写死),所以这里查到的就是集群里生效的值;
# 0..5 的结构校验在 k8s_deploy.ps1 生成期做。
Test-NumericFloor -Id 'debug.cpp.loglevel' -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'LogLevel' -Floor 2 `
    -Why "muduo enum 0=TRACE, 1=DEBUG; in production they flood the log from hot paths such as AOI, movement and saves."

# ─────────────────────────────────────────────────────────────────
# H. 制品与版本(仅在给了 -ReleaseVersion 时检查)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
    要发布的持久 schema 台账必须已封存到最大世代,且每条封存都成立(混版本持久字段保护设计 §3.1「封存」,
    审计 r2-ps#1 / r2-ps#10)。

.DESCRIPTION
    台账 proto/persist_schema_gen.txt 的封存行 sealed=<世代>:<行数>:<sha256> 由
    dev_tools.ps1 -Command persist-schema-seal 追加。不变量是「同一世代号,所有二进制认识的字段集合相同」:
    最大世代没封存,下一个分支就可能往这个已经上线的世代里追加字段,只比世代号的 go/db C1 / C1b / C2、
    scene C3、login C4 都拦不住,旧写方会静默丢掉新列。

    分两步:
      1. 按文本比世代号:最后一条封存的世代必须等于最大世代(没封存就不用往下核)。
      2. 调生成器的只核不写模式 go run ./cmd --persist-schema-verify,与生成同一份摘要口径(persistschema.SealDigest),
         确认每条封存都成立。只比世代号会放过「合并带进同世代的行」:那时封存已被违反,最大世代却仍等于
         最后一条封存的世代(2026-11-05 评审)。摘要不在这里另写一份,避免两种语言的口径漂移。
    档位与本脚本其余检查一致:只有 -ReleaseProfile dev 降为 WARN。内部测试版本不封存,跑预检用 dev 档。
    找不到 go、生成器失败都按档位判,不跳过。
    读的是当前工作树的台账,与本脚本其余配置检查同口径;制品与工作树不是同一提交时 release.buildinfo.commit 会告警。
#>
function Test-PersistSchemaLedgerSealed {
    $ledgerPath = Join-Path $RepoRoot 'proto/persist_schema_gen.txt'
    $severity = if ($ReleaseProfile -eq 'dev') { 'WARN' } else { 'FAIL' }
    if (-not [System.IO.File]::Exists($ledgerPath)) {
        Add-Result -Id 'release.persist_schema.sealed' -Status 'FAIL' -Target $ledgerPath -Detail "Ledger not found; FAIL per fail-safe."
        return
    }
    $maxGen = 0
    $lastSealGen = 0
    foreach ($line in [System.IO.File]::ReadAllLines($ledgerPath)) {
        $text = $line.Trim()
        if ($text -match '^(\d+)=') {
            $gen = [int64]$Matches[1]
            if ($gen -gt $maxGen) { $maxGen = $gen }
        }
        elseif ($text -cmatch '^sealed=(\d+):(\d+):[0-9a-f]{64}$') {
            $lastSealGen = [int64]$Matches[1]
        }
        elseif ($text.StartsWith('sealed=')) {
            Add-Result -Id 'release.persist_schema.sealed' -Status 'FAIL' -Target $ledgerPath -Detail "Malformed seal line: '$text'. Do not edit the ledger by hand; reseal with dev_tools.ps1 -Command persist-schema-seal."
            return
        }
    }
    if ($maxGen -eq 0) {
        Add-Result -Id 'release.persist_schema.sealed' -Status 'FAIL' -Target $ledgerPath -Detail "The ledger has no field lines; FAIL per fail-safe."
        return
    }
    if ($lastSealGen -ne $maxGen) {
        $sealed = if ($lastSealGen -eq 0) { 'never sealed' } else { "last sealed at generation $lastSealGen" }
        Add-Result -Id 'release.persist_schema.sealed' -Status $severity -Target $ledgerPath `
            -Detail "Ledger max generation $maxGen is not sealed ($sealed): before releasing, run pwsh -File tools/scripts/dev_tools.ps1 -Command persist-schema-seal on main and commit it, then cut the release (docs/ops/release-checklist.md §2.1 ①)."
        return
    }
    $verify = Invoke-PersistSchemaLedgerVerify
    if ($verify.Ok) {
        Add-Result -Id 'release.persist_schema.sealed' -Status 'PASS' -Target $ledgerPath -Detail "Ledger sealed up to max generation $maxGen; the generator verified every seal."
        return
    }
    Add-Result -Id 'release.persist_schema.sealed' -Status $severity -Target $ledgerPath `
        -Detail "Ledger is sealed up to max generation $maxGen, but generator verification failed: $($verify.Reason). Fix the ledger as the generator says (usually a merge brought rows into a sealed generation); do not edit seal lines."
}

# 运行生成器的只核不写模式。PROTO_GEN_CONFIG_PATH 只用来定位台账;用 no-unity 配置,不碰客户端仓。
# 原生 stderr 是诊断数据,本函数内不让它按 Stop 终止(脚本顶层是 Stop)。
function Invoke-PersistSchemaLedgerVerify {
    $ErrorActionPreference = 'Continue'
    $go = Get-Command go -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $go) {
        return @{ Ok = $false; Reason = 'go not found; cannot run the generator to verify seals' }
    }
    $generatorDir = Join-Path $RepoRoot 'tools/proto_generator/protogen'
    $configPath = Join-Path $generatorDir 'etc/proto_gen.no-unity.yaml'
    $previousConfig = [Environment]::GetEnvironmentVariable('PROTO_GEN_CONFIG_PATH', 'Process')
    Push-Location -LiteralPath $generatorDir
    try {
        [Environment]::SetEnvironmentVariable('PROTO_GEN_CONFIG_PATH', $configPath, 'Process')
        $output = & $go.Source run ./cmd --persist-schema-verify 2>&1 | Out-String
        $exitCode = $LASTEXITCODE
    }
    finally {
        [Environment]::SetEnvironmentVariable('PROTO_GEN_CONFIG_PATH', $previousConfig, 'Process')
        Pop-Location
    }
    if ($exitCode -eq 0) {
        return @{ Ok = $true; Reason = '' }
    }
    $tail = (@($output -split "`r?`n" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }) | Select-Object -Last 3) -join ' | '
    return @{ Ok = $false; Reason = "go run ./cmd --persist-schema-verify exited ${exitCode}: $tail" }
}

<#
.SYNOPSIS
    检查待发布版本的制品本身:存在、完整、确实是这个版本、来自干净工作树、release manifest 已落盘。

.DESCRIPTION
    为什么预检要查制品:配置全对、要部署的却是另一版镜像(或 tar 被截断/篡改、版本目录是脏树产物),
    一样是事故。A 仓的预检只查配置且没接进流水线(docs/design/release-packaging-standard-20260914.md §2.11)。

    fail-safe 与上面各节一致:目录/文件/字段缺失、JSON 解析失败一律 FAIL,不跳过。
    版本号本身不合法时后续检查全都无从谈起,只登记这一条就返回。
#>
function Invoke-ReleaseArtifactChecks {
    $versionTarget = "-ReleaseVersion=$ReleaseVersion"
    $versionCheck = Test-ReleaseVersion -Version $ReleaseVersion
    if (-not $versionCheck.Ok) {
        Add-Result -Id 'release.version' -Status 'FAIL' -Target $versionTarget -Detail $versionCheck.Reason
        return
    }
    Add-Result -Id 'release.version' -Status 'PASS' -Target $versionTarget -Detail "Valid release version."

    # 版本号四处一致之一:CHANGELOG 段落(与 make_release.ps1 同口径,找不到或为空都算失败)
    $changelog = Test-ChangelogReleaseSection -ChangelogPath (Join-Path $RepoRoot 'CHANGELOG.md') -Version $ReleaseVersion
    $changelogTarget = "CHANGELOG.md $($changelog.Heading)"
    if ($changelog.Ok) {
        Add-Result -Id 'release.changelog' -Status 'PASS' -Target $changelogTarget -Detail "Section exists and is not empty."
    }
    else {
        Add-Result -Id 'release.changelog' -Status 'FAIL' -Target $changelogTarget -Detail $changelog.Reason
    }

    # 制品库按需加载:不给 -ReleaseVersion 的既有调用方不依赖它
    $libPath = Join-Path $ScriptDir 'lib' 'artifacts_lib.ps1'
    if (-not (Test-Path -LiteralPath $libPath -PathType Leaf)) {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target $libPath -Detail "Artifact library script missing; cannot locate or verify artifacts; FAIL per fail-safe."
        return
    }
    . $libPath

    try {
        $channelRoot = Get-ChannelRoot -Channel release -Override $ArtifactRoot
    }
    catch {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target "-ArtifactRoot=$ArtifactRoot" -Detail "Failed to resolve the artifact root: $($_.Exception.Message)"
        return
    }

    $versionDir = Join-Path $channelRoot 'images' $ReleaseVersion
    $manifestPath = Join-Path $channelRoot 'manifests' "$ReleaseVersion.json"
    $buildInfo = $null

    if (-not [System.IO.Directory]::Exists($versionDir)) {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target $versionDir `
            -Detail "Version directory not found: not yet published with publish_images.ps1 -Version $ReleaseVersion, or -ArtifactRoot / MMORPG_ARTIFACT_ROOT points to the wrong place."
    }
    else {
        Add-Result -Id 'release.artifact.dir' -Status 'PASS' -Target $versionDir -Detail "Version directory exists."

        try {
            Test-Sha256Sums -Dir $versionDir | Out-Null
            Add-Result -Id 'release.artifact.sha256sums' -Status 'PASS' -Target $versionDir -Detail "Files in the directory match sha256sums.txt exactly."
        }
        catch {
            Add-Result -Id 'release.artifact.sha256sums' -Status 'FAIL' -Target $versionDir -Detail $_.Exception.Message
        }

        $buildInfoPath = Join-Path $versionDir 'build-info.json'
        try {
            $buildInfo = [System.IO.File]::ReadAllText($buildInfoPath) | ConvertFrom-Json
        }
        catch {
            $buildInfo = $null
            Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.json is missing or not valid JSON: $($_.Exception.Message)"
        }

        if ($null -ne $buildInfo) {
            # app_version 接受带 v 与不带 v 两种写法:两者指同一个版本,不存在歧义
            $appVersion = [string]$buildInfo.app_version
            if ([string]::IsNullOrWhiteSpace($appVersion)) {
                Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.app_version is empty; cannot prove this artifact is $ReleaseVersion."
            }
            elseif ($appVersion -cne $ReleaseVersion -and $appVersion -cne $versionCheck.Normalized) {
                Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.app_version='$appVersion' differs from -ReleaseVersion=${ReleaseVersion}: the version directory name does not match the artifact content."
            }
            else {
                Add-Result -Id 'release.buildinfo.version' -Status 'PASS' -Target $buildInfoPath -Detail "app_version='$appVersion'."
            }

            $dirty = $buildInfo.dirty
            if ($dirty -is [bool] -and -not $dirty) {
                Add-Result -Id 'release.buildinfo.clean' -Status 'PASS' -Target $buildInfoPath -Detail "dirty=false."
            }
            else {
                Add-Result -Id 'release.buildinfo.clean' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.dirty='$dirty' (missing or anything other than false is rejected): a dirty-tree artifact cannot be rebuilt from its commit and must not be released."
            }

            # 上面 A~G 节读的是**当前工作树**的配置文件。工作树不在制品 commit 上时,那些结论不代表
            # 要发布的版本 —— 只提醒不阻断(预检也会在别的 commit 上被用来"看一眼")。
            $artifactCommit = [string]$buildInfo.commit
            $stamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
            if (-not $stamp.Ok) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "Cannot read the working tree commit ($($stamp.Reason)); cannot confirm the config checks above match this version."
            }
            elseif ($artifactCommit -cne $stamp.Commit) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "Artifact commit='$artifactCommit', working tree HEAD=$($stamp.Commit): the config checks above read the working tree, so they do not speak for this version. Rerun preflight on the release commit."
            }
            elseif ($stamp.Dirty) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "The working tree has uncommitted changes: the config checks above may not have read the config committed for this version."
            }
            else {
                Add-Result -Id 'release.buildinfo.commit' -Status 'PASS' -Target $buildInfoPath -Detail "Artifact commit matches working tree HEAD and the tree is clean."
            }
        }
    }

    # manifest JSON 是 make_release.ps1 最后才写的"已发布"哨兵:存在即说明 release 记录完整落盘
    if (-not [System.IO.File]::Exists($manifestPath)) {
        Add-Result -Id 'release.manifest' -Status 'FAIL' -Target $manifestPath -Detail "release manifest not found: make_release.ps1 -Version $ReleaseVersion has not run, or it failed midway."
    }
    else {
        $manifestText = [System.IO.File]::ReadAllText($manifestPath)
        $manifestOk = $false
        $manifestReason = "file is empty."
        if (-not [string]::IsNullOrWhiteSpace($manifestText)) {
            try {
                $manifestText | ConvertFrom-Json | Out-Null
                $manifestOk = $true
            }
            catch {
                $manifestReason = "is not valid JSON: $($_.Exception.Message)"
            }
        }
        if ($manifestOk) {
            Add-Result -Id 'release.manifest' -Status 'PASS' -Target $manifestPath -Detail "manifest exists and is valid JSON."
        }
        else {
            Add-Result -Id 'release.manifest' -Status 'FAIL' -Target $manifestPath -Detail "release manifest $manifestReason"
        }
    }

    # 要部署的 tag 必须就是这份制品:形状由 Get-ReleaseImageTag 决定,commit 必须与 build-info 对上
    if (-not [string]::IsNullOrWhiteSpace($ImageTag)) {
        $tagTarget = "tag=$ImageTag"
        $tagMatch = [regex]::Match($ImageTag, '^' + [regex]::Escape($ReleaseVersion) + '-([0-9a-f]{12})\z')
        if (-not $tagMatch.Success) {
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "A release image tag must look like $ReleaseVersion-<12-char lowercase commit> (per Get-ReleaseImageTag)."
        }
        elseif ($null -eq $buildInfo) {
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "build-info.json is unreadable; cannot check the commit in the tag; FAIL per fail-safe."
        }
        elseif ($tagMatch.Groups[1].Value -cne [string]$buildInfo.commit) {
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "Tag commit=$($tagMatch.Groups[1].Value) differs from build-info.commit='$($buildInfo.commit)': the image to deploy is not this artifact."
        }
        else {
            Add-Result -Id 'release.imagetag.commit' -Status 'PASS' -Target $tagTarget -Detail "Tag commit matches build-info.commit."
        }
    }

    Test-PersistSchemaLedgerSealed
}

if (-not [string]::IsNullOrWhiteSpace($ReleaseVersion)) {
    Invoke-ReleaseArtifactChecks
}

# ─────────────────────────────────────────────────────────────────
# 汇总输出
# ─────────────────────────────────────────────────────────────────

$failCount = @($script:Results | Where-Object { $_.Status -eq 'FAIL' }).Count
$warnCount = @($script:Results | Where-Object { $_.Status -eq 'WARN' }).Count
$passCount = @($script:Results | Where-Object { $_.Status -eq 'PASS' }).Count

if (-not $Quiet) {
    Write-Host ""
    Write-Host "============================================================"
    Write-Host " release preflight (profile=$ReleaseProfile)"
    Write-Host "============================================================"
    foreach ($r in $script:Results) {
        $color = switch ($r.Status) {
            'PASS' { 'Green' }
            'WARN' { 'Yellow' }
            default { 'Red' }
        }
        Write-Host ("  [{0}] {1,-28} {2}" -f $r.Status, $r.Id, $r.Target) -ForegroundColor $color
        if ($r.Status -ne 'PASS') {
            Write-Host ("         {0}" -f $r.Detail) -ForegroundColor $color
        }
    }
    Write-Host "------------------------------------------------------------"
    Write-Host ("  PASS={0}  WARN={1}  FAIL={2}" -f $passCount, $warnCount, $failCount)
    Write-Host "============================================================"
}

if ($failCount -gt 0) {
    Write-Host ""
    Write-Host "release preflight FAILED: $failCount checks failed; release blocked." -ForegroundColor Red
    Write-Host "For secret-related failures, inject environment variables and rerun (see Resolve-InjectedSecret in tools/scripts/lib/release_common.ps1)." -ForegroundColor Red
    if (-not $NoFailExit) {
        exit 1
    }
}
else {
    Write-Host "release preflight PASSED (warn=$warnCount)." -ForegroundColor Green
}

exit 0
