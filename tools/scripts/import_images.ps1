#requires -Version 7
<#
.SYNOPSIS
    把一个镜像版本目录(fetch_images.ps1 的落地目录,或制品根里的版本目录)离线导入本机 Docker。

.DESCRIPTION
    对标 A 仓 tools/scripts/import_images.ps1,按 B 的制品布局改写:
      1. 先 Test-Sha256Sums。缺 sha256sums.txt 默认拒绝:publish / fetch 产出的目录必定带它,
         缺失只可能是手工拷贝,或有人删掉校验文件后连同清单一起改了 tar —— 那时清单里的 image_id
         同样可被改写,防不住。确认来源可信的手工目录才加 -AllowMissingChecksums 降级为 WARN
      2. 按 images-manifest.json 逐个 `docker load -i images/<file>`
         (B 的 publish 每个镜像单独 docker save,规避 A 仓"层链相同的镜像批量 save 丢一个"的坑)
      3. 每个 load 完立刻 `docker image inspect --format '{{.Id}}' <ref>` 核对身份 —— 同名 tag 在本机
         已有旧镜像、tar 里装的不是清单说的那个,都会在这里暴露,而不是部署后才发现跑的是另一份代码。
         先比清单 image_id;不等时按归档复核(见 Get-ArchiveImageDigests):.Id 的口径随 Docker 镜像存储而变,
         经典存储(overlay2 等)是 config digest,containerd 存储(Docker Desktop 4.34+、Docker Engine 29+
         新装默认)是 index / manifest digest,发布机与本机存储不同时同一个 tar 导入后 .Id 必然不等

    清单字段 file 是 images/ 下的文件名;带 "images/" 前缀的相对写法也接受,
    其余形态(含 ..、绝对路径、子目录)一律拒绝。

    docker 命令:-DockerCommand > 环境变量 MMORPG_DOCKER_COMMAND > docker。
    契约测试传一个桩 .ps1 路径,用 & 调用,不依赖真实 docker。

    退出码:0 = 全部导入且 ID 一致;1 = 失败(错误文本以 [ERR ] 开头)。

.EXAMPLE
    pwsh -File tools/scripts/import_images.ps1 -Dir deploy/offline-images/g0123456789ab
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Dir,

    [string]$DockerCommand = 'docker',

    # 缺 sha256sums.txt 时仍导入(只给 WARN);只用于确认来源可信的手工拷贝目录
    [switch]$AllowMissingChecksums
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lib' 'artifacts_lib.ps1')

<#
.SYNOPSIS
    一次顺序扫描读出 tar 里若干根目录文本条目,返回 条目名 -> 文本;不存在的条目不出现在结果里。

.DESCRIPTION
    与 publish_images.ps1 的 Read-TarEntryText 同一取法:优先 .NET System.Formats.Tar(pwsh 7.3+,
    跨平台一致;对可 seek 的文件流跳过条目数据靠 seek,不读 GB 级层数据),老运行时退回系统 tar。
#>
function Read-ArchiveTextEntries {
    param(
        [Parameter(Mandatory = $true)][string]$ArchivePath,
        [Parameter(Mandatory = $true)][string[]]$EntryNames
    )

    $result = @{}
    try { Add-Type -AssemblyName System.Formats.Tar -ErrorAction Stop } catch { }
    if ($null -ne ('System.Formats.Tar.TarReader' -as [type])) {
        $stream = [System.IO.File]::OpenRead($ArchivePath)
        try {
            $reader = [System.Formats.Tar.TarReader]::new($stream, $true)
            try {
                while ($result.Count -lt $EntryNames.Count) {
                    $entry = $reader.GetNextEntry($false)
                    if ($null -eq $entry) { break }
                    $name = $entry.Name -replace '^\./', ''
                    if ($EntryNames -cnotcontains $name -or $null -eq $entry.DataStream) { continue }
                    # leaveOpen:DataStream 归 TarReader 管,提前释放会让下一次 GetNextEntry 读到已释放的流
                    $sr = [System.IO.StreamReader]::new($entry.DataStream, [System.Text.UTF8Encoding]::new($false), $false, 4096, $true)
                    try { $result[$name] = $sr.ReadToEnd() } finally { $sr.Dispose() }
                }
            }
            finally { $reader.Dispose() }
        }
        finally { $stream.Dispose() }
        return $result
    }

    # 条目不存在时 tar 往 stderr 写一行并返回非 0;pwsh 7.0/7.1 在 Stop 下会把重定向的原生 stderr 当终止错误
    $ErrorActionPreference = 'Continue'
    foreach ($n in $EntryNames) {
        $global:LASTEXITCODE = 0
        $text = (& tar -xOf $ArchivePath $n 2>$null | Out-String)
        if ($LASTEXITCODE -eq 0) { $result[$n] = $text }
    }
    return $result
}

<#
.SYNOPSIS
    docker save 归档里这个镜像的身份集合,返回 @{ Digests = HashSet[string]; Reason = <读取失败原因|''> }。

.DESCRIPTION
    集合 = manifest.json 各条 Config 指向的 config digest(经典存储下的 .Id)
         ∪ index.json 顶层 descriptor 的 digest(containerd 存储下的 .Id,即 image 的 target)。
    Docker 25+ 两种存储的 docker save 都同时写 manifest.json 与 index.json,所以任一存储导出、
    任一存储导入,清单 image_id 与本机 .Id 都应落在集合里。
    已知盲区:Docker 25 之前的经典存储导出的 tar 没有 index.json,导入 containerd 存储时 .Id 不在集合里,
    会按"镜像 ID 不符"失败(fail-closed,不会放行错镜像)。
#>
function Get-ArchiveImageDigests {
    param([Parameter(Mandatory = $true)][string]$ArchivePath)

    $digests = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    try {
        $texts = Read-ArchiveTextEntries -ArchivePath $ArchivePath -EntryNames @('manifest.json', 'index.json')
        if ($texts.ContainsKey('manifest.json')) {
            foreach ($m in @($texts['manifest.json'] | ConvertFrom-Json -AsHashtable)) {
                if ($m -isnot [System.Collections.IDictionary]) { continue }
                # 新格式 "blobs/sha256/<hex>",Docker 25 之前 "<hex>.json"
                $hit = [regex]::Match([string]$m['Config'], '(?:^|/)([0-9a-f]{64})(?:\.json)?$')
                if ($hit.Success) { [void]$digests.Add('sha256:' + $hit.Groups[1].Value) }
            }
        }
        if ($texts.ContainsKey('index.json')) {
            $index = $texts['index.json'] | ConvertFrom-Json -AsHashtable
            if ($index -is [System.Collections.IDictionary]) {
                foreach ($d in @($index['manifests'])) {
                    if ($d -is [System.Collections.IDictionary] -and [string]$d['digest'] -cmatch '^sha256:[0-9a-f]{64}$') {
                        [void]$digests.Add([string]$d['digest'])
                    }
                }
            }
        }
        $reason = if ($digests.Count -eq 0) { '归档里没有可识别的 manifest.json / index.json' } else { '' }
        return @{ Digests = $digests; Reason = $reason }
    }
    catch {
        return @{ Digests = $digests; Reason = $_.Exception.Message }
    }
}

# 清单 file 字段 -> images/ 下的纯文件名;非法返回空串。
function Resolve-ManifestTarName {
    param([string]$File)
    if ([string]::IsNullOrWhiteSpace($File)) { return '' }
    $name = $File.Replace('\', '/')
    if ($name.StartsWith('images/', [System.StringComparison]::Ordinal)) { $name = $name.Substring('images/'.Length) }
    if ($name -cnotmatch '^[0-9A-Za-z_][0-9A-Za-z._-]*\.tar$' -or $name.Contains('..')) { return '' }
    return $name
}

try {
    $docker = if ($PSBoundParameters.ContainsKey('DockerCommand')) {
        $DockerCommand
    } elseif (-not [string]::IsNullOrWhiteSpace($env:MMORPG_DOCKER_COMMAND)) {
        $env:MMORPG_DOCKER_COMMAND
    } else {
        'docker'
    }
    if ($null -eq (Get-Command $docker -ErrorAction SilentlyContinue)) {
        throw "找不到 docker 命令:$docker(请先安装并启动 Docker,或用 -DockerCommand 指定)"
    }

    $dirFull = Resolve-ArtifactFullPath -Path $Dir
    if (-not [System.IO.Directory]::Exists($dirFull)) { throw "镜像版本目录不存在:$dirFull" }

    if ([System.IO.File]::Exists((Join-Path $dirFull 'sha256sums.txt'))) {
        Write-Host "[INFO] 校验完整性:$dirFull" -ForegroundColor Cyan
        Test-Sha256Sums -Dir $dirFull
    } elseif ($AllowMissingChecksums) {
        Write-Host "[WARN] $dirFull 没有 sha256sums.txt,按 -AllowMissingChecksums 跳过完整性校验(仍会逐个核对镜像 ID)" -ForegroundColor Yellow
    } else {
        throw "缺少 sha256sums.txt,拒绝导入:$dirFull(publish / fetch 产出的目录必定带它;确认来源可信的手工拷贝目录可加 -AllowMissingChecksums)"
    }

    $manifestPath = Join-Path $dirFull 'images-manifest.json'
    if (-not [System.IO.File]::Exists($manifestPath)) { throw "缺少镜像清单:$manifestPath" }
    try {
        $entries = @(Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json)
    }
    catch {
        throw "镜像清单无法解析:$manifestPath($($_.Exception.Message))"
    }
    if ($entries.Count -eq 0) { throw "镜像清单为空:$manifestPath" }

    # 先把整份清单校验完再动 docker:半路才发现第 5 条非法时,前 4 个已经 load 进去了
    $plan = New-Object System.Collections.Generic.List[object]
    $seenRefs = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    $index = 0
    foreach ($e in $entries) {
        $index++
        $ref = [string]$e.ref
        $imageId = [string]$e.image_id
        $tarName = Resolve-ManifestTarName -File ([string]$e.file)
        if ([string]::IsNullOrWhiteSpace($ref) -or $ref -match '\s') { throw "镜像清单第 $index 条 ref 非法:'$ref'" }
        if ($imageId -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "镜像清单第 $index 条 image_id 非法($ref):'$imageId'" }
        if (-not $tarName) { throw "镜像清单第 $index 条 file 非法($ref):'$($e.file)'(应为 images/ 下的 .tar 文件名)" }
        if (-not $seenRefs.Add($ref)) { throw "镜像清单内 ref 重复:$ref" }
        $tarPath = Join-Path $dirFull 'images' $tarName
        if (-not [System.IO.File]::Exists($tarPath)) { throw "镜像包缺失($ref):$tarPath" }
        $plan.Add([pscustomobject]@{ Ref = $ref; ImageId = $imageId; TarPath = $tarPath })
    }

    Write-Host "[INFO] 待导入 $($plan.Count) 个镜像" -ForegroundColor Cyan
    $mismatches = New-Object System.Collections.Generic.List[string]
    foreach ($item in $plan) {
        Write-Host "[INFO] docker load -i $($item.TarPath)" -ForegroundColor Cyan
        # 不合并 stderr(2>&1):pwsh 7.0/7.1 在 ErrorActionPreference=Stop 下会把原生 stderr 当终止错误
        $global:LASTEXITCODE = 0
        $loadOut = & $docker load -i $item.TarPath
        if ($LASTEXITCODE -ne 0) { throw "docker load 失败(exit=$LASTEXITCODE):$($item.TarPath)" }
        if ($loadOut) { $loadOut | ForEach-Object { Write-Host "       $_" } }

        $global:LASTEXITCODE = 0
        $inspectOut = & $docker image inspect --format '{{.Id}}' $item.Ref
        if ($LASTEXITCODE -ne 0) { throw "导入后查不到镜像 $($item.Ref)(docker image inspect exit=$LASTEXITCODE)" }
        $actualId = ([string]($inspectOut | Out-String)).Trim()
        if ([string]::Equals($actualId, $item.ImageId, [System.StringComparison]::OrdinalIgnoreCase)) {
            Write-Host "[ OK ] $($item.Ref) = $actualId" -ForegroundColor Green
            continue
        }

        # 不等不一定是错镜像,可能只是发布机与本机镜像存储不同导致 .Id 口径不同:按归档复核。
        # 清单 image_id 与本机 .Id 都必须是这个 tar 里镜像的身份之一 —— tar 已被 sha256sums.txt 覆盖,
        # 本机 .Id 落在集合里即证明该 tag 指向的正是 tar 里的镜像;任一不在集合里就判不符
        $archive = Get-ArchiveImageDigests -ArchivePath $item.TarPath
        if ($archive.Digests.Contains($item.ImageId) -and $archive.Digests.Contains($actualId)) {
            Write-Host "[ OK ] $($item.Ref) = $actualId(清单记录 $($item.ImageId):发布机与本机镜像存储口径不同,已按归档内 digest 核对一致)" -ForegroundColor Green
            continue
        }
        $archiveNote = if ($archive.Reason) { "归档复核失败:$($archive.Reason)" } else { "归档内镜像身份 [$(@($archive.Digests) -join ', ')]" }
        $mismatches.Add("$($item.Ref) 期望 $($item.ImageId) 实际 $actualId($archiveNote)")
        Write-Host "[ERR ] 镜像 ID 不符:$($item.Ref)" -ForegroundColor Red
    }

    if ($mismatches.Count -gt 0) {
        throw "镜像 ID 不符($($mismatches.Count) 个,本机该 tag 指向的不是清单里的镜像,已按归档内 config / manifest digest 复核仍不符,禁止继续部署):`n$($mismatches -join "`n")"
    }
    Write-Host "[ OK ] 导入完成:$($plan.Count) 个镜像,ID 与清单一致" -ForegroundColor Green
    exit 0
}
catch {
    Write-Host "[ERR ] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
