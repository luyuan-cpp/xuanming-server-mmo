<#
.SYNOPSIS
    Build and push Docker images for Go micro-services.

.DESCRIPTION
    Iterates over the Go service catalogue and builds each service into its
    own Docker image using deploy/k8s/Dockerfile.go-svc.
    Image naming: {Registry}/{ServiceName}:{Tag}

.EXAMPLE
    # Build all Go service images
    pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all `
        -Registry ghcr.io/luyuancpp -Tag v1

    # Push all Go service images
    pwsh -File tools/scripts/go_svc_image.ps1 -Command push-all `
        -Registry ghcr.io/luyuancpp -Tag v1

    # Build + push in one shot
    pwsh -File tools/scripts/go_svc_image.ps1 -Command release-all `
        -Registry ghcr.io/luyuancpp -Tag v1
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("build-all", "push-all", "release-all", "list")]
    [string]$Command,

    [string]$Registry = "ghcr.io/luyuancpp",
    # 留空 = git 短 sha(脏树带 -dirty)。不再默认 latest:
    # 可变 tag 会让 `kubectl rollout undo` 退回同一个 digest。
    [string]$Tag = "",
    # 只处理目录里的这几个服务(逗号分隔,如 match,login)。留空 = 全部。
    # 目录是顺序遍历、一个失败就 throw,以前想单独构建 match 也得先等 db 过——
    # 而 go/db/go.mod 把 proto2mysql replace 到仓库外(../../../proto2mysql),
    # 以 go/ 为 build context 根本带不进镜像,db 一挂后面全都不构建。
    # (db 现在由 Get-ExternalBuildContexts 以 --build-context 带入仓库外目录,
    #  但宿主没检出 proto2mysql 时仍会失败,所以按需过滤依旧有用。)
    #
    # 类型是 [string[]] 而不是 [string]:从 PowerShell 里 `& go_svc_image.ps1 -Services a,b`
    # 调用时,逗号表达式先被解析成数组,[string] 参数会直接报
    # "Cannot convert value to type System.String" 而不是进到下面的 split;
    # `pwsh -File ... -Services "a,b"` 传的是单个字符串,两种形态这里都接。
    [string[]]$Services = @(),
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")
$GoRoot    = Join-Path $RepoRoot "go"
$Dockerfile = Join-Path $RepoRoot "deploy\k8s\Dockerfile.go-svc"
# 策划表(各 Go 服务 TableDir 默认 ../../generated/tables),随镜像打包
$TablesDir = Join-Path $RepoRoot "generated\tables"

. (Join-Path $ScriptDir "lib\release_common.ps1")

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
if ([string]::IsNullOrWhiteSpace($Tag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -Tag。"
    }
    $Tag = $script:ReleaseStamp.Tag
}

# 注入到镜像里的版本戳(ldflags + OCI label + /app/BUILD_INFO)
$BuildCommit = if ($script:ReleaseStamp.Ok) { $script:ReleaseStamp.Commit } else { "unknown" }
$BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

# Service catalogue: name → { Dir (in go/), Entry (.go file), ImageName }
$Catalogue = [ordered]@{
    db              = @{ Dir = "db";              Entry = "db.go";                  ImageName = "mmorpg-db" }
    "data-service"  = @{ Dir = "data_service";    Entry = "data_service.go";         ImageName = "mmorpg-data-service" }
    login           = @{ Dir = "login";           Entry = "login.go";               ImageName = "mmorpg-login" }
    "player-locator"= @{ Dir = "player_locator";  Entry = "player_locator.go";      ImageName = "mmorpg-player-locator" }
    "scene-manager" = @{ Dir = "scene_manager";   Entry = "scene_manager_service.go"; ImageName = "mmorpg-scene-manager" }
    # 与 k8s_deploy.ps1 $GoSvcCatalogue 的 match 条目配对(ImageName 必须一致,否则 infra-up 拉不到镜像)
    match           = @{ Dir = "match";           Entry = "match_service.go";       ImageName = "mmorpg-match" }
}

# 先把每个元素再按逗号拆一次,兼容 "a,b"(单字符串)和 a,b(数组)两种传法。
$wanted = @($Services | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })
if ($wanted.Count -gt 0) {
    $unknown = @($wanted | Where-Object { -not $Catalogue.Contains($_) })
    if ($unknown.Count -gt 0) {
        throw "未知的 Go 服务:$($unknown -join ', ')。可选:$($Catalogue.Keys -join ', ')"
    }
    $filtered = [ordered]@{}
    foreach ($name in $Catalogue.Keys) {
        if ($wanted -contains $name) { $filtered[$name] = $Catalogue[$name] }
    }
    $Catalogue = $filtered
}

function Get-ImageFullName {
    param([string]$ImageName)
    return "$Registry/${ImageName}:${Tag}"
}

# 仓库外的本地 replace 模块 → docker `--build-context <名字>=<宿主目录>`。
#
# 背景:go/db/go.mod 有 `replace github.com/luyuancpp/proto2mysql => ../../../proto2mysql`,
# 目标在仓库外,以 go/ 为 build context 带不进镜像,db 镜像以前在任何环境都构建不了。
# Dockerfile.go-svc 为它声明了一个空的 `FROM scratch AS proto2mysql` 占位 stage,
# BuildKit 允许命名上下文按名字覆盖同名 stage,所以这里只要把宿主目录传进去即可,
# go.mod 一行不用改。目录名(模块路径最后一段)就是 stage 名,两边必须一致。
#
# 只处理 Dockerfile 里有对应 stage 的模块(白名单),其它 replace(../proto、../shared)
# 本来就在 go/ 里,由 COPY 正常带入。
$script:ExternalReplaceStages = @("proto2mysql")

function Get-ExternalBuildContexts {
    param([Parameter(Mandatory = $true)][string]$ServiceDir)

    $goMod = Join-Path $ServiceDir "go.mod"
    if (-not (Test-Path $goMod)) { return @() }

    $contexts = @()
    foreach ($line in (Get-Content -Path $goMod)) {
        # 只匹配单行 `replace <module> => <本地路径>`;版本形式(=> mod v1.2.3)不含路径分隔符,跳过。
        if ($line -notmatch '^\s*replace\s+(\S+)\s+=>\s+(\.\.?[/\\][^\s]*)\s*$') { continue }
        $target = $Matches[2]
        $abs = [System.IO.Path]::GetFullPath((Join-Path $ServiceDir $target))
        # 还在 go/ 里的(../proto、../shared)由 Dockerfile 的 COPY 带入,不用管。
        $goRootFull = [System.IO.Path]::GetFullPath($GoRoot).TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
        if ($abs.StartsWith($goRootFull, [System.StringComparison]::OrdinalIgnoreCase)) { continue }

        $stage = Split-Path -Leaf $abs
        if ($script:ExternalReplaceStages -notcontains $stage) {
            throw "go.mod 里有指向仓库外的 replace($line),但 Dockerfile.go-svc 没有名为 '$stage' 的占位 stage;请先在 Dockerfile 里加 `FROM scratch AS $stage` + COPY,并把名字加进 go_svc_image.ps1 的 ExternalReplaceStages。"
        }
        if (-not (Test-Path $abs -PathType Container)) {
            throw "go.mod 里 replace 指向的仓库外目录不存在:$abs(来自 $goMod 的 `"$line`")。请先把该仓库检出到这个路径,否则镜像里 go build 找不到模块。"
        }
        $contexts += @("--build-context", "$stage=$abs")
    }
    return $contexts
}

function Invoke-BuildAll {
    if (-not (Test-Path $Dockerfile)) {
        throw "Dockerfile not found: $Dockerfile"
    }
    if (-not (Test-Path (Join-Path $TablesDir "*.json"))) {
        throw "策划表目录不存在或没有 *.json:$TablesDir。Go 服务镜像必须带表(TableDir=../../generated/tables),请先导表。"
    }

    foreach ($kv in $Catalogue.GetEnumerator()) {
        $svc  = $kv.Key
        $info = $kv.Value
        $fullImage = Get-ImageFullName -ImageName $info.ImageName

        Write-Host "[build] $svc -> $fullImage" -ForegroundColor Cyan

        $buildArgs = @(
            "build",
            "-f", $Dockerfile,
            "--build-arg", "SERVICE=$($info.Dir)",
            "--build-arg", "ENTRY=$($info.Entry)",
            "--build-arg", "BUILD_VERSION=$Tag",
            "--build-arg", "BUILD_COMMIT=$BuildCommit",
            "--build-arg", "BUILD_TIME=$BuildTime"
        )
        # 宿主设置了 GOPROXY(buildenv.ps1 → goproxy.cn)就透传给 Dockerfile 的 ARG GOPROXY,
        # 否则 builder 阶段走 proxy.golang.org,国内网络下 `go mod download` 会超时。
        if (-not [string]::IsNullOrWhiteSpace($env:GOPROXY)) {
            $buildArgs += @("--build-arg", "GOPROXY=$($env:GOPROXY)")
        }
        # 仓库外 replace 模块(目前只有 db 的 proto2mysql)以命名上下文带入
        $buildArgs += @(Get-ExternalBuildContexts -ServiceDir (Join-Path $GoRoot $info.Dir))
        # 策划表:仓库根 generated/tables 不在 go/ 里,固定以命名上下文 tables 带入
        # (Dockerfile.go-svc 的 `FROM scratch AS tables` 占位 stage)。
        # 找不到直接 throw:没有表的 login / player-locator / scene-manager 镜像起来就 CrashLoop。
        $buildArgs += @("--build-context", "tables=$TablesDir")
        $buildArgs += @("-t", $fullImage, $GoRoot)

        if ($DryRun) {
            Write-Host "  [dry-run] docker $($buildArgs -join ' ')" -ForegroundColor DarkGray
        } else {
            & docker @buildArgs
            if ($LASTEXITCODE -ne 0) {
                throw "Docker build failed for $svc"
            }
            Write-Host "  [ok] $fullImage" -ForegroundColor Green
        }
    }
}

function Invoke-PushAll {
    foreach ($kv in $Catalogue.GetEnumerator()) {
        $svc  = $kv.Key
        $info = $kv.Value
        $fullImage = Get-ImageFullName -ImageName $info.ImageName

        Write-Host "[push] $svc -> $fullImage" -ForegroundColor Magenta

        if ($DryRun) {
            Write-Host "  [dry-run] docker push $fullImage" -ForegroundColor DarkGray
        } else {
            & docker push $fullImage
            if ($LASTEXITCODE -ne 0) {
                throw "Docker push failed for $svc"
            }
            Write-Host "  [ok] $fullImage" -ForegroundColor Green
        }
    }
}

function Invoke-List {
    Write-Host "`nGo service images (registry=$Registry tag=$Tag commit=$BuildCommit):`n" -ForegroundColor Cyan
    Write-Host ("{0,-18} {1,-30} {2}" -f "SERVICE", "IMAGE", "FULL") -ForegroundColor White
    Write-Host ("{0,-18} {1,-30} {2}" -f "-------", "-----", "----")
    foreach ($kv in $Catalogue.GetEnumerator()) {
        $full = Get-ImageFullName -ImageName $kv.Value.ImageName
        Write-Host ("{0,-18} {1,-30} {2}" -f $kv.Key, $kv.Value.ImageName, $full)
    }
    Write-Host ""
}

switch ($Command) {
    "build-all"   { Invoke-BuildAll }
    "push-all"    { Invoke-PushAll }
    "release-all" { Invoke-BuildAll; Invoke-PushAll }
    "list"        { Invoke-List }
}
