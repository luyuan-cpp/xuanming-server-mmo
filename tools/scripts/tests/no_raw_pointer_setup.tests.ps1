#requires -Version 7
# 用隔离目录验证下载入口的失败行为，不联网、不编译、不修改现有开发库。
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
$fixtureRoot = Join-Path $repoRoot ('build/tests/no-raw-pointer-setup-' + [guid]::NewGuid().ToString('N'))
$scriptDir = Join-Path $fixtureRoot 'tools/scripts/third_party'
New-Item -ItemType Directory -Path $scriptDir -Force | Out-Null
$entry = Join-Path $scriptDir 'setup_no_raw_pointer_check.ps1'
Copy-Item "$PSScriptRoot/../third_party/setup_no_raw_pointer_check.ps1" $entry
$passed = 0

function Assert-Throws([scriptblock]$Action, [string]$Expected) {
    $message = ''
    try { & $Action } catch { $message = $_.Exception.Message }
    if ($message -notlike "*$Expected*") { throw "期望包含 '$Expected' 的错误，实际: $message" }
}

& $entry -DryRun
if (Test-Path "$fixtureRoot/build") { throw 'DryRun 不得创建缓存目录。' }
$passed++

Assert-Throws { & $entry -DownloadOnly -LlvmRoot "$fixtureRoot/missing-sdk" } '不是完整 LLVM/Clang 开发库'
if (Test-Path "$fixtureRoot/build") { throw '指定 SDK 不完整时不得自动下载。' }
$passed++

$cacheRoot = Join-Path $fixtureRoot 'build/deps'
New-Item -ItemType Directory -Path $cacheRoot -Force | Out-Null
$archive = Join-Path $cacheRoot 'clang+llvm-23.1.1-x86_64-pc-windows-msvc.tar.xz'
[IO.File]::WriteAllText($archive, '这是一个损坏的开发包，不允许解压或执行。')
$before = (Get-FileHash $archive).Hash
Assert-Throws { & $entry -DownloadOnly } '缓存包 SHA256 不匹配'
if ((Get-FileHash $archive).Hash -ne $before) { throw '失败时不得覆盖现有缓存。' }
if (Test-Path "$cacheRoot/llvm-23.1.1") { throw '校验失败时不得安装开发库。' }
$passed++

$sdkRoot = Join-Path $cacheRoot 'llvm-23.1.1'
New-Item -ItemType Directory -Path $sdkRoot -Force | Out-Null
Assert-Throws { & $entry -DownloadOnly } '开发库目录不完整或版本标记不符'
$passed++

foreach ($relative in @('lib/cmake/llvm/LLVMConfig.cmake', 'lib/cmake/clang/ClangConfig.cmake',
    'include/clang/Tooling/Tooling.h', 'lib/clangTooling.lib')) {
    $path = Join-Path $sdkRoot $relative
    New-Item -ItemType Directory -Path (Split-Path $path -Parent) -Force | Out-Null
    [IO.File]::WriteAllText($path, '缓存命中测试用占位文件，不用于编译。')
}
[IO.File]::WriteAllText((Join-Path $sdkRoot '.archive-sha256'),
    'c54ac8146b420fe72e11e6fdd56498d6818011ad23267196b6ab37b5ac9264c3')
# 已完成的开发库应直接复用，不再读取上面的损坏归档或联网。
& $entry -DownloadOnly
$passed++

$previousLlvmRoot = $env:LLVM_ROOT
try {
    $env:LLVM_ROOT = $sdkRoot
    & $entry -DownloadOnly
    $passed++
} finally { $env:LLVM_ROOT = $previousLlvmRoot }
Write-Host "PASS: $passed 个下载入口用例。测试目录: $fixtureRoot"
