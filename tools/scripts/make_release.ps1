#requires -Version 7
<#
.SYNOPSIS
    生成版本化 release:语义版本号 + 修复内容(CHANGELOG)+ 制品事实清单(build once, promote many)。

.DESCRIPTION
    在 <制品根>/releases/manifests/ 下产出两份:
      <vX.Y.Z>.json  release manifest(机器可读,也是"已发布"哨兵):版本号、修复内容、
                     源码提交、镜像离线包版本与逐镜像清单、推送后的 digest、配置表摘要
      <vX.Y.Z>.md    release notes(人可读)

    manifest 不可变:同名 .json 已存在即拒绝(新发布用新版本号)。
    修复内容来源优先级:-Notes > -NotesFile > CHANGELOG.md 的 "## [X.Y.Z]" 段落。

    前置:先用 publish_images.ps1 -Version vX.Y.Z 发布 release 轨镜像离线包;
    推过 registry 的话把 -DigestsOut 产出的 JSON 用 -ImageDigestsFile 传进来。

    对标 A 仓 tools/scripts/make_release.ps1,相对 A 的取舍:
      - 去掉 UE 包与 configtable 引用;配置表摘要取镜像 build-info 里构建时记录的值,
        不在发布机上现算(发布机的 generated/tables 未必是出镜像那一刻的表)
      - 版本号只收 vX.Y.Z[-pre](A 还收日历版本与不带 v 的写法,四处一致校验因此容易对不上)
      - 不做旧字段名兼容:项目未上线,制品目录里没有旧格式的历史版本

.EXAMPLE
    # CHANGELOG.md 里已有 "## [1.2.3] - 2026-09-16" 段,引用 releases/images/latest.json 指向的版本
    pwsh -File tools/scripts/make_release.ps1 -Version v1.2.3

    # 推送过 registry,把 digest 记进 manifest
    pwsh -File tools/scripts/make_release.ps1 -Version v1.2.3 -ImageDigestsFile ./image-digests.json
#>
[CmdletBinding()]
param(
    # 语义版本,必须小写 v:v1.2.3 / v1.2.3-rc.1
    [Parameter(Mandatory = $true)][string]$Version,
    # 行内修复内容(最高优先级)
    [string]$Notes = '',
    # 修复内容文件(次优先)
    [string]$NotesFile = '',
    # releases/images/ 下的版本目录名;留空 = 读 releases/images/latest.json
    [string]$ImagesVersion = '',
    # 推送后记录的 { "<repo:tag>": "<repo@sha256:...>" }(release_common.ps1 Write-ImageDigestRecord 的产物)
    [string]$ImageDigestsFile = '',
    # 制品根;留空 = $env:MMORPG_ARTIFACT_ROOT > <仓库父目录>/artifacts
    [string]$ArtifactRoot = '',
    # 仓库根(读 CHANGELOG.md);留空按本脚本位置推算。契约测试的注入缝
    [string]$RepoRoot = '',
    # 允许引用脏树来源的制品(仅内测;正规发布不应走到这里)
    [switch]$AllowDirty
)

$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'lib' 'release_common.ps1')
. (Join-Path $PSScriptRoot 'lib' 'artifacts_lib.ps1')

function Write-ReleaseInfo([string]$Message) { Write-Host "[INFO] $Message" -ForegroundColor Cyan }
function Write-ReleaseOk([string]$Message) { Write-Host "[ OK ] $Message" -ForegroundColor Green }

function Write-Utf8NoBomFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

<#
.SYNOPSIS
    取 CHANGELOG.md 中 "## [X.Y.Z]"(可带 " - YYYY-MM-DD")段落正文。

.DESCRIPTION
    只负责"取正文",调用前必须先过 release_common.ps1 的 Test-ChangelogReleaseSection
    (有没有段、是否重复、是否为空由它判定,release.yml 构建前预检用的是同一个函数,两处结论一致)。
    标题与段落边界规则必须与它逐条一致:标题精确匹配(找 1.2.3 不命中 [1.2.3-rc.1] / [11.2.3]),
    段落到下一个 "## " 为止("### 新增" 属于段内),链接引用定义行([1.2.3]: https://...)不算正文。
#>
function Get-ChangelogSection {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$SemVer
    )

    $heading = '^## \[' + [regex]::Escape($SemVer) + '\](?:\s+-\s+\d{4}-\d{2}-\d{2})?\s*$'
    $lines = [System.IO.File]::ReadAllLines($Path, [System.Text.Encoding]::UTF8)
    $body = New-Object System.Collections.Generic.List[string]
    $inSection = $false
    foreach ($line in $lines) {
        if (-not $inSection) {
            if ($line -match $heading) { $inSection = $true }
            continue
        }
        if ($line -match '^## ') { break }
        if ($line -match '^\[[^\]]+\]:\s*\S') { continue }
        $body.Add($line)
    }
    return (($body -join "`n").Trim())
}

# ─────────────────────────────────────────────────────────────────
# 1. 版本号 + 不可变守卫
# ─────────────────────────────────────────────────────────────────

$versionCheck = Test-ReleaseVersion -Version $Version
if (-not $versionCheck.Ok) {
    throw "发布版本号非法:'$Version'($($versionCheck.Reason))。格式必须是 vX.Y.Z 或 vX.Y.Z-<预发布标识>,v 必须小写。"
}
$semVer = $versionCheck.Normalized

if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
    $RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
}
$RepoRoot = [System.IO.Path]::GetFullPath($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($RepoRoot))

$channelArgs = @{ Channel = 'release' }
if (-not [string]::IsNullOrWhiteSpace($ArtifactRoot)) { $channelArgs.Override = $ArtifactRoot }
$releaseRoot = Get-ChannelRoot @channelArgs

$manifestsDir = Join-Path $releaseRoot 'manifests'
if (-not [System.IO.Directory]::Exists($manifestsDir)) { [System.IO.Directory]::CreateDirectory($manifestsDir) | Out-Null }
$manifestPath = Join-Path $manifestsDir "$Version.json"
$notesPath = Join-Path $manifestsDir "$Version.md"
# 只看 .json:它是最后落盘的哨兵。只有 .md 没有 .json = 上次中途失败,允许直接重跑覆盖 .md。
if ([System.IO.File]::Exists($manifestPath)) {
    throw "release $Version 已存在且不可变:$manifestPath(新发布请提升版本号)"
}

# ─────────────────────────────────────────────────────────────────
# 2. 修复内容:-Notes > -NotesFile > CHANGELOG.md 段落
# ─────────────────────────────────────────────────────────────────

$resolvedNotes = $null
$notesSource = ''
if (-not [string]::IsNullOrWhiteSpace($Notes)) {
    $resolvedNotes = $Notes.Trim()
    $notesSource = '-Notes'
}
elseif (-not [string]::IsNullOrWhiteSpace($NotesFile)) {
    if (-not (Test-Path -LiteralPath $NotesFile -PathType Leaf)) { throw "找不到修复内容文件:$NotesFile" }
    $resolvedNotes = ([System.IO.File]::ReadAllText((Resolve-Path -LiteralPath $NotesFile).Path, [System.Text.Encoding]::UTF8)).Trim()
    if ([string]::IsNullOrWhiteSpace($resolvedNotes)) { throw "修复内容文件为空:$NotesFile" }
    $notesSource = $NotesFile
}
else {
    $changelogPath = Join-Path $RepoRoot 'CHANGELOG.md'
    $sectionCheck = Test-ChangelogReleaseSection -ChangelogPath $changelogPath -Version $Version
    if (-not $sectionCheck.Ok) {
        throw "取不到修复内容,拒绝发布:$($sectionCheck.Reason)。也可以用 -Notes / -NotesFile 直接提供修复内容。"
    }
    $resolvedNotes = Get-ChangelogSection -Path $changelogPath -SemVer $semVer
    # 双保险:判定函数说有正文而这里取出来是空,说明两边规则漂移了,不能带着空说明发布
    if ([string]::IsNullOrWhiteSpace($resolvedNotes)) {
        throw "CHANGELOG.md 的 [$semVer] 段通过了 Test-ChangelogReleaseSection,但取出的正文为空:make_release.ps1 与 release_common.ps1 的段落规则不一致,先对齐再发布。"
    }
    $notesSource = "CHANGELOG.md [$semVer]"
}

# ─────────────────────────────────────────────────────────────────
# 3. 引用的镜像离线包:存在 + 校验和 + 版本交叉校验
# ─────────────────────────────────────────────────────────────────

$imagesRoot = Join-Path $releaseRoot 'images'
if ([string]::IsNullOrWhiteSpace($ImagesVersion)) {
    $latestPath = Join-Path $imagesRoot 'latest.json'
    if (-not [System.IO.File]::Exists($latestPath)) {
        throw "没有已发布的 release 轨镜像版本($latestPath 不存在),先跑 publish_images.ps1 -Version $Version。"
    }
    $ImagesVersion = [string](Get-Content -LiteralPath $latestPath -Raw | ConvertFrom-Json -AsHashtable)['version']
}
# 版本目录名会拼进路径,先挡住 "../" 这类穿越
if ($ImagesVersion -cnotmatch '^[0-9A-Za-z][0-9A-Za-z._-]*$' -or $ImagesVersion.Contains('..')) {
    throw "镜像版本目录名非法:'$ImagesVersion'"
}

$imagesDir = Join-Path $imagesRoot $ImagesVersion
if (-not [System.IO.Directory]::Exists($imagesDir)) {
    throw "release 轨镜像版本不存在:$imagesDir(先跑 publish_images.ps1 -Version $Version)"
}

Write-ReleaseInfo "校验镜像制品:$imagesDir"
try {
    Test-Sha256Sums -Dir $imagesDir
}
catch {
    throw "镜像制品校验失败,拒绝基于被改动过的制品发布:$($_.Exception.Message)"
}

$buildInfoPath = Join-Path $imagesDir 'build-info.json'
$imagesManifestPath = Join-Path $imagesDir 'images-manifest.json'
foreach ($p in @($buildInfoPath, $imagesManifestPath)) {
    if (-not [System.IO.File]::Exists($p)) { throw "镜像制品缺少 $(Split-Path -Leaf $p)(非 publish_images.ps1 产物?):$imagesDir" }
}
$buildInfo = Get-Content -LiteralPath $buildInfoPath -Raw | ConvertFrom-Json -AsHashtable
if ($buildInfo -isnot [System.Collections.IDictionary]) { throw "build-info.json 不是 JSON 对象:$buildInfoPath" }
$imageList = @(Get-Content -LiteralPath $imagesManifestPath -Raw | ConvertFrom-Json | Where-Object { $null -ne $_ })

if ([string]$buildInfo['channel'] -cne 'release' -or [string]$buildInfo['version'] -cne $ImagesVersion) {
    throw "镜像制品身份与目录不符:build-info channel='$($buildInfo['channel'])' version='$($buildInfo['version'])',目录=releases/images/$ImagesVersion(勿手工搬动或改名制品目录)"
}

# 发布包名与镜像自报版本必须一致,否则会出现"release 叫 v1.2.3、镜像里自报别的版本"的错配。
$appVersion = [string]$buildInfo['app_version']
if ([string]::IsNullOrWhiteSpace($appVersion)) {
    throw "镜像 build-info 缺 app_version(疑似没带 -Version 的快照发布):$imagesDir。先跑 publish_images.ps1 -Version $Version。"
}
if ($appVersion -cne $Version) {
    throw "release 版本号与镜像自报版本不一致:发布=$Version,镜像 app_version=$appVersion;用同一个版本号重新出包后再发布(勿手工改名)。"
}

$sourceCommit = [string]$buildInfo['commit']
if ($sourceCommit -cnotmatch '^[0-9a-f]{12}$') {
    throw "镜像 build-info 的 commit 不是 12 位小写 sha:'$sourceCommit'($imagesDir)"
}
$sourceDirty = [bool]$buildInfo['dirty']
if ($sourceDirty -and -not $AllowDirty) {
    throw "镜像制品来自脏工作树(dirty=true,commit=$sourceCommit),正规发布不引用无法追溯到确定提交的制品。清干净工作树重新出包;确需内测显式加 -AllowDirty。"
}

if ($imageList.Count -eq 0) { throw "images-manifest.json 为空:$imagesDir" }
$refs = @($imageList | ForEach-Object { [string]$_.ref })
# ref 必须是 repo:tag(不收 digest 形式):release 清单记的是出包时的 tag,digest 单独记在 digests 里
$badRefs = @($refs | Where-Object { [string]::IsNullOrWhiteSpace($_) -or $_.Contains('@') -or [string]::IsNullOrEmpty((Get-ImageTagFromRef -ImageRef $_)) })
if ($badRefs.Count -gt 0) {
    throw "images-manifest.json 里有条目的 ref 不是 repo:tag 形式:'$($badRefs -join "', '")'($imagesDir)"
}
if ([int]$buildInfo['image_count'] -ne $imageList.Count) {
    throw "build-info image_count=$($buildInfo['image_count']) 与 images-manifest.json 条目数 $($imageList.Count) 不一致:$imagesDir"
}

$tablesSha = [string]$buildInfo['tables_sha256']
if ($tablesSha -cnotmatch '^[0-9a-f]{64}$') {
    throw "镜像 build-info 缺少有效的 tables_sha256:$imagesDir"
}

# ─────────────────────────────────────────────────────────────────
# 4. 推送后的 digest(可选)
# ─────────────────────────────────────────────────────────────────

# 给了就必须恰好覆盖清单里的全部镜像:按 digest 部署时,缺一个就只能回落到可变的 tag,
# 多一个说明 digest 文件来自另一次发布。
$digests = [ordered]@{}
if (-not [string]::IsNullOrWhiteSpace($ImageDigestsFile)) {
    if (-not (Test-Path -LiteralPath $ImageDigestsFile -PathType Leaf)) { throw "找不到镜像 digest 文件:$ImageDigestsFile" }
    $digestMap = Get-Content -LiteralPath $ImageDigestsFile -Raw | ConvertFrom-Json -AsHashtable
    if ($digestMap -isnot [System.Collections.IDictionary]) {
        throw "镜像 digest 文件格式不对,应为 { `"<repo:tag>`": `"<repo@sha256:...>`" }:$ImageDigestsFile"
    }

    $problems = New-Object System.Collections.Generic.List[string]
    foreach ($key in $digestMap.Keys) {
        if ($refs -cnotcontains $key) { $problems.Add("不在本次镜像清单里:$key") }
    }
    foreach ($ref in $refs) {
        if (-not $digestMap.Contains($ref)) { $problems.Add("缺少 digest:$ref"); continue }
        $digest = [string]$digestMap[$ref]
        # digest 必须指向同一个仓库:repo:tag 的 digest 写成别的仓库,按 digest 部署就拉错镜像。
        # 仓库比较口径与写记录的 Write-ImageDigestRecord 相同(Docker Hub 省略形式归一)
        $wantRepo = ConvertTo-ComparableImageRepository -Repository (Get-ReleaseImageRepository -ImageRef $ref)
        if ($digest -cmatch '^(?<repo>[^@\s]+)@sha256:[0-9a-f]{64}$' -and
            (ConvertTo-ComparableImageRepository -Repository $Matches['repo']) -ceq $wantRepo) {
            $digests[$ref] = $digest
        }
        else {
            $problems.Add("digest 格式不对或仓库不符:$ref -> $digest")
        }
    }
    if ($problems.Count -gt 0) {
        throw "镜像 digest 文件与镜像清单对不上($($problems.Count) 项,$ImageDigestsFile):`n$($problems -join "`n")"
    }
}

# ─────────────────────────────────────────────────────────────────
# 5. 落盘:.md 先 rename 上线,.json(哨兵)最后 rename
# ─────────────────────────────────────────────────────────────────

# InvariantCulture:自定义格式里的 ':' 是区域时间分隔符,个别区域设置下会变成 '.'
$createdAt = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)
$imageTag = [string]$buildInfo['image_tag']
$release = [ordered]@{
    version    = $Version
    notes      = $resolvedNotes
    created_at = $createdAt
    machine    = [System.Environment]::MachineName
    publisher  = [System.Environment]::UserName
    source     = [ordered]@{
        commit = $sourceCommit
        dirty  = $sourceDirty
    }
    images     = [ordered]@{
        version    = $ImagesVersion
        path       = "releases/images/$ImagesVersion"
        image_tag  = $imageTag
        image_list = $imageList
        digests    = $digests
    }
    tables     = [ordered]@{
        sha256     = $tablesSha
        file_count = [int]$buildInfo['tables_file_count']
    }
}
$manifestJson = ($release | ConvertTo-Json -Depth 10) + "`n"

$md = New-Object System.Collections.Generic.List[string]
$md.Add("# xuanming-server-mmo $Version")
$md.Add('')
$md.Add("- 发布时间(UTC):$createdAt")
$md.Add("- 源码提交:$sourceCommit$(if ($sourceDirty) { '(dirty,仅内测)' })")
$md.Add("- 镜像离线包:releases/images/$ImagesVersion(tag $imageTag,$($imageList.Count) 个镜像,已记录 digest $($digests.Count) 个)")
$md.Add("- 配置表摘要:$tablesSha($($buildInfo['tables_file_count']) 个文件)")
$md.Add("- 修复内容来源:$notesSource")
$md.Add('')
$md.Add('## 镜像清单')
$md.Add('')
$md.Add('| family | service | ref |')
$md.Add('|---|---|---|')
foreach ($entry in $imageList) { $md.Add("| $($entry.family) | $($entry.service) | ``$($entry.ref)`` |") }
$md.Add('')
$md.Add('## 修复内容')
$md.Add('')
$md.Add($resolvedNotes)
$notesText = ($md -join "`n") + "`n"

# JSON 是"已发布"哨兵(上面的不可变守卫只认它),必须最后落盘:
# 任何一步在 JSON rename 之前失败都不留哨兵,修好后可以直接重跑;
# 反过来先落 JSON 再写 MD 失败,就会留下半套 release 且重跑被不可变守卫拒绝。
$jsonTmp = "$manifestPath.tmp-$PID"
$mdTmp = "$notesPath.tmp-$PID"
try {
    Write-Utf8NoBomFile -Path $jsonTmp -Text $manifestJson
    Write-Utf8NoBomFile -Path $mdTmp -Text $notesText
    [System.IO.File]::Move($mdTmp, $notesPath, $true)
    # overwrite=$false:两个发布者同时走到这里时,后到的一方必须失败而不是覆盖已发布的 manifest
    try {
        [System.IO.File]::Move($jsonTmp, $manifestPath, $false)
    }
    catch {
        if ([System.IO.File]::Exists($manifestPath)) {
            throw "发布竞争:release $Version 在本次生成期间已被他人发布:$manifestPath"
        }
        throw
    }
}
finally {
    foreach ($tmp in @($jsonTmp, $mdTmp)) {
        if ([System.IO.File]::Exists($tmp)) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
    }
}

Write-ReleaseOk "release $Version 已生成"
Write-Host "  manifest: $manifestPath"
Write-Host "  notes   : $notesPath"
Write-ReleaseInfo "离线交付:按 manifest 的 images.path 从制品根拷整个版本目录到目标机(自带 sha256sums.txt)。"
Write-ReleaseInfo "打 tag 由人执行:git tag $Version $sourceCommit"
exit 0
