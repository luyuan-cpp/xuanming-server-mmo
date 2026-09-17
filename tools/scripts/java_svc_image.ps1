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
    # 构建网关镜像(tag 留空 = 12 位 git sha,脏树带 -dirty)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command build -Registry ghcr.io/luyuancpp

    # 只看会产出哪些镜像引用(人读表格)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command list

    # 机器读:每行一个完整镜像引用,无其它输出
    pwsh -File tools/scripts/java_svc_image.ps1 -Command list-refs -Version v1.2.3

    # 发布版本构建 + 推送 + 记录 digest(tag = v1.2.3-<12位sha>,脏树拒绝)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command release -Registry ghcr.io/luyuancpp -Version v1.2.3 -DigestsOut ./digests.json

    # 只打印 docker 命令不执行(Docker 引擎没起来时也能核对参数)
    pwsh -File tools/scripts/java_svc_image.ps1 -Command build -DryRun
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("build", "push", "release", "list", "list-refs")]
    [string]$Command,

    [string]$Registry = "ghcr.io/luyuancpp",
    # 留空 = Get-ReleaseImageTag:有发布版本号时 <vX.Y.Z>-<12位sha>,否则 12 位 git sha(脏树带 -dirty)。
    # 不默认 latest:可变 tag 会让 `kubectl rollout undo` 退回同一个 digest。
    [string]$Tag = "",
    [switch]$DryRun,
    # 新参数一律追加在末尾(没有显式 Position,改声明顺序会改变位置参数绑定)。
    # 发布版本号 vX.Y.Z(可空 = 快照构建),为空时回落 $env:MMORPG_RELEASE_VERSION;非空时脏树一律拒绝。
    [string]$Version = "",
    # push / release 推送后把 "ref -> repo@sha256:..." 合并写进这个 JSON;回读不到 digest 视为失败。
    [string]$DigestsOut = ""
)

$ErrorActionPreference = "Stop"

# 逐级 Split-Path 上溯、Join-Path 分段:反斜杠在 Linux pwsh 里不是路径分隔符。
$ScriptDir  = $PSScriptRoot
$RepoRoot   = (Resolve-Path (Split-Path -Parent (Split-Path -Parent $ScriptDir))).Path
$JavaRoot   = Join-Path $RepoRoot "java"
$Dockerfile = Join-Path $RepoRoot "deploy" "k8s" "Dockerfile.java-svc"

. (Join-Path $ScriptDir "lib" "release_common.ps1")

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

# 发布版本号:-Version > $env:MMORPG_RELEASE_VERSION > 空。与 k8s_image.ps1 / go_svc_image.ps1 同一口径。
$script:ReleaseVersion = ''
if (-not [string]::IsNullOrEmpty($Version)) {
    $script:ReleaseVersion = $Version
}
elseif (-not [string]::IsNullOrEmpty($env:MMORPG_RELEASE_VERSION)) {
    $script:ReleaseVersion = $env:MMORPG_RELEASE_VERSION
}
if ($script:ReleaseVersion) {
    $versionCheck = Test-ReleaseVersion -Version $script:ReleaseVersion
    if (-not $versionCheck.Ok) {
        throw "发布版本号校验失败:$($versionCheck.Reason)"
    }
    if (-not $script:ReleaseStamp.Ok) {
        throw "指定了发布版本 $($script:ReleaseVersion),但取不到 git commit:$($script:ReleaseStamp.Reason)。发布镜像必须能追溯到 commit。"
    }
    # 显式 -Tag 也不放行:版本号会写进 jar 的 build-info 与镜像 LABEL,挂在脏树产物上就无法从 commit 还原。
    if ($script:ReleaseStamp.Dirty) {
        throw "发布版本 $($script:ReleaseVersion) 不接受脏工作树(git status 非空),-Tag 也不能绕过:脏树产物无法从 commit $($script:ReleaseStamp.Commit) 还原。请先提交或清理工作树。"
    }
}

if ([string]::IsNullOrWhiteSpace($Tag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -Tag。"
    }
    $Tag = Get-ReleaseImageTag -Version $script:ReleaseVersion -Commit $script:ReleaseStamp.Commit -Dirty:$script:ReleaseStamp.Dirty
}

# 版本三元组(接口 D),经 --build-arg 进 Dockerfile.java-svc:builder 阶段传给 mvn 写进 build-info,
# 运行阶段写 OCI LABEL 与 /app/BUILD_INFO。快照构建没有版本号,用镜像 tag 充当。
$BuildVersion = if ($script:ReleaseVersion) { $script:ReleaseVersion } else { $Tag }
$BuildCommit = if ($script:ReleaseStamp.Ok) { $script:ReleaseStamp.Commit } else { "unknown" }
# InvariantCulture + 引号包住分隔符:自定义格式里的 ':' 是"区域时间分隔符",不是字面冒号。
$BuildTime = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)

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

        # version / created / title 由 Dockerfile 按 build-arg 写 LABEL;命令行只再给一次 revision:
        # publish_images.ps1 按它核对镜像 == 当前 commit。命令行 --label 会覆盖 Dockerfile 同名 LABEL,
        # 所以这里不再传 version=$Tag,否则发布版本的 version label 会被 tag 盖掉。
        $buildArgs = @(
            "build",
            "-f", $Dockerfile,
            "--build-arg", "BUILD_VERSION=$BuildVersion",
            "--build-arg", "BUILD_COMMIT=$BuildCommit",
            "--build-arg", "BUILD_TIME=$BuildTime",
            "--build-arg", "IMAGE_TITLE=$($info.ImageName)",
            "--label", "org.opencontainers.image.revision=$BuildCommit",
            "-t", $fullImage,
            $context
        )

        if ($DryRun) {
            Write-Host "  [dry-run] docker $($buildArgs -join ' ')" -ForegroundColor DarkGray
        } else {
            # $env:MMORPG_DOCKER_COMMAND 可换成测试桩(release_common.ps1 同一口径)。
            & (Resolve-ReleaseDockerCommand) @buildArgs
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
            if (-not [string]::IsNullOrWhiteSpace($DigestsOut)) {
                Write-Host "  [dry-run] record digest of $fullImage -> $DigestsOut" -ForegroundColor DarkGray
            }
        } else {
            & (Resolve-ReleaseDockerCommand) push $fullImage
            if ($LASTEXITCODE -ne 0) {
                throw "Docker push failed for $svc"
            }
            Write-Host "  [ok] $fullImage" -ForegroundColor Green
            Save-PushedImageDigest -ImageRef $fullImage
        }
    }
}

# -DigestsOut 非空时回读刚推送镜像的 digest 并合并写进记录文件。
# tag 只是别名,部署/回滚要按 digest 定位;回读失败直接抛,不留一份缺条目的发布记录。
function Save-PushedImageDigest {
    param([Parameter(Mandatory = $true)][string]$ImageRef)

    if ([string]::IsNullOrWhiteSpace($DigestsOut)) {
        return
    }
    $pushed = Get-PushedImageDigest -ImageRef $ImageRef
    if (-not $pushed.Ok) {
        throw "推送后回读 digest 失败,发布记录不完整:$($pushed.Reason)"
    }
    Write-ImageDigestRecord -Path $DigestsOut -ImageRef $ImageRef -Digest $pushed.Digest
    Write-Host "  [digest] $($pushed.Digest) -> $DigestsOut" -ForegroundColor Green
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

# 机器读清单:每行一个完整镜像引用,只用 Write-Output。
# 不能掺 Write-Host:pwsh -File 子进程里 Write-Host 同样写进 stdout,调用方按行解析会读到脏行。
function Invoke-ListRefs {
    foreach ($kv in $Catalogue.GetEnumerator()) {
        Write-Output (Get-ImageFullName -ImageName $kv.Value.ImageName)
    }
}

switch ($Command) {
    "build"     { Invoke-Build }
    "push"      { Invoke-Push }
    "release"   { Invoke-Build; Invoke-Push }
    "list"      { Invoke-List }
    "list-refs" { Invoke-ListRefs }
}
