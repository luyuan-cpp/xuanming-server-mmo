#requires -Version 7
<#
.SYNOPSIS
    构建 / 收集服务端运行镜像,逐个离线导出,原子发布到版本库外的制品目录。

.DESCRIPTION
    流程:
      1) git 版本戳:脏树默认拒绝;release 轨(给了 -Version)脏树一律拒绝
      2) 算镜像 tag 与版本目录名;目录已存在即拒绝(-SkipIfExists 且同一提交时 exit 0)—— 先查,不白构建
      3) 调既有镜像脚本构建(子 pwsh 进程):k8s_image.ps1 / go_svc_image.ps1 / java_svc_image.ps1
      4) 用各脚本 -Command list-refs 取期望镜像清单(本脚本不另抄服务清单)
      5) docker image inspect:镜像必须存在,且 OCI label org.opencontainers.image.revision == 当前 commit
      6) staging 里逐个 docker save,读回 tar 内 manifest.json 确认 RepoTags 恰为该镜像
      7) 写 images-manifest.json / build-info.json / symbols/ / sha256sums.txt,rename 上线,更新 latest.json

    制品布局(与 artifacts_lib.ps1、make_release.ps1、fetch/import/retention 共用同一契约):
      <root>/{snapshots|releases}/images/<ver>/
          images/<镜像 ref 中 / 与 : 替换为 _>.tar
          images-manifest.json   [ { family, service, ref, image_id, revision, file } ],file 相对版本目录
          build-info.json
          symbols/*.debug        (可选,C++ 分离符号,来自 bin/symbols)
          sha256sums.txt
      <root>/{snapshots|releases}/images/latest.json
    版本目录名:release 轨 = vX.Y.Z;snapshot 轨 = g<sha12>(-AllowDirty 脏树为 g<sha12>-dirty-yyyyMMdd-HHmmss)。
    制品根:-ArtifactRoot > $env:MMORPG_ARTIFACT_ROOT > <仓库父目录>/artifacts。

    对标 A 仓 publish_offline_images.ps1 + export_images.ps1,相对 A 的取舍:
      - 每个镜像单独 docker save 成一个 tar。A 批量 save 时两个镜像层链完全相同会静默丢一个,
        事后探测再合并;单镜像单文件从根上没有这个问题,fetch/import 也能按镜像校验。
      - 用 revision label == 当前 commit 代替 A 的"镜像 Created 早于源码 mtime"过期守卫:
        CI 全新检出时 mtime 全是检出时刻,守卫形同虚设;label 直接证明镜像出自哪次提交。
      - release 轨脏树即使 -AllowDirty 也拒绝(A 放行)。
      - 不在这里推 registry:推送走各镜像脚本的 push 命令(-DigestsOut 记录 digest),
        制品目录只管离线包,两件事失败重跑的语义互不牵连。

.EXAMPLE
    # 快照轨:从当前提交构建全部镜像并发布(工作树必须干净)
    pwsh -File tools/scripts/publish_images.ps1

    # 发布轨:版本号注入镜像,目录名 releases/images/v1.2.3
    pwsh -File tools/scripts/publish_images.ps1 -Version v1.2.3

    # 镜像已按当前提交构建好,只导出发布;CI 重跑幂等
    pwsh -File tools/scripts/publish_images.ps1 -SkipBuild -SkipIfExists

    # 本机没有 Linux 运行时 staging,只发 Go 与 Java
    pwsh -File tools/scripts/publish_images.ps1 -Families go,java
#>
[CmdletBinding()]
param(
    # 发布版本号 vX.Y.Z[-pre];留空 = $env:MMORPG_RELEASE_VERSION;都没有 = snapshot 轨
    [string]$Version = '',
    # cpp / go / java 的子集,可写成 "go,java"
    [string[]]$Families = @('cpp', 'go', 'java'),
    # 不构建,只收集已有镜像(revision label 守卫照样生效)
    [switch]$SkipBuild,
    # 快照轨允许脏树(版本目录带 -dirty-时间戳);release 轨无效
    [switch]$AllowDirty,
    # 目标版本目录已存在且出自同一提交时 exit 0(CI 幂等重跑)
    [switch]$SkipIfExists,
    [string]$ArtifactRoot = '',
    # 镜像 registry 前缀;留空 = 各镜像脚本自己的默认值
    [string]$Registry = '',
    # 不传 = 含 cpp 且 bin/symbols 下有 *.debug 就带;显式传 = 必须带,缺了报错;-IncludeSymbols:$false = 不带
    [switch]$IncludeSymbols,
    # 不传 = release_common.ps1 Resolve-ReleaseDockerCommand($env:MMORPG_DOCKER_COMMAND > docker)
    [string]$DockerCommand = 'docker',
    # 仓库根(git 版本戳 / generated/tables / bin/symbols 的来源);留空按本脚本位置推算。契约测试的注入缝
    [string]$RepoRoot = '',
    # family -> 镜像脚本路径,只给契约测试替换成桩;正式使用不要传
    [hashtable]$ImageScriptOverrides = @{}
)

$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'lib' 'release_common.ps1')
. (Join-Path $PSScriptRoot 'lib' 'artifacts_lib.ps1')

function Write-PublishInfo([string]$Message) { Write-Host "[INFO] $Message" -ForegroundColor Cyan }
function Write-PublishOk([string]$Message) { Write-Host "[ OK ] $Message" -ForegroundColor Green }
function Write-PublishWarn([string]$Message) { Write-Host "[WARN] $Message" -ForegroundColor Yellow }
function Write-PublishStep([string]$Message) { Write-Host "`n===== $Message =====" -ForegroundColor Magenta }

# family -> 怎么调镜像脚本。这里只记调用方式;"有哪些镜像"的事实源是各脚本自己(list-refs)。
# java_svc_image.ps1 的构建命令叫 build(不是 build-all),k8s_image.ps1 的 tag 参数叫 -ImageTag。
$script:FamilySpecs = [ordered]@{
    cpp  = @{ Script = 'k8s_image.ps1';      BuildCommand = 'build-image'; TagParam = '-ImageTag' }
    go   = @{ Script = 'go_svc_image.ps1';   BuildCommand = 'build-all';   TagParam = '-Tag' }
    java = @{ Script = 'java_svc_image.ps1'; BuildCommand = 'build';       TagParam = '-Tag' }
}

function Write-Utf8NoBomFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

function Resolve-PublishFamilies {
    param([string[]]$Requested)

    # pwsh -File 传进来的 "go,java" 是单个字符串,逐个再按逗号拆
    $wanted = @($Requested | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim().ToLowerInvariant() } | Where-Object { $_ })
    if ($wanted.Count -eq 0) { throw "-Families 不能为空,可选:cpp, go, java" }
    $unknown = @($wanted | Where-Object { -not $script:FamilySpecs.Contains($_) })
    if ($unknown.Count -gt 0) { throw "未知的镜像 family:$($unknown -join ', ')。可选:cpp, go, java" }
    # 固定按 cpp → go → java 去重排序:清单与 build-info 不随传参顺序漂移,两次发布可以直接 diff
    return , @($script:FamilySpecs.Keys | Where-Object { $wanted -contains $_ })
}

function Get-ImageScriptPath {
    param([Parameter(Mandatory = $true)][string]$Family)
    if ($ImageScriptOverrides.ContainsKey($Family)) { return [string]$ImageScriptOverrides[$Family] }
    return (Join-Path $PSScriptRoot $script:FamilySpecs[$Family].Script)
}

<#
.SYNOPSIS
    以子 pwsh 进程调镜像脚本,返回 @{ ExitCode; Lines }。

.DESCRIPTION
    用子进程而不是进程内 `&`:镜像脚本顶层有 throw / exit,还有 $Tag / $Registry / $RepoRoot 这类
    与本脚本同名的脚本级变量,进程内调用会互相覆盖。
    -CaptureOutput 只收 stdout(list-refs 的清单);stderr 照常透传给调用方看。
#>
function Invoke-ImageScript {
    param(
        [Parameter(Mandatory = $true)][string]$Family,
        [Parameter(Mandatory = $true)][string]$ScriptCommand,
        [string[]]$Arguments = @(),
        [switch]$CaptureOutput
    )

    $scriptPath = Get-ImageScriptPath -Family $Family
    if (-not (Test-Path -LiteralPath $scriptPath -PathType Leaf)) { throw "镜像脚本不存在:$scriptPath" }
    $argv = @('-NoProfile', '-NonInteractive', '-File', $scriptPath, '-Command', $ScriptCommand) + $Arguments

    if ($CaptureOutput) {
        $lines = @(& pwsh @argv)
        return @{ ExitCode = $LASTEXITCODE; Lines = $lines }
    }
    & pwsh @argv | Out-Host
    return @{ ExitCode = $LASTEXITCODE; Lines = @() }
}

<#
.SYNOPSIS
    调 <镜像脚本> -Command list-refs,返回镜像引用数组。

.DESCRIPTION
    接口约定是"每行一个 <repo>/<name>:<tag>,不输出任何其它内容"。混进任何非引用行都直接失败,
    不做宽松过滤:这份清单决定哪些镜像进离线包,宁可报错也不猜哪一行是镜像。
#>
function Get-FamilyImageRefs {
    param(
        [Parameter(Mandatory = $true)][string]$Family,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    $scriptName = Split-Path -Leaf (Get-ImageScriptPath -Family $Family)
    $result = Invoke-ImageScript -Family $Family -ScriptCommand 'list-refs' -Arguments $Arguments -CaptureOutput
    if ($result.ExitCode -ne 0) {
        throw "$scriptName -Command list-refs 失败(exit=$($result.ExitCode)),拿不到 $Family 镜像清单。"
    }

    $refs = New-Object System.Collections.Generic.List[string]
    foreach ($raw in $result.Lines) {
        $line = ([string]$raw).Trim()
        if ($line.Length -eq 0) { continue }
        if ($line -cnotmatch '^[^\s@]+/[^\s@/:]+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$') {
            throw "$scriptName -Command list-refs 输出了非镜像引用的内容(接口约定每行只能是 <repo>/<name>:<tag>):'$line'"
        }
        $refs.Add($line)
    }
    if ($refs.Count -eq 0) { throw "$scriptName -Command list-refs 没有输出任何镜像引用($Family)。" }
    return , $refs.ToArray()
}

<#
.SYNOPSIS
    C++ 运行镜像的镜像名(如 mmorpg-node)。

.DESCRIPTION
    k8s_image.ps1 的仓库参数 -ImageRepository 是含镜像名的完整 repository,不是 registry 前缀。
    给了 -Registry 时要拼 "<registry>/<镜像名>",镜像名从它自己默认的 list-refs 里取,
    不在这里再抄一份字面量。
#>
function Get-CppImageName {
    if ($script:CppImageName) { return $script:CppImageName }
    $refs = Get-FamilyImageRefs -Family 'cpp' -Arguments @($script:FamilySpecs['cpp'].TagParam, $script:ImageTag)
    if ($refs.Count -ne 1) {
        throw "k8s_image.ps1 -Command list-refs 应只输出 1 个 C++ 运行镜像,实际 $($refs.Count) 个:$($refs -join ', ')"
    }
    $repo = Get-ReleaseImageRepository -ImageRef $refs[0]
    $script:CppImageName = $repo.Substring($repo.LastIndexOf('/') + 1)
    return $script:CppImageName
}

# 传给镜像脚本的公共参数:tag、发布版本号、registry。构建与 list-refs 用同一份,保证清单就是刚构建的那批。
function Get-ImageScriptArgs {
    param([Parameter(Mandatory = $true)][string]$Family)

    $argv = @($script:FamilySpecs[$Family].TagParam, $script:ImageTag)
    if ($script:ReleaseVersion) { $argv += @('-Version', $script:ReleaseVersion) }
    if ($script:RegistryPrefix) {
        if ($Family -eq 'cpp') {
            $argv += @('-ImageRepository', "$($script:RegistryPrefix)/$(Get-CppImageName)")
        }
        else {
            $argv += @('-Registry', $script:RegistryPrefix)
        }
    }
    return , $argv
}

<#
.SYNOPSIS
    docker image inspect 取镜像 ID 与 revision label;镜像不存在返回 $null。
#>
function Get-DockerImageIdentity {
    param([Parameter(Mandatory = $true)][string]$Ref)

    # 镜像不存在时 docker 往 stderr 写一行并返回非 0;pwsh 7.0/7.1 在 Stop 下会把重定向的原生 stderr
    # 当成终止错误,函数内局部放宽,由退出码判定
    $ErrorActionPreference = 'Continue'
    $global:LASTEXITCODE = 0
    $raw = & $DockerCommand image inspect $Ref 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    $ErrorActionPreference = 'Stop'
    # -AsHashtable:label 键区分大小写,PSCustomObject 会把大小写不同的键折叠成一个
    # 空输出时 ConvertFrom-Json 会往管道写一个 $null,先滤掉,否则会被当成"1 个对象"
    $parsed = @(($raw | Out-String) | ConvertFrom-Json -AsHashtable | Where-Object { $_ -is [System.Collections.IDictionary] })
    if ($parsed.Count -ne 1) { throw "docker image inspect $Ref 返回了 $($parsed.Count) 个镜像对象,期望 1 个" }

    $img = $parsed[0]
    $revision = ''
    $config = $img['Config']
    if ($config -is [System.Collections.IDictionary]) {
        $labels = $config['Labels']
        if ($labels -is [System.Collections.IDictionary] -and $labels.Contains('org.opencontainers.image.revision')) {
            $revision = [string]$labels['org.opencontainers.image.revision']
        }
    }
    return @{ Id = [string]$img['Id']; Revision = $revision }
}

function Test-TarReaderAvailable {
    if ($null -ne $script:TarReaderAvailable) { return $script:TarReaderAvailable }
    try { Add-Type -AssemblyName System.Formats.Tar -ErrorAction Stop } catch { }
    $script:TarReaderAvailable = ($null -ne ('System.Formats.Tar.TarReader' -as [type]))
    return $script:TarReaderAvailable
}

<#
.SYNOPSIS
    读 tar 归档里某个条目的文本;条目不存在返回 $null。

.DESCRIPTION
    优先 .NET System.Formats.Tar(pwsh 7.3+ 跨平台一致,不依赖宿主 tar 实现);
    老运行时退回系统 tar(Windows 10+ 自带 bsdtar,Linux 为 GNU tar)。
#>
function Read-TarEntryText {
    param(
        [Parameter(Mandatory = $true)][string]$ArchivePath,
        [Parameter(Mandatory = $true)][string]$EntryName
    )

    if (Test-TarReaderAvailable) {
        $stream = [System.IO.File]::OpenRead($ArchivePath)
        try {
            $reader = [System.Formats.Tar.TarReader]::new($stream, $true)
            try {
                while ($true) {
                    $entry = $reader.GetNextEntry($false)
                    if ($null -eq $entry) { return $null }
                    if (($entry.Name -replace '^\./', '') -cne $EntryName) { continue }
                    if ($null -eq $entry.DataStream) { return '' }
                    $sr = [System.IO.StreamReader]::new($entry.DataStream, [System.Text.UTF8Encoding]::new($false))
                    try { return $sr.ReadToEnd() } finally { $sr.Dispose() }
                }
            }
            finally { $reader.Dispose() }
        }
        finally { $stream.Dispose() }
    }

    $ErrorActionPreference = 'Continue'
    $global:LASTEXITCODE = 0
    $text = (& tar -xOf $ArchivePath $EntryName 2>$null | Out-String)
    if ($LASTEXITCODE -ne 0) { return $null }
    return $text
}

# docker save 产物里 manifest.json 的全部 RepoTags
function Get-ImageArchiveRepoTags {
    param([Parameter(Mandatory = $true)][string]$ArchivePath)

    $json = Read-TarEntryText -ArchivePath $ArchivePath -EntryName 'manifest.json'
    if ([string]::IsNullOrWhiteSpace($json)) { throw "镜像归档里读不到 manifest.json:$ArchivePath" }
    $tags = New-Object System.Collections.Generic.List[string]
    foreach ($entry in @($json | ConvertFrom-Json -AsHashtable)) {
        if ($entry -isnot [System.Collections.IDictionary]) { continue }
        foreach ($tag in @($entry['RepoTags'])) {
            if (-not [string]::IsNullOrWhiteSpace([string]$tag)) { $tags.Add([string]$tag) }
        }
    }
    return , $tags.ToArray()
}

function Get-SymbolFiles {
    param([Parameter(Mandatory = $true)][string]$SymbolsDir)
    if (-not [System.IO.Directory]::Exists($SymbolsDir)) { return , @() }
    $files = @(Get-ChildItem -LiteralPath $SymbolsDir -File -Filter '*.debug' | Sort-Object -Property Name -CaseSensitive)
    return , $files
}

# ─────────────────────────────────────────────────────────────────
# 1. 参数、版本号、仓库根
# ─────────────────────────────────────────────────────────────────

$families = Resolve-PublishFamilies -Requested $Families

foreach ($key in @($ImageScriptOverrides.Keys)) {
    if (-not $script:FamilySpecs.Contains([string]$key)) { throw "-ImageScriptOverrides 的键只能是 cpp / go / java,实际:$key" }
    if (-not (Test-Path -LiteralPath ([string]$ImageScriptOverrides[$key]) -PathType Leaf)) {
        throw "-ImageScriptOverrides[$key] 指向的脚本不存在:$($ImageScriptOverrides[$key])"
    }
}

$script:ReleaseVersion = ''
if (-not [string]::IsNullOrWhiteSpace($Version)) {
    $script:ReleaseVersion = $Version.Trim()
}
elseif (-not [string]::IsNullOrWhiteSpace($env:MMORPG_RELEASE_VERSION)) {
    $script:ReleaseVersion = $env:MMORPG_RELEASE_VERSION.Trim()
    Write-PublishInfo "发布版本号取自环境变量 MMORPG_RELEASE_VERSION=$($script:ReleaseVersion)"
}

$channel = 'snapshot'
if ($script:ReleaseVersion) {
    $versionCheck = Test-ReleaseVersion -Version $script:ReleaseVersion
    if (-not $versionCheck.Ok) {
        throw "发布版本号非法:'$($script:ReleaseVersion)'($($versionCheck.Reason))。格式必须是 vX.Y.Z 或 vX.Y.Z-<预发布标识>,v 必须小写。"
    }
    $channel = 'release'
}

$script:RegistryPrefix = $Registry.Trim().TrimEnd('/')

if (-not $PSBoundParameters.ContainsKey('DockerCommand')) {
    $DockerCommand = Resolve-ReleaseDockerCommand
}

if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
    $RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
}
$RepoRoot = [System.IO.Path]::GetFullPath($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($RepoRoot))

# ─────────────────────────────────────────────────────────────────
# 2. git 版本戳 → 镜像 tag / 版本目录名
# ─────────────────────────────────────────────────────────────────

$stamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
if (-not $stamp.Ok) { throw "无法取得 git 版本戳,拒绝发布:$($stamp.Reason)" }

if ($stamp.Dirty) {
    if ($channel -eq 'release') {
        # 比 A 仓更严(A 的 -AllowDirty 对发布版本同样放行):发布版本要能 checkout 回确定的源码去
        # 复现问题、打补丁、对符号查崩溃,脏树产物做不到,所以 release 轨不留绕过开关。
        throw "工作树有未提交改动(commit $($stamp.Commit)),release 轨拒绝发布 $($script:ReleaseVersion):发布版本必须能追溯到确定提交,-AllowDirty 对 release 轨无效。先提交或清理工作树。"
    }
    if (-not $AllowDirty) {
        throw "工作树有未提交改动(commit $($stamp.Commit)),快照镜像无法追溯到确定提交。先提交,或本机联调显式加 -AllowDirty(版本目录带 -dirty-时间戳)。"
    }
    Write-PublishWarn "工作树是脏的,按 -AllowDirty 发布快照;该版本无法从 commit 还原,只能本机联调用。"
}

$tagArgs = @{ Commit = $stamp.Commit }
if ($script:ReleaseVersion) { $tagArgs.Version = $script:ReleaseVersion }
elseif ($stamp.Dirty) { $tagArgs.Dirty = $true }
$script:ImageTag = Get-ReleaseImageTag @tagArgs

$versionDirName = if ($channel -eq 'release') {
    $script:ReleaseVersion
}
elseif ($stamp.Dirty) {
    "g$($stamp.Commit)-dirty-$([DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss', [System.Globalization.CultureInfo]::InvariantCulture))"
}
else {
    "g$($stamp.Commit)"
}
# fetch / retention 只认这个形状的快照目录名,这里生成出不合规的名字就等于发布了一个没人能取的版本
if ($channel -eq 'snapshot' -and -not (Test-SnapshotVersionName -Version $versionDirName)) {
    throw "生成的快照版本目录名不合规:'$versionDirName'(git 短 sha 应为 12 位小写 hex)"
}

$channelArgs = @{ Channel = $channel }
if (-not [string]::IsNullOrWhiteSpace($ArtifactRoot)) { $channelArgs.Override = $ArtifactRoot }
$channelRoot = Get-ChannelRoot @channelArgs
$finalDir = Join-Path $channelRoot 'images' $versionDirName

Write-PublishInfo "channel=$channel version=$versionDirName tag=$($script:ImageTag) commit=$($stamp.Commit) families=$($families -join ',')"

if ([System.IO.Directory]::Exists($finalDir)) {
    if (-not $SkipIfExists) {
        throw "版本已发布且不可覆盖:$finalDir($channel 轨制品不可变;新内容请提交新 commit 或提升版本号)"
    }
    # 目录名相同不等于出自同一提交:release 轨目录名只有版本号,换了提交重跑同一个 -Version
    # 若静默 exit 0,CI 会以为新提交已经出包,实际离线包还是旧代码。
    $existingInfoPath = Join-Path $finalDir 'build-info.json'
    $existingCommit = ''
    if ([System.IO.File]::Exists($existingInfoPath)) {
        $existingCommit = [string](Get-Content -LiteralPath $existingInfoPath -Raw | ConvertFrom-Json -AsHashtable)['commit']
    }
    if ($existingCommit -cne $stamp.Commit) {
        throw "版本 $versionDirName 已由其他提交发布(已发布 commit='$existingCommit',当前 commit=$($stamp.Commit)),-SkipIfExists 不能掩盖这种冲突:$finalDir。新提交请用新版本号。"
    }
    Write-PublishOk "版本已发布,跳过(制品不可变):$finalDir"
    exit 0
}

# ─────────────────────────────────────────────────────────────────
# 3. 构建前能做完的检查(配置表、符号、docker),避免白构建
# ─────────────────────────────────────────────────────────────────

# 构建前算:镜像按 generated/tables 打包,表缺失时 Get-TablesDigest 直接抛,不必等构建完才发现
$tablesDigest = Get-TablesDigest -TablesDir (Join-Path $RepoRoot 'generated' 'tables')

$symbolsDir = Join-Path $RepoRoot 'bin' 'symbols'
$symbolFiles = @()
if ($PSBoundParameters.ContainsKey('IncludeSymbols')) {
    if ($IncludeSymbols) {
        # 符号只对应 C++ 二进制;不发 C++ 镜像却带符号,符号与包里任何镜像都对不上
        if ($families -notcontains 'cpp') { throw "-IncludeSymbols 只对 C++ 运行镜像有意义,但 -Families 不含 cpp。" }
        $symbolFiles = Get-SymbolFiles -SymbolsDir $symbolsDir
        if ($symbolFiles.Count -eq 0) {
            throw "指定了 -IncludeSymbols,但 $symbolsDir 下没有 *.debug(先在 Linux 上跑 tools/scripts/build_linux.sh 分离符号)。"
        }
    }
}
elseif ($families -contains 'cpp') {
    $symbolFiles = Get-SymbolFiles -SymbolsDir $symbolsDir
}

if ($null -eq (Get-Command $DockerCommand -ErrorAction SilentlyContinue)) {
    throw "找不到 docker 命令:'$DockerCommand'。导出离线包需要本机 docker(Docker Desktop 或 Linux dockerd)。"
}

# ─────────────────────────────────────────────────────────────────
# 4. 构建
# ─────────────────────────────────────────────────────────────────

if ($SkipBuild) {
    Write-PublishInfo "-SkipBuild:不构建,只收集已有镜像(revision label 必须等于 $($stamp.Commit))"
}
else {
    foreach ($family in $families) {
        $spec = $script:FamilySpecs[$family]
        $familyArgs = Get-ImageScriptArgs -Family $family

        if ($family -eq 'cpp') {
            Write-PublishStep "C++ 运行镜像:检查 Linux 运行时 staging"
            $pre = Invoke-ImageScript -Family $family -ScriptCommand 'preflight' -Arguments $familyArgs
            if ($pre.ExitCode -ne 0) {
                throw "C++ 运行镜像的 Linux 运行时 staging 不完整($($spec.Script) -Command preflight exit=$($pre.ExitCode),缺失项见上方 missing=)。先在 Linux 上跑 tools/scripts/build_linux.sh,再用 tools/scripts/k8s_stage_runtime.ps1 -BinarySourceRoot <Linux 产物目录> 填充 deploy/k8s/runtime/linux;这次不发 C++ 镜像就加 -Families go,java。"
            }
        }

        Write-PublishStep "构建 $family 镜像($($spec.Script) -Command $($spec.BuildCommand))"
        $build = Invoke-ImageScript -Family $family -ScriptCommand $spec.BuildCommand -Arguments $familyArgs
        if ($build.ExitCode -ne 0) {
            throw "$family 镜像构建失败($($spec.Script) -Command $($spec.BuildCommand) exit=$($build.ExitCode)),不发布。"
        }
    }
}

# ─────────────────────────────────────────────────────────────────
# 5. 期望镜像清单 + 本地镜像身份核对
# ─────────────────────────────────────────────────────────────────

Write-PublishStep "收集镜像清单(list-refs)"
$expected = New-Object System.Collections.Generic.List[hashtable]
$seenRefs = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::Ordinal)
foreach ($family in $families) {
    $familyRefs = Get-FamilyImageRefs -Family $family -Arguments (Get-ImageScriptArgs -Family $family)
    foreach ($ref in $familyRefs) {
        # 镜像脚本若忽略了传进去的 tag,清单指向的就是别的版本
        if ((Get-ImageTagFromRef -ImageRef $ref) -cne $script:ImageTag) {
            throw "$family 镜像引用的 tag 不是本次版本 tag:$ref(期望 :$($script:ImageTag))"
        }
        if (-not $seenRefs.Add($ref)) { throw "镜像引用重复出现:$ref" }
        $expected.Add(@{ Family = $family; Ref = $ref; ImageId = ''; Revision = '' })
        Write-PublishInfo "  $family  $ref"
    }
}

Write-PublishStep "核对本地镜像(必须存在,revision label == $($stamp.Commit))"
$missing = New-Object System.Collections.Generic.List[string]
$wrongRevision = New-Object System.Collections.Generic.List[string]
foreach ($item in $expected) {
    $identity = Get-DockerImageIdentity -Ref $item.Ref
    if ($null -eq $identity) { $missing.Add($item.Ref); continue }
    if ($identity.Revision -cne $stamp.Commit) {
        $wrongRevision.Add("$($item.Ref)(revision='$($identity.Revision)')")
        continue
    }
    $item.ImageId = $identity.Id
    $item.Revision = $identity.Revision
}
if ($missing.Count -gt 0 -or $wrongRevision.Count -gt 0) {
    $lines = New-Object System.Collections.Generic.List[string]
    if ($missing.Count -gt 0) {
        $lines.Add("以下镜像本地不存在($($missing.Count) 个):")
        foreach ($m in $missing) { $lines.Add("  - $m") }
    }
    if ($wrongRevision.Count -gt 0) {
        $lines.Add("以下镜像的 org.opencontainers.image.revision 与当前提交 $($stamp.Commit) 不一致($($wrongRevision.Count) 个,旧镜像或别的提交构建的,不能打进本版本):")
        foreach ($w in $wrongRevision) { $lines.Add("  - $w") }
    }
    $hint = if ($SkipBuild) { '去掉 -SkipBuild 按当前提交重新构建。' } else { '检查镜像脚本是否把 BUILD_COMMIT 写进了 revision label。' }
    throw "镜像核对未通过,拒绝发布。`n$($lines -join "`n")`n$hint"
}

# ─────────────────────────────────────────────────────────────────
# 6. staging:逐个导出 + 清单 + 校验和,rename 上线
# ─────────────────────────────────────────────────────────────────

Write-PublishStep "导出 $($expected.Count) 个镜像 → $finalDir"
$staging = New-AtomicStaging -FinalDir $finalDir
try {
    $imagesOut = Join-Path $staging 'images'
    [System.IO.Directory]::CreateDirectory($imagesOut) | Out-Null

    # Windows 文件系统大小写不敏感,按不敏感判重,免得两个镜像在 Windows 上写成同一个文件
    $usedNames = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::OrdinalIgnoreCase)
    $manifestEntries = New-Object System.Collections.Generic.List[object]
    foreach ($item in $expected) {
        $fileName = ($item.Ref -replace '[/:]', '_') + '.tar'
        if (-not $usedNames.Add($fileName)) { throw "两个镜像引用映射到了同一个归档文件名:$fileName" }
        $tarPath = Join-Path $imagesOut $fileName

        Write-PublishInfo "docker save $($item.Ref)"
        & $DockerCommand save -o $tarPath $item.Ref | Out-Host
        $saveExit = $LASTEXITCODE
        if ($saveExit -ne 0 -or -not [System.IO.File]::Exists($tarPath)) {
            throw "docker save 失败:$($item.Ref)(exit=$saveExit)"
        }

        # 读回归档自证:tar 里必须恰好是这一个 tag。docker save 按名字解析镜像,
        # 名字解析到别处或归档不完整时,这里是离线包出门前最后一道闸。
        $tags = Get-ImageArchiveRepoTags -ArchivePath $tarPath
        if ($tags.Count -ne 1 -or $tags[0] -cne $item.Ref) {
            throw "归档内 RepoTags 与期望不一致:$fileName 实际 [$($tags -join ', ')],期望 [$($item.Ref)]"
        }

        $repo = Get-ReleaseImageRepository -ImageRef $item.Ref
        $manifestEntries.Add([ordered]@{
                family   = $item.Family
                # service 只给人看(去掉统一的 mmorpg- 前缀);镜像身份以 ref / image_id 为准
                service  = ($repo.Substring($repo.LastIndexOf('/') + 1) -replace '^mmorpg-', '')
                ref      = $item.Ref
                image_id = $item.ImageId
                revision = $item.Revision
                file     = "images/$fileName"
            })
    }

    if ($symbolFiles.Count -gt 0) {
        $symbolsOut = Join-Path $staging 'symbols'
        [System.IO.Directory]::CreateDirectory($symbolsOut) | Out-Null
        foreach ($f in $symbolFiles) {
            Copy-Item -LiteralPath $f.FullName -Destination (Join-Path $symbolsOut $f.Name)
        }
        Write-PublishInfo "归档 C++ 分离符号 $($symbolFiles.Count) 个(来自 $symbolsDir)"
    }

    Write-Utf8NoBomFile -Path (Join-Path $staging 'images-manifest.json') `
        -Text ((ConvertTo-Json -InputObject @($manifestEntries) -Depth 5) + "`n")

    $buildInfo = [ordered]@{
        version           = $versionDirName
        channel           = $channel
        # 注入镜像的发布版本号;快照轨为空串,make_release.ps1 据此拒绝拿快照当发布版本
        app_version       = $script:ReleaseVersion
        vcs               = 'git'
        source_rev        = "g$($stamp.Commit)"
        commit            = $stamp.Commit
        dirty             = [bool]$stamp.Dirty
        image_tag         = $script:ImageTag
        families          = @($families)
        image_count       = $manifestEntries.Count
        tables_sha256     = $tablesDigest.Sha256
        tables_file_count = $tablesDigest.FileCount
        # InvariantCulture:自定义格式里的 ':' 是区域时间分隔符,个别区域设置下会变成 '.'
        published_at      = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)
        machine           = [System.Environment]::MachineName
        publisher         = [System.Environment]::UserName
    }
    Write-Utf8NoBomFile -Path (Join-Path $staging 'build-info.json') -Text (($buildInfo | ConvertTo-Json -Depth 5) + "`n")

    New-Sha256Sums -Dir $staging | Out-Null
    Complete-AtomicDir -Staging $staging -FinalDir $finalDir
}
catch {
    if ([System.IO.Directory]::Exists($staging)) {
        Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue
    }
    throw
}

# latest.json 是唯一可变文件;版本目录上线之后再动指针,指针永远不会指向不存在的版本
Set-LatestPointer -ChannelRoot $channelRoot -Kind images -Version $versionDirName | Out-Null

Write-PublishOk "镜像离线包已发布 [$channel] ${versionDirName}:$finalDir($($expected.Count) 个镜像,符号 $($symbolFiles.Count) 个)"
if ($channel -eq 'release') {
    Write-PublishInfo "下一步:在 CHANGELOG.md 写好 [$($versionCheck.Normalized)] 段后跑 make_release.ps1 -Version $($script:ReleaseVersion)"
}
exit 0
