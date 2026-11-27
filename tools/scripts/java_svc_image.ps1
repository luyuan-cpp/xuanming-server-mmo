#requires -Version 7
<#
.SYNOPSIS
    构建 / 推送 Java 服务(网关等)的 Docker 镜像。

.DESCRIPTION
    与 go_svc_image.ps1 同一套风格:遍历 Java 服务目录表,每个服务用
    deploy/k8s/Dockerfile.java-svc 构建成独立镜像。
    镜像命名:{Registry}/{ImageName}:{Tag}

    Dockerfile.java-svc 的 build context 必须是R仓库根目录，包含java reactor及正式proto源。
    (多阶段:temurin JDK + maven 打包 → temurin JRE alpine 运行)。

    gm-admin(java/gm_admin,JeecgBoot 扩展)例外:用 deploy/k8s/Dockerfile.gm-admin 只打包宿主机先构建好的
    后端 jar 与前端 dist(两个具名 build context),构建前核对产物;步骤见 java/gm_admin/README.md「K8s 镜像」。

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
    [string]$DigestsOut = "",
    # 显式已批准的Linux protoc原件目录(只含protoc)；不安装、下载或使用Windows exe替代。
    [string]$LinuxProtocDir = "",
    [string]$LinuxProtocSha256 = ""
)

$ErrorActionPreference = "Stop"

# 逐级 Split-Path 上溯、Join-Path 分段:反斜杠在 Linux pwsh 里不是路径分隔符。
$ScriptDir  = $PSScriptRoot
$RepoRoot   = (Resolve-Path (Split-Path -Parent (Split-Path -Parent $ScriptDir))).Path
$JavaRoot   = Join-Path $RepoRoot "java"
$Dockerfile = Join-Path $RepoRoot "deploy" "k8s" "Dockerfile.java-svc"
# gm-admin 的 Dockerfile 与两份宿主机产物所在目录(具名 build context gm-admin-backend / gm-admin-web)。
$GmAdminDockerfile = Join-Path $RepoRoot "deploy" "k8s" "Dockerfile.gm-admin"
$GmAdminBackendDir = Join-Path $RepoRoot "build" "gm-admin-upstream" "jeecg-boot" "jeecg-module-system" "jeecg-system-start" "target"
$GmAdminWebDir     = Join-Path $RepoRoot "build" "gm-admin-upstream" "jeecgboot-vue3" "dist"

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

# 服务目录表:name → { Dir (java/ 下的reactor module), ImageName }
# ImageName 必须与 k8s_deploy.ps1 $JavaSvcCatalogue 的 ImageName 一致,否则 zone-up 拉不到镜像。
# auth(mmorpg-auth)在 $JavaSvcCatalogue 里有条目,但仓库里没有对应的 java/<auth>/ 独立工程
# (springboot_satoken_auth_starter 是 starter 库,不是可打包的服务),所以这里刻意不列:
# 列了只会在 mvn package 阶段炸,不如让 zone-up 在拉镜像时明确报 ImagePullBackOff。
$Catalogue = [ordered]@{
    gateway = @{ Dir = "gateway_node"; ImageName = "mmorpg-gateway" }
    # 官方商城 mall(java/mall_game):与 k8s_deploy.ps1 $JavaSvcCatalogue 的 mall 条目配对(ImageName 必须一致)。
    # 同一个 reactor,`-pl mall_game -am` 连 authority_jdbc 一起构建;镜像入口与 gateway 相同(Dockerfile.java-svc)。
    mall    = @{ Dir = "mall_game";    ImageName = "mmorpg-mall" }
    # GM 后台 gm-admin(java/gm_admin):与 k8s_deploy.ps1 $JavaSvcCatalogue 的 gm-admin 条目配对(ImageName 必须一致)。
    # 不在 java reactor 里:Invoke-Build 转给 Invoke-GmAdminBuild,打包宿主机先构建好的 jar 与前端 dist。
    'gm-admin' = @{ Dir = "gm_admin"; ImageName = "mmorpg-gm-admin" }
}

function Get-ImageFullName {
    param([string]$ImageName)
    return "$Registry/${ImageName}:${Tag}"
}

function Invoke-Build {
    if (-not (Test-Path $Dockerfile)) {
        throw "Dockerfile not found: $Dockerfile"
    }

    if(-not [IO.Path]::IsPathRooted($LinuxProtocDir) -or $LinuxProtocSha256 -cnotmatch '^[a-f0-9]{64}$'){throw '构建必须明确已批准LinuxProtocDir与LinuxProtocSha256；不猜下载地址或借Windows生成器'}
    $toolDir=Get-Item -LiteralPath $LinuxProtocDir -ErrorAction Stop
    if(-not $toolDir.PSIsContainer -or $toolDir.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Linux protoc输入必须为非链接原件目录'}
    $toolFiles=@(Get-ChildItem -LiteralPath $LinuxProtocDir -Force)
    if($toolFiles.Count -ne 1 -or $toolFiles[0].Name -cne 'protoc' -or $toolFiles[0].PSIsContainer -or $toolFiles[0].Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Linux protoc上下文只能包含具名非链接protoc原件'}
    if((Get-FileHash -LiteralPath $toolFiles[0].FullName -Algorithm SHA256).Hash.ToLowerInvariant() -cne $LinuxProtocSha256){throw 'Linux protoc原件SHA不符'}
    $toolStream=[IO.File]::OpenRead($toolFiles[0].FullName)
    try{$magic=[byte[]]::new(4);if($toolStream.Read($magic,0,4) -ne 4 -or [Convert]::ToHexString($magic) -cne '7F454C46'){throw 'Linux protoc必须为已批准ELF原件，不能传入PE或脚本'}}finally{$toolStream.Dispose()}
    # gm-admin 的宿主机产物在构建任何镜像之前核对:不对就整体拒绝,不白跑前面的 Java 镜像。
    if ($Catalogue.Contains('gm-admin')) { Assert-GmAdminArtifacts }
    foreach ($kv in $Catalogue.GetEnumerator()) {
        $svc  = $kv.Key
        $info = $kv.Value
        $context = $RepoRoot
        $fullImage = Get-ImageFullName -ImageName $info.ImageName
        if ($svc -eq 'gm-admin') { Invoke-GmAdminBuild -FullImage $fullImage -ImageName $info.ImageName; continue }

        if (-not (Test-Path (Join-Path (Join-Path $JavaRoot $info.Dir) "pom.xml")) -or -not (Test-Path (Join-Path $JavaRoot "pom.xml")) -or -not (Test-Path (Join-Path $RepoRoot 'proto/common/database/rollback_database_table.proto'))) {
            throw "Java 服务 $svc 的 build context 里没有 pom.xml:$context"
        }

        Write-Host "[build] $svc -> $fullImage" -ForegroundColor Cyan

        # version / created / title 由 Dockerfile 按 build-arg 写 LABEL;命令行只再给一次 revision:
        # publish_images.ps1 按它核对镜像 == 当前 commit。命令行 --label 会覆盖 Dockerfile 同名 LABEL,
        # 所以这里不再传 version=$Tag,否则发布版本的 version label 会被 tag 盖掉。
        $buildArgs = @(
            "build",
            "-f", $Dockerfile,
            "--build-context", "qualified-protoc=$LinuxProtocDir",
            "--build-arg", "LINUX_PROTOC_SHA256=$LinuxProtocSha256",
            "--build-arg", "SERVICE=$($info.Dir)",
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

# gm-admin 构建前 fail-closed 核对宿主机产物:jar 与 dist/index.html 都在、分别晚于各自源码与 prepare.py,
# 协议描述晚于 proto/、jar 晚于协议描述,dist/_app.config.js 是 gmk8s 模式的同源取值(缺了或取了别的模式,页面白屏或连错地址)。
function Assert-GmAdminArtifacts {
    $module = Join-Path $JavaRoot "gm_admin"
    $prepare = Get-Item -LiteralPath (Join-Path $module "prepare.py")
    $rebuild = @(
        "按 java/gm_admin/README.md「K8s 镜像」在仓库根目录重新构建:",
        "  python java/gm_admin/prepare.py",
        "  mvn -s build/gm-admin-runtime/maven-settings.xml -f build/gm-admin-upstream/jeecg-boot/pom.xml -pl jeecg-module-system/jeecg-system-start -am '-Dmaven.compiler.proc=full' -DskipTests package",
        "  pnpm --dir build/gm-admin-upstream/jeecgboot-vue3 install --frozen-lockfile --fetch-timeout=300000",
        "  pnpm --dir build/gm-admin-upstream/jeecgboot-vue3 exec cross-env NODE_ENV=production NODE_OPTIONS=--max-old-space-size=8192 vite build --mode gmk8s",
        "  pnpm --dir build/gm-admin-upstream/jeecgboot-vue3 exec cross-env npm_lifecycle_script=NODE_ENV=gmk8s esno ./build/script/postBuild.ts"
    ) -join "`n"
    foreach ($artifact in @(
            @{ Path = (Join-Path $GmAdminBackendDir "jeecg-system-start-3.9.6.jar"); Sources = (Join-Path $module "src" "main") }
            @{ Path = (Join-Path $GmAdminWebDir "index.html"); Sources = (Join-Path $module "frontend" "src") }
        )) {
        if (-not (Test-Path -LiteralPath $artifact.Path -PathType Leaf)) {
            throw "gm-admin 产物不存在:$($artifact.Path)`n$rebuild"
        }
        $builtAt = (Get-Item -LiteralPath $artifact.Path).LastWriteTimeUtc
        $newest = @(Get-ChildItem -LiteralPath $artifact.Sources -Recurse -File -Force) + $prepare | Sort-Object LastWriteTimeUtc | Select-Object -Last 1
        if ($builtAt -le $newest.LastWriteTimeUtc) {
            throw "gm-admin 产物比源码旧:$($artifact.Path) 不晚于 $($newest.FullName)`n$rebuild"
        }
    }
    # 协议描述:jar 内嵌的 gm-contracts.pb 是 prepare.py 那一刻按 proto/ 生成、拷进上游资源目录的。只核 jar 比后台源码新拦不住
    # 「改了管理协议(批准记录、交易 / 邮件 / 账号类请求)却没重跑 prepare.py」:后台会按旧字段号编请求,下游拒绝或读错字段。
    # 所以再核两条:描述比 proto/ 下每个 .proto 都新(任何协议改动都要重跑 prepare.py),jar 比描述新(jar 内嵌的就是这一份)。
    $descriptor = Join-Path $RepoRoot "build" "gm-admin-upstream" "jeecg-boot" "jeecg-module-system" "jeecg-system-start" "src" "main" "resources" "gm-contracts.pb"
    if (-not (Test-Path -LiteralPath $descriptor -PathType Leaf)) {
        throw "gm-admin 协议描述不存在:$descriptor(prepare.py 生成并拷进上游资源目录)`n$rebuild"
    }
    $descriptorAt = (Get-Item -LiteralPath $descriptor).LastWriteTimeUtc
    $newestProto = @(Get-ChildItem -LiteralPath (Join-Path $RepoRoot "proto") -Recurse -File -Filter "*.proto" -Force) | Sort-Object LastWriteTimeUtc | Select-Object -Last 1
    if ($null -ne $newestProto -and $descriptorAt -le $newestProto.LastWriteTimeUtc) {
        throw "gm-admin 协议描述比协议源码旧:$descriptor 不晚于 $($newestProto.FullName)`n$rebuild"
    }
    if ((Get-Item -LiteralPath (Join-Path $GmAdminBackendDir "jeecg-system-start-3.9.6.jar")).LastWriteTimeUtc -le $descriptorAt) {
        throw "gm-admin 后端 jar 比协议描述旧:jar 里内嵌的还是上一份描述`n$rebuild"
    }
    $appConfig = Join-Path $GmAdminWebDir "_app.config.js"
    $appConfigText = if (Test-Path -LiteralPath $appConfig -PathType Leaf) { [IO.File]::ReadAllText($appConfig) } else { "" }
    if (-not $appConfigText.Contains('"VITE_GLOB_API_URL":"/jeecgboot"') -or -not $appConfigText.Contains('"VITE_GLOB_DOMAIN_URL":"/jeecgboot"')) {
        throw "gm-admin 前端不是 gmk8s 模式的产物:$appConfig 缺失或接口地址不是同源 /jeecgboot(本机 .env.gm 与上游 .env.production 的取值都不能进镜像)。`n$rebuild"
    }
}

# gm-admin:自己的 Dockerfile + 两个具名 context(jar 所在目录、dist 目录),主 context 是仓库根(只取 nginx 配置)。
function Invoke-GmAdminBuild {
    param(
        [Parameter(Mandatory = $true)][string]$FullImage,
        [Parameter(Mandatory = $true)][string]$ImageName
    )
    if (-not (Test-Path $GmAdminDockerfile)) {
        throw "Dockerfile not found: $GmAdminDockerfile"
    }
    Write-Host "[build] gm-admin -> $FullImage" -ForegroundColor Cyan
    # revision label 与其它 Java 镜像同口径在命令行再传一次:publish_images.ps1 按它核对镜像 == 当前 commit。
    $buildArgs = @(
        "build",
        "-f", $GmAdminDockerfile,
        "--build-context", "gm-admin-backend=$GmAdminBackendDir",
        "--build-context", "gm-admin-web=$GmAdminWebDir",
        "--build-arg", "BUILD_VERSION=$BuildVersion",
        "--build-arg", "BUILD_COMMIT=$BuildCommit",
        "--build-arg", "BUILD_TIME=$BuildTime",
        "--build-arg", "IMAGE_TITLE=$ImageName",
        "--label", "org.opencontainers.image.revision=$BuildCommit",
        "-t", $FullImage,
        $RepoRoot
    )
    if ($DryRun) {
        Write-Host "  [dry-run] docker $($buildArgs -join ' ')" -ForegroundColor DarkGray
        return
    }
    & (Resolve-ReleaseDockerCommand) @buildArgs
    if ($LASTEXITCODE -ne 0) {
        throw "Docker build failed for gm-admin"
    }
    Write-Host "  [ok] $FullImage" -ForegroundColor Green
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
