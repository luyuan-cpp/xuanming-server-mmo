#requires -Version 7
<#
.SYNOPSIS
    tools/scripts/lib/artifacts_lib.ps1 的契约测试(制品根解析、校验和、原子发布、latest 指针、表摘要)。

.DESCRIPTION
    这些函数守的是制品目录三条铁律(不可变 / 原子 / 可追溯),publish / fetch / import /
    retention / make_release / release_preflight 全靠它们;这里一旦放水,下游脚本的校验全是摆设。

    全部在进程内 dot-source 调用,只读写本测试自建的临时目录,不依赖 docker / 网络 / git 远端。
    "默认制品根"用例只用 -MustExist 验证 <仓库父目录>/artifacts 的推算,不创建该目录:
    以非 root 用户在容器里跑时仓库父目录可能是 /,创建会因权限失败;开发机上临时建出空根
    还会让同时运行的 fetch / retention 把"制品根不存在"报成"制品不存在"。

    负向用例一律断言错误文本,不只断"抛了异常"。

.EXAMPLE
    pwsh -File tools/scripts/tests/artifacts_lib.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"

# 逐级取父目录而不是拼 "..":被测库按自身位置上溯推算默认制品根,这里给它一个规范化路径
$script:ToolsScriptsDir = Split-Path -Parent $PSScriptRoot
$script:RepoRoot = Split-Path -Parent (Split-Path -Parent $script:ToolsScriptsDir)
. (Join-Path $script:ToolsScriptsDir 'lib' 'artifacts_lib.ps1')

Write-Host ''
Write-Host '=== artifacts_lib.ps1 制品目录公共库契约测试 ==='

$script:TempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-artifacts-lib-tests-' + [guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($script:TempRoot) | Out-Null

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

function Get-ThrownMessage {
    param([scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Test-SamePath {
    param([string]$A, [string]$B)
    $cmp = if ($IsWindows) { [System.StringComparison]::OrdinalIgnoreCase } else { [System.StringComparison]::Ordinal }
    return [string]::Equals($A.TrimEnd([char[]]@('/', '\')), $B.TrimEnd([char[]]@('/', '\')), $cmp)
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

# 一个带子目录、大小写混排文件名的版本目录样本
function New-SampleVersionDir {
    param([string]$Name)
    $dir = New-CaseDir -Name $Name
    Write-TestFile -Path (Join-Path $dir 'images' 'registry.invalid_test_mmorpg-login_v1.tar') -Content 'fake-tar-login'
    Write-TestFile -Path (Join-Path $dir 'build-info.json') -Content '{"version":"g0123456789ab"}'
    Write-TestFile -Path (Join-Path $dir 'B-upper.json') -Content 'upper'
    Write-TestFile -Path (Join-Path $dir 'a-lower.json') -Content 'lower'
    return $dir
}

$savedArtifactRoot = $env:MMORPG_ARTIFACT_ROOT
try {

    # ─────────────────────────────────────────────────────────────
    # 1. 制品根解析优先级
    # ─────────────────────────────────────────────────────────────

    Test-Case '制品根:-Override 优先于环境变量 MMORPG_ARTIFACT_ROOT,且目录被创建' {
        try {
            $envRoot = Join-Path $script:TempRoot 'root-env-1'
            $overrideRoot = Join-Path $script:TempRoot 'root-override-1'
            $env:MMORPG_ARTIFACT_ROOT = $envRoot
            $got = Get-ArtifactRoot -Override $overrideRoot
            Assert-True -Condition (Test-SamePath $got $overrideRoot) -Because "-Override 必须赢过环境变量(实际 $got)"
            Assert-True -Condition ([System.IO.Directory]::Exists($overrideRoot)) -Because '制品根不存在时应当创建'
            Assert-True -Condition (-not [System.IO.Directory]::Exists($envRoot)) -Because '被覆盖的环境变量路径不该被创建'
        }
        finally { $env:MMORPG_ARTIFACT_ROOT = $savedArtifactRoot }
    }

    Test-Case '制品根:无 -Override 时用环境变量 MMORPG_ARTIFACT_ROOT' {
        try {
            $envRoot = Join-Path $script:TempRoot 'root-env-2'
            $env:MMORPG_ARTIFACT_ROOT = $envRoot
            $got = Get-ArtifactRoot
            Assert-True -Condition (Test-SamePath $got $envRoot) -Because "应当取环境变量(实际 $got)"
        }
        finally { $env:MMORPG_ARTIFACT_ROOT = $savedArtifactRoot }
    }

    Test-Case '制品根:两者都没有时默认 <仓库父目录>/artifacts(按脚本位置推算,不在仓库内;-MustExist 验证,不创建目录)' {
        $expected = Join-Path (Split-Path -Parent $script:RepoRoot) 'artifacts'
        $cmp = if ($IsWindows) { [System.StringComparison]::OrdinalIgnoreCase } else { [System.StringComparison]::Ordinal }
        Assert-True -Condition (-not $expected.StartsWith($script:RepoRoot + [System.IO.Path]::DirectorySeparatorChar, $cmp)) -Because '默认制品根不得落在仓库内(GB 级 tar 会被 git add 带走)'
        try {
            $env:MMORPG_ARTIFACT_ROOT = $null
            if ([System.IO.Directory]::Exists($expected)) {
                $got = Get-ArtifactRoot -MustExist
                Assert-True -Condition (Test-SamePath $got $expected) -Because "默认制品根应为 $expected(实际 $got)"
            } else {
                # "不存在时会创建"的语义由 -Override 指向临时目录的用例覆盖,这里只验证推算出的路径
                $msg = Get-ThrownMessage { Get-ArtifactRoot -MustExist }
                Assert-Match -Text $msg -Pattern ('制品根不存在:' + [regex]::Escape($expected) + '(') -Because "默认制品根应推算为 $expected,错误文本应原样报出该路径"
                Assert-True -Condition (-not [System.IO.Directory]::Exists($expected)) -Because '-MustExist 不得创建默认制品根'
            }
        }
        finally { $env:MMORPG_ARTIFACT_ROOT = $savedArtifactRoot }
    }

    Test-Case '两轨分仓:snapshot -> <root>/snapshots,release -> <root>/releases' {
        $root = Join-Path $script:TempRoot 'root-channel'
        $snap = Get-ChannelRoot -Channel snapshot -Override $root
        $rel = Get-ChannelRoot -Channel release -Override $root
        Assert-True -Condition (Test-SamePath $snap (Join-Path $root 'snapshots')) -Because "snapshot 轨路径错误:$snap"
        Assert-True -Condition (Test-SamePath $rel (Join-Path $root 'releases')) -Because "release 轨路径错误:$rel"
        Assert-True -Condition ([System.IO.Directory]::Exists($snap) -and [System.IO.Directory]::Exists($rel)) -Because '轨道目录应当被创建'
    }

    Test-Case '制品根 -MustExist:不存在时抛"制品根不存在"且不创建;存在时 Get-ChannelRoot -MustExist 不建轨道目录' {
        $missing = Join-Path $script:TempRoot 'root-missing'
        $msg1 = Get-ThrownMessage { Get-ArtifactRoot -Override $missing -MustExist }
        Assert-Match -Text $msg1 -Pattern '制品根不存在' -Because '只读用途(fetch / retention)指向不存在的制品根必须报错,不能悄悄建一个空根'
        $msg2 = Get-ThrownMessage { Get-ChannelRoot -Channel snapshot -Override $missing -MustExist }
        Assert-Match -Text $msg2 -Pattern '制品根不存在' -Because 'Get-ChannelRoot -MustExist 同样不得替调用方建出制品根'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($missing)) -Because '-MustExist 不得创建制品根'

        $existing = New-CaseDir -Name 'root-mustexist'
        $snap = Get-ChannelRoot -Channel snapshot -Override $existing -MustExist
        Assert-True -Condition (Test-SamePath $snap (Join-Path $existing 'snapshots')) -Because "轨道路径错误:$snap"
        Assert-True -Condition (-not [System.IO.Directory]::Exists($snap)) -Because '-MustExist 不创建轨道目录,轨道是否发布过由调用方判断'
    }

    # ─────────────────────────────────────────────────────────────
    # 2. sha256sums 生成与校验
    # ─────────────────────────────────────────────────────────────

    Test-Case 'sha256sums 往返:格式、序数排序、/ 分隔、排除自身、哈希正确,校验通过' {
        $dir = New-SampleVersionDir -Name 'sums-roundtrip'
        $out = New-Sha256Sums -Dir $dir
        Assert-True -Condition (Test-SamePath $out (Join-Path $dir 'sha256sums.txt')) -Because "返回值应为 <Dir>/sha256sums.txt(实际 $out)"

        $raw = [System.IO.File]::ReadAllText($out)
        Assert-NotMatch -Text $raw -Pattern "`r" -Because 'sha256sums.txt 必须是 LF 换行(CRLF 会让 sha256sum -c 找不到文件)'
        $lines = @($raw.Split("`n") | Where-Object { $_ -ne '' })
        Assert-Equal -Expected 4 -Actual $lines.Count -Because '4 个文件对应 4 行,且不含 sha256sums.txt 自身'

        $paths = @($lines | ForEach-Object { $_.Substring(66) })
        # 序数排序:大写 B 排在小写 a 之前;区域排序会得到 a-lower 在前
        Assert-Equal -Expected 'B-upper.json|a-lower.json|build-info.json|images/registry.invalid_test_mmorpg-login_v1.tar' -Actual ($paths -join '|') -Because '行序必须按相对路径序数排序,子目录用 / 分隔'

        foreach ($line in $lines) {
            Assert-Match -Text $line -Pattern '^[0-9a-f]{64}  \S+$' -Because '每行必须是 <64 位小写 hex><两个空格><相对路径>'
            $rel = $line.Substring(66)
            $want = (Get-FileHash -LiteralPath (Join-Path $dir $rel) -Algorithm SHA256).Hash.ToLowerInvariant()
            Assert-Equal -Expected $want -Actual $line.Substring(0, 64) -Because "$rel 的哈希必须与文件实际内容一致"
        }

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Equal -Expected '' -Actual $msg -Because '未改动的目录必须校验通过'
    }

    Test-Case '篡改 1 字节被抓:报"哈希不符"并指出文件' {
        $dir = New-SampleVersionDir -Name 'sums-tamper'
        New-Sha256Sums -Dir $dir | Out-Null
        $tar = Join-Path $dir 'images' 'registry.invalid_test_mmorpg-login_v1.tar'
        $bytes = [System.IO.File]::ReadAllBytes($tar)
        $bytes[0] = $bytes[0] -bxor 0x01
        [System.IO.File]::WriteAllBytes($tar, $bytes)

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '哈希不符 images/registry\.invalid_test_mmorpg-login_v1\.tar' -Because '改 1 字节必须被识别为哈希不符'
    }

    Test-Case '清单外多出文件被抓:报"清单外多出文件"(A 仓漏掉的场景)' {
        $dir = New-SampleVersionDir -Name 'sums-extra'
        New-Sha256Sums -Dir $dir | Out-Null
        Write-TestFile -Path (Join-Path $dir 'images' 'smuggled.tar') -Content 'not-in-manifest'

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '清单外多出文件 images/smuggled\.tar' -Because '往版本目录里多塞一个 tar 必须校验失败'
    }

    Test-Case '清单外的隐藏文件同样被抓(Linux dotfile / Windows 隐藏属性)' {
        $dir = New-SampleVersionDir -Name 'sums-hidden'
        New-Sha256Sums -Dir $dir | Out-Null
        $hidden = Join-Path $dir '.hidden-extra'
        Write-TestFile -Path $hidden -Content 'hidden'
        if ($IsWindows) {
            [System.IO.File]::SetAttributes($hidden, [System.IO.File]::GetAttributes($hidden) -bor [System.IO.FileAttributes]::Hidden)
        }

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '清单外多出文件 \.hidden-extra' -Because '枚举文件必须带 -Force,否则隐藏文件能绕过校验'
    }

    Test-Case '缺文件被抓:报"缺失"' {
        $dir = New-SampleVersionDir -Name 'sums-missing'
        New-Sha256Sums -Dir $dir | Out-Null
        Remove-Item -LiteralPath (Join-Path $dir 'build-info.json') -Force

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '缺失 build-info\.json' -Because '清单列出的文件被删必须校验失败'
    }

    Test-Case '缺 sha256sums.txt 被抓:报"缺少校验文件"' {
        $dir = New-SampleVersionDir -Name 'sums-nofile'
        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '缺少校验文件' -Because '没有校验清单不能当作通过'
    }

    Test-Case '清单路径穿越被拒:报"路径非法"' {
        $dir = New-SampleVersionDir -Name 'sums-traversal'
        New-Sha256Sums -Dir $dir | Out-Null
        $sums = Join-Path $dir 'sha256sums.txt'
        $evil = ('0' * 64) + '  ../outside.txt' + "`n"
        [System.IO.File]::AppendAllText($sums, $evil, [System.Text.UTF8Encoding]::new($false))

        $msg = Get-ThrownMessage { Test-Sha256Sums -Dir $dir }
        Assert-Match -Text $msg -Pattern '路径非法 \.\./outside\.txt' -Because '清单里的 .. 段必须拒绝,不能去读版本目录外的文件'
    }

    # ─────────────────────────────────────────────────────────────
    # 3. 不可变 + 原子发布
    # ─────────────────────────────────────────────────────────────

    Test-Case '不可变:FinalDir 已存在时 New-AtomicStaging 抛"不可变"' {
        $parent = New-CaseDir -Name 'atomic-immutable'
        $final = Join-Path $parent 'g0123456789ab'
        [System.IO.Directory]::CreateDirectory($final) | Out-Null

        $msg = Get-ThrownMessage { New-AtomicStaging -FinalDir $final }
        Assert-Match -Text $msg -Pattern '不可变' -Because '已发布版本目录禁止覆盖'
        Assert-Equal -Expected 0 -Actual (@(Get-ChildItem -LiteralPath $parent -Force -Filter '.tmp-*').Count) -Because '被拒时不应留下 staging 目录'
    }

    Test-Case '原子发布:staging 命名为 .tmp-<leaf>-<PID>,rename 后内容就位且 staging 消失' {
        $parent = New-CaseDir -Name 'atomic-ok'
        $final = Join-Path $parent 'g0123456789ab'
        $staging = New-AtomicStaging -FinalDir $final
        Assert-True -Condition (Test-SamePath $staging (Join-Path $parent ".tmp-g0123456789ab-$PID")) -Because "staging 路径不符合约定(实际 $staging)"
        Assert-True -Condition ([System.IO.Directory]::Exists($staging)) -Because 'staging 应当已创建'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($final)) -Because 'Complete 之前 FinalDir 不得出现'

        Write-TestFile -Path (Join-Path $staging 'build-info.json') -Content '{}'
        $out = @(Complete-AtomicDir -Staging $staging -FinalDir $final)
        Assert-Equal -Expected 0 -Actual $out.Count -Because 'Complete-AtomicDir 不得往管道输出东西(会污染调用方函数的返回值)'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $final 'build-info.json'))) -Because 'rename 后内容应在 FinalDir'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($staging)) -Because 'rename 后 staging 应消失'
    }

    Test-Case '发布竞争:staging 期间目标被抢先发布 -> 抛"发布竞争"、删 staging、不污染对方目录' {
        $parent = New-CaseDir -Name 'atomic-race'
        $final = Join-Path $parent 'g0123456789ab'
        $staging = New-AtomicStaging -FinalDir $final
        Write-TestFile -Path (Join-Path $staging 'mine.txt') -Content 'mine'

        # 模拟另一台构建机在我们 rename 之前发布了同一版本
        Write-TestFile -Path (Join-Path $final 'theirs.txt') -Content 'theirs'

        $msg = Get-ThrownMessage { Complete-AtomicDir -Staging $staging -FinalDir $final }
        Assert-Match -Text $msg -Pattern '发布竞争' -Because '目标已被他人发布时必须报发布竞争'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($staging)) -Because '发布竞争时应丢弃本次 staging'
        $names = @(Get-ChildItem -LiteralPath $final -Force | ForEach-Object Name) -join ','
        Assert-Equal -Expected 'theirs.txt' -Actual $names -Because '对方已发布的目录必须原样保留(A 用 Move-Item 会把 staging 嵌进去)'
    }

    Test-Case '同名 staging 已存在(同 PID 并发发布或强杀残留)-> 抛"staging 已存在",不删其中内容' {
        $parent = New-CaseDir -Name 'atomic-staging-exists'
        $final = Join-Path $parent 'g0123456789ab'
        $other = Join-Path $parent ".tmp-g0123456789ab-$PID"
        Write-TestFile -Path (Join-Path $other 'theirs.tar') -Content 'in-progress'

        $msg = Get-ThrownMessage { New-AtomicStaging -FinalDir $final }
        Assert-Match -Text $msg -Pattern 'staging 已存在' -Because '同名 staging 可能属于共享盘上另一个同 PID 的发布者,不能删'
        Assert-True -Condition ([System.IO.File]::Exists((Join-Path $other 'theirs.tar'))) -Because '已存在 staging 里的文件必须原样保留'
        Assert-True -Condition (-not [System.IO.Directory]::Exists($final)) -Because '被拒时不得创建 FinalDir'
    }

    # ─────────────────────────────────────────────────────────────
    # 4. latest 指针
    # ─────────────────────────────────────────────────────────────

    Test-Case 'latest 指针:写 {version, channel, published_at},覆盖写生效,无临时文件残留' {
        $root = Join-Path $script:TempRoot 'root-latest'
        $snapRoot = Get-ChannelRoot -Channel snapshot -Override $root
        $out = @(Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version 'g0123456789ab')
        Assert-Equal -Expected 0 -Actual $out.Count -Because 'Set-LatestPointer 不得往管道输出东西'

        $path = Join-Path $snapRoot 'images' 'latest.json'
        $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
        Assert-Equal -Expected 'g0123456789ab' -Actual $obj.version -Because 'version 字段'
        Assert-Equal -Expected 'snapshot' -Actual $obj.channel -Because 'snapshots 目录下的指针 channel 必须是 snapshot'
        Assert-Match -Text ([string](Get-Content -LiteralPath $path -Raw)) -Pattern '"published_at":\s*"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"' -Because 'published_at 必须是 UTC yyyy-MM-ddTHH:mm:ssZ'

        Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version 'g0123456789ab-dirty-20260916-101010'
        $obj2 = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
        Assert-Equal -Expected 'g0123456789ab-dirty-20260916-101010' -Actual $obj2.version -Because '第二次写应覆盖指针'

        $leftovers = @(Get-ChildItem -LiteralPath (Join-Path $snapRoot 'images') -Force | Where-Object { $_.Name -ne 'latest.json' })
        Assert-Equal -Expected 0 -Actual $leftovers.Count -Because "原子写不应残留临时文件(实际:$(@($leftovers | ForEach-Object Name) -join ','))"

        $relRoot = Get-ChannelRoot -Channel release -Override $root
        Set-LatestPointer -ChannelRoot $relRoot -Kind images -Version 'v1.2.3'
        $obj3 = Get-Content -LiteralPath (Join-Path $relRoot 'images' 'latest.json') -Raw | ConvertFrom-Json
        Assert-Equal -Expected 'release' -Actual $obj3.channel -Because 'releases 目录下的指针 channel 必须是 release'
    }

    Test-Case 'latest 指针:当前区域的时间分隔符不是 ":" 时 published_at 仍是 UTC yyyy-MM-ddTHH:mm:ssZ' {
        $root = Join-Path $script:TempRoot 'root-latest-culture'
        $snapRoot = Get-ChannelRoot -Channel snapshot -Override $root
        # 克隆不变区域再改分隔符,而不是取 fi-FI:CI 若以 .NET 全球化不变模式运行,具名区域取不到,用例会变成空转
        $culture = [System.Globalization.CultureInfo]::InvariantCulture.Clone()
        $culture.DateTimeFormat.TimeSeparator = '.'
        $savedCulture = [System.Globalization.CultureInfo]::CurrentCulture
        try {
            [System.Globalization.CultureInfo]::CurrentCulture = $culture
            Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version 'g0123456789ab'
        }
        finally { [System.Globalization.CultureInfo]::CurrentCulture = $savedCulture }

        $raw = [System.IO.File]::ReadAllText((Join-Path $snapRoot 'images' 'latest.json'))
        Assert-Match -Text $raw -Pattern '"published_at":\s*"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"' -Because '时间分隔符必须固定为 ":",不随区域设置变成 "."'
    }

    Test-Case 'latest 指针:版本号含路径分隔符 / ChannelRoot 不是轨道目录时拒绝' {
        $root = Join-Path $script:TempRoot 'root-latest-bad'
        $snapRoot = Get-ChannelRoot -Channel snapshot -Override $root
        $msg1 = Get-ThrownMessage { Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version '../g0123456789ab' }
        Assert-Match -Text $msg1 -Pattern '版本号非法' -Because '指针版本号必须是单个目录名'
        $msg2 = Get-ThrownMessage { Set-LatestPointer -ChannelRoot $root -Kind images -Version 'g0123456789ab' }
        Assert-Match -Text $msg2 -Pattern 'ChannelRoot 必须是' -Because '直接传制品根会写出无法归属轨道的指针'
        $msg3 = Get-ThrownMessage { Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version "g0123456789ab`n" }
        Assert-Match -Text $msg3 -Pattern '版本号非法' -Because '.NET 正则的 $ 会放过末尾换行,指针版本号必须用 \z 收尾'
        Assert-True -Condition (-not [System.IO.File]::Exists((Join-Path $snapRoot 'images' 'latest.json'))) -Because '版本号非法时不得写出指针'
    }

    Test-Case 'latest 指针:latest.json 正被读者打开(不共享删除)时有限重试,读者释放后写入成功' {
        $root = Join-Path $script:TempRoot 'root-latest-busy'
        $snapRoot = Get-ChannelRoot -Channel snapshot -Override $root
        Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version 'g0123456789ab'
        $path = Join-Path $snapRoot 'images' 'latest.json'

        # 模拟 fetch / retention 用 Get-Content 读指针:只读打开、共享读写、不共享删除。
        # 300ms 后由计时器线程直接调用 FileStream.Dispose 释放句柄(纯 .NET 委托,不需要 PowerShell runspace)。
        # Windows 上覆盖 rename 在句柄释放前失败,须靠重试(100/200/400ms,累计 700ms)跨过去;
        # Linux 上 rename(2) 不受读者影响,首次即成功,本用例只验证结果
        $reader = [System.IO.FileStream]::new($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $cts = [System.Threading.CancellationTokenSource]::new()
        $msg = ''
        try {
            $release = [System.Delegate]::CreateDelegate([System.Action], $reader, 'Dispose')
            $null = $cts.Token.Register($release)
            $cts.CancelAfter(300)
            $msg = Get-ThrownMessage { Set-LatestPointer -ChannelRoot $snapRoot -Kind images -Version 'g0123456789ab-dirty-20260916-101010' }
        }
        finally {
            # 先停计时器再关句柄:Linux 上写入早已返回,避免计时器线程与这里并发 Dispose
            $cts.Dispose()
            $reader.Dispose()
        }

        Assert-Equal -Expected '' -Actual $msg -Because '读者短暂占用 latest.json 时覆盖写应在重试预算内成功'
        $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
        Assert-Equal -Expected 'g0123456789ab-dirty-20260916-101010' -Actual $obj.version -Because '重试成功后指针必须是新版本'
        $leftovers = @(Get-ChildItem -LiteralPath (Join-Path $snapRoot 'images') -Force | Where-Object { $_.Name -ne 'latest.json' })
        Assert-Equal -Expected 0 -Actual $leftovers.Count -Because "重试后不应残留临时文件(实际:$(@($leftovers | ForEach-Object Name) -join ','))"
    }

    Test-Case '快照版本目录名:只认 g<sha12> 与 g<sha12>-dirty-yyyyMMdd-HHmmss' {
        Assert-True -Condition (Test-SnapshotVersionName -Version 'g0123456789ab') -Because '干净快照名'
        Assert-True -Condition (Test-SnapshotVersionName -Version 'g0123456789ab-dirty-20260916-101010') -Because '脏树快照名'
        # 后两个样本是 .NET 正则陷阱:$ 放过末尾换行;\d 匹配全角数字(U+FF16 '６')
        $fullWidthDigitName = 'g0123456789ab-dirty-2026091' + [char]0xFF16 + '-101010'
        foreach ($bad in @('', 'g0123456789AB', 'g0123456789a', '0123456789ab', '../g0123456789ab', 'v1.2.3', 'g0123456789ab-dirty', "g0123456789ab`n", $fullWidthDigitName)) {
            Assert-True -Condition (-not (Test-SnapshotVersionName -Version $bad)) -Because "'$bad' 不应被当成快照版本名"
        }
    }

    # ─────────────────────────────────────────────────────────────
    # 5. 策划表摘要
    # ─────────────────────────────────────────────────────────────

    Test-Case '表摘要:只算顶层 *.json、按文件名序数排序、结果与契约算法一致' {
        $dir = New-CaseDir -Name 'tables'
        Write-TestFile -Path (Join-Path $dir 'item.json') -Content '{"id":1}'
        Write-TestFile -Path (Join-Path $dir 'Buff.json') -Content '{"id":2}'
        Write-TestFile -Path (Join-Path $dir 'item.pb') -Content 'pb-bytes'
        Write-TestFile -Path (Join-Path $dir 'sub' 'nested.json') -Content '{"id":3}'

        $r = Get-TablesDigest -TablesDir $dir
        Assert-Equal -Expected 2 -Actual $r.FileCount -Because '只计顶层 .json(.pb 与子目录不算)'

        $text = ''
        foreach ($name in @('Buff.json', 'item.json')) {
            $h = (Get-FileHash -LiteralPath (Join-Path $dir $name) -Algorithm SHA256).Hash.ToLowerInvariant()
            $text += "$h  $name`n"
        }
        Assert-Equal -Expected (Get-TextSha256 -Text $text) -Actual $r.Sha256 -Because '摘要 = sha256(逐行 "<sha256>  <文件名>\n" 按序数排序拼接)'

        Write-TestFile -Path (Join-Path $dir 'item.pb') -Content 'pb-bytes-changed'
        Assert-Equal -Expected $r.Sha256 -Actual ((Get-TablesDigest -TablesDir $dir).Sha256) -Because '接口 B 约定摘要只覆盖 *.json,.pb 变化不改变摘要(C++ TableDataFormat=binary 时这是已知盲区;改口径须先改接口 B 并同步各方测试)'

        Write-TestFile -Path (Join-Path $dir 'item.json') -Content '{"id":100}'
        Assert-True -Condition ((Get-TablesDigest -TablesDir $dir).Sha256 -ne $r.Sha256) -Because 'json 内容变化必须改变摘要'
    }

    Test-Case '表摘要:目录不存在 / 没有 json 时抛异常' {
        $msg1 = Get-ThrownMessage { Get-TablesDigest -TablesDir (Join-Path $script:TempRoot 'no-such-tables') }
        Assert-Match -Text $msg1 -Pattern '策划表目录不存在' -Because '目录不存在不能返回一个"空摘要"'
        $empty = New-CaseDir -Name 'tables-empty'
        Write-TestFile -Path (Join-Path $empty 'only.pb') -Content 'pb'
        $msg2 = Get-ThrownMessage { Get-TablesDigest -TablesDir $empty }
        Assert-Match -Text $msg2 -Pattern '没有 \*\.json' -Because '0 张表只会是表生成漏跑'
    }
}
finally {
    $env:MMORPG_ARTIFACT_ROOT = $savedArtifactRoot
    if ([System.IO.Directory]::Exists($script:TempRoot)) {
        Remove-Item -LiteralPath $script:TempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'artifacts_lib')
