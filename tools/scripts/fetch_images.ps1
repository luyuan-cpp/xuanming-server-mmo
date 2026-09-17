#requires -Version 7
<#
.SYNOPSIS
    从制品目录拉取一个镜像版本到本机(默认 <仓库>/deploy/offline-images/<版本>),校验通过才落地。

.DESCRIPTION
    对标 A 仓 tools/scripts/fetch_offline_images.ps1,按 B 的制品布局改写:
      源:<制品根>/{snapshots|releases}/images/<版本>/(images/*.tar、images-manifest.json、
          build-info.json、symbols/、sha256sums.txt)
      目的:整个版本目录原样复制,之后交给 import_images.ps1 -Dir <落地目录> 导入 Docker。

    顺序(每一步失败都不留下半成品):
      1. 对源目录 Test-Sha256Sums —— 共享盘上的制品被改坏时不复制
      2. 核对源目录 build-info.json 的 channel / version 与请求一致 —— 校验和不覆盖目录名,
         手工把快照目录复制成 releases/images/v1.2.3 或改名成别的 g<sha> 时在这里拦下
      3. 复制到 OutDir 同级临时目录 .fetching-<名>-<PID>-<guid>
      4. 对临时目录再 Test-Sha256Sums —— 抓复制过程/网络盘传输出的坏字节
      5. rename 成 OutDir。OutDir 已存在时:无 -Force 拒绝;有 -Force 也是临时目录完整就位后
         才把旧目录挪开、换上新目录,再删旧目录;换上失败则把旧目录挪回去

    -Force 只替换"以前 fetch 落地的版本目录"(或空目录):顶层必须有 sha256sums.txt 与
    images-manifest.json,且只含 images/、symbols/、build-info.json 这几类条目;符号链接 / 联接点、
    等于或包含仓库根与当前目录的路径一律拒绝。把父目录(如 deploy/offline-images)、deploy、"."
    误传成 OutDir 时,替换 = 整棵树挪走再递归删除,仓库里未提交的改动也救不回来。

    制品根:-ArtifactRoot > 环境变量 MMORPG_ARTIFACT_ROOT > <仓库父目录>/artifacts
    (目标机经共享盘访问制品根时,把 MMORPG_ARTIFACT_ROOT 设成对应挂载路径即可)。
    制品根必须已存在,不会被创建:路径写错或共享盘没挂上时直接报"制品根不存在"。

    退出码:0 = 成功;1 = 失败(错误文本以 [ERR ] 开头)。

.EXAMPLE
    # 拉最新快照(读 snapshots/images/latest.json)
    pwsh -File tools/scripts/fetch_images.ps1

.EXAMPLE
    # 拉正式版本,落到指定目录;目录已存在时覆盖
    pwsh -File tools/scripts/fetch_images.ps1 -Channel release -Version v1.2.3 -OutDir /data/offline/v1.2.3 -Force
#>
[CmdletBinding()]
param(
    # 快照轨默认;正式版用 -Channel release -Version vX.Y.Z
    [ValidateSet('snapshot', 'release')]
    [string]$Channel = 'snapshot',

    # 发布轨必填(vX.Y.Z);快照轨缺省读 latest.json,指定时形如 g<12 位 sha>
    [string]$Version = '',

    [string]$ArtifactRoot = '',

    # 缺省 <仓库>/deploy/offline-images/<版本>(已被 .gitignore 忽略)
    [string]$OutDir = '',

    # OutDir 已存在且是以前 fetch 落地的版本目录时替换它(仍然先完整校验新内容再交换)
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$ScriptDir = $PSScriptRoot
$RepoRoot = Split-Path -Parent (Split-Path -Parent $ScriptDir)
. (Join-Path $ScriptDir 'lib' 'artifacts_lib.ps1')
. (Join-Path $ScriptDir 'lib' 'release_common.ps1')

# 路径 A 是否等于 B 或位于 B 之下。Windows 文件系统大小写不敏感,Linux 敏感。
function Test-PathWithin {
    param([string]$Path, [string]$Container)
    $cmp = if ($IsWindows) { [System.StringComparison]::OrdinalIgnoreCase } else { [System.StringComparison]::Ordinal }
    if ([string]::Equals($Path, $Container, $cmp)) { return $true }
    $prefix = $Container.TrimEnd([char[]]@('/', '\')) + [System.IO.Path]::DirectorySeparatorChar
    return $Path.StartsWith($prefix, $cmp)
}

# 逐文件复制(含隐藏文件)。不用 Copy-Item -Recurse:它对隐藏文件与"目标已存在时嵌套一层"的
# 行为随版本变化,而这里要求复制结果与源逐文件一致,多一层目录就会被校验判为清单外文件。
function Copy-ArtifactTree {
    param([string]$Source, [string]$Destination)
    [System.IO.Directory]::CreateDirectory($Destination) | Out-Null
    foreach ($f in Get-ChildItem -LiteralPath $Source -Recurse -File -Force) {
        $rel = [System.IO.Path]::GetRelativePath($Source, $f.FullName)
        $dst = Join-Path $Destination $rel
        [System.IO.Directory]::CreateDirectory((Split-Path -Parent $dst)) | Out-Null
        [System.IO.File]::Copy($f.FullName, $dst, $false)
    }
}

# 已存在的 OutDir 是否可以被 -Force 替换:空目录,或顶层形如布局 F 版本目录的 fetch 落地目录。
# 只看顶层条目名,不校验内容 —— 这里要回答的是"它是不是一个落地目录",不是"它是否完好"。
function Test-FetchLandingShape {
    param([string]$Dir)
    $entries = @(Get-ChildItem -LiteralPath $Dir -Force)
    if ($entries.Count -eq 0) { return $true }
    foreach ($e in $entries) {
        if (($e.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { return $false }
        $allowed = if ($e.PSIsContainer) { @('images', 'symbols') } else { @('build-info.json', 'images-manifest.json', 'sha256sums.txt') }
        if ($allowed -cnotcontains $e.Name) { return $false }
    }
    return ([System.IO.File]::Exists((Join-Path $Dir 'sha256sums.txt')) -and
        [System.IO.File]::Exists((Join-Path $Dir 'images-manifest.json')))
}

$tmpDir = ''
try {
    # -MustExist:只读用途不创建制品根,路径写错时报"制品根不存在"而不是误导性的"制品不存在"
    $artifactRootFull = Get-ArtifactRoot -Override $ArtifactRoot -MustExist
    $channelRoot = Get-ChannelRoot -Channel $Channel -Override $ArtifactRoot -MustExist
    $imagesRoot = Join-Path $channelRoot 'images'

    if ([string]::IsNullOrWhiteSpace($Version)) {
        if ($Channel -eq 'release') { throw '发布轨必须指定 -Version(如 v1.2.3)。' }
        $latest = Join-Path $imagesRoot 'latest.json'
        if (-not [System.IO.File]::Exists($latest)) {
            throw "未指定 -Version 且找不到快照指针 $latest;先在构建机发布快照(publish_images.ps1)。"
        }
        try {
            $Version = [string](Get-Content -LiteralPath $latest -Raw | ConvertFrom-Json).version
        }
        catch {
            throw "latest.json 无法解析:$latest($($_.Exception.Message))"
        }
        if ([string]::IsNullOrWhiteSpace($Version)) { throw "latest.json 缺少 version 字段:$latest" }
        Write-Host "[INFO] latest.json 指向快照 $Version" -ForegroundColor Cyan
    }

    # 版本号同时是目录名:先按轨道校验形状,杜绝 "../" 之类穿越出制品根
    if ($Channel -eq 'release') {
        $check = Test-ReleaseVersion -Version $Version
        if (-not $check.Ok) { throw "发布版本号非法:'$Version'($($check.Reason))" }
    } elseif (-not (Test-SnapshotVersionName -Version $Version)) {
        throw "快照版本号非法:'$Version'(应为 g<12 位小写 sha>,脏树为 g<sha12>-dirty-yyyyMMdd-HHmmss)"
    }

    $verDir = Join-Path $imagesRoot $Version
    if (-not [System.IO.Directory]::Exists($verDir)) { throw "制品不存在:$verDir(频道 $Channel)" }

    if ([string]::IsNullOrWhiteSpace($OutDir)) {
        $OutDir = Join-Path $RepoRoot 'deploy' 'offline-images' $Version
    }
    $outFull = Resolve-ArtifactFullPath -Path $OutDir
    # -Force 会删 OutDir:OutDir 与制品根互相包含时,一次误操作就能毁掉不可变制品
    if ((Test-PathWithin -Path $outFull -Container $artifactRootFull) -or (Test-PathWithin -Path $artifactRootFull -Container $outFull)) {
        throw "OutDir 不得位于制品根之内,也不得包含制品根:OutDir=$outFull,制品根=$artifactRootFull"
    }
    # 仓库根 / 当前目录被整个挪走删除无法挽回:明确拒绝,不只靠下面的落地目录形状判断兜底
    $repoRootFull = Resolve-ArtifactFullPath -Path $RepoRoot
    $cwdFull = Resolve-ArtifactFullPath -Path (Get-Location -PSProvider FileSystem).ProviderPath
    if ((Test-PathWithin -Path $repoRootFull -Container $outFull) -or (Test-PathWithin -Path $cwdFull -Container $outFull)) {
        throw "OutDir 不得等于或包含仓库根 / 当前目录:OutDir=$outFull,仓库根=$repoRootFull,当前目录=$cwdFull"
    }
    if ([System.IO.File]::Exists($outFull)) { throw "OutDir 是一个已存在的文件,不是目录:$outFull" }
    $outExists = [System.IO.Directory]::Exists($outFull)
    if ($outExists) {
        if (-not $Force) {
            throw "目标目录已存在:$outFull(它若是以前 fetch 落地的版本目录,确认替换请加 -Force;-Force 不会替换其它目录)"
        }
        # 挪走再递归删除一个链接,可能波及链接指向的真实目录(包括制品根里的版本目录)
        if (((Get-Item -LiteralPath $outFull -Force).Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "OutDir 是符号链接 / 联接点,-Force 不替换:$outFull"
        }
        if (-not (Test-FetchLandingShape -Dir $outFull)) {
            throw "OutDir 已存在且不是 fetch 落地目录,-Force 只替换旧的落地目录(顶层须有 sha256sums.txt 与 images-manifest.json,且只含 images/、symbols/、build-info.json):$outFull"
        }
    }

    Write-Host "[INFO] 校验源制品完整性:$verDir" -ForegroundColor Cyan
    Test-Sha256Sums -Dir $verDir

    # 校验和证明"目录内容没被改",证明不了"目录名对得上内容";与 make_release.ps1 同一口径核对身份
    $buildInfoPath = Join-Path $verDir 'build-info.json'
    if (-not [System.IO.File]::Exists($buildInfoPath)) { throw "制品缺少 build-info.json(非 publish_images.ps1 产物?):$verDir" }
    try {
        $buildInfo = [System.IO.File]::ReadAllText($buildInfoPath) | ConvertFrom-Json -AsHashtable
    }
    catch {
        throw "build-info.json 无法解析:$buildInfoPath($($_.Exception.Message))"
    }
    if ($buildInfo -isnot [System.Collections.IDictionary]) { throw "build-info.json 不是 JSON 对象:$buildInfoPath" }
    if ([string]$buildInfo['channel'] -cne $Channel -or [string]$buildInfo['version'] -cne $Version) {
        throw "镜像制品身份与目录不符:build-info channel='$($buildInfo['channel'])' version='$($buildInfo['version'])',请求 $Channel/$Version(勿手工搬动或改名制品目录)"
    }

    $outParent = Split-Path -Parent $outFull
    $outLeaf = Split-Path -Leaf $outFull
    [System.IO.Directory]::CreateDirectory($outParent) | Out-Null
    # 临时名带 guid 而不是"同名就先删":PID 会复用,删掉的可能是另一个 fetch 正在写的临时目录
    $tmpSuffix = "$PID-" + [guid]::NewGuid().ToString('N')
    $tmpDir = Join-Path $outParent (".fetching-" + $outLeaf + "-" + $tmpSuffix)

    Write-Host "[INFO] 复制到临时目录:$tmpDir" -ForegroundColor Cyan
    Copy-ArtifactTree -Source (Resolve-ArtifactFullPath -Path $verDir) -Destination $tmpDir

    Write-Host '[INFO] 复制完成,重新校验临时目录' -ForegroundColor Cyan
    Test-Sha256Sums -Dir $tmpDir

    if ($outExists) {
        $backup = Join-Path $outParent (".replaced-" + $outLeaf + "-" + $tmpSuffix)
        [System.IO.Directory]::Move($outFull, $backup)
        # OutDir 经符号链接祖先 / 挂载别名与源版本目录是同一个物理目录时,上面按字符串的包含判断拦不住;
        # 但挪走 OutDir 会让源版本目录跟着"消失" —— 立刻挪回并拒绝,绝不删除
        if (-not [System.IO.Directory]::Exists($verDir)) {
            [System.IO.Directory]::Move($backup, $outFull)
            throw "OutDir 与制品版本目录是同一个物理目录(经符号链接或挂载别名),拒绝替换:OutDir=$outFull,制品=$verDir"
        }
        try {
            [System.IO.Directory]::Move($tmpDir, $outFull)
        }
        catch {
            [System.IO.Directory]::Move($backup, $outFull)
            throw "替换目标目录失败,已恢复旧目录:$outFull($($_.Exception.Message))"
        }
        # 新目录已就位,旧目录删不掉(如 Windows 上被占用)不影响本次结果,只提示手工清理
        try {
            Remove-Item -LiteralPath $backup -Recurse -Force
        }
        catch {
            Write-Host "[WARN] 旧目录已挪开但删除失败,请手工清理:$backup($($_.Exception.Message))" -ForegroundColor Yellow
        }
        Write-Host "[INFO] 已替换旧目录:$outFull" -ForegroundColor Yellow
    } else {
        [System.IO.Directory]::Move($tmpDir, $outFull)
    }
    $tmpDir = ''

    $files = @(Get-ChildItem -LiteralPath $outFull -Recurse -File -Force)
    $sizeMB = [math]::Round((($files | Measure-Object -Property Length -Sum).Sum) / 1MB, 1)
    Write-Host "[ OK ] 已拉取 $Channel/$Version -> $outFull($($files.Count) 个文件,$sizeMB MB)" -ForegroundColor Green
    Write-Host "[INFO] 下一步:pwsh -File tools/scripts/import_images.ps1 -Dir `"$outFull`"" -ForegroundColor Cyan
    exit 0
}
catch {
    Write-Host "[ERR ] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
finally {
    if ($tmpDir -and [System.IO.Directory]::Exists($tmpDir)) {
        Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
