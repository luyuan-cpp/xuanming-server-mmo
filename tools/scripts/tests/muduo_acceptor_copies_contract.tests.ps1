#requires -Version 7
<#
.SYNOPSIS
    muduo Acceptor 的 accept 失败降级:三份副本之间的静态契约(2026-11-12)。

.DESCRIPTION
    只读仓库里的源码,不编译、不访问网络。改动本身见 docs/design/gate-connection-admission-control.md 的 G6 / G7 / G8 与
    tools/archived/muduo_linux_overlay/muduo/net/Acceptor.cc 的文件头。

    三份副本各有用处,哪一份单独改了都不会当场报错:
      - cpp/libs/engine/muduo_windows/src/muduo/net/   Windows 编进 muduo.lib;
      - third_party/muduo/muduo/net/                   业务代码与测试 #include "muduo/net/Acceptor.h" 实际命中的头文件镜像;
      - tools/archived/muduo_linux_overlay/muduo/net/  Linux SDK 构建时覆盖进 muduo-linux(生产用的就是这份)。
    Acceptor 加了成员:头文件镜像与 Windows 编译件不一致时,业务按一种类布局分配、库按另一种布局构造,
    链接照样成功,运行期才堆损坏。Linux 那份有平台差异(没有 WIN32 分支),不能逐字节比,比的是去掉注释后的代码。

    守住的东西:
      - A1 Windows 编译件与头文件镜像的 Acceptor.h、Acceptor.cc 逐字节一致;
      - A2 Linux 覆盖件与 Windows 编译件的 Acceptor 类体(去注释、去空行后)一致:公开接口与成员布局相同;
      - A3 两边的 classifyAcceptFailure 函数体(去注释后)一致:同一个 errno 在两个平台上走同一条降级路径;
      - A4 三份 SocketsOps.cc 的 sockets::accept 里,资源耗尽的四个 errno 都在「交回调用方」那一段,不在 LOG_FATAL 那一段;
      - A5 比较函数自检:去注释归一化不会把代码差异也抹掉,保证本测试真能红。

    CI:.github/workflows/deploy-config-tests.yml 在这几个文件有改动时触发,一级目录下的本脚本自动执行。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/muduo_acceptor_copies_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:WindowsDir = Join-Path $script:RepoRoot 'cpp' 'libs' 'engine' 'muduo_windows' 'src' 'muduo' 'net'
$script:MirrorDir = Join-Path $script:RepoRoot 'third_party' 'muduo' 'muduo' 'net'
$script:LinuxDir = Join-Path $script:RepoRoot 'tools' 'archived' 'muduo_linux_overlay' 'muduo' 'net'

function Read-SourceText {
    param([Parameter(Mandatory = $true)][string]$Path)
    return ([IO.File]::ReadAllText($Path)) -replace "`r`n", "`n"
}

# 去掉 // 注释与空行,压掉行首尾空白。两份副本的注释措辞不同(各自说明自己是哪一份),比较的只是代码。
# 被比的片段里没有含 "//" 的字符串字面量。
function ConvertTo-CodeOnly {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $lines = foreach ($line in ($Text -split "`n")) {
        $code = ($line -replace '//.*$', '').Trim()
        if ($code.Length -gt 0) { $code }
    }
    return ($lines -join "`n")
}

# Acceptor.h 里 `class Acceptor` 的类体。
function Get-AcceptorClassBody {
    param([Parameter(Mandatory = $true)][string]$HeaderText)
    $m = [regex]::Match($HeaderText, '(?ms)^class Acceptor : noncopyable\n\{\n(?<body>.*?)^\};')
    if (-not $m.Success) { throw '找不到 class Acceptor 的类体' }
    return $m.Groups['body'].Value
}

# Acceptor.cc 里 classifyAcceptFailure 的函数体。
function Get-ClassifyBody {
    param([Parameter(Mandatory = $true)][string]$SourceText)
    $m = [regex]::Match($SourceText, '(?ms)^Acceptor::AcceptFailure Acceptor::classifyAcceptFailure\(int savedErrno\)\n\{\n(?<body>.*?)^\}')
    if (-not $m.Success) { throw '找不到 Acceptor::classifyAcceptFailure' }
    return $m.Groups['body'].Value
}

# SocketsOps.cc 的 sockets::accept 里 errno switch 的两段:交回调用方的一段(LOCAL DELTA begin..end)与之后 LOG_FATAL 的一段。
function Get-AcceptErrnoSections {
    param([Parameter(Mandatory = $true)][string]$SourceText)
    $fn = [regex]::Match($SourceText, '(?ms)^int sockets::accept\(int sockfd, struct sockaddr_in6\* addr\)\n\{\n(?<body>.*?)^\}')
    if (-not $fn.Success) { throw '找不到 sockets::accept' }
    $m = [regex]::Match($fn.Groups['body'].Value, '(?s)// LOCAL DELTA begin(?<handedBack>.*?)// LOCAL DELTA end(?<fatal>.*)$')
    if (-not $m.Success) { throw 'sockets::accept 里找不到 LOCAL DELTA begin / end 标记' }
    return [pscustomobject]@{ HandedBack = $m.Groups['handedBack'].Value; Fatal = $m.Groups['fatal'].Value }
}

Test-Case 'A1 Windows 编译件与头文件镜像的 Acceptor.h / Acceptor.cc 逐字节一致' {
    foreach ($name in @('Acceptor.h', 'Acceptor.cc')) {
        $built = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $script:WindowsDir $name)).Hash
        $mirror = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $script:MirrorDir $name)).Hash
        Assert-Equal -Expected $built -Actual $mirror -Because "third_party/muduo/muduo/net/$name 是业务 include 命中的镜像,必须与 muduo_windows 的编译件逐字节一致(类布局不一致时运行期才出错)"
    }
}

Test-Case 'A2 Linux 覆盖件与 Windows 编译件的 Acceptor 类体(去注释后)一致' {
    $windows = ConvertTo-CodeOnly -Text (Get-AcceptorClassBody -HeaderText (Read-SourceText (Join-Path $script:WindowsDir 'Acceptor.h')))
    $linux = ConvertTo-CodeOnly -Text (Get-AcceptorClassBody -HeaderText (Read-SourceText (Join-Path $script:LinuxDir 'Acceptor.h')))
    Assert-Equal -Expected $windows -Actual $linux -Because 'Acceptor 的公开接口与成员在两个平台上必须相同(node.cpp 两边都读 Acceptor::exhaustionCounts)'
    Assert-Match -Text $linux -Pattern '(?m)^bool listenning\(\) const \{ return listening\(\); \}$' -Because '覆盖件要保留旧拼写别名:muduo-linux 的 gitlink 在 v2.0.2(listenning)与 v2.0.3(listening)之间翻转过'
}

Test-Case 'A3 两边的 classifyAcceptFailure 函数体(去注释后)一致,资源耗尽归到可降级的两类' {
    $windows = ConvertTo-CodeOnly -Text (Get-ClassifyBody -SourceText (Read-SourceText (Join-Path $script:WindowsDir 'Acceptor.cc')))
    $linux = ConvertTo-CodeOnly -Text (Get-ClassifyBody -SourceText (Read-SourceText (Join-Path $script:LinuxDir 'Acceptor.cc')))
    Assert-Equal -Expected $windows -Actual $linux -Because '同一个 errno 在两个平台上要走同一条降级路径'
    Assert-Match -Text $linux -Pattern '(?s)case EMFILE:\ncase ENFILE:\nreturn AcceptFailure::kDescriptorsExhausted;' -Because 'fd 用尽(进程级与全机级)走让位 / 暂停'
    Assert-Match -Text $linux -Pattern '(?s)case ENOBUFS:\ncase ENOMEM:\nreturn AcceptFailure::kMemoryExhausted;' -Because '内核缓冲 / 内存不足走暂停'
}

Test-Case 'A4 三份 SocketsOps.cc:资源耗尽的 errno 交回调用方,不再 LOG_FATAL' {
    foreach ($dir in @($script:WindowsDir, $script:MirrorDir, $script:LinuxDir)) {
        $relative = [IO.Path]::GetRelativePath($script:RepoRoot, (Join-Path $dir 'SocketsOps.cc'))
        $sections = Get-AcceptErrnoSections -SourceText (Read-SourceText (Join-Path $dir 'SocketsOps.cc'))
        # 这一段的说明注释里提到了原来的 LOG_SYSERR / LOG_FATAL,所以先去注释再判。
        $handedBack = ConvertTo-CodeOnly -Text $sections.HandedBack
        $fatal = ConvertTo-CodeOnly -Text $sections.Fatal
        foreach ($name in @('EMFILE', 'ENFILE', 'ENOBUFS', 'ENOMEM')) {
            Assert-Match -Text $handedBack -Pattern ("(?m)^case {0}:" -f $name) -Because "${relative}:$name 要在交回调用方的那一段"
            Assert-NotMatch -Text $fatal -Pattern ("(?m)^case {0}:" -f $name) -Because "${relative}:$name 不能再落进 LOG_FATAL(连接洪峰下会把整个进程 abort)"
        }
        Assert-NotMatch -Text $handedBack -Pattern 'LOG_(SYSERR|FATAL|SYSFATAL)' -Because "${relative}:交回调用方的那一段不打日志、不终止进程(日志由 Acceptor 限频打)"
        Assert-Match -Text $handedBack -Pattern '(?m)^errno = savedErrno;$' -Because "${relative}:交回调用方时要把 errno 原样放回(Acceptor 靠它分类)"
        Assert-Match -Text $fatal -Pattern 'LOG_FATAL << "unexpected error of ::accept "' -Because "${relative}:编程错误(EBADF 等)仍然 LOG_FATAL"
    }
}

Test-Case 'A5 自检:去注释归一化只抹注释与空白,不抹代码差异' {
    $a = "  int x = 1; // 注释甲`n`n  return x;`n"
    $b = "int x = 1;   // 注释乙`nreturn x;"
    $c = "int x = 2;`nreturn x;"
    Assert-Equal -Expected (ConvertTo-CodeOnly -Text $a) -Actual (ConvertTo-CodeOnly -Text $b) -Because '只差注释、空行与缩进应判一致'
    Assert-True -Condition ((ConvertTo-CodeOnly -Text $a) -cne (ConvertTo-CodeOnly -Text $c)) -Because '代码不同必须判不一致'
}

exit (Complete-TestRun -SuiteName 'muduo_acceptor_copies_contract')
