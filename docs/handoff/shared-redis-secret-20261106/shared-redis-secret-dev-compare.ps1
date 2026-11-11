#requires -Version 7
<#
.SYNOPSIS
    dev 档「改动前后生成物相同」的事后比对(交接 docs/handoff/handoff-global-singletons-p0-20261015.md §9.5 第 5 步)。

.DESCRIPTION
    补丁已于 2026-11-06 打入主工作区,打入前的 k8s_deploy.ps1 备份在本目录 shared-redis-secret-backup/tools/scripts/k8s_deploy.ps1。
    主工作区还在被别的会话改,所以不拿现在那份比。做法:
      1. 在临时目录把备份复制一份,只把本补丁里 tools/scripts/k8s_deploy.ps1 的改动打上去(git apply --include),得到「只差本补丁」的改后版;
      2. 改前、改后两份临时拷进 <仓库>/tools/scripts/(k8s_deploy.before-shared-redis.ps1 / k8s_deploy.after-shared-redis.ps1),
         同一目录下按 $PSScriptRoot 找 lib 与仓库根的逻辑不变,读的是同一批 manifest 与服务 yaml;
      3. 两份各跑一次 dev zone-up 与带全局 Go 服务的 dev infra-up(只用 -DryRun,不碰集群),两份全文写进 %TEMP%,按下面的判据比对;
         结束删掉两份临时拷贝。
    DryRun 夹具(SQL 写者目录)取契约测试 k8s_deploy_contract.tests.ps1 里的 Invoke-DeployDryRun,只把脚本路径换成参数。
    比较前去掉两类夹具噪声:临时目录名 mmorpg-roster-render-<GUID>,以及输出里出现的临时脚本文件名。

    判据(2026-11-10 改):输出拆成 manifest 块(--- BEGIN / END MANIFEST --- 之间)与其余输出行。两边的 manifest 块一一对应、逐字相同,
    其余输出行也一一对应,才算相同;先后不计。不按全文顺序比,是因为部署脚本有两处按普通哈希表 @{} 的 .Keys 遍历服务:
    Apply-GlobalGoSvcManifests 用的 Get-GlobalGoSvcNames(遍历 $GoSvcCatalogue),Apply-JavaSvcManifests 遍历 $JavaSvcCatalogue。
    哈希表按键的哈希值分桶,.NET 的字符串哈希每个进程随机取种子,同一份脚本跑两次,这几个服务的先后就可能不同。补丁没改这两处,也没改两份目录。
    11-06 第一次运行就因此判成 DIFFERENT(旧判据按全文顺序比):两组 manifest 的内容与份数完全一致,差别只在全局 Go 服务之间、
    以及 gateway 与 auth 之间的先后。顺序是否一致照样打印,只作参考。

    -SavedOutputDir:不跑 DryRun,按同一判据重判一个目录里留存的四份全文(shared-redis-dev-zone-up-before.txt 等,命名与本脚本写进
    %TEMP% 的相同),例如 11-06 留在 build/redis-secret-finish-20261106/ 的那一组。
    -ControlRun:把改前那份再跑一遍,与它自己第一次的输出比,打印内容与顺序是否相同(多两次 DryRun;顺序碰巧相同不说明什么)。
    现跑时两份 11-06 的脚本读的是主工作区当前的 lib、manifest 与服务 yaml。若改前改后两次 DryRun 报同一个错,是这些输入后来改了、
    旧脚本跑不动,不是本补丁的问题,改用 -SavedOutputDir。
    先清掉 MMORPG_* 环境变量再跑(dev 基线假定都没设)。通过 = 两组都打印 IDENTICAL CONTENT,退出码 0。
#>
param(
    [string]$RepoRoot = 'D:\luyuan\xuanming-server-mmo-cpp',
    [string]$SavedOutputDir = '',
    [switch]$ControlRun
)

$ErrorActionPreference = 'Stop'
$caseLabels = @('dev zone-up', 'dev infra-up')

function ConvertTo-ComparableOutput {
    param([AllowEmptyString()][string]$Text)
    $t = $Text -replace "`r`n", "`n"
    $t = $t -replace 'mmorpg-roster-render-[0-9a-f]{32}', 'mmorpg-roster-render-<GUID>'
    return $t.Replace('k8s_deploy.before-shared-redis.ps1', 'k8s_deploy.ps1').Replace('k8s_deploy.after-shared-redis.ps1', 'k8s_deploy.ps1')
}

# 一组输出全文的文件名;现跑写进 %TEMP% 与 -SavedOutputDir 读取用同一套命名。
function Get-CompareOutputPath {
    param([string]$Dir, [string]$Label, [string]$Side)
    return Join-Path $Dir ('shared-redis-{0}-{1}.txt' -f ($Label -replace '[^a-z]+', '-'), $Side)
}

# 把 DryRun 输出拆成 manifest 块与其余输出行(切法与 tests/lib/deploy_capture.ps1 的 Get-ManifestBlocks 相同),两组各按序数排好。
# 输出被截断、最后一块没有 END 标记时,这一块照样计入,不丢。
function Split-DryRunOutput {
    param([AllowEmptyString()][string]$Text)
    $blocks = [System.Collections.Generic.List[string]]::new()
    $others = [System.Collections.Generic.List[string]]::new()
    $current = $null
    foreach ($line in ($Text -split "`n")) {
        $marker = $line.Trim()
        if ($marker -eq '--- BEGIN MANIFEST ---') { $current = [System.Collections.Generic.List[string]]::new(); continue }
        if ($marker -eq '--- END MANIFEST ---') {
            if ($null -ne $current) { $blocks.Add($current -join "`n") }
            $current = $null
            continue
        }
        if ($null -ne $current) { $current.Add($line) } else { $others.Add($line) }
    }
    if ($null -ne $current) { $blocks.Add("<unterminated manifest>`n" + ($current -join "`n")) }
    $sortedBlocks = $blocks.ToArray()
    $sortedOthers = $others.ToArray()
    [Array]::Sort($sortedBlocks, [StringComparer]::Ordinal)
    [Array]::Sort($sortedOthers, [StringComparer]::Ordinal)
    return [pscustomobject]@{ Blocks = $sortedBlocks; Others = $sortedOthers }
}

# manifest 块的「kind/name」(块内第一份文档的 kind 与 metadata.name),只用于报告哪几份不一样。
function Get-ManifestLabel {
    param([string]$Block)
    $m = [regex]::Match($Block, '(?m)^kind:\s*(?<kind>\S+)[\s\S]*?^  name:\s*(?<name>\S+)')
    if ($m.Success) { return "$($m.Groups['kind'].Value)/$($m.Groups['name'].Value)" }
    return '(unlabeled manifest)'
}

# 按判据比较两份输出:相同返回 $true;不同时打印只出现在一边的 manifest(kind/name)与前 5 条只出现在一边的输出行。
function Test-SameDryRunContent {
    param([string]$Label, [AllowEmptyString()][string]$OldText, [AllowEmptyString()][string]$NewText)
    $old = Split-DryRunOutput -Text $OldText
    $new = Split-DryRunOutput -Text $NewText
    $separator = "`n<<end of manifest>>`n"
    $sameBlocks = ($old.Blocks.Count -eq $new.Blocks.Count) -and (($old.Blocks -join $separator) -ceq ($new.Blocks -join $separator))
    $sameOthers = ($old.Others.Count -eq $new.Others.Count) -and (($old.Others -join "`n") -ceq ($new.Others -join "`n"))
    if ($sameBlocks -and $sameOthers) { return $true }

    $blockDiff = @(Compare-Object -ReferenceObject $old.Blocks -DifferenceObject $new.Blocks -CaseSensitive)
    $onlyBefore = @($blockDiff | Where-Object SideIndicator -eq '<=' | ForEach-Object { Get-ManifestLabel $_.InputObject })
    $onlyAfter = @($blockDiff | Where-Object SideIndicator -eq '=>' | ForEach-Object { Get-ManifestLabel $_.InputObject })
    Write-Host "${Label}: manifests before=$($old.Blocks.Count) after=$($new.Blocks.Count); only before: [$($onlyBefore -join ', ')]; only after: [$($onlyAfter -join ', ')]"
    foreach ($d in @(Compare-Object -ReferenceObject $old.Others -DifferenceObject $new.Others -CaseSensitive | Select-Object -First 5)) {
        $side = if ($d.SideIndicator -eq '<=') { 'before' } else { 'after' }
        Write-Host "${Label}: output line only in ${side}: $($d.InputObject)"
    }
    return $false
}

# 判一组并打印结论(内容是否相同为准,顺序只作参考);相同返回 $true。
function Compare-CaseOutput {
    param([string]$Label, [AllowEmptyString()][string]$OldText, [AllowEmptyString()][string]$NewText, [string]$OldPath, [string]$NewPath)
    $order = if ($OldText -ceq $NewText) { 'same order' } else { 'order differs; hashtable key order is per process, see the script header' }
    if (Test-SameDryRunContent -Label $Label -OldText $OldText -NewText $NewText) {
        Write-Host "${Label}: IDENTICAL CONTENT ($(($NewText -split "`n").Count) lines; $order)"
        return $true
    }
    Write-Host "${Label}: DIFFERENT CONTENT; full outputs: $OldPath / $NewPath"
    return $false
}

# 重判留存的输出:不跑 DryRun,不碰仓库。
if (-not [string]::IsNullOrWhiteSpace($SavedOutputDir)) {
    $savedDir = (Resolve-Path -LiteralPath $SavedOutputDir).Path
    $failed = $false
    foreach ($label in $caseLabels) {
        $oldPath = Get-CompareOutputPath -Dir $savedDir -Label $label -Side 'before'
        $newPath = Get-CompareOutputPath -Dir $savedDir -Label $label -Side 'after'
        $oldText = ConvertTo-ComparableOutput -Text ([IO.File]::ReadAllText($oldPath))
        $newText = ConvertTo-ComparableOutput -Text ([IO.File]::ReadAllText($newPath))
        if (-not (Compare-CaseOutput -Label $label -OldText $oldText -NewText $newText -OldPath $oldPath -NewPath $newPath)) { $failed = $true }
    }
    if ($failed) { exit 1 }
    exit 0
}

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
$cases = @(
    @{ Label = $caseLabels[0]; Args = $zoneArgs; Env = @{} },
    @{ Label = $caseLabels[1]; Args = $infraArgs; Env = $battleEnv }
)

$outputDir = [IO.Path]::GetTempPath()
$work = Join-Path $outputDir ('shared-redis-compare-' + [Guid]::NewGuid().ToString('N'))
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

    foreach ($case in $cases) {
        $script:DeployScriptUnderTest = $beforeScript
        $old = Invoke-DeployDryRun -Arguments $case.Args -Env $case.Env
        $script:DeployScriptUnderTest = $afterScript
        $new = Invoke-DeployDryRun -Arguments $case.Args -Env $case.Env
        $oldText = ConvertTo-ComparableOutput -Text ([string]$old.Output)
        $newText = ConvertTo-ComparableOutput -Text ([string]$new.Output)
        $oldPath = Get-CompareOutputPath -Dir $outputDir -Label $case.Label -Side 'before'
        $newPath = Get-CompareOutputPath -Dir $outputDir -Label $case.Label -Side 'after'
        Set-Content -LiteralPath $oldPath -Value $oldText -NoNewline
        Set-Content -LiteralPath $newPath -Value $newText -NoNewline
        if ($old.ExitCode -ne 0 -or $new.ExitCode -ne 0) {
            $failed = $true
            Write-Host "$($case.Label): DryRun failed (before exit=$($old.ExitCode), after exit=$($new.ExitCode)); full outputs: $oldPath / $newPath"
            continue
        }
        if (-not (Compare-CaseOutput -Label $case.Label -OldText $oldText -NewText $newText -OldPath $oldPath -NewPath $newPath)) { $failed = $true }

        if ($ControlRun) {
            # 对照:改前那份再跑一次,与它第一次的输出比。内容应相同;顺序变了,说明顺序本来就随进程变。
            $script:DeployScriptUnderTest = $beforeScript
            $again = Invoke-DeployDryRun -Arguments $case.Args -Env $case.Env
            if ($again.ExitCode -ne 0) {
                Write-Host "$($case.Label): control run of the before script failed (exit=$($again.ExitCode))"
                continue
            }
            $againText = ConvertTo-ComparableOutput -Text ([string]$again.Output)
            $content = if (Test-SameDryRunContent -Label "$($case.Label) control" -OldText $oldText -NewText $againText) { 'same content' } else { 'DIFFERENT CONTENT' }
            $order = if ($againText -ceq $oldText) { 'same order' } else { 'order differs' }
            Write-Host "$($case.Label): control run of the before script vs its first run: $content, $order"
        }
    }
}
finally {
    foreach ($temp in @($beforeScript, $afterScript)) { Remove-Item -LiteralPath $temp -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
if ($failed) { exit 1 }
exit 0
