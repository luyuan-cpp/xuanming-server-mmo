#requires -Version 7
<#
.SYNOPSIS
    Hiredis 适配器三份副本逐字节一致的静态契约。

.DESCRIPTION
    只读仓库里的源码，不编译、不访问网络。来历与规则见
    docs/design/redis-async-client-retry-fix.md「2026-10-30：三份 Hiredis 适配器恢复逐字节一致」。

    三份副本各有用处，哪一份单独改了都不会当场报错：
      - cpp/libs/engine/muduo_windows/src/contrib/hiredis/   Windows 编进 muduo.lib；
      - tools/archived/muduo_linux_overlay/contrib/hiredis/  Linux SDK 构建时覆盖进 muduo-linux；
      - third_party/muduo/contrib/hiredis/                   业务代码 #include <muduo/contrib/hiredis/Hiredis.h> 实际命中的头文件镜像。
    头文件镜像与 Windows 编译用的头不一致时，业务按一种布局编译、库按另一种布局编译，链接照样成功，运行期才出错。

    守住的东西：
      - H1 三处都有 Hiredis.h 与 Hiredis.cc；
      - H2 三份 Hiredis.h 逐字节一致，三份 Hiredis.cc 逐字节一致（.gitattributes 把这两类文件钉成 LF，换行不会造成误报）；
      - H3 不调用随 muduo 版本改名的端口访问器：muduo-linux 的 gitlink 在 toPort()（b55b0ac）与 port()（f1fc77e）之间翻转过，
           只能用两版共有的 networkToHost16(portNetEndian())；
      - H4 比较函数自检：内容相同判一致，差一个字节（含只差换行）判不一致，保证本测试真能红。

    CI：.github/workflows/deploy-config-tests.yml 在三处目录有改动时触发，一级目录下的本脚本自动执行。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/hiredis_adapter_copies_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
# 路径按段拼，Linux pwsh 下同样可跑。第一份是比较基准，另外两份逐字节与它比。
$script:CopyDirs = @(
    (Join-Path $script:RepoRoot 'cpp' 'libs' 'engine' 'muduo_windows' 'src' 'contrib' 'hiredis'),
    (Join-Path $script:RepoRoot 'tools' 'archived' 'muduo_linux_overlay' 'contrib' 'hiredis'),
    (Join-Path $script:RepoRoot 'third_party' 'muduo' 'contrib' 'hiredis')
)
$script:FileNames = @('Hiredis.h', 'Hiredis.cc')

function Get-RepoRelative {
    param([Parameter(Mandatory = $true)][string]$Path)
    return [IO.Path]::GetRelativePath($script:RepoRoot, $Path)
}

# 输出与第一份内容不同的副本，每份一行说明；没有输出表示全部一致。按原始字节比较，不做换行归一。
function Get-CopyMismatches {
    param([Parameter(Mandatory = $true)][string[]]$Paths)
    $reference = (Get-FileHash -Algorithm SHA256 -LiteralPath $Paths[0]).Hash
    foreach ($path in ($Paths | Select-Object -Skip 1)) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash
        if ($hash -ne $reference) {
            '{0} (sha256 {1}; reference {2} is {3})' -f (Get-RepoRelative $path), $hash, (Get-RepoRelative $Paths[0]), $reference
        }
    }
}

Test-Case "H1 every copy directory has Hiredis.h and Hiredis.cc" {
    foreach ($dir in $script:CopyDirs) {
        foreach ($name in $script:FileNames) {
            $path = Join-Path $dir $name
            Assert-True -Condition (Test-Path -LiteralPath $path -PathType Leaf) -Because "missing $(Get-RepoRelative $path)"
        }
    }
}

foreach ($name in $script:FileNames) {
    Test-Case "H2 the three copies of $name are byte-identical" {
        $paths = @($script:CopyDirs | ForEach-Object { Join-Path $_ $name })
        $mismatches = @(Get-CopyMismatches -Paths $paths)
        Assert-True -Condition ($mismatches.Count -eq 0) `
            -Because ("all three copies of $name must stay byte-identical; when one changes, copy it to the other two: " + ($mismatches -join '; '))
    }
}

Test-Case "H3 Hiredis.cc does not call the port accessor that muduo renamed between versions" {
    $source = [IO.File]::ReadAllText((Join-Path $script:CopyDirs[0] 'Hiredis.cc'))
    Assert-NotMatch -Text $source -Pattern '\.(toPort|port)\(\)' `
        -Because 'muduo-linux has flipped between toPort() (b55b0ac) and port() (f1fc77e); use networkToHost16(portNetEndian()), which both have'
}

Test-Case "H4 the comparison reports a copy that differs by one byte" {
    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ('hiredis-copies-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tempDir | Out-Null
    try {
        $reference = Join-Path $tempDir 'reference.cc'
        $same = Join-Path $tempDir 'same.cc'
        $changed = Join-Path $tempDir 'changed.cc'
        $crlf = Join-Path $tempDir 'crlf.cc'
        [IO.File]::WriteAllText($reference, "int x = 1;`n")
        [IO.File]::WriteAllText($same, "int x = 1;`n")
        [IO.File]::WriteAllText($changed, "int x = 2;`n")
        [IO.File]::WriteAllText($crlf, "int x = 1;`r`n")

        $identical = @(Get-CopyMismatches -Paths @($reference, $same))
        Assert-Equal -Expected 0 -Actual $identical.Count -Because 'identical content must compare equal'
        $oneByte = @(Get-CopyMismatches -Paths @($reference, $same, $changed))
        Assert-Equal -Expected 1 -Actual $oneByte.Count -Because 'a one-byte difference must be reported exactly once'
        Assert-Match -Text $oneByte[0] -Pattern 'changed\.cc' -Because 'the report must name the copy that differs'
        $lineEnding = @(Get-CopyMismatches -Paths @($reference, $crlf))
        Assert-Equal -Expected 1 -Actual $lineEnding.Count -Because 'the rule is byte-identical, so a line-ending-only difference also counts'
    }
    finally {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

exit (Complete-TestRun -SuiteName 'hiredis_adapter_copies_contract')
