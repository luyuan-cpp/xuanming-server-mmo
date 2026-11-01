#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 的 db-move 命令转发契约测试。
.DESCRIPTION
    dev_tools.ps1 是手册里搬库的入口。退出码必须原样透传:1 拒绝、2 没做成、3 结局未知,三个结论运维的处理完全不同,压成一个数就分不清了。

    做法同 dev_tools_merge_zone_contract:从 AST 取函数,在子进程里 pwsh -File 真跑,`go` 与编译产物都是假替身。不起 go、不连任何库。
.EXAMPLE
    pwsh -File tools/scripts/tests/dev_tools_db_move_contract.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$devToolsPath = Join-Path $PSScriptRoot '../dev_tools.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($devToolsPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$devToolsSource = Get-Content -LiteralPath $devToolsPath -Raw

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-db-move-contract-tests-' + [guid]::NewGuid().ToString('N'))

try {
    New-Item -ItemType Directory -Path (Join-Path $testRoot 'tools/scripts') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $testRoot 'tools/dbmove') -Force | Out-Null

    Test-Case '入口:ValidateSet、分发、帮助和参数都在;DSN 不走参数' {
        $commandParam = $ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Command' }
        $allowed = @(($commandParam.Attributes | Where-Object { $_.TypeName.Name -eq 'ValidateSet' }).PositionalArguments | ForEach-Object { $_.Value })
        Assert-True ('db-move' -in $allowed) '-Command 的 ValidateSet 必须含 db-move'
        Assert-Match $devToolsSource '"db-move"\s*\{\s*Invoke-DbMoveGo\s*\}' 'db-move 必须经 Invoke-DbMoveGo 分发'
        Assert-Match $devToolsSource '-Command db-move\s' '帮助文本必须列出 db-move'
        $names = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        Assert-True ('DbMoveArgs' -in $names) 'param 块必须有 -DbMoveArgs'
        Assert-Equal 'DbMoveArgs' (@($names | Where-Object { $_ -like 'DbMove*' }) -join '|') '连接串只从环境变量读,不加别的 db-move 参数'
        $fn = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-DbMoveGo' }, $true)
        Assert-NotMatch $fn.Extent.Text 'Write-Host' 'Invoke-DbMoveGo 不得 Write-Host'
        Assert-NotMatch $fn.Extent.Text 'go run' '不能用 go run:它把非零退出码压成 1'
    }

    Test-Case '子命令:手册和工具一致' {
        $toolSource = Get-Content -LiteralPath (Join-Path $PSScriptRoot '../../dbmove/main.go') -Raw
        $runbook = Get-Content -LiteralPath (Join-Path $PSScriptRoot '../../../docs/ops/db-move-runbook.md') -Raw
        foreach ($sub in @('plan', 'prepare', 'verify', 'cutover', 'rollback', 'audit', 'cleanup')) {
            Assert-Match $toolSource ('cmd[A-Za-z]+\s*=\s*"' + $sub + '"') "tools/dbmove/main.go 必须有子命令 $sub"
            Assert-Match $runbook $sub "手册必须写 $sub"
        }
    }

    $names = @('New-DbMoveExePath', 'Invoke-DbMoveGo')
    $definitions = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $names }, $false))
    $childDir = Join-Path $testRoot 'child'
    New-Item -ItemType Directory -Path $childDir -Force | Out-Null
    $fakeExe = Join-Path $childDir 'fake_dbmove.ps1'
    $goRecord = Join-Path $childDir 'go_record.txt'
    $toolRecord = Join-Path $childDir 'tool_record.txt'
    $pwshPath = (Get-Process -Id $PID).Path
    $fakeGo = @'
function New-DbMoveExePath { return $env:DM_TEST_FAKE_EXE }
function go {
    Set-Content -LiteralPath $env:DM_TEST_GO_RECORD -Value (@("cwd=" + $PWD.Path) + @($args | ForEach-Object { [string]$_ }))
    if ([int]$env:DM_TEST_BUILD_EXIT -ne 0) { $global:LASTEXITCODE = [int]$env:DM_TEST_BUILD_EXIT; return }
    $list = @($args | ForEach-Object { [string]$_ })
    $out = $list[[array]::IndexOf($list, '-o') + 1]
    Set-Content -LiteralPath $out -Value @(
        'Set-Content -LiteralPath $env:DM_TEST_TOOL_RECORD -Value (@("cwd=" + $PWD.Path) + @($args | ForEach-Object { [string]$_ }))',
        'exit [int]$env:DM_TEST_TOOL_EXIT')
    $global:LASTEXITCODE = 0
}
'@
    function Write-Child([string]$ArgsLiteral) {
        $path = Join-Path $childDir 'run_invoke.ps1'
        Set-Content -LiteralPath $path -Encoding utf8 -Value (@('$ErrorActionPreference = ''Stop''') + @($definitions | ForEach-Object { $_.Extent.Text }) + @(
            ('$ScriptDir = ''{0}''' -f (Join-Path $testRoot 'tools/scripts').Replace("'", "''")), ('$DbMoveArgs = {0}' -f $ArgsLiteral), $fakeGo, 'Invoke-DbMoveGo', 'exit 99'))
        return $path
    }
    $envNames = @('DM_TEST_FAKE_EXE', 'DM_TEST_GO_RECORD', 'DM_TEST_TOOL_RECORD', 'DM_TEST_BUILD_EXIT', 'DM_TEST_TOOL_EXIT')
    $saved = @{}; foreach ($n in $envNames) { $saved[$n] = [Environment]::GetEnvironmentVariable($n, 'Process') }
    function Invoke-Child([string]$Path, [int]$BuildExit, [int]$ToolExit) {
        foreach ($f in @($goRecord, $toolRecord, $fakeExe)) { Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue }
        $env:DM_TEST_FAKE_EXE = $fakeExe; $env:DM_TEST_GO_RECORD = $goRecord; $env:DM_TEST_TOOL_RECORD = $toolRecord
        $env:DM_TEST_BUILD_EXIT = [string]$BuildExit; $env:DM_TEST_TOOL_EXIT = [string]$ToolExit
        $ErrorActionPreference = 'Continue'
        $output = & $pwshPath -NoProfile -NonInteractive -File $Path 2>&1 | Out-String
        return [pscustomobject]@{ Code = $LASTEXITCODE; Output = $output }
    }
    try {
        Test-Case '退出码:原样透传 0/1/2/3;在 tools/dbmove 编译,回仓库根执行;参数逐项转发;跑完删产物' {
            $child = Write-Child "@('plan', '-db', 'mmorpg_guild', '-target-cluster', 'b')"
            foreach ($toolExit in @(0, 1, 2, 3)) {
                $r = Invoke-Child -Path $child -BuildExit 0 -ToolExit $toolExit
                Assert-Equal $toolExit $r.Code "工具退出 $toolExit 时进程退出码必须原样(99 = 漏了 exit)。输出: $($r.Output)"
                Assert-True (-not (Test-Path -LiteralPath $fakeExe)) '跑完必须删掉临时编译产物'
            }
            $goCall = @(Get-Content -LiteralPath $goRecord)
            Assert-Equal ('cwd=' + (Resolve-Path (Join-Path $testRoot 'tools/dbmove')).Path) $goCall[0] 'tools/dbmove 是独立 go module,必须在那个目录里编译'
            Assert-Equal 'build' $goCall[1] '必须先 go build 再直接执行产物'
            $toolCall = @(Get-Content -LiteralPath $toolRecord)
            Assert-Equal ('cwd=' + (Resolve-Path $testRoot).Path) $toolCall[0] '工具在仓库根运行,默认日志目录才落在已忽略的 run/ 下'
            Assert-Equal 'plan|-db|mmorpg_guild|-target-cluster|b' (@($toolCall | Select-Object -Skip 1) -join '|') '参数逐项原样转发'
        }

        Test-Case '编译失败给 2 且不运行工具;没给子命令以 1 拒绝且不编译' {
            $child = Write-Child "@('plan')"
            $r = Invoke-Child -Path $child -BuildExit 1 -ToolExit 0
            Assert-Equal 2 $r.Code "编译失败退出码必须是 2。输出: $($r.Output)"
            Assert-True (-not (Test-Path -LiteralPath $toolRecord)) '编译失败不得运行工具'
            Assert-Match $r.Output 'dbmove build failed' '编译失败必须点名报错'

            $child = Write-Child '@()'
            $r = Invoke-Child -Path $child -BuildExit 0 -ToolExit 0
            Assert-Equal 1 $r.Code "没给子命令退出码必须是 1。输出: $($r.Output)"
            Assert-Match $r.Output 'DbMoveArgs' '报错点名缺的参数'
            Assert-True (-not (Test-Path -LiteralPath $goRecord)) '没给子命令时不编译'
        }
    }
    finally {
        foreach ($n in $envNames) { [Environment]::SetEnvironmentVariable($n, $saved[$n], 'Process') }
    }
}
finally {
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-db-move-contract-tests-*') { throw "拒绝清理测试目录以外的路径: $resolvedRoot" }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'dev_tools db-move 转发契约')
