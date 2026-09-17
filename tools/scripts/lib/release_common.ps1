#requires -Version 7
<#
.SYNOPSIS
    发布/部署工具链的公共函数库(纯 PowerShell,零外部依赖)。

.DESCRIPTION
    被 release_preflight.ps1 / k8s_image.ps1 / k8s_deploy.ps1 / dev_tools.ps1
    以及 tools/scripts/tests/ 下的契约测试 dot-source 引用。

    这里只放三类东西,别往里塞业务逻辑:
      1. YAML 标量/列表的只读提取(不引入 powershell-yaml 模块,离线机器上装不了)
      2. 镜像版本戳(git 短 sha + 脏树标记)与"tag 是否可变"的判定
      3. 占位密钥识别 + 从环境变量取密钥的 fail-closed 包装

    **fail-safe 原则**:所有"查配置"的函数在文件不存在 / 键查不到时
    返回 Found=$false,由调用方判 FAIL —— 绝不能把"查不到"当成"通过"。
#>

Set-StrictMode -Off

# ─────────────────────────────────────────────────────────────────
# 1. YAML 只读提取
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
    去掉 YAML 行尾注释,同时不误伤引号内的 '#'。
#>
function Remove-YamlInlineComment {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

    $text = $Value
    $inSingle = $false
    $inDouble = $false
    for ($i = 0; $i -lt $text.Length; $i++) {
        $ch = $text[$i]
        if ($ch -eq "'" -and -not $inDouble) { $inSingle = -not $inSingle; continue }
        if ($ch -eq '"' -and -not $inSingle) { $inDouble = -not $inDouble; continue }
        if ($ch -eq '#' -and -not $inSingle -and -not $inDouble) {
            # 只有 '#' 前面是行首或空白时才算注释,避免切掉 "Mmorpg#2026db" 这种密码
            if ($i -eq 0 -or [char]::IsWhiteSpace($text[$i - 1])) {
                return $text.Substring(0, $i)
            }
        }
    }
    return $text
}

<#
.SYNOPSIS
    去掉 YAML 标量两侧的引号。
#>
function ConvertFrom-YamlScalarLiteral {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

    $v = $Value.Trim()
    if ($v.Length -ge 2) {
        if (($v[0] -eq '"' -and $v[-1] -eq '"') -or ($v[0] -eq "'" -and $v[-1] -eq "'")) {
            return $v.Substring(1, $v.Length - 2)
        }
    }
    return $v
}

<#
.SYNOPSIS
    把 YAML 文本按缩进展开成 "点分路径 -> 值" 的有序表。

.DESCRIPTION
    只支持项目 etc/*.yaml 实际用到的子集:块式映射 + 块式序列 + 行内标量。
    序列元素以 `<父路径>[i]` 记录,同时 `<父路径>` 记一条 Count 供计数用。

    返回:@{ Scalars = @{path=value}; ListCounts = @{path=count} }
#>
function ConvertFrom-YamlToFlatMap {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

    $scalars = [ordered]@{}
    $listCounts = [ordered]@{}

    # 栈元素:@{ Indent = <int>; Key = <string> }
    $stack = New-Object System.Collections.Generic.List[object]

    $lines = $Text -split "`r?`n"
    foreach ($rawLine in $lines) {
        if ([string]::IsNullOrWhiteSpace($rawLine)) { continue }

        # tab 在 YAML 里非法,但生成器里用 tab 当缩进再统一替换,这里同样先归一
        $line = $rawLine -replace "`t", "    "
        $stripped = Remove-YamlInlineComment -Value $line
        if ([string]::IsNullOrWhiteSpace($stripped)) { continue }

        $indent = $stripped.Length - $stripped.TrimStart(' ').Length
        $content = $stripped.Trim()
        if ($content.StartsWith('---') -or $content.StartsWith('...')) { continue }

        if ($content.StartsWith('- ') -or $content -eq '-') {
            # 序列元素:父路径 = 当前栈顶路径
            while ($stack.Count -gt 0 -and $stack[$stack.Count - 1].Indent -ge $indent) {
                $stack.RemoveAt($stack.Count - 1)
            }
            if ($stack.Count -eq 0) { continue }
            $parentPath = ($stack | ForEach-Object { $_.Key }) -join '.'
            $idx = 0
            if ($listCounts.Contains($parentPath)) { $idx = [int]$listCounts[$parentPath] }
            $itemValue = ConvertFrom-YamlScalarLiteral -Value ($content.Substring(1).Trim())
            $scalars["$parentPath[$idx]"] = $itemValue
            $listCounts[$parentPath] = $idx + 1
            continue
        }

        $m = [regex]::Match($content, '^(?<k>[^:]+):\s*(?<v>.*)$')
        if (-not $m.Success) { continue }

        $key = (ConvertFrom-YamlScalarLiteral -Value $m.Groups['k'].Value)
        $value = $m.Groups['v'].Value.Trim()

        while ($stack.Count -gt 0 -and $stack[$stack.Count - 1].Indent -ge $indent) {
            $stack.RemoveAt($stack.Count - 1)
        }
        $stack.Add(@{ Indent = $indent; Key = $key })
        $path = ($stack | ForEach-Object { $_.Key }) -join '.'

        if (-not [string]::IsNullOrWhiteSpace($value)) {
            $scalars[$path] = (ConvertFrom-YamlScalarLiteral -Value $value)
        }
        elseif (-not $scalars.Contains($path)) {
            # 值为空的键(下面是子块或空串),记一条空值以便区分"键不存在"
            $scalars[$path] = ''
        }
    }

    return @{ Scalars = $scalars; ListCounts = $listCounts }
}

<#
.SYNOPSIS
    从 YAML 文件里取一个标量。文件不存在或键不存在都返回 Found=$false。

.OUTPUTS
    @{ Found = <bool>; Value = <string>; Reason = <string> }
#>
function Get-YamlScalar {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        return @{ Found = $false; Value = $null; Reason = "文件不存在: $Path" }
    }

    $flat = ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath $Path -Raw)
    if (-not $flat.Scalars.Contains($KeyPath)) {
        return @{ Found = $false; Value = $null; Reason = "键不存在: $KeyPath (@$Path)" }
    }

    return @{ Found = $true; Value = [string]$flat.Scalars[$KeyPath]; Reason = "" }
}

<#
.SYNOPSIS
    从 YAML 文件里取一个块式序列的元素个数(如 Kafka.Brokers)。
#>
function Get-YamlListCount {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        return @{ Found = $false; Value = $null; Reason = "文件不存在: $Path" }
    }

    $flat = ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath $Path -Raw)
    if (-not $flat.ListCounts.Contains($KeyPath)) {
        return @{ Found = $false; Value = $null; Reason = "序列不存在: $KeyPath (@$Path)" }
    }

    return @{ Found = $true; Value = [int]$flat.ListCounts[$KeyPath]; Reason = "" }
}

# ─────────────────────────────────────────────────────────────────
# 2. 镜像版本戳
# ─────────────────────────────────────────────────────────────────

# 明确禁止用于发布的"可变 tag"。它们在 registry 上会被覆盖,
# 一旦覆盖,`kubectl rollout undo` 的新旧 revision 指向同一个 digest,
# 回滚等于什么都没做(docs/ops/release-checklist.md §E.2 三级回滚失效)。
$script:MutableImageTags = @(
    'latest', 'dev', 'develop', 'main', 'master', 'stable', 'edge',
    'prod', 'production', 'release', 'nightly', 'snapshot', 'current'
)

<#
.SYNOPSIS
    取 git 版本戳。工作树脏时 Dirty=$true 且 Tag 带 -dirty 后缀。

.OUTPUTS
    @{ Ok=<bool>; Commit=<string>; Tag=<string>; Dirty=<bool>; Reason=<string> }
#>
function Get-GitReleaseStamp {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot
    )

    $git = Get-Command git -ErrorAction SilentlyContinue
    if ($null -eq $git) {
        return @{ Ok = $false; Commit = ''; Tag = ''; Dirty = $true; Reason = "git 不在 PATH 里,无法生成不可变版本戳" }
    }

    $commit = (& git -C $RepoRoot rev-parse --short=12 HEAD 2>$null)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($commit)) {
        return @{ Ok = $false; Commit = ''; Tag = ''; Dirty = $true; Reason = "git rev-parse 失败(不是 git 仓库?): $RepoRoot" }
    }
    $commit = $commit.Trim()

    $porcelain = (& git -C $RepoRoot status --porcelain 2>$null)
    $dirty = -not [string]::IsNullOrWhiteSpace(($porcelain -join ''))

    $tag = if ($dirty) { "$commit-dirty" } else { $commit }
    return @{ Ok = $true; Commit = $commit; Tag = $tag; Dirty = $dirty; Reason = "" }
}

<#
.SYNOPSIS
    判定一个镜像 tag 是否"不可变"(可用于发布/回滚)。

.OUTPUTS
    @{ Ok = <bool>; Reason = <string> }
#>
function Test-ImmutableImageTag {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Tag,
        # prod 路径连 -dirty 也不许:脏树构建出来的镜像无法从 commit 还原
        [switch]$RejectDirty
    )

    if ([string]::IsNullOrWhiteSpace($Tag)) {
        return @{ Ok = $false; Reason = "镜像 tag 为空" }
    }

    $normalized = $Tag.Trim().ToLowerInvariant()
    if ($script:MutableImageTags -contains $normalized) {
        return @{ Ok = $false; Reason = "镜像 tag '$Tag' 是可变 tag(registry 上会被覆盖),rollout undo 会退到同一个 digest 等于没回滚" }
    }

    if ($RejectDirty -and $normalized.EndsWith('-dirty')) {
        return @{ Ok = $false; Reason = "镜像 tag '$Tag' 来自脏工作树,生产发布不接受(无法从 commit 还原产物)" }
    }

    return @{ Ok = $true; Reason = "" }
}

<#
.SYNOPSIS
    从完整镜像引用里切出 tag(registry:5000/foo 这种端口号不算 tag)。
#>
function Get-ImageTagFromRef {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$ImageRef)

    if ([string]::IsNullOrWhiteSpace($ImageRef)) { return '' }

    # digest 形式 repo@sha256:... 本身就是不可变引用
    $atIdx = $ImageRef.IndexOf('@')
    if ($atIdx -ge 0) { return $ImageRef.Substring($atIdx + 1) }

    $lastColon = $ImageRef.LastIndexOf(':')
    if ($lastColon -lt 0) { return '' }
    $candidate = $ImageRef.Substring($lastColon + 1)
    if ($candidate -match '/') { return '' }
    return $candidate
}

<#
.SYNOPSIS
    按 tag 是否可变推导 imagePullPolicy。

.DESCRIPTION
    不可变 tag 用 IfNotPresent(省带宽,内容不会变);可变 tag 只能 Always,
    否则节点上残留的旧层会让"部署了新版本"变成幻觉。
#>
function Resolve-ImagePullPolicy {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$ImageRef)

    $tag = Get-ImageTagFromRef -ImageRef $ImageRef
    $check = Test-ImmutableImageTag -Tag $tag
    if ($check.Ok) { return 'IfNotPresent' }
    return 'Always'
}

# ── 发布版本号 / 发布镜像 tag / 推送后 digest 回读 ───────────────────
#
# 发布可追溯的前提是"同一个版本号四处一致":git tag = 镜像脚本/publish_images/make_release 的
# -Version = CHANGELOG 段落 = 镜像自报版本。版本号长什么样只在这里定义一次,
# 镜像脚本、release_preflight.ps1、.github/workflows/release.yml 都调它,不各写一份正则(会漂移)。
#
# 必须带小写 v:v1.2.3 / V1.2.3 / 1.2.3 混用时,制品目录名、镜像 tag、git tag 各写各的,
# 事故时按版本号查不到东西。
$script:ReleaseVersionPattern = '^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

# Get-GitReleaseStamp 的 Commit 口径:12 位小写十六进制。用 \z 而不是 $:.NET 的 $ 会放过末尾换行。
$script:ReleaseCommitPattern = '^[0-9a-f]{12}\z'

<#
.SYNOPSIS
    校验发布版本号(vX.Y.Z 或 vX.Y.Z-<预发布标识>)。

.DESCRIPTION
    两处 .NET 正则陷阱都会放过肉眼看不出差别的版本号,这里显式堵上:
      - `$` 允许匹配末尾换行之前 —— 所以含空白的一律先拒;
      - `\d` 默认匹配全角数字等非 ASCII 数字 —— 所以用 ECMAScript 语义(\d 只认 0-9)。
    PowerShell 的 -match 不区分大小写,"V1.2.3" 会被放过,所以不用 -match。

.OUTPUTS
    @{ Ok=<bool>; Reason=<string>; Normalized=<去掉 v 的 X.Y.Z[-pre],失败时为空串> }
#>
function Test-ReleaseVersion {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Version
    )

    if ([string]::IsNullOrWhiteSpace($Version)) {
        return @{ Ok = $false; Reason = "发布版本号为空(应形如 v1.2.3)"; Normalized = '' }
    }
    if ($Version -match '\s') {
        return @{ Ok = $false; Reason = "发布版本号 '$Version' 含空白字符"; Normalized = '' }
    }
    $opts = [System.Text.RegularExpressions.RegexOptions]::ECMAScript
    if (-not [regex]::IsMatch($Version, $script:ReleaseVersionPattern, $opts)) {
        return @{
            Ok         = $false
            Reason     = "发布版本号 '$Version' 不合法:必须以小写 v 开头,形如 vX.Y.Z 或 vX.Y.Z-<预发布标识>,数字段不得有前导 0(如 v1.2.3、v1.2.3-rc.1)"
            Normalized = ''
        }
    }
    return @{ Ok = $true; Reason = ''; Normalized = $Version.Substring(1) }
}

<#
.SYNOPSIS
    按"有无发布版本号"生成镜像 tag。

.DESCRIPTION
    release :<vX.Y.Z>-<12位sha>。版本号给人看,sha 让"同一版本号换了 commit 重打"得到不同 tag,
              registry 上不会出现同名 tag 换内容(那会让 rollout undo 退回同一个 digest)。
    snapshot:<12位sha>;脏树 <12位sha>-dirty(与 Get-GitReleaseStamp.Tag 口径一致)。
    发布版本 + 脏树直接抛:脏树产物无法从 commit 还原,不配挂版本号。

.OUTPUTS
    [string]
#>
function Get-ReleaseImageTag {
    param(
        [AllowEmptyString()][AllowNull()][string]$Version = '',
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Commit,
        [switch]$Dirty
    )

    if (-not [regex]::IsMatch($Commit, $script:ReleaseCommitPattern)) {
        throw "Commit '$Commit' 不是 12 位小写十六进制 git sha(应取 Get-GitReleaseStamp 的 Commit),无法生成可追溯的镜像 tag。"
    }

    if ([string]::IsNullOrWhiteSpace($Version)) {
        if ($Dirty) { return "$Commit-dirty" }
        return $Commit
    }

    $check = Test-ReleaseVersion -Version $Version
    if (-not $check.Ok) { throw $check.Reason }
    if ($Dirty) {
        throw "发布版本 $Version 不接受脏工作树(git status 非空):脏树产物无法从 commit $Commit 还原。请先提交或清理工作树。"
    }

    $tag = "$Version-$Commit"
    # docker tag 上限 128 字符。超长的预发布标识在这里拦住,别等 docker build 报一句英文错
    if ($tag.Length -gt 128) {
        throw "镜像 tag '$tag' 长度 $($tag.Length) 超过 docker 上限 128,请缩短版本号的预发布标识。"
    }
    return $tag
}

<#
.SYNOPSIS
    检查 CHANGELOG 里是否恰有一段非空的 `## [X.Y.Z]`(方括号内不带 v,可带 " - YYYY-MM-DD")。

.DESCRIPTION
    规则与 make_release.ps1 的 Get-ChangelogSection 逐条对齐,让"构建前先查"与"最后出 manifest"
    给出同一个结论,不出现 release.yml 预检过了、跑完几十分钟构建才在 make_release 被拒:
      - 标题精确匹配:找 1.2.3 不命中 [1.2.3-rc.1] / [11.2.3];日期后缀只认 " - YYYY-MM-DD"
      - 同一版本出现两段 -> 失败(取哪一段都可能是错的)
      - 段落到下一个 "## " 为止;链接引用定义行([1.2.3]: https://...)不算正文;正文为空 -> 失败
    改规则要两边一起改。

.OUTPUTS
    @{ Ok=<bool>; Reason=<string>; Heading=<期望的标题,如 "## [1.2.3]"> }
#>
function Test-ChangelogReleaseSection {
    param(
        [Parameter(Mandatory = $true)][string]$ChangelogPath,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Version
    )

    $check = Test-ReleaseVersion -Version $Version
    if (-not $check.Ok) {
        return @{ Ok = $false; Reason = $check.Reason; Heading = '' }
    }
    $heading = "## [$($check.Normalized)]"
    if (-not (Test-Path -LiteralPath $ChangelogPath -PathType Leaf)) {
        return @{ Ok = $false; Reason = "CHANGELOG 不存在:$ChangelogPath"; Heading = $heading }
    }

    $headingPattern = '^## \[' + [regex]::Escape($check.Normalized) + '\](?:\s+-\s+\d{4}-\d{2}-\d{2})?\s*$'
    $lines = [System.IO.File]::ReadAllLines($ChangelogPath, [System.Text.Encoding]::UTF8)
    $headingCount = @($lines | Where-Object { $_ -match $headingPattern }).Count
    if ($headingCount -eq 0) {
        return @{ Ok = $false; Reason = "CHANGELOG 里没有 '$heading' 段(发版前把 [Unreleased] 改成 '$heading - YYYY-MM-DD',方括号里不带 v):$ChangelogPath"; Heading = $heading }
    }
    if ($headingCount -gt 1) {
        return @{ Ok = $false; Reason = "CHANGELOG 里 '$heading' 出现了 $headingCount 段,先合并成一段:$ChangelogPath"; Heading = $heading }
    }

    $inSection = $false
    $hasBody = $false
    foreach ($line in $lines) {
        if (-not $inSection) {
            if ($line -match $headingPattern) { $inSection = $true }
            continue
        }
        if ($line -match '^## ') { break }
        if ($line -match '^\[[^\]]+\]:\s*\S') { continue }
        if (-not [string]::IsNullOrWhiteSpace($line)) { $hasBody = $true }
    }
    if (-not $hasBody) {
        return @{ Ok = $false; Reason = "CHANGELOG 的 '$heading' 段是空的,没有修复内容的版本不发布:$ChangelogPath"; Heading = $heading }
    }
    return @{ Ok = $true; Reason = ''; Heading = $heading }
}

<#
.SYNOPSIS
    docker 可执行命令。$env:MMORPG_DOCKER_COMMAND 非空时用它(契约测试放 .ps1 桩,不依赖真实 docker)。
#>
function Resolve-ReleaseDockerCommand {
    if (-not [string]::IsNullOrWhiteSpace($env:MMORPG_DOCKER_COMMAND)) { return $env:MMORPG_DOCKER_COMMAND }
    return 'docker'
}

<#
.SYNOPSIS
    从镜像引用里切掉 @digest 与 :tag,只留 repo(registry:5000/foo 的端口号不算 tag)。
#>
function Get-ReleaseImageRepository {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$ImageRef)

    $ref = $ImageRef.Trim()
    $atIdx = $ref.IndexOf('@')
    if ($atIdx -ge 0) { $ref = $ref.Substring(0, $atIdx) }
    $tag = Get-ImageTagFromRef -ImageRef $ref
    if ([string]::IsNullOrEmpty($tag)) { return $ref }
    return $ref.Substring(0, $ref.Length - $tag.Length - 1)
}

<#
.SYNOPSIS
    repo 比较用的归一形式:docker 在 RepoDigests 里把 Docker Hub 写成省略形式
    (docker.io/library/redis 记成 redis),不归一就会把同一个 repo 判成"不同 repo"。
#>
function ConvertTo-ComparableImageRepository {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Repository)

    $r = $Repository.Trim().ToLowerInvariant()
    foreach ($prefix in @('docker.io/', 'index.docker.io/', 'registry-1.docker.io/')) {
        if ($r.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
            $r = $r.Substring($prefix.Length)
            break
        }
    }
    if ($r.StartsWith('library/', [System.StringComparison]::Ordinal)) { $r = $r.Substring('library/'.Length) }
    return $r
}

<#
.SYNOPSIS
    回读刚推送镜像在目标 repo 上的 digest(docker image inspect 的 RepoDigests)。

.DESCRIPTION
    tag 只是别名,运行身份是 digest:部署/回滚要按 digest 定位,否则 tag 被人覆盖后无从察觉。
    只接受与 ImageRef **同 repo** 的 digest —— 同一个本地镜像可能推过多个 registry,
    拿错 repo 的 digest 去部署会拉不到或拉到别处的内容。
    同 repo 下出现多个不同 digest 时无法判断本次推的是哪个,按失败返回(fail-closed)。

.OUTPUTS
    @{ Ok=<bool>; Digest=<"repo@sha256:..." 或空串>; Reason=<string> }
#>
function Get-PushedImageDigest {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$ImageRef)

    $ref = $ImageRef.Trim()
    if ([string]::IsNullOrWhiteSpace($ref) -or $ref.Contains('@')) {
        return @{ Ok = $false; Digest = ''; Reason = "ImageRef '$ImageRef' 必须是 repo:tag 形式(不接受空串或已带 @digest 的引用)" }
    }
    $tag = Get-ImageTagFromRef -ImageRef $ref
    if ([string]::IsNullOrEmpty($tag)) {
        return @{ Ok = $false; Digest = ''; Reason = "ImageRef '$ref' 没有 tag:docker 会隐式补 latest,不能用来回读发布 digest" }
    }
    $repo = $ref.Substring(0, $ref.Length - $tag.Length - 1)
    $wantRepo = ConvertTo-ComparableImageRepository -Repository $repo

    $docker = Resolve-ReleaseDockerCommand
    $global:LASTEXITCODE = 0
    try {
        $raw = @(& $docker image inspect --format '{{json .RepoDigests}}' $ref 2>&1)
    }
    catch {
        return @{ Ok = $false; Digest = ''; Reason = "调用 '$docker image inspect $ref' 失败:$($_.Exception.Message)" }
    }
    $exitCode = $LASTEXITCODE
    $stdout = @($raw | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } | ForEach-Object { [string]$_ })
    $stderr = @($raw | Where-Object { $_ -is [System.Management.Automation.ErrorRecord] } | ForEach-Object { $_.ToString() })
    if ($exitCode -ne 0) {
        return @{ Ok = $false; Digest = ''; Reason = "docker image inspect $ref 失败(exit=$exitCode):$((@($stderr) + @($stdout)) -join ' ')" }
    }

    $jsonText = (@($stdout | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }) -join "`n").Trim()
    $entries = @()
    if (-not [string]::IsNullOrWhiteSpace($jsonText) -and $jsonText -ne 'null') {
        try {
            $entries = @($jsonText | ConvertFrom-Json)
        }
        catch {
            return @{ Ok = $false; Digest = ''; Reason = "无法解析 docker image inspect $ref 的 RepoDigests 输出:$jsonText" }
        }
    }

    $matched = New-Object System.Collections.Generic.List[string]
    $others = New-Object System.Collections.Generic.List[string]
    foreach ($entry in $entries) {
        if ($entry -isnot [string]) { continue }
        $atIdx = $entry.IndexOf('@')
        if ($atIdx -le 0) { continue }
        $digest = $entry.Substring($atIdx + 1)
        if ($digest -cnotmatch '^sha256:[0-9a-f]{64}$') { continue }
        if ((ConvertTo-ComparableImageRepository -Repository $entry.Substring(0, $atIdx)) -ceq $wantRepo) {
            if (-not $matched.Contains($digest)) { $matched.Add($digest) }
        }
        else {
            $others.Add($entry)
        }
    }

    if ($matched.Count -eq 0) {
        if ($others.Count -gt 0) {
            return @{ Ok = $false; Digest = ''; Reason = "镜像 $ref 的 RepoDigests 里只有其它 repo 的 digest($($others -join ', ')),没有 $repo 的:该镜像还没推到 $repo,或推送失败" }
        }
        return @{ Ok = $false; Digest = ''; Reason = "镜像 $ref 没有 RepoDigests:只在本地构建过、从未推送成功(digest 只有推送或拉取之后才存在)" }
    }
    if ($matched.Count -gt 1) {
        return @{ Ok = $false; Digest = ''; Reason = "镜像 $ref 在 $repo 下有多个 digest($($matched -join ', ')),无法确定本次推送的是哪一个" }
    }
    return @{ Ok = $true; Digest = "$repo@$($matched[0])"; Reason = '' }
}

<#
.SYNOPSIS
    把 "镜像 ref -> digest" 合并写进 JSON 记录文件 { "<ref>": "<repo@sha256:...>" }。

.DESCRIPTION
    读-合并-写临时文件-rename:进程中途被杀时,读者看到的要么是旧文件、要么是新文件,不会是半截 JSON。
    不防并发写(两个进程同时写同一个文件会丢一条):镜像脚本是逐个串行推送的,不要让多个进程共用一个记录文件。

    fail-closed:
      - 已有文件不是合法 JSON 对象 -> 抛,不覆盖(里面可能是别的发布的记录,先人工确认);
      - 同一 ref 已记录**不同** digest -> 抛。不可变 tag 被重推成另一份内容,正是"回滚退到同一个
        digest"那类事故的前兆,不能静默覆盖;确需重做请换一个新记录文件。
      - Digest 的 repo 与 ImageRef 的 repo 不同 -> 抛。
#>
function Write-ImageDigestRecord {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ImageRef,
        [Parameter(Mandatory = $true)][string]$Digest
    )

    if ($ImageRef.Contains('@') -or [string]::IsNullOrEmpty((Get-ImageTagFromRef -ImageRef $ImageRef))) {
        throw "ImageRef '$ImageRef' 必须是 repo:tag 形式,才能作为 digest 记录的键"
    }
    if ($Digest -cnotmatch '^[^@\s]+@sha256:[0-9a-f]{64}$') {
        throw "Digest '$Digest' 不是 repo@sha256:<64 位小写十六进制> 形式"
    }
    $refRepo = ConvertTo-ComparableImageRepository -Repository (Get-ReleaseImageRepository -ImageRef $ImageRef)
    $digestRepo = ConvertTo-ComparableImageRepository -Repository ($Digest.Substring(0, $Digest.IndexOf('@')))
    if ($refRepo -cne $digestRepo) {
        throw "Digest '$Digest' 的 repo 与 ImageRef '$ImageRef' 的 repo 不一致,拒绝记录"
    }

    $full = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    $dir = Split-Path -Parent $full
    if (-not [string]::IsNullOrEmpty($dir) -and -not [System.IO.Directory]::Exists($dir)) {
        [System.IO.Directory]::CreateDirectory($dir) | Out-Null
    }

    # 序数比较 + 排序输出:tag 区分大小写;按键排序让同一批记录无论推送顺序都写出同一份文件,便于 diff
    $records = New-Object 'System.Collections.Generic.SortedDictionary[string,string]' ([System.StringComparer]::Ordinal)
    if ([System.IO.File]::Exists($full)) {
        $text = [System.IO.File]::ReadAllText($full)
        if (-not [string]::IsNullOrWhiteSpace($text)) {
            try {
                $parsed = ConvertFrom-Json -InputObject $text -AsHashtable
            }
            catch {
                throw "digest 记录文件不是合法 JSON,拒绝覆盖(先人工确认内容):$full。$($_.Exception.Message)"
            }
            if ($parsed -isnot [System.Collections.IDictionary]) {
                throw "digest 记录文件顶层必须是 JSON 对象,拒绝覆盖:$full"
            }
            foreach ($key in $parsed.Keys) {
                if ($parsed[$key] -isnot [string]) {
                    throw "digest 记录文件里 '$key' 的值不是字符串,拒绝覆盖:$full"
                }
                $records[[string]$key] = [string]$parsed[$key]
            }
        }
    }

    if ($records.ContainsKey($ImageRef) -and $records[$ImageRef] -cne $Digest) {
        throw "镜像 $ImageRef 在 $full 里已记录不同 digest($($records[$ImageRef])),本次为 ${Digest}:同一 tag 被推成了另一份内容,拒绝覆盖记录。请核实 registry 上该 tag 的实际内容。"
    }
    $records[$ImageRef] = $Digest

    $json = (ConvertTo-Json -InputObject $records -Depth 3) + "`n"
    $tmp = "$full.tmp-$PID"
    try {
        [System.IO.File]::WriteAllText($tmp, $json, [System.Text.UTF8Encoding]::new($false))
        [System.IO.File]::Move($tmp, $full, $true)
    }
    finally {
        if ([System.IO.File]::Exists($tmp)) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
    }
}

# ─────────────────────────────────────────────────────────────────
# 3. 占位密钥 / 环境变量注入
# ─────────────────────────────────────────────────────────────────

# 仓库里散落的占位串。判据不是"两处一致"(占位串本来就处处一致),
# 而是"不得等于占位串 + 长度达标"。
$script:PlaceholderSecrets = @(
    'change-me-in-production-use-a-strong-random-key',
    'change-me-in-production',
    'change-me',
    'changeme',
    'placeholder',
    'todo',
    'secret',
    'password',
    'root',
    '123456',
    'apppass123'
)

function Test-PlaceholderSecret {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Value)

    if ($null -eq $Value) { return $true }
    $v = $Value.Trim()
    if ([string]::IsNullOrWhiteSpace($v)) { return $true }
    return ($script:PlaceholderSecrets -contains $v.ToLowerInvariant())
}

<#
.SYNOPSIS
    从环境变量取密钥;取不到时按 profile 决定是 fail-closed 还是回落到 dev 默认值。

.DESCRIPTION
    ReleaseProfile=dev 允许回落(本地栈要能一键起);staging/prod 一律 throw。
    这是"把 k8s_deploy.ps1 里写死的占位密钥改成环境变量注入"的落点。
#>
function Resolve-InjectedSecret {
    param(
        [Parameter(Mandatory = $true)][string]$EnvName,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$DevFallback,
        [Parameter(Mandatory = $true)][string]$ReleaseProfile,
        [Parameter(Mandatory = $true)][string]$Purpose,
        [int]$MinLength = 1
    )

    $value = [System.Environment]::GetEnvironmentVariable($EnvName)

    if ($ReleaseProfile -eq 'dev') {
        if ([string]::IsNullOrWhiteSpace($value)) { return $DevFallback }
        return $value
    }

    if ([string]::IsNullOrWhiteSpace($value)) {
        throw "ReleaseProfile=$ReleaseProfile 要求从环境变量 $EnvName 注入 $Purpose,当前未设置。生成器不会再把占位常量写进生产 ConfigMap。"
    }
    if (Test-PlaceholderSecret -Value $value) {
        throw "环境变量 $EnvName($Purpose)的值仍是占位串,拒绝用于 ReleaseProfile=$ReleaseProfile。"
    }
    if ($value.Length -lt $MinLength) {
        throw "环境变量 $EnvName($Purpose)长度 $($value.Length) < 要求的 $MinLength,拒绝用于 ReleaseProfile=$ReleaseProfile。"
    }

    return $value
}
