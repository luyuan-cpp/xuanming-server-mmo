#requires -Version 7
<#
.SYNOPSIS
    发布版本公共函数(release_common.ps1)与 release_preflight.ps1 -ReleaseVersion 制品检查的契约测试。

.DESCRIPTION
    守的是"同一个版本号四处一致 + 部署的就是那份制品"这条发布链:
      - Test-ReleaseVersion        版本号长什么样(唯一定义,镜像脚本 / 预检 / release.yml 都调它)
      - Get-ReleaseImageTag        release / snapshot / 脏树三种 tag,发布版本拒脏树
      - Test-ChangelogReleaseSection  CHANGELOG 段落规则(与 make_release.ps1 同口径)
      - Get-PushedImageDigest      推送后只认同 repo 的 digest(docker 用 MMORPG_DOCKER_COMMAND 桩)
      - Write-ImageDigestRecord    读-合并-写,冲突与损坏 fail-closed
      - release_preflight.ps1 -ReleaseVersion  制品目录 / sha256sums / build-info / manifest / tag 核对

    不依赖真实 docker、网络、git 远端;只读写本测试自建的临时目录,结束时删除。
    预检用例以子进程跑真实脚本,断言的是检查项 ID 与错误文本 —— 预检的配置检查会因为仓库里的
    占位密钥而整体 exit 1,只断退出码等于没测。

.EXAMPLE
    pwsh -File tools/scripts/tests/release_common_version.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
# deploy_capture.ps1 会 dot-source lib/release_common.ps1,并提供 Invoke-ToolScript
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== release_common.ps1 发布版本函数 / release_preflight.ps1 制品检查 契约测试 ==='

$script:TempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-release-version-tests-' + [guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($script:TempRoot) | Out-Null

$script:Commit = '0123456789ab'
$script:OtherCommit = 'ba9876543210'
$script:ShaA = 'a' * 64
$script:ShaB = 'b' * 64
$script:ShaC = 'c' * 64

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Write-TestFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Content)
    [System.IO.Directory]::CreateDirectory((Split-Path -Parent $Path)) | Out-Null
    [System.IO.File]::WriteAllText($Path, $Content, [System.Text.UTF8Encoding]::new($false))
}

function New-CaseDir {
    param([Parameter(Mandatory = $true)][string]$Name)
    $dir = Join-Path $script:TempRoot $Name
    [System.IO.Directory]::CreateDirectory($dir) | Out-Null
    return $dir
}

# docker 桩:进程内被 Get-PushedImageDigest 以 `& $env:MMORPG_DOCKER_COMMAND ...` 调起。
# 期望的 ref、要输出的文本、退出码由环境变量给出;参数形状不符时 exit 97,让"调用约定被改坏"也能红。
$script:DockerStubPath = Join-Path $script:TempRoot 'docker_stub.ps1'
Write-TestFile -Path $script:DockerStubPath -Content @'
$argv = @($args | ForEach-Object { [string]$_ })
if ($argv.Count -lt 3 -or $argv[0] -cne 'image' -or $argv[1] -cne 'inspect' -or ($argv -notcontains '{{json .RepoDigests}}') -or $argv[-1] -cne $env:MMORPG_TEST_STUB_EXPECT_REF) {
    Write-Output ('docker 桩收到的参数不符合约定: ' + ($argv -join ' '))
    exit 97
}
if ($env:MMORPG_TEST_STUB_OUTPUT) { Write-Output $env:MMORPG_TEST_STUB_OUTPUT }
exit ([int]$env:MMORPG_TEST_STUB_EXIT)
'@

function Invoke-WithDockerStub {
    param(
        [Parameter(Mandatory = $true)][string]$ExpectRef,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [int]$ExitCode = 0,
        [Parameter(Mandatory = $true)][scriptblock]$Body
    )

    $names = @('MMORPG_DOCKER_COMMAND', 'MMORPG_TEST_STUB_EXPECT_REF', 'MMORPG_TEST_STUB_OUTPUT', 'MMORPG_TEST_STUB_EXIT')
    $saved = @{}
    foreach ($n in $names) { $saved[$n] = [System.Environment]::GetEnvironmentVariable($n) }
    try {
        [System.Environment]::SetEnvironmentVariable('MMORPG_DOCKER_COMMAND', $script:DockerStubPath)
        [System.Environment]::SetEnvironmentVariable('MMORPG_TEST_STUB_EXPECT_REF', $ExpectRef)
        [System.Environment]::SetEnvironmentVariable('MMORPG_TEST_STUB_OUTPUT', $Output)
        [System.Environment]::SetEnvironmentVariable('MMORPG_TEST_STUB_EXIT', [string]$ExitCode)
        return (& $Body)
    }
    finally {
        foreach ($n in $names) { [System.Environment]::SetEnvironmentVariable($n, $saved[$n]) }
    }
}

# 按接口 B 的格式自己写 sha256sums.txt(不借 New-Sha256Sums),预检与 Test-Sha256Sums 的对接才算被独立验证
function Write-FakeSha256Sums {
    param([Parameter(Mandatory = $true)][string]$Dir)

    $rels = New-Object System.Collections.Generic.List[string]
    foreach ($f in Get-ChildItem -LiteralPath $Dir -Recurse -File -Force) {
        $rel = [System.IO.Path]::GetRelativePath($Dir, $f.FullName).Replace('\', '/')
        if ($rel -ceq 'sha256sums.txt') { continue }
        $rels.Add($rel)
    }
    [string[]]$sorted = $rels.ToArray()
    [System.Array]::Sort($sorted, [System.StringComparer]::Ordinal)
    $sb = New-Object System.Text.StringBuilder
    foreach ($rel in $sorted) {
        $hash = (Get-FileHash -LiteralPath (Join-Path $Dir $rel) -Algorithm SHA256).Hash.ToLowerInvariant()
        [void]$sb.Append($hash).Append('  ').Append($rel).Append("`n")
    }
    [System.IO.File]::WriteAllText((Join-Path $Dir 'sha256sums.txt'), $sb.ToString(), [System.Text.UTF8Encoding]::new($false))
}

# 造一份 publish_images.ps1 形状的 release 轨制品(接口 F),可选带 make_release 的 manifest
function New-FakeReleaseArtifact {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$Commit,
        [bool]$Dirty = $false,
        [switch]$WithManifest
    )

    $dir = Join-Path $Root 'releases' 'images' $Version
    Write-TestFile -Path (Join-Path $dir 'images' 'registry.invalid_test_mmorpg-login_fake.tar') -Content 'fake image tar'
    Write-TestFile -Path (Join-Path $dir 'images-manifest.json') -Content '[]'
    $buildInfo = [ordered]@{
        version     = $Version
        channel     = 'release'
        app_version = $Version
        vcs         = 'git'
        source_rev  = "g$Commit"
        commit      = $Commit
        dirty       = $Dirty
        image_tag   = "$Version-$Commit"
        image_count = 1
    }
    Write-TestFile -Path (Join-Path $dir 'build-info.json') -Content ($buildInfo | ConvertTo-Json -Depth 3)
    Write-FakeSha256Sums -Dir $dir

    if ($WithManifest) {
        Write-TestFile -Path (Join-Path $Root 'releases' 'manifests' "$Version.json") -Content (@{ version = $Version } | ConvertTo-Json)
    }
    return $dir
}

function Invoke-Preflight {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    # dev 档:密钥类降级为 WARN,输出更短;本套只断 release.* 检查项,不断总退出码
    return Invoke-ToolScript -ScriptName 'release_preflight.ps1' -Arguments (@('-ReleaseProfile', 'dev') + $Arguments)
}

try {

    # ─────────────────────────────────────────────────────────────────
    # 1. Test-ReleaseVersion
    # ─────────────────────────────────────────────────────────────────

    Test-Case "Test-ReleaseVersion 正例:v1.2.3 / v0.1.0 / v1.2.3-rc.1 合法,Normalized 去掉 v" {
        $cases = @(
            @{ V = 'v1.2.3'; N = '1.2.3' }
            @{ V = 'v0.1.0'; N = '0.1.0' }
            @{ V = 'v1.2.3-rc.1'; N = '1.2.3-rc.1' }
        )
        foreach ($c in $cases) {
            $r = Test-ReleaseVersion -Version $c.V
            Assert-True -Condition $r.Ok -Because "$($c.V) 应当合法(Reason=$($r.Reason))"
            Assert-Equal -Expected $c.N -Actual $r.Normalized -Because "$($c.V) 的 Normalized"
            Assert-Equal -Expected '' -Actual $r.Reason -Because "$($c.V) 合法时 Reason 为空"
        }
    }

    Test-Case "Test-ReleaseVersion 负例:1.2.3 / v01.2.3 / v1.2 / latest / 空串 / V1.2.3 / 行尾换行 / 全角数字 一律拒绝并说明原因" {
        $cases = @(
            @{ V = '1.2.3'; P = '小写 v' }
            @{ V = 'v01.2.3'; P = '前导 0' }
            @{ V = 'v1.2'; P = '不合法' }
            @{ V = 'latest'; P = '不合法' }
            @{ V = ''; P = '为空' }
            @{ V = 'V1.2.3'; P = '小写 v' }
            @{ V = "v1.2.3`n"; P = '空白' }
            @{ V = "v$([char]0xFF11).2.3"; P = '不合法' }
        )
        foreach ($c in $cases) {
            $r = Test-ReleaseVersion -Version $c.V
            $shown = $c.V -replace "`n", '\n'
            Assert-True -Condition (-not $r.Ok) -Because "'$shown' 必须被拒绝"
            Assert-Equal -Expected '' -Actual $r.Normalized -Because "'$shown' 被拒时 Normalized 为空"
            Assert-Match -Text $r.Reason -Pattern $c.P -Because "'$shown' 的拒绝原因要说清楚"
        }
    }

    # ─────────────────────────────────────────────────────────────────
    # 2. Get-ReleaseImageTag
    # ─────────────────────────────────────────────────────────────────

    Test-Case "Get-ReleaseImageTag release 分支:<vX.Y.Z>-<12位sha>,且能通过 prod 的不可变 tag 判定" {
        $tag = Get-ReleaseImageTag -Version 'v1.2.3' -Commit $script:Commit
        Assert-Equal -Expected 'v1.2.3-0123456789ab' -Actual $tag -Because 'release tag = 版本号-commit'
        $chk = Test-ImmutableImageTag -Tag $tag -RejectDirty
        Assert-True -Condition $chk.Ok -Because "release tag 必须被 prod 发布门禁接受($($chk.Reason))"
    }

    Test-Case "Get-ReleaseImageTag snapshot 分支:干净树 = commit,脏树 = commit-dirty(prod 门禁会拒脏树 tag)" {
        Assert-Equal -Expected $script:Commit -Actual (Get-ReleaseImageTag -Commit $script:Commit) -Because '无版本号干净树 = 12 位 commit'
        Assert-Equal -Expected $script:Commit -Actual (Get-ReleaseImageTag -Version '' -Commit $script:Commit) -Because '空串版本号等同于没给'
        $dirtyTag = Get-ReleaseImageTag -Commit $script:Commit -Dirty
        Assert-Equal -Expected '0123456789ab-dirty' -Actual $dirtyTag -Because '无版本号脏树 = commit-dirty(与 Get-GitReleaseStamp.Tag 口径一致)'
        Assert-True -Condition (-not (Test-ImmutableImageTag -Tag $dirtyTag -RejectDirty).Ok) -Because '脏树 tag 不得通过 prod 门禁'
    }

    Test-Case "负向:Get-ReleaseImageTag 发布版本 + 脏树必须抛,并说明原因" {
        $msg = Get-ThrownMessage { Get-ReleaseImageTag -Version 'v1.2.3' -Commit $script:Commit -Dirty }
        Assert-Match -Text $msg -Pattern '不接受脏工作树' -Because '发布版本不能挂在无法还原的脏树产物上'
    }

    Test-Case "负向:Get-ReleaseImageTag 非法版本号 / 非 12 位小写 commit 必须抛" {
        $msg = Get-ThrownMessage { Get-ReleaseImageTag -Version '1.2.3' -Commit $script:Commit }
        Assert-Match -Text $msg -Pattern '小写 v' -Because '非法版本号不能拼进 tag'
        foreach ($bad in @('0123456789AB', 'abc123', '', '0123456789abc')) {
            $msg = Get-ThrownMessage { Get-ReleaseImageTag -Commit $bad }
            Assert-Match -Text $msg -Pattern '12 位小写十六进制' -Because "commit '$bad' 必须被拒"
        }
    }

    # ─────────────────────────────────────────────────────────────────
    # 3. Test-ChangelogReleaseSection
    # ─────────────────────────────────────────────────────────────────

    $changelogDir = New-CaseDir -Name 'changelog'
    $changelogPath = Join-Path $changelogDir 'CHANGELOG.md'
    Write-TestFile -Path $changelogPath -Content (@(
            '# 更新日志'
            ''
            '## [Unreleased]'
            ''
            '## [1.2.3-rc.1] - 2026-09-10'
            ''
            '- 预发布内容'
            ''
            '## [1.2.3] - 2026-09-16'
            ''
            '### 修复'
            ''
            '- 真正的修复内容'
            ''
            '## [1.2.2] - 2026-09-01'
            ''
            '[1.2.2]: https://example.invalid/compare/v1.2.1...v1.2.2'
            ''
            '## [1.0.0]'
            '- 第一段'
            '## [1.0.0]'
            '- 第二段'
        ) -join "`n")

    Test-Case "Test-ChangelogReleaseSection 正例:精确命中 [1.2.3](不被 [1.2.3-rc.1] 干扰)" {
        $r = Test-ChangelogReleaseSection -ChangelogPath $changelogPath -Version 'v1.2.3'
        Assert-True -Condition $r.Ok -Because "[1.2.3] 段存在且非空($($r.Reason))"
        Assert-Equal -Expected '## [1.2.3]' -Actual $r.Heading -Because '标题里不带 v'
    }

    Test-Case "负向:Test-ChangelogReleaseSection 段缺失 / 只有链接引用 / 重复 / 文件不存在 都必须失败" {
        $cases = @(
            @{ V = 'v1.2.4'; Path = $changelogPath; P = '没有' }
            @{ V = 'v1.2.2'; Path = $changelogPath; P = '是空的' }
            @{ V = 'v1.0.0'; Path = $changelogPath; P = '2 段' }
            @{ V = 'v1.2.3'; Path = (Join-Path $changelogDir 'missing.md'); P = '不存在' }
            @{ V = '1.2.3'; Path = $changelogPath; P = '小写 v' }
        )
        foreach ($c in $cases) {
            $r = Test-ChangelogReleaseSection -ChangelogPath $c.Path -Version $c.V
            Assert-True -Condition (-not $r.Ok) -Because "$($c.V) @ $(Split-Path -Leaf $c.Path) 必须失败"
            Assert-Match -Text $r.Reason -Pattern $c.P -Because "$($c.V) 的失败原因"
        }
    }

    # ─────────────────────────────────────────────────────────────────
    # 4. Write-ImageDigestRecord
    # ─────────────────────────────────────────────────────────────────

    $refLogin = "registry.invalid/test/mmorpg-login:v1.2.3-$($script:Commit)"
    $refGateway = "registry.invalid/test/mmorpg-gateway:v1.2.3-$($script:Commit)"
    $digestLogin = "registry.invalid/test/mmorpg-login@sha256:$($script:ShaA)"
    $digestGateway = "registry.invalid/test/mmorpg-gateway@sha256:$($script:ShaB)"

    Test-Case "Write-ImageDigestRecord 两次写入合并成一个 JSON 对象,父目录自动创建,不留临时文件" {
        $dir = Join-Path (New-CaseDir -Name 'digest-merge') 'nested'
        $path = Join-Path $dir 'image-digests.json'
        Write-ImageDigestRecord -Path $path -ImageRef $refLogin -Digest $digestLogin
        Write-ImageDigestRecord -Path $path -ImageRef $refGateway -Digest $digestGateway

        $data = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable
        Assert-Equal -Expected 2 -Actual $data.Count -Because '两条记录都要在'
        Assert-Equal -Expected $digestLogin -Actual $data[$refLogin] -Because '第一次写入的记录不能被第二次覆盖掉'
        Assert-Equal -Expected $digestGateway -Actual $data[$refGateway] -Because '第二次写入的记录'
        $leftovers = @(Get-ChildItem -LiteralPath $dir -Force | Where-Object { $_.Name -ne 'image-digests.json' })
        Assert-Equal -Expected 0 -Actual $leftovers.Count -Because "tmp-rename 之后目录里只剩记录文件(实际多出:$($leftovers.Name -join ', '))"

        # 同一条记录重复写是幂等的(推送脚本重跑)
        Write-ImageDigestRecord -Path $path -ImageRef $refLogin -Digest $digestLogin
        $again = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable
        Assert-Equal -Expected 2 -Actual $again.Count -Because '重复写同一条记录不增不减'
    }

    Test-Case "负向:Write-ImageDigestRecord 同一 ref 记录不同 digest 必须拒绝,原记录不变" {
        $path = Join-Path (New-CaseDir -Name 'digest-conflict') 'image-digests.json'
        Write-ImageDigestRecord -Path $path -ImageRef $refLogin -Digest $digestLogin
        $before = Get-Content -LiteralPath $path -Raw
        $msg = Get-ThrownMessage { Write-ImageDigestRecord -Path $path -ImageRef $refLogin -Digest "registry.invalid/test/mmorpg-login@sha256:$($script:ShaC)" }
        Assert-Match -Text $msg -Pattern '已记录不同 digest' -Because '同名 tag 换内容是回滚失效的前兆,不能静默覆盖'
        Assert-Equal -Expected $before -Actual (Get-Content -LiteralPath $path -Raw) -Because '被拒时记录文件不得改动'
    }

    Test-Case "负向:Write-ImageDigestRecord 已有文件不是合法 JSON / digest 与 ref 不同 repo / digest 格式错 都必须拒绝" {
        $path = Join-Path (New-CaseDir -Name 'digest-bad') 'image-digests.json'
        Write-TestFile -Path $path -Content 'not json {'
        $msg = Get-ThrownMessage { Write-ImageDigestRecord -Path $path -ImageRef $refLogin -Digest $digestLogin }
        Assert-Match -Text $msg -Pattern '不是合法 JSON' -Because '损坏的记录文件不能被覆盖掉'
        Assert-Equal -Expected 'not json {' -Actual (Get-Content -LiteralPath $path -Raw) -Because '被拒时原内容不变'

        $path2 = Join-Path (New-CaseDir -Name 'digest-bad-2') 'image-digests.json'
        $msg = Get-ThrownMessage { Write-ImageDigestRecord -Path $path2 -ImageRef $refLogin -Digest $digestGateway }
        Assert-Match -Text $msg -Pattern 'repo 不一致' -Because '拿别的 repo 的 digest 记账会让部署拉错镜像'
        $msg = Get-ThrownMessage { Write-ImageDigestRecord -Path $path2 -ImageRef $refLogin -Digest 'registry.invalid/test/mmorpg-login@sha256:1234' }
        Assert-Match -Text $msg -Pattern 'repo@sha256' -Because 'digest 格式必须完整'
        Assert-True -Condition (-not (Test-Path -LiteralPath $path2)) -Because '全部被拒时不应生成记录文件'
    }

    # ─────────────────────────────────────────────────────────────────
    # 5. Get-PushedImageDigest(docker 桩)
    # ─────────────────────────────────────────────────────────────────

    Test-Case "Get-PushedImageDigest 从多 repo 的 RepoDigests 里只取同 repo 的那一个" {
        $out = "[`"registry.invalid/other/mmorpg-login@sha256:$($script:ShaA)`",`"registry.invalid/test/mmorpg-login@sha256:$($script:ShaB)`"]"
        $r = Invoke-WithDockerStub -ExpectRef $refLogin -Output $out -Body { Get-PushedImageDigest -ImageRef $refLogin }
        Assert-True -Condition $r.Ok -Because "同 repo 的 digest 存在时应成功($($r.Reason))"
        Assert-Equal -Expected "registry.invalid/test/mmorpg-login@sha256:$($script:ShaB)" -Actual $r.Digest -Because '只能取与 ImageRef 同 repo 的 digest'
    }

    Test-Case "Get-PushedImageDigest Docker Hub 省略写法(redis == docker.io/library/redis)视为同 repo" {
        $ref = 'docker.io/library/redis:7.2.4'
        $r = Invoke-WithDockerStub -ExpectRef $ref -Output "[`"redis@sha256:$($script:ShaC)`"]" -Body { Get-PushedImageDigest -ImageRef $ref }
        Assert-True -Condition $r.Ok -Because "docker 在 RepoDigests 里把 Docker Hub 写成省略形式($($r.Reason))"
        Assert-Equal -Expected "docker.io/library/redis@sha256:$($script:ShaC)" -Actual $r.Digest -Because 'Digest 沿用调用方给的 repo 写法'
    }

    Test-Case "负向:Get-PushedImageDigest 只有其它 repo 的 digest 时必须拒绝" {
        $out = "[`"registry.invalid/other/mmorpg-login@sha256:$($script:ShaA)`"]"
        $r = Invoke-WithDockerStub -ExpectRef $refLogin -Output $out -Body { Get-PushedImageDigest -ImageRef $refLogin }
        Assert-True -Condition (-not $r.Ok) -Because '别的 repo 的 digest 不能冒充'
        Assert-Equal -Expected '' -Actual $r.Digest -Because '失败时 Digest 为空'
        Assert-Match -Text $r.Reason -Pattern '其它 repo' -Because '原因要点明是 repo 对不上'
    }

    Test-Case "负向:Get-PushedImageDigest 无 digest(未推送)/ docker 失败 / 同 repo 多个 digest / 无 tag 都必须失败" {
        foreach ($empty in @('[]', 'null')) {
            $r = Invoke-WithDockerStub -ExpectRef $refLogin -Output $empty -Body { Get-PushedImageDigest -ImageRef $refLogin }
            Assert-True -Condition (-not $r.Ok) -Because "RepoDigests=$empty 说明从未推送"
            Assert-Match -Text $r.Reason -Pattern '没有 RepoDigests' -Because "RepoDigests=$empty 的失败原因"
        }

        $r = Invoke-WithDockerStub -ExpectRef $refLogin -Output 'Error: No such image' -ExitCode 1 -Body { Get-PushedImageDigest -ImageRef $refLogin }
        Assert-True -Condition (-not $r.Ok) -Because 'docker image inspect 失败'
        Assert-Match -Text $r.Reason -Pattern 'exit=1' -Because '失败原因带退出码'
        Assert-Match -Text $r.Reason -Pattern 'No such image' -Because '失败原因带 docker 的输出'

        $out = "[`"registry.invalid/test/mmorpg-login@sha256:$($script:ShaA)`",`"registry.invalid/test/mmorpg-login@sha256:$($script:ShaB)`"]"
        $r = Invoke-WithDockerStub -ExpectRef $refLogin -Output $out -Body { Get-PushedImageDigest -ImageRef $refLogin }
        Assert-True -Condition (-not $r.Ok) -Because '同 repo 两个 digest 无法判断本次推的是哪个'
        Assert-Match -Text $r.Reason -Pattern '多个 digest' -Because '失败原因'

        $r = Get-PushedImageDigest -ImageRef 'registry.invalid/test/mmorpg-login'
        Assert-True -Condition (-not $r.Ok) -Because '无 tag 的引用会被 docker 隐式补 latest'
        Assert-Match -Text $r.Reason -Pattern '没有 tag' -Because '失败原因'
    }

    # ─────────────────────────────────────────────────────────────────
    # 6. release_preflight.ps1 -ReleaseVersion 制品检查(子进程跑真实脚本)
    # ─────────────────────────────────────────────────────────────────

    Test-Case "预检不给 -ReleaseVersion 时不出现任何 release.* 检查项(既有调用方行为不变)" {
        $run = Invoke-Preflight -Arguments @('-ImageTag', $script:Commit)
        Assert-Match -Text $run.Output -Pattern 'release preflight \(profile=dev\)' -Because '预检必须真的跑完并打印汇总'
        Assert-Match -Text $run.Output -Pattern '[一-龥]' -Because '子进程中文输出必须无损带回(否则后面断错误文本的用例全是假红)'
        Assert-NotMatch -Text $run.Output -Pattern '\] release\.' -Because '不给 -ReleaseVersion 时 H 节整节不跑'
    }

    Test-Case "负向:预检 -ReleaseVersion 非法时 release.version FAIL 并说明要小写 v" {
        $run = Invoke-Preflight -Arguments @('-ReleaseVersion', '1.2.3', '-ImageTag', $script:Commit)
        Assert-True -Condition ($run.ExitCode -ne 0) -Because '存在 FAIL 时退出码非 0'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.version\b' -Because '版本号检查项必须判 FAIL'
        Assert-Match -Text $run.Output -Pattern '小写 v' -Because '错误文本说明原因'
        Assert-NotMatch -Text $run.Output -Pattern '\] release\.artifact\.' -Because '版本号非法时不再做依赖版本号的制品检查'
    }

    Test-Case "负向:预检制品版本目录与 manifest 不存在时各自 FAIL 并点名" {
        $root = New-CaseDir -Name 'preflight-missing'
        $run = Invoke-Preflight -Arguments @('-ReleaseVersion', 'v9.9.9', '-ArtifactRoot', $root, '-ImageTag', "v9.9.9-$($script:Commit)")
        Assert-True -Condition ($run.ExitCode -ne 0) -Because '存在 FAIL 时退出码非 0'
        Assert-Match -Text $run.Output -Pattern '\[PASS\]\s+release\.version\b' -Because '合法版本号'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.artifact\.dir\b' -Because '版本目录不存在必须 FAIL'
        Assert-Match -Text $run.Output -Pattern '版本目录不存在' -Because '错误文本'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.manifest\b' -Because 'manifest 不存在必须 FAIL'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.imagetag\.commit\b' -Because 'build-info 不可读时无法核对 tag,fail-safe'
    }

    Test-Case "预检完整制品:sha256sums / app_version / dirty / manifest / tag commit 全部 PASS" {
        $root = New-CaseDir -Name 'preflight-ok'
        New-FakeReleaseArtifact -Root $root -Version 'v9.9.9' -Commit $script:Commit -WithManifest | Out-Null
        $run = Invoke-Preflight -Arguments @('-ReleaseVersion', 'v9.9.9', '-ArtifactRoot', $root, '-ImageTag', "v9.9.9-$($script:Commit)")
        foreach ($id in @('release.version', 'release.artifact.dir', 'release.artifact.sha256sums', 'release.buildinfo.version', 'release.buildinfo.clean', 'release.manifest', 'release.imagetag.commit')) {
            Assert-Match -Text $run.Output -Pattern ('\[PASS\]\s+' + [regex]::Escape($id) + '(\s|$)') -Because "$id 应当 PASS"
        }
    }

    Test-Case "负向:预检篡改制品后 sha256sums FAIL;tag 版本段与 -ReleaseVersion 不符 FAIL" {
        $root = New-CaseDir -Name 'preflight-tampered'
        $dir = New-FakeReleaseArtifact -Root $root -Version 'v9.9.9' -Commit $script:Commit -WithManifest
        Write-TestFile -Path (Join-Path $dir 'images' 'registry.invalid_test_mmorpg-login_fake.tar') -Content 'tampered image tar'
        $run = Invoke-Preflight -Arguments @('-ReleaseVersion', 'v9.9.9', '-ArtifactRoot', $root, '-ImageTag', "v9.9.8-$($script:Commit)")
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.artifact\.sha256sums\b' -Because '制品被改过必须 FAIL'
        Assert-Match -Text $run.Output -Pattern '哈希不符' -Because '错误文本来自 Test-Sha256Sums'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.imagetag\.commit\b' -Because 'tag 不是这个版本号'
        Assert-Match -Text $run.Output -Pattern '必须形如 v9\.9\.9-<12 位小写 commit>' -Because '错误文本说明期望形状'
    }

    Test-Case "负向:预检 build-info.dirty=true 与 tag commit 不等于 build-info.commit 各自 FAIL" {
        $root = New-CaseDir -Name 'preflight-dirty'
        New-FakeReleaseArtifact -Root $root -Version 'v9.9.9' -Commit $script:Commit -Dirty $true -WithManifest | Out-Null
        $run = Invoke-Preflight -Arguments @('-ReleaseVersion', 'v9.9.9', '-ArtifactRoot', $root, '-ImageTag', "v9.9.9-$($script:OtherCommit)")
        Assert-Match -Text $run.Output -Pattern '\[PASS\]\s+release\.artifact\.sha256sums\b' -Because '制品本身完整'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.buildinfo\.clean\b' -Because '脏树产物不能发布'
        Assert-Match -Text $run.Output -Pattern '\[FAIL\]\s+release\.imagetag\.commit\b' -Because 'tag 指向别的提交'
        Assert-Match -Text $run.Output -Pattern '要部署的镜像不是这份制品' -Because '错误文本'
    }
}
finally {
    if ([System.IO.Directory]::Exists($script:TempRoot)) {
        Remove-Item -LiteralPath $script:TempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'release_common version / release_preflight artifacts')
