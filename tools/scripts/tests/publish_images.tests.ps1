#requires -Version 7
<#
.SYNOPSIS
    publish_images.ps1 的契约测试:脏树/版本号守卫、镜像身份核对、离线包布局、不可变与幂等重跑。

.DESCRIPTION
    隔离方式(不依赖真实 docker、网络、git 远端):
      - 仓库根:每个用例在临时目录 git init 一个小仓库(带 generated/tables、可选 bin/symbols),
        作为 -RepoRoot,拿到真实的 git 版本戳。git 用临时 GIT_CONFIG_GLOBAL + GIT_CONFIG_NOSYSTEM,
        不受运行机器全局配置(身份、换行转换、钩子路径等)影响。
      - 镜像脚本:-ImageScriptOverrides 换成桩,桩把每次调用记进 calls.log,list-refs 按约定输出镜像引用。
      - docker:-DockerCommand 指向 pwsh 桩,image inspect 读 docker_state.json,save 用 System.Formats.Tar
        现造一个只含 manifest.json 与假 blob 的归档。
    -ImageScriptOverrides 是 hashtable,`pwsh -File` 的命令行传不了,所以经一个临时驱动脚本
    从 JSON 读参数再进程内调用被测脚本;被测脚本本身仍跑在独立子进程里。

    负向用例一律断言**错误文本**,不只断退出码。

.EXAMPLE
    pwsh -File tools/scripts/tests/publish_images.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. (Join-Path (Get-ToolsScriptsDir) 'lib' 'artifacts_lib.ps1')

Write-Host ""
Write-Host "=== publish_images.ps1 契约测试 ==="

$PublishScript = Join-Path (Get-ToolsScriptsDir) 'publish_images.ps1'
$script:TestRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("publish-images-tests-" + [guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($script:TestRoot) | Out-Null
$script:CaseSeq = 0
$script:RunSeq = 0

$TestRegistry = 'registry.invalid/test'

function Write-TestFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $parent = Split-Path -Parent $Path
    if (-not [System.IO.Directory]::Exists($parent)) { [System.IO.Directory]::CreateDirectory($parent) | Out-Null }
    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

# 压平子进程输出再断言:throw 走 ConciseView 时消息里的换行会被压成空格、再按终端宽度在空白处
# 折行并加 "     | " 前缀,宽度随机器变化。断言模式因此只用单个空格,不依赖换行。
function ConvertTo-FlatText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return (($Text -replace '\s*\r?\n\s*(\|\s?)?', ' ') -replace ' {2,}', ' ')
}

# ─────────────────────────────────────────────────────────────────
# 桩与驱动
# ─────────────────────────────────────────────────────────────────

# git 隔离配置:只给临时仓库提交用的身份,其它一律默认值
$script:GitConfigPath = Join-Path $script:TestRoot 'gitconfig'
Write-TestFile -Path $script:GitConfigPath -Text "[user]`n`tname = publish-images-test`n`temail = publish-images-test@example.invalid`n[init]`n`tdefaultBranch = main`n[core]`n`tautocrlf = false`n"

$DriverPath = Join-Path $script:TestRoot 'run_publish.ps1'
Write-TestFile -Path $DriverPath -Text @'
#requires -Version 7
param([Parameter(Mandatory = $true)][string]$SpecPath)
$ErrorActionPreference = 'Stop'
$spec = Get-Content -LiteralPath $SpecPath -Raw | ConvertFrom-Json -AsHashtable
$params = @{}
foreach ($key in $spec['Params'].Keys) { $params[$key] = $spec['Params'][$key] }
& $spec['Script'] @params
exit $LASTEXITCODE
'@

# 镜像脚本桩:__FAMILY__ / __NAMES__ / __CALL_LOG__ 由 New-PublishFixture 替换
$ImageScriptStubTemplate = @'
#requires -Version 7
param(
    [string]$Command = '',
    [string]$Tag = '',
    [string]$ImageTag = '',
    [string]$Version = '',
    [string]$Registry = '',
    [string]$ImageRepository = '',
    [Parameter(ValueFromRemainingArguments = $true)][object[]]$Rest
)
$family = '__FAMILY__'
$names = @(__NAMES__)
$tagValue = if ($Tag) { $Tag } else { $ImageTag }
[System.IO.File]::AppendAllText('__CALL_LOG__', "family=$family command=$Command tag=$tagValue version=$Version registry=$Registry repository=$ImageRepository`n")

if ($Command -eq 'list-refs') {
    if ($env:PUBLISH_TEST_LISTREFS_NOISE -eq $family) { Write-Output "Image tag resolved: $tagValue (profile=dev)" }
    if ($family -eq 'cpp') {
        $repo = if ($ImageRepository) { $ImageRepository } else { 'registry.invalid/test/' + $names[0] }
        Write-Output "${repo}:$tagValue"
    }
    else {
        $reg = if ($Registry) { $Registry } else { 'registry.invalid/test' }
        foreach ($n in $names) { Write-Output "$reg/${n}:$tagValue" }
    }
    exit 0
}
if ($env:PUBLISH_TEST_FAIL_COMMAND -eq "$family/$Command") {
    Write-Output "stub: $family $Command 模拟失败"
    exit 3
}
exit 0
'@

$DockerStubText = @'
#requires -Version 7
$ErrorActionPreference = 'Stop'
$state = Get-Content -LiteralPath $env:PUBLISH_TEST_DOCKER_STATE -Raw | ConvertFrom-Json -AsHashtable
$images = $state['images']
$a = @($args | ForEach-Object { [string]$_ })

if ($a.Count -eq 3 -and $a[0] -eq 'image' -and $a[1] -eq 'inspect') {
    $ref = $a[2]
    if (-not $images.Contains($ref)) { [Console]::Error.WriteLine("Error response from daemon: No such image: $ref"); exit 1 }
    $img = $images[$ref]
    $labels = [ordered]@{ 'org.opencontainers.image.title' = 'stub' }
    if ($img['revision']) { $labels['org.opencontainers.image.revision'] = $img['revision'] }
    $doc = [ordered]@{ Id = $img['id']; RepoTags = @($ref); Config = [ordered]@{ Labels = $labels } }
    Write-Output (ConvertTo-Json -InputObject @($doc) -Depth 6)
    exit 0
}

if ($a.Count -eq 4 -and $a[0] -eq 'save' -and $a[1] -eq '-o') {
    $out = $a[2]
    $ref = $a[3]
    if (-not $images.Contains($ref)) { [Console]::Error.WriteLine("Error response from daemon: No such image: $ref"); exit 1 }
    $img = $images[$ref]
    $tags = if ($img.Contains('save_tags')) { @($img['save_tags']) } else { @($ref) }
    $work = Join-Path ([System.IO.Path]::GetTempPath()) ('docker-stub-' + [guid]::NewGuid().ToString('N'))
    try {
        $blobs = Join-Path $work 'blobs' 'sha256'
        [System.IO.Directory]::CreateDirectory($blobs) | Out-Null
        $idHex = ([string]$img['id']) -replace '^sha256:', ''
        [System.IO.File]::WriteAllText((Join-Path $blobs $idHex), "config of $ref")
        [System.IO.File]::WriteAllText((Join-Path $blobs 'layer0'), "layer of $ref")
        $manifest = @([ordered]@{ Config = "blobs/sha256/$idHex"; RepoTags = $tags; Layers = @('blobs/sha256/layer0') })
        [System.IO.File]::WriteAllText((Join-Path $work 'manifest.json'), (ConvertTo-Json -InputObject $manifest -Depth 5))
        try { Add-Type -AssemblyName System.Formats.Tar -ErrorAction Stop } catch { }
        if ($null -ne ('System.Formats.Tar.TarFile' -as [type])) {
            [System.Formats.Tar.TarFile]::CreateFromDirectory($work, $out, $false)
        }
        else {
            & tar -cf $out -C $work .
            if ($LASTEXITCODE -ne 0) { exit 1 }
        }
    }
    finally {
        Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
    }
    exit 0
}

[Console]::Error.WriteLine("docker stub: 未支持的调用:$($a -join ' ')")
exit 2
'@

function Invoke-FixtureGit {
    param(
        [Parameter(Mandatory = $true)][string]$Repo,
        [Parameter(Mandatory = $true)][string[]]$GitArgs
    )
    $out = & git -C $Repo @GitArgs 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) { throw "夹具 git $($GitArgs -join ' ') 失败:$out" }
    return $out.Trim()
}

<#
.SYNOPSIS
    造一个用例目录:临时 git 仓库 + 镜像脚本桩 + docker 桩 + 空的 docker 状态。
#>
function New-PublishFixture {
    param([switch]$WithSymbols)

    $script:CaseSeq++
    $caseDir = Join-Path $script:TestRoot ("case-{0:d2}" -f $script:CaseSeq)
    $repo = Join-Path $caseDir 'repo'
    $stubs = Join-Path $caseDir 'stubs'
    [System.IO.Directory]::CreateDirectory($repo) | Out-Null
    [System.IO.Directory]::CreateDirectory($stubs) | Out-Null

    Write-TestFile -Path (Join-Path $repo 'generated' 'tables' 'Item.json') -Text '{"rows":[1]}'
    Write-TestFile -Path (Join-Path $repo 'generated' 'tables' 'Skill.json') -Text '{"rows":[2]}'
    if ($WithSymbols) {
        Write-TestFile -Path (Join-Path $repo 'bin' 'symbols' 'gate.debug') -Text 'gate debug symbols'
        Write-TestFile -Path (Join-Path $repo 'bin' 'symbols' 'scene.debug') -Text 'scene debug symbols'
    }
    Invoke-FixtureGit -Repo $repo -GitArgs @('init', '-q') | Out-Null
    Invoke-FixtureGit -Repo $repo -GitArgs @('add', '-A') | Out-Null
    Invoke-FixtureGit -Repo $repo -GitArgs @('commit', '-q', '-m', 'fixture') | Out-Null
    $commit = Invoke-FixtureGit -Repo $repo -GitArgs @('rev-parse', '--short=12', 'HEAD')

    $callLog = Join-Path $stubs 'calls.log'
    $names = @{ cpp = @('mmorpg-node'); go = @('mmorpg-login', 'mmorpg-db'); java = @('mmorpg-gateway') }
    $overrides = @{}
    foreach ($family in @('cpp', 'go', 'java')) {
        $stubPath = Join-Path $stubs "${family}_image_stub.ps1"
        $text = $ImageScriptStubTemplate.Replace('__FAMILY__', $family)
        $text = $text.Replace('__NAMES__', (($names[$family] | ForEach-Object { "'$_'" }) -join ', '))
        $text = $text.Replace('__CALL_LOG__', $callLog.Replace("'", "''"))
        Write-TestFile -Path $stubPath -Text $text
        $overrides[$family] = $stubPath
    }
    $dockerStub = Join-Path $stubs 'docker_stub.ps1'
    Write-TestFile -Path $dockerStub -Text $DockerStubText
    $dockerState = Join-Path $stubs 'docker_state.json'
    Write-TestFile -Path $dockerState -Text '{ "images": {} }'

    return @{
        CaseDir     = $caseDir
        Repo        = $repo
        Root        = (Join-Path $caseDir 'artifacts')
        Commit      = $commit
        Overrides   = $overrides
        DockerStub  = $dockerStub
        DockerState = $dockerState
        CallLog     = $callLog
    }
}

# 某 family 在给定 tag 下应产出的镜像引用(与桩的 list-refs 同一口径)。
# 本文件的列表型辅助函数一律直接输出元素、由调用方用 @() 收集:
# 若用 "return , $arr",调用方再套 @() 会得到"数组里套数组"。
function Get-FixtureRefs {
    param([Parameter(Mandatory = $true)][string]$Tag, [string[]]$Families = @('cpp', 'go', 'java'))
    if ($Families -contains 'cpp') { "$TestRegistry/mmorpg-node:$Tag" }
    if ($Families -contains 'go') { "$TestRegistry/mmorpg-login:$Tag"; "$TestRegistry/mmorpg-db:$Tag" }
    if ($Families -contains 'java') { "$TestRegistry/mmorpg-gateway:$Tag" }
}

function Get-FakeImageId {
    param([Parameter(Mandatory = $true)][string]$Ref)
    $bytes = [System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($Ref))
    return 'sha256:' + [System.Convert]::ToHexString($bytes).ToLowerInvariant()
}

<#
.SYNOPSIS
    写 docker 桩状态:给定引用全部"已构建",revision = 夹具提交;-Mutate 可改单个镜像。
#>
function Set-DockerImages {
    param(
        [Parameter(Mandatory = $true)][hashtable]$Fixture,
        [Parameter(Mandatory = $true)][string[]]$Refs,
        [scriptblock]$Mutate = $null
    )
    $images = [ordered]@{}
    foreach ($ref in $Refs) {
        $images[$ref] = [ordered]@{ id = (Get-FakeImageId -Ref $ref); revision = $Fixture.Commit }
    }
    if ($null -ne $Mutate) { & $Mutate $images }
    Write-TestFile -Path $Fixture.DockerState -Text (ConvertTo-Json -InputObject ([ordered]@{ images = $images }) -Depth 6)
}

function Invoke-Publish {
    param(
        [Parameter(Mandatory = $true)][hashtable]$Fixture,
        [hashtable]$Params = @{},
        [hashtable]$Env = @{}
    )
    $all = @{
        RepoRoot             = $Fixture.Repo
        ArtifactRoot         = $Fixture.Root
        DockerCommand        = $Fixture.DockerStub
        ImageScriptOverrides = $Fixture.Overrides
    }
    foreach ($k in $Params.Keys) { $all[$k] = $Params[$k] }

    $script:RunSeq++
    $specPath = Join-Path $Fixture.CaseDir ("run-{0:d2}.json" -f $script:RunSeq)
    Write-TestFile -Path $specPath -Text (ConvertTo-Json -InputObject @{ Script = $PublishScript; Params = $all } -Depth 6)

    # 宿主上可能设置的发布相关环境变量一律清掉,用例只认显式参数
    $envAll = @{
        PUBLISH_TEST_DOCKER_STATE     = $Fixture.DockerState
        PUBLISH_TEST_FAIL_COMMAND     = ''
        PUBLISH_TEST_LISTREFS_NOISE   = ''
        MMORPG_RELEASE_VERSION        = ''
        MMORPG_ARTIFACT_ROOT          = ''
        MMORPG_DOCKER_COMMAND         = ''
    }
    foreach ($k in $Env.Keys) { $envAll[$k] = $Env[$k] }

    $run = Invoke-CapturedPwsh -ScriptPath $DriverPath -Arguments @($specPath) -Env $envAll
    $run.Output = ConvertTo-FlatText -Text $run.Output
    return $run
}

function Get-ImagesRoot {
    param([Parameter(Mandatory = $true)][hashtable]$Fixture, [Parameter(Mandatory = $true)][ValidateSet('snapshots', 'releases')][string]$Channel)
    return (Join-Path $Fixture.Root $Channel 'images')
}

# 版本目录与 staging 残留:失败的发布既不能上线半成品,也不能留下 .tmp-* 垃圾
function Get-PublishedEntries {
    param([Parameter(Mandatory = $true)][string]$ImagesRoot)
    if (-not [System.IO.Directory]::Exists($ImagesRoot)) { return }
    Get-ChildItem -LiteralPath $ImagesRoot -Directory -Force | ForEach-Object { $_.Name }
}

function Read-JsonFile {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable)
}

function Get-CallSequence {
    param([Parameter(Mandatory = $true)][hashtable]$Fixture)
    if (-not [System.IO.File]::Exists($Fixture.CallLog)) { return }
    [System.IO.File]::ReadAllLines($Fixture.CallLog) | Where-Object { $_ } | ForEach-Object {
        if ($_ -match '^family=(\S+) command=(\S+) tag=(\S*) version=(\S*)') {
            [pscustomobject]@{ Family = $Matches[1]; Command = $Matches[2]; Tag = $Matches[3]; Version = $Matches[4] }
        }
    }
}

# ─────────────────────────────────────────────────────────────────
# 用例
# ─────────────────────────────────────────────────────────────────

$savedGitEnv = @{
    GIT_CONFIG_GLOBAL   = $env:GIT_CONFIG_GLOBAL
    GIT_CONFIG_NOSYSTEM = $env:GIT_CONFIG_NOSYSTEM
}
$env:GIT_CONFIG_GLOBAL = $script:GitConfigPath
$env:GIT_CONFIG_NOSYSTEM = '1'

try {

    Test-Case "负向:脏工作树且未加 -AllowDirty 必须拒绝,且在任何构建/导出之前" {
        $fx = New-PublishFixture
        Write-TestFile -Path (Join-Path $fx.Repo 'uncommitted.txt') -Text 'dirty'
        Set-DockerImages -Fixture $fx -Refs (Get-FixtureRefs -Tag "$($fx.Commit)-dirty")

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "脏树快照无法追溯到确定提交"
        Assert-Match -Text $run.Output -Pattern '未提交改动' -Because "错误要说清是工作树脏"
        Assert-Match -Text $run.Output -Pattern '-AllowDirty' -Because "错误要告诉本机联调怎么放行"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel snapshots)).Count -Because "拒绝时不得产生版本目录或 staging"
        Assert-Equal -Expected 0 -Actual @(Get-CallSequence -Fixture $fx).Count -Because "脏树检查必须先于调用任何镜像脚本"
    }

    Test-Case "负向:release 轨脏树即使加 -AllowDirty 也必须拒绝" {
        $fx = New-PublishFixture
        Write-TestFile -Path (Join-Path $fx.Repo 'uncommitted.txt') -Text 'dirty'

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; AllowDirty = $true; Version = 'v1.2.3' }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "发布版本必须能追溯到确定提交"
        Assert-Match -Text $run.Output -Pattern 'release 轨拒绝发布 v1\.2\.3' -Because "错误要点明是 release 轨规则"
        Assert-Match -Text $run.Output -Pattern '-AllowDirty 对 release 轨无效' -Because "错误要说清 -AllowDirty 在这里不起作用"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel releases)).Count -Because "拒绝时不得产生版本目录"
    }

    Test-Case "负向:release 版本号非法(缺 v / 大写 V / 两段)必须拒绝" {
        $fx = New-PublishFixture
        foreach ($bad in @('1.2.3', 'V1.2.3', 'v1.2')) {
            $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Version = $bad }
            Assert-True -Condition ($run.ExitCode -ne 0) -Because "版本号 '$bad' 必须被拒绝"
            Assert-Match -Text $run.Output -Pattern '发布版本号非法' -Because "'$bad' 的失败必须来自版本号校验"
        }
        Assert-Equal -Expected 0 -Actual @(Get-CallSequence -Fixture $fx).Count -Because "版本号校验必须先于调用任何镜像脚本"
    }

    Test-Case "负向:清单里的镜像本地不存在必须拒绝并点名" {
        $fx = New-PublishFixture
        $refs = @(Get-FixtureRefs -Tag $fx.Commit -Families @('go'))
        Set-DockerImages -Fixture $fx -Refs @($refs | Where-Object { $_ -notmatch 'mmorpg-db' })

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = @('go') }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "缺镜像不能出包"
        Assert-Match -Text $run.Output -Pattern '本地不存在' -Because "错误要说清是镜像缺失"
        Assert-Match -Text $run.Output -Pattern ([regex]::Escape("$TestRegistry/mmorpg-db:$($fx.Commit)")) -Because "要点名缺的是哪个镜像"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel snapshots)).Count -Because "拒绝时不得产生版本目录或 staging"
    }

    Test-Case "负向:镜像 revision label 与当前提交不一致(旧镜像)必须拒绝" {
        $fx = New-PublishFixture
        $refs = @(Get-FixtureRefs -Tag $fx.Commit -Families @('go', 'java'))
        $gateway = "$TestRegistry/mmorpg-gateway:$($fx.Commit)"
        Set-DockerImages -Fixture $fx -Refs $refs -Mutate { param($images) $images[$gateway]['revision'] = 'ffffffffffff' }

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = 'go,java' }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "别的提交构建的镜像不能打进本版本"
        Assert-Match -Text $run.Output -Pattern 'org\.opencontainers\.image\.revision 与当前提交' -Because "错误要说清是 revision 不符"
        Assert-Match -Text $run.Output -Pattern "mmorpg-gateway:$($fx.Commit)\(revision='ffffffffffff'\)" -Because "要点名镜像并给出实际 revision"
        Assert-Match -Text $run.Output -Pattern '去掉 -SkipBuild' -Because "-SkipBuild 场景要提示重建"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel snapshots)).Count -Because "拒绝时不得产生版本目录"
    }

    Test-Case "负向:docker save 归档里的 RepoTags 与镜像不符必须拒绝,并清掉 staging" {
        $fx = New-PublishFixture
        $refs = @(Get-FixtureRefs -Tag $fx.Commit -Families @('go', 'java'))
        $login = "$TestRegistry/mmorpg-login:$($fx.Commit)"
        Set-DockerImages -Fixture $fx -Refs $refs -Mutate { param($images) $images[$login]['save_tags'] = @("$TestRegistry/mmorpg-other:$($fx.Commit)") }

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = @('go', 'java') }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "归档内容与清单不符不能上线"
        Assert-Match -Text $run.Output -Pattern '归档内 RepoTags 与期望不一致' -Because "错误要来自归档自证检查"
        Assert-Match -Text $run.Output -Pattern 'mmorpg-other' -Because "错误要给出归档里实际的 tag"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel snapshots)).Count -Because "失败后版本目录与 .tmp-* staging 都不得残留"
    }

    Test-Case "负向:镜像脚本 list-refs 混入日志行必须拒绝(接口约定只输出镜像引用)" {
        $fx = New-PublishFixture
        Set-DockerImages -Fixture $fx -Refs (Get-FixtureRefs -Tag $fx.Commit -Families @('cpp'))
        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = @('cpp') } -Env @{ PUBLISH_TEST_LISTREFS_NOISE = 'cpp' }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "清单混入非引用内容时不能猜"
        Assert-Match -Text $run.Output -Pattern '输出了非镜像引用的内容' -Because "错误要指出违反 list-refs 约定"
        Assert-Match -Text $run.Output -Pattern 'Image tag resolved' -Because "错误要带出混进来的那一行"
    }

    # 以下三条共用一个夹具:成功发布 → 重发被拒 → -SkipIfExists 幂等
    $script:SnapshotFixture = $null

    Test-Case "成功:快照轨全量发布,布局 / 清单 / build-info / 符号 / sha256sums / latest 指针正确" {
        $fx = New-PublishFixture -WithSymbols
        $script:SnapshotFixture = $fx
        $refs = @(Get-FixtureRefs -Tag $fx.Commit)
        Set-DockerImages -Fixture $fx -Refs $refs

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true }
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "合法输入应当发布成功。输出: $($run.Output)"

        $versionName = "g$($fx.Commit)"
        $imagesRoot = Get-ImagesRoot -Fixture $fx -Channel snapshots
        $final = Join-Path $imagesRoot $versionName
        Assert-True -Condition ([System.IO.Directory]::Exists($final)) -Because "版本目录必须是 snapshots/images/g<sha12>"
        $published = @(Get-PublishedEntries -ImagesRoot $imagesRoot)
        Assert-Equal -Expected $versionName -Actual ($published -join ',') -Because "images 下只能有这个版本目录,不得残留 .tmp-* staging"

        # 每个镜像一个 tar,文件名 = ref 中 / 与 : 换成 _
        $expectedFiles = @($refs | ForEach-Object { ($_ -replace '[/:]', '_') + '.tar' } | Sort-Object)
        $actualFiles = @(Get-ChildItem -LiteralPath (Join-Path $final 'images') -File | ForEach-Object { $_.Name } | Sort-Object)
        Assert-Equal -Expected ($expectedFiles -join ',') -Actual ($actualFiles -join ',') -Because "每个镜像单独一个 tar"

        $manifest = @(Read-JsonFile -Path (Join-Path $final 'images-manifest.json'))
        Assert-Equal -Expected ($refs -join ',') -Actual (($manifest | ForEach-Object { $_['ref'] }) -join ',') -Because "清单按 cpp → go → java、脚本输出顺序排列"
        Assert-Equal -Expected 'cpp,go,go,java' -Actual (($manifest | ForEach-Object { $_['family'] }) -join ',') -Because "family 字段"
        Assert-Equal -Expected 'node,login,db,gateway' -Actual (($manifest | ForEach-Object { $_['service'] }) -join ',') -Because "service 字段去掉 mmorpg- 前缀"
        foreach ($entry in $manifest) {
            Assert-Equal -Expected (Get-FakeImageId -Ref $entry['ref']) -Actual $entry['image_id'] -Because "image_id 取自 docker image inspect"
            Assert-Equal -Expected $fx.Commit -Actual $entry['revision'] -Because "revision 取自镜像 label"
            Assert-Equal -Expected ("images/" + ($entry['ref'] -replace '[/:]', '_') + '.tar') -Actual $entry['file'] -Because "file 是相对版本目录、'/' 分隔的路径"
        }

        $info = Read-JsonFile -Path (Join-Path $final 'build-info.json')
        $tables = Get-TablesDigest -TablesDir (Join-Path $fx.Repo 'generated' 'tables')
        Assert-Equal -Expected $versionName -Actual $info['version'] -Because "build-info.version"
        Assert-Equal -Expected 'snapshot' -Actual $info['channel'] -Because "build-info.channel"
        Assert-Equal -Expected '' -Actual $info['app_version'] -Because "快照轨 app_version 为空,make_release 据此拒绝拿快照发版"
        Assert-Equal -Expected 'git' -Actual $info['vcs'] -Because "build-info.vcs"
        Assert-Equal -Expected "g$($fx.Commit)" -Actual $info['source_rev'] -Because "build-info.source_rev"
        Assert-Equal -Expected $fx.Commit -Actual $info['commit'] -Because "build-info.commit"
        Assert-Equal -Expected $false -Actual $info['dirty'] -Because "build-info.dirty"
        Assert-Equal -Expected $fx.Commit -Actual $info['image_tag'] -Because "快照轨镜像 tag = 12 位 commit"
        Assert-Equal -Expected 'cpp,go,java' -Actual (@($info['families']) -join ',') -Because "build-info.families"
        Assert-Equal -Expected 4 -Actual $info['image_count'] -Because "build-info.image_count"
        Assert-Equal -Expected $tables.Sha256 -Actual $info['tables_sha256'] -Because "tables_sha256 必须是 <RepoRoot>/generated/tables 的摘要"
        Assert-Equal -Expected 2 -Actual $info['tables_file_count'] -Because "tables_file_count"
        foreach ($field in @('published_at', 'machine', 'publisher')) {
            Assert-True -Condition ($info.Contains($field) -and $null -ne $info[$field]) -Because "build-info 必须有 $field"
        }

        foreach ($sym in @('gate.debug', 'scene.debug')) {
            $p = Join-Path $final 'symbols' $sym
            Assert-True -Condition ([System.IO.File]::Exists($p)) -Because "含 cpp 且 bin/symbols 有 *.debug 时默认归档符号($sym)"
        }

        # sha256sums.txt 独立核对:覆盖版本目录内全部文件(自身除外),哈希逐个相符
        $sumsPath = Join-Path $final 'sha256sums.txt'
        $listed = @{}
        foreach ($line in [System.IO.File]::ReadAllLines($sumsPath)) {
            if (-not $line) { continue }
            Assert-Match -Text $line -Pattern '^[0-9a-f]{64}  [^\\]+$' -Because "行格式 '<64 位小写 hex>  <'/' 分隔相对路径>'"
            $listed[$line.Substring(66)] = $line.Substring(0, 64)
        }
        $allFiles = @(Get-ChildItem -LiteralPath $final -Recurse -File -Force | ForEach-Object {
                [System.IO.Path]::GetRelativePath($final, $_.FullName).Replace('\', '/')
            } | Where-Object { $_ -ne 'sha256sums.txt' } | Sort-Object)
        Assert-Equal -Expected ($allFiles -join ',') -Actual (@($listed.Keys | Sort-Object) -join ',') -Because "sha256sums.txt 必须恰好覆盖全部文件"
        foreach ($rel in $allFiles) {
            $actual = (Get-FileHash -LiteralPath (Join-Path $final $rel) -Algorithm SHA256).Hash.ToLowerInvariant()
            Assert-Equal -Expected $actual -Actual $listed[$rel] -Because "哈希必须与文件一致($rel)"
        }

        $latest = Read-JsonFile -Path (Join-Path $imagesRoot 'latest.json')
        Assert-Equal -Expected $versionName -Actual $latest['version'] -Because "latest.json 指向刚发布的版本"
        Assert-Equal -Expected 'snapshot' -Actual $latest['channel'] -Because "latest.json.channel"
    }

    Test-Case "负向:同一提交再次发布必须被不可变规则拒绝,已发布内容不变" {
        $fx = $script:SnapshotFixture
        Assert-True -Condition ($null -ne $fx) -Because "依赖上一条成功发布用例"
        $sumsPath = Join-Path (Get-ImagesRoot -Fixture $fx -Channel snapshots) "g$($fx.Commit)" 'sha256sums.txt'
        $before = (Get-FileHash -LiteralPath $sumsPath -Algorithm SHA256).Hash

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "版本目录不可变"
        Assert-Match -Text $run.Output -Pattern '版本已发布且不可覆盖' -Because "错误要点明不可变规则"
        Assert-Equal -Expected $before -Actual (Get-FileHash -LiteralPath $sumsPath -Algorithm SHA256).Hash -Because "被拒绝的重发不得改动已发布内容"
    }

    Test-Case "-SkipIfExists:同一提交已发布时 exit 0 且不再调用镜像脚本" {
        $fx = $script:SnapshotFixture
        Assert-True -Condition ($null -ne $fx) -Because "依赖上一条成功发布用例"
        $callsBefore = @(Get-CallSequence -Fixture $fx).Count

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; SkipIfExists = $true }
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "CI 幂等重跑必须静默成功。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern '版本已发布,跳过' -Because "要打印跳过原因"
        Assert-Equal -Expected $callsBefore -Actual @(Get-CallSequence -Fixture $fx).Count -Because "已发布时先查先退,不白跑 list-refs / 构建"
    }

    Test-Case "成功:release 轨发布,版本号注入镜像脚本,目录与 tag 按版本号命名" {
        $fx = New-PublishFixture -WithSymbols
        $script:ReleaseFixture = $fx
        $tag = "v1.2.3-$($fx.Commit)"
        Set-DockerImages -Fixture $fx -Refs (Get-FixtureRefs -Tag $tag -Families @('go'))

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = @('go'); Version = 'v1.2.3' }
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"

        $imagesRoot = Get-ImagesRoot -Fixture $fx -Channel releases
        $final = Join-Path $imagesRoot 'v1.2.3'
        Assert-True -Condition ([System.IO.Directory]::Exists($final)) -Because "release 轨目录名 = 版本号"
        $info = Read-JsonFile -Path (Join-Path $final 'build-info.json')
        Assert-Equal -Expected 'release' -Actual $info['channel'] -Because "build-info.channel"
        Assert-Equal -Expected 'v1.2.3' -Actual $info['app_version'] -Because "app_version 是 make_release 交叉校验的依据"
        Assert-Equal -Expected $tag -Actual $info['image_tag'] -Because "release 轨 tag = <版本>-<commit>"
        Assert-Equal -Expected 'go' -Actual (@($info['families']) -join ',') -Because "只发 go"
        Assert-True -Condition (-not [System.IO.Directory]::Exists((Join-Path $final 'symbols'))) -Because "不含 cpp 时不带 C++ 符号"

        $listRefs = @(Get-CallSequence -Fixture $fx | Where-Object { $_.Command -eq 'list-refs' })
        Assert-Equal -Expected 1 -Actual $listRefs.Count -Because "只调 go 的 list-refs"
        Assert-Equal -Expected $tag -Actual $listRefs[0].Tag -Because "list-refs 必须收到本次 tag"
        Assert-Equal -Expected 'v1.2.3' -Actual $listRefs[0].Version -Because "release 轨要把 -Version 传给镜像脚本"

        $latest = Read-JsonFile -Path (Join-Path $imagesRoot 'latest.json')
        Assert-Equal -Expected 'v1.2.3' -Actual $latest['version'] -Because "releases/images/latest.json 指向该版本"
    }

    Test-Case '负向:-SkipIfExists 不能掩盖"同一版本号已由其他提交发布"' {
        $fx = $script:ReleaseFixture
        Assert-True -Condition ($null -ne $fx) -Because "依赖上一条 release 发布用例"
        Write-TestFile -Path (Join-Path $fx.Repo 'generated' 'tables' 'Skill.json') -Text '{"rows":[3]}'
        Invoke-FixtureGit -Repo $fx.Repo -GitArgs @('commit', '-q', '-a', '-m', 'second') | Out-Null

        $run = Invoke-Publish -Fixture $fx -Params @{ SkipBuild = $true; Families = @('go'); Version = 'v1.2.3'; SkipIfExists = $true }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "换了提交还用旧版本号,静默成功会让 CI 以为新代码已出包"
        Assert-Match -Text $run.Output -Pattern '已由其他提交发布' -Because "错误要说清是版本号被另一提交占用"
    }

    Test-Case "构建路径:按 family 顺序先 preflight/构建,再 list-refs,且都带本次 tag" {
        $fx = New-PublishFixture
        Set-DockerImages -Fixture $fx -Refs (Get-FixtureRefs -Tag $fx.Commit)

        $run = Invoke-Publish -Fixture $fx
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        $calls = @(Get-CallSequence -Fixture $fx)
        Assert-Equal -Expected 'cpp/preflight,cpp/build-image,go/build-all,java/build,cpp/list-refs,go/list-refs,java/list-refs' `
            -Actual (($calls | ForEach-Object { "$($_.Family)/$($_.Command)" }) -join ',') -Because "C++ 先查 staging 再构建;各 family 构建完才收清单"
        foreach ($c in $calls) {
            Assert-Equal -Expected $fx.Commit -Actual $c.Tag -Because "$($c.Family)/$($c.Command) 必须收到本次 tag"
            Assert-Equal -Expected '' -Actual $c.Version -Because "快照轨不传 -Version"
        }
    }

    Test-Case "负向:镜像构建失败 / C++ 运行时 staging 缺失必须拒绝并说清原因" {
        $fx = New-PublishFixture
        Set-DockerImages -Fixture $fx -Refs (Get-FixtureRefs -Tag $fx.Commit)

        $run = Invoke-Publish -Fixture $fx -Env @{ PUBLISH_TEST_FAIL_COMMAND = 'go/build-all' }
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "构建失败不能继续出包"
        Assert-Match -Text $run.Output -Pattern 'go 镜像构建失败' -Because "错误要点名失败的 family"

        $run2 = Invoke-Publish -Fixture $fx -Env @{ PUBLISH_TEST_FAIL_COMMAND = 'cpp/preflight' }
        Assert-True -Condition ($run2.ExitCode -ne 0) -Because "staging 不全不能构建 C++ 镜像"
        Assert-Match -Text $run2.Output -Pattern 'Linux 运行时 staging 不完整' -Because "错误要说清缺的是运行时 staging"
        Assert-Match -Text $run2.Output -Pattern '-Families go,java' -Because "错误要给出跳过 C++ 的办法"
        Assert-Equal -Expected 0 -Actual @(Get-PublishedEntries -ImagesRoot (Get-ImagesRoot -Fixture $fx -Channel snapshots)).Count -Because "失败不得产生版本目录"
    }
}
finally {
    foreach ($k in $savedGitEnv.Keys) { [System.Environment]::SetEnvironmentVariable($k, $savedGitEnv[$k]) }
    if ([System.IO.Directory]::Exists($script:TestRoot)) {
        Remove-Item -LiteralPath $script:TestRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName "publish_images contract")
