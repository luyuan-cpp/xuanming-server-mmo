#requires -Version 7
<#
.SYNOPSIS
    契约测试用的采集工具:跑 k8s_deploy.ps1 -DryRun,把它打印的 manifest 抓回来解析。

.DESCRIPTION
    k8s_deploy.ps1 的产物不入库(每次 apply 现生成),所以契约测试只能通过
    -DryRun 这一条缝去看它到底生成了什么。Invoke-KubectlWithInputFile 在
    DryRun 下会把完整 YAML 夹在 `--- BEGIN MANIFEST ---` / `--- END MANIFEST ---`
    之间打出来,这里就靠这对标记切块。
#>

Set-StrictMode -Off

# $PSScriptRoot 在被 dot-source 的文件里指向**它自己**所在目录(tools/scripts/tests/lib),
# 所以往上两级才是 tools/scripts。
$script:ToolsScriptsDir = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)

# 仓库根 = tools/scripts 再往上两级。刻意逐级 Split-Path 上溯,而不是拼 "..\..":
# 反斜杠在 Linux 上是普通字符不是路径分隔符,而 .github/workflows/deploy-config-tests.yml
# 把本套测试跑在 ubuntu-latest 上(该 workflow 的注释也明确要求"别写反斜杠字面量")。
$script:RepoRootPath = (Resolve-Path (Split-Path -Parent (Split-Path -Parent $script:ToolsScriptsDir))).Path

. (Join-Path $script:ToolsScriptsDir "lib" "release_common.ps1")

function Get-RepoRoot { return $script:RepoRootPath }
function Get-ToolsScriptsDir { return $script:ToolsScriptsDir }

<#
.SYNOPSIS
    写者计划夹具,返回 catalog 文件路径(给环境变量 MMORPG_SQL_K8S_BOOT_CATALOG)。k8s_deploy.ps1 渲染 SQL 写者(db、data-service、login ……)时要它。

.DESCRIPTION
    只供 DryRun 渲染:roster 是两条空 entry,过得了部署脚本的容量校验,真正的 registrar 不认。文件写在 -Root 下,由调用方清理。
#>
function New-SqlWriterCatalogFixture {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][string[]]$Namespaces
    )

    $null = [System.IO.Directory]::CreateDirectory($Root)
    $rosterPath = Join-Path $Root 'roster.json'
    [System.IO.File]::WriteAllText($rosterPath, '{"format":"SQL_WRITER_BOOT_ROSTER_V1","entries":[{},{}]}', [System.Text.UTF8Encoding]::new($false))
    $rosterSha = (Get-FileHash -LiteralPath $rosterPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $catalog = @{}
    foreach ($namespace in $Namespaces) {
        foreach ($service in @('login', 'gateway', 'data-service', 'db', 'chat', 'friend', 'guild', 'mail', 'match', 'player-inbox', 'rank', 'trade')) {
            $catalog["$namespace/$service"] = @{
                requestSecret = 'fixture-request'; registrarSecret = 'fixture-registrar'; registrarImage = ('fixture.invalid/registrar@sha256:' + ('a' * 64))
                physicalAuthoritySha256 = ('b' * 64); runtimeConfigSecret = 'fixture-runtime'; replicas = 2; rosterFile = $rosterPath; rosterSha256 = $rosterSha
                schemaConfigSecret = 'fixture-schema-config'; schemaPermissionSecret = 'fixture-schema-permission'
            }
        }
    }
    $catalogPath = Join-Path $Root 'catalog.json'
    [System.IO.File]::WriteAllText($catalogPath, ($catalog | ConvertTo-Json -Depth 8 -Compress), [System.Text.UTF8Encoding]::new($false))
    return $catalogPath
}

<#
.SYNOPSIS
    给跑 k8s_deploy.ps1 的子进程补玩家域 Redis Cluster 的参数,返回 @{ Arguments; Env }。

.DESCRIPTION
    staging / prod 必须显式给 -PlayerRedisMasters,要 apply 集群时还要容量,开了集群还要口令(docs/design/redis-player-store-cluster.md §5.1)。
    绝大多数用例测的不是这条闸,不补的话全部非 dev 档的 DryRun 都停在这里。
    用例自己给了 -PlayerRedisMasters(含 0,测闸本身)就原样不动;-Env 写了 MMORPG_PLAYER_REDIS_PASSWORD(含空串)就用它。
    dev 档不补:dev 缺省不建集群,这是要钉住的行为。
#>
function Add-PlayerRedisDeployFixture {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][hashtable]$Env
    )

    $result = @{ Arguments = $Arguments; Env = $Env }
    $profileAt = [array]::IndexOf($Arguments, '-ReleaseProfile')
    if ($profileAt -lt 0 -or ($profileAt + 1) -ge $Arguments.Count -or $Arguments[$profileAt + 1] -notin @('staging', 'prod')) { return $result }
    if ($Arguments -contains '-PlayerRedisMasters') { return $result }
    $added = @('-PlayerRedisMasters', '3')
    # 容量只在本次 apply 集群时生效,zone-up 带上它只会多一条 NoEffect 警告。
    if (($Arguments -contains 'infra-up') -or ($Arguments -contains 'all-up')) { $added += @('-PlayerRedisMaxMemory', '1gb', '-PlayerRedisStorage', '8Gi') }
    $result.Arguments = @($Arguments) + $added
    if (-not $Env.ContainsKey('MMORPG_PLAYER_REDIS_PASSWORD')) {
        $result.Env = @{} + $Env
        $result.Env['MMORPG_PLAYER_REDIS_PASSWORD'] = 'contract-player-redis-pw-01'
    }
    return $result
}

<#
.SYNOPSIS
    以子进程跑一个 pwsh 脚本,合并捕获 stdout+stderr,返回 @{ ExitCode; Output }。

.DESCRIPTION
    **为什么要在这里管编码**:本套测试的负向用例断的是子进程打出来的**中文错误
    文本**(见测试头注释:只断退出码等于没测)。而 PowerShell 捕获原生进程输出时,
    是按 [Console]::OutputEncoding 解码的,子进程也按同一个控制台码页编码 ——
    Windows 运维机/CI 容器的控制台默认是 ibm437、GBK 这类 OEM 码页,中文在这一
    编一解里会被整体打成 '?',于是所有断中文的用例集体变红。红的是编码链路,
    不是被测脚本:同一套断言在 UTF-8 控制台下全绿,在 ibm437 控制台下 6 条红。

    所以这里在捕获期间把控制台编码顶成 UTF-8(子进程继承同一个控制台码页),
    跑完立刻还原,不污染调用方终端。

    **子进程一律 -NonInteractive**:测试从不给子进程任何交互输入。被测脚本缺必填参数
    (例如 lib 内部把 `-Reason'…'` 写成位置参数)、或走到 Read-Host 这类提示时,
    交互模式会停在提示上等输入,把整套测试(含 CI)挂死;非交互模式下直接报错、非 0 退出,
    变成一条带原因的红用例。

.PARAMETER Env
    要在子进程里设置的环境变量(hashtable)。跑完自动还原,避免污染同一轮里的其它用例。
#>
function Invoke-CapturedPwsh {
    param(
        [Parameter(Mandatory = $true)][string]$ScriptPath,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{}
    )

    # 部署脚本(以及照抄它 param 块的驱动)在 staging / prod 要带玩家域集群参数,见 Add-PlayerRedisDeployFixture。
    $scriptLeaf = [System.IO.Path]::GetFileName($ScriptPath)
    if ($scriptLeaf -ceq 'k8s_deploy.ps1' -or $scriptLeaf -like 'mmorpg-client-entry-dryrun-*') {
        $fixture = Add-PlayerRedisDeployFixture -Arguments $Arguments -Env $Env
        $Arguments = $fixture.Arguments
        $Env = $fixture.Env
    }

    $savedEnv = @{}
    foreach ($k in $Env.Keys) {
        $savedEnv[$k] = [System.Environment]::GetEnvironmentVariable($k)
        [System.Environment]::SetEnvironmentVariable($k, $Env[$k])
    }

    # 输出被完全重定向、进程没有真实控制台时,读写 [Console]::OutputEncoding 会抛。
    # 那种场景下保持默认即可(Linux pwsh 本来就是 UTF-8),不能让"拿不到控制台"
    # 反过来把测试打红。
    $savedConsoleEncoding = $null
    try {
        $savedConsoleEncoding = [Console]::OutputEncoding
        [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
    }
    catch {
        $savedConsoleEncoding = $null
    }

    try {
        # 终止异常按原始 Message 输出,不让子进程的 ConciseView 按控制台宽度插入换行和 `|`。
        # 驱动同时保留显式 exit N,负向测试仍检查真实退出码和完整原因。
        $captureDriver = Join-Path $PSScriptRoot 'captured_script_driver.ps1'
        $output = & pwsh -NoProfile -NonInteractive -File $captureDriver $ScriptPath @Arguments 2>&1 | Out-String
        return @{ ExitCode = $LASTEXITCODE; Output = $output }
    }
    finally {
        if ($null -ne $savedConsoleEncoding) {
            try { [Console]::OutputEncoding = $savedConsoleEncoding } catch { }
        }
        foreach ($k in $Env.Keys) {
            [System.Environment]::SetEnvironmentVariable($k, $savedEnv[$k])
        }
    }
}

<#
.SYNOPSIS
    以子进程跑 k8s_deploy.ps1,返回 @{ ExitCode; Output }。
#>
function Invoke-DeployDryRun {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{}
    )

    return Invoke-CapturedPwsh -ScriptPath (Join-Path $script:ToolsScriptsDir "k8s_deploy.ps1") -Arguments $Arguments -Env $Env
}

<#
.SYNOPSIS
    以子进程跑 tools/scripts 下任意脚本,返回 @{ ExitCode; Output }。
#>
function Invoke-ToolScript {
    param(
        [Parameter(Mandatory = $true)][string]$ScriptName,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{}
    )

    return Invoke-CapturedPwsh -ScriptPath (Join-Path $script:ToolsScriptsDir $ScriptName) -Arguments $Arguments -Env $Env
}

<#
.SYNOPSIS
    从 DryRun 输出里切出所有 manifest 文本块。
#>
function Get-ManifestBlocks {
    param([Parameter(Mandatory = $true)][string]$Output)

    $blocks = New-Object System.Collections.Generic.List[string]
    $lines = $Output -split "`r?`n"
    $current = $null
    foreach ($line in $lines) {
        if ($line.Trim() -eq '--- BEGIN MANIFEST ---') {
            $current = New-Object System.Collections.Generic.List[string]
            continue
        }
        if ($line.Trim() -eq '--- END MANIFEST ---') {
            if ($null -ne $current) { $blocks.Add(($current -join "`n")) }
            $current = $null
            continue
        }
        if ($null -ne $current) { $current.Add($line) }
    }
    return $blocks
}

<#
.SYNOPSIS
    找出 metadata.name 等于指定值的那个 manifest 块。找不到返回 $null。
#>
function Select-ManifestByName {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$Name
    )

    foreach ($block in (Get-ManifestBlocks -Output $Output)) {
        if ($block -match "(?m)^\s*name:\s*$([regex]::Escape($Name))\s*$") {
            return $block
        }
    }
    return $null
}

<#
.SYNOPSIS
    把 manifest 块拍平成 "点分路径 -> 值"。ConfigMap 里内嵌的配置文件
    (`data: <file.yaml>: |`)会以 `data.<file.yaml>.<原路径>` 的形式出现。
#>
function ConvertTo-FlatManifest {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Block)

    # fail-closed:块拿不到就直接报清楚,不能让后续断言在空字符串上"顺利通过"
    if ([string]::IsNullOrWhiteSpace($Block)) {
        throw "manifest 块为空 —— DryRun 输出里没找到目标 ConfigMap/Deployment(视为契约破坏)"
    }
    return (ConvertFrom-YamlToFlatMap -Text $Block)
}

<#
.SYNOPSIS
    从拍平后的 manifest 里取值,取不到抛异常(fail-closed:测试不能因为"没找到"而静默通过)。
#>
function Get-FlatValue {
    param(
        [Parameter(Mandatory = $true)]$Flat,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    if (-not $Flat.Scalars.Contains($KeyPath)) {
        throw "生成的 manifest 里查不到 '$KeyPath'(fail-closed:视为契约破坏,不是跳过)"
    }
    return [string]$Flat.Scalars[$KeyPath]
}

<#
.SYNOPSIS
    读服务侧 etc/*.yaml 的权威值,取不到抛异常。
#>
function Get-EtcValue {
    param(
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    $full = Join-Path $script:RepoRootPath ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
    $r = Get-YamlScalar -Path $full -KeyPath $KeyPath
    if (-not $r.Found) { throw "读不到权威值 ${RelativePath}:${KeyPath} —— $($r.Reason)" }
    return $r.Value
}
