#requires -Version 7
<#
.SYNOPSIS
    dev 档「改动前后生成物逐字相同」的事后比对(交接 docs/handoff/handoff-global-singletons-p0-20261015.md §9.5 第 5 步)。

.DESCRIPTION
    补丁已于 2026-11-06 打入主工作区,打入前的 k8s_deploy.ps1 备份在本目录 shared-redis-secret-backup/tools/scripts/k8s_deploy.ps1。
    主工作区还在被别的会话改,所以不拿现在那份比。做法:
      1. 在临时目录把备份复制一份,只把本补丁里 tools/scripts/k8s_deploy.ps1 的改动打上去(git apply --include),得到「只差本补丁」的改后版;
      2. 改前、改后两份临时拷进 <仓库>/tools/scripts/(k8s_deploy.before-shared-redis.ps1 / k8s_deploy.after-shared-redis.ps1),
         同一目录下按 $PSScriptRoot 找 lib 与仓库根的逻辑不变,读的是同一批 manifest 与服务 yaml;
      3. 两份各跑一次 dev zone-up 与带全局 Go 服务的 dev infra-up(只用 -DryRun,不碰集群),逐字比对输出;结束删掉两份临时拷贝。
    DryRun 夹具(SQL 写者目录)取契约测试 k8s_deploy_contract.tests.ps1 里的 Invoke-DeployDryRun,只把脚本路径换成参数。
    比较前去掉两类夹具噪声:临时目录名 mmorpg-roster-render-<GUID>,以及输出里出现的临时脚本文件名。
    先清掉 MMORPG_* 环境变量再跑(dev 基线假定都没设)。通过 = 两组都打印 IDENTICAL,退出码 0。
#>
param([string]$RepoRoot = 'D:\luyuan\xuanming-server-mmo-cpp')

$ErrorActionPreference = 'Stop'
$beforeSource = Join-Path $PSScriptRoot 'shared-redis-secret-backup/tools/scripts/k8s_deploy.ps1'
$patch = Join-Path $PSScriptRoot 'shared-redis-secret.patch'
foreach ($required in @($beforeSource, $patch)) {
    if (-not (Test-Path -LiteralPath $required)) { throw "找不到 $required" }
}
$beforeScript = Join-Path $RepoRoot 'tools/scripts/k8s_deploy.before-shared-redis.ps1'
$afterScript = Join-Path $RepoRoot 'tools/scripts/k8s_deploy.after-shared-redis.ps1'
foreach ($temp in @($beforeScript, $afterScript)) {
    if (Test-Path -LiteralPath $temp) { throw "$temp 已存在(上次没清干净?),先确认它是本脚本留下的再删" }
}

. (Join-Path $RepoRoot 'tools/scripts/tests/lib/test_harness.ps1')
. (Join-Path $RepoRoot 'tools/scripts/tests/lib/deploy_capture.ps1')
$tokens = $null; $parseErrors = $null
$testAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $RepoRoot 'tools/scripts/tests/k8s_deploy_contract.tests.ps1'), [ref]$tokens, [ref]$parseErrors)
$fixture = $testAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Invoke-DeployDryRun' }, $false)
if ($null -eq $fixture) { throw '契约测试里找不到 Invoke-DeployDryRun' }
$fixtureText = $fixture.Extent.Text.Replace("(Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1')", '$script:DeployScriptUnderTest')
if ($fixtureText -ceq $fixture.Extent.Text) { throw '契约测试里 Invoke-DeployDryRun 的脚本路径写法变了,本脚本要同步' }
. ([scriptblock]::Create($fixtureText))

$zoneArgs = @('-Command', 'zone-up', '-ZoneName', 'contract-test', '-ZoneId', '101', '-DryRun', '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000',
    '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test')
$infraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipJavaSvc', '-InfraNamespace', 'contract-battle-infra',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab', '-GoSvcRegistry', 'registry.invalid/test', '-KafkaBrokers', '3')
$battleEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }

function ConvertTo-ComparableOutput {
    param([AllowEmptyString()][string]$Text)
    $t = $Text -replace "`r`n", "`n"
    $t = $t -replace 'mmorpg-roster-render-[0-9a-f]{32}', 'mmorpg-roster-render-<GUID>'
    return $t.Replace('k8s_deploy.before-shared-redis.ps1', 'k8s_deploy.ps1').Replace('k8s_deploy.after-shared-redis.ps1', 'k8s_deploy.ps1')
}

$work = Join-Path ([IO.Path]::GetTempPath()) ('shared-redis-compare-' + [Guid]::NewGuid().ToString('N'))
$failed = $false
try {
    # 1. 只差本补丁的改后版(临时目录不在任何 git 仓库里,git apply 按当前目录的相对路径打,不做换行转换)。
    $null = New-Item -ItemType Directory -Path (Join-Path $work 'tools/scripts')
    Copy-Item -LiteralPath $beforeSource -Destination (Join-Path $work 'tools/scripts/k8s_deploy.ps1')
    Push-Location $work
    try {
        & git apply --include=tools/scripts/k8s_deploy.ps1 $patch
        if ($LASTEXITCODE -ne 0) { throw "把补丁打到备份上失败(git apply 退出码 $LASTEXITCODE)" }
    }
    finally { Pop-Location }
    Copy-Item -LiteralPath $beforeSource -Destination $beforeScript
    Copy-Item -LiteralPath (Join-Path $work 'tools/scripts/k8s_deploy.ps1') -Destination $afterScript

    foreach ($case in @(@{ Label = 'dev zone-up'; Args = $zoneArgs; Env = @{} }, @{ Label = 'dev infra-up'; Args = $infraArgs; Env = $battleEnv })) {
        $script:DeployScriptUnderTest = $beforeScript
        $old = Invoke-DeployDryRun -Arguments $case.Args -Env $case.Env
        $script:DeployScriptUnderTest = $afterScript
        $new = Invoke-DeployDryRun -Arguments $case.Args -Env $case.Env
        if ($old.ExitCode -ne 0 -or $new.ExitCode -ne 0) {
            $failed = $true
            Write-Host "$($case.Label): DryRun failed (before exit=$($old.ExitCode), after exit=$($new.ExitCode))"
            continue
        }
        $oldText = ConvertTo-ComparableOutput -Text ([string]$old.Output)
        $newText = ConvertTo-ComparableOutput -Text ([string]$new.Output)
        if ($oldText -ceq $newText) {
            Write-Host "$($case.Label): IDENTICAL ($(($newText -split "`n").Count) lines)"
            continue
        }
        $failed = $true
        $tag = $case.Label -replace '[^a-z]+', '-'
        $oldPath = Join-Path ([IO.Path]::GetTempPath()) "shared-redis-$tag-before.txt"
        $newPath = Join-Path ([IO.Path]::GetTempPath()) "shared-redis-$tag-after.txt"
        Set-Content -LiteralPath $oldPath -Value $oldText -NoNewline
        Set-Content -LiteralPath $newPath -Value $newText -NoNewline
        $a = $oldText -split "`n"; $b = $newText -split "`n"
        $first = 0
        while ($first -lt [Math]::Min($a.Count, $b.Count) -and $a[$first] -ceq $b[$first]) { $first++ }
        Write-Host "$($case.Label): DIFFERENT, first difference at line $($first + 1); full outputs: $oldPath / $newPath"
    }
}
finally {
    foreach ($temp in @($beforeScript, $afterScript)) { Remove-Item -LiteralPath $temp -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
if ($failed) { exit 1 }
exit 0
