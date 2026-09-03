#requires -Version 7
<#
.SYNOPSIS
    构建 / 推送 Java 服务(网关等)的 Docker 镜像。

.DESCRIPTION
    与 go_svc_image.ps1 同一套风格:遍历 Java 服务目录表,每个服务用
    deploy/k8s/Dockerfile.java-svc 构建成独立镜像。
    镜像命名:{Registry}/{ImageName}:{Tag}

    Dockerfile.java-svc 的 build context 必须是 java/<service>/ 目录
    (多阶段:temurin JDK + maven 打包 → temurin JRE alpine 运行)。

    dev_tools.ps1 的 java-svc-build-image / java-svc-push-image / build-images
    三条命令都调到这里(以前脚本根本不存在,dev 一键出镜像走到 Java 这步就炸)。

.EXAMPLE
    # 构建网关镜像(tag 留空 = git 短 sha,脏树带 -dirty)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command build -Registry ghcr.io/luyuancpp

    # 只看会产出哪些镜像引用
    pwsh -File tools/scripts/java_svc_image.ps1 -Command list

    # 构建 + 推送
    pwsh -File tools/scripts/java_svc_image.ps1 -Command release -Registry ghcr.io/luyuancpp -Tag v1

    # 只打印 docker 命令不执行(Docker 引擎没起来时也能核对参数)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command build -DryRun
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("build", "push", "release", "list")]
    [string]$Command,

    [string]$Registry = "ghcr.io/luyuancpp",
    # 留空 = git 短 sha(脏树带 -dirty)。不默认 latest:
    # 可变 tag 会让 `kubectl rollout undo` 退回同一个 digest。
    [string]$Tag = "",
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

$ScriptDir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot   = Resolve-Path (Join-Path $ScriptDir "..\..")
$JavaRoot   = Join-Path $RepoRoot "java"
$Dockerfile = Join-Path $RepoRoot "deploy\k8s\Dockerfile.java-svc"

. (Join-Path $ScriptDir "lib\release_common.ps1")

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
if ([string]::IsNullOrWhiteSpace($Tag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -Tag。"
    }
    $Tag = $script:ReleaseStamp.Tag
}

# 写进 OCI label 的版本戳。Dockerfile.java-svc 没有 ARG,所以不走 --build-arg
# (docker 对未声明的 build-arg 只 WARN 不报错,但那等于什么都没注进去);
# 用 --label 让 `docker inspect` 能对上是哪次提交。
$BuildCommit = if ($script:ReleaseStamp.Ok) { $script:ReleaseStamp.Commit } else { "unknown" }
$BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

# 服务目录表:name → { Dir (java/ 下的目录,即 build context), ImageName }
# ImageName 必须与 k8s_deploy.ps1 $JavaSvcCatalogue 的 ImageName 一致,否则 zone-up 拉不到镜像。
# auth(mmorpg-auth)在 $JavaSvcCatalogue 里有条目,但仓库里没有对应的 java/<auth>/ 独立工程
# (springboot_satoken_auth_starter 是 starter 库,不是可打包的服务),所以这里刻意不列:
# 列了只会在 mvn package 阶段炸,不如让 zone-up 在拉镜像时明确报 ImagePullBackOff。
$Catalogue = [ordered]@{
    gateway = @{ Dir = "gateway_node"; ImageName = "mmorpg-gateway" }
}

function Get-ImageFullName {
    param([string]$ImageName)
    return "$Registry/${ImageName}:${Tag}"
}

function Invoke-Build {
    if (-not (Test-Path $Dockerfile)) {
        throw "Dockerfile not found: $Dockerfile"
    }

    foreach ($kv in $Catalogue.GetEnumerator()) {
        $svc  = $kv.Key
        $info = $kv.Value
        $context = Join-Path $JavaRoot $info.Dir
        $fullImage = Get-ImageFullName -ImageName $info.ImageName

        if (-not (Test-Path (Join-Path $context "pom.xml"))) {
            throw "Java 服务 $svc 的 build context 里没有 pom.xml:$context"
        }

        Write-Host "[build] $svc -> $fullImage" -ForegroundColor Cyan

        $buildArgs = @(
            "build",
            "-f", $Dockerfile,
            "--label", "org.opencontainers.image.version=$Tag",
            "--label", "org.opencontainers.image.revision=$BuildCommit",
            "--label", "org.opencontainers.image.created=$BuildTime",
            "-t", $fullImage,
            $context
        )

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

function Invoke-Push {
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
    Write-Host "`nJava service images (registry=$Registry tag=$Tag commit=$BuildCommit):`n" -ForegroundColor Cyan
    Write-Host ("{0,-12} {1,-20} {2,-18} {3}" -f "SERVICE", "CONTEXT", "IMAGE", "FULL") -ForegroundColor White
    Write-Host ("{0,-12} {1,-20} {2,-18} {3}" -f "-------", "-------", "-----", "----")
    foreach ($kv in $Catalogue.GetEnumerator()) {
        $full = Get-ImageFullName -ImageName $kv.Value.ImageName
        Write-Host ("{0,-12} {1,-20} {2,-18} {3}" -f $kv.Key, "java/$($kv.Value.Dir)", $kv.Value.ImageName, $full)
    }
    Write-Host ""
}

switch ($Command) {
    "build"   { Invoke-Build }
    "push"    { Invoke-Push }
    "release" { Invoke-Build; Invoke-Push }
    "list"    { Invoke-List }
}
