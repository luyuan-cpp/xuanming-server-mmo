<#
.SYNOPSIS
    只保留 main:把其余分支和 worktree(含未提交改动)留底到 refs/archive,再删除。

.DESCRIPTION
    方案和取舍见同目录的 main-only-cleanup-20261101.md。
    不合并、不推送,不改 main 的提交、索引和工作区。
    不带参数只读:只打印计划,并把现状写进 build/main-only-cleanup/。
    -ArchiveOnly 只留底,不删除。-Apply 留底之后删除。
    在仓库根目录运行。可以重复运行:已经删掉的分支,留底还在。
#>
param([switch]$Apply, [switch]$ArchiveOnly)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

# 第一个包含整批改动的提交。HEAD 不包含它,说明跑错了地方。
$RequiredCommit = 'e33b99351'
# 这么久之内有文件改动的 worktree 只留底不删:可能还有人在用。
$RecentHours = 24

# 传给它的单横线参数要加引号:不加引号的 -p 会被当成 PowerShell 的公共参数,报 ambiguous。
function Invoke-Git {
    param([Parameter(Mandatory)][string]$Dir, [Parameter(ValueFromRemainingArguments)][string[]]$GitArgs)
    $output = & git -C $Dir @GitArgs
    if ($LASTEXITCODE -ne 0) { throw "git -C $Dir $($GitArgs -join ' ') exited with $LASTEXITCODE" }
    return $output
}

function Get-Worktrees {
    param([Parameter(Mandatory)][string]$Repo)
    $entries = [Collections.Generic.List[object]]::new()
    $current = $null
    foreach ($line in (Invoke-Git $Repo worktree list --porcelain)) {
        if ($line -like 'worktree *') {
            $current = [pscustomobject]@{ Path = $line.Substring(9); Head = ''; Branch = '' }
            $entries.Add($current)
        } elseif ($line -like 'HEAD *') {
            $current.Head = $line.Substring(5)
        } elseif ($line -like 'branch refs/heads/*') {
            $current.Branch = $line.Substring(18)
        }
    }
    return $entries
}

# worktree 的现状:未提交条数、最新改动时间。目录不存在时 Exists 为假,git worktree prune 会清掉它的记录。
function Get-WorktreeState {
    param([Parameter(Mandatory)]$Worktree)
    $state = [pscustomobject]@{
        Path = $Worktree.Path; Head = $Worktree.Head; Branch = $Worktree.Branch
        Exists = (Test-Path -LiteralPath (Join-Path $Worktree.Path '.git')); Unreadable = $false; Changed = 0; Newest = $null; Recent = $false
        Name = ($Worktree.Path -replace '[:/\\ ]+', '_').Trim('_')
    }
    if (-not $state.Exists) { return $state }
    # 旧 worktree 的子模块目录可能已坏,带子模块时 status 直接失败。子模块不在清理范围内,不看。
    try {
        $lines = @(Invoke-Git $Worktree.Path status --short --ignore-submodules=all)
    } catch {
        $state.Unreadable = $true
        return $state
    }
    $state.Changed = $lines.Count
    foreach ($line in $lines) {
        $relative = ($line.Substring(3) -split ' -> ')[-1].Trim('"')
        $file = Join-Path $Worktree.Path $relative
        if (Test-Path -LiteralPath $file) {
            $time = (Get-Item -LiteralPath $file -Force).LastWriteTime
            if ($null -eq $state.Newest -or $time -gt $state.Newest) { $state.Newest = $time }
        }
    }
    $state.Recent = ($null -ne $state.Newest) -and ($state.Newest -gt (Get-Date).AddHours(-$RecentHours))
    return $state
}

# 用临时索引把 worktree 当前内容(含未跟踪文件)写成一个提交,不动它的索引和工作区。.gitignore 忽略的文件不进来。
# 子模块路径排除在外,保留 HEAD 记录的版本:碰它们会在子模块目录已坏的 worktree 上失败。
function Save-WorktreeSnapshot {
    param([Parameter(Mandatory)][string]$Repo, [Parameter(Mandatory)]$State, [Parameter(Mandatory)][string]$OutDir)
    $index = Join-Path $OutDir "$($State.Name).index"
    $pathspec = [Collections.Generic.List[string]]::new()
    $pathspec.Add('.')
    if (Test-Path -LiteralPath (Join-Path $State.Path '.gitmodules')) {
        foreach ($line in @(& git -C $State.Path config -f .gitmodules --get-regexp '\.path$')) {
            $pathspec.Add(":(exclude)$(($line -split ' ', 2)[1])")
        }
    }
    $env:GIT_INDEX_FILE = $index
    try {
        Invoke-Git $State.Path read-tree $State.Head | Out-Null
        Invoke-Git $State.Path add '-A' '--' @pathspec | Out-Null
        $tree = (Invoke-Git $State.Path write-tree).Trim()
    } finally {
        Remove-Item Env:GIT_INDEX_FILE -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $index -Force -ErrorAction SilentlyContinue
    }
    $commit = (Invoke-Git $Repo commit-tree $tree '-p' $State.Head '-m' "worktree snapshot before main-only cleanup: $($State.Path)").Trim()
    Invoke-Git $Repo update-ref "refs/archive/worktree/$($State.Name)" $commit | Out-Null
    $saved = @(Invoke-Git $Repo --no-lazy-fetch diff --name-only --no-renames $State.Head $commit)
    if ($State.Changed -gt 0 -and $saved.Count -eq 0) { throw "Snapshot of $($State.Path) is empty but the worktree has changes." }
    return $saved.Count
}

$repo = (& git rev-parse --show-toplevel).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Run this from inside the repository.' }
if ((Invoke-Git $repo rev-parse --abbrev-ref HEAD).Trim() -cne 'main') { throw 'The main checkout is not on branch main.' }
& git -C $repo merge-base --is-ancestor $RequiredCommit HEAD
if ($LASTEXITCODE -ne 0) { throw "HEAD does not contain $RequiredCommit." }

$outDir = Join-Path $repo 'build/main-only-cleanup'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$headBefore = (Invoke-Git $repo rev-parse HEAD).Trim()
$statusBefore = @(Invoke-Git $repo status --short)
Set-Content -LiteralPath (Join-Path $outDir 'head-before.txt') -Value $headBefore
Set-Content -LiteralPath (Join-Path $outDir 'status-before.txt') -Value $statusBefore
Set-Content -LiteralPath (Join-Path $outDir 'refs-before.txt') -Value @(Invoke-Git $repo for-each-ref '--format=%(objectname) %(refname)' refs/heads refs/remotes refs/archive)
Set-Content -LiteralPath (Join-Path $outDir 'worktrees-before.txt') -Value @(Invoke-Git $repo worktree list --porcelain)

$branches = @(Invoke-Git $repo for-each-ref '--format=%(refname:short)' refs/heads | Where-Object { $_ -cne 'main' })
$normalizedRepo = $repo.Replace('\', '/').TrimEnd('/')
$states = @(Get-Worktrees $repo | Where-Object { $_.Path.Replace('\', '/').TrimEnd('/') -ine $normalizedRepo } | ForEach-Object { Get-WorktreeState $_ })

Write-Host "HEAD $headBefore; main checkout has $($statusBefore.Count) uncommitted entries (left untouched)"
Write-Host "Branches to archive and delete: $($branches.Count)"
foreach ($branch in $branches) {
    Write-Host ("  {0,3} ahead of main  {1}" -f (Invoke-Git $repo rev-list --count "main..$branch").Trim(), $branch)
}
Write-Host "Worktrees: $($states.Count)"
foreach ($state in $states) {
    $action = if (-not $state.Exists) { 'prune (no longer a worktree)' } elseif ($state.Unreadable) { 'KEEP (git cannot read it)' } elseif ($state.Recent) { 'snapshot, KEEP (changed recently)' } elseif ($state.Changed -gt 0) { 'snapshot, remove' } else { 'remove' }
    $newest = if ($state.Newest) { $state.Newest.ToString('MM-dd HH:mm') } else { '-' }
    Write-Host ("  {0,4} changed  newest {1}  {2}  {3}" -f $state.Changed, $newest, $action, $state.Path)
}
if (-not $Apply -and -not $ArchiveOnly) {
    Write-Host 'Dry run: nothing changed. Pass -ArchiveOnly to archive, or -Apply to archive and delete.'
    exit 0
}

# 先全部留底,再删。分支写不进留底就直接停;某个 worktree 留底失败只保留它,不影响其余的。
foreach ($branch in $branches) {
    Invoke-Git $repo update-ref "refs/archive/branch/$branch" "refs/heads/$branch" | Out-Null
}
$problems = [Collections.Generic.List[string]]::new()
$archived = @{}
foreach ($state in $states) {
    if (-not $state.Exists) { continue }
    if ($state.Unreadable) {
        $problems.Add("Git cannot read it, worktree kept: $($state.Path)")
        continue
    }
    try {
        & git -C $repo merge-base --is-ancestor $state.Head main
        $headInMain = ($LASTEXITCODE -eq 0)
        if ($state.Changed -gt 0) {
            $count = Save-WorktreeSnapshot -Repo $repo -State $state -OutDir $outDir
            Write-Host "Archived $($state.Path): $count files differ from its HEAD"
        } elseif (-not $headInMain) {
            Invoke-Git $repo update-ref "refs/archive/worktree/$($state.Name)" $state.Head | Out-Null
        }
        $archived[$state.Path] = $true
    } catch {
        $problems.Add("Could not archive, worktree kept: $($state.Path): $($_.Exception.Message)")
    }
}
if ($ArchiveOnly) {
    Write-Host "Archived refs: $(@(Invoke-Git $repo for-each-ref refs/archive).Count); nothing deleted."
    $problems | ForEach-Object { Write-Host $_ }
    Write-Host "Problems: $($problems.Count)"
    if ($problems.Count -gt 0) { exit 1 }
    exit 0
}

foreach ($state in $states) {
    if (-not $state.Exists -or -not $archived.ContainsKey($state.Path)) { continue }
    if ($state.Recent) {
        $problems.Add("Kept worktree changed within $RecentHours hours: $($state.Path)")
        continue
    }
    & git -C $repo worktree remove --force $state.Path
    if ($LASTEXITCODE -ne 0) { $problems.Add("Could not remove worktree: $($state.Path)") }
}
Invoke-Git $repo worktree prune | Out-Null

$stillCheckedOut = @(Get-Worktrees $repo | ForEach-Object { $_.Branch })
foreach ($branch in $branches) {
    if ($stillCheckedOut -ccontains $branch) {
        $problems.Add("Branch still checked out, not deleted: $branch")
        continue
    }
    & git -C $repo branch -D $branch
    if ($LASTEXITCODE -ne 0) { $problems.Add("Could not delete branch: $branch") }
}

if ((Invoke-Git $repo rev-parse HEAD).Trim() -cne $headBefore) { $problems.Add('HEAD changed during cleanup.') }
$statusAfter = @(Invoke-Git $repo status --short)
if (Compare-Object $statusBefore $statusAfter) { $problems.Add('The main checkout status changed during cleanup.') }

$report = [Collections.Generic.List[string]]::new()
$report.Add("HEAD: $headBefore")
$report.Add('Branches:')
$report.AddRange([string[]]@(Invoke-Git $repo branch --list))
$report.Add('Worktrees:')
$report.AddRange([string[]]@(Invoke-Git $repo worktree list))
$report.Add("Archived refs: $(@(Invoke-Git $repo for-each-ref refs/archive).Count)")
$report.Add("Problems: $($problems.Count)")
$report.AddRange([string[]]$problems)
Set-Content -LiteralPath (Join-Path $outDir 'result.txt') -Value $report
$report | ForEach-Object { Write-Host $_ }
if ($problems.Count -gt 0) { exit 1 }
