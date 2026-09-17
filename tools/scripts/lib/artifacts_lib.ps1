#requires -Version 7
<#
.SYNOPSIS
    制品目录公共函数库(只定义函数,dot-source 无副作用,不单独运行)。

.DESCRIPTION
    被 publish_images.ps1 / fetch_images.ps1 / import_images.ps1 / artifacts_retention.ps1 /
    make_release.ps1 / release_preflight.ps1 以及 tools/scripts/tests/ 下的契约测试引用。

    制品目录("版本库外"的构建产物归档地)布局:
      <root>/{snapshots|releases}/images/<ver>/
          images/<镜像 ref 中 / 与 : 替换为 _>.tar
          images-manifest.json / build-info.json / symbols/*.debug(可选) / sha256sums.txt
      <root>/{snapshots|releases}/images/latest.json      唯一可变文件(指针)
      <root>/releases/manifests/<vX.Y.Z>.json|.md

    三条铁律由本库的函数强制,调用方不要绕开:
      1. 不可变:版本目录已存在即拒绝覆盖(New-AtomicStaging)
      2. 原子发布:先写 staging,整目录 rename 上线;期间被抢先发布则丢弃并报"发布竞争"
      3. 可追溯:sha256sums.txt 覆盖版本目录内全部文件,清单外多一个文件也算失败

    设计文档:docs/design/release-packaging-standard-20260914.md §2.3、§4 P2。
    对标 A 仓 tools/scripts/artifacts_lib.ps1,相对 A 的改动见各函数注释。

    约定:所有接收路径的函数都按 PowerShell 当前位置解析相对路径(见 Resolve-ArtifactFullPath),
    只有 New-Sha256Sums 有返回值(sha256sums.txt 路径)与 Get-* / Test-SnapshotVersionName 返回结果,
    其余函数不往管道输出任何东西 —— 调用方在函数里顺手调用时不会污染自己的返回值。

    刻意不写 Set-StrictMode:dot-source 时它会改掉调用方作用域的严格模式(副作用);
    本库代码刻意不依赖"读未初始化变量 / 读不存在属性得 $null"这类宽松语义,调用方开严格模式也应可用。
#>

<#
.SYNOPSIS
    把路径解析成去掉尾部分隔符的绝对路径。

.DESCRIPTION
    [System.IO.Path]::GetFullPath 按**进程**工作目录解析相对路径,而 PowerShell 的
    Set-Location 不改进程工作目录,两者会分家;这里先按 PowerShell 当前位置展开。
#>
function Resolve-ArtifactFullPath {
    param([Parameter(Mandatory = $true)][string]$Path)

    $p = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    $p = [System.IO.Path]::GetFullPath($p)
    $trimmed = $p.TrimEnd([char[]]@('/', '\'))
    # 文件系统根("/" 或 "C:\")本身不能再裁掉分隔符
    if ($trimmed.Length -eq 0 -or $trimmed -match '^[A-Za-z]:$') { return $p }
    return $trimmed
}

<#
.SYNOPSIS
    判断字符串是不是合法的快照版本目录名:g<12 位小写 sha>,脏树为 g<sha12>-dirty-yyyyMMdd-HHmmss。

.DESCRIPTION
    fetch 的 -Version 与 retention 的删除对象都只认这个形状 —— 前者防 "../" 穿越出制品根,
    后者防手工放进 snapshots/images 的备份目录被当成过期快照删掉。
#>
function Test-SnapshotVersionName {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Version)
    return ($Version -cmatch '^g[0-9a-f]{12}(-dirty-\d{8}-\d{6})?$')
}

<#
.SYNOPSIS
    解析制品根目录,不存在则创建(带 -MustExist 时改为抛异常、不创建)。

.DESCRIPTION
    优先级:-Override > $env:MMORPG_ARTIFACT_ROOT > <仓库父目录>/artifacts。

    默认值按本文件位置($PSScriptRoot = <仓库>/tools/scripts/lib)上溯推算,
    不写死盘符:A 仓写死 'F:\work\artifacts',换机器/换 Linux CI 就指到不存在的盘。
    制品根刻意放在仓库**外**,避免 GB 级 tar 被 `git add -A` 带进版本库。

    -MustExist 给只读用途(fetch / retention)用:路径写错、计划任务没带上环境变量、共享盘没挂上时,
    "不存在就创建"会悄悄建出一个空制品根 —— retention 从此每次都"无快照需要清理"地假绿、真实制品根
    一直没人清;fetch 则把真正原因报成"制品不存在"。发布类调用方(publish / make_release)不带它。

.OUTPUTS
    [string] 绝对路径(不带尾部分隔符)。
#>
function Get-ArtifactRoot {
    param(
        [string]$Override,
        [switch]$MustExist
    )

    $raw = if (-not [string]::IsNullOrWhiteSpace($Override)) {
        $Override
    } elseif (-not [string]::IsNullOrWhiteSpace($env:MMORPG_ARTIFACT_ROOT)) {
        $env:MMORPG_ARTIFACT_ROOT
    } else {
        # lib -> scripts -> tools -> <仓库>,再取父目录。先规范化:调用方若以 "tests/../lib/..."
        # 这类带 .. 的路径 dot-source,逐级 Split-Path 会数错层级
        $libDir = [System.IO.Path]::GetFullPath($PSScriptRoot)
        $repoRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $libDir))
        Join-Path (Split-Path -Parent $repoRoot) 'artifacts'
    }

    $root = Resolve-ArtifactFullPath -Path $raw
    if (-not [System.IO.Directory]::Exists($root)) {
        if ($MustExist) {
            throw "制品根不存在:$root(检查 -ArtifactRoot / MMORPG_ARTIFACT_ROOT,或共享盘是否已挂载)"
        }
        [System.IO.Directory]::CreateDirectory($root) | Out-Null
    }
    return $root
}

<#
.SYNOPSIS
    两轨分仓:snapshot -> <root>/snapshots(激进清理);release -> <root>/releases(永久保留)。
    目录不存在则创建。

.DESCRIPTION
    -MustExist:制品根必须已存在(见 Get-ArtifactRoot),且本调用**不创建任何目录**;
    轨道目录可能不存在,由调用方自行判断(制品根在、轨道目录不在 = 该轨道还没发布过,不是配置错误)。
#>
function Get-ChannelRoot {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('snapshot', 'release')][string]$Channel,
        [string]$Override,
        [switch]$MustExist
    )

    $root = Get-ArtifactRoot -Override $Override -MustExist:$MustExist
    $sub = if ($Channel -eq 'release') { 'releases' } else { 'snapshots' }
    $dir = Join-Path $root $sub
    if (-not $MustExist -and -not [System.IO.Directory]::Exists($dir)) {
        [System.IO.Directory]::CreateDirectory($dir) | Out-Null
    }
    return $dir
}

function Get-ArtifactFileSha256 {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

<#
.SYNOPSIS
    列出目录内全部文件的相对路径('/' 分隔,序数排序)。

.DESCRIPTION
    带 -Force:Linux 上 dotfile 默认隐藏,不带就会漏校验。
    只排除**根目录**下的 sha256sums.txt 自身;子目录里的同名文件照常参与。
    序数排序与区域设置无关,Windows 构建机与 Linux CI 生成的清单行序一致,可直接 diff。
#>
function Get-ArtifactRelativeFiles {
    param([Parameter(Mandatory = $true)][string]$Dir)

    $list = New-Object System.Collections.Generic.List[string]
    foreach ($f in Get-ChildItem -LiteralPath $Dir -Recurse -File -Force) {
        $rel = [System.IO.Path]::GetRelativePath($Dir, $f.FullName)
        if ([System.IO.Path]::DirectorySeparatorChar -ne '/') {
            $rel = $rel.Replace([System.IO.Path]::DirectorySeparatorChar, '/')
        }
        if ($rel -ceq 'sha256sums.txt') { continue }
        $list.Add($rel)
    }
    [string[]]$arr = $list.ToArray()
    [System.Array]::Sort($arr, [System.StringComparer]::Ordinal)
    return , $arr
}

<#
.SYNOPSIS
    对目录内全部文件生成 <Dir>/sha256sums.txt,返回该文件路径。

.DESCRIPTION
    每行 "<64 位小写 hex>  <相对路径,'/' 分隔>",按相对路径序数排序,LF 换行、UTF-8 无 BOM,
    与 `sha256sum -c sha256sums.txt` 兼容(CRLF 会让 GNU sha256sum 把 \r 当成文件名的一部分)。
    已存在的 sha256sums.txt 会被覆盖 —— 只应在 staging 目录里调用,已发布目录不可变。
#>
function New-Sha256Sums {
    param([Parameter(Mandatory = $true)][string]$Dir)

    $dirFull = Resolve-ArtifactFullPath -Path $Dir
    if (-not [System.IO.Directory]::Exists($dirFull)) { throw "生成校验和失败:目录不存在:$dirFull" }
    $out = Join-Path $dirFull 'sha256sums.txt'

    $sb = New-Object System.Text.StringBuilder
    foreach ($rel in (Get-ArtifactRelativeFiles -Dir $dirFull)) {
        $hash = Get-ArtifactFileSha256 -Path (Join-Path $dirFull $rel)
        [void]$sb.Append($hash).Append('  ').Append($rel).Append("`n")
    }
    [System.IO.File]::WriteAllText($out, $sb.ToString(), [System.Text.UTF8Encoding]::new($false))
    return $out
}

<#
.SYNOPSIS
    校验目录与 sha256sums.txt 完全一致;不一致抛异常,异常文本逐项列出问题。

.DESCRIPTION
    失败条件(任意一项):
      - sha256sums.txt 不存在
      - 清单行格式非法、路径非法(绝对路径 / 含空、. 或 .. 段 / 反斜杠)、同一路径重复
      - 清单列出的文件缺失
      - 哈希不符
      - 目录里存在清单外的文件(根目录 sha256sums.txt 自身除外)

    相对 A 的改进:A 对格式非法的行静默跳过、对清单外多出的文件不报 ——
    往离线包里多塞一个 tar,A 的校验照样通过。
#>
function Test-Sha256Sums {
    param([Parameter(Mandatory = $true)][string]$Dir)

    $dirFull = Resolve-ArtifactFullPath -Path $Dir
    if (-not [System.IO.Directory]::Exists($dirFull)) { throw "制品校验失败:目录不存在:$dirFull" }
    $sums = Join-Path $dirFull 'sha256sums.txt'
    if (-not [System.IO.File]::Exists($sums)) { throw "制品校验失败:缺少校验文件 $sums" }

    $bad = New-Object System.Collections.Generic.List[string]
    $listed = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    $lineNo = 0
    foreach ($line in [System.IO.File]::ReadAllLines($sums)) {
        $lineNo++
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $m = [regex]::Match($line, '^([0-9a-f]{64})  (.+)$')
        if (-not $m.Success) {
            $bad.Add("第 $lineNo 行格式非法(应为 '<64 位小写 hex>  <相对路径>')")
            continue
        }
        $want = $m.Groups[1].Value
        $rel = $m.Groups[2].Value

        $badSegments = @($rel.Split('/') | Where-Object { $_ -eq '' -or $_ -eq '.' -or $_ -eq '..' })
        $unsafe = $rel.Contains('\') -or ($rel -match '^[A-Za-z]:') -or $badSegments.Count -gt 0
        if ($unsafe -or $rel -ceq 'sha256sums.txt') {
            $bad.Add("第 $lineNo 行路径非法 $rel")
            continue
        }
        if (-not $listed.Add($rel)) {
            $bad.Add("清单内路径重复 $rel")
            continue
        }

        $file = Join-Path $dirFull $rel
        if (-not [System.IO.File]::Exists($file)) {
            $bad.Add("缺失 $rel")
            continue
        }
        if ((Get-ArtifactFileSha256 -Path $file) -cne $want) { $bad.Add("哈希不符 $rel") }
    }

    foreach ($rel in (Get-ArtifactRelativeFiles -Dir $dirFull)) {
        if (-not $listed.Contains($rel)) { $bad.Add("清单外多出文件 $rel") }
    }

    if ($bad.Count -gt 0) {
        throw "制品校验失败($($bad.Count) 项,目录 $dirFull):`n$($bad -join "`n")"
    }
}

<#
.SYNOPSIS
    为原子发布准备 staging 目录,返回其路径 <FinalDir 父目录>/.tmp-<leaf>-<PID>。

.DESCRIPTION
    调用方把内容全部写进 staging,再调 Complete-AtomicDir 一次 rename 上线。
    FinalDir 已存在时按不可变原则拒绝;CI 重跑想静默成功由调用方先 Test-Path 走 -SkipIfExists。
    staging 与 FinalDir 同父目录,保证 rename 不跨卷;.tmp- 前缀让 retention 跳过它。

    同名 staging 已存在时抛异常、**不删除**:PID 会复用,容器里的 pwsh 常是个位数 PID,
    制品根放共享盘时两台机器 / 两个容器可能以相同 PID 并发发布同一版本 —— 删掉等于毁掉对方
    正在写的 staging,两边随后写进同一个目录,先 rename 的一方会发布一个校验和自洽、内容混杂的版本。
    代价:被强杀留下的残留会挡住同 PID 的下一次发布,需人工确认后删除;retention 也不清 .tmp-*。
    Exists 与 CreateDirectory 之间仍有极小的检查-创建窗口(.NET 没有"目录已存在即失败"的创建原语)。
#>
function New-AtomicStaging {
    param([Parameter(Mandatory = $true)][string]$FinalDir)

    $final = Resolve-ArtifactFullPath -Path $FinalDir
    if ([System.IO.Directory]::Exists($final) -or [System.IO.File]::Exists($final)) {
        throw "制品版本目录已存在,不可变原则禁止覆盖:$final(需要新版本请提升版本号)"
    }
    $parent = Split-Path -Parent $final
    if (-not [System.IO.Directory]::Exists($parent)) {
        [System.IO.Directory]::CreateDirectory($parent) | Out-Null
    }
    $staging = Join-Path $parent (".tmp-" + (Split-Path -Leaf $final) + "-" + $PID)
    if ([System.IO.Directory]::Exists($staging) -or [System.IO.File]::Exists($staging)) {
        throw "staging 已存在:$staging(可能是共享盘或容器里同 PID 的并发发布,也可能是上次被强杀留下的残留;确认没有发布在进行后手工删除再重试)"
    }
    [System.IO.Directory]::CreateDirectory($staging) | Out-Null
    return $staging
}

<#
.SYNOPSIS
    把 staging 整目录 rename 成 FinalDir;期间被他人抢先发布则删除 staging 并抛"发布竞争"。

.DESCRIPTION
    用 [System.IO.Directory]::Move 而不是 A 的 Move-Item:
    Move-Item 在目标已是目录时会把源**移进**目标里面(变成 FinalDir/.tmp-xxx),
    A 在"Test-Path 之后、Move-Item 之前"被抢先发布时,会悄悄污染别人已发布的版本目录。
    Directory.Move 在目标存在时直接失败,这里复查目标后按"发布竞争"处理。
#>
function Complete-AtomicDir {
    param(
        [Parameter(Mandatory = $true)][string]$Staging,
        [Parameter(Mandatory = $true)][string]$FinalDir
    )

    $stagingFull = Resolve-ArtifactFullPath -Path $Staging
    $final = Resolve-ArtifactFullPath -Path $FinalDir
    if (-not [System.IO.Directory]::Exists($stagingFull)) { throw "原子发布失败:staging 目录不存在:$stagingFull" }

    $raceMessage = "发布竞争:目标在 staging 期间被他人发布,已丢弃本次 staging:$final"
    if ([System.IO.Directory]::Exists($final) -or [System.IO.File]::Exists($final)) {
        Remove-Item -LiteralPath $stagingFull -Recurse -Force
        throw $raceMessage
    }
    try {
        [System.IO.Directory]::Move($stagingFull, $final)
    }
    catch {
        if ([System.IO.Directory]::Exists($final) -or [System.IO.File]::Exists($final)) {
            if ([System.IO.Directory]::Exists($stagingFull)) { Remove-Item -LiteralPath $stagingFull -Recurse -Force }
            throw $raceMessage
        }
        throw "原子发布失败:rename $stagingFull -> $final 出错:$($_.Exception.Message)"
    }
}

<#
.SYNOPSIS
    原子写 <ChannelRoot>/<Kind>/latest.json = {version, channel, published_at}。

.DESCRIPTION
    latest.json 是制品目录里唯一可变的文件。先写同目录临时文件再覆盖式 rename,
    读者(fetch_images.ps1 / artifacts_retention.ps1)不会读到半截 JSON。
    channel 由 ChannelRoot 的目录名推出(snapshots -> snapshot,releases -> release),
    不另开参数,杜绝"写进 snapshots 目录却标成 release"这种自相矛盾的指针。
    Version 只校验"是单个安全目录名";版本目录是否已上线由调用方保证
    (publish 必须在 Complete-AtomicDir 成功之后才调用本函数)。
#>
function Set-LatestPointer {
    param(
        [Parameter(Mandatory = $true)][string]$ChannelRoot,
        [Parameter(Mandatory = $true)][ValidateSet('images')][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if ($Version -cnotmatch '^[0-9A-Za-z][0-9A-Za-z._-]*$' -or $Version.Contains('..')) {
        throw "latest 指针版本号非法(必须是单个目录名,不得含路径分隔符或 ..):'$Version'"
    }
    $channelFull = Resolve-ArtifactFullPath -Path $ChannelRoot
    $leaf = Split-Path -Leaf $channelFull
    $channel = if ($leaf -ceq 'snapshots') { 'snapshot' } elseif ($leaf -ceq 'releases') { 'release' } else { '' }
    if (-not $channel) {
        throw "ChannelRoot 必须是 <制品根>/snapshots 或 <制品根>/releases,实际:$channelFull"
    }

    $kindDir = Join-Path $channelFull $Kind
    if (-not [System.IO.Directory]::Exists($kindDir)) {
        [System.IO.Directory]::CreateDirectory($kindDir) | Out-Null
    }
    $dest = Join-Path $kindDir 'latest.json'
    # 临时名带 guid:共享盘上同 PID 的两个发布者若共用一个临时名,一方的 File.Move 会找不到文件,
    # 在版本目录已上线之后才失败
    $tmp = Join-Path $kindDir (".latest.json.tmp-" + $PID + "-" + [guid]::NewGuid().ToString('N'))

    $payload = [ordered]@{
        version      = $Version
        channel      = $channel
        # InvariantCulture 且分隔符加引号:自定义格式里的 ':' 是区域时间分隔符,fi-FI 等区域会输出 '.'
        # (与 publish_images.ps1 的 published_at 同一写法)
        published_at = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)
    }
    $json = ($payload | ConvertTo-Json -Depth 3) + "`n"
    try {
        [System.IO.File]::WriteAllText($tmp, $json, [System.Text.UTF8Encoding]::new($false))
        [System.IO.File]::Move($tmp, $dest, $true)
    }
    finally {
        if ([System.IO.File]::Exists($tmp)) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
    }
}

<#
.SYNOPSIS
    策划表摘要:对 TablesDir 顶层 *.json 算一个总 sha256,写进 build-info / release manifest。

.DESCRIPTION
    算法是契约(make_release / release_preflight 靠它比对,改了要同步改 artifacts_lib.tests.ps1):
      1. 取 TablesDir **顶层**、文件名以 ".json" 结尾(大小写敏感)的文件,含隐藏文件
      2. 按文件名序数排序
      3. 逐个拼 "<sha256 小写 hex>  <文件名>\n"
      4. 整段文本按 UTF-8(无 BOM)编码后再算 sha256

    只认 .json 是接口 B 的约定,**不是**"服务只读 json":C++ 节点在 TableDataFormat=binary 时读 .pb
    (cpp/libs/engine/config/config.cpp 的 UseProtoBinaryTables;本机 bin/etc/base_deploy_config.yaml 即为 binary),
    Go scene_manager 在 UseBinary=true 时也读 .pb。导表器同源生成 .json 与 .pb,正常流程两者同步变化,
    但摘要不覆盖 .pb:.pb 被单独改坏或漏生成时 tables_sha256 不变,追溯不到实际加载的表。
    要纳入 .pb 必须改接口 B,并由 publish / make_release / release_preflight 各方同步改测试,不能在这里单方面改算法。
    大小写敏感 + 序数排序保证 Windows 与 Linux 上同一份表算出同一个摘要。
    目录不存在或一个 json 都没有时抛异常:发布记录里出现"0 张表"只会是表生成步骤漏跑。

.OUTPUTS
    @{ Sha256 = <64 位小写 hex>; FileCount = <int> }
#>
function Get-TablesDigest {
    param([Parameter(Mandatory = $true)][string]$TablesDir)

    $dirFull = Resolve-ArtifactFullPath -Path $TablesDir
    if (-not [System.IO.Directory]::Exists($dirFull)) { throw "策划表目录不存在:$dirFull" }
    $names = New-Object System.Collections.Generic.List[string]
    foreach ($f in Get-ChildItem -LiteralPath $dirFull -File -Force) {
        if ($f.Name.EndsWith('.json', [System.StringComparison]::Ordinal)) { $names.Add($f.Name) }
    }
    if ($names.Count -eq 0) { throw "策划表目录下没有 *.json(表生成步骤漏跑?):$dirFull" }

    [string[]]$sorted = $names.ToArray()
    [System.Array]::Sort($sorted, [System.StringComparer]::Ordinal)

    $sb = New-Object System.Text.StringBuilder
    foreach ($name in $sorted) {
        $hash = Get-ArtifactFileSha256 -Path (Join-Path $dirFull $name)
        [void]$sb.Append($hash).Append('  ').Append($name).Append("`n")
    }

    $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($sb.ToString())
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $digest = [System.BitConverter]::ToString($sha.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $sha.Dispose()
    }
    return @{ Sha256 = $digest; FileCount = $sorted.Count }
}
