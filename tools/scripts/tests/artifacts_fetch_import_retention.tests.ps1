#requires -Version 7
<#
.SYNOPSIS
    fetch_images.ps1 / import_images.ps1 / artifacts_retention.ps1 的契约测试。

.DESCRIPTION
    三个脚本都以子进程跑(与真实使用方式一致:pwsh -File),断言退出码 + 输出文本 + 文件系统结果。
      - fetch:坏制品不落地、正常落地可复验、已存在目录无 -Force 不动、-Force 只替换旧落地目录
               (非落地目录即使带 -Force 也拒绝)、目录名与 build-info 不符拒绝、路径穿越拒绝、制品根不存在报错不创建
      - import:docker 用临时目录里生成的桩 .ps1 代替(记录收到的参数,对 inspect 回显预设镜像 ID),
               不依赖真实 docker;镜像 ID 与清单不符必须失败并报出 ref;缺 sha256sums.txt 默认拒绝;
               .Id 口径不同(发布机与本机镜像存储不同)时按真 tar 里的 manifest.json / index.json 复核
               —— 真 tar 用 System.Formats.Tar 现场生成,需要 pwsh 7.3+
      - retention:默认 dry-run 不删、-Force 删最旧、releases/ 不动、latest.json 指向的不删、
               .tmp-* 与非快照名目录不删(超过 24 小时的 .tmp-* 给 WARN)、.deleting-* 残留被 -Force 清掉、
               制品根不存在报错不创建

    制品根一律用 -ArtifactRoot 指到本测试的临时目录;环境变量 MMORPG_ARTIFACT_ROOT 被指向一个
    "陷阱"制品根 —— 它是合法的制品根,里面有 3 个诱饵快照(g0000000dead0..2)和 latest.json。
    脚本若忽略 -ArtifactRoot 而落到环境变量上:fetch 会读到诱饵,retention -Force 会删掉最旧的诱饵。
    最后一条用例核对诱饵原样未动、所有子进程输出里从未出现陷阱路径与诱饵版本名。
    (陷阱若只是"不存在的目录"抓不住这种回归:fetch / retention 带 -MustExist,只会报错不会建出它)
    负向用例一律断言错误文本,不只断退出码。

.EXAMPLE
    pwsh -File tools/scripts/tests/artifacts_fetch_import_retention.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) 'lib' 'artifacts_lib.ps1')

Write-Host ''
Write-Host '=== fetch_images / import_images / artifacts_retention 契约测试 ==='

$script:TempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-artifacts-scripts-tests-' + [guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($script:TempRoot) | Out-Null
$script:TrapRoot = Join-Path $script:TempRoot 'env-trap-root'
# 子进程环境:Invoke-CapturedPwsh 跑完即还原,不污染本进程与其它用例
$script:ChildEnv = @{ MMORPG_ARTIFACT_ROOT = $script:TrapRoot; MMORPG_DOCKER_COMMAND = $null }

function New-CaseDir {
    param([Parameter(Mandatory = $true)][string]$Name)
    $dir = Join-Path $script:TempRoot $Name
    [System.IO.Directory]::CreateDirectory($dir) | Out-Null
    return $dir
}

function Write-TestFile {
    param([string]$Path, [string]$Content)
    [System.IO.Directory]::CreateDirectory((Split-Path -Parent $Path)) | Out-Null
    [System.IO.File]::WriteAllText($Path, $Content, [System.Text.UTF8Encoding]::new($false))
}

function Get-TextSha256 {
    param([string]$Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($Text)
        return [System.BitConverter]::ToString($sha.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant()
    }
    finally { $sha.Dispose() }
}

# 汇总所有子进程输出,给末尾的隔离自检检查"是否出现过陷阱制品根"
$script:AllToolOutput = New-Object System.Text.StringBuilder

function Invoke-Tool {
    param([string]$ScriptName, [string[]]$Arguments)
    $result = Invoke-ToolScript -ScriptName $ScriptName -Arguments $Arguments -Env $script:ChildEnv
    [void]$script:AllToolOutput.Append([string]$result.Output)
    return $result
}

function Assert-ToolFailed {
    param($Result, [string]$Pattern, [string]$Because)
    Assert-True -Condition ($Result.ExitCode -ne 0) -Because "$Because —— 应当失败,实际 exit=$($Result.ExitCode)。输出:$($Result.Output)"
    Assert-Match -Text $Result.Output -Pattern $Pattern -Because $Because
}

function Assert-ToolSucceeded {
    param($Result, [string]$Because)
    Assert-True -Condition ($Result.ExitCode -eq 0) -Because "$Because —— 应当成功,实际 exit=$($Result.ExitCode)。输出:$($Result.Output)"
}

<#
.SYNOPSIS
    在 <Root>/<轨道>/images/<Version>/ 下造一个符合布局 F 的镜像版本目录(假 tar + 清单 + 校验和)。
#>
function New-ImageVersionFixture {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [ValidateSet('snapshot', 'release')][string]$Channel = 'snapshot',
        [Parameter(Mandatory = $true)][string]$Version,
        [int]$ImageCount = 2,
        # bare = file 只写文件名;prefixed = file 写成 images/<文件名>
        [ValidateSet('bare', 'prefixed')][string]$FileStyle = 'bare'
    )

    $channelRoot = Get-ChannelRoot -Channel $Channel -Override $Root
    $verDir = Join-Path $channelRoot 'images' $Version
    $entries = New-Object System.Collections.Generic.List[object]
    for ($i = 1; $i -le $ImageCount; $i++) {
        $service = "svc$i"
        $ref = "registry.invalid/test/mmorpg-${service}:0123456789ab"
        $tarName = ($ref -replace '[/:]', '_') + '.tar'
        Write-TestFile -Path (Join-Path $verDir 'images' $tarName) -Content ("fake-image-$service-$Version-" + ('x' * 256))
        $entries.Add([ordered]@{
            family   = 'go'
            service  = $service
            ref      = $ref
            image_id = 'sha256:' + (Get-TextSha256 -Text "image-$service-$Version")
            revision = '0123456789ab'
            file     = $(if ($FileStyle -eq 'prefixed') { "images/$tarName" } else { $tarName })
        })
    }
    Write-TestFile -Path (Join-Path $verDir 'images-manifest.json') -Content ($entries | ConvertTo-Json -Depth 5 -AsArray)
    Write-TestFile -Path (Join-Path $verDir 'build-info.json') -Content (@{ version = $Version; channel = $Channel } | ConvertTo-Json)
    New-Sha256Sums -Dir $verDir | Out-Null
    return [pscustomobject]@{ VersionDir = $verDir; Entries = $entries.ToArray() }
}

<#
.SYNOPSIS
    在 Dir 下生成 docker 桩 .ps1,返回 @{ Path; LogPath }。
#>
function New-DockerStub {
    param(
        [Parameter(Mandatory = $true)][string]$Dir,
        [Parameter(Mandatory = $true)][hashtable]$IdByRef,
        [int]$LoadExitCode = 0
    )

    $logPath = Join-Path $Dir 'docker-calls.log'
    $idsPath = Join-Path $Dir 'docker-ids.json'
    Write-TestFile -Path $idsPath -Content ($IdByRef | ConvertTo-Json)

    $template = @'
#requires -Version 7
# import_images.ps1 契约测试生成的 docker 桩:记录收到的参数;load 按预设退出码返回;
# image inspect 对预设 ref 回显预设镜像 ID,未知 ref 返回 1(与真实 docker 查不到镜像一致)。
$logPath = '__LOG_PATH__'
$idsPath = '__IDS_PATH__'
$loadExitCode = __LOAD_EXIT__
Add-Content -LiteralPath $logPath -Value ([string]::Join(' ', [string[]]$args)) -Encoding utf8NoBOM
if ($args.Count -ge 1 -and $args[0] -eq 'load') { exit $loadExitCode }
if ($args.Count -ge 2 -and $args[0] -eq 'image' -and $args[1] -eq 'inspect') {
    $ref = [string]$args[$args.Count - 1]
    $ids = Get-Content -LiteralPath $idsPath -Raw | ConvertFrom-Json -AsHashtable
    if ($ids.ContainsKey($ref)) { Write-Output $ids[$ref]; exit 0 }
    exit 1
}
exit 9
'@
    # 路径按单引号字面量嵌入:只需把 ' 转义成 ''
    $content = $template.Replace('__LOG_PATH__', $logPath.Replace("'", "''"))
    $content = $content.Replace('__IDS_PATH__', $idsPath.Replace("'", "''"))
    $content = $content.Replace('__LOAD_EXIT__', [string]$LoadExitCode)
    $stubPath = Join-Path $Dir 'docker-stub.ps1'
    Write-TestFile -Path $stubPath -Content $content
    return [pscustomobject]@{ Path = $stubPath; LogPath = $logPath }
}

<#
.SYNOPSIS
    把 fixture 第 1 个镜像的假 tar 换成 docker save 的最小真骨架(manifest.json + index.json + config blob),
    清单 image_id 改成 config digest(经典存储发布机记录的 .Id),重生成校验和。

.OUTPUTS
    @{ Ref; ConfigId = 'sha256:<config>'; TargetId = 'sha256:<index>' }(TargetId 即 containerd 存储导入后的 .Id)
#>
function Set-FixtureSaveLikeArchive {
    param([Parameter(Mandatory = $true)]$Fixture)

    try { Add-Type -AssemblyName System.Formats.Tar -ErrorAction Stop } catch { }
    if ($null -eq ('System.Formats.Tar.TarFile' -as [type])) { throw '生成真 tar 需要 pwsh 7.3+(System.Formats.Tar)' }

    $entry = $Fixture.Entries[0]
    $configJson = '{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}'
    $configHex = Get-TextSha256 -Text $configJson
    $targetId = 'sha256:' + (Get-TextSha256 -Text "index-$($entry.ref)")

    $src = New-CaseDir -Name ('tar-src-' + [guid]::NewGuid().ToString('N'))
    Write-TestFile -Path (Join-Path $src 'blobs' 'sha256' $configHex) -Content $configJson
    Write-TestFile -Path (Join-Path $src 'manifest.json') -Content (ConvertTo-Json -Depth 5 -InputObject @(
            [ordered]@{ Config = "blobs/sha256/$configHex"; RepoTags = @($entry.ref); Layers = @() }))
    Write-TestFile -Path (Join-Path $src 'index.json') -Content (ConvertTo-Json -Depth 5 -InputObject ([ordered]@{
                schemaVersion = 2
                manifests     = @([ordered]@{ mediaType = 'application/vnd.oci.image.index.v1+json'; digest = $targetId; size = 1 })
            }))

    $tarPath = Join-Path $Fixture.VersionDir 'images' $entry.file
    [System.IO.File]::Delete($tarPath)
    [System.Formats.Tar.TarFile]::CreateFromDirectory($src, $tarPath, $false)

    $entry.image_id = 'sha256:' + $configHex
    Write-TestFile -Path (Join-Path $Fixture.VersionDir 'images-manifest.json') -Content (@($Fixture.Entries) | ConvertTo-Json -Depth 5 -AsArray)
    New-Sha256Sums -Dir $Fixture.VersionDir | Out-Null
    return [pscustomobject]@{ Ref = $entry.ref; ConfigId = $entry.image_id; TargetId = $targetId }
}

function Get-StubCalls {
    param([string]$LogPath)
    if (-not [System.IO.File]::Exists($LogPath)) { return , @() }
    return , @([System.IO.File]::ReadAllLines($LogPath) | Where-Object { $_ -ne '' })
}

function Get-IdMap {
    param($Fixture)
    $map = @{}
    foreach ($e in $Fixture.Entries) { $map[$e.ref] = $e.image_id }
    return $map
}

function New-SnapshotName {
    param([int]$Index)
    return 'g' + ('{0:x12}' -f $Index)
}

function New-DatedDir {
    param([string]$Path, [datetime]$WriteTimeUtc)
    Write-TestFile -Path (Join-Path $Path 'sha256sums.txt') -Content ''
    [System.IO.Directory]::SetLastWriteTimeUtc($Path, $WriteTimeUtc)
}

$script:BaseTime = [datetime]::new(2026, 1, 1, 0, 0, 0, [System.DateTimeKind]::Utc)

try {

    # 陷阱制品根(见文件头):3 个诱饵快照按时间从旧到新,latest 指向最新的一个。
    # retention 任一 -KeepLast 1/2 -Force 用例若落到这里,都会删掉 g0000000dead0
    $script:TrapVersions = @('g0000000dead0', 'g0000000dead1', 'g0000000dead2')
    for ($i = 0; $i -lt $script:TrapVersions.Count; $i++) {
        $trapFx = New-ImageVersionFixture -Root $script:TrapRoot -Version $script:TrapVersions[$i] -ImageCount 1
        [System.IO.Directory]::SetLastWriteTimeUtc($trapFx.VersionDir, $script:BaseTime.AddYears(-3).AddDays($i))
    }
    $script:TrapImagesDir = Join-Path $script:TrapRoot 'snapshots' 'images'
    Set-LatestPointer -ChannelRoot (Join-Path $script:TrapRoot 'snapshots') -Kind images -Version $script:TrapVersions[2]
    $script:TrapLatestText = [System.IO.File]::ReadAllText((Join-Path $script:TrapImagesDir 'latest.json'))

    # ─────────────────────────────────────────────────────────────
    # 1. fetch_images.ps1
    # ─────────────────────────────────────────────────────────────

    Test-Case 'fetch:源制品被篡改 1 字节 -> 拒绝并报"哈希不符",OutDir 与临时目录都不留下' {
        $root = New-CaseDir -Name 'fetch-tamper-root'
        $outParent = New-CaseDir -Name 'fetch-tamper-out'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab'
        $tar = Join-Path $fx.VersionDir 'images' $fx.Entries[0].file
        $bytes = [System.IO.File]::ReadAllBytes($tar)
        $bytes[3] = $bytes[3] -bxor 0x01
        [System.IO.File]::WriteAllBytes($tar, $bytes)

        $out = Join-Path $outParent 'g0123456789ab'
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out)
        Assert-ToolFailed -Result $r -Pattern '哈希不符' -Because '坏制品必须在复制前被拒绝'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($out)) -Because '校验失败时不得创建 OutDir'
        Assert-Equal -Expected 0 -Actual (@(Get-ChildItem -LiteralPath $outParent -Force).Count) -Because '校验失败时不得留下 .fetching-* 临时目录'
    }

    Test-Case 'fetch:不带 -Version 按 latest.json 拉快照,落地目录可复验、无临时目录残留' {
        $root = New-CaseDir -Name 'fetch-ok-root'
        $outParent = New-CaseDir -Name 'fetch-ok-out'
        New-ImageVersionFixture -Root $root -Version 'g00000000000a' | Out-Null
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab'
        Set-LatestPointer -ChannelRoot (Get-ChannelRoot -Channel snapshot -Override $root) -Kind images -Version 'g0123456789ab'

        $out = Join-Path $outParent 'landed'
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-OutDir', $out)
        Assert-ToolSucceeded -Result $r -Because 'latest 指向的快照应当拉取成功'
        Assert-Match -Text $r.Output -Pattern 'latest\.json 指向快照 g0123456789ab' -Because '应当说明读的是 latest 指针'

        $msg = ''
        try { Test-Sha256Sums -Dir $out } catch { $msg = $_.Exception.Message }
        Assert-Equal -Expected '' -Actual $msg -Because '落地目录必须能独立通过 sha256sums 复验'
        foreach ($e in $fx.Entries) {
            Assert-True -Condition ([System.IO.File]::Exists((Join-Path $out 'images' $e.file))) -Because "落地目录缺少 images/$($e.file)"
        }
        $names = @(Get-ChildItem -LiteralPath $outParent -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'landed' -Actual $names -Because '成功后 OutDir 同级只应有 OutDir 本身(无 .fetching-* 残留)'
    }

    Test-Case 'fetch:OutDir 已存在且无 -Force -> 拒绝,原目录内容不动' {
        $root = New-CaseDir -Name 'fetch-exists-root'
        New-ImageVersionFixture -Root $root -Version 'g0123456789ab' | Out-Null
        $out = New-CaseDir -Name 'fetch-exists-out'
        Write-TestFile -Path (Join-Path $out 'keep-me.txt') -Content 'old'

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out)
        Assert-ToolFailed -Result $r -Pattern '目标目录已存在.*-Force' -Because '已存在的 OutDir 默认不覆盖'
        $names = @(Get-ChildItem -LiteralPath $out -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'keep-me.txt' -Actual $names -Because '被拒时原目录必须原样保留'
    }

    Test-Case 'fetch:-Force 替换已存在的旧落地目录,旧文件消失、新内容可复验、无备份残留' {
        $root = New-CaseDir -Name 'fetch-force-root'
        New-ImageVersionFixture -Root $root -Version 'g0123456789ab' | Out-Null
        $outParent = New-CaseDir -Name 'fetch-force-out'
        $out = Join-Path $outParent 'g0123456789ab'
        # 旧目录造成一次落地的形状:-Force 只替换落地目录
        Write-TestFile -Path (Join-Path $out 'images' 'stale.tar') -Content 'old'
        Write-TestFile -Path (Join-Path $out 'images-manifest.json') -Content '[]'
        New-Sha256Sums -Dir $out | Out-Null

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out, '-Force')
        Assert-ToolSucceeded -Result $r -Because '-Force 应当允许替换旧落地目录'
        Assert-True -Condition (-not [System.IO.File]::Exists((Join-Path $out 'images' 'stale.tar'))) -Because '旧目录里的文件不得混进新目录(混进去会被判清单外文件)'
        $msg = ''
        try { Test-Sha256Sums -Dir $out } catch { $msg = $_.Exception.Message }
        Assert-Equal -Expected '' -Actual $msg -Because '替换后的目录必须能通过复验'
        $names = @(Get-ChildItem -LiteralPath $outParent -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'g0123456789ab' -Actual $names -Because '替换后不得残留 .fetching-* / .replaced-* 目录'
    }

    Test-Case 'fetch:-Force 也不替换非落地目录(父目录 / 无关目录误传成 OutDir)-> 拒绝,原内容不动' {
        $root = New-CaseDir -Name 'fetch-force-notlanding-root'
        New-ImageVersionFixture -Root $root -Version 'g0123456789ab' | Out-Null
        $outParent = New-CaseDir -Name 'fetch-force-notlanding-parent'
        $out = Join-Path $outParent 'offline-images'
        # 形似 deploy/offline-images:里面是别的已落地版本 + 无关文件
        Write-TestFile -Path (Join-Path $out 'keep-me.txt') -Content 'precious'
        Write-TestFile -Path (Join-Path $out 'g00000000000a' 'sha256sums.txt') -Content ''

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out, '-Force')
        Assert-ToolFailed -Result $r -Pattern '不是 fetch 落地目录' -Because '-Force 只替换旧落地目录,误传父目录时不得整棵挪走删除'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $out 'keep-me.txt'))) -Because '被拒时无关文件必须还在'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $out 'g00000000000a' 'sha256sums.txt'))) -Because '被拒时其它已落地版本必须还在'
        $names = @(Get-ChildItem -LiteralPath $outParent -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'offline-images' -Actual $names -Because '被拒时不得留下 .fetching-* / .replaced-* 目录'
    }

    Test-Case 'fetch:目录名与 build-info 不符(制品目录被手工改名)-> 拒绝并报"身份与目录不符"' {
        $root = New-CaseDir -Name 'fetch-identity-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        [System.IO.Directory]::Move($fx.VersionDir, (Join-Path (Split-Path -Parent $fx.VersionDir) 'g00000000000a'))
        $out = Join-Path (New-CaseDir -Name 'fetch-identity-out') 'g00000000000a'

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g00000000000a', '-OutDir', $out)
        Assert-ToolFailed -Result $r -Pattern '镜像制品身份与目录不符' -Because '校验和不覆盖目录名,改名后的制品必须靠 build-info 身份核对拦下'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($out)) -Because '身份不符时不得落地'
    }

    Test-Case 'fetch:发布轨缺 -Version -> 拒绝' {
        $root = New-CaseDir -Name 'fetch-release-root'
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-Channel', 'release', '-ArtifactRoot', $root, '-OutDir', (Join-Path $script:TempRoot 'fetch-release-out'))
        Assert-ToolFailed -Result $r -Pattern '发布轨必须指定 -Version' -Because '发布轨不读 latest 指针,必须显式给版本'
    }

    Test-Case 'fetch:发布轨 -Version v1.2.3 从 releases/images/v1.2.3 落地' {
        $root = New-CaseDir -Name 'fetch-release-ok-root'
        New-ImageVersionFixture -Root $root -Channel release -Version 'v1.2.3' -ImageCount 1 | Out-Null
        $out = Join-Path (New-CaseDir -Name 'fetch-release-ok-out') 'v1.2.3'

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-Channel', 'release', '-Version', 'v1.2.3', '-ArtifactRoot', $root, '-OutDir', $out)
        Assert-ToolSucceeded -Result $r -Because '合法发布版本应当拉取成功'
        $info = Get-Content -LiteralPath (Join-Path $out 'build-info.json') -Raw | ConvertFrom-Json
        Assert-Equal -Expected 'release' -Actual $info.channel -Because '应当从 releases/ 轨道取,而不是 snapshots/'
    }

    Test-Case 'fetch:-Channel 大小写不敏感(Release)-> 按发布轨正常落地,不误报身份不符' {
        $root = New-CaseDir -Name 'fetch-release-case-root'
        New-ImageVersionFixture -Root $root -Channel release -Version 'v1.2.3' -ImageCount 1 | Out-Null
        $out = Join-Path (New-CaseDir -Name 'fetch-release-case-out') 'v1.2.3'

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-Channel', 'Release', '-Version', 'v1.2.3', '-ArtifactRoot', $root, '-OutDir', $out)
        Assert-ToolSucceeded -Result $r -Because 'ValidateSet 放行了 Release,build-info 身份核对不能因大小写把合法发布版本判成不符'
        Assert-NotMatch -Text $r.Output -Pattern '身份与目录不符' -Because '大小写不同不是制品被搬动或改名'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $out 'build-info.json'))) -Because '应当落地发布版本目录'
    }

    Test-Case 'fetch:发布轨版本号不合法(大写 V) -> 拒绝' {
        $root = New-CaseDir -Name 'fetch-release-bad-root'
        New-ImageVersionFixture -Root $root -Channel release -Version 'V1.2.3' -ImageCount 1 | Out-Null
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-Channel', 'release', '-Version', 'V1.2.3', '-ArtifactRoot', $root, '-OutDir', (Join-Path $script:TempRoot 'fetch-release-bad-out'))
        Assert-ToolFailed -Result $r -Pattern '发布版本号非法' -Because '发布版本号走 Test-ReleaseVersion,必须小写 v 开头(即使目录碰巧存在)'
    }

    Test-Case 'fetch:快照版本号带路径穿越 -> 拒绝' {
        $root = New-CaseDir -Name 'fetch-traversal-root'
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', '../../outside', '-OutDir', (Join-Path $script:TempRoot 'fetch-traversal-out'))
        Assert-ToolFailed -Result $r -Pattern '快照版本号非法' -Because '版本号就是目录名,必须先校验形状'
    }

    Test-Case 'fetch:OutDir 位于制品根之内 -> 拒绝(防 -Force 删掉不可变制品)' {
        $root = New-CaseDir -Name 'fetch-inside-root'
        New-ImageVersionFixture -Root $root -Version 'g0123456789ab' | Out-Null
        $out = Join-Path $root 'snapshots' 'images' 'g0123456789ab'
        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out, '-Force')
        Assert-ToolFailed -Result $r -Pattern 'OutDir 不得位于制品根之内' -Because 'OutDir 指回制品目录时 -Force 会删掉已发布版本'
        $msg = ''
        try { Test-Sha256Sums -Dir $out } catch { $msg = $_.Exception.Message }
        Assert-Equal -Expected '' -Actual $msg -Because '被拒后源制品必须完好'
    }

    # ─────────────────────────────────────────────────────────────
    # 2. import_images.ps1(docker 桩)
    # ─────────────────────────────────────────────────────────────

    Test-Case 'import:逐个 load images/<file> 后 inspect 核对 ID,全部一致 -> 成功' {
        $root = New-CaseDir -Name 'import-ok-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab'
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-ok-stub') -IdByRef (Get-IdMap $fx)

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolSucceeded -Result $r -Because 'ID 与清单一致时导入应成功'

        $calls = Get-StubCalls -LogPath $stub.LogPath
        Assert-Equal -Expected 4 -Actual $calls.Count -Because "2 个镜像各 1 次 load + 1 次 inspect(实际:$($calls -join ' | '))"
        for ($i = 0; $i -lt 2; $i++) {
            $e = $fx.Entries[$i]
            Assert-Match -Text $calls[2 * $i] -Pattern ('^load -i .*images.' + [regex]::Escape($e.file) + '$') -Because "第 $($i + 1) 个镜像应当先 docker load -i images/$($e.file)"
            Assert-Equal -Expected ("image inspect --format {{.Id}} " + $e.ref) -Actual $calls[2 * $i + 1] -Because "load 之后应当立刻 inspect $($e.ref) 取 ID"
        }
    }

    Test-Case 'import:镜像 ID 与清单不符 -> 失败并报出 ref' {
        $root = New-CaseDir -Name 'import-mismatch-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab'
        $ids = Get-IdMap $fx
        $badRef = $fx.Entries[1].ref
        $ids[$badRef] = 'sha256:' + ('f' * 64)
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-mismatch-stub') -IdByRef $ids

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r -Pattern ('镜像 ID 不符[\s\S]*' + [regex]::Escape($badRef)) -Because '本机 tag 指向的不是清单里的镜像时必须失败并指出是哪个 ref'
        Assert-NotMatch -Text $r.Output -Pattern '导入完成' -Because 'ID 不符时不得打印成功汇总'
    }

    Test-Case 'import:目录被篡改 -> 先报"哈希不符",docker 一次都不调用' {
        $root = New-CaseDir -Name 'import-tamper-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab'
        Write-TestFile -Path (Join-Path $fx.VersionDir 'build-info.json') -Content '{"version":"tampered"}'
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-tamper-stub') -IdByRef (Get-IdMap $fx)

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r -Pattern '哈希不符 build-info\.json' -Because '有 sha256sums.txt 时必须先校验'
        Assert-Equal -Expected 0 -Actual ((Get-StubCalls -LogPath $stub.LogPath).Count) -Because '校验失败时不得调用 docker'
    }

    Test-Case 'import:清单 file 写成 images/<文件名> 也能导入' {
        $root = New-CaseDir -Name 'import-prefixed-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1 -FileStyle prefixed
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-prefixed-stub') -IdByRef (Get-IdMap $fx)

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolSucceeded -Result $r -Because 'images/ 前缀的相对写法应当被接受'
        $calls = Get-StubCalls -LogPath $stub.LogPath
        Assert-Match -Text $calls[0] -Pattern 'images.registry\.invalid_test_mmorpg-svc1_0123456789ab\.tar$' -Because '不得拼成 images/images/...'
    }

    Test-Case 'import:清单 file 含 .. -> 拒绝,docker 一次都不调用' {
        $root = New-CaseDir -Name 'import-traversal-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        $manifestPath = Join-Path $fx.VersionDir 'images-manifest.json'
        $entry = $fx.Entries[0]
        $entry.file = '../../evil.tar'
        Write-TestFile -Path $manifestPath -Content (@($entry) | ConvertTo-Json -Depth 5 -AsArray)
        New-Sha256Sums -Dir $fx.VersionDir | Out-Null
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-traversal-stub') -IdByRef (Get-IdMap $fx)

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r -Pattern 'file 非法' -Because '清单不得引用版本目录外的 tar'
        Assert-Equal -Expected 0 -Actual ((Get-StubCalls -LogPath $stub.LogPath).Count) -Because '清单非法时不得调用 docker'
    }

    Test-Case 'import:docker load 失败 -> 失败并报"docker load 失败"' {
        $root = New-CaseDir -Name 'import-loadfail-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-loadfail-stub') -IdByRef (Get-IdMap $fx) -LoadExitCode 1

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r -Pattern 'docker load 失败' -Because 'load 退出码非 0 不能当成功'
    }

    Test-Case 'import:缺 sha256sums.txt 默认拒绝且不调 docker;带 -AllowMissingChecksums 才导入' {
        $root = New-CaseDir -Name 'import-nosums-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        Remove-Item -LiteralPath (Join-Path $fx.VersionDir 'sha256sums.txt') -Force
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-nosums-stub') -IdByRef (Get-IdMap $fx)

        $r1 = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r1 -Pattern '缺少 sha256sums\.txt.拒绝导入' -Because 'publish / fetch 产出的目录必定带校验文件,缺失默认 fail-closed'
        Assert-Equal -Expected 0 -Actual ((Get-StubCalls -LogPath $stub.LogPath).Count) -Because '缺校验文件时不得调用 docker'

        $r2 = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path, '-AllowMissingChecksums')
        Assert-ToolSucceeded -Result $r2 -Because '显式 -AllowMissingChecksums 时应当导入'
        Assert-Match -Text $r2.Output -Pattern '\[WARN\].*AllowMissingChecksums' -Because '降级放行必须留下 WARN'
        Assert-Equal -Expected 2 -Actual ((Get-StubCalls -LogPath $stub.LogPath).Count) -Because '1 个镜像应 load + inspect 各 1 次'
    }

    Test-Case 'import:本机 .Id 与清单不同但同属归档内镜像身份(发布机与本机镜像存储不同)-> 成功' {
        $root = New-CaseDir -Name 'import-crossstore-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        $archive = Set-FixtureSaveLikeArchive -Fixture $fx
        # 发布机经典存储记录的是 config digest;本机 containerd 存储导入后 .Id 是 index digest
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-crossstore-stub') -IdByRef @{ ($archive.Ref) = $archive.TargetId }

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolSucceeded -Result $r -Because '.Id 口径不同但都指向归档里同一个镜像时不能误报 ID 不符'
        Assert-Match -Text $r.Output -Pattern '镜像存储口径不同' -Because '应当说明是按归档内 digest 复核通过的'
    }

    Test-Case 'import:本机 .Id 不在归档内镜像身份里(真 tar)-> 失败并报出 ref' {
        $root = New-CaseDir -Name 'import-crossstore-bad-root'
        $fx = New-ImageVersionFixture -Root $root -Version 'g0123456789ab' -ImageCount 1
        $archive = Set-FixtureSaveLikeArchive -Fixture $fx
        $stub = New-DockerStub -Dir (New-CaseDir -Name 'import-crossstore-bad-stub') -IdByRef @{ ($archive.Ref) = ('sha256:' + ('e' * 64)) }

        $r = Invoke-Tool -ScriptName 'import_images.ps1' -Arguments @('-Dir', $fx.VersionDir, '-DockerCommand', $stub.Path)
        Assert-ToolFailed -Result $r -Pattern ('镜像 ID 不符[\s\S]*' + [regex]::Escape($archive.Ref)) -Because '本机 tag 指向的镜像不在归档身份里,复核后仍必须判不符'
        Assert-Match -Text $r.Output -Pattern ('归档内镜像身份[\s\S]*' + [regex]::Escape($archive.TargetId)) -Because '复核失败时应列出归档里的身份,便于排查'
        Assert-NotMatch -Text $r.Output -Pattern '导入完成' -Because 'ID 不符时不得打印成功汇总'
    }

    # ─────────────────────────────────────────────────────────────
    # 3. artifacts_retention.ps1
    # ─────────────────────────────────────────────────────────────

    # 5 个快照,时间从旧到新 g000000000001 .. g000000000005;另有 releases 下一个更老的版本
    function New-RetentionFixture {
        param([string]$Name)
        $root = New-CaseDir -Name $Name
        $snapImages = Join-Path $root 'snapshots' 'images'
        for ($i = 1; $i -le 5; $i++) {
            New-DatedDir -Path (Join-Path $snapImages (New-SnapshotName $i)) -WriteTimeUtc $script:BaseTime.AddDays($i)
        }
        $relVer = Join-Path $root 'releases' 'images' 'v0.0.1'
        Write-TestFile -Path (Join-Path $relVer 'build-info.json') -Content '{"version":"v0.0.1"}'
        [System.IO.Directory]::SetLastWriteTimeUtc($relVer, $script:BaseTime.AddYears(-1))
        return [pscustomobject]@{ Root = $root; SnapImages = $snapImages; ReleaseVersionDir = $relVer }
    }

    function Get-SnapshotNames {
        param([string]$SnapImages)
        return (@(Get-ChildItem -LiteralPath $SnapImages -Directory -Force | ForEach-Object Name | Sort-Object) -join ',')
    }

    Test-Case 'retention:默认 dry-run 只列计划,不删任何目录' {
        $fx = New-RetentionFixture -Name 'ret-dry'
        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '2')
        Assert-ToolSucceeded -Result $r -Because 'dry-run 应当成功'
        foreach ($i in 1..3) {
            Assert-Match -Text $r.Output -Pattern ('\[DRY \] 将删除:snapshots/images/' + (New-SnapshotName $i)) -Because "最旧的 3 个应列入计划($(New-SnapshotName $i))"
        }
        Assert-NotMatch -Text $r.Output -Pattern ('将删除:snapshots/images/(' + (New-SnapshotName 4) + '|' + (New-SnapshotName 5) + ')') -Because '最新的 2 个不得列入计划'
        Assert-Equal -Expected ((1..5 | ForEach-Object { New-SnapshotName $_ }) -join ',') -Actual (Get-SnapshotNames $fx.SnapImages) -Because 'dry-run 不得删除任何目录'
    }

    Test-Case 'retention:-Force 删最旧的,保留最近 N 个;releases/ 不动' {
        $fx = New-RetentionFixture -Name 'ret-force'
        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '2', '-Force')
        Assert-ToolSucceeded -Result $r -Because '-Force 清理应当成功'
        Assert-Equal -Expected ((4..5 | ForEach-Object { New-SnapshotName $_ }) -join ',') -Actual (Get-SnapshotNames $fx.SnapImages) -Because '只应剩最新的 2 个快照'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $fx.ReleaseVersionDir 'build-info.json'))) -Because 'releases/ 下的版本(即使比所有快照都旧)永不触碰'
        Assert-NotMatch -Text $r.Output -Pattern 'releases/images' -Because '输出里不应出现对 releases/ 的任何处理'
    }

    Test-Case 'retention:latest.json 指向的旧版本即使超出保留数也不删' {
        $fx = New-RetentionFixture -Name 'ret-latest'
        $oldest = New-SnapshotName 1
        Set-LatestPointer -ChannelRoot (Join-Path $fx.Root 'snapshots') -Kind images -Version $oldest

        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '2', '-Force')
        Assert-ToolSucceeded -Result $r -Because '清理应当成功'
        Assert-Match -Text $r.Output -Pattern ([regex]::Escape($oldest) + ' 是 latest\.json 指向的版本') -Because '应当说明为何保留'
        Assert-Equal -Expected (@($oldest, (New-SnapshotName 4), (New-SnapshotName 5)) -join ',') -Actual (Get-SnapshotNames $fx.SnapImages) -Because 'latest 指向的版本 + 最新 2 个保留,其余删除'
    }

    Test-Case 'retention:.tmp-* 与非快照名目录即使最旧也不删;超过 24 小时的 .tmp-* 给出 WARN' {
        $fx = New-RetentionFixture -Name 'ret-skip'
        New-DatedDir -Path (Join-Path $fx.SnapImages ('.tmp-' + (New-SnapshotName 9) + '-4242')) -WriteTimeUtc $script:BaseTime.AddYears(-2)
        New-DatedDir -Path (Join-Path $fx.SnapImages 'manual-backup') -WriteTimeUtc $script:BaseTime.AddYears(-2)

        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '1', '-Force')
        Assert-ToolSucceeded -Result $r -Because '清理应当成功(遗留 staging 只提示,不算失败)'
        Assert-Match -Text $r.Output -Pattern '\[WARN\] 疑似被中断发布遗留的 staging.*snapshots/images/\.tmp-g000000000009-4242' -Because '被中断发布遗留的 staging 不删,但必须看得见,否则排期清理一直假绿'
        Assert-Match -Text $r.Output -Pattern '1 个疑似遗留 staging 待人工确认' -Because '汇总行应写出遗留 staging 数量'
        $left = Get-SnapshotNames $fx.SnapImages
        Assert-Match -Text $left -Pattern '\.tmp-g000000000009-4242' -Because '发布中的 staging 目录不得被删'
        Assert-Match -Text $left -Pattern 'manual-backup' -Because '不是快照版本名的目录不得被删'
        Assert-Match -Text $left -Pattern (New-SnapshotName 5) -Because '最新快照保留'
        Assert-NotMatch -Text $left -Pattern (New-SnapshotName 4) -Because 'KeepLast=1 时次新快照应被删'
    }

    Test-Case 'retention:-KeepLast 0 -> 拒绝' {
        $fx = New-RetentionFixture -Name 'ret-keep0'
        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '0', '-Force')
        Assert-ToolFailed -Result $r -Pattern '-KeepLast 至少为 1' -Because '保留 0 个等于全删'
        Assert-Equal -Expected ((1..5 | ForEach-Object { New-SnapshotName $_ }) -join ',') -Actual (Get-SnapshotNames $fx.SnapImages) -Because '参数非法时不得删除任何目录'
    }

    Test-Case 'retention:.deleting-* 残留 dry-run 只列出、-Force 清掉;.tmp-* 不动;删除后不留 .deleting-*' {
        $fx = New-RetentionFixture -Name 'ret-residue'
        $residue = Join-Path $fx.SnapImages ('.deleting-' + (New-SnapshotName 7) + '-' + ('0' * 32))
        Write-TestFile -Path (Join-Path $residue 'images' 'half.tar') -Content 'half-deleted'
        $staging = Join-Path $fx.SnapImages ('.tmp-' + (New-SnapshotName 8) + '-4242')
        Write-TestFile -Path (Join-Path $staging 'build-info.json') -Content '{}'

        $dry = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '3')
        Assert-ToolSucceeded -Result $dry -Because 'dry-run 应当成功'
        Assert-Match -Text $dry.Output -Pattern '将清理上次未删完的残留' -Because 'dry-run 应列出 .deleting-* 残留'
        Assert-NotMatch -Text $dry.Output -Pattern '疑似被中断发布遗留的 staging' -Because '刚创建的 .tmp-*(可能正在发布)不得报成遗留'
        Assert-True -Condition ([System.IO.Directory]::Exists($residue)) -Because 'dry-run 不得删除残留'

        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '3', '-Force')
        Assert-ToolSucceeded -Result $r -Because '-Force 清理应当成功'
        # 目录名含 "." 前缀,区域排序下位置不稳定:逐项断言而不是比对整串
        $left = Get-SnapshotNames $fx.SnapImages
        Assert-NotMatch -Text $left -Pattern '\.deleting-' -Because '残留与本次删除的临时名都不得留下'
        Assert-Match -Text $left -Pattern '\.tmp-g000000000008-4242' -Because '发布中的 staging 目录不得被删'
        foreach ($i in 3..5) { Assert-Match -Text $left -Pattern (New-SnapshotName $i) -Because "最新 3 个快照保留($(New-SnapshotName $i))" }
        foreach ($i in 1..2) { Assert-NotMatch -Text $left -Pattern (New-SnapshotName $i) -Because "过期快照应被删($(New-SnapshotName $i))" }
    }

    Test-Case 'fetch / retention:制品根不存在 -> 报"制品根不存在"并失败,且不创建该目录' {
        $missing = Join-Path $script:TempRoot 'no-such-artifact-root'
        $r1 = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $missing, '-Version', 'g0123456789ab', '-OutDir', (Join-Path $script:TempRoot 'fetch-missing-root-out'))
        Assert-ToolFailed -Result $r1 -Pattern '制品根不存在' -Because 'fetch 指错制品根时要报出真正原因,而不是"制品不存在"'
        $r2 = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $missing, '-Force')
        Assert-ToolFailed -Result $r2 -Pattern '制品根不存在' -Because '排期任务指错制品根不能"无快照需要清理"地假绿'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($missing)) -Because '只读用途不得建出制品根'
    }

    # ─────────────────────────────────────────────────────────────
    # 4. 隔离自检
    # ─────────────────────────────────────────────────────────────

    Test-Case '自检:所有脚本都尊重 -ArtifactRoot,环境变量指向的陷阱制品根原样未动、输出里从未出现' {
        foreach ($v in $script:TrapVersions) {
            $msg = ''
            try { Test-Sha256Sums -Dir (Join-Path $script:TrapImagesDir $v) } catch { $msg = $_.Exception.Message }
            Assert-Equal -Expected '' -Actual $msg -Because "诱饵快照 $v 被删或被改 = 某个脚本忽略了 -ArtifactRoot、落到了环境变量上"
        }
        $entries = @(Get-ChildItem -LiteralPath $script:TrapImagesDir -Force | ForEach-Object Name)
        Assert-Equal -Expected 4 -Actual $entries.Count -Because "陷阱 snapshots/images 只应有 3 个诱饵 + latest.json(实际:$($entries -join ','))"
        Assert-Equal -Expected $script:TrapLatestText -Actual ([System.IO.File]::ReadAllText((Join-Path $script:TrapImagesDir 'latest.json'))) -Because '陷阱 latest.json 不得被改写'
        Assert-True -Condition ($script:AllToolOutput.Length -gt 0) -Because '自检依赖前面用例的子进程输出,输出为空说明汇总没接上'
        Assert-NotMatch -Text $script:AllToolOutput.ToString() -Pattern '(env-trap-root|g0000000dead)' -Because '子进程输出出现陷阱制品根或诱饵版本 = 某个脚本读了环境变量而不是 -ArtifactRoot'
    }
}
finally {
    if ([System.IO.Directory]::Exists($script:TempRoot)) {
        Remove-Item -LiteralPath $script:TempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'artifacts_fetch_import_retention')
