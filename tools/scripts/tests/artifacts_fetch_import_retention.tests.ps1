#requires -Version 7
<#
.SYNOPSIS
    fetch_images.ps1 / import_images.ps1 / artifacts_retention.ps1 的契约测试。

.DESCRIPTION
    三个脚本都以子进程跑(与真实使用方式一致:pwsh -File),断言退出码 + 输出文本 + 文件系统结果。
      - fetch:坏制品不落地、正常落地可复验、已存在目录无 -Force 不动、-Force 完整替换、路径穿越拒绝
      - import:docker 用临时目录里生成的桩 .ps1 代替(记录收到的参数,对 inspect 回显预设镜像 ID),
               不依赖真实 docker;镜像 ID 与清单不符必须失败并报出 ref
      - retention:默认 dry-run 不删、-Force 删最旧、releases/ 不动、latest.json 指向的不删、
               .tmp-* 与非快照名目录不删

    制品根一律用 -ArtifactRoot 指到本测试的临时目录;环境变量 MMORPG_ARTIFACT_ROOT 被指向一个
    "陷阱"目录,脚本若忽略 -ArtifactRoot 就会建出它,最后一条用例专门抓这个。
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

function Invoke-Tool {
    param([string]$ScriptName, [string[]]$Arguments)
    return Invoke-ToolScript -ScriptName $ScriptName -Arguments $Arguments -Env $script:ChildEnv
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

    Test-Case 'fetch:-Force 替换已存在的 OutDir,旧文件消失、新内容可复验、无备份残留' {
        $root = New-CaseDir -Name 'fetch-force-root'
        New-ImageVersionFixture -Root $root -Version 'g0123456789ab' | Out-Null
        $outParent = New-CaseDir -Name 'fetch-force-out'
        $out = Join-Path $outParent 'g0123456789ab'
        Write-TestFile -Path (Join-Path $out 'stale.txt') -Content 'old'

        $r = Invoke-Tool -ScriptName 'fetch_images.ps1' -Arguments @('-ArtifactRoot', $root, '-Version', 'g0123456789ab', '-OutDir', $out, '-Force')
        Assert-ToolSucceeded -Result $r -Because '-Force 应当允许替换'
        Assert-True -Condition (-not [System.IO.File]::Exists((Join-Path $out 'stale.txt'))) -Because '旧目录里的文件不得混进新目录(混进去会被判清单外文件)'
        $msg = ''
        try { Test-Sha256Sums -Dir $out } catch { $msg = $_.Exception.Message }
        Assert-Equal -Expected '' -Actual $msg -Because '替换后的目录必须能通过复验'
        $names = @(Get-ChildItem -LiteralPath $outParent -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'g0123456789ab' -Actual $names -Because '替换后不得残留 .fetching-* / .replaced-* 目录'
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

    Test-Case 'retention:.tmp-* 与非快照名目录即使最旧也不删' {
        $fx = New-RetentionFixture -Name 'ret-skip'
        New-DatedDir -Path (Join-Path $fx.SnapImages ('.tmp-' + (New-SnapshotName 9) + '-4242')) -WriteTimeUtc $script:BaseTime.AddYears(-2)
        New-DatedDir -Path (Join-Path $fx.SnapImages 'manual-backup') -WriteTimeUtc $script:BaseTime.AddYears(-2)

        $r = Invoke-Tool -ScriptName 'artifacts_retention.ps1' -Arguments @('-ArtifactRoot', $fx.Root, '-KeepLast', '1', '-Force')
        Assert-ToolSucceeded -Result $r -Because '清理应当成功'
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

    # ─────────────────────────────────────────────────────────────
    # 4. 隔离自检
    # ─────────────────────────────────────────────────────────────

    Test-Case '自检:所有脚本都尊重 -ArtifactRoot,没有落到环境变量指向的陷阱目录' {
        Assert-True -Condition (-not [System.IO.Directory]::Exists($script:TrapRoot)) -Because "出现了 $($script:TrapRoot) = 某个脚本忽略了 -ArtifactRoot"
    }
}
finally {
    if ([System.IO.Directory]::Exists($script:TempRoot)) {
        Remove-Item -LiteralPath $script:TempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'artifacts_fetch_import_retention')
