#requires -Version 7
param([string]$CheckerPath = '', [string]$ClangQueryPath = '')
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$repoRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
. (Join-Path $PSScriptRoot '../lib/no_raw_pointer_project.ps1')
if (-not $CheckerPath) { $CheckerPath = Join-Path $repoRoot 'cpp/plugin/build/Release/no_raw_ptr_check.exe' }
if (-not (Test-Path -LiteralPath $CheckerPath)) { throw "检查器尚未编译: $CheckerPath" }
$fixtureRoot = Join-Path $repoRoot ('build/tests/no-raw-pointer-check-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
foreach ($directory in @('third_party/vendor', 'Third_Party/vendor', 'cpp/libs/engine/muduo_windows', 'third_party_tools', 'cpp/libs/engine/muduo_windows_adapter')) {
    $vendorDir = Join-Path $fixtureRoot $directory
    New-Item -ItemType Directory -Path $vendorDir -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $vendorDir 'vendor.h'), '#pragma once' + "`nstruct Vendor { int* internal; };")
}
$cases = @(
    @{ Name = '合法成员'; File = 'good.cpp'; Source = 'struct Good { int number; };'; Exit = 0 },
    @{ Name = '裸指针成员'; File = 'raw.cpp'; Source = 'struct Bad { int* pointer; };'; Exit = 1 },
    @{ Name = '第三方头内部成员不检查'; File = 'vendor.cpp'; Source = '#include "third_party/vendor/vendor.h"'; Exit = 0 },
    @{ Name = '第三方目录大小写兼容'; File = 'vendor_case.cpp'; Source = '#include "Third_Party/vendor/vendor.h"'; Exit = 0 },
    @{ Name = '内嵌 muduo 库头不检查'; File = 'muduo.cpp'; Source = '#include "cpp/libs/engine/muduo_windows/vendor.h"'; Exit = 0 },
    @{ Name = '项目持有第三方类型仍须检查'; File = 'business.cpp'; Source = '#include "third_party/vendor/vendor.h"' + "`nstruct Business { Vendor* borrowed; };"; Exit = 1; Member = 'borrowed' },
    @{ Name = '名字含第三方字样的自有文件仍须检查'; File = 'third_party_adapter.cpp'; Source = 'struct Adapter { int* value; };'; Exit = 1 },
    @{ Name = '第三方近似目录仍须检查'; File = 'vendor_adapter.cpp'; Source = '#include "third_party_tools/vendor.h"'; Exit = 1 },
    @{ Name = 'muduo 近似目录仍须检查'; File = 'muduo_adapter.cpp'; Source = '#include "cpp/libs/engine/muduo_windows_adapter/vendor.h"'; Exit = 1 },
    @{ Name = '解析失败不得误报通过'; File = 'broken.cpp'; Source = '#include "missing_fixture_header.h"'; Exit = 2 }
)
foreach ($case in $cases) {
    $path = Join-Path $fixtureRoot $case.File
    [IO.File]::WriteAllText($path, $case.Source)
    $output = (& $CheckerPath $path 2>&1 | Out-String)
    $actual = $LASTEXITCODE
    if ($actual -ne $case.Exit) { throw "$($case.Name): 期望退出码 $($case.Exit)，实际 $actual。输出: $output" }
    if ($case.Member -and ($output -notmatch "raw pointer member '$($case.Member)'" -or $output -match "raw pointer member 'internal'")) {
        throw "必须只报告业务成员，不能报告第三方内部成员: $output"
    }
    Write-Host "PASS: $($case.Name)"
    if ($ClangQueryPath) {
        $queryOutput = (& $ClangQueryPath -f (Join-Path $repoRoot 'cpp/plugin/no_raw_ptr_matcher.cq') $path -- -xc++ -std=c++20 2>&1 | Out-String)
        $queryExit = $LASTEXITCODE
        $queryResult = Get-ClangQueryExitCode -OutputText $queryOutput -ProcessExitCode $queryExit
        if ($queryResult -ne $case.Exit) { throw "clang-query $($case.Name): 期望 $($case.Exit)，实际 $queryResult。输出: $queryOutput" }
        if ($case.Member -and ($queryOutput -notmatch [regex]::Escape($case.Member) -or [regex]::Matches($queryOutput, '"root" binds here').Count -ne 1)) {
            throw "clang-query 必须只报告业务成员: $queryOutput"
        }
        Write-Host "PASS: clang-query $($case.Name)"
    }
}
Write-Host "PASS: $($cases.Count) 个检查器行为用例。"

# 通过真实构建钩子验证发现、响应文件引用(含空格路径)和成功缓存。
$hookRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg checker smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $hookRoot | Out-Null
$hookSource = Join-Path $hookRoot 'main.cpp'
[IO.File]::WriteAllText($hookSource, 'struct Good { int value; };')
& "$PSScriptRoot/../check_no_raw_pointer_member.ps1" -ProjectDir $hookRoot -ProjectName setup_smoke
$marker = Join-Path $hookRoot '.no_raw_ptr_check_ok'
if (-not (Test-Path -LiteralPath $marker)) { throw '合法代码的构建钩子未生成成功缓存。' }
Write-Host 'PASS: 构建钩子通过并缓存(含空格路径)。'
[IO.File]::WriteAllText($hookSource, 'struct Bad { int* pointer; };')
[IO.File]::SetLastWriteTimeUtc($hookSource, [DateTime]::UtcNow.AddSeconds(1))
& "$PSScriptRoot/../check_no_raw_pointer_member.ps1" -ProjectDir $hookRoot -ProjectName setup_smoke
if ($LASTEXITCODE -ne 1 -or (Test-Path -LiteralPath $marker)) { throw '裸指针成员必须被钩子拒绝并移除旧成功缓存。' }
Write-Host 'PASS: 构建钩子拒绝裸指针成员并清除旧缓存。'
# 上面的非零退出码是预期断言，不向调用准备脚本泄漏为失败状态。
$global:LASTEXITCODE = 0
