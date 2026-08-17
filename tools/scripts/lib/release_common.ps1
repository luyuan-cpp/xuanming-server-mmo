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
