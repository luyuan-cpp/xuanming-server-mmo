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
      2. 复制到 OutDir 同级临时目录 .fetching-<名>-<PID>
      3. 对临时目录再 Test-Sha256Sums —— 抓复制过程/网络盘传输出的坏字节
      4. rename 成 OutDir。OutDir 已存在时:无 -Force 拒绝;有 -Force 也是临时目录完整就位后
         才把旧目录挪开、换上新目录,再删旧目录;换上失败则把旧目录挪回去

    制品根:-ArtifactRoot > 环境变量 MMORPG_ARTIFACT_ROOT > <仓库父目录>/artifacts
    (目标机经共享盘访问制品根时,把 MMORPG_ARTIFACT_ROOT 设成对应挂载路径即可)。

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

    # OutDir 已存在时替换它(仍然先完整校验新内容再交换)
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

$tmpDir = ''
try {
    $artifactRootFull = Get-ArtifactRoot -Override $ArtifactRoot
    $channelRoot = Get-ChannelRoot -Channel $Channel -Override $ArtifactRoot
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
    if ([System.IO.File]::Exists($outFull)) { throw "OutDir 是一个已存在的文件,不是目录:$outFull" }
    $outExists = [System.IO.Directory]::Exists($outFull)
    if ($outExists -and -not $Force) {
        throw "目标目录已存在:$outFull(确认要替换请加 -Force)"
    }

    Write-Host "[INFO] 校验源制品完整性:$verDir" -ForegroundColor Cyan
    Test-Sha256Sums -Dir $verDir

    $outParent = Split-Path -Parent $outFull
    $outLeaf = Split-Path -Leaf $outFull
    [System.IO.Directory]::CreateDirectory($outParent) | Out-Null
    $tmpDir = Join-Path $outParent (".fetching-" + $outLeaf + "-" + $PID)
    if ([System.IO.Directory]::Exists($tmpDir)) { Remove-Item -LiteralPath $tmpDir -Recurse -Force }

    Write-Host "[INFO] 复制到临时目录:$tmpDir" -ForegroundColor Cyan
    Copy-ArtifactTree -Source (Resolve-ArtifactFullPath -Path $verDir) -Destination $tmpDir

    Write-Host '[INFO] 复制完成,重新校验临时目录' -ForegroundColor Cyan
    Test-Sha256Sums -Dir $tmpDir

    if ($outExists) {
        $backup = Join-Path $outParent (".replaced-" + $outLeaf + "-" + $PID)
        if ([System.IO.Directory]::Exists($backup)) { Remove-Item -LiteralPath $backup -Recurse -Force }
        [System.IO.Directory]::Move($outFull, $backup)
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
