<#
.SYNOPSIS
    构建并运行 C++ 单元测试工程,输出一张汇总表。

.DESCRIPTION
    仓库里 20 多个 gtest 工程长期没人整体跑过 —— 它们没进 game.sln 的构建配置,
    也没有统一入口,于是一个个悄悄烂掉(引用已删除的头/类、库名停在 release 变体、
    相对路径少一级)。本脚本就是那个统一入口:

      pwsh tools/scripts/run_cpp_tests.ps1            # 只跑(用已有 exe)
      pwsh tools/scripts/run_cpp_tests.ps1 -Build     # 先串行编译再跑
      pwsh tools/scripts/run_cpp_tests.ps1 -Filter aoi

    退出码:全绿 0,有失败/超时 1。CI 与本地都可直接用。

    两个容易踩的点已在脚本里处理:
      1) 部分 exe 运行时依赖 zlibd.dll / rdkafka*.dll,而它们只在 bin/ 下,
         测试输出目录 build/cpp/tests/ 没有 —— 缺了会以 0xC0000135 静默退出,
         没有任何 gtest 输出。脚本每次运行前把这几个 DLL 同步过去。
      2) 读配表的测试要能找到 bin/etc,脚本以仓库根为工作目录启动,
         由测试自身逐级上溯定位(见 turn_battle_engine_test 的真表契约用例)。

.NOTES
    MSBuild 必须串行 /m:1,并发会报假的 C1041/LNK1104(见 CLAUDE.md)。
#>
[CmdletBinding()]
param(
    [switch]$Build,
    [string]$Filter = '',
    [int]$TimeoutSeconds = 120,
    [string]$Configuration = 'Debug'
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$testOutDir = Join-Path $repoRoot 'build\cpp\tests'

# 能编能跑的工程。不在表里的是「被测代码已被删除」的死工程,修不了配置就能救活,
# 需要连测试源码一起重写或直接删,别贸然加进来(当前:scene_test = SceneSystem/
# SceneNodeStateSystem/SceneNodeSelectorSystem 全没了;team_test = team_system.h 没了;
# consistent_hash_node_test = ConsistentHashNode 没了;redis_test / mrediscli_test =
# 引用 common/src/pb/pbc 那棵已删的 proto 树)。
$projects = [ordered]@{
    'agones_lifecycle_test'    = 'cpp\tests\agones_lifecycle_test\agones_lifecycle_test.vcxproj'
    'aoi_test'                 = 'cpp\tests\aoi_test\aoi_test.vcxproj'
    'bag_test'                 = 'cpp\tests\bag_test\bag_test.vcxproj'
    'buff_test'                = 'cpp\tests\buff_test\buff_test.vcxproj'
    'configuration_table_test' = 'cpp\tests\configuration_table_test\configuration_table_test.vcxproj'
    'cool_down_time_test'      = 'cpp\tests\cool_down_time_test\cool_down_time_test.vcxproj'
    'cross_zone_test'          = 'cpp\tests\cross_zone_test\cross_zone_test.vcxproj'
    'currency_test'            = 'cpp\tests\currency_test\currency_test.vcxproj'
    'message_limiter_test'     = 'cpp\tests\message_limiter_test\message_limiter.vcxproj'
    'missions_test'            = 'cpp\tests\missions_test\missions.vcxproj'
    'node_sequence_test'       = 'cpp\tests\node_sequence_test\server_sequence.vcxproj'
    'proto_field_checker_test' = 'cpp\tests\proto_field_checker_test\proto_field_checker_test.vcxproj'
    'readfile2string_test'     = 'cpp\tests\readfile2string_test\readfile2string.vcxproj'
    'reward_test'              = 'cpp\tests\reward_test\reward.vcxproj'
    'skill_test'               = 'cpp\tests\skill_test\skill_test.vcxproj'
    'snow_flake_test'          = 'cpp\tests\snow_flake_test\snow_flake.vcxproj'
    'time_meter_test'          = 'cpp\tests\time_meter_test\time_meter_test.vcxproj'
    'time_util_test'           = 'cpp\tests\time_util_test\time_util_test.vcxproj'
    'timer_destroy_test'       = 'cpp\tests\timer_destroy_test\timer_destroy_test.vcxproj'
    'timer_queue_unit_test'    = 'cpp\tests\timer_queue_unit_test\timer_queue_unit_test.vcxproj'
    'turn_battle_engine_test'  = 'cpp\tests\turn_battle_engine_test\turn_battle_engine_test.vcxproj'
}

if ($Filter) {
    $keys = @($projects.Keys | Where-Object { $_ -like "*$Filter*" })
    if (-not $keys) { Write-Error "没有匹配 '$Filter' 的测试工程"; exit 2 }
    $filtered = [ordered]@{}
    foreach ($k in $keys) { $filtered[$k] = $projects[$k] }
    $projects = $filtered
}

function Resolve-MSBuild {
    $candidates = @(
        'D:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe',
        'C:\Program Files\Microsoft Visual Studio\2022\Enterprise\MSBuild\Current\Bin\MSBuild.exe',
        'C:\Program Files\Microsoft Visual Studio\2022\Professional\MSBuild\Current\Bin\MSBuild.exe',
        'C:\Program Files\Microsoft Visual Studio\2022\Community\MSBuild\Current\Bin\MSBuild.exe'
    )
    foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
    $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
    if (Test-Path $vswhere) {
        $p = & $vswhere -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe' | Select-Object -First 1
        if ($p) { return $p }
    }
    throw '找不到 MSBuild.exe,请用 -Verbose 查看候选路径或自行设置'
}

$results = [System.Collections.Generic.List[object]]::new()

if ($Build) {
    $msbuild = Resolve-MSBuild
    Write-Host "MSBuild: $msbuild" -ForegroundColor DarkGray
    foreach ($name in $projects.Keys) {
        $proj = Join-Path $repoRoot $projects[$name]
        if (-not (Test-Path $proj)) {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'FAIL'; 结果 = "缺工程文件 $($projects[$name])" })
            continue
        }
        # 串行 /m:1:并发会报假的 C1041/LNK1104
        $log = & $msbuild $proj /m:1 /p:Configuration=$Configuration /p:Platform=x64 /nologo /v:minimal 2>&1
        $err = $log | Select-String -Pattern 'error [A-Z]+[0-9]+' | Select-Object -First 1
        if ($err) {
            $msg = ($err.ToString().Trim() -replace '\s+', ' ')
            if ($msg.Length -gt 160) { $msg = $msg.Substring(0, 160) }
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'FAIL'; 结果 = $msg })
        }
    }
}

# 运行期 DLL:缺了会以 0xC0000135 静默退出,连 gtest 头一行都不会打
foreach ($dll in @('zlibd.dll', 'rdkafka.dll', 'rdkafka++.dll')) {
    $src = Join-Path $repoRoot "bin\$dll"
    if ((Test-Path $src) -and (Test-Path $testOutDir)) {
        Copy-Item $src $testOutDir -Force -ErrorAction SilentlyContinue
    }
}

$failed = 0
foreach ($name in $projects.Keys) {
    if ($results | Where-Object { $_.名称 -eq $name -and $_.构建 -eq 'FAIL' }) { $failed++; continue }
    $exe = Join-Path $testOutDir "$name.exe"
    if (-not (Test-Path $exe)) {
        $results.Add([pscustomobject]@{ 名称 = $name; 构建 = '-'; 结果 = '无 exe(先跑 -Build)' })
        $failed++
        continue
    }
    $stdout = [IO.Path]::GetTempFileName()
    $stderr = "$stdout.err"
    $proc = Start-Process -FilePath $exe -WorkingDirectory $repoRoot -NoNewWindow -PassThru `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    if (-not $proc.WaitForExit($TimeoutSeconds * 1000)) {
        try { $proc.Kill() } catch { }
        $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "超时 ${TimeoutSeconds}s 被杀" })
        $failed++
    }
    else {
        $out = Get-Content $stdout -Raw
        if ($null -eq $out) { $out = '' }
        $ran = [regex]::Match($out, 'Running (\d+) test')
        $pass = [regex]::Match($out, '\[  PASSED  \] (\d+)')
        $fail = [regex]::Match($out, '\[  FAILED  \] (\d+)')
        if (-not $ran.Success) {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "退出码 $($proc.ExitCode),无 gtest 输出" })
            $failed++
        }
        elseif ($proc.ExitCode -ne 0 -and -not $fail.Success) {
            # 跑了用例、没有 [FAILED] 却非 0 退出:典型是用例中途 LOG_FATAL / 崩溃,
            # gtest 来不及打 PASSED 汇总。不能按「没失败」放行(readfile2string_test 就是这样假绿的)。
            $lastRun = ([regex]::Matches($out, '\[ RUN      \] ([A-Za-z0-9_]+\.[A-Za-z0-9_]+)') | Select-Object -Last 1)
            $where = if ($lastRun) { $lastRun.Groups[1].Value } else { '?' }
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "退出码 $($proc.ExitCode),疑似崩在 $where(无 PASSED 汇总)" })
            $failed++
        }
        elseif (-not $pass.Success) {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "退出码 0 但无 PASSED 汇总,输出异常" })
            $failed++
        }
        elseif ($fail.Success) {
            $names = ([regex]::Matches($out, '\[  FAILED  \] ([A-Za-z0-9_]+\.[A-Za-z0-9_]+)') |
                ForEach-Object { $_.Groups[1].Value } | Select-Object -Unique) -join ', '
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "$($pass.Groups[1].Value)/$($ran.Groups[1].Value) 过,挂:$names" })
            $failed++
        }
        else {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "$($pass.Groups[1].Value)/$($ran.Groups[1].Value) 全过" })
        }
    }
    Remove-Item $stdout, $stderr -ErrorAction SilentlyContinue
}

$results | Format-Table -AutoSize
if ($failed -gt 0) {
    Write-Host "有 $failed 个测试工程未通过" -ForegroundColor Red
    exit 1
}
Write-Host "全部通过($($projects.Count) 个工程)" -ForegroundColor Green
exit 0
