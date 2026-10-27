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
      pwsh tools/scripts/run_cpp_tests.ps1 -Filter muduo_grpc -LogDir build/verification/cpp-tests

    退出码:全绿 0,有失败/超时 1。CI 与本地都可直接用。

    两个容易踩的点已在脚本里处理:
      1) 部分 exe 运行时依赖 zlibd.dll / rdkafka*.dll,而它们只在 bin/ 下,
         测试输出目录 build/cpp/tests/ 没有 —— 缺了会以 0xC0000135 静默退出,
         没有任何 gtest 输出。脚本每次运行前把这几个 DLL 同步过去。
      2) 读配表的测试要能找到 bin/etc,脚本以仓库根为工作目录启动,
         由测试自身逐级上溯定位(见 turn_battle_engine_test 的真表契约用例)。

.PARAMETER LogDir
    保存每个测试工程的完整 stdout/stderr；省略时沿用临时文件清理。

.NOTES
    MSBuild 必须串行 /m:1,并发会报假的 C1041/LNK1104(见 CLAUDE.md)。
#>
[CmdletBinding()]
param(
    [switch]$Build,
    [string]$Filter = '',
    [int]$TimeoutSeconds = 120,
    # 当前统一输出目录只由 Debug|x64 工程保证；不把旧 Debug EXE 冒充 Release。
    [ValidateSet('Debug')][string]$Configuration = 'Debug',
    [string]$LogDir = ''
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$testOutDir = Join-Path $repoRoot 'build\cpp\tests'
if ($LogDir) {
    $LogDir = [IO.Path]::GetFullPath($LogDir)
    New-Item -ItemType Directory -Path $LogDir -Force | Out-Null
}

# 能编能跑的工程。不在表里的是「被测代码已被删除」的死工程,修不了配置就能救活,
# 需要连测试源码一起重写或直接删,别贸然加进来(当前:
# consistent_hash_node_test = ConsistentHashNode 没了;redis_test / mrediscli_test =
# 引用 common/src/pb/pbc 那棵已删的 proto 树)。
$projects = [ordered]@{
    'agones_lifecycle_test'    = 'cpp\tests\agones_lifecycle_test\agones_lifecycle_test.vcxproj'
    'aoi_delivery_test'        = 'cpp\tests\aoi_delivery_test\aoi_delivery_test.vcxproj'
    'aoi_test'                 = 'cpp\tests\aoi_test\aoi_test.vcxproj'
    'bag_test'                 = 'cpp\tests\bag_test\bag_test.vcxproj'
    'buff_test'                = 'cpp\tests\buff_test\buff_test.vcxproj'
    'client_endpoint_test'     = 'cpp\tests\client_endpoint_test\client_endpoint_test.vcxproj'
    'configuration_table_test' = 'cpp\tests\configuration_table_test\configuration_table_test.vcxproj'
    'cool_down_time_test'      = 'cpp\tests\cool_down_time_test\cool_down_time_test.vcxproj'
    'cross_zone_test'          = 'cpp\tests\cross_zone_test\cross_zone_test.vcxproj'
    'currency_test'            = 'cpp\tests\currency_test\currency_test.vcxproj'
    'ecs_context_test'         = 'cpp\tests\ecs_context_test\ecs_context_test.vcxproj'
    'kafka_command_test'       = 'cpp\tests\kafka_command_test\kafka_command_test.vcxproj'
    'message_limiter_test'     = 'cpp\tests\message_limiter_test\message_limiter.vcxproj'
    'missions_test'            = 'cpp\tests\missions_test\missions.vcxproj'
    'muduo_grpc_test'          = 'cpp\tests\muduo_grpc_test\muduo_grpc_test.vcxproj'
    'node_sequence_test'       = 'cpp\tests\node_sequence_test\server_sequence.vcxproj'
    'player_landing_test'      = 'cpp\tests\player_landing_test\player_landing_test.vcxproj'
    'proto_field_checker_test' = 'cpp\tests\proto_field_checker_test\proto_field_checker_test.vcxproj'
    'readfile2string_test'     = 'cpp\tests\readfile2string_test\readfile2string.vcxproj'
    'reward_test'              = 'cpp\tests\reward_test\reward.vcxproj'
    'rpc_controller_test'      = 'cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj'
    'routing_identity_test'    = 'cpp\tests\routing_identity_test\routing_identity_test.vcxproj'
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
    if (-not $keys) { Write-Error "No test projects match '$Filter'"; exit 2 }
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
        $buildExitCode = $LASTEXITCODE
        if ($LogDir) { $log | Set-Content -LiteralPath (Join-Path $LogDir "$name.build.log") -Encoding utf8 }
        $err = $log | Select-String -Pattern 'error [A-Z]+[0-9]+' | Select-Object -First 1
        if ($buildExitCode -ne 0 -or $err) {
            $msg = if ($err) { $err.ToString().Trim() -replace '\s+', ' ' } else { "MSBuild 退出码 $buildExitCode，禁止运行旧 EXE" }
            if ($msg.Length -gt 160) { $msg = $msg.Substring(0, 160) }
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'FAIL'; 结果 = $msg })
        }
    }
}

# 运行期 DLL:缺了会以 0xC0000135 静默退出,连 gtest 头一行都不会打。
# 独立依赖的 DLL 只从本仓库正式安装目录取用，避免 PATH 中的旧 SDK 掩盖缺项。
$dllSourceDirs = @('bin', 'third_party\protobuf\install_vs2026_dbg\bin', 'third_party\zlib\install_vs2026_dbg\bin')
foreach ($dll in @('zlibd.dll', 'rdkafka.dll', 'rdkafka++.dll')) {
    foreach ($dir in $dllSourceDirs) {
        $src = Join-Path $repoRoot (Join-Path $dir $dll)
        if ((Test-Path $src) -and (Test-Path $testOutDir)) {
            Copy-Item $src $testOutDir -Force
            break
        }
    }
}

$failed = 0
$gtestEnvironment = @{}
# 每个 EXE 应按自己的默认全集运行；同时隔离 flagfile、调试器及日志输出开关的外部副作用。
foreach ($variable in [Environment]::GetEnvironmentVariables('Process').Keys) {
    if ($variable -like 'GTEST_*') {
        $gtestEnvironment[$variable] = [Environment]::GetEnvironmentVariable($variable, 'Process')
    }
}
foreach ($variable in @('GTEST_FILTER', 'GTEST_REPEAT')) {
    if (-not $gtestEnvironment.ContainsKey($variable)) { $gtestEnvironment[$variable] = $null }
}
try {
    foreach ($variable in $gtestEnvironment.Keys) { [Environment]::SetEnvironmentVariable($variable, $null, 'Process') }
    $env:GTEST_FILTER = '*'
    $env:GTEST_REPEAT = '1'
    foreach ($name in $projects.Keys) {
        if ($results | Where-Object { $_.名称 -eq $name -and $_.构建 -eq 'FAIL' }) { $failed++; continue }
        $exe = Join-Path $testOutDir "$name.exe"
        if (-not (Test-Path $exe)) {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = '-'; 结果 = '无 exe(先跑 -Build)' })
            $failed++
            continue
        }
        if ($LogDir) {
            # 验证与失败交接需要完整 gtest 输出，指定目录时保留原始日志。
            $stdout = Join-Path $LogDir "$name.stdout.log"
            $stderr = Join-Path $LogDir "$name.stderr.log"
        }
        else {
            $stdout = [IO.Path]::GetTempFileName()
            $stderr = "$stdout.err"
        }
        # 启动失败(被安全软件拦截、文件被占用、Access is denied)只记一条 FAIL 继续跑下一个;
        # 不能让整张汇总表随 $ErrorActionPreference='Stop' 一起消失
        # (2026-09-13 agones_lifecycle_test.exe 一个 Access is denied 就把全套 21 个工程的结果吞掉了)。
        try {
            $proc = Start-Process -FilePath $exe -WorkingDirectory $repoRoot -WindowStyle Hidden -PassThru `
                -RedirectStandardOutput $stdout -RedirectStandardError $stderr
        }
        catch {
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "启动失败:$($_.Exception.Message)" })
            $failed++
            continue
        }
        if (-not $proc.WaitForExit($TimeoutSeconds * 1000)) {
            $killError = ''
            try { $proc.Kill() } catch { $killError = $_.Exception.Message }
            if (-not $proc.WaitForExit(5000)) {
                $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "超时且自有测试进程未退出，PID=$($proc.Id)；$killError" })
                $results | Format-Table -AutoSize
                throw "$name 超时后未能退出，停止后续测试；保留日志：$stdout；$stderr"
            }
            $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "超时 ${TimeoutSeconds}s，已确认进程退出" })
            $failed++
        }
        else {
            $out = Get-Content $stdout -Raw
            if ($null -eq $out) { $out = '' }
            $ran = [regex]::Match($out, 'Running (\d+) test')
            $pass = [regex]::Match($out, '\[  PASSED  \] (\d+)')
            $fail = [regex]::Match($out, '\[  FAILED  \] (\d+)')
            $skip = [regex]::Match($out, '\[  SKIPPED \] (\d+) test')
            $runCount = if ($ran.Success) { [int]$ran.Groups[1].Value } else { 0 }
            $passCount = if ($pass.Success) { [int]$pass.Groups[1].Value } else { 0 }
            $skipCount = if ($skip.Success) { [int]$skip.Groups[1].Value } else { 0 }
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
            elseif ($runCount -le 0 -or ($passCount + $skipCount) -ne $runCount) {
                $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "用例汇总不完整：运行 $runCount，通过 $passCount，跳过 $skipCount" })
                $failed++
            }
            elseif ($skip.Success) {
                $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "$($pass.Groups[1].Value)/$($ran.Groups[1].Value) 通过，$($skip.Groups[1].Value) 跳过" })
            }
            else {
                $results.Add([pscustomobject]@{ 名称 = $name; 构建 = 'OK'; 结果 = "$($pass.Groups[1].Value)/$($ran.Groups[1].Value) 全过" })
            }
        }
        if (-not $LogDir) { Remove-Item $stdout, $stderr -ErrorAction SilentlyContinue }
    }
} finally {
    foreach ($variable in $gtestEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($variable, $gtestEnvironment[$variable], 'Process')
    }
}

$results | Format-Table -AutoSize
if ($failed -gt 0) {
    Write-Host "$failed test projects failed" -ForegroundColor Red
    exit 1
}
Write-Host "全部 $($projects.Count) 个测试程序正常完成；用例跳过情况见汇总。" -ForegroundColor Green
exit 0
