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

function Invoke-BuildAll {
    if (-not (Test-Path $Dockerfile)) {
        throw "Dockerfile not found: $Dockerfile"
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
