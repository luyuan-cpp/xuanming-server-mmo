#requires -Version 7
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

    # 发布版本构建(tag = v1.2.3-<12位commit>,镜像与二进制自报 v1.2.3;脏树直接拒绝),
    # 推送后把 registry digest 记进 JSON。记录文件的位置有两条约束(当前目录 = 仓库根):
    #  - 不要放进 releases/images/<版本>/:多出的文件不在 sha256sums.txt 里,Test-Sha256Sums 判失败,该版本再也 fetch 不了;
    #  - 不要放在仓库内:未跟踪文件让工作树变脏,同一批后续带 -Version 的命令会以"不接受脏工作树"拒绝。
    pwsh -File tools/scripts/go_svc_image.ps1 -Command release-all `
        -Registry ghcr.io/luyuancpp -Version v1.2.3 `
        -DigestsOut (Join-Path (Split-Path $PWD.Path -Parent) 'image-digests-v1.2.3.json')

    # 机器可读镜像清单:每行一个完整引用,无其它输出(publish_images.ps1 用)
    pwsh -File tools/scripts/go_svc_image.ps1 -Command list-refs -Registry local -Version v1.2.3
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("build-all", "push-all", "release-all", "list", "list-refs")]
    [string]$Command,

    [string]$Registry = "ghcr.io/luyuancpp",
    # 留空 = git 短 sha(脏树带 -dirty);给了 -Version 时为 <版本号>-<sha>。不再默认 latest:
    # 可变 tag 会让 `kubectl rollout undo` 退回同一个 digest。
    [string]$Tag = "",
    # 只处理目录里的这几个服务(逗号分隔,如 match,login)。留空 = 全部。
    # 目录是顺序遍历、一个失败就 throw,以前想单独构建 match 也得先等 db 过。
    # (2026-09-17 核对:db / data_service 的 proto2mysql 已改为远程仓名映射,不再依赖宿主目录,
    #  不带 -Services 的 build-all 覆盖目录里全部服务;按需过滤只为省时间。)
    #
    # 类型是 [string[]] 而不是 [string]:从 PowerShell 里 `& go_svc_image.ps1 -Services a,b`
    # 调用时,逗号表达式先被解析成数组,[string] 参数会直接报
    # "Cannot convert value to type System.String" 而不是进到下面的 split;
    # `pwsh -File ... -Services "a,b"` 传的是单个字符串,两种形态这里都接。
    [string[]]$Services = @(),
    # 发布版本号 vX.Y.Z[-预发布标识](格式由 lib/release_common.ps1 Test-ReleaseVersion 唯一定义)。留空 = 快照构建。
    # 非空时:未显式给 -Tag 则 tag = <版本号>-<12位commit>;注入镜像 / 二进制的 BUILD_VERSION = 该版本号。
    # 次优先来源是环境变量 MMORPG_RELEASE_VERSION(发布流水线设一次,三个镜像脚本同口径)。
    [string]$Version = "",
    # 只对 push-all / release-all 有效:每推完一个镜像回读 registry digest,合并写进这个 JSON
    # ({ "<repo:tag>": "<repo@sha256:...>" })。tag 只是别名,部署 / 回滚要按 digest 定位。留空 = 不记录。
    [string]$DigestsOut = "",
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

# 路径一律用 '/' 分段:Windows 与 Linux pwsh 都认;反斜杠在 Linux 上只是文件名里的普通字符。
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "../..")
$GoRoot    = Join-Path $RepoRoot "go"
$Dockerfile = Join-Path $RepoRoot "deploy/k8s/Dockerfile.go-svc"
# 策划表(各 Go 服务 TableDir 默认 ../../generated/tables),随镜像打包
$TablesDir = Join-Path $RepoRoot "generated/tables"

. (Join-Path $ScriptDir "lib/release_common.ps1")

if (-not [string]::IsNullOrWhiteSpace($DigestsOut) -and $Command -notin @("push-all", "release-all")) {
    throw "-DigestsOut 只对 push-all / release-all 有效(digest 在推送之后才存在),当前命令:$Command"
}

# 发布版本号:-Version > MMORPG_RELEASE_VERSION > 空(快照)。两个来源同一套校验,拼进 tag / 注入镜像之前拦住非法值。
$ReleaseVersion = ""
if (-not [string]::IsNullOrWhiteSpace($Version)) {
    $versionCheck = Test-ReleaseVersion -Version $Version
    if (-not $versionCheck.Ok) { throw "-Version 不合法:$($versionCheck.Reason)" }
    $ReleaseVersion = $Version
}
elseif (-not [string]::IsNullOrWhiteSpace($env:MMORPG_RELEASE_VERSION)) {
    $versionCheck = Test-ReleaseVersion -Version $env:MMORPG_RELEASE_VERSION
    if (-not $versionCheck.Ok) { throw "环境变量 MMORPG_RELEASE_VERSION 不合法:$($versionCheck.Reason)" }
    $ReleaseVersion = $env:MMORPG_RELEASE_VERSION
}

$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot
if ([string]::IsNullOrWhiteSpace($Tag)) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag:$($script:ReleaseStamp.Reason)。请显式传 -Tag。"
    }
    # 无版本号:<sha12> / <sha12>-dirty(与旧行为一致);有版本号:<vX.Y.Z>-<sha12>,脏树由 Get-ReleaseImageTag 抛出拒绝。
    $Tag = Get-ReleaseImageTag -Version $ReleaseVersion -Commit $script:ReleaseStamp.Commit -Dirty:([bool]$script:ReleaseStamp.Dirty)
}

# 显式 -Tag 绕过了上面的脏树判定与 tag 生成,构建类命令再兜一次:
#   - 挂发布版本号的镜像必须能从一个干净 commit 还原,否则 BUILD_VERSION=v1.2.3 的二进制里可能是未提交的代码;
#   - tag 必须就是 Get-ReleaseImageTag 给出的 <版本号>-<12位commit>,否则镜像 tag / OCI label / 二进制自报版本
#     三处对不上(如 -Version v1.2.3 -Tag latest 或 -Tag v1.2.4-<sha>),回滚与复盘按 tag 找到的不是它声称的那一版。
# publish_images.ps1 传的 -Tag 就是同一个函数算出来的,不受影响。
if ($ReleaseVersion -and $Command -in @("build-all", "release-all")) {
    if (-not $script:ReleaseStamp.Ok) {
        throw "发布版本 $ReleaseVersion 需要可追溯的 git commit:$($script:ReleaseStamp.Reason)"
    }
    if ($script:ReleaseStamp.Dirty) {
        throw "发布版本 $ReleaseVersion 不接受脏工作树(git status 非空):脏树产物无法从 commit $($script:ReleaseStamp.Commit) 还原。请先提交或清理工作树。"
    }
    $expectedTag = Get-ReleaseImageTag -Version $ReleaseVersion -Commit $script:ReleaseStamp.Commit
    if ($Tag -cne $expectedTag) {
        throw "发布版本 $ReleaseVersion 的镜像 tag 必须是 $expectedTag(<版本号>-<12位commit>),实际 -Tag 为 '$Tag'。不传 -Tag 即自动生成。"
    }
}

# 快照构建同样不许脏树冒充干净 commit:脏树上 `build-all -Tag <sha12>` 产出的镜像,tag / revision label /
# BUILD_VERSION / 启动行 commit 全是干净的 <sha12>,与从该 commit 干净构建的镜像无从区分;之后清理工作树再跑
# `publish_images.ps1 -SkipBuild`(只核对 revision label == HEAD),未提交的代码就被当成 g<sha12> 快照发布出去。
# 只拦"以当前 commit 结尾"的 tag(<sha12>、<任意前缀>-<sha12>);p1test 这类临时 tag 与 <sha12>-dirty 不受影响,
# dev_tools.ps1 / publish_images.ps1 -AllowDirty 在脏树上传的正是 <sha12>-dirty。
if ($Command -in @("build-all", "release-all") -and $script:ReleaseStamp.Ok -and $script:ReleaseStamp.Dirty -and
    $Tag -cmatch "(^|-)$([regex]::Escape($script:ReleaseStamp.Commit))\z") {
    throw "脏工作树(git status 非空)不能构建与干净 commit 同名的 tag '$Tag':会被 publish_images.ps1 -SkipBuild 当成 commit $($script:ReleaseStamp.Commit) 的干净产物。不传 -Tag 自动生成 $($script:ReleaseStamp.Commit)-dirty,或先提交 / 清理工作树。"
}

# 注入到镜像里的版本戳(ldflags + OCI label + /app/BUILD_INFO)
# BUILD_VERSION:有发布版本号用版本号,否则沿用镜像 tag(快照镜像的"版本"就是 commit)。
$BuildVersion = if ($ReleaseVersion) { $ReleaseVersion } else { $Tag }
$BuildCommit = if ($script:ReleaseStamp.Ok) { $script:ReleaseStamp.Commit } else { "unknown" }
# InvariantCulture:自定义格式里的 ':' 是"时间分隔符"占位,部分区域性(如 fi-FI)会换成 '.',
# 产出 2026-09-16T08.00.00Z 这种不符合接口约定的 created label。
$BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ", [System.Globalization.CultureInfo]::InvariantCulture)
# docker 可执行命令:与 release_common.ps1 同一入口,MMORPG_DOCKER_COMMAND 可换成契约测试桩。
$DockerCommand = Resolve-ReleaseDockerCommand

# Service catalogue: name → { Dir (in go/), Entry (.go file), ImageName }
$Catalogue = [ordered]@{
    db              = @{ Dir = "db";              Entry = "db.go";                  ImageName = "mmorpg-db" }
    "data-service"  = @{ Dir = "data_service";    Entry = "data_service.go";         ImageName = "mmorpg-data-service" }
    login           = @{ Dir = "login";           Entry = "login.go";               ImageName = "mmorpg-login" }
    "player-locator"= @{ Dir = "player_locator";  Entry = "player_locator.go";      ImageName = "mmorpg-player-locator" }
    "scene-manager" = @{ Dir = "scene_manager";   Entry = "scene_manager_service.go"; ImageName = "mmorpg-scene-manager" }
    # 与 k8s_deploy.ps1 $GoSvcCatalogue 的 match 条目配对(ImageName 必须一致,否则 infra-up 拉不到镜像)
    match           = @{ Dir = "match";           Entry = "match_service.go";       ImageName = "mmorpg-match" }
    # 全局聊天 chat v1:与 k8s_deploy.ps1 $GoSvcCatalogue 的 chat 条目配对(ImageName 必须一致,理由同 match)。
    # chat 只承诺经 gate → 路由服可达(docs/design/client-rpc-router.md D34;
    # docs/design/xuanming-port-decisions-20260910.md D-12),所以镜像必须和下面的路由服成对发布。
    chat            = @{ Dir = "chat";            Entry = "chat.go";                ImageName = "mmorpg-chat" }
    # 客户端 RPC 路由服:GATE_CLIENT_RPC_ROUTER=1 时 gate 唯一的 gRPC 目标(契约 zone_contract_v1 §1/§7 路由服部署链)。
    # 与 k8s_deploy.ps1 $GoSvcCatalogue 的 client-rpc-router 条目配对(ImageName 必须一致)。
    # 发布顺序:改 proto 后路由服先、gate 后(新消息号两边生成物必须同一次 proto-gen)。
    "client-rpc-router" = @{ Dir = "client_rpc_router"; Entry = "client_rpc_router_service.go"; ImageName = "mmorpg-client-rpc-router" }
    # 聚宝斋 trade:与 k8s_deploy.ps1 $GoSvcCatalogue 的 trade 条目配对(ImageName 必须一致)。只经路由服可达,与路由服成对发布。
    # go/trade/go.mod 的 `replace schemamigrate => ../schemamigrate` 在 go/ 之内,由 Dockerfile.go-svc 的
    # `COPY schemamigrate/` 带入,不走 ExternalReplaceStages;schemamigrate 依赖的 proto2mysql 用已发布 tag(仅远程仓名映射,不使用本地目录 replace)。
    # 同一镜像既跑 trade Deployment,也跑 trade-migrate Job(args 加 -migrate,D-14)。
    trade           = @{ Dir = "trade";           Entry = "trade.go";               ImageName = "mmorpg-trade" }
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
# 背景:go/db/go.mod 当初是 `replace github.com/luyuancpp/proto2mysql => ../../../proto2mysql`,
# 目标在仓库外,以 go/ 为 build context 带不进镜像,db 镜像以前在任何环境都构建不了。
# Dockerfile.go-svc 为它声明了一个空的 `FROM scratch AS proto2mysql` 占位 stage,
# BuildKit 允许命名上下文按名字覆盖同名 stage,所以这里只要把宿主目录传进去即可,
# go.mod 一行不用改。目录名(replace 目标路径最后一段)就是 stage 名,两边必须一致。
#
# 只处理 Dockerfile 里有对应 stage 的模块(白名单),其它 replace(../proto、../shared)
# 本来就在 go/ 里,由 COPY 正常带入。
#
# 现状(2026-09-17 核对 go.mod):db / data_service / trade / schemamigrate 对 proto2mysql 都是远程仓名映射
# (`replace github.com/luyuancpp/proto2mysql <版本> => github.com/luyuan-cpp/proto2mysql <版本>`),
# 下面的正则只认本地路径形式,不会命中,所有服务都返回空列表。白名单与 Dockerfile 占位 stage 暂留作防回归:
# 再有人加回指向仓库外其它目录名的本地 replace 会被 throw 拦下,并提示优先改回已发布版本。
# 删除要与 .github/workflows/release.yml 的"仓库外 replace"检查一起做(另立任务)。
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
            throw "go.mod 里有指向仓库外的 replace($line),但 Dockerfile.go-svc 没有名为 '$stage' 的占位 stage。优先改为 require 已发布 tag 并删掉这条本地 replace(参照 go/schemamigrate/go.mod);确需宿主目录时才在 Dockerfile 里加 `FROM scratch AS $stage` + COPY,并把名字加进 go_svc_image.ps1 的 ExternalReplaceStages。"
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
            "--build-arg", "BUILD_VERSION=$BuildVersion",
            "--build-arg", "BUILD_COMMIT=$BuildCommit",
            "--build-arg", "BUILD_TIME=$BuildTime",
            # 与 Dockerfile 里的 LABEL 同值,命令行再传一次:发布脚本按这个 label 核对 revision == 当前 commit,
            # 不依赖 Dockerfile 那行 LABEL 有没有被改掉。
            "--label", "org.opencontainers.image.revision=$BuildCommit"
        )
        # 宿主设置了 GOPROXY(buildenv.ps1 → goproxy.cn)就透传给 Dockerfile 的 ARG GOPROXY,
        # 否则 builder 阶段走 proxy.golang.org,国内网络下 `go mod download` 会超时。
        if (-not [string]::IsNullOrWhiteSpace($env:GOPROXY)) {
            $buildArgs += @("--build-arg", "GOPROXY=$($env:GOPROXY)")
        }
        # 仓库外本地 replace 模块以命名上下文带入(2026-09-17 起没有服务需要,正常返回空)
        $buildArgs += @(Get-ExternalBuildContexts -ServiceDir (Join-Path $GoRoot $info.Dir))
        # 策划表:仓库根 generated/tables 不在 go/ 里,固定以命名上下文 tables 带入
        # (Dockerfile.go-svc 的 `FROM scratch AS tables` 占位 stage)。
        # 找不到直接 throw:没有表的 login / player-locator / scene-manager 镜像起来就 CrashLoop。
        $buildArgs += @("--build-context", "tables=$TablesDir")
        $buildArgs += @("-t", $fullImage, $GoRoot)

        if ($DryRun) {
            Write-Host "  [dry-run] $DockerCommand $($buildArgs -join ' ')" -ForegroundColor DarkGray
        } else {
            & $DockerCommand @buildArgs
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
            Write-Host "  [dry-run] $DockerCommand push $fullImage" -ForegroundColor DarkGray
            if (-not [string]::IsNullOrWhiteSpace($DigestsOut)) {
                Write-Host "  [dry-run] 回读 digest 并记录到 $DigestsOut" -ForegroundColor DarkGray
            }
        } else {
            & $DockerCommand push $fullImage
            if ($LASTEXITCODE -ne 0) {
                throw "Docker push failed for $svc"
            }
            if (-not [string]::IsNullOrWhiteSpace($DigestsOut)) {
                # 推一个记一个:中途失败时已推成功的镜像 digest 不丢,重跑时同 ref 同 digest 幂等。
                $pushed = Get-PushedImageDigest -ImageRef $fullImage
                if (-not $pushed.Ok) {
                    throw "镜像已推送但回读 digest 失败($svc):$($pushed.Reason)"
                }
                Write-ImageDigestRecord -Path $DigestsOut -ImageRef $fullImage -Digest $pushed.Digest
                Write-Host "  [digest] $($pushed.Digest)" -ForegroundColor DarkGray
            }
            Write-Host "  [ok] $fullImage" -ForegroundColor Green
        }
    }
}

# 机器可读清单:每行一个完整镜像引用,只走 Write-Output。调用方(publish_images.ps1)逐行解析,
# 这里不能加任何提示 —— `pwsh -File` 起子进程时 Write-Host 同样会进 stdout。
function Invoke-ListRefs {
    foreach ($kv in $Catalogue.GetEnumerator()) {
        Write-Output (Get-ImageFullName -ImageName $kv.Value.ImageName)
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
    "list-refs"   { Invoke-ListRefs }
}
