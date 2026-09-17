#requires -Version 7
<#
.SYNOPSIS
    把一个镜像版本目录(fetch_images.ps1 的落地目录,或制品根里的版本目录)离线导入本机 Docker。

.DESCRIPTION
    对标 A 仓 tools/scripts/import_images.ps1,按 B 的制品布局改写:
      1. 目录里有 sha256sums.txt 就先 Test-Sha256Sums(缺失时给 WARN 继续 —— 手工拷来的旧目录)
      2. 按 images-manifest.json 逐个 `docker load -i images/<file>`
         (B 的 publish 每个镜像单独 docker save,规避 A 仓"层链相同的镜像批量 save 丢一个"的坑)
      3. 每个 load 完立刻 `docker image inspect --format '{{.Id}}' <ref>`,
         必须等于清单里的 image_id —— 同名 tag 在本机已有旧镜像、tar 里装的不是清单说的那个,
         都会在这里暴露,而不是部署后才发现跑的是另一份代码

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

    [string]$DockerCommand = 'docker'
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lib' 'artifacts_lib.ps1')

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
    } else {
        Write-Host "[WARN] $dirFull 没有 sha256sums.txt,跳过完整性校验(仍会逐个核对镜像 ID)" -ForegroundColor Yellow
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
        if (-not [string]::Equals($actualId, $item.ImageId, [System.StringComparison]::OrdinalIgnoreCase)) {
            $mismatches.Add("$($item.Ref) 期望 $($item.ImageId) 实际 $actualId")
            Write-Host "[ERR ] 镜像 ID 不符:$($item.Ref)" -ForegroundColor Red
            continue
        }
        Write-Host "[ OK ] $($item.Ref) = $actualId" -ForegroundColor Green
    }

    if ($mismatches.Count -gt 0) {
        throw "镜像 ID 不符($($mismatches.Count) 个,本机该 tag 指向的不是清单里的镜像,禁止继续部署):`n$($mismatches -join "`n")"
    }
    Write-Host "[ OK ] 导入完成:$($plan.Count) 个镜像,ID 与清单一致" -ForegroundColor Green
    exit 0
}
catch {
    Write-Host "[ERR ] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
