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
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("查不到该键,按 fail-safe 判 FAIL。" + $r.Reason)
        return $null
    }

    $value = $r.Value
    if ([string]::IsNullOrWhiteSpace($value)) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "值为空。空密钥 = 校验被关掉,不是'默认安全'。"
        return $value
    }
    if (Test-PlaceholderSecret -Value $value) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "值仍是占位串 '$value',上线前必须替换成强随机值。"
        return $value
    }
    if ($value.Length -lt $MinLength) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "长度 $($value.Length) < 要求的 $MinLength。"
        return $value
    }

    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "非空、非占位、长度 $($value.Length) >= $MinLength。"
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
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("查不到该键,按 fail-safe 判 FAIL。" + $r.Reason)
        return
    }

    $num = 0.0
    if (-not [double]::TryParse($r.Value, [ref]$num)) {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail "值 '$($r.Value)' 不是数字。"
        return
    }
    if ($num -lt $Floor) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "值 $num < 下界 $Floor。$Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "值 $num >= 下界 $Floor。"
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
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("查不到该序列,按 fail-safe 判 FAIL。" + $r.Reason)
        return
    }
    if ([int]$r.Value -lt $Floor) {
        Add-Result -Id $Id -Status $Severity -Target $target -Detail "元素数 $($r.Value) < 下界 $Floor。$Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "元素数 $($r.Value) >= 下界 $Floor。"
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
            Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "键不存在 = 采用代码默认值(关)。$Why"
        }
        else {
            Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail ("查不到该键,按 fail-safe 判 FAIL。" + $r.Reason)
        }
        return
    }

    if ($r.Value.Trim().ToLowerInvariant() -eq 'true') {
        Add-Result -Id $Id -Status 'FAIL' -Target $target -Detail "开关为 true,生产必须关断。$Why"
        return
    }
    Add-Result -Id $Id -Status 'PASS' -Target $target -Detail "开关为 '$($r.Value)'(关)。"
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
        Add-Result -Id 'secret.gate.consistency' -Status 'FAIL' -Target 'GateTokenSecret (跨 5 个文件)' `
            -Detail "共出现 $($distinct.Count) 个不同取值,gate 验签会整片 fail。涉及文件: $(($gateSecretValues.Keys | Sort-Object) -join ', ')"
    }
    else {
        Add-Result -Id 'secret.gate.consistency' -Status 'PASS' -Target 'GateTokenSecret (跨 5 个文件)' -Detail "$($gateSecretValues.Count) 处取值一致。"
    }
}
else {
    Add-Result -Id 'secret.gate.consistency' -Status 'FAIL' -Target 'GateTokenSecret (跨 5 个文件)' `
        -Detail "只读到 $($gateSecretValues.Count) 处可比对的取值,无法完成一致性比对,按 fail-safe 判 FAIL。"
}

# Admin API key(Java Gateway 管理面)
Test-SecretValue -Id 'secret.admin.apikey' -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'admin.api-key' -MinLength 16 | Out-Null

# ─────────────────────────────────────────────────────────────────
# B. MySQL / Redis 密码
# ─────────────────────────────────────────────────────────────────

Test-SecretValue -Id 'secret.mysql.db'      -RelativePath 'go/db/etc/db.yaml'                                     -KeyPath 'ServerConfig.Database.Passwd' -MinLength 12 | Out-Null
Test-SecretValue -Id 'secret.mysql.gateway' -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.password'    -MinLength 12 | Out-Null

Test-SecretValue -Id 'secret.redis.db'      -RelativePath 'go/db/etc/db.yaml'                     -KeyPath 'ServerConfig.RedisClient.Password' -MinLength 12 | Out-Null
Test-SecretValue -Id 'secret.redis.locator' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'RedisClient.Password'          -MinLength 12 | Out-Null
Test-SecretValue -Id 'secret.redis.login'   -RelativePath 'go/login/etc/login.yaml'                -KeyPath 'Node.RedisClient.Password'        -MinLength 12 | Out-Null

# ─────────────────────────────────────────────────────────────────
# C. 镜像 tag 不可变
# ─────────────────────────────────────────────────────────────────

if ([string]::IsNullOrWhiteSpace($ImageTag) -and $ImageRef.Count -eq 0) {
    # fail-safe:没人告诉我要发什么 tag,不能当"没问题"
    Add-Result -Id 'image.tag.provided' -Status 'FAIL' -Target '-ImageTag / -ImageRef' `
        -Detail "调用方没有传入待发布的镜像 tag,无法判定其可变性,按 fail-safe 判 FAIL。"
}
else {
    Add-Result -Id 'image.tag.provided' -Status 'PASS' -Target '-ImageTag / -ImageRef' -Detail "已传入待校验的镜像标识。"
}

# 可变 tag 在 dev 只是提醒(本地 minikube 天天用 latest);
# staging/prod 是硬阻断 —— 三级回滚的前提就是新旧 revision 指向不同 digest。
$ImageTagSeverity = if ($ReleaseProfile -eq 'dev') { 'WARN' } else { 'FAIL' }

if (-not [string]::IsNullOrWhiteSpace($ImageTag)) {
    $chk = Test-ImmutableImageTag -Tag $ImageTag -RejectDirty:$RejectDirtyTag
    if ($chk.Ok) {
        Add-Result -Id 'image.tag.immutable' -Status 'PASS' -Target "tag=$ImageTag" -Detail "不可变 tag,rollout undo 能真的换 digest。"
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
        Add-Result -Id 'image.ref.immutable' -Status 'PASS' -Target $ref -Detail "tag='$tag' 不可变。"
    }
    else {
        Add-Result -Id 'image.ref.immutable' -Status $ImageTagSeverity -Target $ref -Detail $chk.Reason
    }
}

# ─────────────────────────────────────────────────────────────────
# D. Kafka broker 数下界
# ─────────────────────────────────────────────────────────────────

$kafkaWhy = "单 broker 无 ISR 冗余,broker 一挂就是全区停写(release-checklist §F.B-2)。"
Test-ListFloor -Id 'kafka.brokers.cpp'      -RelativePath 'bin/etc/base_deploy_config.yaml'            -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.login'    -RelativePath 'go/login/etc/login.yaml'                    -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.db'       -RelativePath 'go/db/etc/db.yaml'                          -KeyPath 'ServerConfig.Kafka.Brokers'  -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity
Test-ListFloor -Id 'kafka.brokers.locator'  -RelativePath 'go/player_locator/etc/player_locator.yaml'  -KeyPath 'Kafka.Brokers'               -Floor $MinKafkaBrokers -Why $kafkaWhy -Severity $SecretSeverity

# login / db 的 PartitionCnt 必须完全一致(启动门禁 fail-closed,见 db.yaml 注释)
$loginPart = Get-YamlScalar -Path (Resolve-RepoPath 'go/login/etc/login.yaml') -KeyPath 'Kafka.PartitionCnt'
$dbPart = Get-YamlScalar -Path (Resolve-RepoPath 'go/db/etc/db.yaml') -KeyPath 'ServerConfig.Kafka.PartitionCnt'
if (-not $loginPart.Found -or -not $dbPart.Found) {
    Add-Result -Id 'kafka.partition.match' -Status 'FAIL' -Target 'Kafka.PartitionCnt (login vs db)' `
        -Detail "至少一侧查不到,按 fail-safe 判 FAIL。login: $($loginPart.Reason) db: $($dbPart.Reason)"
}
elseif ($loginPart.Value -ne $dbPart.Value) {
    Add-Result -Id 'kafka.partition.match' -Status 'FAIL' -Target 'Kafka.PartitionCnt (login vs db)' `
        -Detail "login=$($loginPart.Value) db=$($dbPart.Value) 不一致,db 启动门禁会 fail-closed。"
}
else {
    Add-Result -Id 'kafka.partition.match' -Status 'PASS' -Target 'Kafka.PartitionCnt (login vs db)' -Detail "两侧均为 $($loginPart.Value)。"
}

# ─────────────────────────────────────────────────────────────────
# E. player_locator LeaseTTL 下界
# ─────────────────────────────────────────────────────────────────

Test-NumericFloor -Id 'locator.lease.disconnect' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Lease.DefaultTTLSeconds' `
    -Floor $MinLeaseTtlSeconds -Why "断线重连窗口就是这个值,小于 30s 会让 30s 重连约定失效(release-checklist §A.2)。"

Test-NumericFloor -Id 'locator.lease.etcd' -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Node.LeaseTTL' `
    -Floor $MinLeaseTtlSeconds -Why "etcd lease TTL(秒),过小会让节点在 GC 抖动时被误判下线。"

# ─────────────────────────────────────────────────────────────────
# F. 限流默认值
# ─────────────────────────────────────────────────────────────────

$rlPath = 'java/gateway_node/src/main/resources/application.yaml'
$rlFull = Resolve-RepoPath -Relative $rlPath

Test-NumericFloor -Id 'ratelimit.zone.rps'   -RelativePath $rlPath -KeyPath 'gate.rate-limit.zone-default-rps'   -Floor 1 -Why "限流阈值 <= 0 等于把闸门焊死,开服直接全量 429。"
Test-NumericFloor -Id 'ratelimit.zone.burst' -RelativePath $rlPath -KeyPath 'gate.rate-limit.zone-default-burst' -Floor 1 -Why "同上。"
Test-NumericFloor -Id 'ratelimit.ip.rps'     -RelativePath $rlPath -KeyPath 'gate.rate-limit.ip-rps'             -Floor 1 -Why "同上。"
Test-NumericFloor -Id 'ratelimit.queue.timeout' -RelativePath $rlPath -KeyPath 'gate.rate-limit.queue-timeout-ms' -Floor 1000 -Why "排队超时过小会把正常排队的玩家直接踢掉。"

$rps = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.zone-default-rps'
$burst = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.zone-default-burst'
if (-not $rps.Found -or -not $burst.Found) {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit" -Detail "rps/burst 至少一侧查不到,按 fail-safe 判 FAIL。"
}
elseif ([double]$burst.Value -lt [double]$rps.Value) {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit" -Detail "burst=$($burst.Value) < rps=$($rps.Value),令牌桶配置自相矛盾。"
}
else {
    Add-Result -Id 'ratelimit.burst.ge.rps' -Status 'PASS' -Target "${rlPath}:gate.rate-limit" -Detail "burst=$($burst.Value) >= rps=$($rps.Value)。"
}

# enabled 本身是灰度决策(§C T+0 要求默认关),这里只提醒不阻断;
# 但键必须存在 —— 缺键意味着走 Java 侧默认值,那是没人决策过的状态。
$rlEnabled = Get-YamlScalar -Path $rlFull -KeyPath 'gate.rate-limit.enabled'
if (-not $rlEnabled.Found) {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'FAIL' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "键不存在,限流开关必须是显式决策,按 fail-safe 判 FAIL。"
}
elseif ($rlEnabled.Value.Trim().ToLowerInvariant() -ne 'true') {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'WARN' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "当前为 '$($rlEnabled.Value)'。灰度 T+0 按 checklist §C 就该是关的;开服日必须先开(§D.1 波次)。"
}
else {
    Add-Result -Id 'ratelimit.enabled.declared' -Status 'PASS' -Target "${rlPath}:gate.rate-limit.enabled" -Detail "限流已开。"
}

# ─────────────────────────────────────────────────────────────────
# G. debug / GM 开关关断
# ─────────────────────────────────────────────────────────────────

# show-sql 会把每条 SQL 打进日志,生产开着既是性能问题也是数据泄漏面
Test-SwitchOff -Id 'debug.jpa.showsql' -RelativePath $rlPath -KeyPath 'spring.jpa.show-sql' -MissingPolicy 'fail' `
    -Why "JPA show-sql 会把全部 SQL(含参数)打进日志。"

# DevPasswordAuth 是"绕过账号库口令体系"的开发后门。
# go/login/internal/config/config.go:30 是 optional,零值 Enabled=false,
# 所以键缺失 = 关断,这里用 pass-off。
Test-SwitchOff -Id 'debug.login.devpassword' -RelativePath 'go/login/etc/login.yaml' -KeyPath 'DevPasswordAuth.Enabled' -MissingPolicy 'pass-off' `
    -Why "DevPasswordAuth 绕过账号库口令体系,只允许 dev/test。"

# Actuator 暴露面
$actuator = Get-YamlScalar -Path $rlFull -KeyPath 'management.endpoints.web.exposure.include'
if (-not $actuator.Found) {
    Add-Result -Id 'debug.actuator.exposure' -Status 'FAIL' -Target "${rlPath}:management.endpoints.web.exposure.include" `
        -Detail "键不存在,Actuator 暴露面必须是显式决策(Spring 默认会暴露 health/info 之外的端点),按 fail-safe 判 FAIL。"
}
else {
    $dangerous = @('*', 'env', 'heapdump', 'threaddump', 'configprops', 'beans', 'shutdown', 'loggers')
    $items = @($actuator.Value -split ',' | ForEach-Object { $_.Trim().ToLowerInvariant() } | Where-Object { $_ })
    $hit = @($items | Where-Object { $dangerous -contains $_ })
    if ($hit.Count -gt 0) {
        Add-Result -Id 'debug.actuator.exposure' -Status 'FAIL' -Target "${rlPath}:management.endpoints.web.exposure.include" `
            -Detail "暴露了危险端点: $($hit -join ', ')。生产只应暴露 health/info。"
    }
    else {
        Add-Result -Id 'debug.actuator.exposure' -Status 'PASS' -Target "${rlPath}:management.endpoints.web.exposure.include" -Detail "仅暴露 '$($actuator.Value)'。"
    }
}

# C++ 节点日志级别:0=DEBUG。生产开 DEBUG 会把热点路径打爆(见 base_deploy_config.yaml 注释)
Test-NumericFloor -Id 'debug.cpp.loglevel' -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'LogLevel' -Floor 1 `
    -Why "0=DEBUG,生产开着会在 AOI/移动热点路径上产生海量日志。"

# ─────────────────────────────────────────────────────────────────
# H. 制品与版本(仅在给了 -ReleaseVersion 时检查)
# ─────────────────────────────────────────────────────────────────

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
    Add-Result -Id 'release.version' -Status 'PASS' -Target $versionTarget -Detail "合法的发布版本号。"

    # 版本号四处一致之一:CHANGELOG 段落(与 make_release.ps1 同口径,找不到或为空都算失败)
    $changelog = Test-ChangelogReleaseSection -ChangelogPath (Join-Path $RepoRoot 'CHANGELOG.md') -Version $ReleaseVersion
    $changelogTarget = "CHANGELOG.md $($changelog.Heading)"
    if ($changelog.Ok) {
        Add-Result -Id 'release.changelog' -Status 'PASS' -Target $changelogTarget -Detail "段落存在且非空。"
    }
    else {
        Add-Result -Id 'release.changelog' -Status 'FAIL' -Target $changelogTarget -Detail $changelog.Reason
    }

    # 制品库按需加载:不给 -ReleaseVersion 的既有调用方不依赖它
    $libPath = Join-Path $ScriptDir 'lib' 'artifacts_lib.ps1'
    if (-not (Test-Path -LiteralPath $libPath -PathType Leaf)) {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target $libPath -Detail "制品库脚本缺失,无法定位与校验制品,按 fail-safe 判 FAIL。"
        return
    }
    . $libPath

    try {
        $channelRoot = Get-ChannelRoot -Channel release -Override $ArtifactRoot
    }
    catch {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target "-ArtifactRoot=$ArtifactRoot" -Detail "解析制品根失败:$($_.Exception.Message)"
        return
    }

    $versionDir = Join-Path $channelRoot 'images' $ReleaseVersion
    $manifestPath = Join-Path $channelRoot 'manifests' "$ReleaseVersion.json"
    $buildInfo = $null

    if (-not [System.IO.Directory]::Exists($versionDir)) {
        Add-Result -Id 'release.artifact.dir' -Status 'FAIL' -Target $versionDir `
            -Detail "版本目录不存在:还没用 publish_images.ps1 -Version $ReleaseVersion 发布过,或 -ArtifactRoot / MMORPG_ARTIFACT_ROOT 指错了地方。"
    }
    else {
        Add-Result -Id 'release.artifact.dir' -Status 'PASS' -Target $versionDir -Detail "版本目录存在。"

        try {
            Test-Sha256Sums -Dir $versionDir | Out-Null
            Add-Result -Id 'release.artifact.sha256sums' -Status 'PASS' -Target $versionDir -Detail "目录内文件与 sha256sums.txt 完全一致。"
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
            Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.json 缺失或不是合法 JSON:$($_.Exception.Message)"
        }

        if ($null -ne $buildInfo) {
            # app_version 接受带 v 与不带 v 两种写法:两者指同一个版本,不存在歧义
            $appVersion = [string]$buildInfo.app_version
            if ([string]::IsNullOrWhiteSpace($appVersion)) {
                Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.app_version 为空,无法证明这份制品就是 $ReleaseVersion。"
            }
            elseif ($appVersion -cne $ReleaseVersion -and $appVersion -cne $versionCheck.Normalized) {
                Add-Result -Id 'release.buildinfo.version' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.app_version='$appVersion' 与 -ReleaseVersion=$ReleaseVersion 不一致:版本目录名与制品内容对不上。"
            }
            else {
                Add-Result -Id 'release.buildinfo.version' -Status 'PASS' -Target $buildInfoPath -Detail "app_version='$appVersion'。"
            }

            $dirty = $buildInfo.dirty
            if ($dirty -is [bool] -and -not $dirty) {
                Add-Result -Id 'release.buildinfo.clean' -Status 'PASS' -Target $buildInfoPath -Detail "dirty=false。"
            }
            else {
                Add-Result -Id 'release.buildinfo.clean' -Status 'FAIL' -Target $buildInfoPath -Detail "build-info.dirty='$dirty'(缺失或不是 false 一律拒):脏树产物无法从 commit 还原,不能发布。"
            }

            # 上面 A~G 节读的是**当前工作树**的配置文件。工作树不在制品 commit 上时,那些结论不代表
            # 要发布的版本 —— 只提醒不阻断(预检也会在别的 commit 上被用来"看一眼")。
            $artifactCommit = [string]$buildInfo.commit
            $stamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
            if (-not $stamp.Ok) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "读不到当前工作树 commit($($stamp.Reason)),无法确认上面的配置检查对应该版本。"
            }
            elseif ($artifactCommit -cne $stamp.Commit) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "制品 commit='$artifactCommit',当前工作树 HEAD=$($stamp.Commit):上面的配置检查读的是当前工作树,结论不代表该版本。请在发布 commit 上重跑预检。"
            }
            elseif ($stamp.Dirty) {
                Add-Result -Id 'release.buildinfo.commit' -Status 'WARN' -Target $buildInfoPath -Detail "当前工作树有未提交改动:上面的配置检查读到的可能不是该版本提交的配置。"
            }
            else {
                Add-Result -Id 'release.buildinfo.commit' -Status 'PASS' -Target $buildInfoPath -Detail "制品 commit 与当前工作树 HEAD 一致且工作树干净。"
            }
        }
    }

    # manifest JSON 是 make_release.ps1 最后才写的"已发布"哨兵:存在即说明 release 记录完整落盘
    if (-not [System.IO.File]::Exists($manifestPath)) {
        Add-Result -Id 'release.manifest' -Status 'FAIL' -Target $manifestPath -Detail "release manifest 不存在:还没跑 make_release.ps1 -Version $ReleaseVersion,或它中途失败。"
    }
    else {
        $manifestText = [System.IO.File]::ReadAllText($manifestPath)
        $manifestOk = $false
        $manifestReason = "文件为空。"
        if (-not [string]::IsNullOrWhiteSpace($manifestText)) {
            try {
                $manifestText | ConvertFrom-Json | Out-Null
                $manifestOk = $true
            }
            catch {
                $manifestReason = "不是合法 JSON:$($_.Exception.Message)"
            }
        }
        if ($manifestOk) {
            Add-Result -Id 'release.manifest' -Status 'PASS' -Target $manifestPath -Detail "manifest 存在且是合法 JSON。"
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
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "版本发布的镜像 tag 必须形如 $ReleaseVersion-<12 位小写 commit>(Get-ReleaseImageTag 口径)。"
        }
        elseif ($null -eq $buildInfo) {
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "build-info.json 不可读,无法核对 tag 里的 commit,按 fail-safe 判 FAIL。"
        }
        elseif ($tagMatch.Groups[1].Value -cne [string]$buildInfo.commit) {
            Add-Result -Id 'release.imagetag.commit' -Status 'FAIL' -Target $tagTarget -Detail "tag 里的 commit=$($tagMatch.Groups[1].Value) 与 build-info.commit='$($buildInfo.commit)' 不一致:要部署的镜像不是这份制品。"
        }
        else {
            Add-Result -Id 'release.imagetag.commit' -Status 'PASS' -Target $tagTarget -Detail "tag 的 commit 与 build-info.commit 一致。"
        }
    }
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
    Write-Host "release preflight FAILED: $failCount 项不达标,发布被阻断。" -ForegroundColor Red
    Write-Host "密钥类问题请注入环境变量后重跑(见 tools/scripts/lib/release_common.ps1 Resolve-InjectedSecret)。" -ForegroundColor Red
    if (-not $NoFailExit) {
        exit 1
    }
}
else {
    Write-Host "release preflight PASSED (warn=$warnCount)。" -ForegroundColor Green
}

exit 0
