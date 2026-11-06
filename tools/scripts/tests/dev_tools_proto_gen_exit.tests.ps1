#requires -Version 7
<#
.SYNOPSIS
    在隔离目录运行真实 dev_tools.ps1 入口，验证生成器失败不会被包装成成功。
.DESCRIPTION
    外部生成器和 go 使用同一个最小进程桩；不运行真正的协议生成、不改生成树。
    子进程同时记录 finally 后的环境和工作目录，覆盖成功、失败及各候选选择路径。
    另覆盖旧 exe 防护：选中的 exe 比生成器源码旧时，默认路径告警并改走 go run，-UseBinary 拒绝运行；
    以及 proto-gen-build 给 exe 打「构建开始时刻」写入时间戳后，build → run -UseBinary 的判旧结果。
#>
param([string]$EvidenceDir = (Join-Path ([IO.Path]::GetTempPath()) 'codex-proto-gen-exit-tests'))

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
. "$PSScriptRoot/lib/test_harness.ps1"

$fixture = Join-Path $EvidenceDir ('proto generator fixture ' + [Guid]::NewGuid().ToString('N'))
$scriptDir = Join-Path $fixture 'tools/scripts'
$generatorDir = Join-Path $fixture 'tools/proto_generator/protogen'
$stubDir = Join-Path $fixture 'stub'
foreach ($directory in @($scriptDir, (Join-Path $scriptDir 'lib'), (Join-Path $generatorDir 'etc'), $stubDir)) {
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
}
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '../dev_tools.ps1') -Destination $scriptDir
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '../lib/release_common.ps1') -Destination (Join-Path $scriptDir 'lib')
foreach ($configName in @('proto_gen.yaml', 'custom.yaml')) {
    Set-Content -LiteralPath (Join-Path $generatorDir "etc/$configName") -Value '# 夹具配置，不执行真实生成'
}

# 原生进程确实返回非零码，避免用 PowerShell 函数替身掩盖 LASTEXITCODE 的行为。
# 第一个参数是 build 时进程桩模拟 go build（只有 proto-gen-build 用例走到）：
#   - 设了 PROTOGEN_TEST_SAVE_DURING_BUILD 时，先等 100ms 再写这份源码、写入时间取当时时刻，再等 100ms 才退出，
#     模拟构建期间别的会话保存源码，保证其写入时间严格落在「构建开始」与「构建结束」之间（不受系统时钟粒度影响）；
#   - -o 产物已存在时既不重写也不刷新写入时间，模拟 go build 判定产物已是最新、且尽力而为的 Chtimes 失败；
#     不存在时复制进程桩自身作为产物，后续 proto-gen-run 仍能运行它。
# 桩源码只用 ASCII：它由旧版 csc 编译，无 BOM 的非 ASCII 注释可能按系统代码页误读。
$stubSource = Join-Path $stubDir 'generator.cs'
@'
using System;
using System.IO;
using System.Reflection;
using System.Threading;
class GeneratorStub {
    static int Main(string[] args) {
        File.AppendAllLines(Environment.GetEnvironmentVariable("PROTOGEN_TEST_RECORD"), new[] {
            "executable=" + Path.GetFileNameWithoutExtension(Assembly.GetExecutingAssembly().Location),
            "config=" + Environment.GetEnvironmentVariable("PROTO_GEN_CONFIG_PATH"),
            "pprof=" + Environment.GetEnvironmentVariable("PROTOGEN_ENABLE_PPROF"),
            "legacy=" + Environment.GetEnvironmentVariable("PBGEN_ENABLE_PPROF"),
            "cwd=" + Environment.CurrentDirectory,
            "arguments=" + String.Join("|", args)
        });
        if (args.Length > 0 && args[0] == "build") {
            string savedDuringBuild = Environment.GetEnvironmentVariable("PROTOGEN_TEST_SAVE_DURING_BUILD");
            if (!String.IsNullOrEmpty(savedDuringBuild)) {
                Thread.Sleep(100);
                Directory.CreateDirectory(Path.GetDirectoryName(savedDuringBuild));
                File.WriteAllText(savedDuringBuild, "// fixture source saved during build");
                File.SetLastWriteTimeUtc(savedDuringBuild, DateTime.UtcNow);
                Thread.Sleep(100);
            }
            int outputIndex = Array.IndexOf(args, "-o");
            if (outputIndex >= 0 && outputIndex + 1 < args.Length && !File.Exists(args[outputIndex + 1])) {
                File.Copy(Assembly.GetExecutingAssembly().Location, args[outputIndex + 1]);
            }
        }
        return Int32.Parse(Environment.GetEnvironmentVariable("PROTOGEN_TEST_EXIT"));
    }
}
'@ | Set-Content -LiteralPath $stubSource -Encoding utf8
$compiler = Join-Path $env:WINDIR 'Microsoft.NET/Framework64/v4.0.30319/csc.exe'
$stubExe = Join-Path $stubDir 'generator.exe'
& $compiler /nologo /target:exe "/out:$stubExe" $stubSource 2>&1 | Set-Content -LiteralPath (Join-Path $fixture 'stub-build.log')
if ($LASTEXITCODE -ne 0) { throw "进程桩编译失败：$fixture/stub-build.log" }
Copy-Item -LiteralPath $stubExe -Destination (Join-Path $stubDir 'go.exe')

$childPath = Join-Path $fixture 'run.ps1'
@'
param([string]$FixtureRoot, [string]$Mode, [int]$ToolExit, [bool]$InitialEnvironment, [string]$SaveDuringBuild = '', [string]$Command = 'proto-gen-run')
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$env:PATH = (Join-Path $FixtureRoot 'stub') + [IO.Path]::PathSeparator + $env:PATH
$env:PROTOGEN_TEST_RECORD = Join-Path $FixtureRoot 'generator-record.txt'
$env:PROTOGEN_TEST_EXIT = [string]$ToolExit
$env:PROTOGEN_TEST_SAVE_DURING_BUILD = $SaveDuringBuild
$names = @('PROTO_GEN_CONFIG_PATH', 'PROTOGEN_ENABLE_PPROF', 'PBGEN_ENABLE_PPROF')
foreach ($name in $names) {
    $value = if ($InitialEnvironment) { '原值-' + $name } else { $null }
    [Environment]::SetEnvironmentVariable($name, $value, 'Process')
}
$before = @{}
foreach ($name in $names) { $before[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Set-Location -LiteralPath $FixtureRoot
$beforeLocation = $PWD.Path
$parameters = @{
    Command = $Command; ImageTag = 'fixture'; GoSvcTag = 'fixture'
    JavaSvcTag = 'fixture'; NodeImage = 'fixture:local'
}
if ($Mode -eq 'go-run') { $parameters.UseGoRun = $true }
if ($Mode -like 'binary-*' -or $Mode -like 'build-*') { $parameters.UseBinary = $true }
if ($InitialEnvironment) {
    $parameters.ConfigPath = 'tools/proto_generator/protogen/etc/custom.yaml'
} else {
    $parameters.EnablePprof = $true
}
try {
    # build-* 按 dev.bat 的顺序：先 proto-gen-build，再 proto-gen-run -UseBinary。
    if ($Mode -like 'build-*') {
        & (Join-Path $FixtureRoot 'tools/scripts/dev_tools.ps1') -Command 'proto-gen-build' -ImageTag 'fixture' -GoSvcTag 'fixture' -JavaSvcTag 'fixture' -NodeImage 'fixture:local'
    }
    & (Join-Path $FixtureRoot 'tools/scripts/dev_tools.ps1') @parameters
} finally {
    $after = @{}
    foreach ($name in $names) { $after[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    @{ Before = $before; After = $after; BeforeLocation = $beforeLocation; AfterLocation = $PWD.Path } |
        ConvertTo-Json -Depth 3 | Set-Content -LiteralPath (Join-Path $FixtureRoot 'restored.json')
}
'@ | Set-Content -LiteralPath $childPath -Encoding utf8

$pwshPath = (Get-Process -Id $PID).Path
$modes = @(
    @{ Name = 'go-run'; Primary = $true; Legacy = $true; Executable = 'go' },
    @{ Name = 'binary-primary'; Primary = $true; Legacy = $true; Executable = 'proto-gen' },
    @{ Name = 'binary-legacy'; Primary = $false; Legacy = $true; Executable = 'pbgen' },
    @{ Name = 'default-primary'; Primary = $true; Legacy = $true; Executable = 'proto-gen' },
    @{ Name = 'default-legacy'; Primary = $false; Legacy = $true; Executable = 'pbgen' },
    @{ Name = 'default-go'; Primary = $false; Legacy = $false; Executable = 'go' }
)
foreach ($mode in $modes) {
    foreach ($candidate in @(@{ Name = 'proto-gen.exe'; Present = $mode.Primary }, @{ Name = 'pbgen.exe'; Present = $mode.Legacy })) {
        $candidatePath = Join-Path $generatorDir $candidate.Name
        if ($candidate.Present) {
            Copy-Item -LiteralPath $stubExe -Destination $candidatePath -Force
        } elseif (Test-Path -LiteralPath $candidatePath) {
            Remove-Item -LiteralPath $candidatePath
        }
    }
    foreach ($initialEnvironment in @($false, $true)) {
        foreach ($toolExit in @(0, 23)) {
            Test-Case "$($mode.Name)：生成器退出 $toolExit，原环境非空=$initialEnvironment" {
                $recordPath = Join-Path $fixture 'generator-record.txt'
                $statePath = Join-Path $fixture 'restored.json'
                foreach ($path in @($recordPath, $statePath)) {
                    if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path }
                }
                # 原生 stderr 必须保留为测试数据，由断言判定，不能提前终止测试套件。
                $output = & $pwshPath -NoProfile -NonInteractive -File $childPath -FixtureRoot $fixture -Mode $mode.Name -ToolExit $toolExit -InitialEnvironment:$initialEnvironment 2>&1 | Out-String
                $processExit = $LASTEXITCODE
                $caseName = "$($mode.Name)-env-$initialEnvironment-exit-$toolExit"
                $output | Set-Content -LiteralPath (Join-Path $fixture "$caseName.log")
                Assert-True (Test-Path -LiteralPath $recordPath) '必须运行隔离进程桩，不能把入口自身报错算作生成器失败'
                $record = @(Get-Content -LiteralPath $recordPath)
                Assert-Equal 6 $record.Count '每次只调用一个生成器，失败时不得偷偷换旧版本再跑'
                Assert-Equal "executable=$($mode.Executable)" $record[0] '按入口参数选择生成器'
                $expectedConfig = if ($initialEnvironment) { 'custom.yaml' } else { 'proto_gen.yaml' }
                Assert-Equal ("config=" + (Join-Path $generatorDir "etc/$expectedConfig")) $record[1] '把本次配置传给生成器'
                $expectedPprof = if ($initialEnvironment) { '' } else { '1' }
                Assert-Equal "pprof=$expectedPprof" $record[2] 'pprof 开关按本次参数生效'
                Assert-Equal 'legacy=' $record[3] '本次生成不继承旧 profiling 开关'
                Assert-Equal "cwd=$generatorDir" $record[4] '在生成器模块目录运行'
                $expectedArguments = if ($mode.Executable -eq 'go') { 'arguments=run|./cmd' } else { 'arguments=' }
                Assert-Equal $expectedArguments $record[5] '可执行程序和 go run 使用各自正式参数'
                Assert-True (Test-Path -LiteralPath $statePath) '失败也必须完成 finally'
                $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json -AsHashtable
                foreach ($name in @('PROTO_GEN_CONFIG_PATH', 'PROTOGEN_ENABLE_PPROF', 'PBGEN_ENABLE_PPROF')) {
                    Assert-Equal $state.Before[$name] $state.After[$name] "finally 必须恢复 $name"
                }
                Assert-Equal $state.BeforeLocation $state.AfterLocation 'finally 必须恢复调用者的工作目录'
                if ($toolExit -eq 0) {
                    Assert-Equal 0 $processExit '成功生成应保持成功退出'
                } else {
                    Assert-True ($processExit -ne 0) "生成器退出 $toolExit 时包装入口必须失败，实际退出 $processExit"
                    Assert-Match $output '23' '失败诊断必须带原始生成器退出码'
                }
            }
        }
    }
}

# ── persist-schema-seal / persist-schema-verify：复用 proto-gen-run 的 exe 选择，只多传各自的开关，失败码照样传出 ──
# 用例前后保持 proto-gen.exe 的有无状态（上面循环的最后一个模式把两个 exe 都移走了），不影响后面的旧 exe 用例。
$sealPrimaryPath = Join-Path $generatorDir 'proto-gen.exe'
$sealPrimaryExisted = Test-Path -LiteralPath $sealPrimaryPath
Copy-Item -LiteralPath $stubExe -Destination $sealPrimaryPath -Force
try {
    foreach ($ledgerCommand in @('persist-schema-seal', 'persist-schema-verify')) {
        foreach ($sealCase in @(
                @{ Mode = 'binary-primary'; Executable = 'proto-gen'; Arguments = "arguments=--$ledgerCommand" },
                @{ Mode = 'go-run'; Executable = 'go'; Arguments = "arguments=run|./cmd|--$ledgerCommand" })) {
            foreach ($toolExit in @(0, 23)) {
                Test-Case "$ledgerCommand（$($sealCase.Mode)）：生成器退出 $toolExit" {
                    $recordPath = Join-Path $fixture 'generator-record.txt'
                    if (Test-Path -LiteralPath $recordPath) { Remove-Item -LiteralPath $recordPath }
                    $output = & $pwshPath -NoProfile -NonInteractive -File $childPath -FixtureRoot $fixture -Mode $sealCase.Mode -ToolExit $toolExit -InitialEnvironment:$false -Command $ledgerCommand 2>&1 | Out-String
                    $processExit = $LASTEXITCODE
                    $output | Set-Content -LiteralPath (Join-Path $fixture "$ledgerCommand-$($sealCase.Mode)-exit-$toolExit.log")
                    Assert-True (Test-Path -LiteralPath $recordPath) '必须运行隔离进程桩'
                    $record = @(Get-Content -LiteralPath $recordPath)
                    Assert-Equal 6 $record.Count '只调用一次生成器'
                    Assert-Equal "executable=$($sealCase.Executable)" $record[0] '与 proto-gen-run 同一套生成器选择'
                    Assert-Equal $sealCase.Arguments $record[5] "把 --$ledgerCommand 原样传给生成器"
                    if ($toolExit -eq 0) {
                        Assert-Equal 0 $processExit '生成器成功时入口应保持成功退出'
                    } else {
                        Assert-True ($processExit -ne 0) "生成器退出 $toolExit 时入口必须失败，实际退出 $processExit"
                        Assert-Match $output '23' '失败诊断必须带原始生成器退出码'
                    }
                }
            }
        }
    }
} finally {
    if (-not $sealPrimaryExisted -and (Test-Path -LiteralPath $sealPrimaryPath)) { Remove-Item -LiteralPath $sealPrimaryPath }
}

# ── 旧 exe 防护：选中的 exe 比生成器源码旧时，默认路径告警并改走 go run，-UseBinary 拒绝运行 ──
# 上面的循环运行时生成器夹具目录里没有任何 .go / .tmpl / go.mod，旧 exe 判定恒为「不旧」，那组用例的期望不受影响。
# 下面每个用例自己放 exe（默认两个都放）与源码文件，并显式设置 LastWriteTimeUtc（不依赖墙钟和文件系统时间精度；
# 末尾两个 proto-gen-build 用例例外，见其注释）；结束时删掉自己建的源码（连同新建的顶层目录），
# 把两个 exe 恢复成用例前的有无状态，不影响其他用例。
$binaryWriteTimeUtc = [DateTime]::new(2026, 1, 1, 0, 0, 0, [DateTimeKind]::Utc)

function Invoke-StaleBinaryCase {
    param(
        [Parameter(Mandatory = $true)][string]$Mode,
        # 相对生成器目录的源码路径（用 / 分隔）→ 相对 exe 写入时间的秒数偏移（正数 = 比 exe 新）
        [Parameter(Mandatory = $true)][hashtable]$SourceOffsetSeconds,
        # 用例期间放进生成器目录的 exe；不在其中的候选在用例期间移走。默认两个都放，此时选中的是 proto-gen.exe。
        [string[]]$BinaryNames = @('proto-gen.exe', 'pbgen.exe'),
        # 仅 build-* 模式：进程桩在 go build 期间写入的源码（相对生成器目录，用 / 分隔），模拟别的会话并发保存。
        [string]$SaveDuringBuild = ''
    )
    $allBinaryNames = @('proto-gen.exe', 'pbgen.exe')
    $binaryExisted = @{}
    foreach ($name in $allBinaryNames) { $binaryExisted[$name] = Test-Path -LiteralPath (Join-Path $generatorDir $name) }
    $createdSources = [System.Collections.Generic.List[string]]::new()
    $createdTopLevelDirs = [System.Collections.Generic.List[string]]::new()
    $trackedRelativePaths = @($SourceOffsetSeconds.Keys) + @($SaveDuringBuild | Where-Object { $_ })
    foreach ($relativePath in $trackedRelativePaths) {
        if ($relativePath.Contains('/')) {
            $topLevelDir = Join-Path $generatorDir ($relativePath.Split('/')[0])
            if (-not (Test-Path -LiteralPath $topLevelDir) -and -not $createdTopLevelDirs.Contains($topLevelDir)) { $createdTopLevelDirs.Add($topLevelDir) }
        }
    }
    $savedDuringBuildPath = ''
    if ($SaveDuringBuild) {
        $savedDuringBuildPath = Join-Path $generatorDir $SaveDuringBuild
        $createdSources.Add($savedDuringBuildPath)
    }
    try {
        foreach ($name in $allBinaryNames) {
            $binaryPath = Join-Path $generatorDir $name
            if ($BinaryNames -contains $name) {
                Copy-Item -LiteralPath $stubExe -Destination $binaryPath -Force
                [IO.File]::SetLastWriteTimeUtc($binaryPath, $binaryWriteTimeUtc)
            } elseif (Test-Path -LiteralPath $binaryPath) {
                Remove-Item -LiteralPath $binaryPath
            }
        }
        foreach ($relativePath in $SourceOffsetSeconds.Keys) {
            $sourcePath = Join-Path $generatorDir $relativePath
            New-Item -ItemType Directory -Path (Split-Path -Parent $sourcePath) -Force | Out-Null
            Set-Content -LiteralPath $sourcePath -Value '// 夹具源码，只用于比较写入时间'
            $createdSources.Add($sourcePath)
            [IO.File]::SetLastWriteTimeUtc($sourcePath, $binaryWriteTimeUtc.AddSeconds($SourceOffsetSeconds[$relativePath]))
        }

        $recordPath = Join-Path $fixture 'generator-record.txt'
        $statePath = Join-Path $fixture 'restored.json'
        foreach ($path in @($recordPath, $statePath)) {
            if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path }
        }
        # 空字符串参数在不同原生参数传递模式下可能被丢掉，所以只在需要时才传 -SaveDuringBuild。
        $extraChildArguments = @()
        if ($savedDuringBuildPath) { $extraChildArguments = @('-SaveDuringBuild', $savedDuringBuildPath) }
        # 下界：dev_tools 取的构建开始时刻必然不早于此刻。
        $launchUtc = [DateTime]::UtcNow
        $output = & $pwshPath -NoProfile -NonInteractive -File $childPath -FixtureRoot $fixture -Mode $Mode -ToolExit 0 -InitialEnvironment:$false @extraChildArguments 2>&1 | Out-String
        $processExit = $LASTEXITCODE
        $output | Set-Content -LiteralPath (Join-Path $fixture "stale-$Mode.log")
        $record = if (Test-Path -LiteralPath $recordPath) { @(Get-Content -LiteralPath $recordPath) } else { $null }
        $state = if (Test-Path -LiteralPath $statePath) { Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json -AsHashtable } else { $null }
        # exe 与并发保存源码的写入时间必须在 finally 清理之前取。
        $binaryWriteTimesUtc = @{}
        foreach ($name in $allBinaryNames) {
            $binaryPath = Join-Path $generatorDir $name
            if (Test-Path -LiteralPath $binaryPath) { $binaryWriteTimesUtc[$name] = [IO.File]::GetLastWriteTimeUtc($binaryPath) }
        }
        $savedDuringBuildWriteTimeUtc = if ($savedDuringBuildPath -and (Test-Path -LiteralPath $savedDuringBuildPath)) { [IO.File]::GetLastWriteTimeUtc($savedDuringBuildPath) } else { $null }
        return [pscustomobject]@{
            Output = $output; Exit = $processExit; Record = $record; State = $state
            LaunchUtc = $launchUtc; BinaryWriteTimesUtc = $binaryWriteTimesUtc; SavedDuringBuildWriteTimeUtc = $savedDuringBuildWriteTimeUtc
        }
    } finally {
        foreach ($sourcePath in $createdSources) {
            if (Test-Path -LiteralPath $sourcePath) { Remove-Item -LiteralPath $sourcePath }
        }
        foreach ($directory in $createdTopLevelDirs) {
            if (Test-Path -LiteralPath $directory) { Remove-Item -LiteralPath $directory -Recurse }
        }
        foreach ($name in $allBinaryNames) {
            $binaryPath = Join-Path $generatorDir $name
            if ($binaryExisted[$name]) {
                Copy-Item -LiteralPath $stubExe -Destination $binaryPath -Force
            } elseif (Test-Path -LiteralPath $binaryPath) {
                Remove-Item -LiteralPath $binaryPath
            }
        }
    }
}

function Assert-ProtoGenRunRestored {
    param([Parameter(Mandatory = $true)][AllowNull()]$State)
    Assert-True ($null -ne $State) '拒绝或改走 go run 也必须完成 finally'
    foreach ($name in @('PROTO_GEN_CONFIG_PATH', 'PROTOGEN_ENABLE_PPROF', 'PBGEN_ENABLE_PPROF')) {
        Assert-Equal $State.Before[$name] $State.After[$name] "finally 必须恢复 $name"
    }
    Assert-Equal $State.BeforeLocation $State.AfterLocation 'finally 必须恢复调用者的工作目录'
}

Test-Case '旧 exe 防护：默认路径下 exe 比 internal/x.go 旧，告警并改走 go run' {
    $result = Invoke-StaleBinaryCase -Mode 'default-stale-source' -SourceOffsetSeconds @{ 'internal/x.go' = 60 }
    Assert-Equal 0 $result.Exit '改走 go run 且生成成功时入口保持成功退出'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 6 $result.Record.Count '只调用一个生成器，不在 go run 之外再跑旧 exe'
    Assert-Equal 'executable=go' $result.Record[0] 'exe 比源码旧时默认路径改用 go run'
    Assert-Equal 'arguments=run|./cmd' $result.Record[5] 'go run 使用正式参数'
    Assert-Match $result.Output 'WARNING' '必须告警，不能静默换生成器'
    Assert-Match $result.Output 'x\.go' '告警指出比 exe 新的源码文件'
    Assert-Match $result.Output 'proto-gen-build' '告警提示重建 exe'
    Assert-ProtoGenRunRestored $result.State
}

Test-Case '旧 exe 防护：-UseBinary 下 exe 比源码旧，拒绝运行任何生成器' {
    $result = Invoke-StaleBinaryCase -Mode 'binary-stale-source' -SourceOffsetSeconds @{ 'internal/x.go' = 60 }
    Assert-True ($result.Exit -ne 0) "exe 比源码旧时 -UseBinary 必须失败，实际退出 $($result.Exit)"
    Assert-True ($null -eq $result.Record) '拒绝时既不得运行旧 exe，也不得偷偷改走 go run'
    Assert-Match $result.Output 'proto-gen-build' '拒绝诊断提示先重建 exe'
    Assert-ProtoGenRunRestored $result.State
}

Test-Case '旧 exe 防护：exe 比源码新时默认路径照旧跑 exe' {
    $result = Invoke-StaleBinaryCase -Mode 'default-fresh-source' -SourceOffsetSeconds @{ 'internal/x.go' = -60; 'internal/template/y.tmpl' = -60 }
    Assert-Equal 0 $result.Exit '生成成功时入口保持成功退出'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 'executable=proto-gen' $result.Record[0] 'exe 不旧时默认路径照旧优先跑 proto-gen.exe'
    Assert-NotMatch $result.Output 'WARNING' 'exe 不旧时不告警'
}

Test-Case '旧 exe 防护：只有 *_test.go 比 exe 新不算旧，-UseBinary 照旧跑 exe' {
    $result = Invoke-StaleBinaryCase -Mode 'binary-test-source-only' -SourceOffsetSeconds @{ 'internal/x.go' = -60; 'internal/x_test.go' = 60 }
    Assert-Equal 0 $result.Exit '测试文件不进二进制，不得据此拒绝运行'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 'executable=proto-gen' $result.Record[0] '照旧跑 proto-gen.exe'
}

Test-Case '旧 exe 防护：*.tmpl 比 exe 新也算旧，默认路径改走 go run' {
    $result = Invoke-StaleBinaryCase -Mode 'default-stale-template' -SourceOffsetSeconds @{ 'internal/x.go' = -60; 'internal/template/y.tmpl' = 60 }
    Assert-Equal 0 $result.Exit '改走 go run 且生成成功时入口保持成功退出'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 'executable=go' $result.Record[0] '部分模板经 go:embed 编进二进制，模板更新也要改用 go run'
    Assert-Match $result.Output 'y\.tmpl' '告警指出比 exe 新的模板文件'
}

Test-Case '旧 exe 防护：根目录 go.mod 比 exe 新也算旧，-UseBinary 拒绝运行' {
    $result = Invoke-StaleBinaryCase -Mode 'binary-stale-go-mod' -SourceOffsetSeconds @{ 'internal/x.go' = -60; 'go.mod' = 60 }
    Assert-True ($result.Exit -ne 0) "依赖版本变化后 -UseBinary 必须失败，实际退出 $($result.Exit)"
    Assert-True ($null -eq $result.Record) '拒绝时不得运行任何生成器'
    Assert-Match $result.Output 'proto-gen-build' '拒绝诊断提示先重建 exe'
}

Test-Case '旧 exe 防护：运行期输出目录 cpp/ 与配置目录 etc/ 下的文件不算源码' {
    $result = Invoke-StaleBinaryCase -Mode 'binary-excluded-dirs' -SourceOffsetSeconds @{ 'internal/x.go' = -60; 'cpp/generated/z.go' = 60; 'etc/w.tmpl' = 60 }
    Assert-Equal 0 $result.Exit 'regen 写入的输出与运行期配置不得让 exe 被判为旧'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 'executable=proto-gen' $result.Record[0] '照旧跑 proto-gen.exe'
}

# 只剩兼容名 pbgen.exe 时，判旧必须针对它本身（它恰恰最可能是留下来的旧版本）。
# -UseBinary 的拒绝文案可能被错误视图按控制台宽度折行，所以先去掉空白和折行前缀「|」再找文件名。
Test-Case '旧 exe 防护：只剩 pbgen.exe 且比源码旧，-UseBinary 拒绝运行' {
    $result = Invoke-StaleBinaryCase -Mode 'binary-legacy-stale-source' -SourceOffsetSeconds @{ 'internal/x.go' = 60 } -BinaryNames @('pbgen.exe')
    Assert-True ($result.Exit -ne 0) "pbgen.exe 比源码旧时 -UseBinary 必须失败，实际退出 $($result.Exit)"
    Assert-True ($null -eq $result.Record) '拒绝时不得运行任何生成器'
    Assert-Match $result.Output 'Stale proto-gen binary refused' '必须是判旧拒绝，而不是找不到 exe 或判定过程本身出错'
    Assert-Match ($result.Output -replace '[\s|]', '') 'pbgen\.exe\(' '拒绝诊断指名被判旧的 pbgen.exe'
    Assert-ProtoGenRunRestored $result.State
}

Test-Case '旧 exe 防护：只剩 pbgen.exe 且比源码旧，默认路径告警并改走 go run' {
    $result = Invoke-StaleBinaryCase -Mode 'default-legacy-stale-source' -SourceOffsetSeconds @{ 'internal/x.go' = 60 } -BinaryNames @('pbgen.exe')
    Assert-Equal 0 $result.Exit '改走 go run 且生成成功时入口保持成功退出'
    Assert-True ($null -ne $result.Record) '必须运行一次生成器'
    Assert-Equal 6 $result.Record.Count '只调用一个生成器，不在 go run 之外再跑旧 pbgen.exe'
    Assert-Equal 'executable=go' $result.Record[0] 'pbgen.exe 比源码旧时默认路径改用 go run'
    Assert-Match $result.Output 'WARNING' '必须告警，不能静默换生成器'
    Assert-Match $result.Output 'pbgen\.exe' '告警指名被判旧的 pbgen.exe'
    Assert-ProtoGenRunRestored $result.State
}

# ── proto-gen-build 给 exe 打「构建开始时刻」的写入时间戳 ──
# 进程桩模拟 go build：产物已存在时不重写、不刷新写入时间（go 判定已是最新且 Chtimes 失败），只在 build 时可选地并发写一份源码。
# 两个 exe 预置的写入时间是 2026-01-01（$binaryWriteTimeUtc），源码偏移相对它；构建开始时刻取真实时钟，必然晚于这些固定时间。
Test-Case 'proto-gen-build 打戳：源码只被 touch、go build 未重写产物时，构建后 -UseBinary 照旧运行' {
    $result = Invoke-StaleBinaryCase -Mode 'build-touched-source' -SourceOffsetSeconds @{ 'internal/x.go' = 60 }
    Assert-Equal 0 $result.Exit 'build → run -UseBinary（dev.bat 的顺序）不得因只 touch 过的源码被拒'
    Assert-True ($null -ne $result.Record) '必须运行构建与生成器'
    Assert-Equal 12 $result.Record.Count '先一次 go build，再一次 exe，共两次进程调用'
    Assert-Equal 'executable=go' $result.Record[0] '构建走 go'
    Assert-True ($result.Record[5] -like 'arguments=build|*') "构建使用 go build 参数，实际 '$($result.Record[5])'"
    Assert-Equal 'executable=proto-gen' $result.Record[6] '构建后照旧跑 proto-gen.exe'
    $primaryWriteTimeUtc = $result.BinaryWriteTimesUtc['proto-gen.exe']
    $legacyWriteTimeUtc = $result.BinaryWriteTimesUtc['pbgen.exe']
    Assert-True ($null -ne $primaryWriteTimeUtc -and $null -ne $legacyWriteTimeUtc) '构建后两个 exe 都在'
    Assert-Equal $legacyWriteTimeUtc.Ticks $primaryWriteTimeUtc.Ticks '两个 exe 打同一个戳'
    Assert-True ($primaryWriteTimeUtc -ge $result.LaunchUtc) "戳必须是本次构建的时刻，不能沿用旧 exe 的写入时间（实际 $($primaryWriteTimeUtc.ToString('o'))，下界 $($result.LaunchUtc.ToString('o'))）"
    Assert-ProtoGenRunRestored $result.State
}

Test-Case 'proto-gen-build 打戳：构建期间保存的源码晚于戳，构建后 -UseBinary 拒绝运行' {
    $result = Invoke-StaleBinaryCase -Mode 'build-save-during-build' -SourceOffsetSeconds @{ 'internal/x.go' = 60 } -SaveDuringBuild 'internal/saved_during_build.go'
    Assert-True ($result.Exit -ne 0) "构建期间保存、可能没进产物的源码必须让 -UseBinary 失败，实际退出 $($result.Exit)"
    Assert-True ($null -ne $result.Record) '必须运行构建'
    Assert-Equal 6 $result.Record.Count '只运行了构建，拒绝时不得再跑 exe 或 go run'
    Assert-Equal 'executable=go' $result.Record[0] '构建走 go'
    Assert-Match $result.Output 'Stale proto-gen binary refused' '必须是判旧拒绝'
    Assert-Match ($result.Output -replace '[\s|]', '') 'saved_during_build\.go' '拒绝诊断指出构建期间保存的源码'
    $primaryWriteTimeUtc = $result.BinaryWriteTimesUtc['proto-gen.exe']
    $legacyWriteTimeUtc = $result.BinaryWriteTimesUtc['pbgen.exe']
    Assert-True ($null -ne $primaryWriteTimeUtc -and $null -ne $legacyWriteTimeUtc) '构建后两个 exe 都在'
    Assert-Equal $legacyWriteTimeUtc.Ticks $primaryWriteTimeUtc.Ticks '两个 exe 打同一个戳'
    Assert-True ($primaryWriteTimeUtc -ge $result.LaunchUtc) '戳必须是本次构建的时刻'
    Assert-True ($null -ne $result.SavedDuringBuildWriteTimeUtc) '进程桩必须在构建期间写入源码'
    Assert-True ($primaryWriteTimeUtc -lt $result.SavedDuringBuildWriteTimeUtc) "戳必须取构建开始而非构建结束时刻（戳 $($primaryWriteTimeUtc.ToString('o'))，构建期间保存 $($result.SavedDuringBuildWriteTimeUtc.ToString('o'))）"
    Assert-ProtoGenRunRestored $result.State
}
Write-Host "证据目录：$fixture"
exit (Complete-TestRun -SuiteName 'proto-gen-run 退出码与 finally 恢复')
