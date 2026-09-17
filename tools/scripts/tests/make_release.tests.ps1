#requires -Version 7
<#
.SYNOPSIS
    make_release.ps1 的契约测试:修复内容来源、版本交叉校验、脏制品守卫、不可变与落盘产物。

.DESCRIPTION
    每个用例在临时目录里造一份"publish_images.ps1 -Version 产出的 release 轨镜像离线包"
    (images-manifest.json / build-info.json / 假 tar,sha256sums.txt 用 artifacts_lib.ps1 的
    New-Sha256Sums 生成,与真实发布同一个生产者),再以子进程跑 make_release.ps1。
    不依赖 docker、网络、git 远端;临时目录跑完即删。

    负向用例一律断言**错误文本**,并确认没有留下 manifest 哨兵 —— 只断退出码的负向测试,
    分不清是被测守卫拦下的还是脚本自己写错了。

.EXAMPLE
    pwsh -File tools/scripts/tests/make_release.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) 'lib' 'artifacts_lib.ps1')

Write-Host ""
Write-Host "=== make_release.ps1 契约测试 ==="

$script:TestRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("make-release-tests-" + [guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($script:TestRoot) | Out-Null
$script:CaseSeq = 0

$FixtureCommit = '0123456789ab'
$FixtureTablesSha = ('ab' * 32)

# 标准 CHANGELOG:1.2.3 段带日期,上下各有一段干扰内容,用来确认段落边界切得准
$ChangelogWithDate = @'
# 更新日志

## [Unreleased]

- 未发布的改动:不应出现在 1.2.3 的发布说明里

## [1.2.3] - 2026-09-16

### 修复

- 修复 A 问题(1.2.3 专属)

## [1.2.2] - 2026-09-01

- 1.2.2 的旧内容
'@

function Write-TestFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $parent = Split-Path -Parent $Path
    if (-not [System.IO.Directory]::Exists($parent)) { [System.IO.Directory]::CreateDirectory($parent) | Out-Null }
    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

<#
.SYNOPSIS
    造一份 release 轨镜像离线包 + 仓库根(只放 CHANGELOG.md)。
#>
function New-ReleaseFixture {
    param(
        [string]$ImagesVersion = 'v1.2.3',
        [AllowEmptyString()][string]$AppVersion = 'v1.2.3',
        [bool]$Dirty = $false,
        [string]$Changelog = $ChangelogWithDate,
        # 不写 CHANGELOG.md(不能靠 -Changelog $null:[string] 参数会把 $null 转成空串)
        [switch]$NoChangelog
    )

    $script:CaseSeq++
    $caseDir = Join-Path $script:TestRoot ("case-{0:d2}" -f $script:CaseSeq)
    $repo = Join-Path $caseDir 'repo'
    $root = Join-Path $caseDir 'artifacts'
    [System.IO.Directory]::CreateDirectory($repo) | Out-Null
    if (-not $NoChangelog) { Write-TestFile -Path (Join-Path $repo 'CHANGELOG.md') -Text $Changelog }

    $tag = "$ImagesVersion-$FixtureCommit"
    $verDir = Join-Path $root 'releases' 'images' $ImagesVersion
    $refs = @("registry.invalid/test/mmorpg-node:$tag", "registry.invalid/test/mmorpg-login:$tag")
    $families = @('cpp', 'go')
    $entries = @()
    for ($i = 0; $i -lt $refs.Count; $i++) {
        $fileName = ($refs[$i] -replace '[/:]', '_') + '.tar'
        Write-TestFile -Path (Join-Path $verDir 'images' $fileName) -Text "fake image archive $($refs[$i])"
        $entries += [ordered]@{
            family   = $families[$i]
            service  = @('node', 'login')[$i]
            ref      = $refs[$i]
            image_id = 'sha256:' + ([string]$i * 64)
            revision = $FixtureCommit
            file     = "images/$fileName"
        }
    }
    Write-TestFile -Path (Join-Path $verDir 'images-manifest.json') -Text (ConvertTo-Json -InputObject $entries -Depth 5)
    $buildInfo = [ordered]@{
        version           = $ImagesVersion
        channel           = 'release'
        app_version       = $AppVersion
        vcs               = 'git'
        source_rev        = "g$FixtureCommit"
        commit            = $FixtureCommit
        dirty             = $Dirty
        image_tag         = $tag
        families          = $families
        image_count       = $refs.Count
        tables_sha256     = $FixtureTablesSha
        tables_file_count = 3
        published_at      = '2026-09-16T00:00:00Z'
        machine           = 'fixture'
        publisher         = 'fixture'
    }
    Write-TestFile -Path (Join-Path $verDir 'build-info.json') -Text ($buildInfo | ConvertTo-Json -Depth 5)
    New-Sha256Sums -Dir $verDir | Out-Null
    Write-TestFile -Path (Join-Path $root 'releases' 'images' 'latest.json') `
        -Text (@{ version = $ImagesVersion; channel = 'release'; published_at = '2026-09-16T00:00:00Z' } | ConvertTo-Json)

    return @{
        CaseDir      = $caseDir
        Repo         = $repo
        Root         = $root
        VersionDir   = $verDir
        Refs         = $refs
        ManifestsDir = (Join-Path $root 'releases' 'manifests')
    }
}

function Invoke-MakeRelease {
    param(
        [Parameter(Mandatory = $true)][hashtable]$Fixture,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )
    $argv = @('-ArtifactRoot', $Fixture.Root, '-RepoRoot', $Fixture.Repo) + $Arguments
    # 清掉宿主可能设置的制品根,用例只认 -ArtifactRoot
    $run = Invoke-ToolScript -ScriptName 'make_release.ps1' -Arguments $argv -Env @{ MMORPG_ARTIFACT_ROOT = '' }
    $run.Output = ConvertTo-FlatText -Text $run.Output
    return $run
}

<#
.SYNOPSIS
    把子进程输出压成一行再做正则断言。

.DESCRIPTION
    脚本以 throw 失败时,pwsh 默认的 ConciseView 会把异常消息里的换行压成空格,
    再按控制台宽度在空白处折行并给每行加 "     | " 前缀;宽度随宿主终端变化(Windows 控制台 /
    Linux CI 无 TTY 各不相同)。不先压平,断言会在某台机器上因为折行点刚好落在模式中间而假红。
    断言模式因此只用单个空格,不依赖换行。
#>
function ConvertTo-FlatText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return (($Text -replace '\s*\r?\n\s*(\|\s?)?', ' ') -replace ' {2,}', ' ')
}

function Read-JsonFile {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable)
}

function Assert-NoReleaseSentinel {
    param([Parameter(Mandatory = $true)][hashtable]$Fixture, [Parameter(Mandatory = $true)][string]$Version)
    $json = Join-Path $Fixture.ManifestsDir "$Version.json"
    Assert-True -Condition (-not [System.IO.File]::Exists($json)) -Because "被拒绝的发布不得留下 manifest 哨兵($json),否则修好后重跑会被不可变守卫挡住"
}

try {

    Test-Case "负向:版本号不是 vX.Y.Z[-pre](缺 v / 大写 V / 两段 / 四段)必须拒绝" {
        $fx = New-ReleaseFixture
        foreach ($bad in @('1.2.3', 'V1.2.3', 'v1.2', 'v1.2.3.4')) {
            $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', $bad)
            Assert-True -Condition ($run.ExitCode -ne 0) -Because "版本号 '$bad' 必须被拒绝"
            Assert-Match -Text $run.Output -Pattern '发布版本号非法' -Because "'$bad' 的失败必须来自版本号校验,而不是别的路径"
            Assert-NoReleaseSentinel -Fixture $fx -Version $bad
        }
    }

    Test-Case "成功:CHANGELOG 带日期段落,.json/.md 都落盘且 manifest 字段正确" {
        $fx = New-ReleaseFixture
        $digestsPath = Join-Path $fx.CaseDir 'digests.json'
        $digestMap = [ordered]@{}
        foreach ($ref in $fx.Refs) {
            $repo = $ref.Substring(0, $ref.LastIndexOf(':'))
            $digestMap[$ref] = "$repo@sha256:" + ('c' * 64)
        }
        Write-TestFile -Path $digestsPath -Text ($digestMap | ConvertTo-Json)

        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3', '-ImageDigestsFile', $digestsPath)
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "合法输入应当发布成功。输出: $($run.Output)"

        $jsonPath = Join-Path $fx.ManifestsDir 'v1.2.3.json'
        $mdPath = Join-Path $fx.ManifestsDir 'v1.2.3.md'
        Assert-True -Condition ([System.IO.File]::Exists($jsonPath)) -Because "manifest .json 必须落盘"
        Assert-True -Condition ([System.IO.File]::Exists($mdPath)) -Because "release notes .md 必须落盘"
        $leftovers = @(Get-ChildItem -LiteralPath $fx.ManifestsDir -Force | Where-Object { $_.Name -like '*.tmp-*' })
        Assert-Equal -Expected 0 -Actual $leftovers.Count -Because "成功后不得残留 tmp 文件"

        $m = Read-JsonFile -Path $jsonPath
        Assert-Equal -Expected 'v1.2.3' -Actual $m['version'] -Because "manifest.version"
        Assert-Match -Text $m['notes'] -Pattern '修复 A 问题\(1\.2\.3 专属\)' -Because "notes 必须取 [1.2.3] 段正文"
        Assert-Match -Text $m['notes'] -Pattern '### 修复' -Because "段内三级标题属于段落正文"
        Assert-NotMatch -Text $m['notes'] -Pattern '未发布的改动|1\.2\.2 的旧内容' -Because "不得混入 [Unreleased] 或下一个版本段"
        Assert-True -Condition ($null -ne $m['created_at']) -Because "manifest.created_at 必须有值"
        Assert-True -Condition (-not [string]::IsNullOrWhiteSpace([string]$m['machine'])) -Because "manifest.machine 必须有值"
        Assert-True -Condition ($m.Contains('publisher')) -Because "manifest 必须有 publisher 字段"
        Assert-Equal -Expected $FixtureCommit -Actual $m['source']['commit'] -Because "source.commit 取自镜像 build-info"
        Assert-Equal -Expected $false -Actual $m['source']['dirty'] -Because "source.dirty"
        Assert-Equal -Expected 'v1.2.3' -Actual $m['images']['version'] -Because "images.version 取自 latest.json"
        Assert-Equal -Expected 'releases/images/v1.2.3' -Actual $m['images']['path'] -Because "images.path 是制品根下的相对路径,'/' 分隔"
        Assert-Equal -Expected "v1.2.3-$FixtureCommit" -Actual $m['images']['image_tag'] -Because "images.image_tag"
        $list = @($m['images']['image_list'])
        Assert-Equal -Expected 2 -Actual $list.Count -Because "image_list 是 images-manifest.json 的完整内容"
        Assert-Equal -Expected ($fx.Refs -join ',') -Actual (($list | ForEach-Object { $_['ref'] }) -join ',') -Because "image_list 的 ref 与顺序必须原样保留"
        Assert-Equal -Expected 'images/registry.invalid_test_mmorpg-node_v1.2.3-0123456789ab.tar' -Actual $list[0]['file'] -Because "image_list 条目字段原样保留"
        foreach ($ref in $fx.Refs) {
            Assert-Equal -Expected $digestMap[$ref] -Actual $m['images']['digests'][$ref] -Because "digests 必须按 ref 记录"
        }
        Assert-Equal -Expected $FixtureTablesSha -Actual $m['tables']['sha256'] -Because "tables.sha256 取自镜像 build-info(出包时的表),不在发布机上现算"
        Assert-Equal -Expected 3 -Actual $m['tables']['file_count'] -Because "tables.file_count"

        $md = [System.IO.File]::ReadAllText($mdPath)
        Assert-Match -Text $md -Pattern 'v1\.2\.3' -Because ".md 标题要有版本号"
        Assert-Match -Text $md -Pattern '修复 A 问题' -Because ".md 要带修复内容"
        Assert-Match -Text $md -Pattern 'mmorpg-login' -Because ".md 要列镜像清单"
    }

    Test-Case "成功:CHANGELOG 段落标题不带日期也能取到" {
        $changelog = @'
# 更新日志

## [Unreleased]

## [1.2.4]

- 无日期标题的内容

## [1.2.3] - 2026-09-16

- 旧版本内容
'@
        $fx = New-ReleaseFixture -ImagesVersion 'v1.2.4' -AppVersion 'v1.2.4' -Changelog $changelog
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.4')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "'## [1.2.4]' 是合法标题。输出: $($run.Output)"
        $m = Read-JsonFile -Path (Join-Path $fx.ManifestsDir 'v1.2.4.json')
        Assert-Equal -Expected '- 无日期标题的内容' -Actual $m['notes'] -Because "段落正文去掉首尾空行后原样写入,且不越过下一个 '## ' 标题"
    }

    Test-Case "负向:CHANGELOG 没有精确的 [X.Y.Z] 段落必须拒绝(相近版本号不算)" {
        $changelog = @'
# 更新日志

## [Unreleased]

## [1.2.3-rc.1] - 2026-09-10

- 预发布内容

## [11.2.3] - 2026-09-01

- 另一个版本
'@
        $fx = New-ReleaseFixture -Changelog $changelog
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "找不到段落必须拒绝发布"
        Assert-Match -Text $run.Output -Pattern '取不到修复内容,拒绝发布' -Because "错误要来自修复内容解析这一步"
        Assert-Match -Text $run.Output -Pattern "没有 '## \[1\.2\.3\]' 段" -Because "错误要点名缺的是哪个版本段(判定与 release.yml 预检共用 Test-ChangelogReleaseSection)"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'
        Assert-True -Condition (-not [System.IO.File]::Exists((Join-Path $fx.ManifestsDir 'v1.2.3.md'))) -Because "修复内容缺失时 .md 也不该写"

        $fx2 = New-ReleaseFixture -NoChangelog
        $run2 = Invoke-MakeRelease -Fixture $fx2 -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run2.ExitCode -ne 0) -Because "没有 CHANGELOG.md 且没给 -Notes 必须拒绝"
        Assert-Match -Text $run2.Output -Pattern 'CHANGELOG 不存在' -Because "要说清是 CHANGELOG.md 文件本身缺失"
    }

    Test-Case "负向:CHANGELOG 段落只有标题没有正文必须拒绝" {
        $changelog = @'
# 更新日志

## [1.2.3] - 2026-09-16

## [1.2.2] - 2026-09-01

- 旧内容
'@
        $fx = New-ReleaseFixture -Changelog $changelog
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "没有修复内容的版本不发布"
        Assert-Match -Text $run.Output -Pattern '段是空的' -Because "错误要说清段落为空,而不是找不到"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'
    }

    Test-Case "修复内容优先级:-Notes > -NotesFile > CHANGELOG" {
        $fx = New-ReleaseFixture
        $notesFile = Join-Path $fx.CaseDir 'notes.md'
        Write-TestFile -Path $notesFile -Text "`n来自 NotesFile 的说明`n"

        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3', '-Notes', '行内说明优先', '-NotesFile', $notesFile)
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        $m = Read-JsonFile -Path (Join-Path $fx.ManifestsDir 'v1.2.3.json')
        Assert-Equal -Expected '行内说明优先' -Actual $m['notes'] -Because "-Notes 优先于 -NotesFile 与 CHANGELOG"

        $fx2 = New-ReleaseFixture
        $run2 = Invoke-MakeRelease -Fixture $fx2 -Arguments @('-Version', 'v1.2.3', '-NotesFile', $notesFile)
        Assert-Equal -Expected 0 -Actual $run2.ExitCode -Because "输出: $($run2.Output)"
        $m2 = Read-JsonFile -Path (Join-Path $fx2.ManifestsDir 'v1.2.3.json')
        Assert-Equal -Expected '来自 NotesFile 的说明' -Actual $m2['notes'] -Because "-NotesFile 优先于 CHANGELOG,首尾空白去掉"
    }

    Test-Case "负向:镜像 build-info.app_version 与 -Version 不一致或为空必须拒绝" {
        $fx = New-ReleaseFixture -AppVersion 'v9.9.9'
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "版本错配必须拒绝"
        Assert-Match -Text $run.Output -Pattern '与镜像自报版本不一致' -Because "错误要说清是版本交叉校验失败"
        Assert-Match -Text $run.Output -Pattern 'v9\.9\.9' -Because "错误要给出镜像实际自报的版本"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'

        $fx2 = New-ReleaseFixture -AppVersion ''
        $run2 = Invoke-MakeRelease -Fixture $fx2 -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run2.ExitCode -ne 0) -Because "app_version 为空(快照包)必须拒绝"
        Assert-Match -Text $run2.Output -Pattern '缺 app_version' -Because "错误要指出镜像没有自报版本"
        Assert-NoReleaseSentinel -Fixture $fx2 -Version 'v1.2.3'
    }

    Test-Case "负向:引用脏树制品默认拒绝,-AllowDirty 放行并如实记录 dirty" {
        $fx = New-ReleaseFixture -Dirty $true
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "dirty 制品必须拒绝"
        Assert-Match -Text $run.Output -Pattern '脏工作树' -Because "错误要说清是 dirty 来源"
        Assert-Match -Text $run.Output -Pattern '-AllowDirty' -Because "错误要告诉内测场景怎么放行"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'

        $run2 = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3', '-AllowDirty')
        Assert-Equal -Expected 0 -Actual $run2.ExitCode -Because "显式 -AllowDirty 放行。输出: $($run2.Output)"
        $m = Read-JsonFile -Path (Join-Path $fx.ManifestsDir 'v1.2.3.json')
        Assert-Equal -Expected $true -Actual $m['source']['dirty'] -Because "放行不等于隐瞒,manifest 必须记 dirty=true"
    }

    Test-Case "负向:同一版本重复发布必须拒绝,已发布的 manifest 不被改动" {
        $fx = New-ReleaseFixture
        $first = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-Equal -Expected 0 -Actual $first.ExitCode -Because "首次发布应成功。输出: $($first.Output)"
        $jsonPath = Join-Path $fx.ManifestsDir 'v1.2.3.json'
        $before = (Get-FileHash -LiteralPath $jsonPath -Algorithm SHA256).Hash

        $second = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3', '-Notes', '想偷偷改说明')
        Assert-True -Condition ($second.ExitCode -ne 0) -Because "manifest 不可变"
        Assert-Match -Text $second.Output -Pattern '已存在且不可变' -Because "错误要点明不可变规则"
        Assert-Equal -Expected $before -Actual (Get-FileHash -LiteralPath $jsonPath -Algorithm SHA256).Hash -Because "被拒绝的重发不得改动已发布的 manifest"
    }

    Test-Case "负向:镜像制品被改动过(sha256sums 不符)必须拒绝" {
        $fx = New-ReleaseFixture
        $tar = Get-ChildItem -LiteralPath (Join-Path $fx.VersionDir 'images') -Filter '*.tar' | Select-Object -First 1
        [System.IO.File]::AppendAllText($tar.FullName, 'x')
        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "被篡改的制品不能发布"
        Assert-Match -Text $run.Output -Pattern '镜像制品校验失败' -Because "错误要来自校验和守卫"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'
    }

    Test-Case "负向:digest 文件与镜像清单对不上(缺镜像 / 仓库不符 / 多余条目)必须拒绝" {
        $fx = New-ReleaseFixture
        $nodeRef = $fx.Refs[0]
        $digestsPath = Join-Path $fx.CaseDir 'digests-bad.json'
        $bad = [ordered]@{}
        $bad[$nodeRef] = 'registry.invalid/other/mmorpg-node@sha256:' + ('d' * 64)
        $bad['registry.invalid/test/mmorpg-ghost:v1.2.3-0123456789ab'] = 'registry.invalid/test/mmorpg-ghost@sha256:' + ('e' * 64)
        Write-TestFile -Path $digestsPath -Text ($bad | ConvertTo-Json)

        $run = Invoke-MakeRelease -Fixture $fx -Arguments @('-Version', 'v1.2.3', '-ImageDigestsFile', $digestsPath)
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "digest 对不上必须拒绝"
        Assert-Match -Text $run.Output -Pattern '缺少 digest:registry\.invalid/test/mmorpg-login' -Because "要点名缺 digest 的镜像"
        Assert-Match -Text $run.Output -Pattern '仓库不符' -Because "digest 指向别的仓库要报出来"
        Assert-Match -Text $run.Output -Pattern '不在本次镜像清单里:registry\.invalid/test/mmorpg-ghost' -Because "多余条目说明 digest 文件来自另一次发布"
        Assert-NoReleaseSentinel -Fixture $fx -Version 'v1.2.3'
    }
}
finally {
    if ([System.IO.Directory]::Exists($script:TestRoot)) {
        Remove-Item -LiteralPath $script:TestRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName "make_release contract")
